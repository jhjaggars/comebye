# Test tiers

comebye's tests run in three tiers so that the free GitHub runners do the right amount of work at the right
time. The plan, with the measurements behind it, is the Page "Test tiers plan" in the Pages screen.

| Tier | Trigger | What runs | Target | A failure blocks |
|---|---|---|---|---|
| Pre-submit | a pull request | lint; `go test -race` of the packages the change touches and their importers; the browser smoke set (dark, ~160 checks, about 2 min); the scripts for the paths touched | 3.5 min (5 when `internal/server` changed) | merging the pull request |
| Merge | a push to main | lint; all Go in 2 shards; every browser script once (dark), as a `drive` job and a `rest` job; the visual pass in dark and light | 4 min | the next release of that commit (and the landing queue reads it) |
| Release | a `v*` tag | the gate (below), govulncheck, the 4 builds, then the publish | 2 min | nothing: it publishes |

The **day in the life** (`tools/smoke/run.sh`, about 15 seconds after the build) is one more job in the pre-submit tier
(with the browser smoke set) and in the merge tier: handoff, report, a to-do answered on a phone, landing to a
bare origin, a Page and its chat, park, resume, restart, GPG unlock and the coordinator's board, log and digest, on a
throwaway hub with stand-in agents. It is the guard for hardening: a slice that makes normal use hard turns it red.
See `tools/smoke/README.md`.

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
the merge tier runs everything, so a wrong rule is caught on the next push to main.

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

## No flaky list, no warnings

A check that fails fails the run. There is no list of checks allowed to fail, no automatic retry (it hides the
cause), and no step that only warns: a CI check either fails the build or is removed. A flaky check is fixed (wait on
the condition the next step needs) or deleted.

## Finding a new flake

To find a flaky check, run a script (or all of them) several times by hand and look at what fails only sometimes:

```sh
tools/ci/flakes.sh repeat 5 leftnav reviewcard      # these two, five runs each, 4 at a time (JOBS=n changes it)
tools/ci/flakes.sh repeat 3                         # every script run.sh knows
```

It prints `FLAKY script / check: failed k of N` for a check that failed in some runs and not all (exit 1), and `BROKEN` for one
that failed every time. Slow runners are mimicked locally with `taskset -c 0,1` plus a few busy loops on those cores. It runs by hand only, not in CI. The cause is nearly always a wait on a fixed time or on
state the app refreshes underneath the test: wait on the condition that the next step needs instead.

## Releasing

The Release workflow checks the tagged commit itself, and each of these fails the
release: `tools/ci/releasegate.sh` finds no green push CI run on the commit, so `go vet` and `go test -race` run
(with a green run they are skipped); `govulncheck` (the newest advisory database); the 4-target build. There is no
force input: a failure is fixed and the workflow re-run (by hand from the Actions tab with the tag, if the tag exists).
