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

# SCHEME=dark|light picks the emulated prefers-color-scheme (CI runs one per
# job; dark also serves pages from a second listener unless PAGES_LISTENER
# says otherwise, light from the app's origin). Unset, the whole
# run happens twice on fresh servers (state left by one pass would skew the
# other): dark in OUT_DIR, then light in OUT_DIR/light.
if [ -z "${SCHEME:-}" ]; then
  me="${BASH_SOURCE[0]}"; rc=0
  o="${2:-$(mktemp -d)}"
  # The dark pass serves pages from a second listener on its own origin
  # (-pages-listen); the light pass serves them from the app's origin (the
  # default), so both set-ups are driven in a browser.
  SCHEME=dark PAGES_LISTENER=1 "$me" "${1:-}" "$o" || rc=1
  SCHEME=light PAGES_LISTENER=0 PORT=$(( ${PORT:-18741} + 1 )) "$me" "${1:-}" "$o/light" || rc=1
  exit $rc
fi

# SCRIPTS="drive sidepanel" runs only those scripts (a name per tools/uitest/<name>.mjs); unset runs all.
# The suites (smoke, behaviour, visual) are SCRIPTS lists in tools/ci/suites.sh.
want() { [ -z "${SCRIPTS:-}" ] || [[ " $SCRIPTS " == *" $1 "* ]]; }

# Set up for the pass picked above: the dark pass uses the pages listener.
if [ -z "${PAGES_LISTENER:-}" ]; then
  [ "$SCHEME" = dark ] && PAGES_LISTENER=1 || PAGES_LISTENER=0
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
app="$(cd "${1:-$root}" && pwd)"
out="${2:-$(mktemp -d)}"
port="${PORT:-18741}"
tmp="$(mktemp -d)"
# The servers started here must never prune the real /tmp (their fake herdr has no live agents).
export COMEBYE_SCRATCH_DIR="$tmp/scratch"; mkdir -p "$COMEBYE_SCRATCH_DIR"
mkdir -p "$out"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$tmp"' EXIT

(cd "$here" && [ -d node_modules/playwright-core ] || npm ci --no-audit --no-fund --silent)

(cd "$root" && go build -o "$tmp/fake-herdr" ./tools/devserver)
(cd "$app" && go build -o "$tmp/comebye" .)

# shellcheck disable=SC2086
"$tmp/fake-herdr" -sock "$tmp/h.sock" -dir "$tmp" ${FAKE_ARGS--trust-prompt} >"$out/fake-herdr.log" 2>&1 &
for _ in $(seq 300); do [ -S "$tmp/h.sock" ] && break; sleep 0.1; done

"$tmp/fake-herdr" -recall-fixture "$tmp/recall.db" 2>"$out/recall-fixture.log"

# Something else already listening on the port would answer the readiness loop below and the driver would then
# test (or hang on) that: refuse to start instead.
if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
  echo "port $port is already in use by another process: set PORT=<free port> (the light pass uses PORT+1)" >&2
  exit 1
fi

pages_args=()
pages_url=""
if [ "${PAGES_LISTENER:-0}" = 1 ]; then
  pport="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')"
  pages_url="http://127.0.0.1:$pport"
  pages_args=(-pages-listen "127.0.0.1:$pport" -pages-url "$pages_url" -public-url "http://127.0.0.1:$port")
fi
"$tmp/comebye" -sock "$tmp/h.sock" -addr "127.0.0.1:$port" -insecure-no-auth -test-mode -push=false "${pages_args[@]}" \
  -state-dir "$tmp/state" -host-name uitest -recall-index "$tmp/recall.db" -release-simulate >"$out/comebye.log" 2>&1 &
for _ in $(seq 300); do curl -sf "http://127.0.0.1:$port/api/agents" >/dev/null && break; sleep 0.1; done

echo "app: $app ($(git -C "$app" log -1 --format='%h %s'))"
cd "$here"
rc=0
if want drive; then node drive.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi

# Markdown rendering in the chat, in a real browser (its own debugging port, so suites side by side do not clash).
if want markdown; then node "$root/web/test/markdown.test.mjs" "http://127.0.0.1:$port/" --chrome "${CHROME:-google-chrome}" --port $((port + 500)) || rc=1; fi

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
  "$tmp/comebye" -sock "$tmp/h.sock" -addr "127.0.0.1:$iport" -insecure-no-auth -test-mode -push=false \
    -state-dir "$tmp/state-stt" -host-name uitest -recall-index "$tmp/recall.db" >"$out/comebye-stt.log" 2>&1 &
  for _ in $(seq 300); do curl -sf "http://127.0.0.1:$iport/api/agents" >/dev/null && break; sleep 0.1; done
  inst="http://127.0.0.1:$iport"
fi
if want stt; then node stt.mjs "http://127.0.0.1:$port" "$out" $inst || rc=1 ; fi

# The Copy strip on code blocks (clipboard read back in headless Chrome).
if want copycode; then node copycode.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
if want fontsetting; then node fontsetting.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# Buttons and the one focus ring (design language slice 4).
if want buttons; then node buttons.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# Chips, cards and empty states share one shape (design language slice 5).
if want chips; then node chips.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# Segmented controls, menus and sheets, notices, stacking order (design language slice 6).
if want controls; then node controls.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# Tap-to-copy values, whole-message copy and the other copy buttons.
if want copytap; then node copytap.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# Decision sets in the To do panel: the card, the phone stepper, conflicts, drafts and Submit.
if want decisionsets; then node decisionsets.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
if want chatswitch; then node chatswitch.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
if want parked; then node parked.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
if want quietmark; then node quietmark.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# The Pages checks publish with the real CLI, so they need the binary and its state.
if want pages; then PAGES_URL="$pages_url" COMEBYE_BIN="$tmp/comebye" COMEBYE_STATE_DIR="$tmp/state" node pages.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# The page starters (comebye pages new): published by the real CLI, opened in the viewer under the real CSP.
if want starters; then COMEBYE_BIN="$tmp/comebye" COMEBYE_STATE_DIR="$tmp/state" node starters.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# Comments on a page: select, comment, send; and what the viewer refuses from a hostile page.
if want pagecomments; then COMEBYE_BIN="$tmp/comebye" COMEBYE_STATE_DIR="$tmp/state" node pagecomments.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# Revision marks: data-revision blocks outlined from the Page menu.
if want pagerevisions; then COMEBYE_BIN="$tmp/comebye" COMEBYE_STATE_DIR="$tmp/state" node pagerevisions.mjs "http://127.0.0.1:$port" "$out" || rc=1 ; fi
# A release item closes itself once the fleet runs its version: a second build with a version stamped in, its own state.
if want releaseauto; then
  (cd "$app" && go build -ldflags "-X main.version=v100.0.0" -o "$tmp/comebye-stamped" .)
  COMEBYE_STAMPED_BIN="$tmp/comebye-stamped" FAKE_SOCK="$tmp/h.sock" AUTO_PORT=$((port + 400)) node releaseauto.mjs "$out" || rc=1
fi
# herdr calls a busy agent blocked: its own fake herdr and hub, so the shared fleet's agent count is untouched.
"$tmp/fake-herdr" -sock "$tmp/fb.sock" -dir "$tmp/fb" -false-blocked >"$out/fake-herdr-fb.log" 2>&1 &
for _ in $(seq 300); do [ -S "$tmp/fb.sock" ] && break; sleep 0.1; done
fport=$((port + 200))
"$tmp/comebye" -sock "$tmp/fb.sock" -addr "127.0.0.1:$fport" -insecure-no-auth -test-mode -push=false \
  -state-dir "$tmp/state-fb" -host-name uitest >"$out/comebye-fb.log" 2>&1 &
for _ in $(seq 300); do curl -sf "http://127.0.0.1:$fport/api/agents" >/dev/null && break; sleep 0.1; done
if want falseblocked; then node falseblocked.mjs "http://127.0.0.1:$fport" "$out" || rc=1 ; fi
# Claude's auto-mode prompt with the "will automatically block" countdown: its own fake herdr and hub.
"$tmp/fake-herdr" -sock "$tmp/ab.sock" -dir "$tmp/ab" -autoblock >"$out/fake-herdr-ab.log" 2>&1 &
for _ in $(seq 300); do [ -S "$tmp/ab.sock" ] && break; sleep 0.1; done
abport=$((port + 250))
"$tmp/comebye" -sock "$tmp/ab.sock" -addr "127.0.0.1:$abport" -insecure-no-auth -test-mode -push=false \
  -state-dir "$tmp/state-ab" -host-name uitest >"$out/comebye-ab.log" 2>&1 &
for _ in $(seq 300); do curl -sf "http://127.0.0.1:$abport/api/agents" >/dev/null && break; sleep 0.1; done
if want autoblock; then node autoblock.mjs "http://127.0.0.1:$abport" "$out" || rc=1 ; fi
# Answering that kind of to-do from the To do panel: a hub that files the item after 2 s, on its own fake herdr.
"$tmp/fake-herdr" -sock "$tmp/ta.sock" -dir "$tmp/ta" -autoblock >"$out/fake-herdr-ta.log" 2>&1 &
for _ in $(seq 300); do [ -S "$tmp/ta.sock" ] && break; sleep 0.1; done
taport=$((port + 260))
"$tmp/comebye" -sock "$tmp/ta.sock" -addr "127.0.0.1:$taport" -insecure-no-auth -test-mode -push=false -auto-todo-blocked-after 2s \
  -state-dir "$tmp/state-ta" -host-name uitest >"$out/comebye-ta.log" 2>&1 &
for _ in $(seq 300); do curl -sf "http://127.0.0.1:$taport/api/agents" >/dev/null && break; sleep 0.1; done
if want todoanswer; then node todoanswer.mjs "http://127.0.0.1:$taport" "$out" || rc=1 ; fi
# The Doc and Diff side panels: an agent in a real git worktree (a temp HOME, under a dot-directory), declaring its document
# with the real CLI; its own fake herdr and hub so the shared fleet's agent count is untouched.
pdir="$tmp/phome"; mkdir -p "$pdir"
"$tmp/fake-herdr" -sock "$tmp/fp.sock" -dir "$tmp/fp" -panel-dir "$pdir/.worktrees/proj/feat" >"$out/fake-herdr-panel.log" 2>&1 &
for _ in $(seq 300); do [ -S "$tmp/fp.sock" ] && break; sleep 0.1; done
pport=$((port + 350))
HOME="$pdir" "$tmp/comebye" -sock "$tmp/fp.sock" -addr "127.0.0.1:$pport" -insecure-no-auth -test-mode -push=false \
  -state-dir "$tmp/state-panel" -host-name uitest >"$out/comebye-panel.log" 2>&1 &
for _ in $(seq 300); do curl -sf "http://127.0.0.1:$pport/api/agents" >/dev/null && break; sleep 0.1; done
if want sidepanel; then PANEL_HOME="$pdir" COMEBYE_BIN="$tmp/comebye" COMEBYE_STATE_DIR="$tmp/state-panel" node sidepanel.mjs "http://127.0.0.1:$pport" "$out" || rc=1 ; fi
# The Peers screen and the host defaults: real servers (a hub, a peer, a peer that is down) on the same fake herdr.
if want peers; then FAKE_SOCK="$tmp/h.sock" COMEBYE_BIN="$tmp/comebye" node peers.mjs "$out" || rc=1 ; fi
# The rolling rollout end to end: a simulated hub and three simulated peers, the hub killed and restarted mid-rollout.
if want rolloutlive; then FAKE_SOCK="$tmp/h.sock" COMEBYE_BIN="$tmp/comebye" node rolloutlive.mjs "$out" || rc=1 ; fi
# Adding a host, slice 1: a hub with a default provisioner (a peer running a stand-in for the worker tool) and the price browser.
if want provision; then FAKE_SOCK="$tmp/h.sock" COMEBYE_BIN="$tmp/comebye" node provision.mjs "$out" || rc=1 ; fi
# Slice 3: hosts built from the app, their spend, and Destroy… (the real path on, against a stand-in worker).
if want provhosts; then FAKE_SOCK="$tmp/h.sock" COMEBYE_BIN="$tmp/comebye" node provhosts.mjs "$out" || rc=1 ; fi
# The Usage screen: a hub and a peer, each with fixture session files (counts only), a ledger, and a coordinator.
if want usage; then FAKE_HERDR="$tmp/fake-herdr" COMEBYE_BIN="$tmp/comebye" node usage.mjs "$out" || rc=1 ; fi
exit $rc
