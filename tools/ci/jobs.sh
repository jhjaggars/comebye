#!/usr/bin/env bash
# Parallelism rules for heavy local runs. Source it; it sets COMEBYE_JOBS and COMEBYE_GOP.
#
#   Locally (CI and GITHUB_ACTIONS unset):
#     JOBS    defaults to min(4, nproc/4), at least 1. An explicit JOBS above nproc/2 is lowered to nproc/2
#             unless JOBS_FORCE=1 (the override), with one line saying so.
#     go test -p is capped at nproc/2 (GOTEST_P=<n> overrides).
#   In CI: JOBS defaults to 4 (as before, no clamp), and go test keeps its default -p.
_cores="$(nproc 2>/dev/null || echo 2)"
_half=$(( _cores / 2 )); [ "$_half" -lt 1 ] && _half=1; _q=0
if [ -n "${CI:-}${GITHUB_ACTIONS:-}" ]; then
  COMEBYE_JOBS="${JOBS:-4}"; COMEBYE_GOP=""
else
  _q=$(( _cores / 4 )); [ "$_q" -gt 4 ] && _q=4; [ "$_q" -lt 1 ] && _q=1
  COMEBYE_JOBS="${JOBS:-$_q}"
  if [ "$COMEBYE_JOBS" -gt "$_half" ] && [ "${JOBS_FORCE:-}" != 1 ]; then
    echo "jobs: JOBS=$COMEBYE_JOBS is over half of $_cores cores; using $_half (JOBS_FORCE=1 to override)" >&2
    COMEBYE_JOBS="$_half"
  fi
  COMEBYE_GOP="${GOTEST_P:-$_half}"
fi
