#!/usr/bin/env bash
# The day-in-the-life smoke: one end-to-end run of what the user and agents actually do, on a throwaway hub, a
# stand-in herdr and a bare throwaway origin. See README.md.
#
#   tools/smoke/run.sh [OUT_DIR]
#
# Needs go, node, npm, git, gpg and google-chrome (CHROME=/path to override). Screenshots and logs land in OUT_DIR
# (default: a temp dir); a failed run keeps the hub's log and the stand-in herdr's there. Exit 1 on failure.
#
# Nothing here touches your real comebye, herdr, git config or GnuPG: HOME, the hub's state, GNUPGHOME and the
# herdr socket are all under one temp directory, which is removed at the end.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="${1:-$(mktemp -d)}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
tmp="$(mktemp -d)"
# The throwaway GNUPGHOME comes first, before anything that can fail, and the exit trap stops a gpg-agent only for a GNUPGHOME inside
# $tmp (cleanup.sh): a failure at any point must never kill the user's real agent and end a live unlock.
export GNUPGHOME="$tmp/gnupg"; mkdir -m 700 "$GNUPGHOME"
. "$here/cleanup.sh"
trap 'smoke_cleanup "$tmp"' EXIT

if [ -z "${CHROME:-}" ]; then
  CHROME="$(command -v google-chrome || command -v chromium || true)"
  [ -n "$CHROME" ] || { echo "no google-chrome or chromium on this machine (set CHROME)" >&2; exit 1; }
fi
export CHROME

# A free port: another process on a fixed one would answer the readiness loop and be tested instead.
free_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
port="${PORT:-$(free_port)}"; pport="$(free_port)"

(cd "$root/tools/uitest" && [ -d node_modules/playwright-core ] || npm ci --no-audit --no-fund --silent)
(cd "$root" && go build -o "$tmp/comebye" . && go build -o "$tmp/fakeherdr" ./tools/smoke/fakeherdr && go build -o "$tmp/devserver" ./tools/devserver)
"$tmp/devserver" -recall-fixture "$tmp/recall.db" 2>"$out/recall-fixture.log" # the app asks the transcript index about a page's sessions

# The throwaway world: HOME (git identity, agents' session files), the hub's state, GnuPG, a bare origin, a repository.
export HOME="$tmp/home"; mkdir -p "$HOME"
export COMEBYE_STATE_DIR="$tmp/state"
export COMEBYE_SCRATCH_DIR="$tmp/scratch"; mkdir -p "$COMEBYE_SCRATCH_DIR"
unset XDG_CONFIG_HOME XDG_STATE_HOME HERDR_PANE_ID HERDR_SOCKET_PATH
export GIT_CONFIG_GLOBAL="$HOME/.gitconfig" GIT_CONFIG_SYSTEM=/dev/null
git config --global user.name "Smoke Test"; git config --global user.email "smoke@example.invalid"
git config --global init.defaultBranch main; git config --global commit.gpgsign false
git config --global protocol.file.allow always

mkdir -p "$tmp/code" "$tmp/bin"
git init -q --bare "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/code/proj" 2>/dev/null
(cd "$tmp/code/proj" && printf '# proj\n' >README.md && git add README.md && git commit -qm "initial" && git push -q origin main \
  && git remote set-head origin main >/dev/null)
ln -s "$here/stubs/agent" "$tmp/bin/claude"; ln -s "$here/stubs/agent" "$tmp/bin/pi"

"$tmp/fakeherdr" -sock "$tmp/h.sock" -home "$HOME" >"$out/fakeherdr.log" 2>&1 &
for _ in $(seq 100); do [ -S "$tmp/h.sock" ] && break; sleep 0.1; done

PATH="$tmp/bin:$PATH" "$tmp/comebye" -sock "$tmp/h.sock" -addr "127.0.0.1:$port" -insecure-no-auth -test-mode -push=false \
  -pages-listen "127.0.0.1:$pport" -pages-url "http://localhost:$pport" -public-url "http://localhost:$port" \
  -recall-index "$tmp/recall.db" -state-dir "$tmp/state" -host-name smoke -launch-root "$tmp/code" -worktree-root "$tmp/worktrees" \
  -auto-todo off -park-mode off -auto-remove=false >"$out/comebye.log" 2>&1 &
for _ in $(seq 300); do curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null && break; sleep 0.1; done
curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null || { echo "the hub did not start:"; tail -20 "$out/comebye.log"; exit 1; }

if [ -n "${SMOKE_HOLD:-}" ]; then # for debugging: leave the world up and print how to talk to it
  cat <<EOT
hub:  http://127.0.0.1:$port   (pages http://127.0.0.1:$pport)
env:  export HOME=$HOME GNUPGHOME=$GNUPGHOME COMEBYE_STATE_DIR=$COMEBYE_STATE_DIR COMEBYE_URL=http://127.0.0.1:$port PATH=$tmp/bin:\$PATH
Ctrl-C to tear it down.
EOT
  wait
fi
rc=0
SMOKE_TMP="$tmp" COMEBYE_BIN="$tmp/comebye" COMEBYE_URL="http://127.0.0.1:$port" PAGES_URL="http://127.0.0.1:$pport" \
  PATH="$tmp/bin:$PATH" node "$here/day.mjs" "http://127.0.0.1:$port" "$out" || rc=1
if [ "$rc" -ne 0 ]; then echo "failed: logs and screenshots in $out" >&2; fi
exit $rc
