#!/usr/bin/env bash
# Self-test of tools/ci/capped.sh and tools/ci/jobs.sh: tools/ci/capped_test.sh
#  1. a 600 MB allocator under CAP_MEM=200M is killed while this shell survives (needs user systemd; skipped without)
#  2. a small command under the cap runs and exits 0
#  3. the fallback (no systemd-run on PATH) runs the command uncapped and says so
#  4. CI=1 runs the command with no cap and no message
#  5. JOBS defaults and the nproc/2 clamp
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fail=0; ok() { echo "ok   $1"; }; bad() { echo "FAIL $1"; fail=1; }
alloc='python3 -c "b=bytearray(600*1024*1024); b[::4096]=b\"x\"*len(b[::4096]); print(\"ALLOCATED\")"'
env -u CI -u GITHUB_ACTIONS true
if systemd-run --user --scope --quiet true >/dev/null 2>&1; then
  out="$(env -u CI -u GITHUB_ACTIONS CAP_MEM=200M "$here/capped.sh" bash -c "$alloc" 2>&1)"; rc=$?
  { [ $rc -ne 0 ] && ! grep -q ALLOCATED <<<"$out"; } && ok "allocator killed under 200M (rc=$rc)" || bad "allocator survived (rc=$rc): $out"
  env -u CI -u GITHUB_ACTIONS CAP_MEM=200M "$here/capped.sh" true 2>/dev/null && ok "small command passes under cap" || bad "small command failed"
else echo "skip 1,2: no user systemd"; fi
# fallback: a PATH with no systemd-run
fb="$(mktemp -d)"; for t in bash env awk true cat grep sed; do ln -s "$(command -v $t)" "$fb/$t"; done
out="$(env -u CI -u GITHUB_ACTIONS PATH="$fb" "$here/capped.sh" bash -c 'echo ran' 2>&1)"; rc=$?
{ [ $rc -eq 0 ] && grep -q 'no user systemd' <<<"$out" && grep -q '^ran$' <<<"$out"; } && ok "fallback runs uncapped and says so" || bad "fallback: rc=$rc $out"
rm -rf "$fb"
out="$(CI=1 "$here/capped.sh" bash -c 'echo ran' 2>&1)"
[ "$out" = ran ] && ok "CI: no cap, no message" || bad "CI: $out"
n="$(nproc)"; half=$((n/2)); [ $half -lt 1 ] && half=1
j="$(env -u CI -u GITHUB_ACTIONS -u JOBS bash -c ". $here/jobs.sh; echo \$COMEBYE_JOBS")"
want=$((n/4)); [ $want -gt 4 ] && want=4; [ $want -lt 1 ] && want=1
[ "$j" = "$want" ] && ok "JOBS default $j" || bad "JOBS default $j want $want"
j="$(env -u CI -u GITHUB_ACTIONS JOBS=999 bash -c ". $here/jobs.sh; echo \$COMEBYE_JOBS" 2>/dev/null)"
[ "$j" = "$half" ] && ok "JOBS=999 clamped to $half" || bad "clamp $j"
j="$(env -u CI -u GITHUB_ACTIONS JOBS=999 JOBS_FORCE=1 bash -c ". $here/jobs.sh; echo \$COMEBYE_JOBS")"
[ "$j" = 999 ] && ok "JOBS_FORCE=1 honoured" || bad "force $j"
j="$(CI=1 bash -c ". $here/jobs.sh; echo \$COMEBYE_JOBS:\$COMEBYE_GOP")"
[ "$j" = "4:" ] && ok "CI: JOBS 4, default -p" || bad "CI jobs $j"
exit $fail
