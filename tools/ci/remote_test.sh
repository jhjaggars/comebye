#!/usr/bin/env bash
# Self-test of tools/ci/remote.sh, with no VM and no network: tools/ci/remote_test.sh
#
# A fake ssh on PATH insists on the provisioner's ssh config and host kv1, and runs the command in a local folder
# that plays the VM's home. The caller is a small git repository with stand-ins for capped.sh, browser.sh and
# flakes.sh that record what they saw. Covers: the send (committed, modified, staged, deleted, untracked; not
# ignored files, secret-looking files or the remotes), the run (under capped.sh, the VM's caches, GOTOOLCHAIN),
# live output, the exit status, the artifacts back, the run folder removed, the flakes argument order, a stop
# mid-run killing the suite on the VM, and the refusals (unknown host, no toolchain, wrong pins, two runs at once,
# ssh down).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fail=0; ok() { echo "ok   $1"; }; bad() { echo "FAIL $1"; fail=1; }
check() { local name="$1"; shift; if "$@"; then ok "$name"; else bad "$name"; fi; }
rc_says() { [ "$1" = "$2" ] && grep -q -- "$3" <<<"$4"; } # RC WANT PATTERN OUTPUT
command -v git >/dev/null && command -v flock >/dev/null && command -v setsid >/dev/null ||
  { echo "skip: needs git, flock and setsid"; exit 0; }

root="$(mktemp -d)"; trap 'rm -rf "$root"' EXIT
state="$root/state"; vm="$root/vm"; bin="$root/bin"; tools="$root/tools"; caller="$root/caller"
mkdir -p "$state/kv1" "$vm/etc" "$bin" "$tools" "$caller"
printf 'Host kv1\n  HostName 127.0.0.1\n' >"$state/kv1/ssh_config"
export COMEBYE_WORKER_STATE="$state" RUNNER_TEMP="$root/rt" XDG_STATE_HOME="$root/nope"
unset REMOTE_ROOT OUT_DIR JOBS SCHEME EXTRA PARALLEL CAP_MEM BASE PORT
export GIT_CONFIG_GLOBAL="$root/gitconfig" GIT_CONFIG_NOSYSTEM=1
printf '[init]\n\tdefaultBranch = main\n[user]\n\tname = t\n\temail = t@example.com\n[commit]\n\tgpgsign = false\n' >"$GIT_CONFIG_GLOBAL"

# The fake ssh: -F must be kv1's provisioner config, the host must be kv1; the command runs with HOME at the VM.
cat >"$bin/ssh" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >>"$root/ssh.log"
[ -n "\${FAKE_SSH_DOWN:-}" ] && { echo "ssh: connect: no route" >&2; exit 255; }
[ "\$1" = -F ] && [ "\$2" = "$state/kv1/ssh_config" ] || { echo "fake ssh: not the provisioner's config: \$*" >&2; exit 255; }
shift 2
while [ "\$1" = -o ]; do shift 2; done
[ "\$1" = kv1 ] || { echo "fake ssh: unknown host \$1" >&2; exit 255; }
shift
cd "$vm" && HOME="$vm" COMEBYE_TEST_RUNNER_FILE="$vm/etc/test-runner" exec bash -c "\$*"
EOF
for t in node npm google-chrome; do printf '#!/bin/sh\necho stub\n' >"$tools/$t"; done
command -v go >/dev/null || printf '#!/bin/sh\necho stub\n' >"$tools/go"
chmod +x "$bin"/* "$tools"/*
export PATH="$bin:$PATH:$tools"
pins() { printf 'go=%s\nnode=%s\nchrome=%s\n' "$1" "$2" "$3" >"$vm/etc/test-runner"; }
pins 1.2.3 4.5.6 7.8.9.10

# ---- the caller's worktree
cd "$caller" && git init -q && git remote add origin "https://someone:tok3n-SECRET@example.com/x.git" || exit 1
mkdir -p tools/ci tools/worker
cp "$here/remote.sh" tools/ci/remote.sh
cat >tools/worker/testrunner.go <<'EOF'
var testPins = testRunnerPins{
	GoVersion:     "1.2.3",
	NodeVersion:   "4.5.6",
	ChromeVersion: "7.8.9.10",
}
EOF
cat >tools/ci/capped.sh <<'EOF'
#!/bin/bash
export COMEBYE_CAPPED=1 CAPPED_BY_TEST=yes
exec "$@"
EOF
# browser.sh SUITE SCHEME OUT: records what it saw into OUT, streams a line, leaves a failure artifact, exits with
# rc.txt (an untracked file, so it also proves untracked files arrive). With wait.txt, it waits for $root/release.
cat >tools/ci/browser.sh <<EOF
#!/bin/bash
out="\$3"; mkdir -p "\$out/failures"
echo "STREAMED from the vm"
{ echo "args=\$*"; echo "pwd=\$PWD"; echo "capped=\${CAPPED_BY_TEST:-no}"; echo "toolchain=\$GOTOOLCHAIN"
  echo "gocache=\$GOCACHE"; echo "scheme=\${SCHEME:-}"; echo "jobs=\${JOBS:-}"; echo "branch=\$(git branch --show-current)"
  echo "origin_main=\$(git rev-parse --verify -q refs/remotes/origin/main >/dev/null && echo yes || echo no)"
  echo "remotes=\$(git remote -v | wc -l)"
  echo "committed=\$(cat committed.txt)"; echo "modified=\$(cat modified.txt)"; echo "staged=\$(cat staged.txt)"
  echo "new=\$(cat sub/new.txt 2>/dev/null)"; echo "tokengo=\$([ -f sub/token.go ] && echo yes)"; echo "bin=\$(od -An -tx1 data.bin | tr -d ' \n')"
  for f in deleted.txt ignored.log secret.pem api-token.txt .env; do [ -e "\$f" ] && echo "present=\$f"; done
} >"\$out/record.txt"
echo shot >"\$out/failures/check.png"
if [ -e wait.txt ]; then
  echo \$\$ >"$root/suite.pid"; touch "$root/started"
  for _ in \$(seq 200); do [ -e "$root/release" ] && break; sleep 0.1; done
fi
exit "\$(cat rc.txt 2>/dev/null || echo 0)"
EOF
cat >tools/ci/flakes.sh <<'EOF'
#!/bin/bash
mkdir -p "$OUT_DIR"; echo "args=$*" >"$OUT_DIR/flakes.txt"; echo "FLAKY none"; exit 1
EOF
chmod +x tools/ci/*.sh
echo committed >committed.txt; echo before >modified.txt; echo before >staged.txt; echo gone >deleted.txt
printf '\000\001\002' >data.bin; echo ignored.log >.gitignore
git add -A && git commit -qm base && git update-ref refs/remotes/origin/main HEAD
git checkout -q -b feat/x
echo after >modified.txt; echo staged-after >staged.txt; git add staged.txt; rm deleted.txt
printf '\003\004' >data.bin
mkdir sub && echo hello >sub/new.txt; echo 3 >rc.txt; echo package x >sub/token.go
echo log >ignored.log; echo key >secret.pem; echo t >api-token.txt; echo E=1 >.env

r="$caller/tools/ci/remote.sh"

# ---- 1. send, run, stream, exit status, artifacts, cleanup
out="$(SCHEME=light "$r" kv1 browser rest 2>&1)"; rc=$?
rec="$RUNNER_TEMP/uitest-rest-dark/record.txt"
check "exit status is the suite's (3, got $rc)" [ $rc = 3 ]
check "output streamed back" grep -q '^STREAMED from the vm$' <<<"$out"
check "artifacts back at the browser default path" [ -f "$RUNNER_TEMP/uitest-rest-dark/failures/check.png" ]
check "the suite ran under capped.sh" grep -qx 'capped=yes' "$rec"
check "with GOTOOLCHAIN=local" grep -qx 'toolchain=local' "$rec"
check "Go cache in the VM's shared cache folder" grep -qx "gocache=$vm/.cache/comebye-remote/cache/go-build" "$rec"
check "in a per-run folder on the VM" grep -q "^pwd=$vm/.cache/comebye-remote/runs/.*/src$" "$rec"
check "artifact folder rewritten to the VM's" grep -q "^args=rest dark $vm/.cache/comebye-remote/runs/.*/art$" "$rec"
check "SCHEME forwarded" grep -qx 'scheme=light' "$rec"
check "same branch name" grep -qx 'branch=feat/x' "$rec"
check "origin/main sent for 'changed'" grep -qx 'origin_main=yes' "$rec"
check "committed file" grep -qx 'committed=committed' "$rec"
check "unstaged change" grep -qx 'modified=after' "$rec"
check "staged change" grep -qx 'staged=staged-after' "$rec"
check "binary change" grep -qx 'bin=0304' "$rec"
check "untracked file" grep -qx 'new=hello' "$rec"
check "untracked source named token.go still sent" grep -qx 'tokengo=yes' "$rec"
check "no deleted, ignored or secret-looking file" bash -c "! grep -q '^present=' '$rec'"
check "said which secret-looking files it kept back" bash -c "grep -q 'not sending untracked secret.pem' <<<\"\$1\" && grep -q 'api-token.txt' <<<\"\$1\" && grep -q 'not sending untracked .env' <<<\"\$1\"" _ "$out"
check "no git remotes on the VM" grep -qx 'remotes=0' "$rec"
check "the remote's token is nowhere on the VM" bash -c "! grep -rqa tok3n '$vm'"
check "run folder removed from the VM" bash -c "[ -z \"\$(ls -A '$vm/.cache/comebye-remote/runs')\" ]"
check "cache folder kept" [ -d "$vm/.cache/comebye-remote/cache/go-build" ]
check "lock released" bash -c "flock -n '$caller/.git/comebye-remote.lock' true"

echo 0 >rc.txt
out="$("$r" kv1 browser smoke dark "$root/mine" 2>&1)"; rc=$?
check "exit 0 passes through" [ $rc = 0 ]
check "artifacts to the given OUT_DIR" [ -f "$root/mine/failures/check.png" ]

# ---- 2. go, flakes
if command -v go >/dev/null && [ ! -e "$tools/go" ]; then
  out="$("$r" kv1 go env GOTOOLCHAIN GOMODCACHE 2>/dev/null)"; rc=$?
  check "go runs on the VM, GOTOOLCHAIN=local, the shared module cache (rc=$rc)" \
    [ "$rc:$(echo $out)" = "0:local $vm/.cache/comebye-remote/cache/go-mod" ]
fi
out="$(OUT_DIR="$root/fl" "$r" kv1 flakes repeat reviewcard 20 2>&1)"; rc=$?
check "flakes: exit 1 passes through" [ $rc = 1 ]
check "flakes repeat SCRIPT N becomes repeat N SCRIPT" grep -qx 'args=repeat 20 reviewcard' "$root/fl/flakes.txt"

# ---- 3. refusals
: >"$root/ssh.log"
out="$("$r" kv9 browser rest 2>&1)"; rc=$?
check "unknown host refused (rc=$rc)" rc_says "$rc" 2 'kv9 is not a worker this provisioner knows' "$out"
check "...without any ssh" [ ! -s "$root/ssh.log" ]
check "bad host name refused" bash -c "! '$r' '../kv1' go version 2>/dev/null"
pins 1.2.2 4.5.6 7.8.9.10
out="$("$r" kv1 browser rest 2>&1)"; rc=$?
check "wrong pins refused (rc=$rc)" rc_says "$rc" 2 'worker toolchain kv1' "$out"
rm -f "$vm/etc/test-runner"
out="$("$r" kv1 browser rest 2>&1)"; rc=$?
check "no toolchain refused (rc=$rc)" rc_says "$rc" 2 'has no test toolchain' "$out"
check "...before anything was sent" bash -c "[ -z \"\$(ls -A '$vm/.cache/comebye-remote/runs' 2>/dev/null)\" ]"
pins 1.2.3 4.5.6 7.8.9.10
out="$(FAKE_SSH_DOWN=1 "$r" kv1 browser rest 2>&1)"; rc=$?
check "ssh down: 255 (got $rc)" [ $rc = 255 ]
check "unknown suite refused" bash -c "! '$r' kv1 rm -rf / 2>/dev/null"

# two at once, then a stop mid-run
echo 0 >rc.txt; touch wait.txt
"$r" kv1 browser rest >"$root/first.log" 2>&1 & first=$!
for _ in $(seq 100); do [ -e "$root/started" ] && break; sleep 0.1; done
out="$("$r" kv1 browser rest 2>&1)"; rc=$?
check "a second run from the same worktree refused (rc=$rc)" rc_says "$rc" 2 'another remote run from this worktree' "$out"
suite="$(cat "$root/suite.pid" 2>/dev/null)"
sleep 6 # past the watchdog's TERM-then-KILL, were it to fire on its own
check "a running suite is left running while ssh is up" kill -0 "${suite:-0}"
kill -TERM "$first"; wait "$first"; rc=$?
check "stopped mid-run: exits 143 (got $rc)" [ $rc = 143 ]
check "...and the suite on the VM is gone" bash -c "! kill -0 '${suite:-0}' 2>/dev/null"
check "...and its run folder too" bash -c "[ -z \"\$(ls -A '$vm/.cache/comebye-remote/runs')\" ]"
touch "$root/release"
exit $fail
