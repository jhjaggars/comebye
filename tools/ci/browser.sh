#!/usr/bin/env bash
# One browser suite, the way CI and a worker's pre-landing run both run it, so the two cannot drift.
#
#   tools/ci/browser.sh SUITE [SCHEME] [OUT_DIR] [SHARD]
#
# SUITE: smoke (whole groups of drive.mjs checks, one set per screen, plus pages, usage and peers: ~160 checks, about 2 min), drive (drive.mjs), rest (every other behaviour script),
#        visual (the design-language scripts), full (everything, as tools/uitest/run.sh does)
#        EXTRA="peers sidepanel" adds scripts to smoke.
# SHARD: I/N runs the I-th of N parts of the suite (rest only; CI runs rest as 4 jobs). The parts are balanced by each script's
#        time in tools/ci/browser-times.txt (name, seconds; a script not listed counts 30 s), biggest first into the lightest part,
#        so every script is in exactly one part and a new script needs no list edited. Refresh the file from a run's
#        "== script NAME: N s" lines when the parts drift apart.
# SCHEME: dark (default) or light. Chrome comes from $CHROME or the first google-chrome or chromium on PATH.
#
# Any failed check fails the suite; there is no list of checks allowed to fail.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "${COMEBYE_CAPPED:-}" ] || exec "$here/capped.sh" "${BASH_SOURCE[0]}" "$@"
suite="${1:?suite: smoke|drive|rest|visual|full}"; scheme="${2:-dark}"; out="${3:-${RUNNER_TEMP:-/tmp}/uitest-$suite-$scheme}"; shard="${4:-}"

VISUAL="shapes appearance starters depth"
# Every script run.sh knows (its "want NAME" lines), so a script added there is in "rest" and "full" without editing this file.
ALL="$(grep -o 'want [a-z]*;' "$here/../uitest/run.sh" | awk '{print $2}' | tr -d ';' | tr '\n' ' ')"
rest=""
for s in $ALL; do
  case " drive $VISUAL " in *" $s "*) ;; *) rest="$rest $s";; esac
done

case "$suite" in
  smoke)  export SMOKE=1; scripts="drive pages usage peers ${EXTRA:-}" ;;
  drive)  scripts="drive" ;;
  rest)   scripts="${rest# }" ;;
  visual) scripts="$VISUAL" ;;
  full)   scripts="" ;;
  *) echo "unknown suite $suite" >&2; exit 2 ;;
esac

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
