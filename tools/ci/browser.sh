#!/usr/bin/env bash
# One browser suite, the way CI and a worker's pre-landing run both run it, so the two cannot drift.
#
#   tools/ci/browser.sh SUITE [SCHEME] [OUT_DIR] [SHARD]
#
# SUITE: smoke (whole groups of drive.mjs checks, one set per screen, plus pages, usage and peers: ~160 checks, about 2 min), drive (drive.mjs), rest (every other behaviour script),
#        visual (the design-language scripts), full (everything, as tools/uitest/run.sh does),
#        changed (what a pull request's pre-submit tier runs for this branch's changes against BASE, default origin/main:
#        tools/ci/select.sh's smoke set plus the scripts for the touched paths, nothing for docs, full when it says all)
#        EXTRA="peers sidepanel" adds scripts to smoke.
# SHARD: I/N runs the I-th of N parts of the suite (rest only; CI runs rest as 4 jobs). The parts are balanced by each script's
#        time in tools/ci/browser-times.txt (name, seconds; a script not listed counts 30 s), biggest first into the lightest part,
#        so every script is in exactly one part and a new script needs no list edited. Refresh the file from a run's
#        "== script NAME: N s" lines when the parts drift apart.
# PARALLEL=N: rest only, its N parts at once on this machine (see below). SCHEME: dark (default) or light. Chrome comes from $CHROME or the first google-chrome or chromium on PATH.
#
# Any failed check fails the suite; there is no list of checks allowed to fail.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# On the test VM when one is configured and answers (tools/ci/prefer-vm.sh; TEST_VM=local keeps it here).
. "$here/prefer-vm.sh"; prefer_vm browser "$@"
[ -n "${COMEBYE_CAPPED:-}" ] || exec "$here/capped.sh" "${BASH_SOURCE[0]}" "$@"
suite="${1:?suite: smoke|drive|rest|visual|full|changed}"; scheme="${2:-dark}"; out="${3:-${RUNNER_TEMP:-/tmp}/uitest-$suite-$scheme}"; shard="${4:-}"

VISUAL="shapes appearance starters depth gradients landscape"
# Every script run.sh knows (its "want NAME" lines), so a script added there is in "rest" and "full" without editing this file.
ALL="$(grep -o 'want [a-z]*;' "$here/../uitest/run.sh" | awk '{print $2}' | tr -d ';' | tr '\n' ' ')"
rest=""
for s in $ALL; do
  case " drive $VISUAL " in *" $s "*) ;; *) rest="$rest $s";; esac
done

if [ "$suite" = changed ]; then
  base="$(git -C "$here/../.." merge-base "${BASE:-origin/main}" HEAD)" || { echo "no merge base with ${BASE:-origin/main}" >&2; exit 2; }
  # Committed, staged, unstaged and untracked changes alike: a worker runs this before committing too.
  sel="$( { git -C "$here/../.." diff --name-only "$base"; git -C "$here/../.." ls-files --others --exclude-standard; } | sort -u | "$here/select.sh")"
  sel_scripts="$(sed -n 's/^scripts=//p' <<< "$sel")"; sel_smoke="$(sed -n 's/^smoke=//p' <<< "$sel")"
  if [ "$sel_scripts" = all ]; then
    echo "changed: the change needs every script (select.sh says scripts=all): running full"; suite=full
  elif [ "$sel_smoke" = true ] || [ -n "$sel_scripts" ]; then
    echo "changed: smoke${sel_scripts:+ plus $sel_scripts}"; suite=smoke; EXTRA="${EXTRA:-} $sel_scripts"
  else
    echo "changed: no browser script covers these changes"; exit 0
  fi
fi

case "$suite" in
  smoke)  export SMOKE=1; scripts="drive pages usage peers ${EXTRA:-}" ;;
  drive)  scripts="drive" ;;
  rest)   scripts="${rest# }" ;;
  visual) scripts="$VISUAL" ;;
  full)   scripts="" ;;
  *) echo "unknown suite $suite" >&2; exit 2 ;;
esac

# PARALLEL=N (rest only, no SHARD): the N parts at once on this machine, each with its own servers on its own ports
# (PORT, PORT+1000, ...) and its own OUT_DIR/part-I, inside the one memory cap above. Locally the parts are
# independent, so the wall time is about the longest part's. Serial unless asked: two or more Chromes and hubs at once
# load the machine, so keep N small (2 to 4) and never beside another heavy suite.
if [ "$suite" = rest ] && [ -z "$shard" ] && [ "${PARALLEL:-1}" -gt 1 ]; then
  n="$PARALLEL"; [ "$n" -le 9 ] || n=9
  base_port="${PORT:-18741}"; pids=()
  for i in $(seq 1 "$n"); do
    PARALLEL=1 PORT=$(( base_port + (i - 1) * 1000 )) "${BASH_SOURCE[0]}" rest "$scheme" "$out/part-$i" "$i/$n" \
      > >(sed -u "s/^/[$i\/$n] /") 2>&1 &
    pids+=($!)
  done
  rc=0; for p in "${pids[@]}"; do wait "$p" || rc=1; done
  echo "rest in $n parallel parts: $([ $rc = 0 ] && echo ok || echo FAILED)"
  exit $rc
fi

if [ -n "$shard" ]; then
  [ "$suite" = rest ] || { echo "a shard is for the rest suite only" >&2; exit 2; }
  case "$shard" in [1-9]/[1-9]) ;; *) echo "shard must look like 2/4" >&2; exit 2;; esac
  # Greedy balancing (longest script first into the lightest part), the same answer for every part's job.
  scripts="$(for s in $scripts; do
      echo "$s $(awk -v n="$s" '$1 == n {t = $2} END {print t ? t : 30}' "$here/browser-times.txt")"
    done | sort -k2,2nr -k1,1 | awk -v i="${shard%/*}" -v n="${shard#*/}" '
      { m = 1; for (b = 2; b <= n; b++) if (load[b] < load[m]) m = b
        load[m] += $2; if (m == i) printf "%s ", $1 }')"
  scripts="${scripts% }"
  # An empty SCRIPTS means "everything" to run.sh: never let a part with nothing in it fall through to that.
  [ -n "$scripts" ] || { echo "shard $shard has no scripts" >&2; exit 1; }
  echo "shard $shard: $scripts"
fi

if [ -z "${CHROME:-}" ]; then
  CHROME="$(command -v google-chrome || command -v chromium || true)"
  if [ -z "$CHROME" ]; then echo "no google-chrome or chromium on this machine (set CHROME)" >&2; exit 1; fi
  export CHROME
fi

mkdir -p "$out"
# Each suite's servers take their ports from the base: two suites on one machine need different PORT values.
SCRIPTS="$scripts" SCHEME="$scheme" "$here/../uitest/run.sh" "${APP_DIR:-$here/../..}" "$out" 2>&1 | tee "$out/run.log"
exit "${PIPESTATUS[0]}"
