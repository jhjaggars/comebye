#!/usr/bin/env bash
# May this commit be released? Prints key=value lines for $GITHUB_OUTPUT and exits 1 to block.
#
#   tools/ci/releasegate.sh SHA        needs GH_TOKEN and GITHUB_REPOSITORY; FORCE=true overrides the nightly
#
# A release needs a green CI run on the same commit (the merge tier ran everything there), or else the
# release workflow tests it itself (tested=false). It also needs the last nightly to be green and under 36 hours
# old: the nightly runs what a push does not (both themes, shuffled Go, the flaky list, the cross-build).
# A red nightly whose failing checks are all on the active flaky list is green: the nightly judges that itself.
# FORCE=true (the release workflow's force input, for a hotfix) lets a release through and says so.
set -euo pipefail
sha="${1:?sha}"
repo="${GITHUB_REPOSITORY:?}"

ci="$(gh run list --repo "$repo" --workflow ci.yml --commit "$sha" --event push --json conclusion --jq '[.[] | select(.conclusion == "success")] | length')"
if [ "$ci" -gt 0 ]; then echo "tested=true"; echo "CI is green on $sha: not testing again" >&2
else echo "tested=false"; echo "no green CI run on $sha: the release workflow tests it itself" >&2; fi

n="$(gh run list --repo "$repo" --workflow nightly.yml --status completed --limit 1 --json conclusion,createdAt --jq '.[0] | if . == null then "none" else "\(.conclusion) \(.createdAt)" end')"
case "$n" in
  none) echo "no nightly has run yet: not gating on it" >&2; exit 0 ;;
esac
concl="${n%% *}"; at="${n#* }"
age=$(( ( $(date -u +%s) - $(date -u -d "$at" +%s) ) / 3600 ))
if [ "$concl" = success ] && [ "$age" -lt 36 ]; then echo "nightly: green, ${age} h old" >&2; exit 0; fi
msg="the last nightly was $concl, ${age} h old (a release needs a green one under 36 h old; see the Nightly workflow and the nightly-red issue)"
if [ "${FORCE:-}" = true ]; then echo "::warning::forced past the nightly gate: $msg" >&2; echo "forced=true"; exit 0; fi
echo "::error::$msg. To release anyway, run the Release workflow by hand with force set." >&2
exit 1
