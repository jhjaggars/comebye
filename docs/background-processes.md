# Background processes

What the `comebye` server does when nobody is asking it anything: every loop it
starts, where it runs, how often it runs, what it does, and how to turn it off.

`background_doc_test.go` keeps this page current. It fails when `main.go` starts a
goroutine, or code under `internal/` makes a ticker, that this page does not name in
backticks, and when a row below names a loop the code no longer has. If you add a
loop, add a row. The line numbers are where each function was when this page was
last edited. The test checks the name and the file, not the line.

**Terms.** *Hub* here means the instance that keeps the handoff ledger: a hub, or a
single instance on its own (`-ledger`, on by default, with a state directory and no
`-hub`). A *peer* runs with `-hub` and keeps no ledger, so the hub-only loops return
at once there. *Every host* means hub and peers. Defaults are the code's defaults; a
deployment's flags or environment (`COMEBYE_*`) can change them.

## The processes

`main.go:544` to `main.go:562` start the server's loops, in this order, after the
fleet and the herdr state manager (`main.go:372`).

| Process | Code | Runs on | Trigger and default | What it does | Acts on its own? | Switch |
|---|---|---|---|---|---|---|
| Host lock heartbeat | `Lock.beat`<br>`internal/hublock/hublock.go:239` | Every host | Every 5 s, from start (`hublock.Acquire`, `main.go:228`) | Refreshes the lock record in the state directory, so a second server on the same directory knows this one is alive (a record stale for 20 s may be taken over). If a refresh fails, only `/readyz` turns unready; the server keeps running. | No: it writes only its own lock file. | None. |
| herdr state manager | `Manager.Run`<br>`internal/state/state.go:97` | Every host with herdr | herdr's event stream, plus a full refresh every 30 s as a safety net | Keeps this host's agent list current and publishes changes to the fleet. | No. | `-hub-only` (no herdr, so not started). |
| Fleet | `Fleet.Run`<br>`internal/fleet/fleet.go:282` | Every host | Local updates and each peer's stream, held open and reconnected | Merges this host's agents with its peers' and broadcasts them to browsers. | No. | None; with no peers it only relays local updates. |
| Peer health probe | `Fleet.probeLoop`<br>`internal/fleet/health.go:184` | Every host with peers | At start, then every 3 min | Probes each peer, so a peer that is down shows a specific error. | No. | None. |
| Announce to the hub | `announceToHub`<br>`announce.go:18` | A peer | Once at start; retries with backoff up to 5 min until it succeeds | Registers this node with its hub as a peer. | Yes, once: it adds itself to the hub's peers. | `-announce` (off by default). |
| Ledger follower | `Server.RunLedger`<br>`internal/server/handoff.go:662` | Hub | Fleet updates; and every 30 s | Updates the ledger as workers finish. On the 30 s tick it marks landed workers *ready to remove* (landed, CI green, remote branch gone, worktree clean; each entry looked at every 3 min for 6 h after landing) and asks each one whether it is safe to remove. Then it removes a marked worker whose checks are all clean and who answered SAFE (its pane, worktree and branch). | Yes: it prompts workers, marks them, and removes them without a tap. | `-auto-remove=false` leaves every removal to the user; the marking still runs. |
| Scheduled sweep | `Server.RunSweepLoop`<br>`internal/server/sweeploop.go:251` | Hub | Checks the clock every minute; sweeps hourly; looks at marked sessions every 20 min | Marks finished sessions, and sessions idle for 2 h, and asks each one whether it is safe to remove. With clean-up on, after a 20 min grace it removes those with nothing to lose (3 at a time) and deletes their provably stale remote branches. Off, it still marks and asks. | Yes: it prompts sessions; with clean-up on (the default), it removes sessions and deletes branches. | Settings, `sweep_loop`: `enabled` (on when absent), `every_minutes` 15 to 1440 (`PUT /api/settings`, see `docs/api.md`). |
| Token usage | `Server.RunUsage`<br>`internal/server/usage.go:73` | Every host with a state directory | At start, again after 10 s, then every 2 min | Counts tokens from this host's Claude Code and Pi session files (counts and labels only, never text); the hub also gathers its peers' counts. | No: it writes `usage.json`. | `-usage=false`; `-usage-interval`. |
| OpenCode mirror | `Server.RunOpencodeMirror`<br>`internal/server/opencode.go:75` | Every host | Every minute | Mirrors each live OpenCode agent's session, so the archive and usage see it. Does nothing without `opencode` on the PATH and an OpenCode agent. | No. | None. |
| Automatic to-dos | `Server.RunAutoTodo`<br>`internal/server/autotodo.go:163` | Hub | Fleet updates; and every 30 s | Files a to-do for an agent blocked at a dialog for 10 min with no device viewing it, and suggests to-dos for an agent idle on a question for 3 min. | Yes: it files to-do items. | `-auto-todo` (`blocked,question`; `off` for none); `-auto-todo-blocked-after`, `-auto-todo-question-after`. |
| Release item sweep | `Server.RunReleaseSweep`<br>`internal/server/releases.go:323` | Hub | Every 30 s | Closes finished release to-do items, so a rollout that ends with nobody watching still closes its item. | Yes: it closes items. | None. |
| Rolling rollout | `Server.RunRollout`, `rolloutEngine.Run`<br>`internal/server/rollout_ops.go:82` | Hub | Every 5 s; resumes a rollout after a restart | Moves a rollout that someone started from host to host: it upgrades each host, runs the deploy command for the hub, and starts stopped worker VMs so they can upgrade (within the wake limits). The first failure stops it. | Yes, once a person has started a rollout; it never starts one. Waking VMs costs money. | None for the loop; a rollout is paused or cancelled from Peers. |
| Provisioner and its to-dos | `Server.RunProvision`<br>`internal/server/provhosts.go:216` | Every host; the work depends on the role | Every 5 min; at start it resumes a wake in progress | On a provisioner: reads the cloud's view of its hosts, warns at 80% of the spend cap and stops a host at 100%, and destroys a host stopped for longer than it was asked to wait. On the hub: raises to-dos from what the provisioners recorded. | Yes, on a provisioner: it stops and destroys hosts (each audited). | `-provision-config` (none: not a provisioner, so the hub side only). |
| Blocked-dialog relay | `Server.RunBlockedRelay`<br>`internal/server/blockedrelay.go:159` | Hub | Fleet updates; and every 3 s | Logs an agent with a coordinator blocked at a dialog for 15 s (`comebye log`, kind `blocked`), and messages the coordinator if it stays blocked for 30 min with no to-do for it. | Yes: it messages the coordinator. | `-relay-blocked-after` (15 s; 0 off). |
| Quiet worker | `Server.RunQuiet`<br>`internal/server/quiet.go:91` | Hub | Every minute | Marks a working handoff worker quiet when it has written nothing to its session file for 30 min (not while a background task runs), files one to-do, and clears both when it writes again. | Yes: it files and closes to-dos. | `-quiet-after` (30 min; 0 off). |
| Loop nudge | `Server.RunLoopNudge`<br>`internal/server/loopnudge.go:129` | Hub | Every 10 s | A worker handed off with `--loop` that sits idle with nothing running is nudged after 10 min and again 20 min later; 10 min after that, the requester is told the loop is dead. | Yes: it prompts the worker and messages the requester. | `-loop-nudge-after` (10 min; 0 off), `-loop-nudge-again` (20 min). |
| Park suggestions and auto-park | `Server.RunParkSuggest`<br>`internal/server/parksuggest.go:146` | Every host; only the hub parks | Every minute | Marks an agent idle for 24 h as *can park*. In `auto` mode the hub parks one whose worktree is clean and that has nothing open (question, queued prompt, background task, loop, wakeup): it closes the pane and keeps the worktree. At most 2 per step and 10 an hour. | Yes, in `auto` mode (the default): it closes panes. | `-park-mode` (`auto`, `suggest`, `off`); `-park-suggest-after` (24 h, at least 1 h; 0 off). |
| Park retention | `Server.RunParkRetention`<br>`internal/server/parkretain.go:96` | Hub | Every hour | Proposes removing a parked agent kept for 14 days, unless it is pinned. Removal waits for the user's confirmation. | No: it only proposes. | `-park-retention` (14 days; 0 keeps parked agents for ever). |
| Re-ask stale UNSAFE answers | `Server.RunReask`<br>`internal/server/markask.go:452` | Hub | Every 2 min | Asks a marked session again when its UNSAFE answer was overtaken by an event, or is an hour old with something still unresolved. Rate-limited per mark; a working session waits until it is idle. | Yes: it prompts the session. | None. |
| Repository health | `Server.RunRepoHealth`, `Health.Run`<br>`internal/server/gitops.go:461` | Every host | Looks every 10 min; each repository is fsck'd once a day | Runs `git fsck --connectivity-only` on the repositories this host's agents work in, once a day between 02:00 and 06:00 local time, holding the repository's fetch lock for up to 30 min. Records damage for the host and the hub, and keeps checking after the agents are gone. On a `-hub-only` hub it has nothing to check. | No: `fsck` reads; it records what it finds and never repairs. | None. |
| Scratch prune | `Server.RunScratchPrune`<br>`internal/server/scratch.go:149` | Every host with herdr | 2 min after start, then hourly | Removes scratchpads whose worktree is gone (untouched for 6 h), and `tmp.*` and `hl.*` directories of this user untouched for 48 h. It stops any process whose working directory is inside a directory it deletes, except shells and agent hosts. Any herdr error means nothing is pruned, so on a `-hub-only` hub it logs a skip every hour. | Yes: it deletes directories and stops processes. | `-scratch-prune-every` (1 h; 0 off); `-scratch-dir` (`/tmp`; empty off); `-tmp-max-age` (48 h; negative leaves them alone). |
| Scheduled briefings | `Server.RunBriefSchedule`<br>`internal/server/briefsched.go:271` | Hub | Every minute, from start | When a briefing schedule is set, prompts a coordinator for each slot and counts missed ones. | Yes, with a schedule: it prompts a coordinator. | The briefing schedule in the app (off until set). |
| Coordinator reset | `Server.RunReset`<br>`internal/server/reset.go:629` | Hub | Every 10 min, from start | Marks a coordinator's reset due when its context passes 150,000 tokens, then sends `/clear` into its pane when it is idle with nothing queued. | Yes: it clears the coordinator's context. | `-coordinator-reset-tokens` (150000; 0 off). |
| Shutdown signal | `Server.ShuttingDown`<br>`internal/server/health.go:71` | Every host | SIGINT or SIGTERM | Marks the server as shutting down for the health probes, releases the host lock, and exits. | Yes, on a signal. | None. |
| Health listener | `Server.HealthHandler`<br>`internal/server/health.go:128` | Hosts with `-health-addr` | Incoming requests | Answers `/healthz` and `/readyz` on a separate port. | No. | `-health-addr` (empty: not started). |
| Pages listener | `Server.ListenPages`<br>`internal/server/pages.go:567` | Hosts with `-pages-listen` | Incoming requests | Serves published pages from their own origin. | No. | `-pages-listen` (empty: not started; needs `-pages-url`). |

### Not background processes

These make a ticker too, but live only as long as a browser holds a connection open,
so they do nothing when nobody is looking:

- `Server.handleEvents` (`internal/server/server.go:1191`): the app's event stream; a heartbeat every ping interval.
- `Server.handlePanelEvents` (`internal/server/panelwatch.go:120`): a panel's event stream; a heartbeat every ping interval.
- `panelWatcher.run` (`internal/server/panelwatch.go:100`): checks a watched panel every second while at least one viewer subscribes, and stops with the last.

One-shot goroutines (a request's work, an ask, a send, a release pass) are not listed:
each does one thing and ends.

### Started by events, not a clock

These run in the background but start when something happens, so the test does not
look for them. The ones that act:

- **Wake** (`internal/server/wake.go`, hub): a prompt, launch, queued prompt, handoff or rollout aimed at a stopped worker VM asks the provisioner to start it. At most 3 per host and 10 per fleet an hour, with backoff. It resumes after a restart. It costs money.
- **Queue and typing retry** (`internal/server/promptq.go`, `typing.go`, `deliver.go`, hub): types held prompts, callbacks and to-do notices into a pane once its input box is free, retrying every 2 s while anything is held.
- **Park cache refresh** (`internal/server/parkhub.go`, hub with peers): on fleet updates, at most every 20 s, mirrors the peers' parked lists and re-points queued prompts at a resumed pane.
- **Web Push** (`internal/push/service.go`): sends notifications, and deletes subscriptions the push service answers 404 or 410 for.
- **Notify timers** (`internal/state/notify.go`, every host): the blocked delay (`-notify-blocked-delay`, 15 s) and done digest (`-notify-done-digest`, off) before a notification goes out.

### Outside the server

These are not part of the `comebye` process, so the test does not see them.

`deploy/` ships only `comebye.service` and `comebye-gpg-watch.service`, and neither is
installed for you (`docs/setup.md`). The two archive timers exist only as examples in
the docs: copy them by hand on the machines that archive.

- **Archive timer** (example in `docs/archive.md`, not in `deploy/`): `comebye archive run && comebye archive index --all-hosts --if-changed` **every 5 minutes**, under `flock -n`. `comebye-worker destroy` starts `comebye-archive.service` by name, so a destroyed worker's sessions are searchable at once.
- **Worker archive timer** (example in `docs/workers.md`, not in `deploy/`): `comebye-worker archive --all` hourly, optional. It copies sessions from running workers, and does not count as activity on the VM.
- **GPG watch** (`deploy/comebye-gpg-watch.service`, install it yourself): `comebye gpg watch` looks every 30 s. It re-locks after 8 hours, and files a "GPG locked" item while an agent on the host is active.

On a worker VM, installed by `tools/worker`:

- **Idle poweroff** (`comebye-idle.timer`, `tools/worker/scripts/bootstrap-root.sh` and `idle-check.sh`): 5 min after boot, then every 5 min. It powers the VM off after `idle_minutes` (default 120) with no working herdr agent and no interactive ssh session.
- **Maximum run time** (`comebye-maxrun.timer`, EC2 only): powers off `WK_MAX_RUN_SECONDS` after boot.
- **OpenShell sandbox idle exit** (`tools/worker/openshell/comebye-sandbox-init`): every 300 s, `comebye idle --minutes 120`. It ends the sandbox when idle (`SBX_IDLE_MINUTES`, `SBX_IDLE_CMD`).

Agents are not processes of the binary, but some run on a clock of their own. A
coordinator acts on hub notices, and a long-running worker can pace itself with
`ScheduleWakeup`. See `docs/agents.md`.

## Known risks and overlaps

These come from a review of the code (November 2026). Each line names the loops involved.

- **herdr stream resync could hang the state manager** (`Manager.Run`, herdr `Stream`): fixed in ef7339b (#87).
- **The VM idle poweroff misses work in flight** (`comebye-idle.timer`; Wake, `Server.RunRollout`): a blocked agent, an upgrade, an archive pull or a handoff typed into an idle pane all look idle. Being fixed in #88.
- **A self-paced worker looks finished between passes** (`Server.RunLedger`, `Server.RunSweepLoop`, `Server.RunLoopNudge`): one not handed off with `--loop` can be marked and, with a clean worktree, auto-removed, and nothing notices if its wakeup dies. Being fixed.
- **Two paths remove the same worker** (`Server.RunLedger` auto-remove, `Server.RunSweepLoop` clean-up): they share a busy key, but only auto-remove keeps the 3 min gap per worker. This is double work, not double harm: each re-checks before removing.
- **Several prompts in one pane** (`Server.RunSweepLoop` or `Server.RunLedger` ask, `Server.RunLoopNudge`, `Server.RunReask`): a finished loop worker can be asked "safe to remove?" and told "start it again" close together, and re-asked 30 min later.
- **A blocked agent gets a to-do and a coordinator prompt** (`Server.RunAutoTodo`, `Server.RunBlockedRelay`): the relay's check for an existing to-do skips hub-made ones, so after 30 min both the user and the coordinator are asked.
- **Quiet to-dos close when a peer drops** (`Server.RunQuiet`): `quietClose` treats an agent missing from the list as gone, without checking that its host is up. `Server.RunAutoTodo` does check.
- **Losing the host lock does not stop the server** (`Lock.beat`): only `/readyz` changes. After 20 s stale, a second instance could take the lock, and both would write the state directory.
- **fsck holds the fetch lock for up to 30 min** (`Server.RunRepoHealth`; the git checks of `Server.RunLedger` and `Server.RunSweepLoop`): those checks time out as busy and retry later.
- **The scratch prune stops processes** (`Server.RunScratchPrune`): a worktree created between the prune's read of live paths and its delete is protected only by the 6 h minimum age.
- **A rollout wakes every stopped VM** (`Server.RunRollout`, Wake): only the wake limits apply, and agents get no veto. It costs money.
- **Reset and briefings prompt the same coordinator** (`Server.RunReset`, `Server.RunBriefSchedule`): a briefing prompt still in flight does not count as queued for the reset.
- **The OpenCode mirror can append a tail twice** (`Server.RunOpencodeMirror`, the archive flush on removal): the append runs outside the lock.
- **Automatic VM clean-up needs `-provision-create`** (`Server.RunProvision`): without it, the budget stop and the auto-destroy of long-stopped hosts never run, and a stopped VM stays until removed by hand.
- **Some loops do nothing on a `-hub-only` hub** (`Server.RunScratchPrune`, `Server.RunRepoHealth`): they run there anyway, and the prune logs a skip every hour.

## Timeline

From the server's start:

- **0 s**: the host lock heartbeat (every 5 s), the fleet and its first peer probe, the herdr state manager, a first usage scan, a first look by the briefing schedule and the coordinator reset; the provisioner resumes a wake, and the rollout engine a rollout.
- **10 s**: a second usage scan, now that the hub has reached its peers.
- **2 min**: the first scratch prune.
- **Then**, every 3 s the blocked relay; 5 s the rollout; 10 s the loop nudge; 30 s the ledger, auto-to-dos, release items and a herdr refresh; 1 min the sweep clock, quiet, park suggestions, the OpenCode mirror and briefings; 2 min usage and re-asks; 3 min peer probes; 5 min the provisioner; 10 min coordinator reset and repository health; 1 h the sweep, scratch prune and park retention; 1 day each repository's fsck.

For one idle or stuck agent, with the defaults:

- **15 s** blocked at a dialog: logged (`comebye log`).
- **3 min** idle on a question: to-dos suggested.
- **10 min** blocked, unwatched: a to-do. **10 min** idle on a `--loop` handoff: the first nudge.
- **30 min** working but silent: marked quiet, with a to-do. **30 min** blocked: its coordinator is messaged. **30 min** on a dead loop: a second nudge.
- **40 min** on a dead loop: the requester is told.
- **24 h** idle: *can park*, and in `auto` mode parked.
- **14 days** parked: proposed for removal, which the user confirms.
