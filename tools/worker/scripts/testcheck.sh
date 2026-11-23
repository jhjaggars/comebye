#!/bin/bash
# Prove a worker can run comebye's tests: the pinned tools answer at their
# versions, then `go test ./internal/...` and `tools/ci/browser.sh smoke` pass
# in a checkout on the VM. Run as the worker user (stdin, or bash -c). The
# caller prepends the WK_* values; WK_QUICK=1 stops after the versions.
set -euo pipefail
: "${WK_GO_VERSION:?}" "${WK_NODE_VERSION:?}" "${WK_CHROME_VERSION:?}" "${WK_REPO_DIR:?}"
# The pinned tools first: a pane's PATH may also hold mise's Node (pi's), which is not CI's.
export PATH="/usr/local/bin:$HOME/.local/bin:/usr/bin:/bin"
# Never let go.mod's toolchain line download a different Go: the installed one must be it.
export GOTOOLCHAIN=local
# tools/ci/capped.sh caps the suites' memory with the user's systemd, which needs the user bus.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

fail=0
want() { # NAME GOT WANT
  if [ "$2" = "$3" ]; then echo "ok    $1 $2"; else echo "FAIL  $1 is '$2', want '$3'"; fail=1; fi
}
want go "$(go env GOVERSION 2>/dev/null || true)" "go$WK_GO_VERSION"
want node "$(node --version 2>/dev/null || true)" "v$WK_NODE_VERSION"
want npm "$(npm --version >/dev/null 2>&1 && echo present || echo missing)" present
want google-chrome "$(google-chrome --version 2>/dev/null | awk '{print $NF}' || true)" "$WK_CHROME_VERSION"
want /usr/bin/google-chrome "$(/usr/bin/google-chrome --version 2>/dev/null | awk '{print $NF}' || true)" "$WK_CHROME_VERSION"
want python3 "$(python3 -c 'print("present")' 2>/dev/null || echo missing)" present
want sqlite3 "$(sqlite3 -version >/dev/null 2>&1 && echo present || echo missing)" present
want gpg "$(gpg --version >/dev/null 2>&1 && echo present || echo missing)" present
[ "$fail" = 0 ] || { echo "testcheck: the toolchain is not as pinned (run: worker toolchain NAME)"; exit 1; }
[ "${WK_QUICK:-0}" = 1 ] && { echo "testcheck: toolchain ok (quick: no tests run)"; exit 0; }

[ -f "$WK_REPO_DIR/go.mod" ] && [ -x "$WK_REPO_DIR/tools/ci/browser.sh" ] ||
  { echo "testcheck: $WK_REPO_DIR is not a comebye checkout"; exit 1; }
cd "$WK_REPO_DIR"
echo "testcheck: $(git rev-parse --short HEAD 2>/dev/null || echo '?') in $WK_REPO_DIR, $(nproc) CPUs"
out=$(mktemp -d "${TMPDIR:-/tmp}/testcheck.XXXXXX")

t0=$SECONDS
if go test -p 4 ./internal/... >"$out/go-test.log" 2>&1; then
  echo "ok    go test ./internal/... ($((SECONDS - t0)) s)"
else
  tail -40 "$out/go-test.log"; echo "FAIL  go test ./internal/... (log: $out/go-test.log)"; exit 1
fi

t0=$SECONDS
if tools/ci/browser.sh smoke dark "$out/smoke" >"$out/smoke.log" 2>&1; then
  echo "ok    tools/ci/browser.sh smoke ($((SECONDS - t0)) s)"
else
  tail -40 "$out/smoke.log"; echo "FAIL  tools/ci/browser.sh smoke (logs: $out)"; exit 1
fi
rm -rf "$out"
echo "testcheck: this worker runs comebye's tests"
