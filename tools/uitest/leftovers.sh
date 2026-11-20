#!/usr/bin/env bash
# The sweep's leftovers (internal/server/leftovers.go) against a throwaway hub: a fake herdr (no live
# agents, so every session here is GONE), a throwaway bare git "origin" in a temp dir (never a real
# remote), and a seeded ledger of marked sessions whose answer still lists something left:
#   tool-runs-s6-keys  the real example: delete origin/<own branch>, a pre-rebase backup, same content in main
#   extra-commit       the same words, but the backup has one commit main lacks
#   stop-server        a leftover the hub cannot check (a process to stop)
# leftovers.mjs then checks the card, the batched item, both modes of the setting and that one confirm
# deletes exactly the proven branch, and takes screenshots at 390 and 1280 px.
#
#   tools/uitest/leftovers.sh [OUT_DIR]      (needs go, node, git, google-chrome or $CHROME)
set -Eeuo pipefail
[ -n "${COMEBYE_CAPPED:-}" ] || exec "$(dirname "${BASH_SOURCE[0]}")/../ci/capped.sh" "${BASH_SOURCE[0]}" "$@"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="${1:-$(mktemp -d)}"; mkdir -p "$out"
d="$(mktemp -d -t comebye-leftovers.XXXXXX)"
pids=()
cleanup() { for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; sleep 0.3; rm -rf "$d"; }
trap cleanup EXIT
(cd "$here" && [ -d node_modules/playwright-core ] || npm ci --no-audit --no-fund --silent)
(cd "$root" && go build -o "$d/fake-herdr" ./tools/devserver && go build -o "$d/comebye" .)
# gh is not available (and must not be asked about real PRs): a stub that says no pull request is open.
mkdir -p "$d/bin"; printf '#!/bin/sh\necho "[]"\n' >"$d/bin/gh"; chmod +x "$d/bin/gh"
export PATH="$d/bin:$PATH"

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
git init -q --bare -b main "$d/origin.git"
git clone -q "$d/origin.git" "$d/proj" 2>/dev/null
git -C "$d/proj" checkout -q -b main 2>/dev/null || true
seq 1 40 | sed 's/^/line /' >"$d/proj/f.txt"; git -C "$d/proj" add f.txt; git -C "$d/proj" commit -q -m base; git -C "$d/proj" push -q origin main
wt="$d/worktrees/proj"; mkdir -p "$wt"
B1=feat/tool-runs-6-keyboard
# 1: the example. The backup (pre-rebase) is pushed, then main gets the same change under another SHA.
git -C "$d/proj" worktree add -q -b "$B1" "$wt/s6" main
sed -i '3s/.*/the keyboard change/' "$wt/s6/f.txt"; git -C "$wt/s6" commit -q -am "keyboard"; git -C "$wt/s6" push -q origin "$B1"
sed -i '38s/.*/someone elses work/' "$d/proj/f.txt"; git -C "$d/proj" commit -q -am "other"
git -C "$d/proj" cherry-pick "$B1" >/dev/null; git -C "$d/proj" push -q origin main
# 2: a backup with one commit main lacks.
git -C "$d/proj" worktree add -q -b feat/extra "$wt/extra" main
sed -i '10s/.*/landed part/' "$wt/extra/f.txt"; git -C "$wt/extra" commit -q -am "part one"; git -C "$wt/extra" push -q origin feat/extra
git -C "$d/proj" cherry-pick feat/extra >/dev/null; git -C "$d/proj" push -q origin main
sed -i '20s/.*/not landed/' "$wt/extra/f.txt"; git -C "$wt/extra" commit -q -am "part two"; git -C "$wt/extra" push -q origin feat/extra
# 3: a process to stop.
git -C "$d/proj" worktree add -q -b feat/server "$wt/server" main
git -C "$d/proj" fetch -q origin

state="$d/state"; mkdir -p "$state"
node - "$d" "$state" "$B1" <<'JS'
const [d, state, b1] = process.argv.slice(2);
const fs = require("fs");
const now = Date.now(), ago = (h) => new Date(now - h * 3600e3).toISOString();
const sessions = [
  { n: "tool-runs-s6-keys", b: b1, dir: "s6", text: "Delete the backup branch `origin/" + b1 + "`. It still points at the pre-rebase commit 4e3219b, and the same content is already in main as 8551b4f. This waits on a go-ahead." },
  { n: "extra-commit", b: "feat/extra", dir: "extra", text: "Delete the backup branch `origin/feat/extra`. The same content is already in main. This waits on a go-ahead." },
  { n: "stop-server", b: "feat/server", dir: "server", text: "Stop the dev server I started (pid 4242), then close my PR." },
];
const entries = sessions.map((s, i) => ({
  id: "h" + String(i + 1).padStart(11, "0"), host: "devbox", repo: d + "/proj", repo_name: "proj", branch: s.b, base: "main", kind: "claude",
  agent_name: s.n, agent_id: "devbox/w" + (50 + i) + ":p1", pane_id: "w" + (50 + i) + ":p1", workspace_id: "w" + (50 + i),
  worktree: d + "/worktrees/proj/" + s.dir, brief: "fixture", status: "ready_to_remove", saw_working: true,
  created_at: ago(80), finished_at: ago(60), final_report: "Done: " + s.n, report_source: "explicit",
  mark: { agent_id: "devbox/w" + (50 + i) + ":p1", host: "devbox", name: s.n, by: "coordinator", reason: "landed, CI green", at: ago(48),
    ask: { state: "unsafe", reason: "see items", at: ago(26), asked_at: ago(26), items: [{ type: "cleanup", text: s.text }] } } }));
fs.writeFileSync(state + "/ledger.json", JSON.stringify({ version: 1, entries, marks: [] }));
fs.writeFileSync(state + "/settings.json", JSON.stringify({ rev: 1, sweep_loop: { enabled: true, every_minutes: 15 } }));
JS

free_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
export COMEBYE_SCRATCH_DIR="$d/scratch"; mkdir -p "$COMEBYE_SCRATCH_DIR"
setsid "$d/fake-herdr" -sock "$d/h.sock" -dir "$d/tx" >"$d/herdr.log" 2>&1 & pids+=("$!")
for _ in $(seq 100); do [ -S "$d/h.sock" ] && break; sleep 0.1; done
hub=""
start_hub() {
  port="$(free_port)"
  COMEBYE_DEV_WEB_DIR="$root/web" setsid "$d/comebye" -sock "$d/h.sock" -addr "127.0.0.1:$port" -insecure-no-auth -test-mode -push=false \
    -state-dir "$state" -host-name devbox -auto-remove=false -launch-root "$d" -worktree-root "$d/worktrees" >>"$d/comebye.log" 2>&1 & hub=$!; pids+=("$hub")
  for _ in $(seq 100); do curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null && break; sleep 0.1; done
}
rc=0
# Phase 1: the default (confirm) mode.
start_hub
node "$here/leftovers.mjs" confirm "http://127.0.0.1:$port" "$out" "$d" "$B1" || rc=$?
kill "$hub" 2>/dev/null || true; sleep 0.5
# Phase 2: auto mode, with one more provable session.
if [ $rc = 0 ]; then
  git -C "$d/proj" worktree add -q -b feat/auto "$wt/auto" main
  sed -i '30s/.*/auto change/' "$wt/auto/f.txt"; git -C "$wt/auto" commit -q -am "auto"; git -C "$wt/auto" push -q origin feat/auto
  git -C "$d/proj" cherry-pick feat/auto >/dev/null; git -C "$d/proj" push -q origin main
  node - "$d" "$state" <<'JS'
const [d, state] = process.argv.slice(2);
const fs = require("fs");
const L = JSON.parse(fs.readFileSync(state + "/ledger.json"));
const ago = (h) => new Date(Date.now() - h * 3600e3).toISOString();
const tmpl = L.entries.find((e) => e.agent_name === "extra-commit");
L.entries.push({ ...tmpl, id: "h00000000099", agent_name: "auto-case", branch: "feat/auto", worktree: d + "/worktrees/proj/auto", agent_id: "devbox/w60:p1", pane_id: "w60:p1", workspace_id: "w60",
  mark: { ...tmpl.mark, agent_id: "devbox/w60:p1", name: "auto-case", ask: { state: "unsafe", reason: "see items", at: ago(30), asked_at: ago(30),
    items: [{ type: "cleanup", text: "Delete the backup branch `origin/feat/auto`. The same content is already in main." }] } } });
fs.writeFileSync(state + "/ledger.json", JSON.stringify(L));
fs.writeFileSync(state + "/settings.json", JSON.stringify({ rev: 2, sweep_loop: { enabled: true, every_minutes: 15, auto_delete_branches: true } }));
fs.rmSync(state + "/sweeploop.json", { force: true });
JS
  start_hub
  node "$here/leftovers.mjs" auto "http://127.0.0.1:$port" "$out" "$d" "$B1" || rc=$?
fi
[ $rc = 0 ] || { echo "--- hub log"; tail -30 "$d/comebye.log"; }
exit $rc
