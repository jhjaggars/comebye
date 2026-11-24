# Sourced by the heavy wrappers (browser.sh, gotest.sh, flakes.sh, prelanding.sh): run the suite on the test VM when
# there is one, and on this machine when there is not.
#
#   . tools/ci/prefer-vm.sh; prefer_vm SUITE [ARG...]
#
# SUITE is remote.sh's: browser, gotest, flakes or prelanding. When this machine names a test VM and the VM answers,
# prefer_vm replaces the caller with `tools/ci/remote.sh HOST SUITE ARG...` (it never returns). Otherwise it returns,
# after one line on stderr saying why, and the caller runs here as it always did.
#
# The test VM is named by a small config, never by the repository (it is public):
#   ${TEST_VM_CONFIG:-${XDG_CONFIG_HOME:-~/.config}/comebye-worker/test-vm}
#     host=kv1                                           # a worker the provisioner made (tools/worker)
#     worker_config=~/.config/comebye-worker/kubevirt.conf  # optional: the provisioner's -config for it
# TEST_VM=NAME uses that worker instead; TEST_VM=local runs here and says nothing (the opt-out). A VM runs as many
# suites at once as it has slots (tools/ci/remote.sh): a run waits for one there, or with TEST_VM_NO_WAIT=1 runs here
# when the VM is busy.
#
# A pool of test VMs, when this machine has one, comes before test-vm:
#   ${TEST_POOL_CONFIG:-${XDG_CONFIG_HOME:-~/.config}/comebye-worker/test-pool}
#     # tried in this order; one line per VM: vm NAME [PROVISIONER_CONFIG] [remote_root=DIR] [nvme_max_c=C] [prometheus=URL]
#     vm kv-a ~/.config/comebye-worker/kv-a.conf remote_root=/data/comebye-remote
#     vm kv1  ~/.config/comebye-worker/kubevirt.conf nvme_max_c=65
#     prometheus=https://prometheus.example   # the heat check's, for every VM without its own
#     nvme_max_c=68                           # the default for every VM without its own (68 when not given)
# Other KEY=VALUE words are ignored. Heat check: with a Prometheus, one query per pool (tools/ci/temps.sh, alongside the
# state calls below, 2 s at most) reads every cluster node's NVMe temperature, and a VM whose node (its `node` in
# `worker status --json`) is at or over its nvme_max_c is skipped, for a free slot and for a start alike, with one
# line saying so. When Prometheus does not answer, or has no reading for a VM's node, nothing is skipped.
# With two or more VMs, a run asks each one's
# state from its provisioner (`remote.sh --state`, no ssh), all at once, then probes the running ones (`remote.sh
# --probe`, one ssh each) and goes to the one with the most free slots; a tie goes to the one listed first. When every
# running VM is full, the first stopped one in the list is started (`remote.sh --check`) and the run goes there. When
# none can start either, the run waits: it probes the running VMs every TEST_POOL_POLL seconds (10) and takes the first
# free slot anywhere in the pool; Ctrl-C gives up, and TEST_VM_NO_WAIT=1 runs here instead. A VM that is down,
# unreachable or without this checkout's pins is passed over; with none left, the run stays here. Two runs can pick
# the same last slot: the slower one waits for a slot on that VM. A pool of one VM is test-vm with its config.
# TEST_VM=NAME takes that VM, with its pool line when it has one.
#
# Here regardless, silently: in CI (CI or GITHUB_ACTIONS), on the VM itself (COMEBYE_ON_VM, set by remote.sh), and
# once a wrapper has decided (COMEBYE_VM_DECIDED, so a wrapper that re-runs itself under capped.sh asks once).
# Quick runs stay here too: gotest.sh with named packages (only ./..., a --shard or no argument goes), and flakes.sh
# other than `repeat`. "Answers" means `remote.sh --check HOST`: the provisioner knows it (a stopped one is started),
# ssh reaches it, and it has this checkout's pinned toolchain.

prefer_vm() {
  local suite="$1"; shift
  [ -n "${CI:-}${GITHUB_ACTIONS:-}${COMEBYE_ON_VM:-}${COMEBYE_VM_DECIDED:-}" ] && return 0
  export COMEBYE_VM_DECIDED=1
  case "$suite" in
    gotest)
      local whole=1 a
      for a in "$@"; do case "$a" in ./...|--shard) whole=1; break ;; -*) ;; *) whole="" ;; esac; done
      [ -n "$whole" ] || return 0 ;;
    flakes) [ "${1:-}" = repeat ] || return 0 ;;
  esac
  local cfg="${TEST_VM_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/comebye-worker/test-vm}"
  local pool="${TEST_POOL_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/comebye-worker/test-pool}"
  local host="${TEST_VM:-}" wcfg="" root="" k v i
  [ "$host" = local ] && return 0
  # The pool: _pvm_names, _pvm_confs, _pvm_roots, _pvm_proms and _pvm_maxes, in order; the pool-wide lines in
  # _pvm_prom and _pvm_max.
  _pvm_names=(); _pvm_confs=(); _pvm_roots=(); _pvm_proms=(); _pvm_maxes=(); _pvm_prom=""; _pvm_max=68
  if [ -f "$pool" ]; then
    local w words
    while read -r -a words; do
      case "${words[0]:-}" in
        prometheus=*) _pvm_prom="${words[0]#prometheus=}"; continue ;;
        nvme_max_c=*) [[ "${words[0]#nvme_max_c=}" =~ ^[0-9]+$ ]] && _pvm_max="${words[0]#nvme_max_c=}"; continue ;;
      esac
      [ "${words[0]:-}" = vm ] && [[ "${words[1]:-}" =~ ^[a-z][a-z0-9-]{0,62}$ ]] || continue
      _pvm_names+=("${words[1]}"); _pvm_confs+=(""); _pvm_roots+=(""); _pvm_proms+=(""); _pvm_maxes+=("")
      for w in "${words[@]:2}"; do
        case "$w" in
          \#*) break ;;
          remote_root=*) _pvm_roots[-1]="${w#remote_root=}" ;;
          prometheus=*) _pvm_proms[-1]="${w#prometheus=}" ;;
          nvme_max_c=*) [[ "${w#nvme_max_c=}" =~ ^[0-9]+$ ]] && _pvm_maxes[-1]="${w#nvme_max_c=}" ;;
          *=*) ;;
          *) _pvm_confs[-1]="${w/#\~/$HOME}" ;;
        esac
      done
    done < "$pool"
  fi
  if [ ${#_pvm_names[@]} -gt 0 ]; then
    cfg="$pool"
    if [ -n "$host" ]; then # TEST_VM=NAME: its pool line, when it has one
      for i in "${!_pvm_names[@]}"; do [ "${_pvm_names[$i]}" = "$host" ] && { wcfg="${_pvm_confs[$i]}"; root="${_pvm_roots[$i]}"; }; done
    elif [ ${#_pvm_names[@]} = 1 ]; then
      host="${_pvm_names[0]}"; wcfg="${_pvm_confs[0]}"; root="${_pvm_roots[0]}"
    else
      _pvm_pool "$suite" "$@" # returns only when the run stays here
      return 0
    fi
  elif [ -f "$cfg" ]; then
    while IFS='=' read -r k v; do
      k="${k//[[:space:]]/}"; v="${v%%#*}"; v="${v%"${v##*[![:space:]]}"}"; v="${v#"${v%%[![:space:]]*}"}"
      case "$k" in
        host) [ -n "$host" ] || host="$v" ;;
        worker_config) wcfg="${v/#\~/$HOME}" ;;
      esac
    done < "$cfg"
  fi
  if [ -z "$host" ]; then
    echo "tests: running on this machine (no test VM in $cfg; see docs/workers.md; TEST_VM=local hides this)" >&2
    return 0
  fi
  local remote; remote="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/remote.sh"
  [ -n "$wcfg" ] && [ -z "${COMEBYE_WORKER_CONFIG:-}" ] && export COMEBYE_WORKER_CONFIG="$wcfg"
  [ -n "$root" ] && export REMOTE_ROOT="$root"
  # The check's lines about a start show as they come (a start takes a while); the rest is kept for the warning.
  local why="" l rc=1
  local nw=(); [ -n "${TEST_VM_NO_WAIT:-}" ] && [ "${TEST_VM_NO_WAIT}" != 0 ] && nw=(--no-wait)
  while IFS= read -r l; do
    case "$l" in
      "rc="*) rc="${l#rc=}" ;;
      *"; starting it"*|*" started in "*|*"; this run will wait its turn") echo "$l" >&2 ;;
      "remote: "*) why="${l#remote: }" ;;
    esac
  done < <("$remote" --check "${nw[@]}" "$host" 2>&1 >/dev/null; echo "rc=$?")
  if [ "$rc" = 0 ]; then
    echo "tests: running $suite on the test VM $host (TEST_VM=local runs it here)" >&2
    exec "$remote" "$host" "$suite" "$@"
  fi
  echo "tests: running on this machine: test VM $host is not available (${why:-exit $rc})" >&2
  return 0
}

# _pvm_env I: the pool's VM I for remote.sh (its provisioner config and REMOTE_ROOT, when its line has them).
_pvm_env() {
  [ -n "${_pvm_confs[$1]}" ] && export COMEBYE_WORKER_CONFIG="${_pvm_confs[$1]}"
  [ -n "${_pvm_roots[$1]}" ] && export REMOTE_ROOT="${_pvm_roots[$1]}"
  return 0
}

# _pvm_pool SUITE ARG...: pick a VM of the pool (see the top) and exec remote.sh there; return to run here.
_pvm_pool() {
  local suite="$1"; shift
  local remote; remote="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/remote.sh"
  local d; d="$(mktemp -d "${TMPDIR:-/tmp}/test-pool.XXXXXX")" || return 0
  local i st best="" bf=0 sum="" nwait=()
  local nowait=""; [ -n "${TEST_VM_NO_WAIT:-}" ] && [ "${TEST_VM_NO_WAIT}" != 0 ] && nowait=1
  # _pvm_probe I...: each VM's free slots into $d/I.probe, all at once.
  _pvm_probe() { local j; for j in "$@"; do ( _pvm_env "$j"; "$remote" --probe "${_pvm_names[$j]}" >"$d/$j.probe" 2>/dev/null || : >"$d/$j.probe" ) & done; wait; }
  # _pvm_best I...: the one with the most free slots (the first listed on a tie) into best and bf; the line into sum.
  _pvm_best() {
    local j s f t; best=""; bf=0; sum=""
    for j in "$@"; do
      read -r s f t <"$d/$j.probe" 2>/dev/null; s="${s#slots=}"; f="${f#free=}"; t="${t#toolchain=}"
      if [ -z "$t" ]; then sum="$sum${sum:+, }${_pvm_names[$j]} unreachable"; continue; fi
      if [ "$t" != ok ]; then sum="$sum${sum:+, }${_pvm_names[$j]} without this checkout's pins"; continue; fi
      sum="$sum${sum:+, }${_pvm_names[$j]} $f/$s free"
      if [ "$f" -gt "$bf" ] 2>/dev/null; then best=$j; bf=$f; fi
    done
    return 0 # a caller under set -e (gotest.sh) must not stop here
  }
  _pvm_go() { # I WHY POOL ARG...: run there
    local j="$1" said="$2" p="$3"; shift 3
    rm -rf "$d"; _pvm_env "$j"
    echo "tests: running $suite on the test VM ${_pvm_names[$j]}: $said (pool: $p; TEST_VM=local runs it here)" >&2
    exec "$remote" "${_pvm_names[$j]}" "$suite" "$@"
  }
  # The nodes' temperatures, one query per Prometheus (one for the whole pool, usually), alongside the provisioners'
  # answers; then a probe of each running VM (or one whose provisioner did not answer: ssh says).
  local urls=() u k node nv mx
  for i in "${!_pvm_names[@]}"; do
    u="${_pvm_proms[$i]:-$_pvm_prom}"; [ -n "$u" ] || continue
    case " ${urls[*]} " in *" $u "*) ;; *) urls+=("$u") ;; esac
  done
  for k in "${!urls[@]}"; do "$(dirname "$remote")/temps.sh" "${urls[$k]}" >"$d/temps-$k" 2>/dev/null & done
  for i in "${!_pvm_names[@]}"; do ( _pvm_env "$i"; "$remote" --state "${_pvm_names[$i]}" >"$d/$i.state" 2>/dev/null || echo none >"$d/$i.state" ) & done; wait
  local up=() down=() off=""
  for i in "${!_pvm_names[@]}"; do
    read -r st node <"$d/$i.state"
    u="${_pvm_proms[$i]:-$_pvm_prom}"; nv=""; mx="${_pvm_maxes[$i]:-$_pvm_max}"
    if [ -n "$u" ] && [ -n "$node" ]; then
      for k in "${!urls[@]}"; do [ "${urls[$k]}" = "$u" ] && nv="$(awk -v n="$node" '$1 == n { sub(/^nvme=/, "", $3); print $3 }' "$d/temps-$k")"; done
    fi
    if [ -n "$nv" ] && [ "$nv" -ge "$mx" ] 2>/dev/null; then
      echo "tests: skipping test VM ${_pvm_names[$i]}: its node $node has its NVMe at ${nv}°C (nvme_max_c=$mx)" >&2
      sum="$sum${sum:+, }${_pvm_names[$i]} hot"; continue
    fi
    case "$st" in
      RUNNING|"") up+=("$i") ;;
      STOPPED|TERMINATED|SUSPENDED|Stopped|Paused) down+=("$i"); off="$off${off:+, }${_pvm_names[$i]} stopped" ;;
      none) sum="$sum${sum:+, }${_pvm_names[$i]} unknown to its provisioner" ;;
      *) sum="$sum${sum:+, }${_pvm_names[$i]} ${st}" ;;
    esac
  done
  local other="$sum"
  _pvm_probe "${up[@]}"; _pvm_best "${up[@]}"
  [ -n "$best" ] && _pvm_go "$best" "$bf of $(sed 's/^slots=\([0-9]*\).*/\1/' "$d/$best.probe") slots free" "$sum${other:+, $other}${off:+, $off}" "$@"
  # Every running VM is full: start the first stopped one that comes up with this checkout's pins.
  for i in "${down[@]}"; do
    local l rc=1 w=""
    while IFS= read -r l; do
      case "$l" in
        "rc="*) rc="${l#rc=}" ;;
        *"; starting it"*|*" started in "*) echo "$l" >&2 ;;
        "remote: "*) w="${l#remote: }" ;;
      esac
    done < <(_pvm_env "$i"; "$remote" --check "${_pvm_names[$i]}" 2>&1 >/dev/null; echo "rc=$?")
    if [ "$rc" = 0 ]; then _pvm_go "$i" "started, as every running VM was full" "${sum:-none running}${other:+, $other}" "$@"; fi
    other="$other${other:+, }${_pvm_names[$i]} would not start (${w:-exit $rc})"
  done
  # Wait for a free slot on any running VM with the pins.
  for i in "${up[@]}"; do [ "$(cut -d' ' -f3 "$d/$i.probe" 2>/dev/null)" = toolchain=ok ] && nwait+=("$i"); done
  if [ ${#nwait[@]} = 0 ]; then
    rm -rf "$d"; echo "tests: running on this machine: no test VM in the pool is available (${sum}${other:+${sum:+, }$other})" >&2; return 0
  fi
  if [ -n "$nowait" ]; then
    rm -rf "$d"; echo "tests: running on this machine: every test VM in the pool is full ($sum) and TEST_VM_NO_WAIT is set" >&2; return 0
  fi
  echo "tests: every test VM in the pool is full ($sum); waiting for a free slot anywhere in it (Ctrl-C gives up)" >&2
  local t0=$SECONDS
  while :; do
    sleep "${TEST_POOL_POLL:-10}"
    _pvm_probe "${nwait[@]}"; _pvm_best "${nwait[@]}"
    [ -n "$best" ] && _pvm_go "$best" "a slot free after $((SECONDS - t0)) s" "$sum" "$@"
  done
}
