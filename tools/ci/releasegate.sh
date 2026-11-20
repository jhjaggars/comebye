#!/usr/bin/env bash
# Has CI already tested this commit? Prints key=value lines for $GITHUB_OUTPUT.
#
#   tools/ci/releasegate.sh SHA        needs GH_TOKEN and GITHUB_REPOSITORY
#
# tested=true when a green push CI run exists on SHA (the merge tier ran everything there); tested=false
# otherwise, and the release workflow then runs vet and the Go tests itself. Either way the release workflow
# also builds all targets and runs govulncheck on the exact commit. Nothing here blocks a release.
set -euo pipefail
sha="${1:?sha}"
repo="${GITHUB_REPOSITORY:?}"

ci="$(gh run list --repo "$repo" --workflow ci.yml --commit "$sha" --event push --json conclusion --jq '[.[] | select(.conclusion == "success")] | length')"
if [ "$ci" -gt 0 ]; then echo "tested=true"; echo "CI is green on $sha: not testing again" >&2
else echo "tested=false"; echo "no green CI run on $sha: the release workflow tests it itself" >&2; fi
