#!/usr/bin/env bash
# Run a heavy command inside a memory cap, so the kernel kills the run instead of freezing the machine.
#
#   tools/ci/capped.sh CMD [ARG...]
#
# Runs CMD in `systemd-run --user --scope -p MemoryMax=N -p MemorySwapMax=0`. N is the lower of 40% of RAM
# and 24G; CAP_MEM=<size> (systemd syntax, e.g. 8G) overrides it, CAP_MEM=off turns the cap off.
# No cap, with one line saying so: CI (CI or GITHUB_ACTIONS set: silently, CI behaves as it always did),
# or a machine with no user systemd (a container, a runner). Sets COMEBYE_CAPPED=1 for CMD, so a script that
# re-runs itself under this wrapper does not nest scopes. Also exports COMEBYE_LOCAL=1 when not in CI.
#
set -uo pipefail
[ $# -gt 0 ] || { sed -n '2,10p' "$0"; exit 2; }

export COMEBYE_CAPPED=1
if [ -n "${CI:-}${GITHUB_ACTIONS:-}" ]; then exec "$@"; fi
export COMEBYE_LOCAL=1

if [ "${CAP_MEM:-}" = off ]; then echo "capped: CAP_MEM=off, running without a memory cap" >&2; exec "$@"; fi
if ! command -v systemd-run >/dev/null 2>&1 || ! systemd-run --user --scope --quiet true >/dev/null 2>&1; then
  echo "capped: no user systemd here; running without a memory cap" >&2
  exec "$@"
fi

cap="${CAP_MEM:-}"
if [ -z "$cap" ]; then
  kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  mb=$(( kb / 1024 * 40 / 100 ))
  [ "$mb" -gt 24576 ] && mb=24576
  [ "$mb" -lt 1024 ] && mb=1024
  cap="${mb}M"
fi
echo "capped: MemoryMax=$cap, no swap (CAP_MEM=<size> to change, off to disable)" >&2
exec systemd-run --user --scope --quiet --collect -p "MemoryMax=$cap" -p MemorySwapMax=0 "$@"
