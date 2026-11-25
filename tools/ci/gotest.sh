#!/usr/bin/env bash
# go test -race -shuffle=on "${pflag[@]}", optionally one part of the work.
#
#   tools/ci/gotest.sh                       every package
#   tools/ci/gotest.sh PKG...                just these packages (go test's own patterns)
#   tools/ci/gotest.sh --shard 1/2           the odd-numbered tests of internal/server
#   tools/ci/gotest.sh --shard 2/2           the even-numbered tests of internal/server, and every other package
#   tools/ci/gotest.sh --shard 1/3, 2/3      the odd- and even-numbered tests of internal/server (what CI runs)
#   tools/ci/gotest.sh --shard 3/3           every other package
#   tools/ci/gotest.sh --shard I/N PKG...    the same parts of just these packages (a pull request's go-changed jobs):
#                                            internal/server must be one of them
#
# internal/server takes the longest by far (about 185 s of tests on a CI runner, the others about 80 s, each
# with a minute of -race compiling), so CI runs it in 2 halves and the rest beside them. Tests are split by
# their position in `go test -list`, so a new test lands in a shard by itself and every test runs in exactly one.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

# The whole set (no package named, ./..., or a shard) runs on the test VM when one is configured and answers
# (tools/ci/prefer-vm.sh; TEST_VM=local keeps it here); named packages always run here.
. tools/ci/prefer-vm.sh; prefer_vm gotest "$@"
[ -n "${COMEBYE_CAPPED:-}" ] || exec "$(dirname "${BASH_SOURCE[0]}")/capped.sh" "${BASH_SOURCE[0]}" "$@"
. "$(dirname "${BASH_SOURCE[0]}")/jobs.sh"
pflag=(); [ -n "$COMEBYE_GOP" ] && pflag=(-p "$COMEBYE_GOP")
shard=""
if [ "${1:-}" = "--shard" ]; then shard="$2"; shift 2; fi

if [ -z "$shard" ]; then
  if [ $# -gt 0 ]; then exec go test -race -shuffle=on "${pflag[@]}" "$@"; fi
  exec go test -race -shuffle=on "${pflag[@]}" ./...
fi

n="${shard%/*}"; of="${shard#*/}"
case "$of/$n" in 2/1|2/2|3/1|3/2|3/3) ;; *) echo "--shard is 1/2, 2/2, 1/3, 2/3 or 3/3" >&2; exit 2 ;; esac
# The packages besides internal/server: every one, or the ones named.
if [ $# -gt 0 ]; then
  others=(); has_server=""
  for p in "$@"; do
    case "$p" in ./internal/server|internal/server|*/comebye/internal/server) has_server=1 ;; *) others+=("$p") ;; esac
  done
  [ -n "$has_server" ] || { echo "--shard splits internal/server's tests: name ./internal/server among the packages" >&2; exit 2; }
else
  # shellcheck disable=SC2207
  others=($(go list ./... | grep -v '/internal/server$'))
fi
run_others() {
  if [ ${#others[@]} -eq 0 ]; then echo "no packages besides internal/server"; return 0; fi
  go test -race -shuffle=on "${pflag[@]}" "${others[@]}"
}
# Part 3 of 3 never builds internal/server's tests.
if [ "$of/$n" = 3/3 ]; then run_others; exit; fi
names="$(go test -list '.*' ./internal/server | grep -E '^Test' | sort)"
odd="$(echo "$names" | awk 'NR%2==1' | paste -sd'|')"
even="$(echo "$names" | awk 'NR%2==0' | paste -sd'|')"
if [ "$n" = 1 ]; then
  exec go test -race -shuffle=on "${pflag[@]}" -run "^($odd)\$" ./internal/server
fi
[ "$of" = 3 ] && exec go test -race -shuffle=on "${pflag[@]}" -run "^($even)\$" ./internal/server
rc=0
go test -race -shuffle=on "${pflag[@]}" -run "^($even)\$" ./internal/server || rc=1
run_others || rc=1
exit $rc
