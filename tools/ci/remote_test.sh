#!/usr/bin/env bash
# Self-test of tools/ci/remote.sh, with no VM and no network: tools/ci/remote_test.sh
#
# A fake ssh on PATH insists on the provisioner's ssh config and host kv1, and runs the command in a local folder
# that plays the VM's home. The caller is a small git repository with stand-ins for capped.sh, browser.sh and
# flakes.sh that record what they saw. Covers: the send (committed, modified, staged, deleted, untracked; not
# ignored files, secret-looking files or the remotes), the run (under capped.sh, the VM's caches, GOTOOLCHAIN),
# live output, the exit status, the artifacts back, the run folder removed, the flakes argument order, a stop
# mid-run killing the suite on the VM, the idle hold, a stopped VM started through a fake provisioner, and the
# refusals (unknown host, no VM, stopped with REMOTE_START=0, a start that fails, no toolchain, wrong pins, two runs
# at once, ssh down). Then tools/ci/prefer-vm.sh, through stand-in wrappers that call the real prefer_vm: the test VM
# from its config (and the provisioner's -config), TEST_VM=local, CI, no config, a VM that is not available (each with
# its one line), a start shown as it happens, and which gotest and flakes runs go (only whole sets and repeats).
# Then one suite at a time on the VM, with a second worktree as a second caller: a run waits its turn (saying whose
# run holds the VM, under its own idle hold) and starts only once the first ends; a holder killed hard frees the
# lock; giving up while waiting; --no-wait, directly and through prefer-vm.
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
[ "\$(cat "$root/vmstate" 2>/dev/null)" = RUNNING ] || { echo 'virtualmachineinstance.kubevirt.io "kv1" not found' >&2; exit 255; }
[ "\$1" = -F ] && [ "\$2" = "$state/kv1/ssh_config" ] || { echo "fake ssh: not the provisioner's config: \$*" >&2; exit 255; }
shift 2
while [ "\$1" = -o ]; do shift 2; done
[ "\$1" = kv1 ] || { echo "fake ssh: unknown host \$1" >&2; exit 255; }
shift
cd "$vm" && HOME="$vm" COMEBYE_TEST_RUNNER_FILE="$vm/etc/test-runner" exec bash -c "\$*"
EOF
# The fake provisioner: status --json from $root/vmstate (absent: no such VM); start sets RUNNING unless FAKE_START_FAIL.
cat >"$bin/fake-worker" <<EOF
#!/bin/bash
[ "\$1" = -config ] && { echo "config=\$2" >>"$root/worker.log"; shift 2; }
printf '%s\n' "\$*" >>"$root/worker.log"
case "\$1" in
  status) [ "\$2" = --json ] && [ "\$3" = kv1 ] && [ -f "$root/vmstate" ] || { echo '{"v":1,"provider":"kubevirt","workers":[]}'; exit 0; }
          echo "{\"v\":1,\"provider\":\"kubevirt\",\"workers\":[{\"name\":\"kv1\",\"state\":\"\$(cat "$root/vmstate")\"}]}" ;;
  start)  [ "\$2" = kv1 ] || exit 2; [ -n "\${FAKE_START_FAIL:-}" ] && { echo "start: the cluster said no" >&2; exit 1; }
          sleep 1; echo RUNNING >"$root/vmstate"; echo "kv1 is up, comebye v1" ;;
  *) exit 2 ;;
esac
EOF
echo RUNNING >"$root/vmstate"
export COMEBYE_WORKER_TOOL="$bin/fake-worker" # never the real provisioner
unset REMOTE_START TEST_VM COMEBYE_WORKER_CONFIG COMEBYE_ON_VM COMEBYE_VM_DECIDED CI GITHUB_ACTIONS
export TEST_VM_CONFIG="$root/test-vm" # absent unless a check writes it: never the real ~/.config one
for t in node npm google-chrome; do printf '#!/bin/sh\necho stub\n' >"$tools/$t"; done
command -v go >/dev/null || printf '#!/bin/sh\necho stub\n' >"$tools/go"
chmod +x "$bin"/* "$tools"/*
export PATH="$bin:$PATH:$tools"
pins() { printf 'go=%s\nnode=%s\nchrome=%s\n' "$1" "$2" "$3" >"$vm/etc/test-runner"; }
pins 1.2.3 4.5.6 7.8.9.10

# ---- the caller's worktree
cd "$caller" && git init -q && git remote add origin "https://someone:tok3n-SECRET@example.com/x.git" || exit 1
mkdir -p tools/ci tools/worker
cp "$here/remote.sh" tools/ci/remote.sh; cp "$here/prefer-vm.sh" tools/ci/prefer-vm.sh
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
. "\$(dirname "\$0")/prefer-vm.sh"; prefer_vm browser "\$@"
out="\${3:-\$RUNNER_TEMP/uitest-\$1-\${2:-dark}}"; mkdir -p "\$out/failures"
echo "STREAMED from the vm"
echo "start \$PWD" >>"$root/starts.log"; trap 'echo "end \$PWD" >>"$root/starts.log"' EXIT
echo "where=\$([ -n "\${COMEBYE_ON_VM:-}" ] && echo vm || echo local)" >"\$out/where.txt"
{ echo "args=\$*"; echo "pwd=\$PWD"; echo "capped=\${CAPPED_BY_TEST:-no}"; echo "toolchain=\$GOTOOLCHAIN"
  echo "gocache=\$GOCACHE"; echo "scheme=\${SCHEME:-}"; echo "jobs=\${JOBS:-}"; echo "branch=\$(git branch --show-current)"
  echo "origin_main=\$(git rev-parse --verify -q refs/remotes/origin/main >/dev/null && echo yes || echo no)"
  echo "remotes=\$(git remote -v | wc -l)"
  echo "hold=\$(cat "\$HOME/.local/state/comebye/idle-holds/"remote-test-* 2>/dev/null)"
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
. "$(dirname "$0")/prefer-vm.sh"; prefer_vm flakes "$@"
w=$([ -n "${COMEBYE_ON_VM:-}" ] && echo vm || echo local)
o="${OUT_DIR:-$RUNNER_TEMP/flakes}"; mkdir -p "$o"; echo "args=$*" >"$o/flakes.txt"; echo "FLAKY none where=$w"; exit 1
EOF
cat >tools/ci/gotest.sh <<'EOF'
#!/bin/bash
. "$(dirname "$0")/prefer-vm.sh"; prefer_vm gotest "$@"
echo "gotest where=$([ -n "${COMEBYE_ON_VM:-}" ] && echo vm || echo local) args=$*"
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
now=$(date +%s); hold_until=$(sed -n 's/^hold=\([0-9]*\) remote test run .*/\1/p' "$rec")
check "an idle hold for about 20 min while the suite ran" [ "$(( ${hold_until:-0} - now > 900 && ${hold_until:-0} - now <= 1200 ))" = 1 ]
check "...removed afterwards" bash -c "[ -z \"\$(ls -A '$vm/.local/state/comebye/idle-holds')\" ]"
check "a running VM is not started" bash -c "! grep -q '^start' '$root/worker.log'"
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

# a stopped VM
echo 0 >rc.txt; : >"$root/worker.log"
echo STOPPED >"$root/vmstate"
out="$(REMOTE_START=0 "$r" kv1 browser rest 2>&1)"; rc=$?
check "stopped, REMOTE_START=0: refused, saying how to start it (rc=$rc)" rc_says "$rc" 2 "kv1 is STOPPED (a worker powers off when idle). Start it with: $bin/fake-worker start kv1" "$out"
check "...and not started" bash -c "! grep -q '^start' '$root/worker.log'"
out="$(FAKE_START_FAIL=1 "$r" kv1 browser rest 2>&1)"; rc=$?
check "a start that fails: refused (rc=$rc)" rc_says "$rc" 2 'kv1 did not start' "$out"
check "...with the provisioner's own words" grep -q 'the cluster said no' <<<"$out"
: >"$root/worker.log"
out="$("$r" kv1 browser rest 2>&1)"; rc=$?
check "stopped: started through the provisioner, then run (rc=$rc)" rc_says "$rc" 0 'kv1 is STOPPED; starting it' "$out"
check "...saying how long the start took" grep -Eq '^remote: kv1 started in [0-9]+ s$' <<<"$out"
check "...exactly once" [ "$(grep -c '^start kv1$' "$root/worker.log")" = 1 ]
rm -f "$root/vmstate"
out="$("$r" kv1 browser rest 2>&1)"; rc=$?
check "the provisioner has no such VM: refused (rc=$rc)" rc_says "$rc" 2 'has no VM kv1' "$out"
echo PROVISIONING >"$root/vmstate"
out="$("$r" kv1 browser rest 2>&1)"; rc=$?
check "neither up nor stopped: refused (rc=$rc)" rc_says "$rc" 2 'kv1 is PROVISIONING' "$out"
echo RUNNING >"$root/vmstate"

# ---- the wrappers prefer the test VM (tools/ci/prefer-vm.sh)
check "the real browser.sh asks prefer_vm" grep -q 'prefer_vm browser "$@"' "$here/browser.sh"
check "the real flakes.sh asks prefer_vm" grep -q 'prefer_vm flakes "$cmd" "$@"' "$here/flakes.sh"
check "the real gotest.sh asks prefer_vm" grep -q 'prefer_vm gotest "$@"' "$here/gotest.sh"
check "the real prelanding.sh asks prefer_vm" grep -q 'prefer_vm prelanding "$@"' "$here/prelanding.sh"
b="$caller/tools/ci/browser.sh"; where() { sed "s/^where=//" "$RUNNER_TEMP/uitest-rest-dark/where.txt" 2>/dev/null; }
echo 0 >rc.txt; rm -rf "$RUNNER_TEMP"; : >"$root/ssh.log"
out="$("$b" rest 2>&1)"; rc=$?
check "no config: runs here (rc=$rc)" [ "$rc:$(where)" = 0:local ]
check "...saying so in one line" [ "$(grep -c '^tests: running on this machine (no test VM in ' <<<"$out")" = 1 ]
check "...without ssh" [ ! -s "$root/ssh.log" ]
printf '# the test VM\nhost = kv1  # a worker\nworker_config=~/wk.conf\n' >"$TEST_VM_CONFIG"
echo 3 >rc.txt; rm -rf "$RUNNER_TEMP"; : >"$root/worker.log"
out="$("$b" rest 2>&1)"; rc=$?
check "configured: runs on the VM, with the suite's status (rc=$rc)" [ "$rc:$(where)" = 3:vm ]
check "...saying so" grep -q '^tests: running browser on the test VM kv1 (TEST_VM=local runs it here)$' <<<"$out"
check "...with the provisioner's -config from the config file" grep -qx "config=$HOME/wk.conf" "$root/worker.log"
echo 0 >rc.txt; rm -rf "$RUNNER_TEMP"; : >"$root/ssh.log"
out="$(TEST_VM=local "$b" rest 2>&1)"; rc=$?
check "TEST_VM=local: here, silently, no ssh (rc=$rc)" [ "$rc:$(where):$(grep -c '^tests:' <<<"$out"):$(wc -c <"$root/ssh.log")" = 0:local:0:0 ]
rm -rf "$RUNNER_TEMP"
out="$(CI=1 "$b" rest 2>&1)"; rc=$?
check "CI: here, silently" [ "$rc:$(where):$(grep -c '^tests:' <<<"$out")" = 0:local:0 ]
rm -rf "$RUNNER_TEMP"
out="$(TEST_VM=kv2 "$b" rest 2>&1)"; rc=$?
check "TEST_VM=NAME: that VM (kv2 is unknown, so here, saying why)" rc_says "$rc:$(where)" 0:local '^tests: running on this machine: test VM kv2 is not available (kv2 is not a worker this provisioner knows' "$out"
rm -rf "$RUNNER_TEMP"
out="$(FAKE_SSH_DOWN=1 "$b" rest 2>&1)"; rc=$?
check "VM unreachable: here, with the reason in one line (rc=$rc)" rc_says "$rc:$(where):$(grep -c '^tests:' <<<"$out")" 0:local:1 'test VM kv1 is not available (ssh could not reach kv1' "$out"
pins 9.9.9 4.5.6 7.8.9.10; rm -rf "$RUNNER_TEMP"
out="$("$b" rest 2>&1)"; rc=$?
check "VM without the pins: here (rc=$rc)" rc_says "$rc:$(where)" 0:local 'test VM kv1 is not available (kv1 has no test toolchain' "$out"
pins 1.2.3 4.5.6 7.8.9.10
echo STOPPED >"$root/vmstate"; rm -rf "$RUNNER_TEMP"
out="$("$b" rest 2>&1)"; rc=$?
check "VM stopped: started, shown as it happens, then the run goes there (rc=$rc)" rc_says "$rc:$(where)" 0:vm '^remote: kv1 is STOPPED; starting it' "$out"
check "...and how long it took" grep -Eq '^remote: kv1 started in [0-9]+ s$' <<<"$out"
g="$caller/tools/ci/gotest.sh"
check "gotest with no package: the VM" grep -q '^gotest where=vm args=$' <<<"$("$g" 2>&1)"
check "gotest ./...: the VM" grep -q '^gotest where=vm args=-count=1 ./...$' <<<"$("$g" -count=1 ./... 2>&1)"
check "gotest --shard: the VM" grep -q '^gotest where=vm args=--shard 1/2$' <<<"$("$g" --shard 1/2 2>&1)"
: >"$root/ssh.log"; out="$("$g" -run X ./internal/x 2>&1)"
check "gotest of one package: here, silently, no ssh" [ "$out:$(wc -c <"$root/ssh.log")" = "gotest where=local args=-run X ./internal/x:0" ]
f="$caller/tools/ci/flakes.sh"
check "flakes repeat: the VM" grep -q 'FLAKY none where=vm' <<<"$(OUT_DIR="$root/fl2" "$f" repeat 2 x 2>&1)"
check "flakes other than repeat: here" grep -q 'FLAKY none where=local' <<<"$("$f" list 2>&1)"
check "a relative OUT_DIR lands in the caller's folder" bash -c "cd '$caller/sub' && '$caller/tools/ci/remote.sh' kv1 browser smoke dark rel-out >/dev/null 2>&1; [ -f '$caller/sub/rel-out/failures/check.png' ]"
rm -f "$TEST_VM_CONFIG"

# ---- one suite at a time on the VM
wf() { local i; for i in $(seq 150); do eval "$1" && return 0; sleep 0.1; done; return 1; } # wait (15 s at most) until
git -C "$caller" worktree add -q --detach "$root/caller2" HEAD
r2="$root/caller2/tools/ci/remote.sh"
hold_n() { ls "$vm/.local/state/comebye/idle-holds/" 2>/dev/null | grep -c '^remote-test-'; }
start_a() { rm -f "$root/started" "$root/release" "$root/suite.pid"; touch wait.txt; "$r" kv1 browser rest >"$root/a.log" 2>&1 & a=$!; wf '[ -e "$root/started" ]'; }
echo 0 >rc.txt; : >"$root/starts.log"
start_a
RUNNER_TEMP="$root/rt2" "$r2" kv1 browser rest >"$root/b.log" 2>&1 & b=$!
check "a second run waits its turn, saying whose run holds the VM and since when" wf "grep -Eq '^remote\(vm\): .+ is busy with [a-z0-9-]+ \(browser, feat/x, from [a-z0-9-]+\) since [0-9:]+Z; waiting for it \(Ctrl-C gives up\)$' '$root/b.log'"
sleep 6 # past a wait step
check "...its suite has not started" [ "$(grep -c '^start' "$root/starts.log")" = 1 ]
check "...and its idle hold covers the wait" [ "$(hold_n)" = 2 ]
touch "$root/release"; wait "$a"; wait "$b"; rc=$?
check "...then it runs, once the first has ended (rc=$rc)" [ "$rc:$(cut -d' ' -f1 "$root/starts.log" | tr '\n' ' ')" = "0:start end start end " ]
check "...saying it is free" grep -Eq '^remote\(vm\): .+ is free after [0-9]+ s; starting$' "$root/b.log"
check "...the second run's own place is the one after the first" [ "$(sed -n 3p "$root/starts.log")" = "start $vm/.cache/comebye-remote/runs/$(sed -n 's/^remote: kv1 \([a-z0-9-]*\): .*/\1/p' "$root/b.log")/src" ]
check "no hold left" [ "$(hold_n)" = 0 ]

# a holder killed hard on the VM (no cleanup of its own) frees the lock
: >"$root/starts.log"; start_a
RUNNER_TEMP="$root/rt2" "$r2" kv1 browser rest >"$root/b.log" 2>&1 & b=$!
wf "grep -q 'waiting for it' '$root/b.log'"
ida="$(sed -n 's/^remote: kv1 \([a-z0-9-]*\): .*/\1/p' "$root/a.log")"
pkill -KILL -f "runs/'$ida'"; kill -KILL -- "-$(cat "$root/suite.pid")" 2>/dev/null; kill -KILL "$(cat "$root/suite.pid")" 2>/dev/null
check "a holder killed hard: the waiting run goes ahead" wf "grep -q 'is free after' '$root/b.log'"
wait "$b"; rc=$?; wait "$a"
check "...and passes (rc=$rc)" [ "$rc" = 0 ]

# giving up while waiting
: >"$root/starts.log"; start_a; suite_a="$(cat "$root/suite.pid")"
RUNNER_TEMP="$root/rt2" "$r2" kv1 browser rest >"$root/b.log" 2>&1 & b=$!
wf "grep -q 'waiting for it' '$root/b.log'"
idb="$(sed -n 's/^remote: kv1 \([a-z0-9-]*\): .*/\1/p' "$root/b.log")"
t0=$SECONDS; kill -TERM "$b"; wait "$b"; rc=$?
check "giving up while waiting: exits 143 within 10 s (rc=$rc, $((SECONDS - t0)) s)" [ "$rc" = 143 ] && [ $((SECONDS - t0)) -le 10 ]
check "...the VM side gave up" wf "grep -q 'gave up waiting' '$root/b.log'"
check "...its suite never ran" [ "$(grep -c '^start' "$root/starts.log")" = 1 ]
check "...its run folder is gone" [ ! -e "$vm/.cache/comebye-remote/runs/$idb" ]
check "...and the first run carries on" kill -0 "$suite_a"

# --no-wait, while the first still holds the VM
out="$("$r2" --no-wait kv1 browser rest 2>&1)"; rc=$?
check "--no-wait on a busy VM: refused (rc=$rc)" rc_says "$rc" 2 'kv1 is busy with .*: not waiting (--no-wait)' "$out"
printf 'host=kv1\n' >"$TEST_VM_CONFIG"
out="$(cd "$root/caller2" && RUNNER_TEMP="$root/rt2" TEST_VM_NO_WAIT=1 tools/ci/browser.sh rest 2>&1)"; rc=$?
check "TEST_VM_NO_WAIT=1 on a busy VM: runs here, saying why (rc=$rc)" rc_says "$rc" 0 '^tests: running on this machine: test VM kv1 is not available (kv1 is busy with ' "$out"
out="$(cd "$root/caller2" && RUNNER_TEMP="$root/rt2" timeout 3 tools/ci/browser.sh rest 2>&1)"
check "without it, prefer-vm says the run will wait its turn" grep -q '^remote: kv1 is busy with .*; this run will wait its turn$' <<<"$out"
rm -f "$TEST_VM_CONFIG"; touch "$root/release"; wait "$a"
rm -f wait.txt "$root/started" "$root/release"
git -C "$caller" worktree remove --force "$root/caller2"

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
