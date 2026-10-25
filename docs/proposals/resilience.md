# Proposal: surviving restarts, renames and moved panes

Status: investigation and design. Nothing here is implemented except the
two small fixes in sections 3a and 4. Written after the Oct 23 reboot of
jjaggars-kubevirt (the Comebye hub's host).

What happened, in short: the VM was rebooted and the agents were stopped
cleanly, with resume notes in `~/.cache/comebye-resume/<id>.md`. herdr
restored nothing (a separate herdr bug, see
`~/.worktrees/herdr/investigate-session-restore-wipe`: pane processes that
exit normally during shutdown count as `Exited`, the last exit empties the
layout, and the next save clears `session.json`). A Pi agent rebuilt 17
sessions with `pi --session <file>` and reorganised the workspaces. Then
the old coordinator (pane `laharl/w1T:p1`) lost its role, 13 scheduled
briefings were dismissed ("no coordinator was running"), and a
`comebye prompt` with a file path sent 17 agents the literal string
`reboot-prompt.md`.

The common thread: Comebye identifies an agent by **where it sits** (pane
id) and **what herdr calls it** (name). Both are things herdr is free to
change, and both changed. The agent's own session is the one thing that
stayed put.

Limits of this investigation: I read the code of both projects and the live
agent list, but had no access to herdr's server log or the old
coordinator's pane history from before the loss, so the cause in section 1
is the mechanism the code allows, checked against what the agent list
shows now, not a replay of the incident.

## 1. Role loss

### What the code does

**Comebye** (`internal/server/coordinator.go`):

- `roles.json` is `{ "<host>/<pane>": {role, name, by, at} }`. The key is a
  pane id; `name` is the herdr agent name when the role was set.
- `liveRoles()` (what `GET /api/roles`, `GET /api/ledger` and the app use)
  drops a role when the agent in that pane is **seen and its name differs
  from the stored name**: `n != r.Name`. A name that vanished is `""`, which
  differs from `"coordinator"`, so the role is hidden. It is not deleted:
  `roles.json` still holds it, and it would reappear if the name came back.
  This is the "role dropped" symptom. Anything that asks "is a coordinator
  running?" through the roles list sees none from that moment.
- `isCoordinator()` is looser (an empty live name matches), so the two
  disagree about the same pane.
- The same pane-id key is used by the ledger: `Entry.AgentID`/`PaneID`/
  `AgentName`, `Mark.AgentID`, `Store.ByAgent`, `Store.Observe` (matches
  `herdr.AgentInfo.ID`), and `agentPresent()` in `handoff.go` (matches the
  pane id only, no name check at all). After a restart that renumbers or
  reuses pane ids, an entry can point at a different agent than the one it
  was made for, and a handoff answers `existing: true` for a stranger.

**herdr** (read-only look at `~/code/herdr`, 5608de06):

- A name lives on the terminal (`agent_name`) next to an `agent_name_owner`
  = `{agent_label, session_ref}` (`src/terminal/state.rs`).
- `reconcile_agent_name_owner` (state.rs ~2353) **clears the name** when it
  is called with an agent label different from the owner's, or with a
  session ref different from the owner's. It is called from process
  detection (state.rs ~434, with `session_ref = None`), from hook reports
  (~777, ~1231) and from session reports (~1670). So a name is not a
  property of the agent: it is a label that survives only while herdr's
  view of "which agent is in this terminal" stays the same. Triggers:
  `/clear`, `/resume` or a `--resume` that yields a new session id; the
  foreground process being detected as another agent kind (a coordinator
  that runs `pi` or another agent CLI in its own pane); and
  `agent.rename` with no name.
- On a cold restore, names come back only for panes saved as a managed
  agent (`src/persist/restore.rs` ~536-710); an unmanaged agent's name is
  dropped (there is a test, `cold_restore_with_gapped_public_tab_numbers_
  drops_unmanaged_agent_name`). This is why every Pi agent the rebuild
  started on the VM shows `-` as its name in `comebye agents` today.

For `laharl/w1T:p1` the session id never changed, so the label path
(detection flipping the agent kind with no session ref to compare) is the
candidate, not the session path. I could not confirm which trigger fired;
the herdr log of that pane around the time would say. What matters for the
design is that **it can happen, with the session untouched, and that herdr
treats it as normal**.

### Proposal: key by the agent's session, not its pane or name

`agent_session` is already in `agent.list` (`{source, agent, kind: id|path,
value}`: a Claude Code session id, a Pi session file path, ...). Comebye
keeps it only inside the hub (`herdr.AgentInfo.AgentSession`; peers are read
with `?full=1`) and strips it from the browser and CLI view
(`browserAgent` in `slim.go`).

**Identity.** The hub keeps an identity per agent, `aid` (`a` + 8 hex,
minted by the hub), with:

```json
{
  "aid": "a3f9c1d2",
  "host": "jjaggars-kubevirt",
  "kind": "pi",
  "refs": [{"agent":"pi","kind":"path","value":"/home/u/.pi/sessions/....jsonl"}],
  "weak": {"pane":"w1T:p1","name":"coordinator","cwd":"/home/u","worktree":""},
  "first_seen": "...", "last_seen": "...", "state": "live"
}
```

Roles, ledger entries and marks are keyed by `aid`. The pane id and name
become attributes read from the live agent, shown but never trusted as
identity. I propose a hub-minted `aid` that *holds* session refs rather
than using the raw session value as the key, because a session value
itself changes legitimately (Claude Code `/clear`, a resume that forks the
session) and some agents have no ref at all (not yet reported, or a harness
without hooks). With an `aid` those cases are a rebind, not an orphaning.

**Resolution** (every time the fleet's agent list changes,
`identity.Observe`, which also keeps the `Server.fleet` hot path free of
disk writes unless something changed):

1. A live agent whose ref is in some identity's `refs` is that identity.
2. Otherwise, a *weak match* against identities that are not currently
   bound to a live agent: same host and kind, and (same pane or same name)
   and same cwd/worktree. Unique match only. The agent's new ref is
   appended to the identity's `refs` (a rebind).
3. Otherwise a new identity.

An identity whose agent is gone becomes `absent`, never deleted (section 2).
Two agents claiming the same ref (a duplicate pane) are flagged and neither
rebinds silently.

A restart that is *expected* (section 2) registers an `expect{aid, host,
pane}`: the first new ref seen in that pane within two minutes binds to the
`aid`, which covers `claude --resume` creating a new session id.

**What changes for callers:**

- `GET /api/roles` returns `{aid: Role}` plus an `agents` map `aid ->
  current id` for display. Roles survive renames, pane moves, a pane being
  closed and the agent resumed elsewhere. Role changes (`POST /api/roles`)
  accept either an agent id (resolved to its `aid`) or an `aid`.
- `isCoordinator(aid)`; the empty-name special case disappears.
- `liveRoles` no longer compares names. A role whose identity is `absent`
  is reported as such (`present: false`) instead of vanishing, so a
  "coordinator running?" check can tell "no coordinator" from "its host is
  down or it is restarting".
- Ledger: `Entry` gains `aid`; `ByAgent`, `Observe`, `SetMark`, `Removed`,
  `agentPresent` look up the live agent through the `aid`. `AgentID`,
  `PaneID` and `AgentName` stay as the last known values (display, and
  fallback while an identity has no ref).
- The CLI accepts an `aid` anywhere it takes an agent (`comebye inspect
  a3f9c1d2`) and prints it in `comebye agents --json`.
- `session` (agent, kind, value) is added to the browser/CLI agent view.
  Besides identity it is what the app needs for the "no session ref"
  warning (section 2).

### Migration

Run once, at startup, when `roles.json` has no `version` or the ledger's
`version` is 1:

1. Back up `roles.json` and `ledger.json` as `*.v1.bak` (never overwrite an
   existing backup).
2. Wait for the first full agent list (all hosts, or a bounded 30 s: hosts
   still `connecting` are retried later).
3. For each role and each active ledger entry or mark, find the live agent
   with that id. If found, take or mint its `aid` and set it. The old
   name check is applied as before, except an empty live name is accepted
   when the cwd or worktree also matches (this is the case that failed).
4. Not found (host down, agent gone): mint an `absent` identity from the
   stored fields as the weak binding (host, pane, name, worktree, kind), so
   it rebinds when a matching agent shows up. Nothing is dropped.
5. Write `roles.json` as `{"version":2,"roles":{aid: Role}}` and the ledger
   with `version: 2` and the `aid` fields. For one release keep writing the
   old `AgentID`-keyed fields in the ledger so a rollback works; the loader
   accepts the old `roles.json` shape (a bare map).

Stopgap that does not need any of this (one line, worth doing first): treat
an empty live name in `liveRoles` as "unknown", like `isCoordinator` does.
It stops the role from being hidden by a vanished name while the real fix
is built.

Part of the same symptom: the scheduled briefings that were dismissed
(`internal/server/briefsched.go`). Whatever asks "is a coordinator
running?" should treat "cannot tell" (roles unavailable, role present but
the agent absent) as *skip and retry*, not *dismiss*.

Also worth telling herdr upstream: clearing a name because detection
briefly sees another agent label, with no change of session, loses
information for no gain. A draft issue is in the open questions.

## 2. Save and restore independent of herdr's session.json and of pane and workspace ids

### Data model

The hub keeps a **fleet record** per host, versioned, on its own disk
(`<state>/fleet/<host>.json`, plus `<state>/fleet/snapshots/<host>/<rev>.json`):

```json
{
  "version": 1,
  "host": "jjaggars-kubevirt",
  "rev": 42,
  "saved_at": "...",
  "epoch": {"id": "e7", "interrupted_at": null},
  "workspaces": [
    {"key": "w:repo:/home/u/code/openshell", "label": "OpenShell",
     "cwd": "/home/u/code/openshell", "parent": null,
     "children": [
       {"key": "w:wt:/home/u/.worktrees/openshell/feat-x", "label": "openshell/feat-x",
        "worktree": "/home/u/.worktrees/openshell/feat-x", "repo": "/home/u/code/openshell"}]}
  ],
  "agents": [
    {
      "aid": "a3f9c1d2",
      "state": "live",
      "kind": "pi", "name": "gateway-name", "cwd": "...",
      "workspace": "w:wt:/home/u/.worktrees/openshell/feat-x",
      "branch": "feat/4840-...", "worktree": "...",
      "session": {"agent":"pi","kind":"path","value":"..."},
      "resume": ["pi", "--session", "..."],
      "entry_id": "h1a2b3c4", "role": "",
      "git": {"dirty": 2, "unpushed": 1, "checked_at": "..."},
      "notes": "~/.local/state/comebye/resume/a3f9c1d2.md",
      "last_seen": "...", "last_status": "idle"
    }
  ]
}
```

- Workspaces are keyed by what they *are* (repo path or worktree path), not
  by herdr's `w1T`-style ids, which renumber. Parent/child follows the
  worktree relationship (a repo's workspace and its worktree workspaces),
  so a restore can rebuild "one workspace per repo, worktree sessions as
  children" without remembering herdr's ids.
- `branch` is the real branch (`git rev-parse --abbrev-ref HEAD`, as the
  checks do), not herdr's `WorktreeInfo.Branch`, which is only the trailing
  path segment of the checkout. They differ whenever a branch name has a
  slash or was renamed (section 3b).
- `resume` is the argv that reopens the session. herdr has the table
  (`src/agent_resume.rs::plan`: `pi --session <v>`, `claude --resume <id>`,
  `codex resume <id>`, ...). Until herdr exposes it per agent (open
  question), Comebye carries a small table for the kinds it launches,
  with a test that pins it, and stores the argv it used so a later table
  change cannot break an old snapshot.
- Agents with no `session` get `"resume": null` and `"anchored": false`.
  They are saved (cwd, kind, name, workspace) and can be restarted fresh,
  but not resumed.

### Never shrinks

A record leaves the fleet record only by an explicit act:

- the user confirms a removal in the app (`Store.Removed`), or
- `comebye forget AID` / the app's "forget" on an `absent` agent, or
- the existing prune-with-confirm flow, for records `absent` longer than a
  retention (default 30 days).

Everything else moves state: `live` -> `absent` (not in the agent list, host
up) -> back to `live` when it returns. A host that is unreachable keeps its
last record untouched.

**Interruption detection.** The recorder watches agent-list deltas per
host. If at least max(3, half) of a host's live agents go `absent` within
30 s, or herdr's server restarts (the socket reconnects after a gap and the
revision counter resets), the recorder opens an **interrupted epoch**:

- the record from just before the burst is kept as snapshot `rev N` and is
  never overwritten by smaller ones during the epoch;
- agents in it are `absent`, ledger entries get a new status
  `interrupted` (not `finished`, not `removed`), so briefings and
  `comebye ledger` say "interrupted by a restart", not "ended";
- notifications "session ended" are suppressed for the epoch; one
  "host interrupted: 17 agents" notification replaces them.
- the epoch closes when `restore` completes or the user dismisses it.

Snapshots are written when the record changes materially (agent added or
gone, workspace/branch/session changed), at most every 60 s, plus always on
quiesce start and end. Keep the last 50 and one per day for 30 days.

### CLI and API

```
comebye snapshot ls|show [--host H] [REV]       read-only
comebye quiesce  --host H [--timeout 10m] [--dry-run] [--json]
comebye restore  --host H [--from REV] [--only AID,...] [--dry-run] [--yes]
comebye forget   AID
```

Endpoints (all under the existing auth; agent-role callers are restricted
as below):

| Endpoint | Description |
| --- | --- |
| `GET /api/fleet/{host}` | The current record (`?rev=N` for a snapshot; `GET /api/fleet/{host}/revs` lists them). |
| `POST /api/hosts/{host}/quiesce` | Starts a quiesce run, returns `{run}`; `GET /api/quiesce/{run}` reports progress and results. |
| `POST /api/hosts/{host}/restore` | Body `{from, only, dry_run}`; returns the plan, or with `dry_run: false` starts a run (`GET /api/restore/{run}`). |
| `POST /api/identities/{aid}/forget` | Drops an identity (confirmation as for removals). |

Quiesce and restore are the user's by default. A coordinator can *propose*
them (create the run in `proposed` state and the app asks the user to
confirm), the same pattern as marking sessions to remove, so
"everything is doable by talking to the coordinator" holds without letting
an agent fan a prompt out to every agent unattended.

### `quiesce`

Prepares a host for a restart. It does not stop anything or reboot.

1. **Snapshot** the host now (reason `quiesce`), and refresh git state per
   worktree through the existing checks (`dirty`, `unpushed`, in-progress
   rebase/merge).
2. **Select**: live agents on the host. `blocked` agents (waiting on a
   dialog) are not prompted, since a prompt would answer their dialog;
   they are reported. Agents with no session ref are prompted but reported
   (they will restart fresh).
3. **Prompt** each agent itself, workers first, coordinators last. The text
   is generated, per agent, from a template:

   > The host `<host>` is about to restart. You are `<name>` (agent id
   > `<aid>`, session `<value>`), working in `<worktree>` on `<branch>`.
   > Commit or stash your work, then write your resume notes to
   > `<notes path>`: what you were doing, what is done, what is next, and
   > anything not in the repository. Do not start new work. When done,
   > reply with exactly `READY <aid>` as your last line.

   Notes are keyed by `aid` (the file name) with the session value inside,
   so the restore can find them whatever the pane is called afterwards.
   The notes dir moves from `~/.cache/comebye-resume` to
   `~/.local/state/comebye/resume` (a cache dir may be cleaned); the old
   dir is read as a fallback.
4. **Collect**: wait for each agent to reach `idle`/`done` and its last
   message to contain `READY <aid>` (the transcript reader already exists
   for `comebye read`). Per-agent timeout (default 5 min); a still-working
   agent is left alone and reported as `timeout`.
5. **Verify**: notes file exists (a small host-side check, no file reading
   by agents' tokens), git state re-checked, report new dirt.
6. **Report** per agent: `ready` | `ready, uncommitted work` | `timeout` |
   `blocked (not prompted)` | `no session ref (will restart fresh)` |
   `gone`. Exit status is non-zero unless all are `ready`. Final snapshot
   with `state: quiesced` on the ready ones.

Failure modes: the hub restarts mid-run (the run is persisted; `quiesce
--resume RUN` continues, agents already `ready` are not prompted again); an
agent answers READY without writing notes (verify step flags it); two
quiesces on one host (one lock per host, second refused); an agent that is
the caller (refused, as `prompt` refuses self-prompting: the coordinator
that runs quiesce is excluded and quiesced by its user, last).

### `restore`

Plan first, always: `--dry-run` prints it, and without `--yes` the CLI
prints it and asks. Source is the latest record or `--from REV`.

1. **Diff** the record against the live agent list by `aid`/session ref.
   Agents already live are skipped (this makes the command idempotent and
   safe to re-run after a partial restore).
2. **Workspaces**: for each needed workspace, parents before children.
   A repo workspace: create at its cwd with its label. A worktree
   workspace: `worktree.open` for its path (the same call `startWorker`
   uses to reopen a branch's worktree), falling back to `worktree.create`
   only if the record says it was created and the path is gone *and*
   the branch exists. Existing workspaces with the same cwd are reused.
3. **Agents**: per agent, in a free pane of its workspace: `agent.start`
   with its kind, `args` = the stored `resume` argv minus the executable,
   and its name (with a suffix if taken). Kind availability uses the same
   detection as launch (section 3a). No `resume` -> start fresh and say so.
4. **Wait** for ready (the launch code already waits for a started agent to
   become promptable).
5. **Re-link** with an `expect{aid, host, pane}` so the new ref, even if
   resume forked the session, binds to the same `aid`. The ledger entry is
   updated by `aid`: new pane, workspace, worktree; its status goes from
   `interrupted` back to `running` (or stays `finished`). Roles need no
   change; they are keyed by `aid`.
6. **Prompt** a restore message: you were restored after a restart; read
   `<notes path>` if it exists; check `git status` and the log; continue.
   If there are no notes (an unquiesced interruption) it says so and points
   at the transcript.
7. **Report** per agent: `restored` | `restored fresh (no session ref)` |
   `skipped (already live)` | `worktree missing` | `kind not installed` |
   `resume failed (pane left open)` | `name taken, renamed`.

Failure modes and how they are handled: the worktree was removed or its
branch renamed (match by worktree path, section 3b; report if truly
missing); the resume argv fails in the shell (the pane stays open and the
agent never becomes ready: report `resume failed` and keep going, never
retry in a loop); herdr names it differently (names are re-applied, an
unnamed agent gets its recorded name via `agent.rename`); the host is
unreachable (nothing is attempted, run stays `pending`); a user working in
the host meanwhile (the plan is re-checked just before each agent, so
anything that came alive is skipped).

A run can be started on boot by a service later (`comebye restore --host
self --yes --only-interrupted`), but the first version is manual: the
failure the user hit was a surprise, not a lack of automation.

### App warning

An agent with no session ref cannot be resumed and has no stable identity
(it is tracked by its weak binding). The app shows a "no session" badge on
its card and detail, and a count on the host line ("3 agents can't be
restored"); `comebye agents` marks them with `!`; the quiesce report lists
them. What fixes it is per kind (hooks not installed for Claude Code; a Pi
extension that reports the session), so the badge links to the
corresponding line in `docs/agents.md`. Data needed: the `session` field on
the browser agent (section 1).

### Phasing

0. **Stopgap**: the one-line `liveRoles` fix; `session` and `anchored` on
   the browser/CLI agent (read-only).
1. **Identity**: identity store, resolution, migration of roles, ledger and
   marks, `aid` in the API. This is what ends the role-loss class of bug
   and is the base for everything else.
2. **Recorder**: fleet record, snapshots, never-shrink, interruption epoch,
   `interrupted` ledger status, notification suppression, `snapshot`
   commands, the app badge.
3. **Quiesce**.
4. **Restore**: dry-run/plan only first (usable by hand as a checklist),
   then execution.
5. **Hardening**: local mirror per host (a host restarted while the hub is
   unreachable can still restore itself), herdr `resume_argv` upstream,
   boot-time restore.

## 3. Two bugs

### a) "agent kind pi is not available on this host"

`launchKinds` offers a kind if an agent of that kind is running or
`installed(kind)` finds it. `installed` checked `PATH` and a fixed list of
bin dirs. The hub runs as a systemd user unit with a minimal `PATH`, and an
`npm -g` install under nvm lands in `~/.nvm/versions/node/<version>/bin`,
which was in neither. So pi was "available" only while a pi agent was
running, which is exactly what the reporter saw.

**Implemented** (commit "Launch: find agents installed under node version
managers"): the search list now includes nvm, fnm, mise, asdf, volta, pnpm,
yarn, Linuxbrew and snap directories, with globs expanded for the
per-node-version ones, and a test with a fake home. Running kinds still
count, so nothing that worked stops working.

**Implemented** (with the handoff fix below): `kinds.json` in the state
directory (`{kind: {last_ok}}`, no path), updated when a kind is found, seen
running or launched; `pickKind` accepts a kind remembered within 90 days even
if the lookup fails. A remembered kind that then fails to start reports "worked
on this host before but could not be started now" and is forgotten.

Not done, proposed:

- **Remember kinds that worked** (original proposal, with a path): Persist `<state>/kinds.json`
  (`{kind: {path, last_ok}}`), updated whenever a kind is found or an agent
  of that kind is seen running; `pickKind` accepts a kind remembered within
  90 days even if the lookup fails. It covers installs in places nobody
  listed, and a PATH that is wrong *now* because of a restart.
- **Ask a login shell once.** If the lookup still fails, run
  `$SHELL -lc 'command -v <kind>'` with a 2 s timeout, cache the answer for
  a minute. herdr starts agents in the pane's shell, so that shell's PATH
  is the one that matters. It runs the user's rc files, so keep it a
  fallback and off by default if that is a concern.

### b) Handoff restart fails after the worker renamed its branch

Mechanism, traced through `handleHandoff` and `startWorker`:

1. The ledger entry says `feat/gateway-name-service-hostname`. The worker
   later runs `git branch -m` (to `feat/4840-.../jhjaggars`); the worktree
   directory does not move.
2. A handoff for the old branch finds the entry (`Find` matches on host,
   repo and `Branch`), sees its agent gone, and calls `startWorker` with
   the old branch.
3. `startWorker` computes `wtPath` from the old branch name, finds the
   existing worktree **by path** (`filepath.Clean(wt.Path) == wtPath`), then
   rejects it: `existing.Branch != branch` -> `path_taken` ("is a worktree
   for another branch").
4. Giving the *new* branch name instead does not help either: no entry
   matches, so a second entry for the same worktree is created (and the
   path/branch checks pass only by luck of the reuse-by-branch branch).

Proposal: treat the entry's `Worktree` (and its `aid`) as the identity of a
worker, the branch as an attribute that can change.

- `handleHandoff` with a previous entry passes `prev.Worktree` as a hint in
  `workerBody` (`Worktree string`, internal, not accepted from clients).
- `startWorker` with a hint looks the worktree up by *path only*, requires
  it to be linked and inside the roots, takes the live branch from herdr's
  worktree list, and reports it in `workerResult.Branch`.
- The restart updates `Entry.Branch` to the live branch and keeps the old
  one in `Entry.PreviousBranches`, so a handoff naming either finds the
  entry.
- `Find` also matches `Entry.Worktree == <path of the worktree whose live
  branch is the requested one>`, which closes the duplicate-entry case in
  step 4.
- Longer term the observer refreshes `Entry.Branch` from the real branch
  whenever it checks the worktree, so the ledger does not lag.
- With identities (section 1) the restart rebinds the new agent to the
  same `aid` and `agentPresent` stops matching on pane id, which also
  fixes handoffs answering `existing: true` for an unrelated agent in a
  reused pane.

**Implemented**: `handleHandoff` passes `prev.Worktree` as
`workerBody.Worktree`; `startWorker` finds the worktree by that path only and
takes its live branch; the entry's `Branch` is updated and the old name kept in
`PreviousBranches`, which `Find` also matches. A restart is refused
(`path_taken`) when another live entry owns the worktree. Not done: the
observer refreshing `Branch`, and matching an entry when a handoff names the
*new* branch of an entry that was never restarted. The original note follows.

Not implemented (original note): it touches the worker start path and its checks
(`outside_roots`, `branch_in_primary`), and wants its own tests with a
real git repository; small enough to do right after the identity work.

## 4. `comebye prompt` with a file path

`comebye prompt AGENT TEXT` sends TEXT as is, so a path passed by mistake
reached 17 agents as a literal string.

**Implemented** (commit "prompt: refuse a TEXT that is an existing file"):
when the text is a single argument that names an existing regular file, the
command exits with a usage error before contacting the hub and says what to
run instead: `comebye prompt AGENT - < FILE`. Multi-word text, `-`, and text
that merely resembles a path that does not exist are unaffected. A test
covers the refusal and that nothing reaches the hub. The docs line for the
command mentions it.

Not done: the same footgun for `comebye brief`/`handoff --brief`, which
already read a file when given one (`--brief -` or a path); the behaviour
there is deliberate and documented, so I left it.

## Open questions

1. **Identity key**: a hub-minted `aid` holding session refs (recommended)
   or the raw session ref as the key? The second is simpler but breaks on
   `/clear` and for agents without a ref.
2. **Who runs quiesce and restore**: the user only, or coordinators may
   propose and the user confirms (recommended; consistent with marks and
   discard)?
3. **herdr**: three asks, in order of value: expose the resume argv in
   `agent.list`'s `agent_session` (so Comebye does not mirror the table);
   do not clear an agent name on a label-only change with no session
   change; expose a stable agent id. Do you want Comebye to file these, or
   are they yours to raise with the herdr project? Draft for the second:
   "agent name is cleared by `reconcile_agent_name_owner` when process
   detection reports a different agent label while the session ref is
   unchanged (state.rs, called with `session_ref = None`)."
4. **Where the record lives**: the hub's disk only (a hub host that is
   rebooted keeps it), or also mirrored on each host (phase 5)? Mirroring
   needs a peer endpoint and a merge rule.
5. **Notes location**: `~/.local/state/comebye/resume/<aid>.md` instead of
   the `~/.cache` path used so far, OK?
6. **Boot-time restore**: manual only at first. Do you want it automatic
   later, and if so with or without the confirmation?
7. **Retention**: 30 days for `absent` agents and 50 snapshots per host
   proposed; any other number?
8. **Scheduled briefings**: the scheduler that asks "is a
   coordinator running?" is `internal/server/briefsched.go`; it now skips and
   retries when it cannot tell.
9. **Names**: after a rebuild the VM's Pi agents have no herdr name. Should
   the hub re-apply the recorded name when it finds an identity without
   one (a "name self-heal"), given that names are only cosmetic once roles
   are keyed by `aid`?

## Suggested implementation order

1. Stopgap `liveRoles` fix, plus `session`/`anchored` on the agent view.
   (small, safe, immediately useful)
2. Identity store and migration; roles, marks and ledger by `aid`;
   `agentPresent` by identity. (the real fix for section 1)
3. Handoff restart by worktree path and live branch (3b), on top of 2.
4. Fleet recorder, never-shrink, interruption epoch, `interrupted` status,
   snapshot commands, app badge.
5. `quiesce`.
6. `restore`: plan only, then execution.
7. Kind memory and login-shell probe (3a, rest); local mirror; herdr
   upstream asks; boot-time restore.

Done already, in this branch: 3a (search list) and 4.
