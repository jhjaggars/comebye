#!/usr/bin/env bash
# The full suite a worker runs before landing, with a block to paste into the landing report.
#
#   tools/ci/prelanding.sh [OUT_DIR]
#
# `go test -race ./...`, then every browser script in both colour schemes (tools/uitest/run.sh), each timed. The
# block it prints ends with the commit and the tool versions, so a difference from CI (a different Chrome, say) is
# visible in the report. Exit 1 if anything failed. Needs go, node and Chrome (CHROME=/path to override).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$here/../.."
out="${1:-$(mktemp -d)}"; mkdir -p "$out"
cd "$root"

t() { local s=$SECONDS; "$@"; local rc=$?; echo $((SECONDS - s)) > "$out/last-secs"; return $rc; }
rc=0; res=()
step() { # step LABEL CMD...
  local label="$1" f; f="${1//[^A-Za-z0-9._-]/_}"; shift
  if t "$@" > "$out/$f.log" 2>&1; then res+=("$label: passed in $(cat "$out/last-secs") s"); else res+=("$label: FAILED after $(cat "$out/last-secs") s (log: $out/$f.log)"); rc=1; fi
}
step "go test -race ./..." go test -race ./...
step "browser checks, both themes" tools/uitest/run.sh "$root" "$out/uitest"

chrome="${CHROME:-$(command -v google-chrome || command -v chromium || echo none)}"
echo "Pre-landing run:"
printf '  %s\n' "${res[@]}"
echo "  commit $(git rev-parse --short HEAD) $(git diff --quiet && git diff --cached --quiet || echo '(uncommitted changes)')"
echo "  $(go version | cut -d' ' -f3-4), node $(node --version), $("$chrome" --version 2>/dev/null | head -1 || echo 'no chrome'), $(uname -sr), $(nproc) cores"
exit $rc
