#!/usr/bin/env bash
# Run a heavy suite on a worker VM instead of this machine, on this worktree's exact state.
#
#   tools/ci/remote.sh HOST SUITE [ARG...]
#   tools/ci/remote.sh --check HOST     only the checks below (and a start); exit 0 when a run would go ahead
#   tools/ci/remote.sh --probe HOST     one ssh, no start: prints `slots=N free=F toolchain=ok|bad` (255: ssh failed)
#   tools/ci/remote.sh --state HOST     no ssh: prints the provisioner's state (RUNNING, STOPPED, ...; none: no such VM;
#                                       an empty line: no answer), then the VM's node when the provisioner names one.
#                                       For the pool picker in tools/ci/prefer-vm.sh.
#   --no-wait (before HOST, or REMOTE_NO_WAIT=1): refuse when the VM is busy instead of waiting for it
#
#   SUITE  browser ARG...   tools/ci/browser.sh ARG...    artifacts -> ARG 3, or ${RUNNER_TEMP:-/tmp}/uitest-SUITE-SCHEME
#          flakes ARG...    tools/ci/flakes.sh ARG...     artifacts -> $OUT_DIR, or a new /tmp/flakes.XXXXXX
#                           (`flakes repeat SCRIPT... N` is read as flakes.sh's `repeat N SCRIPT...`)
#          prelanding [OUT] tools/ci/prelanding.sh        artifacts -> OUT, or a new /tmp/prelanding.XXXXXX
#          gotest ARG...    tools/ci/gotest.sh ARG...     (output only)
#          go ARG...        go ARG...                     (output only), e.g. go test -race -p 4 ./...
#
# HOST is a worker the provisioner (tools/worker) made: its ssh config is $COMEBYE_WORKER_STATE/HOST/ssh_config
# (default ${XDG_STATE_HOME:-~/.local/state}/comebye-worker; set it if the provisioner's config has a state_dir).
# The worktree's HEAD (a git bundle, with origin/main when there is one), its tracked changes (git diff --binary HEAD)
# and its untracked files that are not ignored go to a fresh folder on the VM; nothing else does: no .git/config,
# no remotes, no hooks, no ~/.config. Untracked files that look like secrets (keys, .env, tokens, credentials) are
# left out, with a line saying so. The suite runs there under tools/ci/capped.sh (memory and JOBS sized to the
# VM), with the pinned toolchain first on PATH and GOTOOLCHAIN=local, its output streamed here. Its artifacts come
# back to the local path above, the run folder on the VM is removed, and this exits with the suite's status.
# Go, npm and uitest's node_modules stay warm in the VM's cache folder (REMOTE_ROOT, default ~/.cache/comebye-remote
# on the VM). Forwarded when set here: SCHEME EXTRA PARALLEL JOBS JOBS_FORCE GOTEST_P CAP_MEM BASE PORT REPEAT_PORT
# (on a VM with more than one slot, the slot's own values below win over these).
#
# A worker powers itself off when idle. A stopped one is started first, through the provisioner (`worker start HOST`,
# saying so and how long it took); REMOTE_START=0 refuses instead. While the suite runs, an idle hold on the VM
# (remote-test-ID, 20 min, renewed every 5) keeps it from powering off: a plain ssh session is not activity to it.
# Beside it, once the suite has its slot, the run's record (test-runs/ID.json in the VM's comebye state dir, renewed
# and expiring with the hold, removed when the run ends) says what runs there: the run, slot I/N, suite, branch, HEAD,
# start, the starting agent (`comebye whoami --json` here; empty when there is none) and this host. The VM's comebye
# reports the unexpired ones to the hub, and the app shows them on the VM's row and the agent's.
# When the suite ends, its done record goes to test-runs/done/ID.json: the same fields and the end, seconds and exit
# status, with the node's CPU and NVMe temperatures at the start and the end (the log's below) added by this side's
# cleanup. Beside it, done/ID.log keeps the last 256 KiB of what the suite printed (its stdout and stderr, nothing
# else). The VM writes both itself, so a run whose caller was killed still has them; when the VM's script was killed
# instead, this side's cleanup writes them from what the run left. The VM keeps the last 50 (and their logs); its
# comebye reports the last 20, and serves a log to the hub.
# (REMOTE_RENEW_SECS, default 300, is how often both are renewed; the self-test uses it.)
# The provisioner is $COMEBYE_WORKER_TOOL (words, e.g. "comebye-worker -config FILE"), else comebye-worker if it is
# on PATH, else `go run ./tools/worker`; COMEBYE_WORKER_CONFIG=FILE adds `-config FILE`. The repository is the one
# this script lives in, whatever the current directory. The heavy wrappers call this by themselves when a test VM
# is configured (tools/ci/prefer-vm.sh).
#
# Slots: a VM runs up to N suites at once, N from its test-slots file (REMOTE_ROOT/test-slots, else
# ~/.cache/comebye-remote/test-slots, where `worker create` and `worker toolchain` write it), 1 when there is none.
# A run takes a free slot there (REMOTE_ROOT/slot-I.lock, I from 0, who holds it in slot-I.who) before its suite
# starts. When every slot is taken, this one says whose runs hold them and since when, and waits for any slot, under
# its idle hold; Ctrl-C gives up.
# A killed run's slot goes with its processes. With N > 1 each slot gets its share of the VM, so suites side by side
# cannot clash: GOTEST_P = vCPUs / N, JOBS the same but at most 4, CAP_MEM = 85% of RAM / N (a lower value sent from
# here is kept), PARALLEL at most 4, and PORT = REPEAT_PORT = 18741 + I * 5000: every port a browser or flakes run
# uses is within that 5000 (run.sh uses PORT..PORT+401; PARALLEL's parts and flakes' JOBS add 1000 each, so 4 of
# them reach PORT+3401). remote_test.sh checks those offsets against the real scripts. With N = 1 nothing is set.
# (COMEBYE_SLOT_PORT_BASE on the VM moves the 18741; the self-test uses it.) An old client waiting for run.lock can be
# kept waiting while new runs overlap on a busy VM: flock lets shared holders in ahead of it.
# Clients from before slots took REMOTE_ROOT/run.lock exclusively. A run here also holds run.lock, shared, for as
# long as its slot: an old client's exclusive lock waits until no new run is going, and a new run waits while an
# old one holds it, so old and new clients still take turns on the same VM.
#
# Temperatures: every run that is sent appends one line to a local log, ${COMEBYE_REMOTE_LOG:-${XDG_STATE_HOME:-
# ~/.local/state}/comebye-remote/temps.log}: when it started, the VM, its node (`worker status --json`), the slot,
# the suite, how long it took and its exit status, and the node's CPU and NVMe temperatures as it started and as its
# suite ended (tools/ci/temps.sh), e.g.
#   2026-11-23T18:30:00Z vm=kv1 node=N slot=1/1 suite=browser secs=312 rc=0 cpu_start=72 nvme_start=63 cpu_end=88 nvme_end=71 run=ID branch=B
# Prometheus is $TEST_PROMETHEUS, else the `prometheus=URL` word on this VM's line of the test pool, else a
# `prometheus=URL` line in the test pool or test-vm file (tools/ci/prefer-vm.sh). A reading never fails or holds up a
# run: each is taken alongside other work and gives up after 2 s, leaving its fields empty, as does a run with no
# Prometheus or no node. A run stopped with Ctrl-C is logged too, with its exit status.
#
# Refused before anything runs (exit 2): a HOST with no provisioner ssh config or no VM, a VM that is stopped with
# REMOTE_START=0 or would not start, a VM whose toolchain is missing or not this checkout's pins
# (`worker toolchain HOST` installs them), and a second run from the same worktree while one is going.
# 255: ssh could not reach the VM.
set -uo pipefail

usage() { sed -n '2,/^set -uo/{/^#/p}' "${BASH_SOURCE[0]}" >&2; exit 2; }
die() { echo "remote: $*" >&2; exit 2; }
check=""; probe=""; state_only=""; nowait="${REMOTE_NO_WAIT:-}"; [ "$nowait" = 0 ] && nowait=""
while :; do case "${1:-}" in --check) check=1; shift ;; --probe) check=1; probe=1; shift ;; --state) check=1; state_only=1; shift ;; --no-wait) nowait=1; shift ;; *) break ;; esac; done
if [ -n "$check" ]; then [ $# = 1 ] || usage; else [ $# -ge 2 ] || usage; fi
host="$1"; shift
[[ "$host" =~ ^[a-z][a-z0-9-]{0,62}$ ]] || die "host '$host': want a worker name (lower-case letters, digits, '-')"

# q: one shell word, for a command line the VM's shell reads.
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

# The VM's slots, as shell functions its scripts start with ($root is REMOTE_ROOT there). nslots: N from the
# test-slots file (1 when absent or not a number; at most 8, so the port ranges stay below 65536). old_holds: a
# client from before slots holds run.lock (exclusively). nfree N: the free slots, 0 while an old client holds the VM.
# holders N: who holds the VM, as "WHO since TIME", several joined with "; ".
slot_fns='slots_file="$root/test-slots"; [ -e "$slots_file" ] || slots_file="$HOME/.cache/comebye-remote/test-slots"
nslots() { local n=""; [ -r "$slots_file" ] && read -r n <"$slots_file"; n="${n//[[:space:]]/}"; [[ "$n" =~ ^[1-9][0-9]*$ ]] || n=1; [ "$n" -le 8 ] || n=8; echo "$n"; }
old_holds() { [ -e "$root/run.lock" ] && ! flock -s -n "$root/run.lock" true 2>/dev/null; }
slot_busy() { [ -e "$root/slot-$1.lock" ] && ! flock -n "$root/slot-$1.lock" true 2>/dev/null; }
nfree() { local i f=0; old_holds && { echo 0; return; }; for ((i = 0; i < $1; i++)); do slot_busy "$i" || f=$((f + 1)); done; echo "$f"; }
holders() { local i w=""
  if old_holds; then cat "$root/run.who" 2>/dev/null || echo "another run"; return; fi
  for ((i = 0; i < $1; i++)); do slot_busy "$i" && w="$w${w:+; }$(cat "$root/slot-$i.who" 2>/dev/null || echo "another run")"; done
  echo "${w:-another run}"; }'

# The run's done record (see the top), as a shell function both the VM's script and this side's cleanup start with.
# done_write RUN FIXED DIR ID RC [EXTRA]: from RUN/slot, RUN/started and RUN/t0 (written when the suite took its slot:
# nothing is written for a run that never had one), the record DIR/ID.json with FIXED's members, the end, seconds and
# RC, then EXTRA (more members, each starting with a comma); and DIR/ID.log, the last 256 KiB of RUN/out.log. Each is
# written whole (a .tmp, then a rename). Then only the newest 50 records, and their logs, are kept.
done_fns='done_write() { local run="$1" fixed="$2" d="$3" id="$4" rc="$5" x="${6:-}" s st t0 now f
  read -r s <"$run/slot" 2>/dev/null && read -r st <"$run/started" 2>/dev/null && read -r t0 <"$run/t0" 2>/dev/null || return 0
  [[ "$rc" =~ ^[0-9]+$ ]] || rc=255; mkdir -p "$d" || return 0; now="$(date +%s)"
  if [ -f "$run/out.log" ]; then tail -c 262144 "$run/out.log" >"$d/$id.log.tmp" && mv -f "$d/$id.log.tmp" "$d/$id.log"; fi
  printf "{\"v\":1,%s,\"slot\":%s,\"slots\":%s,\"started\":\"%s\",\"ended\":\"%s\",\"secs\":%s,\"rc\":%s%s}\n" "$fixed" "${s%/*}" "${s#*/}" "$st" \
    "$(date -u -d "@$now" +%Y-%m-%dT%H:%M:%SZ)" "$((now - t0))" "$rc" "$x" >"$d/$id.json.tmp" && mv -f "$d/$id.json.tmp" "$d/$id.json"
  ls -1t "$d"/*.json 2>/dev/null | tail -n +51 | while IFS= read -r f; do rm -f "$f" "${f%.json}.log"; done; }'

here_dir="$PWD"
top="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel 2>/dev/null)" || die "$(dirname "${BASH_SOURCE[0]}") is not in a git worktree"
cd "$top" || exit 2
state="${COMEBYE_WORKER_STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/comebye-worker}"
cfg="$state/$host/ssh_config"
[ -f "$cfg" ] || die "$host is not a worker this provisioner knows (no $cfg)"
ssh_vm() { ssh -F "$cfg" -o BatchMode=yes "$host" "$@" 9>&-; }

# ---- the suite, and where its artifacts land here
kind="${1:-}"; [ -n "$check" ] && kind=go; shift || true
loc_art=""; rem_art_arg=""; env_art=""
args=("$@")
case "$kind" in
  browser)
    [ ${#args[@]} -ge 1 ] || die "browser needs a suite (smoke, rest, ...)"
    loc_art="${args[2]:-${RUNNER_TEMP:-/tmp}/uitest-${args[0]}-${args[1]:-dark}}"
    [ ${#args[@]} -ge 2 ] || args+=(dark)
    rem_art_arg=2 # args[2] becomes the VM's artifact folder
    cmd=(tools/ci/browser.sh) ;;
  flakes)
    if [ "${args[0]:-}" = repeat ] && [ ${#args[@]} -ge 3 ] && ! [[ "${args[1]}" =~ ^[0-9]+$ ]] && [[ "${args[-1]}" =~ ^[0-9]+$ ]]; then
      args=(repeat "${args[-1]}" "${args[@]:1:${#args[@]}-2}")
      echo "remote: running flakes.sh ${args[*]}" >&2
    fi
    loc_art="${OUT_DIR:-}"; env_art=OUT_DIR
    cmd=(tools/ci/flakes.sh) ;;
  prelanding)
    loc_art="${args[0]:-}"; args=(); rem_art_arg=0
    cmd=(tools/ci/prelanding.sh) ;;
  gotest) cmd=(tools/ci/gotest.sh) ;;
  go) cmd=(go) ;;
  *) echo "remote: unknown suite '$kind'" >&2; usage ;;
esac
case "$loc_art" in ""|/*) ;; *) loc_art="$here_dir/$loc_art" ;; esac # a relative OUT_DIR is the caller's, not $top's

# ---- one run per worktree at a time
gitdir="$(git rev-parse --absolute-git-dir)"
lock="$gitdir/comebye-remote.lock"
exec 9>"$lock" || die "cannot open $lock"
[ -n "$check" ] || flock -n 9 || die "another remote run from this worktree is going ($(cat "$gitdir/comebye-remote.run" 2>/dev/null || echo '?')); wait for it"

me="$(hostname -s 2>/dev/null | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-\n' - | cut -c1-16)"
run_id="${me:-host}-$(date +%Y%m%d-%H%M%S)-$$" # also the idle hold's name: [a-z0-9-], at most 63 with its prefix
[ -n "$check" ] || echo "$host $run_id" >"$gitdir/comebye-remote.run"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/remote.XXXXXX")"
hold=""; sent=""; vm_node=""; t0pid=""; t1pid=""
rroot='"${REMOTE_ROOT:-$HOME/.cache/comebye-remote}"'
[ -n "${REMOTE_ROOT:-}" ] && rroot="$(q "$REMOTE_ROOT")"
rrun="$rroot/runs/$(q "$run_id")"
rhold='"${WK_IDLE_HOLDS:-$HOME/.local/state/comebye/idle-holds}"'"/remote-test-$run_id"
rrec='"${WK_TEST_RUNS:-$HOME/.local/state/comebye/test-runs}"'"/$run_id.json" # the run's record (internal/testruns)
rdone='"${WK_TEST_RUNS:-$HOME/.local/state/comebye/test-runs}"'"/done" # finished runs' records and logs
rec_fixed=""
renew_secs="${REMOTE_RENEW_SECS:-300}"; [[ "$renew_secs" =~ ^[1-9][0-9]*$ ]] || renew_secs=300
# hold_cmd MINUTES: the VM's shell line that holds the VM up that long (idle-check.sh reads "<until> <reason>").
hold_cmd() { echo "h=$rhold; mkdir -p \"\${h%/*}\" && echo \"\$((\$(date +%s) + $1 * 60)) remote test run $run_id\" >\"\$h.tmp\" && mv -f \"\$h.tmp\" \"\$h\""; }
spid=""
cleanup() {
  local st=$? slot=""
  [ -n "$hold" ] && kill "$hold" 2>/dev/null
  if [ -n "$spid" ]; then # stopped mid-run: give the VM 15 s to stop the suite, then drop the link
    ( sleep 15; kill "$spid" ) >/dev/null 2>&1 9>&- & local t=$!
    wait "$spid" 2>/dev/null; kill "$t" 2>/dev/null
  fi
  if [ -n "$sent" ]; then
    [ -n "$t1pid" ] || temps_at end
    temps_read
    # The done record: the VM's own gets the temperatures; when there is none (its script was killed), this side
    # writes it from what the run left. Then the run folder goes.
    local x="" k v fin=""
    for k in cpu_start:"$c0" nvme_start:"$n0" cpu_end:"$c1" nvme_end:"$n1"; do v="${k#*:}"; [[ "$v" =~ ^[0-9]{1,3}$ ]] && x="$x,\"${k%%:*}\":$v"; done
    [ -n "$rec_fixed" ] && fin="$done_fns
      f=$rdone/$run_id.json; x=$(q "$x")
      if [ -s \"\$f\" ]; then [ -z \"\$x\" ] || { sed \"s/}\\\$/\$x}/\" \"\$f\" >\"\$f.tmp\" && mv -f \"\$f.tmp\" \"\$f\"; }
      else done_write $rrun $(q "$rec_fixed") $rdone $run_id $st \"\$x\"; fi"
    slot="$(ssh_vm "cat $rrun/slot 2>/dev/null; $fin
      rm -rf $rrun; rm -f $rhold $rrec" </dev/null 2>/dev/null | head -1)"
    log_run "$st" "$slot"
  fi
  rm -rf "$tmp"; [ -n "$check" ] || rm -f "$gitdir/comebye-remote.run"
}
# The temperature log (see the top). prom_url: where Prometheus is, or nothing.
prom_url() {
  [ -n "${TEST_PROMETHEUS:-}" ] && { echo "$TEST_PROMETHEUS"; return; }
  local c="${XDG_CONFIG_HOME:-$HOME/.config}/comebye-worker" f
  for f in "${TEST_POOL_CONFIG:-$c/test-pool}" "${TEST_VM_CONFIG:-$c/test-vm}"; do
    [ -f "$f" ] || continue
    awk -v h="$host" '
      $1 == "vm" && $2 == h { for (i = 3; i <= NF && $i !~ /^#/; i++) if ($i ~ /^prometheus=/) { mine = substr($i, 12) } }
      $0 ~ /^[[:space:]]*prometheus[[:space:]]*=/ && !all { l = $0; sub(/^[^=]*=[[:space:]]*/, "", l); sub(/[[:space:]]*(#.*)?$/, "", l); all = l }
      END { if (mine != "") print mine; else if (all != "") print all }' "$f" 2>/dev/null | head -1 | grep . && return
  done
}
# temps_at start|end: this VM's node's temperatures into $tmp/temps-start|end, in the background (2 s at most).
temps_at() {
  local p; [ -n "$vm_node" ] && p="$(prom_url)" && [ -n "$p" ] || return 0
  "$(dirname "${BASH_SOURCE[0]}")/temps.sh" "$p" "$vm_node" >"$tmp/temps-$1" 2>/dev/null 9>&- &
  if [ "$1" = start ]; then t0pid=$!; else t1pid=$!; fi
}
# temps_read: the readings, once both are in, as c0 n0 (start) and c1 n1 (end), whole degrees or empty.
c0=""; n0=""; c1=""; n1=""
temps_read() {
  [ -n "$t0pid" ] && wait "$t0pid" 2>/dev/null; [ -n "$t1pid" ] && wait "$t1pid" 2>/dev/null
  read -r _ c0 n0 <"$tmp/temps-start" 2>/dev/null; read -r _ c1 n1 <"$tmp/temps-end" 2>/dev/null
  c0="${c0#cpu=}" n0="${n0#nvme=}" c1="${c1#cpu=}" n1="${n1#nvme=}"
}
# log_run RC SLOT: the run's line in the log; never fails the run.
log_run() {
  local f="${COMEBYE_REMOTE_LOG:-${XDG_STATE_HOME:-$HOME/.local/state}/comebye-remote/temps.log}"
  { mkdir -p "$(dirname "$f")" &&
    echo "$started vm=$host node=$vm_node slot=$2 suite=$kind secs=$((SECONDS - t_sent)) rc=$1 cpu_start=$c0 nvme_start=$n0 cpu_end=$c1 nvme_end=$n1 run=$run_id branch=$branch" >>"$f"
  } 2>/dev/null
  return 0
}
trap cleanup EXIT
trap 'exit 130' INT; trap 'exit 143' TERM HUP

# ---- the VM is up: a stopped one is started through the provisioner
if [ -n "${COMEBYE_WORKER_TOOL:-}" ]; then read -r -a wtool <<<"$COMEBYE_WORKER_TOOL"
elif command -v comebye-worker >/dev/null; then wtool=(comebye-worker)
else wtool=(go run ./tools/worker); fi
[ -n "${COMEBYE_WORKER_CONFIG:-}" ] && wtool+=(-config "$COMEBYE_WORKER_CONFIG")
# The cloud's view, no ssh: RUNNING, STOPPED, ...; "none" when the provisioner has no such VM, "" when it did not answer.
# vm_state: "STATE NODE", the node when the provisioner names one (kubevirt: where the VM runs or is pinned).
vm_state() {
  local j; j="$("${wtool[@]}" status --json "$host" 2>/dev/null 9>&-)" || return 0
  grep -q '"name"' <<<"$j" || { echo none; return 0; }
  echo "$(sed -n 's/.*"state": *"\([^"]*\)".*/\1/p' <<<"$j" | head -1) $(sed -n 's/.*"node": *"\([^"]*\)".*/\1/p' <<<"$j" | head -1)"
}
vm=RUNNING; [ -n "$probe" ] || read -r vm vm_node <<<"$(vm_state)" # a probe asks the VM only: the caller asked the provisioner already
if [ -n "$state_only" ]; then echo "$vm${vm_node:+ $vm_node}"; exit 0; fi
case "$vm" in
  RUNNING|"") ;;
  none) die "the provisioner (${wtool[*]}) has no VM $host" ;;
  STOPPED|TERMINATED|SUSPENDED|Stopped|Paused)
    [ "${REMOTE_START:-1}" = 0 ] && die "$host is $vm (a worker powers off when idle). Start it with: ${wtool[*]} start $host
  (or leave REMOTE_START unset and this starts it)"
    echo "remote: $host is $vm; starting it (${wtool[*]} start $host)" >&2
    t0=$SECONDS
    "${wtool[@]}" start "$host" >&2 9>&- || die "$host did not start (${wtool[*]} start $host failed; ${wtool[*]} status $host says more)"
    echo "remote: $host started in $((SECONDS - t0)) s" >&2 ;;
  *) die "$host is $vm: neither up nor stopped; try again shortly (${wtool[*]} status $host)" ;;
esac

# ---- the VM has this checkout's pinned toolchain
pins="tools/worker/testrunner.go"
[ -f "$pins" ] || die "no $pins here: is this a comebye checkout?"
pin() { sed -n "s/^[[:space:]]*$1:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$pins" | head -1; }
want="go=$(pin GoVersion) node=$(pin NodeVersion) chrome=$(pin ChromeVersion)"
have="$(ssh_vm 'f="${COMEBYE_TEST_RUNNER_FILE:-/etc/comebye-worker/test-runner}"; export PATH="/usr/local/bin:$HOME/.local/bin:/usr/bin:/bin:$PATH"
  for t in go node npm google-chrome; do command -v "$t" >/dev/null || { echo "missing $t"; exit 0; }; done
  [ -f "$f" ] && tr "\n" " " <"$f" || echo "no $f"
  root='"$rroot"'; echo
  '"$slot_fns"'
  n="$(nslots)"; free="$(nfree "$n")"; echo "SLOTS $n $free"
  [ "$free" -gt 0 ] || echo "BUSY $(holders "$n")"' </dev/null)"
rc=$?
busy="$(sed -n 's/^BUSY //p' <<<"$have")"; slots="$(sed -n 's/^SLOTS //p' <<<"$have")"; have="$(head -1 <<<"$have")"
[ $rc = 255 ] && { echo "remote: ssh could not reach $host${probe:+" (ssh -F $cfg $host)"}${probe:-" (the provisioner says: ${vm:-no answer}; ${wtool[*]} status $host)"}" >&2; exit 255; }
have="${have% }"
if [ -n "$probe" ]; then
  read -r n f <<<"$slots"
  echo "slots=${n:-1} free=${f:-0} toolchain=$([ "$have" = "$want" ] && echo ok || echo bad)"
  exit 0
fi
[ "$have" = "$want" ] || die "$host has no test toolchain at this checkout's pins (it says: ${have:-nothing}; want: $want).
  Install it with: go run ./tools/worker toolchain $host"
if [ -n "$busy" ]; then
  [ -n "$nowait" ] && die "$host is busy with ${busy:-another run}: not waiting (--no-wait)"
  echo "remote: $host is busy with $busy; this run will wait its turn" >&2
fi
[ -n "$check" ] && { echo "remote: $host is ready ($have)" >&2; exit 0; }

# ---- what is sent: HEAD, the tracked changes, the untracked files (no .git/config, no remotes)
head="$(git rev-parse HEAD)" || die "no HEAD"
branch="$(git symbolic-ref --quiet --short HEAD || echo detached)"
refs=(HEAD)
git rev-parse --verify --quiet refs/remotes/origin/main >/dev/null && refs+=(refs/remotes/origin/main)
git bundle create "$tmp/repo.bundle" "${refs[@]}" 2>/dev/null || die "git bundle failed"
git diff --binary HEAD >"$tmp/changes.patch" || die "git diff failed"
# Untracked files that look like secrets stay here: well-known names and key files, and any other file whose name
# says token, secret, credential or password (but not source code, so a new token.go still goes).
secret='(^|/)(\.env(\..*)?|\.netrc|\.git-credentials|\.npmrc|id_(rsa|ed25519|ecdsa|dsa)(\.pub)?)$|\.(pem|key|p12|pfx)$'
secret_word='(token|secret|credential|password)[^/]*$'
code='\.(go|js|mjs|cjs|ts|css|html|md|sh|py|sql)$'
: >"$tmp/untracked.list"
while IFS= read -r -d '' f; do
  if [[ "$f" =~ $secret ]] || { [[ "${f,,}" =~ $secret_word ]] && ! [[ "$f" =~ $code ]]; }; then echo "remote: not sending untracked $f (looks like a secret)" >&2; continue; fi
  printf '%s\0' "$f" >>"$tmp/untracked.list"
done < <(git ls-files -z --others --exclude-standard)
tar --null -T "$tmp/untracked.list" -cf "$tmp/untracked.tar" || die "tar of untracked files failed"
printf '%s\n' "$head" >"$tmp/HEAD"
printf '%s\n' "$branch" >"$tmp/BRANCH"
rm -f "$tmp/untracked.list"

ndirty=$(git diff --name-only HEAD | wc -l); nnew=$(tar -tf "$tmp/untracked.tar" | grep -vc '/$')
echo "remote: $host $run_id: $(git rev-parse --short HEAD) ($branch), $ndirty changed and $nnew untracked file(s)" >&2
started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; t_sent=$SECONDS; temps_at start
sent=1
tar -C "$tmp" -cf - repo.bundle changes.patch untracked.tar HEAD BRANCH |
  ssh_vm "set -e; $(hold_cmd 20); root=$rroot; find \"\$root/runs\" -mindepth 1 -maxdepth 1 -mmin +1440 -exec rm -rf {} + 2>/dev/null || true
    mkdir -p $rrun/in $rrun/art; tar -xf - -C $rrun/in" ||
  { echo "remote: sending the worktree to $host failed" >&2; exit 255; }

# ---- the command line on the VM
rem_art="$rrun/art"
line=""
for a in "${cmd[@]}"; do line="$line $(q "$a")"; done
if [ -n "$rem_art_arg" ]; then
  while [ ${#args[@]} -lt "$rem_art_arg" ]; do args+=(""); done
  args[$rem_art_arg]=__ART__
fi
for a in "${args[@]}"; do
  if [ "$a" = __ART__ ]; then line="$line \"\$art\""; else line="$line $(q "$a")"; fi
done
envs=""
for v in SCHEME EXTRA PARALLEL JOBS JOBS_FORCE GOTEST_P CAP_MEM BASE PORT REPEAT_PORT; do
  [ -n "${!v+x}" ] && envs="$envs $v=$(q "${!v}")"
done
[ -n "$env_art" ] && envs="$envs $env_art=\"\$art\""
[ -n "$envs" ] && envs="export$envs"

# The run's record (see the top): what is known here, as JSON members; the VM adds the slot, start and expiry.
# jstr: a JSON string (backslash and quote escaped, control characters dropped). The agent is whoever runs this, from
# `comebye whoami --json`: empty when that names none, fails, or takes over 5 s.
jstr() { local s="${1//\\/\\\\}"; s="${s//\"/\\\"}"; printf '"%s"' "$(printf '%s' "$s" | tr -d '\000-\037')"; }
agent=""
if command -v comebye >/dev/null; then
  agent="$(timeout 5 comebye whoami --json </dev/null 2>/dev/null 9>&- | sed -n 's/^[[:space:]]*"agent":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 | tr -cd 'A-Za-z0-9/:._-' | cut -c1-200)"
fi
rec_fixed="\"id\":$(jstr "$run_id"),\"suite\":$(jstr "$kind"),\"branch\":$(jstr "$branch"),\"head\":$(jstr "$head"),\"agent\":$(jstr "$agent"),\"from\":$(jstr "$me")"

# The script the VM runs. Its stdin is held open by this side: when ssh goes (Ctrl-C, a dropped link), the VM sees
# EOF and stops the suite's whole process group, so nothing keeps running there.
remote_script='set -u
root='"$rroot"'; run='"$rrun"'; art='"$rem_art"'
cache="$root/cache"; mkdir -p "$cache/go-build" "$cache/go-mod" "$cache/npm" "$cache/node_modules"
export COMEBYE_ON_VM=1 # the wrappers run here, never back to a VM (tools/ci/prefer-vm.sh)
export GOCACHE="$cache/go-build" GOMODCACHE="$cache/go-mod" npm_config_cache="$cache/npm" GOTOOLCHAIN=local
export PATH="/usr/local/bin:$HOME/.local/bin:/usr/bin:/bin:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
unset GIT_DIR GIT_WORK_TREE
src="$run/src"
git init -q "$src" && cd "$src" || exit 2
git -c advice.detachedHead=false fetch -q "$run/in/repo.bundle" HEAD || { echo "remote(vm): the bundle did not unpack" >&2; exit 2; }
git checkout -q -B "$(cat "$run/in/BRANCH")" FETCH_HEAD || exit 2
git fetch -q "$run/in/repo.bundle" "+refs/remotes/origin/main:refs/remotes/origin/main" 2>/dev/null || true
[ "$(git rev-parse HEAD)" = "$(cat "$run/in/HEAD")" ] || { echo "remote(vm): HEAD did not arrive" >&2; exit 2; }
[ -s "$run/in/changes.patch" ] && { git apply --binary --whitespace=nowarn "$run/in/changes.patch" || { echo "remote(vm): the changes did not apply" >&2; exit 2; }; }
tar -xf "$run/in/untracked.tar" -C "$src" || exit 2
lockh="$(sha256sum tools/uitest/package-lock.json 2>/dev/null | cut -c1-16)"
nm="$cache/node_modules/$lockh"
if [ -n "$lockh" ] && [ -d "$nm" ] && [ ! -e tools/uitest/node_modules ]; then
  cp -al "$nm" tools/uitest/node_modules 2>/dev/null || cp -a "$nm" tools/uitest/node_modules
fi
echo "remote(vm): $(hostname), $(nproc) CPUs, in $src" >&2
# The run'"'"'s record (see the top of remote.sh), once it has a slot: $run/slot and $run/started say which and since
# when. Written whole (a .tmp, then a rename), so a reader never sees half of one.
rec='"$rrec"'; fixed='"$(q "$rec_fixed")"'; dd='"$rdone"'
'"$done_fns"'
rec_write() { local s st; read -r s <"$run/slot" 2>/dev/null && read -r st <"$run/started" 2>/dev/null || return 0
  mkdir -p "${rec%/*}" && printf "{\"v\":1,%s,\"slot\":%s,\"slots\":%s,\"started\":\"%s\",\"expires\":\"%s\"}\n" "$fixed" "${s%/*}" "${s#*/}" "$st" \
    "$(date -u -d "@$(( $(date +%s) + 1200 ))" +%Y-%m-%dT%H:%M:%SZ)" >"$rec.tmp" && mv -f "$rec.tmp" "$rec"; }
# The idle hold and the record, renewed while this run waits and its suite runs, and only while this script is alive
# (both lapse on their own 20 min after a lost link). Neither helper keeps the lock below.
( while sleep '"$renew_secs"' && kill -0 $$ 2>/dev/null; do '"$(hold_cmd 20)"'; rec_write; done ) </dev/null >/dev/null 2>&1 7>&- 8>&- &
renew=$!
trap '"'"'kill "$renew" 2>/dev/null; rm -f "$rec" "$rec.tmp"'"'"' EXIT
# A slot on this VM (see the top of remote.sh). The suite inherits the slot lock (fd 7) and run.lock, shared
# (fd 8), so both are free once the suite and this script are gone, however they end. While every slot is taken,
# try again every 2 s, and give up when this side goes (EOF on stdin).
'"$slot_fns"'
'"$envs"'
n="$(nslots)"; slot=""
exec 8>>"$root/run.lock"
take() { local i
  for ((i = 0; i < n; i++)); do
    exec 7>>"$root/slot-$i.lock"
    if flock -n 7; then flock -s -n 8 && { slot=$i; return 0; }; exec 7>&-; return 1; fi
    exec 7>&-
  done
  return 1; }
if ! take; then
  w0=$SECONDS
  if [ "$n" = 1 ] || old_holds; then
    echo "remote(vm): $(hostname) is busy with $(holders "$n"); waiting for it (Ctrl-C gives up)" >&2
  else
    echo "remote(vm): $(hostname) has all $n slots busy ($(holders "$n")); waiting for one (Ctrl-C gives up)" >&2
  fi
  until take; do
    read -t 2 -r _; [ $? -gt 128 ] || { echo "remote(vm): gave up waiting" >&2; exit 130; }
  done
  echo "remote(vm): $(hostname) is free after $((SECONDS - w0)) s; starting" >&2
fi
who="$(printf "%s since %s" '"$(q "$run_id ($kind, $branch, from $me)")"' "$(date -u +%H:%M:%SZ)")"
echo "$who" >"$root/slot-$slot.who"; echo "$who" >"$root/run.who" # run.who: what an old client says it waits for
echo "$((slot + 1))/$n" >"$run/slot" # for the temperature log, and the record
t0="$(date +%s)"; echo "$t0" >"$run/t0"; date -u -d "@$t0" +%Y-%m-%dT%H:%M:%SZ >"$run/started"; rec_write
if [ "$n" -gt 1 ]; then # this slot'"'"'s share of the VM: a lower value sent from here is kept, a higher one is not
  lower() { local v="${!1:-}"; [[ "$v" =~ ^[0-9]+$ ]] && [ "$v" -le "$2" ] && [ "$v" -ge 1 ] || export "$1=$2"; }
  mb() { [[ "$1" =~ ^([0-9]+)([KMGT]?)$ ]] || return 1
    case "${BASH_REMATCH[2]}" in K) echo $((BASH_REMATCH[1] / 1024)) ;; M) echo "${BASH_REMATCH[1]}" ;;
      G) echo $((BASH_REMATCH[1] * 1024)) ;; T) echo $((BASH_REMATCH[1] * 1048576)) ;; *) echo $((BASH_REMATCH[1] / 1048576)) ;; esac; }
  cpus="$(nproc)"; sj=$((cpus / n)); [ "$sj" -ge 1 ] || sj=1
  smem=$(( $(awk '"'"'/^MemTotal:/ {print $2}'"'"' /proc/meminfo) / 1024 * 85 / 100 / n )); [ "$smem" -ge 1024 ] || smem=1024
  lower GOTEST_P "$sj"; [ "$sj" -le 4 ] || sj=4; lower JOBS "$sj"; [ -z "${PARALLEL:-}" ] || lower PARALLEL 4
  c="$(mb "${CAP_MEM:-}")" && [ "$c" -ge 1 ] && [ "$c" -le "$smem" ] || export CAP_MEM="${smem}M"
  export PORT=$(( ${COMEBYE_SLOT_PORT_BASE:-18741} + slot * 5000 )); export REPEAT_PORT=$PORT
  echo "remote(vm): slot $((slot + 1)) of $n: JOBS=$JOBS CAP_MEM=$CAP_MEM ports $PORT-$((PORT + 4999))" >&2
fi
# The suite'"'"'s output streams back to the caller and, through tee, into $run/out.log for its done record'"'"'s log (the
# suite'"'"'s stdout and stderr only, nothing this script says). -p: tee keeps the copy going once the link is gone.
tp=""; tee -p </dev/null >/dev/null 2>&1 && tp=-p
mkfifo "$run/o.fifo" "$run/e.fifo" || exit 2
tee -a $tp "$run/out.log" <"$run/o.fifo" 7>&- 8>&- & to=$!
tee -a $tp "$run/out.log" <"$run/e.fifo" >&2 7>&- 8>&- & te=$!
setsid tools/ci/capped.sh'"$line"' </dev/null >"$run/o.fifo" 2>"$run/e.fifo" &
pid=$!
( while IFS= read -r _; do :; done; kill -TERM -- "-$pid"; sleep 5; kill -KILL -- "-$pid" ) <&0 >/dev/null 2>&1 7>&- 8>&- & # <&0: a background job reads /dev/null otherwise
watch=$!
wait "$pid"; rc=$?
kill "$watch" "$renew" 2>/dev/null
# The copies end with the suite'"'"'s output: 10 s at most, for a leftover process of the suite that keeps it open.
( sleep 10; kill "$to" "$te" ) </dev/null >/dev/null 2>&1 7>&- 8>&- & k=$!
wait "$to" "$te" 2>/dev/null; kill "$k" 2>/dev/null
rm -f "$rec" "$rec.tmp"
done_write "$run" "$fixed" "$dd" '"$(q "$run_id")"' "$rc"
if [ -n "$lockh" ] && [ -d tools/uitest/node_modules/playwright-core ] && [ ! -d "$nm" ]; then
  cp -al tools/uitest/node_modules "$nm.$$" 2>/dev/null || cp -a tools/uitest/node_modules "$nm.$$"
  mv -T "$nm.$$" "$nm" 2>/dev/null || rm -rf "$nm.$$"
fi
exit $rc'

mkfifo "$tmp/hold"
sleep 2147483647 >"$tmp/hold" 9>&- &
hold=$!
# In the background with a wait, so Ctrl-C or a TERM reaches the trap at once (cleanup then closes the held stdin,
# and waits for ssh while the VM stops the suite).
ssh_vm "bash -c $(q "$remote_script")" <"$tmp/hold" &
spid=$!
wait "$spid"; rc=$?
spid=""
kill "$hold" 2>/dev/null; hold=""
temps_at end # alongside the artifacts' copy

# ---- artifacts back
if [ -z "$loc_art" ] && [ -n "$env_art$rem_art_arg" ]; then
  loc_art="$(mktemp -d "/tmp/$kind.XXXXXX")"
fi
if [ -n "$loc_art" ]; then
  mkdir -p "$loc_art"
  if ssh_vm "cd $rem_art && tar -cf - ." </dev/null | tar -xf - -C "$loc_art"; then
    [ -n "$(ls -A "$loc_art" 2>/dev/null)" ] && echo "remote: artifacts in $loc_art" >&2
  else
    echo "remote: could not copy the artifacts back from $host" >&2
  fi
fi
[ $rc = 255 ] && echo "remote: ssh to $host failed (or the suite exited 255)" >&2
echo "remote: $kind on $host exited $rc" >&2
exit $rc
