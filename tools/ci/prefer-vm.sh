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
# TEST_VM=NAME uses that worker instead; TEST_VM=local runs here and says nothing (the opt-out). A VM runs one suite
# at a time: a run waits its turn there, or with TEST_VM_NO_WAIT=1 runs here when the VM is busy.
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
  local host="${TEST_VM:-}" wcfg="" k v
  [ "$host" = local ] && return 0
  if [ -f "$cfg" ]; then
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
