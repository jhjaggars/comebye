#!/usr/bin/env bash
# Run a heavy suite on a worker VM instead of this machine, on this worktree's exact state.
#
#   tools/ci/remote.sh HOST SUITE [ARG...]
#   tools/ci/remote.sh --check HOST     only the checks below (and a start); exit 0 when a run would go ahead
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
# on the VM). Forwarded when set here: SCHEME EXTRA PARALLEL JOBS JOBS_FORCE GOTEST_P CAP_MEM BASE PORT REPEAT_PORT.
#
# A worker powers itself off when idle. A stopped one is started first, through the provisioner (`worker start HOST`,
# saying so and how long it took); REMOTE_START=0 refuses instead. While the suite runs, an idle hold on the VM
# (remote-test-ID, 20 min, renewed every 5) keeps it from powering off: a plain ssh session is not activity to it.
# The provisioner is $COMEBYE_WORKER_TOOL (words, e.g. "comebye-worker -config FILE"), else comebye-worker if it is
# on PATH, else `go run ./tools/worker`; COMEBYE_WORKER_CONFIG=FILE adds `-config FILE`. The repository is the one
# this script lives in, whatever the current directory. The heavy wrappers call this by themselves when a test VM
# is configured (tools/ci/prefer-vm.sh).
#
# One suite at a time on a VM: a run takes a lock there (REMOTE_ROOT/run.lock) before its suite starts. When another
# run holds it, this one says whose it is and since when, and waits its turn, under its idle hold; Ctrl-C gives up.
# A killed run's lock goes with its processes.
#
# Refused before anything runs (exit 2): a HOST with no provisioner ssh config or no VM, a VM that is stopped with
# REMOTE_START=0 or would not start, a VM whose toolchain is missing or not this checkout's pins
# (`worker toolchain HOST` installs them), and a second run from the same worktree while one is going.
# 255: ssh could not reach the VM.
set -uo pipefail

usage() { sed -n '2,41p' "${BASH_SOURCE[0]}" >&2; exit 2; }
die() { echo "remote: $*" >&2; exit 2; }
check=""; nowait="${REMOTE_NO_WAIT:-}"; [ "$nowait" = 0 ] && nowait=""
while :; do case "${1:-}" in --check) check=1; shift ;; --no-wait) nowait=1; shift ;; *) break ;; esac; done
if [ -n "$check" ]; then [ $# = 1 ] || usage; else [ $# -ge 2 ] || usage; fi
host="$1"; shift
[[ "$host" =~ ^[a-z][a-z0-9-]{0,62}$ ]] || die "host '$host': want a worker name (lower-case letters, digits, '-')"

# q: one shell word, for a command line the VM's shell reads.
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

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
hold=""; sent=""
rroot='"${REMOTE_ROOT:-$HOME/.cache/comebye-remote}"'
[ -n "${REMOTE_ROOT:-}" ] && rroot="$(q "$REMOTE_ROOT")"
rrun="$rroot/runs/$(q "$run_id")"
rhold='"${WK_IDLE_HOLDS:-$HOME/.local/state/comebye/idle-holds}"'"/remote-test-$run_id"
# hold_cmd MINUTES: the VM's shell line that holds the VM up that long (idle-check.sh reads "<until> <reason>").
hold_cmd() { echo "h=$rhold; mkdir -p \"\${h%/*}\" && echo \"\$((\$(date +%s) + $1 * 60)) remote test run $run_id\" >\"\$h.tmp\" && mv -f \"\$h.tmp\" \"\$h\""; }
spid=""
cleanup() {
  [ -n "$hold" ] && kill "$hold" 2>/dev/null
  if [ -n "$spid" ]; then # stopped mid-run: give the VM 15 s to stop the suite, then drop the link
    ( sleep 15; kill "$spid" ) >/dev/null 2>&1 9>&- & local t=$!
    wait "$spid" 2>/dev/null; kill "$t" 2>/dev/null
  fi
  [ -n "$sent" ] && ssh_vm "rm -rf $rrun; rm -f $rhold" </dev/null >/dev/null 2>&1
  rm -rf "$tmp"; [ -n "$check" ] || rm -f "$gitdir/comebye-remote.run"
}
trap cleanup EXIT
trap 'exit 130' INT; trap 'exit 143' TERM HUP

# ---- the VM is up: a stopped one is started through the provisioner
if [ -n "${COMEBYE_WORKER_TOOL:-}" ]; then read -r -a wtool <<<"$COMEBYE_WORKER_TOOL"
elif command -v comebye-worker >/dev/null; then wtool=(comebye-worker)
else wtool=(go run ./tools/worker); fi
[ -n "${COMEBYE_WORKER_CONFIG:-}" ] && wtool+=(-config "$COMEBYE_WORKER_CONFIG")
# The cloud's view, no ssh: RUNNING, STOPPED, ...; "none" when the provisioner has no such VM, "" when it did not answer.
vm_state() {
  local j; j="$("${wtool[@]}" status --json "$host" 2>/dev/null 9>&-)" || return 0
  grep -q '"name"' <<<"$j" || { echo none; return 0; }
  sed -n 's/.*"state": *"\([^"]*\)".*/\1/p' <<<"$j" | head -1
}
vm="$(vm_state)"
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
  r='"$rroot"'; echo
  if [ -e "$r/run.lock" ] && ! flock -n "$r/run.lock" true 2>/dev/null; then echo "BUSY $(cat "$r/run.who" 2>/dev/null)"; fi' </dev/null)"
rc=$?
busy="$(sed -n 's/^BUSY //p' <<<"$have")"; have="$(head -1 <<<"$have")"
[ $rc = 255 ] && { echo "remote: ssh could not reach $host (the provisioner says: ${vm:-no answer}; ${wtool[*]} status $host)" >&2; exit 255; }
have="${have% }"
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
# The idle hold, renewed while this run waits and its suite runs, and only while this script is alive (it lapses on
# its own 20 min after a lost link). Neither helper keeps the lock below.
( while sleep 300 && kill -0 $$ 2>/dev/null; do '"$(hold_cmd 20)"'; done ) </dev/null >/dev/null 2>&1 8>&- &
renew=$!
trap '"'"'kill "$renew" 2>/dev/null'"'"' EXIT
# One suite at a time on this VM. The suite inherits the lock, so it is free once the suite and this script are gone,
# however they end. While another run holds it, wait in steps, and give up when this side goes (EOF on stdin).
exec 8>>"$root/run.lock"
if ! flock -n 8; then
  w0=$SECONDS
  echo "remote(vm): $(hostname) is busy with $(cat "$root/run.who" 2>/dev/null || echo "another run"); waiting for it (Ctrl-C gives up)" >&2
  until flock -w 5 8; do
    read -t 1 -r _; [ $? -gt 128 ] || { echo "remote(vm): gave up waiting" >&2; exit 130; }
  done
  echo "remote(vm): $(hostname) is free after $((SECONDS - w0)) s; starting" >&2
fi
printf "%s since %s\n" '"$(q "$run_id ($kind, $branch, from $me)")"' "$(date -u +%H:%M:%SZ)" >"$root/run.who"
setsid env'"$envs"' tools/ci/capped.sh'"$line"' </dev/null &
pid=$!
( while IFS= read -r _; do :; done; kill -TERM -- "-$pid"; sleep 5; kill -KILL -- "-$pid" ) <&0 >/dev/null 2>&1 8>&- & # <&0: a background job reads /dev/null otherwise
watch=$!
wait "$pid"; rc=$?
kill "$watch" "$renew" 2>/dev/null
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
