#!/usr/bin/env bash
# The archived state (internal/server/archived.go, issue #86 slice 5) against a throwaway hub: a
# fake herdr (tools/devserver, four idle pi agents with session files, no real session), a
# throwaway bare git "origin" in a temp dir (never a real remote), and a seeded ledger of four
# finished workers, all idle for days:
#   pi-a  feat/unmerged  one commit pushed, not merged
#   pi-b  poc/demo       one local commit (a delivered kind)
#   pi-c  feat/dirty     an uncommitted file
#   pi-d  feat/rust      Rust-shaped: Cargo.toml, an ignored target/ holding 50 MB, untracked notes
# The hub runs with -test-mode -test-archive-after 3s, so its loop ticks every second and archives
# after 3 s. Then, through the CLI binary only: all four are archived; their worktree source and
# branches are still there; target/ is gone and `comebye archived --json` reports about 50 MB
# freed; their fake panes are closed, with park records; `comebye archived restore pi-a --resume`
# brings one back. The CLI talks to the throwaway hub alone (COMEBYE_URL and its state dir).
#
#   tools/uitest/archived.sh      (needs go, git, python3, jq)
set -Eeuo pipefail
[ -n "${COMEBYE_CAPPED:-}" ] || exec "$(dirname "${BASH_SOURCE[0]}")/../ci/capped.sh" "${BASH_SOURCE[0]}" "$@"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
d="$(mktemp -d -t comebye-archived.XXXXXX)"
pids=()
cleanup() { for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; sleep 0.3; rm -rf "$d"; }
trap cleanup EXIT
(cd "$root" && go build -o "$d/fake-herdr" ./tools/devserver && go build -o "$d/comebye" .)
mkdir -p "$d/bin"; printf '#!/bin/sh\necho "[]"\n' >"$d/bin/gh"; chmod +x "$d/bin/gh"
export PATH="$d/bin:$PATH"
fail() { echo "FAIL: $*" >&2; exit 1; }

# --- git fixtures ---
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
git init -q --bare -b main "$d/origin.git"
git clone -q "$d/origin.git" "$d/proj" 2>/dev/null
git -C "$d/proj" checkout -q -b main 2>/dev/null || true
printf 'target/\nnode_modules/\n.env\n' >"$d/proj/.gitignore"; echo base >"$d/proj/README"
git -C "$d/proj" add .; git -C "$d/proj" commit -q -m base; git -C "$d/proj" push -q origin main
wt="$d/worktrees/proj"; mkdir -p "$wt"
add() { git -C "$d/proj" worktree add -q -b "$2" "$wt/$1" main; }
add a feat/unmerged; echo a >"$wt/a/a.txt"; git -C "$wt/a" add .; git -C "$wt/a" commit -q -m a; git -C "$wt/a" push -q origin feat/unmerged
add b poc/demo;      echo b >"$wt/b/b.txt"; git -C "$wt/b" add .; git -C "$wt/b" commit -q -m b
add c feat/dirty;    echo wip >"$wt/c/wip.txt"
add d feat/rust;     printf '[package]\nname = "r"\n' >"$wt/d/Cargo.toml"; git -C "$wt/d" add .; git -C "$wt/d" commit -q -m rust
mkdir -p "$wt/d/target/debug"; head -c 52428800 /dev/zero >"$wt/d/target/debug/big.rlib"
printf 'Signature: 8a477f597d28d172789f06886806bc55\n' >"$wt/d/target/CACHEDIR.TAG"
echo notes >"$wt/d/notes.md"; echo SECRET=1 >"$wt/d/.env"
git -C "$d/proj" fetch -q origin

# --- the seeded ledger ---
state="$d/state"; mkdir -p "$state"
python3 - "$d" "$state" <<'PY'
import json, sys, datetime
d, state = sys.argv[1:]
now = datetime.datetime.now(datetime.timezone.utc)
ago = lambda days: (now - datetime.timedelta(days=days)).isoformat()
rows = [("a", "feat/unmerged", "pi-a"), ("b", "poc/demo", "pi-b"), ("c", "feat/dirty", "pi-c"), ("d", "feat/rust", "pi-d")]
entries = [{"id": "h%011d" % (i + 1), "host": "devbox", "repo": d + "/proj", "repo_name": "proj", "branch": b, "base": "main",
            "kind": "pi", "agent_name": n, "agent_id": "devbox/w%d:p1" % (20 + i), "pane_id": "w%d:p1" % (20 + i), "workspace_id": "w%d" % (20 + i),
            "worktree": d + "/worktrees/proj/" + w, "brief": "fixture", "status": "finished",
            "created_at": ago(12), "finished_at": ago(9), "final_report": "Done: " + n, "report_source": "explicit"}
           for i, (w, b, n) in enumerate(rows)]
json.dump({"version": 1, "entries": entries, "marks": []}, open(state + "/ledger.json", "w"))
json.dump({"rev": 1, "sweep_loop": {"enabled": True, "every_minutes": 15}}, open(state + "/settings.json", "w"))
PY

# --- the hub, on a fake herdr ---
port="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')"
export COMEBYE_SCRATCH_DIR="$d/scratch"; mkdir -p "$COMEBYE_SCRATCH_DIR"
setsid "$d/fake-herdr" -sock "$d/h.sock" -dir "$d/tx" -pi-dir "$wt/a,$wt/b,$wt/c,$wt/d" >"$d/herdr.log" 2>&1 & pids+=("$!")
for _ in $(seq 100); do [ -S "$d/h.sock" ] && break; sleep 0.1; done
COMEBYE_DEV_WEB_DIR="$root/web" setsid "$d/comebye" -sock "$d/h.sock" -addr "127.0.0.1:$port" -insecure-no-auth -test-mode -test-archive-after 3s -push=false \
  -state-dir "$state" -host-name devbox -auto-remove=false -park-mode off -launch-root "$d" -worktree-root "$d/worktrees" >"$d/comebye.log" 2>&1 & pids+=("$!")
for _ in $(seq 100); do curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null && break; sleep 0.1; done

# The CLI, pointed at the throwaway hub only (its own state dir and token), as the fake agent alpha,
# recorded as a coordinator: archiving and restoring are the user's or a coordinator's.
for _ in $(seq 50); do [ -s "$state/agent-token" ] && break; sleep 0.1; done
curl -sf -X POST "http://127.0.0.1:$port/api/roles" -H 'Content-Type: application/json' -d '{"agent_id":"devbox/w1:p1","role":"coordinator"}' >/dev/null
cb() { COMEBYE_URL="http://127.0.0.1:$port" COMEBYE_STATE_DIR="$state" HERDR_PANE_ID=w1:p1 "$d/comebye" "$@"; }

trap 'rc=$?; [ $rc = 0 ] || { echo "--- hub log"; tail -40 "$d/comebye.log"; }; cleanup' EXIT
for _ in $(seq 60); do
  n="$(cb archived --json | jq '.count')"; [ "$n" = 4 ] && break; sleep 1
done
cb archived --json >"$d/archived.json"
[ "$(jq '.count' "$d/archived.json")" = 4 ] || { cat "$d/archived.json"; fail "want 4 archived"; }
echo "--- comebye archived"; cb archived
echo "--- comebye ledger --archived"; cb ledger --archived

# The source and the branches are all there; only target/ went.
for f in a/a.txt b/b.txt c/wip.txt d/Cargo.toml d/notes.md d/.env; do [ -e "$wt/$f" ] || fail "$f was removed"; done
[ ! -e "$wt/d/target" ] || fail "target/ is still there"
for b in feat/unmerged poc/demo feat/dirty feat/rust; do git -C "$d/proj" rev-parse -q --verify "refs/heads/$b" >/dev/null || fail "branch $b went"; done
git -C "$d/proj" ls-remote --exit-code origin feat/unmerged >/dev/null || fail "origin/feat/unmerged went"
freed="$(jq '[.archived[] | select(.name=="pi-d") | .archive.build_freed][0]' "$d/archived.json")"
[ "$freed" -ge 52428800 ] && [ "$freed" -lt 53000000 ] || fail "pi-d freed $freed bytes, want about 50 MB"
total="$(jq '.build_freed' "$d/archived.json")"; echo "build output freed: $total bytes (pi-d: $freed)"
# Their panes are closed, each with a park record.
parks="$(jq -r '[.archived[].archive.park_id | select(. != null and . != "")] | length' "$d/archived.json")"
[ "$parks" = 4 ] || fail "want 4 park ids, got $parks"
live="$(curl -sf "http://127.0.0.1:$port/api/agents" | jq -r '[.. | objects | select(has("name")) | .name | select(startswith("pi-"))] | unique | join(",")')"
[ -z "$live" ] || fail "panes still open: $live"
cb parked --json | jq -e '[.parked[] | select(.why=="archive")] | length == 4' >/dev/null || { cb parked --json; fail "want 4 park records with why=archive"; }
# The board counts them and lists none of them.
board="$(curl -sf "http://127.0.0.1:$port/api/board")"
[ "$(jq '.loose.archived' <<<"$board")" = 4 ] || fail "board loose.archived = $(jq '.loose' <<<"$board")"
jq -e '[.lanes[] | select(.key != "hub_did") | .items[] | select(.type=="handoff")] | length == 0' <<<"$board" >/dev/null || fail "an archived entry is in a lane"
echo "--- hub did"; jq -r '.lanes[] | select(.key=="hub_did") | .items[] | .title' <<<"$board" | head -8

# Restore one, with its pane.
echo "--- comebye archived restore pi-a --resume"; cb archived restore pi-a --resume
st="$(cb ledger --json | jq -r '.entries[] | select(.agent_name=="pi-a") | .status')"
[ "$st" = finished ] || [ "$st" = running ] || fail "pi-a is $st after restore"
for _ in $(seq 20); do
  curl -sf "http://127.0.0.1:$port/api/agents" | jq -e '[.. | objects | select(.name? == "pi-a")] | length > 0' >/dev/null && break; sleep 0.5
done
curl -sf "http://127.0.0.1:$port/api/agents" | jq -e '[.. | objects | select(.name? == "pi-a")] | length > 0' >/dev/null || fail "pi-a was not resumed"
[ "$(cb archived --json | jq '.count')" = 3 ] || fail "want 3 archived after the restore"
echo "OK: 4 archived, source and branches kept, target/ cleared ($freed bytes), panes parked, pi-a restored and resumed"
