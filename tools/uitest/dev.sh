#!/usr/bin/env bash
# A long-running dev server for looking at the web app while you change it.
#
#   tools/uitest/dev.sh start [--port P]   build, start, seed, print the URL, return
#   tools/uitest/dev.sh restart            rebuild the Go side and restart (same port)
#   tools/uitest/dev.sh stop               kill everything it started, remove its temp dir
#   tools/uitest/dev.sh status | url | logs
#
# It starts the fake herdr (tools/devserver -fixtures: workers, a coordinator,
# home agents, a blocked dialog, long names, a marked session) and comebye
# built from THIS checkout, plus a second fake herdr + comebye as a peer host
# ("peerbox"), seeded with a coordinator, a mark and a briefing (seed.mjs).
# web/ is served from disk (COMEBYE_DEV_WEB_DIR): edit web/*.js|css|html and
# reload, no rebuild. Go changes need `restart`. State lives in a mktemp dir
# recorded in tools/uitest/.dev-run (git-ignored), so each checkout has its
# own server and several agents can run at once on different ports.
# Look at it with shot.mjs (see CONTRIBUTING.md). Needs go, node, google-chrome.
set -Eeuo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
rec="$here/.dev-run"

run_dir() { [ -f "$rec" ] && cat "$rec" || true; }
alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }
free_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }

running() { # is the recorded server up?
  local d; d="$(run_dir)"
  [ -n "$d" ] && [ -f "$d/pids" ] && alive "$(head -1 "$d/pids")"
}

spawn() { # spawn NAME LOGFILE CMD...: a detached background process, pid recorded
  local name="$1" log="$2"; shift 2
  setsid nohup "$@" >"$log" 2>&1 </dev/null &
  echo "$!" >>"$d/pids"
  echo "$name" >>"$d/names"
}

wait_for() { # wait_for DESCRIPTION CMD...: up to 10s
  local what="$1"; shift
  for _ in $(seq 100); do "$@" >/dev/null 2>&1 && return 0; sleep 0.1; done
  echo "dev: timed out waiting for $what (logs in $d)" >&2
  return 1
}

start_servers() { # uses $d, $port, $peerport
  : >"$d/pids"; : >"$d/names"
  # Token usage: session files for the hub and the peer (counts only), and the ledger entries of the hub's workers.
  node "$here/usagefix.mjs" "$d/usage" >/dev/null
  mkdir -p "$d/state" && cp "$d/usage/ledger.json" "$d/state/ledger.json"
  local web="$root/web" peer_url="http://127.0.0.1:$peerport"
  # The peer host first, so the hub can reach it.
  spawn peer-herdr "$d/peer-herdr.log" "$d/fake-herdr" -sock "$d/peer.sock" -dir "$d/peer" -fixtures -fixture-host peerbox
  wait_for "the peer's fake herdr" test -S "$d/peer.sock"
  COMEBYE_ARCHIVE_CLAUDE_DIR="$d/usage/peer/claude" COMEBYE_ARCHIVE_PI_DIR="$d/usage/peer/pi" COMEBYE_USAGE_INTERVAL=5s COMEBYE_DEV_WEB_DIR="$web" spawn peer "$d/peer.log" "$d/comebye" -sock "$d/peer.sock" -addr "127.0.0.1:$peerport" \
    -insecure-no-auth -push=false -state-dir "$d/peer-state" -host-name peerbox -ledger=false
  wait_for "the peer" curl -sf "$peer_url/api/agents"
  # A synthetic transcript index, for the Transcripts screen. Needs sqlite3.
  [ -f "$d/recall.db" ] || "$d/fake-herdr" -recall-fixture "$d/recall.db" || echo "dev: no transcript fixture (is sqlite3 installed?)" >&2
  spawn herdr "$d/herdr.log" "$d/fake-herdr" -sock "$d/h.sock" -dir "$d/tx" -fixtures
  wait_for "the fake herdr" test -S "$d/h.sock"
  COMEBYE_ARCHIVE_CLAUDE_DIR="$d/usage/hub/claude" COMEBYE_ARCHIVE_PI_DIR="$d/usage/hub/pi" COMEBYE_USAGE_INTERVAL=5s COMEBYE_DEV_WEB_DIR="$web" COMEBYE_TEST_INSECURE_PEERS=1 spawn comebye "$d/comebye.log" "$d/comebye" -sock "$d/h.sock" \
    -addr "127.0.0.1:$port" -insecure-no-auth -push=false -state-dir "$d/state" -host-name devbox -peers "peerbox=$peer_url" -recall-index "$d/recall.db"
  wait_for "comebye" curl -sf "http://127.0.0.1:$port/api/agents"
  # the peer's agents arrive on the hub's next poll
  wait_for "the peer's agents" bash -c "curl -sf http://127.0.0.1:$port/api/agents | grep -q peerbox-build"
  node "$here/seed.mjs" "http://127.0.0.1:$port" >"$d/seed.log" 2>&1 || { cat "$d/seed.log" >&2; return 1; }
}

build() {
  (cd "$root" && go build -o "$d/fake-herdr" ./tools/devserver && go build -o "$d/comebye" .)
  (cd "$here" && [ -d node_modules/playwright-core ] || npm ci --no-audit --no-fund --silent)
}

do_stop() {
  local d; d="$(run_dir)"
  if [ -n "$d" ] && [ -f "$d/pids" ]; then
    while read -r pid; do
      if alive "$pid"; then kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true; fi
    done <"$d/pids"
    for _ in $(seq 30); do
      local any=0
      while read -r pid; do alive "$pid" && any=1; done <"$d/pids"
      [ $any = 0 ] && break; sleep 0.1
    done
    while read -r pid; do alive "$pid" && kill -9 "$pid" 2>/dev/null || true; done <"$d/pids"
  fi
  [ -n "$d" ] && case "$d" in */comebye-dev.*) rm -rf "$d" ;; esac
  rm -f "$rec"
}

cmd="${1:-start}"; shift || true
case "$cmd" in
start|restart)
  port=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --port) port="$2"; shift 2 ;;
      *) echo "dev: unknown option $1" >&2; exit 2 ;;
    esac
  done
  if [ "$cmd" = restart ]; then
    d="$(run_dir)"
    if [ -n "$d" ] && [ -f "$d/port" ]; then port="${port:-$(cat "$d/port")}"; fi
    do_stop
  elif running; then
    echo "already running: $(cat "$(run_dir)/url") (dev.sh stop first, or restart)"; exit 0
  fi
  d="$(mktemp -d -t comebye-dev.XXXXXX)"
  echo "$d" >"$rec"
  trap 'do_stop' ERR
  build
  port="${port:-$(free_port)}"; peerport="$(free_port)"
  echo "$port" >"$d/port"
  start_servers
  trap - ERR
  echo "http://127.0.0.1:$port" >"$d/url"
  echo "dev server: http://127.0.0.1:$port   (web/ served from disk: edit and reload; Go changes: dev.sh restart)"
  echo "  logs: $d/*.log   stop: tools/uitest/dev.sh stop"
  ;;
stop) do_stop; echo "stopped" ;;
status) if running; then echo "running: $(cat "$(run_dir)/url") ($(run_dir))"; else echo "not running"; exit 1; fi ;;
url) running && cat "$(run_dir)/url" || { echo "dev: not running" >&2; exit 1; } ;;
logs) d="$(run_dir)"; [ -n "$d" ] && tail -n 40 "$d"/*.log || { echo "dev: not running" >&2; exit 1; } ;;
*) echo "usage: dev.sh start [--port P] | restart | stop | status | url | logs" >&2; exit 2 ;;
esac
