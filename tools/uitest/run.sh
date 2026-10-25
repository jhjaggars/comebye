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
"$tmp/fake-herdr" -sock "$tmp/h.sock" -dir "$tmp" ${FAKE_ARGS:-} >"$out/fake-herdr.log" 2>&1 &
for _ in $(seq 50); do [ -S "$tmp/h.sock" ] && break; sleep 0.1; done

"$tmp/comebye" -sock "$tmp/h.sock" -addr "127.0.0.1:$port" -insecure-no-auth -push=false \
  -state-dir "$tmp/state" -host-name uitest >"$out/comebye.log" 2>&1 &
for _ in $(seq 50); do curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null && break; sleep 0.1; done

echo "app: $app ($(git -C "$app" log -1 --format='%h %s'))"
cd "$here"
node drive.mjs "http://127.0.0.1:$port" "$out"
