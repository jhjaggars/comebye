#!/usr/bin/env bash
# Run a heavy suite on a worker VM instead of this machine, on this worktree's exact state.
#
#   tools/ci/remote.sh HOST SUITE [ARG...]
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
# Refused before anything runs (exit 2): a HOST with no provisioner ssh config, a VM whose toolchain is missing or
# not this checkout's pins (`go run ./tools/worker toolchain HOST` installs them), and a second run from the same
# worktree while one is going. 255: ssh could not reach the VM.
set -uo pipefail

usage() { sed -n '2,27p' "${BASH_SOURCE[0]}" >&2; exit 2; }
die() { echo "remote: $*" >&2; exit 2; }
[ $# -ge 2 ] || usage
host="$1"; shift
[[ "$host" =~ ^[a-z][a-z0-9-]{0,62}$ ]] || die "host '$host': want a worker name (lower-case letters, digits, '-')"

# q: one shell word, for a command line the VM's shell reads.
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

top="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not in a git worktree"
cd "$top" || exit 2
state="${COMEBYE_WORKER_STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/comebye-worker}"
cfg="$state/$host/ssh_config"
[ -f "$cfg" ] || die "$host is not a worker this provisioner knows (no $cfg)"
ssh_vm() { ssh -F "$cfg" -o BatchMode=yes "$host" "$@" 9>&-; }

# ---- the suite, and where its artifacts land here
kind="$1"; shift
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

# ---- one run per worktree at a time
gitdir="$(git rev-parse --absolute-git-dir)"
lock="$gitdir/comebye-remote.lock"
exec 9>"$lock" || die "cannot open $lock"
flock -n 9 || die "another remote run from this worktree is going ($(cat "$gitdir/comebye-remote.run" 2>/dev/null || echo '?')); wait for it"

run_id="$(hostname -s 2>/dev/null || echo host)-$(date +%Y%m%d-%H%M%S)-$$"
echo "$host $run_id" >"$gitdir/comebye-remote.run"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/remote.XXXXXX")"
hold=""; sent=""
rroot='"${REMOTE_ROOT:-$HOME/.cache/comebye-remote}"'
[ -n "${REMOTE_ROOT:-}" ] && rroot="$(q "$REMOTE_ROOT")"
rrun="$rroot/runs/$(q "$run_id")"
spid=""
cleanup() {
  [ -n "$hold" ] && kill "$hold" 2>/dev/null
  if [ -n "$spid" ]; then # stopped mid-run: give the VM 15 s to stop the suite, then drop the link
    ( sleep 15; kill "$spid" ) >/dev/null 2>&1 9>&- & local t=$!
    wait "$spid" 2>/dev/null; kill "$t" 2>/dev/null
  fi
  [ -n "$sent" ] && ssh_vm "rm -rf $rrun" </dev/null >/dev/null 2>&1
  rm -rf "$tmp"; rm -f "$gitdir/comebye-remote.run"
}
trap cleanup EXIT
trap 'exit 130' INT; trap 'exit 143' TERM HUP

# ---- the VM has this checkout's pinned toolchain
pins="tools/worker/testrunner.go"
[ -f "$pins" ] || die "no $pins here: is this a comebye checkout?"
pin() { sed -n "s/^[[:space:]]*$1:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$pins" | head -1; }
want="go=$(pin GoVersion) node=$(pin NodeVersion) chrome=$(pin ChromeVersion)"
have="$(ssh_vm 'f="${COMEBYE_TEST_RUNNER_FILE:-/etc/comebye-worker/test-runner}"; export PATH="/usr/local/bin:$HOME/.local/bin:/usr/bin:/bin:$PATH"
  for t in go node npm google-chrome; do command -v "$t" >/dev/null || { echo "missing $t"; exit 0; }; done
  [ -f "$f" ] && tr "\n" " " <"$f" || echo "no $f"' </dev/null)"
rc=$?
[ $rc = 255 ] && { echo "remote: ssh could not reach $host" >&2; exit 255; }
have="${have% }"
[ "$have" = "$want" ] || die "$host has no test toolchain at this checkout's pins (it says: ${have:-nothing}; want: $want).
  Install it with: go run ./tools/worker toolchain $host"

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
  ssh_vm "set -e; root=$rroot; find \"\$root/runs\" -mindepth 1 -maxdepth 1 -mmin +1440 -exec rm -rf {} + 2>/dev/null || true
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
setsid env'"$envs"' tools/ci/capped.sh'"$line"' </dev/null &
pid=$!
( while IFS= read -r _; do :; done; kill -TERM -- "-$pid"; sleep 5; kill -KILL -- "-$pid" ) <&0 >/dev/null 2>&1 & # <&0: a background job reads /dev/null otherwise
watch=$!
wait "$pid"; rc=$?
kill "$watch" 2>/dev/null
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
