# Test tiers

comebye's tests run in three tiers so that the free GitHub runners do the right amount of work at the right
time. The plan, with the measurements behind it, is the Page "Test tiers plan" in the Pages screen.

| Tier | Trigger | What runs | Target | A failure blocks |
|---|---|---|---|---|
| Pre-submit | a pull request | lint; `go test -race` of the packages the change touches and their importers; the browser smoke set (dark, ~160 checks, about 2 min); the scripts for the paths touched (a change that needs every script, such as one to CI or the driver, runs the merge tier's parallel browser jobs instead) | 3.5 min (5 when `internal/server` changed) | merging the pull request |
| Merge | a push to main | lint; all Go in 2 shards; every browser script once (dark), as a `drive` job and the `rest` scripts in 4 parallel parts; the visual pass in dark and light | 4 min | the next release of that commit (and the landing queue reads it) |
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
tools/ci/browser.sh changed      # what a pull request would run for your changes (committed or not) against origin/main
tools/ci/browser.sh smoke        # ~160 checks covering every screen, about 2 min
tools/ci/browser.sh drive light  # drive.mjs in the light scheme
tools/ci/browser.sh rest dark "" 2/4  # part 2 of 4 of the rest scripts (what one CI job runs)
tools/ci/browser.sh full         # everything, one scheme (tools/uitest/run.sh runs both)
tools/ci/gotest.sh --shard 1/2   # half of the Go work
SCRIPTS="sidepanel peers" tools/uitest/run.sh   # just these scripts
SHOTS=1 tools/uitest/run.sh      # a screenshot after every drive.mjs check (otherwise only failed ones)
UITEST_PROFILE=/tmp/p.txt tools/ci/browser.sh rest  # also log each Chrome start, for profiling
```

Several suites on one machine need different bases: `PORT=24741 tools/uitest/run.sh`.

`PARALLEL=2 tools/ci/browser.sh rest` runs the rest parts at once (each on its own servers and ports, inside the one
memory cap): the wall time drops to about the longest part's. It is serial unless asked, because it loads the machine
more; keep it to 2 to 4 and never beside another heavy suite.

## Keeping the browser suites fast

Where the time went, and the rules that keep it from coming back:

- **A wait that times out must fail the check.** `waitForFunction(…, { timeout: 40000 }).catch(() => {})` or an
  optional `click().catch(() => {})` on something that is not there spends its whole timeout on every run, silently
  (wakepolish spent 86 s of its 122 s that way; reviewcard 30 s; tododeeplink 32 s). Check for the element first
  (`if (await loc.count())`), or let the wait throw.
- **One Chrome per script.** `launch()` in `tools/uitest/lib.mjs` starts Chrome once per process and gives each call a
  fresh context (its own cookies, storage and permissions); `browser.close()` closes that context.
- **Slow down only what is under test.** pagetheme throttles the network after the app has loaded, and its heavy Page
  is as heavy as the check needs (its load comes seconds after the first paint), not more.
- **`-test-mode` shortens waits that only exist for real fleets:** the app polls every 300 ms, and a peer that is down
  is retried every 2 s at most (not 30 s), so a worker that starts is seen at once.
- `UITEST_PROFILE=FILE` logs every Chrome start; the `== script NAME: N s` lines give each script's time.

## Sharding the browser job

`tools/ci/browser.sh rest dark OUT I/N` runs part I of N. The parts are balanced by `tools/ci/browser-times.txt`
(each script's seconds; an unlisted script counts 30), so a new script lands in some part with no list to edit.
Every script prints `== script NAME: N s` when the next one starts: when the parts drift apart, copy those numbers
from a CI log into the file. `ci-status` is still the one required check; it waits for all the parts.

## Heavy local runs are capped

`tools/ci/gotest.sh`, `tools/ci/browser.sh`, `tools/uitest/run.sh` and `tools/ci/flakes.sh repeat` re-run themselves inside `tools/ci/capped.sh`: `systemd-run --user --scope -p MemoryMax=N -p MemorySwapMax=0`, so a runaway run is killed by the kernel instead of freezing the machine. N is the lower of 40% of RAM and 24G; `CAP_MEM=8G` sets it, `CAP_MEM=off` disables it. Without user systemd (a container, a runner) the command runs uncapped and one line says so. On CI (`CI` or `GITHUB_ACTIONS` set) nothing changes.

Locally, parallelism is bounded too (`tools/ci/jobs.sh`): `JOBS` for `flakes.sh repeat` defaults to min(4, cores/4); a larger `JOBS` is lowered to cores/2 unless `JOBS_FORCE=1`; `go test -p` is capped at cores/2 (`GOTEST_P=n`). **Do not run two heavy suites at once** (two `repeat` runs, or `repeat` beside a `-race` loop): each is capped alone, but two together can still starve the machine. `tools/ci/capped_test.sh` checks the wrapper (an allocator over the cap is killed, the fallback path, the JOBS rules).

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
