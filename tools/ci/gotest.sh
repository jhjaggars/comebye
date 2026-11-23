#!/usr/bin/env bash
# go test -race -shuffle=on "${pflag[@]}", optionally one half of the work.
#
#   tools/ci/gotest.sh                       every package
#   tools/ci/gotest.sh PKG...                just these packages (go test's own patterns)
#   tools/ci/gotest.sh --shard 1/2           the odd-numbered tests of internal/server
#   tools/ci/gotest.sh --shard 2/2           the even-numbered tests of internal/server, and every other package
#
# internal/server alone takes longer than all the other packages together (about 200 s of 210 s), so the
# two shards of a full run take about half each. Tests are split by their position in `go test -list`, so a
# new test lands in a shard by itself and every test runs in exactly one.
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
[ "$of" = 2 ] || { echo "only 2 shards" >&2; exit 2; }
names="$(go test -list '.*' ./internal/server | grep -E '^Test' | sort)"
odd="$(echo "$names" | awk 'NR%2==1' | paste -sd'|')"
even="$(echo "$names" | awk 'NR%2==0' | paste -sd'|')"
if [ "$n" = 1 ]; then
  exec go test -race -shuffle=on "${pflag[@]}" -run "^($odd)\$" ./internal/server
fi
rc=0
go test -race -shuffle=on "${pflag[@]}" -run "^($even)\$" ./internal/server || rc=1
# shellcheck disable=SC2046
go test -race -shuffle=on "${pflag[@]}" $(go list ./... | grep -v '/internal/server$') || rc=1
exit $rc
