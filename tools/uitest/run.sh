#!/usr/bin/env bash
# Run the browser checks against the web app of a checkout.
#
#   tools/uitest/run.sh [APP_DIR] [OUT_DIR]
#
# APP_DIR is the checkout whose comebye binary and web/ are tested (default:
# this repo), so a regression can be bisected by pointing it at a scratch
# `git worktree` of an older commit. The fake herdr (tools/devserver) and the
# driver always come from the checkout this script lives in. Needs go, node,
# npm and google-chrome (CHROME=/path to override). Screenshots, the console
# and network logs land in OUT_DIR (default: a temp dir). Exit 1 on failure.
set -euo pipefail

# SCHEME=dark|light picks the emulated prefers-color-scheme. Unset, the whole
# run happens twice on fresh servers (state left by one pass would skew the
# other): dark in OUT_DIR, then light in OUT_DIR/light.
if [ -z "${SCHEME:-}" ]; then
  me="${BASH_SOURCE[0]}"; rc=0
  o="${2:-$(mktemp -d)}"
  SCHEME=dark "$me" "${1:-}" "$o" || rc=1
  SCHEME=light PORT=$(( ${PORT:-18741} + 1 )) "$me" "${1:-}" "$o/light" || rc=1
  exit $rc
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
app="$(cd "${1:-$root}" && pwd)"
out="${2:-$(mktemp -d)}"
port="${PORT:-18741}"
tmp="$(mktemp -d)"
mkdir -p "$out"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$tmp"' EXIT

(cd "$here" && [ -d node_modules/playwright-core ] || npm ci --no-audit --no-fund --silent)

(cd "$root" && go build -o "$tmp/fake-herdr" ./tools/devserver)
(cd "$app" && go build -o "$tmp/comebye" .)

# shellcheck disable=SC2086
"$tmp/fake-herdr" -sock "$tmp/h.sock" -dir "$tmp" ${FAKE_ARGS--trust-prompt} >"$out/fake-herdr.log" 2>&1 &
for _ in $(seq 50); do [ -S "$tmp/h.sock" ] && break; sleep 0.1; done

"$tmp/fake-herdr" -recall-fixture "$tmp/recall.db" 2>"$out/recall-fixture.log"

# Something else already listening on the port would answer the readiness loop below and the driver would then
# test (or hang on) that: refuse to start instead.
if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
  echo "port $port is already in use by another process: set PORT=<free port> (the light pass uses PORT+1)" >&2
  exit 1
fi

"$tmp/comebye" -sock "$tmp/h.sock" -addr "127.0.0.1:$port" -insecure-no-auth -push=false \
  -state-dir "$tmp/state" -host-name uitest -recall-index "$tmp/recall.db" -release-simulate >"$out/comebye.log" 2>&1 &
for _ in $(seq 50); do curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null && break; sleep 0.1; done

echo "app: $app ($(git -C "$app" log -1 --format='%h %s'))"
cd "$here"
rc=0
node drive.mjs "http://127.0.0.1:$port" "$out" || rc=1

# Voice input (stt.mjs). The "not installed" and secure-context checks always
# run, against the server above (no speech files in its state dir). The rest
# need the real model: set STT_DATA to a state directory that
# `comebye stt fetch --state-dir DIR` filled (about 290 MB, not downloaded
# here), and a second server is started on it. STT_GPU=1 runs them on the GPU.
inst=""
if [ -n "${STT_DATA:-}" ]; then
  mkdir -p "$tmp/state-stt"
  ln -s "$STT_DATA/stt" "$tmp/state-stt/stt"
  iport=$((port + 100))
  "$tmp/comebye" -sock "$tmp/h.sock" -addr "127.0.0.1:$iport" -insecure-no-auth -push=false \
    -state-dir "$tmp/state-stt" -host-name uitest -recall-index "$tmp/recall.db" >"$out/comebye-stt.log" 2>&1 &
  for _ in $(seq 50); do curl -sf "http://127.0.0.1:$iport/api/agents" >/dev/null && break; sleep 0.1; done
  inst="http://127.0.0.1:$iport"
fi
node stt.mjs "http://127.0.0.1:$port" "$out" $inst || rc=1
exit $rc
