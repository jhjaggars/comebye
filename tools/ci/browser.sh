#!/usr/bin/env bash
# One browser suite, the way CI and a worker's pre-landing run both run it, so the two cannot drift.
#
#   tools/ci/browser.sh SUITE [SCHEME] [OUT_DIR]
#
# SUITE: smoke (whole groups of drive.mjs checks, one set per screen, plus pages, usage and peers: ~160 checks, about 2 min), drive (drive.mjs), rest (every other behaviour script),
#        visual (the design-language scripts), full (everything, as tools/uitest/run.sh does)
#        EXTRA="peers sidepanel" adds scripts to smoke.
# SCHEME: dark (default) or light. Chrome comes from $CHROME or the first google-chrome or chromium on PATH.
#
# A red run is judged against tools/ci/flaky.tsv: failed checks on an active entry do not fail the suite.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
suite="${1:?suite: smoke|drive|rest|visual|full}"; scheme="${2:-dark}"; out="${3:-${RUNNER_TEMP:-/tmp}/uitest-$suite-$scheme}"

VISUAL="buttons chips controls fontsetting decisionsets starters"
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

if [ -z "${CHROME:-}" ]; then
  CHROME="$(command -v google-chrome || command -v chromium || true)"
  if [ -z "$CHROME" ]; then echo "::warning::no google-chrome or chromium on this machine; skipping browser checks"; exit 0; fi
  export CHROME
fi

mkdir -p "$out"
# Each suite's servers take their ports from the base: two suites on one machine need different PORT values.
SCRIPTS="$scripts" SCHEME="$scheme" "$here/../uitest/run.sh" "${APP_DIR:-$here/../..}" "$out" 2>&1 | tee "$out/run.log"
rc=${PIPESTATUS[0]}
[ "$rc" -eq 0 ] && exit 0
"$here/flakes.sh" judge "$out/run.log"
