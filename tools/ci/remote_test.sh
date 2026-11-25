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
# Then slots (a test-slots file on the fake VM): --probe; two runs on a 2-slot VM run together, each in its own slot
# with its own JOBS, CAP_MEM and ports; two browser stand-ins hold every port the real run.sh would use, side by side,
# and the real scripts' port offsets fit a slot's range; a third run waits for either slot; an old client (exclusive
# run.lock, as before slots) against new runs, both ways, and the real old remote.sh when the history has it.
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

# The fake VMs: kv1 lives in $root/vm with its state in $root/vmstate; any other HOST in $root/vm-HOST and
# $root/vmstate-HOST (made by mkvm below). $root/down-HOST makes ssh to it fail.
vmdir() { if [ "$1" = kv1 ]; then echo "$vm"; else echo "$root/vm-$1"; fi; }
# The fake ssh: -F must be HOST's provisioner config; the command runs with HOME at that VM's folder.
cat >"$bin/ssh" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >>"$root/ssh.log"
[ -n "\${FAKE_SSH_DOWN:-}" ] && { echo "ssh: connect: no route" >&2; exit 255; }
[ "\$1" = -F ] || { echo "fake ssh: no -F: \$*" >&2; exit 255; }
f="\$2"; shift 2
while [ "\$1" = -o ]; do shift 2; done
h="\$1"; shift
if [ "\$h" = kv1 ]; then d="$vm"; st="$root/vmstate"; else d="$root/vm-\$h"; st="$root/vmstate-\$h"; fi
[ "\$f" = "$state/\$h/ssh_config" ] && [ -d "\$d" ] || { echo "fake ssh: unknown host \$h or not the provisioner's config \$f" >&2; exit 255; }
[ -e "$root/down-\$h" ] && { echo "ssh: connect to \$h: no route" >&2; exit 255; }
[ "\$(cat "\$st" 2>/dev/null)" = RUNNING ] || { echo "virtualmachineinstance.kubevirt.io \"\$h\" not found" >&2; exit 255; }
cd "\$d" && HOME="\$d" COMEBYE_TEST_RUNNER_FILE="\$d/etc/test-runner" exec bash -c "\$*"
EOF
# The fake provisioner: status --json HOST from HOST's state file (absent: no such VM); start sets RUNNING unless
# FAKE_START_FAIL. Each call is logged twice: "config=FILE" then the command (as before), and "with FILE: command".
cat >"$bin/fake-worker" <<EOF
#!/bin/bash
c=""; [ "\$1" = -config ] && { c="\$2"; echo "config=\$2" >>"$root/worker.log"; shift 2; }
printf '%s\n' "\$*" >>"$root/worker.log"; printf 'with %s: %s\n' "\$c" "\$*" >>"$root/worker.log"
h="\${3:-\$2}"; [ "\$1" = start ] && h="\$2"
if [ "\$h" = kv1 ]; then st="$root/vmstate"; else st="$root/vmstate-\$h"; fi
case "\$1" in
  status) [ "\$2" = --json ] && [ -f "\$st" ] || { echo '{"v":1,"provider":"kubevirt","workers":[]}'; exit 0; }
          n=""; [ -f "$root/node-\$h" ] && n=",\"node\":\"\$(cat "$root/node-\$h")\""
          echo "{\"v\":1,\"provider\":\"kubevirt\",\"workers\":[{\"name\":\"\$h\",\"state\":\"\$(cat "\$st")\"\$n}]}" ;;
  start)  [ -f "\$st" ] || exit 2; [ -n "\${FAKE_START_FAIL:-}" ] && { echo "start: the cluster said no" >&2; exit 1; }
          sleep 1; echo RUNNING >"\$st"; echo "\$h is up, comebye v1" ;;
  *) exit 2 ;;
esac
EOF
# The fake Prometheus, as curl: answers http://fake-prom/api/v1/query with $root/prom.json (absent: connection
# refused); with $root/prom-slow it hangs. Each call's query goes to $root/prom.log.
cat >"$bin/curl" <<EOF
#!/bin/bash
q=""; u=""
while [ \$# -gt 0 ]; do case "\$1" in --data-urlencode) q="\$2"; shift 2 ;; -m) shift 2 ;; -*) shift ;; *) u="\$1"; shift ;; esac; done
printf '%s %s\n' "\$u" "\$q" >>"$root/prom.log"
[ "\$u" = http://fake-prom/api/v1/query ] || { echo "fake curl: \$u" >&2; exit 7; }
[ -e "$root/prom-slow" ] && sleep 20
[ -f "$root/prom.json" ] || exit 7
cat "$root/prom.json"
EOF
chmod +x "$bin/curl"
# prom_json NODE CPU NVME...: Prometheus' answer to temps.sh's query, as the real one looks.
prom_json() {
  local r="" t=1795475468.956
  while [ $# -ge 3 ]; do
    r="$r${r:+,}{\"metric\":{\"node\":\"$1\",\"t\":\"nvme\"},\"value\":[$t,\"$3\"]},{\"metric\":{\"node\":\"$1\",\"t\":\"cpu\"},\"value\":[$t,\"$2\"]}"
    shift 3
  done
  echo "{\"status\":\"success\",\"data\":{\"resultType\":\"vector\",\"result\":[$r]}}"
}
# holdports.py BASE RELEASE OUT: listen on BASE+each offset in $PORT_OFFSETS (what run.sh uses), write "bound ..." or
# "clash PORT" to OUT, and keep them until the file RELEASE appears (30 s at most). Exit 98 on a clash.
cat >"$root/holdports.py" <<'EOF'
import os, socket, sys, time
base, release, out = int(sys.argv[1]), sys.argv[2], sys.argv[3]
socks = []
for off in os.environ["PORT_OFFSETS"].split():
    s = socket.socket(); p = base + int(off)
    try:
        s.bind(("127.0.0.1", p)); s.listen(1)
    except OSError:
        open(out, "w").write("clash %d\n" % p); print("holdports: port %d is taken" % p, file=sys.stderr); sys.exit(98)
    socks.append(s)
open(out, "w").write("bound %d ports %d-%d\n" % (len(socks), base, base + max(int(o) for o in os.environ["PORT_OFFSETS"].split())))
for _ in range(300):
    if os.path.exists(release): break
    time.sleep(0.1)
EOF
echo RUNNING >"$root/vmstate"
export COMEBYE_WORKER_TOOL="$bin/fake-worker" # never the real provisioner
unset REMOTE_START TEST_VM COMEBYE_WORKER_CONFIG COMEBYE_ON_VM COMEBYE_VM_DECIDED CI GITHUB_ACTIONS
export TEST_VM_CONFIG="$root/test-vm" # absent unless a check writes it: never the real ~/.config one
export TEST_POOL_CONFIG="$root/test-pool" # the same for the pool
unset TEST_VM_NO_WAIT TEST_POOL_POLL
for t in node npm google-chrome; do printf '#!/bin/sh\necho stub\n' >"$tools/$t"; done
# The fake comebye, for `comebye whoami --json` (the run record's agent): $root/whoami.json, else no agent here.
cat >"$bin/comebye" <<EOF
#!/bin/bash
[ "\$1 \$2" = "whoami --json" ] && [ -f "$root/whoami.json" ] && exec cat "$root/whoami.json"
echo "comebye: not an agent" >&2; exit 1
EOF
command -v go >/dev/null || printf '#!/bin/sh\necho stub\n' >"$tools/go"
chmod +x "$bin"/* "$tools"/*
export PATH="$bin:$PATH:$tools"
pins() { printf 'go=%s\nnode=%s\nchrome=%s\n' "$1" "$2" "$3" >"$vm/etc/test-runner"; }
pins 1.2.3 4.5.6 7.8.9.10

# ---- the caller's worktree
cd "$caller" && git init -q && git remote add origin "https://someone:tok3n-SECRET@example.com/x.git" || exit 1
mkdir -p tools/ci tools/worker
cp "$here/remote.sh" tools/ci/remote.sh; cp "$here/prefer-vm.sh" tools/ci/prefer-vm.sh; cp "$here/temps.sh" tools/ci/temps.sh
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
  echo "port=\${PORT:-}"; echo "repeat_port=\${REPEAT_PORT:-}"; echo "capmem=\${CAP_MEM:-}"; echo "gotest_p=\${GOTEST_P:-}"; echo "parallel=\${PARALLEL:-}"
  echo "origin_main=\$(git rev-parse --verify -q refs/remotes/origin/main >/dev/null && echo yes || echo no)"
  echo "remotes=\$(git remote -v | wc -l)"
  echo "hold=\$(cat "\$HOME/.local/state/comebye/idle-holds/"remote-test-* 2>/dev/null)"
  echo "committed=\$(cat committed.txt)"; echo "modified=\$(cat modified.txt)"; echo "staged=\$(cat staged.txt)"
  echo "new=\$(cat sub/new.txt 2>/dev/null)"; echo "tokengo=\$([ -f sub/token.go ] && echo yes)"; echo "bin=\$(od -An -tx1 data.bin | tr -d ' \n')"
  for f in deleted.txt ignored.log secret.pem api-token.txt .env; do [ -e "\$f" ] && echo "present=\$f"; done
} >"\$out/record.txt"
echo shot >"\$out/failures/check.png"
[ -f "$root/prom-end.json" ] && cp "$root/prom-end.json" "$root/prom.json" # the temperatures as the suite ends
if [ -e wait.txt ]; then
  echo \$\$ >"$root/suite.pid"; touch "$root/started"
  if [ -e "$root/bind" ]; then # hold every port run.sh would listen on with this PORT (holdports.py), until released
    python3 "$root/holdports.py" "\${PORT:-18741}" "$root/release" "\$out/bound.txt" || exit 98
  else
    for _ in \$(seq 200); do [ -e "$root/release" ] && break; sleep 0.1; done
  fi
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
set -euo pipefail # as the real gotest.sh: prefer_vm must not fail a step under set -e
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

# ---- the run's record on the VM (test-runs/ID.json): written once the suite has its slot, renewed with the hold,
# removed at the end
runs_dir="$vm/.local/state/comebye/test-runs"
recf() { ls "$runs_dir"/*.json 2>/dev/null | head -1; }
# field FILE KEY: one field of a record, through a real JSON parser (so the file must be valid JSON)
field() { python3 -c 'import json,sys; v=json.load(open(sys.argv[1])).get(sys.argv[2]); print("" if v is None else v)' "$1" "$2"; }
printf '{\n  "agent": "deskhost/w8J:p1",\n  "by": "runs (deskhost/w8J:p1)",\n  "host": "deskhost"\n}\n' >"$root/whoami.json"
git checkout -q -b "feat/q\"uo'te"
echo 0 >rc.txt; touch wait.txt; rm -f "$root/started" "$root/release"
REMOTE_RENEW_SECS=1 "$r" kv1 browser rest >"$root/rec.log" 2>&1 & a=$!
for _ in $(seq 150); do [ -e "$root/started" ] && break; sleep 0.1; done
f="$(recf)"; id="$(sed -n 's/^remote: kv1 \([a-z0-9-]*\): .*/\1/p' "$root/rec.log")"
check "record: written on the VM while the suite runs, named for the run" [ -n "$id" -a "$f" = "$runs_dir/$id.json" ]
check "...valid JSON with the run, slot, suite and HEAD" [ "$(field "$f" id) $(field "$f" slot)/$(field "$f" slots) $(field "$f" suite) $(field "$f" head)" = "$id 1/1 browser $(git rev-parse HEAD)" ]
check "...the branch, however it is spelt" [ "$(field "$f" branch)" = "feat/q\"uo'te" ]
check "...the starting agent, from comebye whoami" [ "$(field "$f" agent)" = deskhost/w8J:p1 ]
check "...and this host" [ "$(field "$f" from)" = "$(hostname -s | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-\n' - | cut -c1-16)" ]
st="$(field "$f" started)"; ex="$(field "$f" expires)"
check "...started just now, expiring with the hold (20 min on)" [ "$(( $(date -d "$ex" +%s) - $(date -d "$st" +%s) ))" -ge 1190 -a "$(( $(date +%s) - $(date -d "$st" +%s) ))" -le 30 ]
sleep 2.5
check "...renewed with the hold: a later expiry, the same start" [ "$(date -d "$(field "$f" expires)" +%s)" -gt "$(date -d "$ex" +%s)" -a "$(field "$f" started)" = "$st" ]
check "...nothing half-written beside it" [ "$(ls "$runs_dir")" = "$id.json" ]
touch "$root/release"; wait "$a"; rc=$?
check "...removed when the run ends (rc=$rc)" [ "$rc" = 0 -a -z "$(ls -A "$runs_dir" 2>/dev/null)" ]
rm -f "$root/whoami.json" "$root/started" "$root/release"; git checkout -q feat/x
"$r" kv1 browser rest >"$root/rec.log" 2>&1 & a=$!
for _ in $(seq 150); do [ -e "$root/started" ] && break; sleep 0.1; done
f="$(recf)"
check "no agent where comebye whoami names none: the record's agent is empty" [ -n "$f" ] && [ "$(field "$f" agent):$(field "$f" suite)" = ":browser" ]
touch "$root/release"; wait "$a"; rm -f wait.txt "$root/started" "$root/release"

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
check "...and the killed run's record is gone with it" [ -z "$(ls -A "$vm/.local/state/comebye/test-runs" 2>/dev/null)" ]

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
check "...and logged, with its exit status" grep -q ' vm=kv1 .* suite=browser secs=[0-9]* rc=143 ' <<<"$(tail -1 "$root/nope/comebye-remote/temps.log")"
check "...and the suite on the VM is gone" bash -c "! kill -0 '${suite:-0}' 2>/dev/null"
check "...and its run folder too" bash -c "[ -z \"\$(ls -A '$vm/.cache/comebye-remote/runs')\" ]"
touch "$root/release"
wait "$first" 2>/dev/null

# ---- slots: a VM with test-slots says how many suites run on it at once
rr="$vm/.cache/comebye-remote"
for w in caller2 caller3; do git -C "$caller" worktree add -q --detach "$root/$w" HEAD; echo 0 >"$root/$w/rc.txt"; touch "$root/$w/wait.txt"; done
r3="$root/caller3/tools/ci/remote.sh"
starts() { grep -c '^start' "$root/starts.log"; }
# --probe: one ssh, no provisioner, slots and free slots
: >"$root/worker.log"
check "--probe, no test-slots file: one slot, free" [ "$("$r" --probe kv1 2>/dev/null)" = "slots=1 free=1 toolchain=ok" ]
check "...without asking the provisioner" [ ! -s "$root/worker.log" ]
echo 2 >"$vm/.cache/comebye-remote/test-slots"
check "--probe, test-slots 2: two slots, both free" [ "$("$r" --probe kv1 2>/dev/null)" = "slots=2 free=2 toolchain=ok" ]
check "--probe, REMOTE_ROOT elsewhere: the slots file where the provisioner writes it" [ "$(REMOTE_ROOT="$vm/data" "$r" --probe kv1 2>/dev/null)" = "slots=2 free=2 toolchain=ok" ]
echo junk >"$vm/.cache/comebye-remote/test-slots"
check "--probe, test-slots not a number: one slot" [ "$("$r" --probe kv1 2>/dev/null)" = "slots=1 free=1 toolchain=ok" ]
echo 2 >"$vm/.cache/comebye-remote/test-slots"; pins 9.9.9 4.5.6 7.8.9.10
check "--probe, wrong pins: toolchain=bad" [ "$("$r" --probe kv1 2>/dev/null)" = "slots=2 free=2 toolchain=bad" ]
pins 1.2.3 4.5.6 7.8.9.10
FAKE_SSH_DOWN=1 "$r" --probe kv1 >/dev/null 2>&1; rc=$?
check "--probe, ssh down: 255 (got $rc)" [ $rc = 255 ]

# The ports a browser suite listens on: every `port + N` in the real run.sh, and +1 of each for its light pass.
offs="$(grep -o 'port + [0-9]*' "$here/../uitest/run.sh" | awk '{print $3}' | sort -un | tr '\n' ' ')"
export PORT_OFFSETS="$(for o in 0 $offs; do printf '%s %s ' "$o" $((o + 1)); done)"
maxoff=$(tr ' ' '\n' <<<"$PORT_OFFSETS" | sort -n | tail -1)
flk=$(grep -o 'for p in [0-9 ]*; do' "$here/flakes.sh" | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -n | tail -1)
fits() { [ $((3 * 1000 + maxoff)) -lt 5000 ] && [ $((3 * 1000 + ${flk:-9999})) -lt 5000 ] &&
  grep -qF 'PORT=$(( base_port + (i - 1) * 1000 ))' "$here/browser.sh" && grep -qF 'port=$((base + 1000 * free))' "$here/flakes.sh"; }
check "a slot's 5000 ports hold run.sh (+$maxoff), 4 PARALLEL parts and 4 flakes jobs (+$flk)" fits
# The VM's slots from here: 27000-27900 (+5401 for the second slot), clear of a real run.sh or flakes run on this
# machine (18741..about 24500) and mostly below the kernel's ephemeral ports (32768 up), which outgoing connections take.
export COMEBYE_SLOT_PORT_BASE=$((27000 + RANDOM % 10 * 100))
if command -v python3 >/dev/null; then touch "$root/bind"; else echo "skip: no python3, so the browser stand-ins hold no ports"; fi

# two runs on a 2-slot VM run together, each in its own slot with its own share; a third waits for either
echo 0 >rc.txt; touch wait.txt; rm -f "$root/started" "$root/release"; : >"$root/starts.log"
JOBS=1 CAP_MEM=512M PORT=18741 "$r" kv1 browser rest >"$root/a.log" 2>&1 & a=$!
wf '[ "$(starts)" = 1 ]'
JOBS=99 CAP_MEM=999G PORT=18741 RUNNER_TEMP="$root/rt2" "$r2" kv1 browser rest >"$root/b.log" 2>&1 & b=$!
check "two runs on a 2-slot VM run together" wf '[ "$(starts)" = 2 ]'
check "...each in its own slot" bash -c "grep -q 'slot 1 of 2' '$root/a.log' && grep -q 'slot 2 of 2' '$root/b.log'"
check "--probe: both slots taken" [ "$("$r" --probe kv1 2>/dev/null)" = "slots=2 free=0 toolchain=ok" ]
runid() { sed -n 's/^remote: kv1 \([a-z0-9-]*\): .*/\1/p' "$1"; }
va="$rr/runs/$(runid "$root/a.log")/art"; vb="$rr/runs/$(runid "$root/b.log")/art" # their artifacts, on the VM while they run
pa="$(sed -n 's/^port=//p' "$va/record.txt")"; pb="$(sed -n 's/^port=//p' "$vb/record.txt")"
check "...the slots' own ports, not the PORT sent from here ($pa, $pb)" [ "$pa:$pb" = "$COMEBYE_SLOT_PORT_BASE:$((COMEBYE_SLOT_PORT_BASE + 5000))" ]
check "...REPEAT_PORT (flakes) from the same range" [ "$(sed -n 's/^repeat_port=//p' "$vb/record.txt")" = "$pb" ]
if [ -e "$root/bind" ]; then
  check "two browser suites on one VM don't clash on ports: both hold all of run.sh's" \
    bash -c "wf() { local i; for i in \$(seq 100); do eval \"\$1\" && return 0; sleep 0.1; done; return 1; }
      wf 'grep -q ^bound \"$va/bound.txt\" 2>/dev/null && grep -q ^bound \"$vb/bound.txt\" 2>/dev/null'" ||
    grep -h '^holdports:' "$root/a.log" "$root/b.log" # which port was taken, and by whom it may be: ss -ltnp
  check "...and a clash would show (the first suite's port is taken)" \
    python3 -c "import socket,sys; s=socket.socket(); sys.exit(0 if s.connect_ex(('127.0.0.1', ${pa:-1})) == 0 else 1)"
fi
RUNNER_TEMP="$root/rt3" "$r3" kv1 browser rest >"$root/c.log" 2>&1 & c=$!
check "a third waits, naming both runs that hold the slots" wf "grep -Eq '^remote\(vm\): .+ has all 2 slots busy \([a-z0-9-]+ \(browser, [^)]*\) since [0-9:]+Z; [a-z0-9-]+ \(browser, [^)]*\) since [0-9:]+Z\); waiting for one \(Ctrl-C gives up\)$' '$root/c.log'"
sleep 3
check "...its suite has not started" [ "$(starts)" = 2 ]
touch "$root/release"; wait "$a"; ra_rc=$?; wait "$b"; rb_rc=$?; wait "$c"; rc_rc=$?
check "...then it runs in a freed slot, after one of the first two ended (rc $ra_rc $rb_rc $rc_rc)" \
  [ "$ra_rc:$rb_rc:$rc_rc:$(head -3 "$root/starts.log" | cut -d' ' -f1 | tr '\n' ' ')" = "0:0:0:start start end " ]
check "...saying so" grep -Eq '^remote\(vm\): .+ is free after [0-9]+ s; starting$' "$root/c.log"
sj=$(( $(nproc) / 2 )); [ $sj -ge 1 ] || sj=1; sjobs=$sj; [ $sjobs -le 4 ] || sjobs=4
smem=$(( $(awk '/^MemTotal:/ {print $2}' /proc/meminfo) / 1024 * 85 / 100 / 2 )); [ $smem -ge 1024 ] || smem=1024
check "...a lower JOBS and CAP_MEM sent from here are kept" bash -c "grep -qx 'jobs=1' '$RUNNER_TEMP/uitest-rest-dark/record.txt' && grep -qx 'capmem=512M' '$RUNNER_TEMP/uitest-rest-dark/record.txt'"
check "...higher ones become the slot's share (JOBS $sjobs, GOTEST_P $sj, CAP_MEM ${smem}M)" \
  bash -c "grep -qx 'jobs=$sjobs' '$root/rt2/uitest-rest-dark/record.txt' && grep -qx 'gotest_p=$sj' '$root/rt2/uitest-rest-dark/record.txt' && grep -qx 'capmem=${smem}M' '$root/rt2/uitest-rest-dark/record.txt'"
check "all slots free again" [ "$("$r" --probe kv1 2>/dev/null)" = "slots=2 free=2 toolchain=ok" ]

# an old client (before slots: run.lock, exclusive) against new slots. The old client's VM side, as it was:
# exec 8>>run.lock; flock -n 8 || until flock -w 5 8; ...; write run.who; run. Played here by old_vm_side.
old_vm_side() { # TAG: take run.lock the old way, log start/end, hold it until $root/release
  ( exec 8>>"$rr/run.lock"; flock -n 8 || until flock -w 5 8; do :; done
    printf '%s since %s\n' "old-$1 (browser, main, from old)" "$(date -u +%H:%M:%SZ)" >"$rr/run.who"
    echo "start old-$1" >>"$root/starts.log"
    until [ -e "$root/release" ]; do sleep 0.1; done; echo "end old-$1" >>"$root/starts.log" ) 9>&- &
}
rm -f "$root/release" "$root/bind"; : >"$root/starts.log" # no ports held from here on: an old client's PORT is 18741
old_vm_side 1; o=$!
wf 'grep -q "start old-1" "$root/starts.log"'
check "an old client holds the VM: --probe says no slot is free" [ "$("$r" --probe kv1 2>/dev/null)" = "slots=2 free=0 toolchain=ok" ]
"$r" kv1 browser rest >"$root/a.log" 2>&1 & a=$!
check "...a new run waits for it, saying whose run it is" wf "grep -q 'is busy with old-1 (browser, main, from old) since .*; waiting for it' '$root/a.log'"
sleep 3
check "...and has not started" [ "$(starts)" = 1 ]
touch "$root/release"; wait "$o"; wait "$a"; rc=$?
check "...then runs once the old one has ended (rc=$rc)" [ "$rc:$(head -3 "$root/starts.log" | cut -d' ' -f1,2 | tr '\n' ' ')" = "0:start old-1 end old-1 start $vm/.cache/comebye-remote/runs/$(sed -n 's/^remote: kv1 \([a-z0-9-]*\): .*/\1/p' "$root/a.log")/src " ]
rm -f "$root/release"; : >"$root/starts.log"
"$r" kv1 browser rest >"$root/a.log" 2>&1 & a=$!
wf '[ "$(starts)" = 1 ]'
check "a new run holds a slot: an old client's check sees the VM busy" bash -c "[ -e '$rr/run.lock' ] && ! flock -n '$rr/run.lock' true"
old_vm_side 2; o=$!
sleep 3
check "...an old client waits for it, though a slot is free" [ "$(grep -c 'old-2' "$root/starts.log")" = 0 ]
touch "$root/release"; wait "$a"; wait "$o"
check "...and runs once the new run has ended" [ "$(sed -n '2,3p' "$root/starts.log" | cut -d' ' -f1,2 | tr '\n' ' ')" = "end $vm/.cache/comebye-remote/runs/$(sed -n 's/^remote: kv1 \([a-z0-9-]*\): .*/\1/p' "$root/a.log")/src start old-2 " ]
# The real old client, when this checkout has its history (CI's shallow lint checkout does not).
if git -C "$here" cat-file -e cca272b:tools/ci/remote.sh 2>/dev/null; then
  git -C "$here" show cca272b:tools/ci/remote.sh >"$root/caller3/tools/ci/remote-old.sh"; chmod +x "$root/caller3/tools/ci/remote-old.sh"
  rm -f "$root/release"; : >"$root/starts.log"
  "$r" kv1 browser rest >"$root/a.log" 2>&1 & a=$!
  wf '[ "$(starts)" = 1 ]'
  RUNNER_TEMP="$root/rt3" "$root/caller3/tools/ci/remote-old.sh" kv1 browser rest >"$root/c.log" 2>&1 & c=$!
  check "the real old remote.sh (cca272b) waits for a new run, saying whose" wf "grep -Eq '^remote\(vm\): .+ is busy with [a-z0-9-]+ \(browser, feat/x, .* waiting for it' '$root/c.log'"
  sleep 3
  check "...and has not started" [ "$(starts)" = 1 ]
  touch "$root/release"; wait "$a"; wait "$c"; rc=$?
  check "...then runs after it (rc=$rc)" [ "$rc:$(cut -d' ' -f1 "$root/starts.log" | tr '\n' ' ')" = "0:start end start end " ]
else
  echo "skip: no cca272b in this checkout's history, so no run of the real old client"
fi
rm -f wait.txt "$root/bind" "$root/release" "$vm/.cache/comebye-remote/test-slots"; unset COMEBYE_SLOT_PORT_BASE
for w in caller2 caller3; do git -C "$caller" worktree remove --force "$root/$w"; done

# ---- the pool (tools/ci/prefer-vm.sh with a test-pool file): most free slots first, start a stopped VM only when
# every running one is full, then wait for a free slot anywhere
bw="$caller/tools/ci/browser.sh"
mkvm() { # HOST SLOTS STATE: a fake VM with this checkout's pins
  local d; d="$(vmdir "$1")"; mkdir -p "$state/$1" "$d/etc" "$d/.cache/comebye-remote"
  printf 'Host %s\n  HostName 127.0.0.1\n' "$1" >"$state/$1/ssh_config"
  printf 'go=1.2.3\nnode=4.5.6\nchrome=7.8.9.10\n' >"$d/etc/test-runner"; echo "$2" >"$d/.cache/comebye-remote/test-slots"
  echo "$3" >"$root/vmstate-$1"
}
hold() { # HOST I [ROOT]: hold that slot (as a run would) until $root/rel-HOST-I appears
  local r="${3:-$(vmdir "$1")/.cache/comebye-remote}"; mkdir -p "$r"; rm -f "$root/rel-$1-$2"
  ( exec 7>>"$r/slot-$2.lock"; flock 7; until [ -e "$root/rel-$1-$2" ]; do sleep 0.1; done ) 9>&- &
  wf "! flock -n '$r/slot-$2.lock' true"
}
went() { sed -n "s|^pwd=$root/vm-\([a-z0-9-]*\)/.*|\1|p" "$RUNNER_TEMP/uitest-rest-dark/record.txt" 2>/dev/null; }
pool_run() { rm -rf "$RUNNER_TEMP"; out="$(timeout 60 "$bw" rest 2>&1)"; rc=$?; [ -z "${POOL_DEBUG:-}" ] || printf '%s\n' "$out" | grep '^tests:\|^remote: kv' >&2; }
mkvm kv-a 2 RUNNING; mkvm kv-b 1 RUNNING; mkvm kv-c 1 STOPPED
mkdir -p "$root/vm-kv-b/data"
cat >"$TEST_POOL_CONFIG" <<EOF
# tried in this order
vm kv-a
vm kv-b $root/kv-b.conf remote_root=$root/vm-kv-b/data nvme_max_c=68   # a key for later is left alone
vm kv-c
prometheus=http://example.invalid
EOF
printf 'host=kv1\n' >"$TEST_VM_CONFIG" # the pool comes first
echo 0 >rc.txt; : >"$root/worker.log"
pool_run
check "pool: the VM with the most free slots (kv-a, 2 of 2) (rc=$rc)" [ "$rc:$(went)" = 0:kv-a ]
check "...saying so, with the pool's free slots" grep -qx 'tests: running browser on the test VM kv-a: 2 of 2 slots free (pool: kv-a 2/2 free, kv-b 1/1 free, kv-c stopped; TEST_VM=local runs it here)' <<<"$out"
out="$(timeout 60 "$caller/tools/ci/gotest.sh" 2>&1)"; rc=$?
check "pool under set -e (gotest.sh): the last VM has fewer free slots (kv-b 1/1 after kv-a 2/2), still kv-a (rc=$rc)" \
  bash -c "[ '$rc' = 0 ] && grep -qx 'tests: running gotest on the test VM kv-a: 2 of 2 slots free (pool: kv-a 2/2 free, kv-b 1/1 free, kv-c stopped; TEST_VM=local runs it here)' <<<\"\$1\" && grep -q '^gotest where=vm args=$' <<<\"\$1\"" _ "$out"
hold kv-a 0
pool_run
check "pool: a tie (1 free each) goes to the one listed first" [ "$rc:$(went)" = 0:kv-a ]
hold kv-a 1
pool_run
check "pool: kv-a full, so kv-b (rc=$rc)" [ "$rc:$(went)" = 0:kv-b ]
check "...with kv-b's provisioner config from its pool line" grep -qx "with $root/kv-b.conf: status --json kv-b" "$root/worker.log"
check "...and its remote_root" grep -q "^pwd=$root/vm-kv-b/data/runs/" "$RUNNER_TEMP/uitest-rest-dark/record.txt"
check "no VM started while a running one had a free slot" bash -c "! grep -q '^start' '$root/worker.log'"
hold kv-b 0 "$root/vm-kv-b/data"
pool_run
check "pool: every running VM full: the first stopped one is started, and the run goes there (rc=$rc)" [ "$rc:$(went)" = 0:kv-c ]
check "...saying so" bash -c "grep -q '^remote: kv-c is STOPPED; starting it' <<<\"\$1\" && grep -q '^tests: running browser on the test VM kv-c: started, as every running VM was full (pool: kv-a 0/2 free, kv-b 0/1 free; ' <<<\"\$1\"" _ "$out"
check "...started once" [ "$(grep -c '^start kv-c$' "$root/worker.log")" = 1 ]
hold kv-c 0
check "pool full, TEST_VM_NO_WAIT=1: here, saying why" bash -c "out=\$(TEST_VM_NO_WAIT=1 '$bw' rest 2>&1); grep -q '^tests: running on this machine: every test VM in the pool is full (kv-a 0/2 free, kv-b 0/1 free, kv-c 0/1 free) and TEST_VM_NO_WAIT is set' <<<\"\$out\" && grep -q '^where=local' '$RUNNER_TEMP/uitest-rest-dark/where.txt'"
rm -rf "$RUNNER_TEMP"; TEST_POOL_POLL=1 "$bw" rest >"$root/p.log" 2>&1 & p=$!
check "pool full: the run waits for a free slot anywhere, saying so" wf "grep -q '^tests: every test VM in the pool is full (kv-a 0/2 free, kv-b 0/1 free, kv-c 0/1 free); waiting for a free slot anywhere in it (Ctrl-C gives up)$' '$root/p.log'"
sleep 2
check "...and has not run yet" [ ! -e "$RUNNER_TEMP/uitest-rest-dark/record.txt" ]
touch "$root/rel-kv-b-0"; wait "$p"; rc=$?
check "...then takes the slot that frees (kv-b) (rc=$rc)" [ "$rc:$(went)" = 0:kv-b ]
check "...saying how long it waited" grep -Eq '^tests: running browser on the test VM kv-b: a slot free after [0-9]+ s \(pool: kv-a 0/2 free, kv-b 1/1 free, kv-c 0/1 free; ' "$root/p.log"
touch "$root/rel-kv-a-0" "$root/rel-kv-a-1" "$root/rel-kv-c-0"; wait
TEST_VM=kv-b pool_run
check "TEST_VM=kv-b with a pool: kv-b, with its pool line's remote_root (rc=$rc)" bash -c "[ '$rc:$(went)' = 0:kv-b ] && grep -q '^pwd=$root/vm-kv-b/data/runs/' '$RUNNER_TEMP/uitest-rest-dark/record.txt'"
touch "$root/down-kv-a"; pins_b() { printf 'go=0\nnode=0\nchrome=0\n' >"$root/vm-kv-b/etc/test-runner"; }; pins_b
echo STOPPED >"$root/vmstate-kv-c"
FAKE_START_FAIL=1 pool_run
check "pool: nothing usable (unreachable, wrong pins, a start that fails): here, saying why (rc=$rc)" \
  rc_says "$rc" 0 '^tests: running on this machine: no test VM in the pool is available (kv-a unreachable, kv-b without this checkout.s pins, kv-c would not start (kv-c did not start' "$out"
rm -f "$root/down-kv-a"
printf 'vm kv-a\n' >"$TEST_POOL_CONFIG"
pool_run
check "a pool of one: as test-vm (rc=$rc)" bash -c "[ '$rc:$(went)' = 0:kv-a ] && grep -qx 'tests: running browser on the test VM kv-a (TEST_VM=local runs it here)' <<<\"\$1\"" _ "$out"
rm -f "$TEST_POOL_CONFIG" "$TEST_VM_CONFIG"

# ---- temperatures (tools/ci/temps.sh): the heat check in the pool, and the log of every run
tlog="$root/nope/comebye-remote/temps.log"
prom_json etna.x 72 39 rozalin.x 73.4 62.5 >"$root/prom.json"
check "temps.sh: one line per node, whole degrees" [ "$("$here/temps.sh" http://fake-prom | tr '\n' ';')" = "etna.x cpu=72 nvme=39;rozalin.x cpu=73 nvme=63;" ]
check "temps.sh NODE: that node only" [ "$("$here/temps.sh" http://fake-prom rozalin.x)" = "rozalin.x cpu=73 nvme=63" ]
check "temps.sh: the query joins both exporters to their node by pod (kube_pod_info)" \
  bash -c "grep -q 'smartctl_device_temperature{temperature_type=\"current\",device=~\"nvme.\*\"} \* on (pod) group_left (node)' '$root/prom.log' && grep -q 'chip_name=~\"coretemp|k10temp\"} \* on (pod) group_left (node) max by (pod, node) (kube_pod_info{namespace=\"monitoring\"})' '$root/prom.log'"
touch "$root/prom-slow"; t0=$SECONDS; o="$("$here/temps.sh" http://fake-prom)"; rc=$?
took=$((SECONDS - t0))
check "temps.sh: Prometheus hangs: nothing, exit 0, within 3 s ($took s)" [ "$rc:$o:$((took <= 3))" = "0::1" ]
rm -f "$root/prom-slow"
check "temps.sh: Prometheus down: nothing, exit 0" [ "$(mv "$root/prom.json" "$root/p.j"; "$here/temps.sh" http://fake-prom; echo "rc=$?"; mv "$root/p.j" "$root/prom.json")" = "rc=0" ]
echo rozalin.x >"$root/node-kv1"
check "--state: the state, then the VM's node" [ "$("$r" --state kv1 2>/dev/null)" = "RUNNING rozalin.x" ]

# the heat check: kv-a has the most free slots, but its node is hot
mkvm kv-a 2 RUNNING; mkvm kv-b 1 RUNNING; mkvm kv-c 1 STOPPED; rm -f "$root/down-kv-a"
printf 'go=1.2.3\nnode=4.5.6\nchrome=7.8.9.10\n' >"$root/vm-kv-b/etc/test-runner"
echo na.x >"$root/node-kv-a"; echo nb.x >"$root/node-kv-b"; echo nc.x >"$root/node-kv-c"
prom_json na.x 90 70 nb.x 60 50 nc.x 95 80 >"$root/prom.json"
cat >"$TEST_POOL_CONFIG" <<EOF
prometheus=http://fake-prom
vm kv-a
vm kv-b
vm kv-c
EOF
: >"$root/prom.log"; : >"$root/worker.log"
pool_run
check "heat: a VM whose node's NVMe is at or over 68 is skipped; the run goes to the next (rc=$rc)" [ "$rc:$(went)" = 0:kv-b ]
check "...saying so in one line" [ "$(grep -c '^tests: skipping test VM kv-a: its node na.x has its NVMe at 70°C (nvme_max_c=68)$' <<<"$out")" = 1 ]
check "...one Prometheus query for the whole pool (and the run's own two for the log)" [ "$(grep -c 'smartctl' "$root/prom.log")" = 3 ]
check "...and the run's log line has kv-b's node and its temperatures" grep -q " vm=kv-b node=nb.x slot=1/1 suite=browser secs=[0-9]* rc=0 cpu_start=60 nvme_start=50 cpu_end=60 nvme_end=50 run=" <<<"$(tail -1 "$tlog")"
sed -i 's/^vm kv-a$/vm kv-a nvme_max_c=75/' "$TEST_POOL_CONFIG"
pool_run
check "heat: a VM's own nvme_max_c (75) lets it through (rc=$rc)" [ "$rc:$(went)" = 0:kv-a ]
sed -i 's/^vm kv-a nvme_max_c=75$/vm kv-a/; 1a nvme_max_c=72' "$TEST_POOL_CONFIG"
pool_run
check "heat: a pool-wide nvme_max_c line (72) too (rc=$rc)" [ "$rc:$(went)" = 0:kv-a ]
sed -i '/^nvme_max_c=/d' "$TEST_POOL_CONFIG"
mv "$root/prom.json" "$root/p.j"
pool_run
check "heat: Prometheus does not answer: nothing skipped (rc=$rc)" [ "$rc:$(went):$(grep -c skipping <<<"$out")" = 0:kv-a:0 ]
touch "$root/prom-slow"; t0=$SECONDS
pool_run; took=$((SECONDS - t0))
check "heat: Prometheus hangs: nothing skipped, and it costs at most its 2 s twice ($took s)" [ "$rc:$(went):$((took <= 12))" = 0:kv-a:1 ]
check "...the run is logged with its temperatures empty" grep -q " vm=kv-a node=na.x slot=1/2 suite=browser secs=[0-9]* rc=0 cpu_start= nvme_start= cpu_end= nvme_end= run=" <<<"$(tail -1 "$tlog")"
rm -f "$root/prom-slow"; mv "$root/p.j" "$root/prom.json"
hold kv-b 0
: >"$root/worker.log"
out="$(TEST_VM_NO_WAIT=1 timeout 60 "$bw" rest 2>&1)"
check "heat: a hot stopped VM is not started either" bash -c "! grep -q '^start' '$root/worker.log' && grep -q 'skipping test VM kv-c: its node nc.x' <<<\"\$1\"" _ "$out"
touch "$root/rel-kv-b-0"; wait
rm -f "$TEST_POOL_CONFIG"

# the log of a run on kv1 (test-vm, prometheus= there), temperatures as it started and as its suite ended
printf 'host=kv1\nprometheus = http://fake-prom   # for the log\n' >"$TEST_VM_CONFIG"
prom_json rozalin.x 61 60 >"$root/prom.json"; prom_json rozalin.x 88 71 >"$root/prom-end.json"
echo 3 >rc.txt
rm -rf "$RUNNER_TEMP"; out="$(timeout 60 "$bw" rest 2>&1)"; rc=$?
check "the log: one line per run, start and end temperatures, slot, suite, VM, node, time and status (rc=$rc)" \
  grep -Eq '^20[0-9-]+T[0-9:]+Z vm=kv1 node=rozalin.x slot=1/1 suite=browser secs=[0-9]+ rc=3 cpu_start=61 nvme_start=60 cpu_end=88 nvme_end=71 run=[a-z0-9-]+ branch=feat/x$' <<<"$(tail -1 "$tlog")"
rm -f "$root/prom-end.json"
printf 'host=kv1\n' >"$TEST_VM_CONFIG"; echo 0 >rc.txt
rm -rf "$RUNNER_TEMP"; out="$(timeout 60 "$bw" rest 2>&1)"
check "no Prometheus: still logged, temperatures empty" grep -q ' vm=kv1 node=rozalin.x slot=1/1 suite=browser secs=[0-9]* rc=0 cpu_start= nvme_start= cpu_end= nvme_end= ' <<<"$(tail -1 "$tlog")"
rm -f "$root/node-kv1"
out="$("$r" kv1 browser rest 2>&1)"
check "no node from the provisioner: logged, node and temperatures empty" grep -q ' vm=kv1 node= slot=1/1 ' <<<"$(tail -1 "$tlog")"
n0=$(wc -l <"$tlog"); "$r" --probe kv1 >/dev/null 2>&1; "$r" --check kv1 >/dev/null 2>&1; "$r" --state kv1 >/dev/null 2>&1
check "--probe, --check and --state log nothing" [ "$(wc -l <"$tlog")" = "$n0" ]
rm -f "$TEST_VM_CONFIG" "$root/prom.json"
exit $fail
