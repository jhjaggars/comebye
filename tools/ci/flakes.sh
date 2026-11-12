#!/usr/bin/env bash
# The flaky-check list (tools/ci/flaky.tsv): who owns each entry, which ones are still active, and what a red run means.
#
#   tools/ci/flakes.sh list                 the active entries
#   tools/ci/flakes.sh expired              entries past their date (they block again): exit 1 if any
#   tools/ci/flakes.sh judge LOG            read run.sh's output; exit 1 if a failed check is not on the active list
#   tools/ci/flakes.sh stress N             run each active entry's script N times (the nightly); exit 1 if any
#                                           entry fails every time (a flake that fails sometimes is the point)
#
# judge prints which failures it let through and why, and appends the same to $GITHUB_STEP_SUMMARY.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
list="$here/flaky.tsv"
today="${TODAY:-$(date -u +%F)}"

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
    n="${1:-5}"; rc=0
    while IFS=$'\t' read -r script check owner _; do
      [ -n "$script" ] || continue
      pass=0
      for _ in $(seq "$n"); do
        if SCRIPTS="$script" ONLY="$check" SCHEME=dark "$here/../uitest/run.sh" "${APP_DIR:-$here/../..}" "${OUT_DIR:-$(mktemp -d)}" >/dev/null 2>&1; then pass=$((pass + 1)); fi
      done
      echo "$script / $check (owner $owner): passed $pass of $n"
      [ "$pass" -eq 0 ] && rc=1
    done < <(active)
    exit $rc ;;
  *) sed -n '2,12p' "$0"; exit 2 ;;
esac
