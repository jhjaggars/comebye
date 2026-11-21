#!/usr/bin/env bash
# The scheduled sweep and its clean-up (internal/server/sweeploop.go, sweepclean.go, leftovers.go)
# against a throwaway hub: a fake herdr (tools/devserver, no real session), a throwaway bare git
# "origin" in a temp dir (never a real remote), and a seeded ledger of finished workers:
#   6 merged and clean (removed by the loop), 2 pushed but not merged, 1 dirty, 1 with local commits
#   (all four stay), and three marked sessions whose answer lists something left:
#   tool-runs-s6-keys (delete origin/<own branch>, in main: branch deleted, session removed),
#   extra-commit (the same words but the backup has a commit main lacks: kept) and stop-server
#   (a process to stop: kept).
# The hub runs with -test-mode, so its loop ticks every second and a mark counts after 3 s. Then
# sweeploop.mjs checks what was removed and deleted, that no To do is filed, the board line and the
# removals review, and takes the screenshots at 390 and 1280 px.
#
#   tools/uitest/sweeploop.sh [OUT_DIR]      (needs go, node, git, google-chrome)
set -Eeuo pipefail
[ -n "${COMEBYE_CAPPED:-}" ] || exec "$(dirname "${BASH_SOURCE[0]}")/../ci/capped.sh" "${BASH_SOURCE[0]}" "$@"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="${1:-$(mktemp -d)}"; mkdir -p "$out"
d="$(mktemp -d -t comebye-sweeploop.XXXXXX)"
pids=()
cleanup() { for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; sleep 0.3; rm -rf "$d"; }
trap cleanup EXIT
(cd "$here" && [ -d node_modules/playwright-core ] || npm ci --no-audit --no-fund --silent)
(cd "$root" && go build -o "$d/fake-herdr" ./tools/devserver && go build -o "$d/comebye" .)

# gh is not available (and must not be asked about real PRs): a stub that says no pull request is open.
mkdir -p "$d/bin"; printf '#!/bin/sh\necho "[]"\n' >"$d/bin/gh"; chmod +x "$d/bin/gh"
export PATH="$d/bin:$PATH"

# --- git fixtures: an origin, a clone with worktrees ---
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
git init -q --bare -b main "$d/origin.git"
git clone -q "$d/origin.git" "$d/proj" 2>/dev/null
git -C "$d/proj" checkout -q -b main 2>/dev/null || true
echo base >"$d/proj/README"; git -C "$d/proj" add README; git -C "$d/proj" commit -q -m base; git -C "$d/proj" push -q origin main
wt="$d/worktrees/proj"; mkdir -p "$wt"
names=(); declare -A kind
mk() { # mk NAME KIND
  local n="$1" k="$2"
  git -C "$d/proj" worktree add -q -b "$n" "$wt/$n" main
  case "$k" in
    merged) echo "$n" >"$wt/$n/$n.txt"; git -C "$wt/$n" add .; git -C "$wt/$n" commit -q -m "$n"; git -C "$wt/$n" push -q origin "$n"
            git -C "$d/proj" merge -q --no-edit "$n"; git -C "$d/proj" push -q origin main ;;
    pushed) echo "$n" >"$wt/$n/$n.txt"; git -C "$wt/$n" add .; git -C "$wt/$n" commit -q -m "$n"; git -C "$wt/$n" push -q origin "$n" ;;
    dirty)  echo wip >"$wt/$n/wip.txt" ;;
    local)  echo "$n" >"$wt/$n/$n.txt"; git -C "$wt/$n" add .; git -C "$wt/$n" commit -q -m "$n" ;;
  esac
  names+=("$n"); kind[$n]="$k"
}
for n in fix-header fix-footer add-cache drop-flag rename-api bump-deps; do mk "$n" merged; done
mk open-pr-a pushed; mk open-pr-b pushed; mk half-done dirty; mk local-only local
B1=feat/tool-runs-6-keyboard
# The example: the backup (pre-rebase) is pushed, then main gets the same change under another SHA.
git -C "$d/proj" worktree add -q -b "$B1" "$wt/s6" main
echo keyboard >"$wt/s6/keys.txt"; git -C "$wt/s6" add .; git -C "$wt/s6" commit -q -m keyboard; git -C "$wt/s6" push -q origin "$B1"
git -C "$d/proj" cherry-pick "$B1" >/dev/null; git -C "$d/proj" push -q origin main
# A backup with one commit main lacks.
git -C "$d/proj" worktree add -q -b feat/extra "$wt/extra" main
echo one >"$wt/extra/one.txt"; git -C "$wt/extra" add .; git -C "$wt/extra" commit -q -m "part one"; git -C "$wt/extra" push -q origin feat/extra
git -C "$d/proj" cherry-pick feat/extra >/dev/null; git -C "$d/proj" push -q origin main
echo two >"$wt/extra/two.txt"; git -C "$wt/extra" add .; git -C "$wt/extra" commit -q -m "part two"; git -C "$wt/extra" push -q origin feat/extra
git -C "$d/proj" worktree add -q -b feat/server "$wt/server" main
git -C "$d/proj" fetch -q origin
echo "$B1" >"$d/b1"

# --- the seeded ledger and the settings ---
state="$d/state"; mkdir -p "$state"
node - "$d" "$state" "${names[*]}" <<'JS'
const [d, state, list] = process.argv.slice(2);
const fs = require("fs");
const now = Date.now();
const entries = list.split(" ").map((n, i) => ({
  id: "h" + String(i + 1).padStart(11, "0"), host: "devbox", repo: d + "/proj", repo_name: "proj", branch: n, base: "main", kind: "claude",
  agent_name: n, agent_id: "devbox/w" + (50 + i) + ":p1", pane_id: "w" + (50 + i) + ":p1", workspace_id: "w" + (50 + i),
  worktree: d + "/worktrees/proj/" + n, brief: "fixture", status: "finished", saw_working: true,
  created_at: new Date(now - 6 * 3600e3).toISOString(), finished_at: new Date(now - 3 * 3600e3).toISOString(),
  final_report: "Done: " + n, report_source: "explicit" }));
const ago = (h) => new Date(now - h * 3600e3).toISOString();
const left = [
  { n: "tool-runs-s6-keys", b: "feat/tool-runs-6-keyboard", dir: "s6", text: "Delete the backup branch `origin/feat/tool-runs-6-keyboard`. It still points at the pre-rebase commit, and the same content is already in main. This waits on a go-ahead." },
  { n: "extra-commit", b: "feat/extra", dir: "extra", text: "Delete the backup branch `origin/feat/extra`. The same content is already in main. This waits on a go-ahead." },
  { n: "stop-server", b: "feat/server", dir: "server", text: "Stop the dev server I started (pid 4242), then close my PR." },
];
left.forEach((s, j) => { const i = entries.length + j; entries.push({
  id: "h" + String(i + 1).padStart(11, "0"), host: "devbox", repo: d + "/proj", repo_name: "proj", branch: s.b, base: "main", kind: "claude",
  agent_name: s.n, agent_id: "devbox/w" + (50 + i) + ":p1", pane_id: "w" + (50 + i) + ":p1", workspace_id: "w" + (50 + i),
  worktree: d + "/worktrees/proj/" + s.dir, brief: "fixture", status: "ready_to_remove", saw_working: true,
  created_at: ago(80), finished_at: ago(60), final_report: "Done: " + s.n, report_source: "explicit",
  mark: { agent_id: "devbox/w" + (50 + i) + ":p1", host: "devbox", name: s.n, by: "coordinator", reason: "landed, CI green", at: ago(48),
    ask: { state: "unsafe", reason: "see items", at: ago(26), asked_at: ago(26), items: [{ type: "cleanup", text: s.text }] } } }); });
fs.writeFileSync(state + "/ledger.json", JSON.stringify({ version: 1, entries, marks: [] }));
fs.writeFileSync(state + "/settings.json", JSON.stringify({ rev: 1, sweep_loop: { enabled: true, every_minutes: 15 } }));
JS

# --- the hub, on a fake herdr ---
free_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
port="$(free_port)"
export COMEBYE_SCRATCH_DIR="$d/scratch"; mkdir -p "$COMEBYE_SCRATCH_DIR"
setsid "$d/fake-herdr" -sock "$d/h.sock" -dir "$d/tx" >"$d/herdr.log" 2>&1 & pids+=("$!")
for _ in $(seq 100); do [ -S "$d/h.sock" ] && break; sleep 0.1; done
COMEBYE_DEV_WEB_DIR="$root/web" setsid "$d/comebye" -sock "$d/h.sock" -addr "127.0.0.1:$port" -insecure-no-auth -test-mode -push=false \
  -state-dir "$state" -host-name devbox -auto-remove=false -launch-root "$d" -worktree-root "$d/worktrees" >"$d/comebye.log" 2>&1 & pids+=("$!")
for _ in $(seq 100); do curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null && break; sleep 0.1; done
rc=0
node "$here/sweeploop.mjs" "http://127.0.0.1:$port" "$out" "$d" "$(cat "$d/b1")" || rc=$?
[ $rc = 0 ] || { echo "--- hub log"; tail -30 "$d/comebye.log"; }
exit $rc
