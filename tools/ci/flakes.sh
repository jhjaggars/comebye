#!/usr/bin/env bash
# The flaky-check list (tools/ci/flaky.tsv): who owns each entry, which ones are still active, and what a red run means.
#
#   tools/ci/flakes.sh list                 the active entries
#   tools/ci/flakes.sh expired              entries past their date (they block again): exit 1 if any
#   tools/ci/flakes.sh judge LOG            read run.sh's output; exit 1 if a failed check is not on the active list
#   tools/ci/flakes.sh stress N             run each script that has an entry N times, whole (the nightly); exit 1 if any
#                                           entry fails every time (a flake that fails sometimes is the point)
#   tools/ci/flakes.sh repeat N [SCRIPT...] run each uitest script (default: every one run.sh knows) N times, JOBS (default 4)
#                                           at a time, each on its own server; list the checks that failed in some runs but
#                                           not all (FLAKY k/N) apart from those that failed every time (BROKEN); exit 1 if any
#                                           is FLAKY. Not tied to the flaky list: this is how a new flake is found.
#
# judge prints which failures it let through and why, and appends the same to $GITHUB_STEP_SUMMARY.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
list="$here/flaky.tsv"
today="${TODAY:-$(date -u +%F)}"

# ports_free P: nothing listens on any of the ports a run.sh with PORT=P uses.
ports_free() { local p; for p in 0 1 100 200 250 270 400 500; do (exec 3<>"/dev/tcp/127.0.0.1/$(($1 + p))") 2>/dev/null && return 1; done; return 0; }

entries() { grep -vE '^\s*(#|$)' "$list" || true; }           # script check owner added expires note
active() { entries | awk -F'\t' -v t="$today" '$5 >= t'; }
expired() { entries | awk -F'\t' -v t="$today" '$5 < t'; }

cmd="${1:-}"; shift || true
case "$cmd" in
  list) active ;;
  expired)
    e="$(expired)"; [ -z "$e" ] && exit 0
    echo "expired flaky-list entries (renew them or fix the check):"; echo "$e" | awk -F'\t' '{print "  " $2 " (" $3 ", expired " $5 ")"}'; exit 1 ;;
  judge)
    log="$1"
    # run.sh prints "FAIL  <name> -- <error>" (drive and most scripts) or "FAIL <name>".
    fails="$(sed -n 's/^[[:space:]]*FAIL[[:space:]]\{1,\}//p' "$log" | sed 's/ -- .*$//' | sort -u)"
    [ -z "$fails" ] && exit 0
    blocking=""; allowed=""
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      hit=""
      while IFS=$'\t' read -r _ check owner _ exp _; do
        [ -n "$check" ] || continue
        case "$name" in *"$check"*) hit="$check (owner $owner, until $exp)"; break;; esac
      done < <(active)
      if [ -n "$hit" ]; then allowed="$allowed
  $name  <- $hit"; else blocking="$blocking
  $name"; fi
    done <<< "$fails"
    out=""
    [ -n "$allowed" ] && out="Known-flaky checks that failed (not blocking):$allowed"
    [ -n "$blocking" ] && out="$out
Failed checks that block:$blocking"
    echo "$out"
    [ -n "${GITHUB_STEP_SUMMARY:-}" ] && { echo '```'; echo "$out"; echo '```'; } >> "$GITHUB_STEP_SUMMARY"
    [ -z "$blocking" ] ;;
  stress)
    # A check cannot be run alone: drive.mjs checks build on each other's state ("fleet menu" fails by itself, always).
    # So each script that has an entry runs whole, n times, and an entry passes a run when no failed line names it.
    n="${1:-5}"; rc=0
    out="${OUT_DIR:-$(mktemp -d)}"; mkdir -p "$out"
    for script in $(active | cut -f1 | sort -u); do
      for i in $(seq "$n"); do
        SCRIPTS="$script" SCHEME=dark "$here/../uitest/run.sh" "${APP_DIR:-$here/../..}" "$out/$script-$i" > "$out/$script-$i.log" 2>&1 || true
      done
      while IFS=$'\t' read -r s check owner _; do
        [ "$s" = "$script" ] || continue
        pass=0
        for i in $(seq "$n"); do
          grep -E '^[[:space:]]*FAIL' "$out/$script-$i.log" | grep -qF -- "$check" || pass=$((pass + 1))
        done
        echo "$script / $check (owner $owner): passed $pass of $n"
        [ "$pass" -eq 0 ] && rc=1
      done < <(active)
    done
    exit $rc ;;
  repeat)
    n="${1:-3}"; shift || true
    scripts="${*:-$(grep -o 'want [a-z]*;' "$here/../uitest/run.sh" | awk '{print $2}' | tr -d ';' | tr '\n' ' ')}"
    jobs="${JOBS:-4}"; base="${REPEAT_PORT:-21000}"; scheme="${SCHEME:-dark}"
    out="${OUT_DIR:-$(mktemp -d)}"; mkdir -p "$out"
    app="${APP_DIR:-$here/../..}"
    # One npm ci before the jobs start, not one per job racing on node_modules.
    (cd "$here/../uitest" && { [ -d node_modules/playwright-core ] || npm ci --no-audit --no-fund --silent; })
    declare -a slot=()
    for script in $scripts; do
      for i in $(seq "$n"); do
        free=""
        while [ -z "$free" ]; do
          for k in $(seq 0 $((jobs - 1))); do
            if [ -z "${slot[$k]:-}" ] || ! kill -0 "${slot[$k]}" 2>/dev/null; then free="$k"; break; fi
          done
          [ -n "$free" ] || sleep 1
        done
        # Each job has its own server on ports PORT, +1 and up to +500 (what run.sh uses): the first PORT from
        # base+1000*slot whose ports nothing listens on (a developer's machine has a syncthing on 22000).
        port=$((base + 1000 * free))
        while ! ports_free "$port"; do port=$((port + 10)); done
        ( SCRIPTS="$script" SCHEME="$scheme" PORT=$port "$here/../uitest/run.sh" "$app" "$out/$script-$i" > "$out/$script-$i.log" 2>&1
          echo $? > "$out/$script-$i.rc" ) &
        slot[$free]=$!
      done
    done
    wait
    rc=0; table=""
    for script in $scripts; do
      # The failed checks of each run, one per line; a run that failed with no FAIL line (a crash, a port taken) counts as its own check.
      all=""
      for i in $(seq "$n"); do
        f="$(sed -n 's/^[[:space:]]*FAIL[[:space:]]\{1,\}//p' "$out/$script-$i.log" | sed 's/ -- .*$//' | sort -u)"
        [ -z "$f" ] && [ "$(cat "$out/$script-$i.rc" 2>/dev/null || echo 1)" != 0 ] && f="(the script exited non-zero with no FAIL line; see its .log)"
        all="$all
$f"
      done
      while read -r cnt name; do
        [ -n "$name" ] || continue
        if [ "$cnt" -ge "$n" ]; then table="$table
BROKEN $script / $name: failed $cnt of $n"
        else table="$table
FLAKY  $script / $name: failed $cnt of $n"; rc=1; fi
      done < <(printf '%s\n' "$all" | grep -v '^$' | sort | uniq -c | sed -E 's/^ *([0-9]+) /\1 /')
    done
    if [ -z "$table" ]; then msg="No check failed in $n runs of each of: $scripts"; else msg="$(printf '%s\n' "$table" | sed '/^$/d')"; fi
    echo "$msg"
    [ -n "${GITHUB_STEP_SUMMARY:-}" ] && { echo "### Repeat runs ($n each)"; echo '```'; echo "$msg"; echo '```'; } >> "$GITHUB_STEP_SUMMARY"
    exit $rc ;;
  *) sed -n '2,16p' "$0"; exit 2 ;;
esac
