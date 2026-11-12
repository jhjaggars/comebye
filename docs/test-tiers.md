# Test tiers

comebye's tests run in three tiers so that the free GitHub runners do the right amount of work at the right
time. The plan, with the measurements behind it, is the Page "Test tiers plan" in the Pages screen.

| Tier | Trigger | What runs | Target | A failure blocks |
|---|---|---|---|---|
| Pre-submit | a pull request | lint; `go test -race` of the packages the change touches and their importers; the browser smoke set (dark, ~160 checks, about 2 min); the scripts for the paths touched | 3.5 min (5 when `internal/server` changed) | merging the pull request |
| Merge | a push to main | lint; all Go in 2 shards; every browser script once (dark), as a `drive` job and a `rest` job; the visual pass in dark and light | 4 min | the next release of that commit (and the landing queue reads it) |
| Nightly | 02:00 UTC, and by hand | everything in merge, both schemes; Go shuffled with no cache; a fresh govulncheck; the release cross-build; each flaky-list check 5 times | 7 min | the next release |
| Release | a `v*` tag | the gate (below), then the 4 builds and the publish | 2 min | nothing: it publishes |

Workers still land by fast-forward after the full suite on their own machine: `go test -race ./...` and
`tools/uitest/run.sh` in both themes. Put the result in the landing report.

## Which checks does a change need?

`tools/ci/select.sh` reads the changed paths and prints what to run. Docs-only changes run nothing; a leaf Go
package runs its tests and its importers', no browser; anything under `internal/server`, `main.go` or `web/`
runs the browser too; a path it does not know runs everything. Try it:

```sh
git diff --name-only origin/main... | tools/ci/select.sh
```

The merge tier is full except for docs-only and leaf-Go-only changes. The pre-submit tier uses the whole table
(`tools/ci/select.sh` has it, one rule per path pattern). A rule never runs fewer checks than the table says, and
the nightly runs everything, so a wrong rule is caught within a day.

## Running a suite locally

```sh
tools/ci/browser.sh smoke        # ~160 checks covering every screen, about 2 min
tools/ci/browser.sh drive light  # drive.mjs in the light scheme
tools/ci/browser.sh full         # everything, one scheme (tools/uitest/run.sh runs both)
tools/ci/gotest.sh --shard 1/2   # half of the Go work
SCRIPTS="sidepanel peers" tools/uitest/run.sh   # just these scripts
SHOTS=1 tools/uitest/run.sh      # a screenshot after every drive.mjs check (otherwise only failed ones)
```

Several suites on one machine need different bases: `PORT=24741 tools/uitest/run.sh`.

## Test mode

`comebye -test-mode` (off by default; `run.sh` and `dev.sh` set it) makes the app poll every 300 ms (the side
panel every 1 s) instead of every 3 to 20 s, and lifts the to-do list's 20-adds-a-minute limit. It changes nothing
else. The browser checks wait for conditions, not for the app's own timers.

## The flaky list

`tools/ci/flaky.tsv` lists checks that are known to fail now and then, each with an **owner**, the date added and
an **expiry 14 days later**. While an entry is active the check still runs and shows red, but a run in which only
listed checks failed passes (`tools/ci/flakes.sh judge`). The nightly runs each entry five times and prints how many
passed. An expired entry blocks again (the lint job fails): renew it with a new date and a note on what you found, or
fix the check and delete the line. No automatic retries: they hide the cause.

## A red nightly

The nightly opens one issue titled **Nightly is red**, label **`nightly-red`**, comments on it for each further red
night, and closes it when a night is green. To find out from a script:

```sh
gh issue list --label nightly-red --state open
gh run list --workflow nightly.yml --limit 1 --json conclusion,databaseId,createdAt
```

## Releasing

`tools/ci/releasegate.sh` allows a release when CI is green on the tagged commit (otherwise the release workflow
tests it itself) and the last nightly is green and under 36 hours old. To release past a red or stale nightly, run
the Release workflow by hand from the Actions tab with the tag and **force** set; the override is logged on the run.
