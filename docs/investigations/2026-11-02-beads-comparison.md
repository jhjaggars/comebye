# Comebye handoffs vs. beads

Date: 2026-11-02. Question from the user, via the coordinator: how does
Comebye's handoff and communication scheme compare with
[beads](https://github.com/gastownhall/beads), and should Comebye adopt it,
integrate with it, borrow from it, or leave it alone?

**Short answer:** don't adopt beads as Comebye's store. Borrow four of its
ideas: typed waits (gates), an outcome on every report, persisted progress
notes plus an event history, and workflow templates. Optionally record an
external issue reference (a bead ID, a GitHub issue) on a handoff. Beads is
a work graph that agents pull from. Comebye is a control plane for live
sessions that pushes work to them. They overlap only in the "plan with
dependencies" corner, and Comebye already owns the hard part beads leaves to
"an orchestrator".

Sources: the Comebye tree at `origin/main` (544f565); beads at `22e9178`
(main, 2026-11-02) and its v1.3.2 release binary, run in a temp dir (see
[Verification](#what-was-verified)).

## Comebye's model

**Who talks to whom.** The user talks to the coordinator in chat and
decides in the phone app. The coordinator acts only through `comebye`
commands, which go to the hub's HTTP API. Workers talk only to the hub
(`comebye report`). The hub talks to agents by typing into their herdr panes
(`agent.prompt`) on whichever host owns the pane: a peer forwards its
agents' CLI calls to the hub, and the hub calls the peers' `/api/worker`,
`/api/checks` and `/api/remove`. Workers never talk to each other. The
agent-comms proposal (`docs/proposals/agent-comms.md`) deliberately routes
peer messages through the coordinator.

**Through what.**
- *Handoff*: `comebye handoff … --brief FILE` creates a branch and worktree
  on the target host, starts an agent in a herdr pane, and types the brief
  wrapped in the template (`internal/server/handoff.go:38`). The template
  fixes the rules (this worktree, commit, don't push, no tmux) and the
  report format (Summary / Changes / Verification / Follow-ups).
- *Report*: `comebye report -` moves the entry to `finished` with
  `report_source: explicit` and queues a **callback**. The callback is a
  fixed-text "Comebye (hub notice)" prompt, delivered to the requester only
  while it is idle, and to the coordinator if the requester is gone
  (`callback.go`). Without an explicit report, going idle after working
  counts as finished, and the hub takes the last reply as the report.
- *Prompt*: `comebye prompt` types text now and is refused on an open
  dialog. `--queue` holds it on the hub for up to 2 h, at most 5 per agent
  (`promptq.go`). `comebye land` is a standard prompt sent through the same
  path.
- *Plan*: `plan create --size N` groups up to 20 handoffs. `--after STEP`
  makes a `waiting` entry that the hub starts once every STEP has
  *reported*, with their reports appended to its brief (`after.go`).
- *Marks, ask and sweep*: proposals to remove a session. The hub asks the
  session whether it is SAFE, and the user confirms each removal in the app.
- *Briefing*: one markdown card per hub, published by the coordinator,
  optionally on a schedule.

**What is persisted where.** All of it is on the hub, as JSON files in the
state dir: `ledger.json` (entries, marks, plans), `queue.json`,
`brief.json`, `brief-schedule.json` and `roles.json`. An entry is the
*current* state of one handoff: status, one `final_report` (20 kB cap), one
mark, one callback. There is no event log, and a follow-up round overwrites
the report (keeping rounds is planned in the lifecycle draft). The ledger
lists only the latest 20 removed or failed entries. The work itself
persists in git (branch, commits), and the conversation in the agent's own
transcript.

**What a worker sees.** Only its brief, wrapped in the template, typed as
its first message, plus any later prompts. It has no view of the plan, its
siblings or the ledger, apart from the reports of the steps it waited for,
which are appended to its brief. If its context is compacted, the brief
goes with it.

## Beads' model

**What it is.** `bd`, a "distributed graph issue tracker for AI agents". It
is a single Go CLI over a [Dolt](https://github.com/dolthub/dolt) database
(version-controlled SQL). It is an issue tracker built for agents, not an
orchestrator: it starts no agents and sees no sessions.

**Data model** (`internal/types/types.go`):
- **Bead** = issue. Fields: an ID, title, description, design, acceptance
  criteria, notes, type (bug, feature, task, epic, chore, message, gate, …),
  priority 0–4, assignee, owner, created/started/closed timestamps, a close
  reason, `closed_by_session`, labels, metadata, comments.
- **IDs** are content hashes with a project prefix (`proj-5rl`) that grow on
  collision, so agents on different branches or machines never mint the same
  ID. Children are hierarchical: `bd-a3f8.1`.
- **Statuses**: `open`, `in_progress`, `blocked`, `deferred`, `closed`,
  `pinned` (stays open for ever) and `hooked` (claimed by a worker).
- **Dependency edges**: `blocks`, `parent-child`, `conditional-blocks` (B
  runs only if A fails), `waits-for` (fan-in over dynamic children),
  `discovered-from`, `related`, `replies-to`, `supersedes`, `duplicates`,
  `delegated-from` (completion cascades up), `until`, `caused-by`,
  `approved-by` and others. Only the blocking kinds affect readiness.
- **Ready** = open beads with no open blocker that are not in progress,
  deferred or held by a gate. `bd ready --claim` takes the first one
  atomically.
- **Claims** set assignee and `in_progress` atomically. A second claim by
  another actor is refused. Claims carry a **lease** with a TTL, refreshed
  by `bd heartbeat`; `bd reclaim` returns stale-lease work to ready (it
  recovers work from dead workers).
- **Workflows**: a *formula* (a TOML DAG of steps) is cooked into a *proto*,
  which is poured as a *molecule* (real beads) or a *wisp* (ephemeral beads,
  kept out of sync and purged when done). A **gate** is a bead that blocks a
  step until something external happens: `human` (closed by
  `bd gate resolve`), `timer`, `gh:run` (CI green), `gh:pr` (PR merged), or
  `bead` (another bead closes). `bd gate check` evaluates the gates; nothing
  polls on its own.
- **Messaging**: `type: message` beads, threaded by `replies-to`. `bd mail`
  *delegates delivery to an external orchestrator* (`gt mail`, from the
  same authors' Gas Town). In their words: "Beads = data plane, Orchestrator
  = control plane". Scripts in `.beads/hooks/` (`on_create`, `on_update`,
  `on_close`) are the integration point.
- **Agent memory**: `bd remember "…"` stores a project fact, and `bd prime`
  injects about 1–2k tokens of workflow context plus those memories. It is
  run from a Claude Code `SessionStart` hook, so it fires again after
  compaction.

**Storage and sync.** Embedded Dolt in `.beads/embeddeddolt/` is the
default: in-process, **single writer**, file-locked. Server mode
(`dolt sql-server`) allows concurrent writers. Every write is a Dolt
commit, so history comes free (`bd history ID`). Cross-machine sync is
`bd dolt push` / `bd dolt pull` against `refs/dolt/data` on the git remote,
merged cell by cell. Federation (peer to peer, or through an S3/GCS bucket)
and `bd serve` / `bd connect` (an HTTP backend, marked **experimental**) are
the alternatives. `.beads/issues.jsonl` is only an export. All worktrees of
a repo share the primary checkout's `.beads`.

**How agents use it.** Through the CLI with `--json`. `bd setup claude`
installs the SessionStart hook and a CLAUDE.md pointer, and similar setups
exist for Codex, Cursor, Factory and others. There is an MCP server for
environments without a shell, but the docs recommend CLI plus hooks because
of the context cost. Identity is `BEADS_ACTOR`, else git `user.name`, else
`$USER`, and is not authenticated.

**Handoff, claiming, progress and multi-agent coordination.** A handoff is
an assignment plus a comment (`bd assign`, `bd comment`); the next agent
finds it with `bd list --assignee`. There is no agent registry: assignees
are free strings. A merge slot serialises conflict-prone work. Progress
notes are comments and the notes field. Multi-host coordination works only
through Dolt sync: agents must pull to see each other's claims ("sync at
session end"), and a lease is enforceable only on the replica that granted
it. Cross-repo `external:` dependencies exist, but cross-rig bead gates
"never resolve on their own" (known limitation #5861).

**Maturity.** MIT licence. 1.0 shipped 2026-04-02; the current release is
1.3.2 (2026-10-23), with releases every few weeks and RCs in between. It is
very active: 10–11 commits on each of the last two days, and a CHANGELOG
over 9,000 lines. That brings churn: the schema is at migration 74, and a
schema migration on a shared database must run on exactly one clone
(`bd migrate schema`) while the other clones re-bootstrap. A binary behind
the schema refuses to open the database. Dependencies are heavy: Dolt,
go-mysql-server, vitess, OpenTelemetry, testcontainers and the Anthropic
SDK; the linux binary is 154 MB; it needs Go 1.27.3 to build from source.

## Comparison

| | Comebye | beads |
| --- | --- | --- |
| Unit of work | A **handoff**: a live agent session, a branch and worktree, a brief and a report. Identity is host/repo/branch. | A **bead**: a task record (title, description, acceptance criteria) with no session, branch or worktree. Identity is a hash ID. |
| Lifecycle | waiting → running → finished → (landing → landed, planned) → ready_to_remove → removed; failed | open → in_progress/hooked → closed; also blocked, deferred, pinned |
| Planning | `plan --size N` (≤20) and `--after STEP` (≤8). The only edge is "starts when the step *reported*", and a failed step skips its successors. | A general DAG with typed edges: blocks, conditional-blocks, waits-for, parent-child; `bd ready` computes the frontier; formulas and molecules make reusable DAGs. |
| Who picks work | **Push**: the coordinator chooses the worker, host, model and brief, and the hub starts it. | **Pull**: agents run `bd ready --claim`; a coordinator can `bd assign`. |
| Communication | The hub types into panes: the brief, prompts (refused on dialogs, or queued), fixed-text callbacks on report, plan notices and the ask on mark. Synchronous and live. | Agents read and write the shared graph: comments, labels, message beads. Delivery and notification are left to an external orchestrator (`gt mail`) or `.beads/hooks`. |
| Liveness | Real: herdr reports working, idle, blocked or dialog open; the hub sees a stall or a question directly. | Inferred: claim leases with TTL and heartbeat, and `reclaim` for dead workers. Nothing knows whether an agent is actually running. |
| Persistence | Hub JSON files: current state only, one report per entry, the last 20 removed shown. Git holds the work; transcripts hold the conversation. Survives a hub restart; a worker that finishes while the hub is down stays `running`. | A Dolt DB with every write committed: a full per-bead history, comments and close reasons. It survives everything, including the machine if pushed. |
| Audit trail | Thin: `from`, mark `by` and reason, callback state, removal record. No event log. | Rich: Dolt commit history per bead, `closed_by_session`, created_by. But actor identity is self-asserted. |
| Multi-host | One hub, many peers over Tailscale. Agents on every host act on the same ledger immediately; git checks run on the owning host. | Each clone has its own DB; it converges through push/pull (cell-level merge) or a shared server/`bd serve`. Claims are only as fresh as the last sync. |
| Multi-agent | One worker per branch, enforced. Workers are isolated and don't see each other. | Atomic claims, merge slots, fan-out/fan-in through dependencies; agents see the whole graph. |
| Human in the loop | Central: the user OKs landing and deploy, confirms every removal in the app after git checks and the session's own SAFE/UNSAFE answer, answers dialogs, reads briefings. Push notifications on the phone. | `human` gates (`bd gate resolve`); otherwise the human is just another actor with the CLI. No approval UI, no removal concept, no notifications. |
| Context for the agent | The brief once, at start. Lost on compaction. | `bd prime` on every SessionStart, including after compaction, plus `bd remember` memories. |
| Session control | Launch, prompt, queue, model switch, land, mark, remove, worktree lifecycle. | None. |
| Git integration | Worktrees, branch checks, patch-id merge detection, landing to main, discard. | Stores its DB under `refs/dolt/data`; knows nothing about the work's branches beyond `gh:pr`/`gh:run` gates. |

### Multiple workflow types

The user is designing these separately (`docs/lifecycle.md` on
`design-agent-lifecycle`). Here is how each tool would carry each type:

| Workflow | Comebye today | beads | What the comparison suggests |
| --- | --- | --- | --- |
| feature | handoff → report → land (user OK) → mark. Fits. | molecule: steps blocked in order, a `human` gate before merge, a `gh:pr` gate. | Comebye already fits. Record the user's OK as a gate, as the lifecycle draft does with `--note`. |
| test-and-report | A handoff whose report is the product; nothing to land, but it still goes through landing-shaped states. | A wisp: ephemeral, purged when closed. | A workflow type with `lands: false` whose terminal state is `finished`, swept straight after the report is read. |
| research | Like this task: commits a doc, may land it, and feeds a later step through `--after`. | A bead whose close reason or notes carry the result; `discovered-from` beads for findings. | The report feeds successors already. Add structured follow-ups (below). |
| review | Not modelled. A worker would need the PR's branch; a reviewer is a planned standing role. | A bead gated by `gh:pr`; `approved-by` and `validates` edges. | Reviewing needs a "wait for PR or ready" trigger and an outcome (approve / changes requested) that routes the next step: `conditional-blocks`. |
| watch | Not modelled (planned PR-watcher role). | A pinned bead, or a timer, `gh:run` or `gh:pr` gate polled by `bd gate check`. | A standing role plus hub-evaluated gates (PR merged, CI green, timer) that wake it or its successor. |
| deploy | `land --deploy` (batched `comebye deploy` planned). | A formula with a `human` gate and a `gh:run` gate. | Deploy = human gate + CI gate + recorded SHAs. The gate vocabulary fits it exactly. |

Beads' formulas are the closest thing to "workflow types": a named DAG of
steps with variables, gates and a persistence phase (`vapor` = ephemeral).
That is the part most worth borrowing for the lifecycle work, as a
*template* idea. Comebye doesn't need its storage.

### What each does that the other can't

- **Only Comebye**: starts and stops agents of any kind on any host; knows
  if an agent is really working, blocked or asking; refuses to type into a
  dialog; manages worktrees and landing; checks merged-ness by patch-id;
  asks a session if it is safe to remove; gates every destructive action on
  the user in a phone app; pushes notifications.
- **Only beads**: a general typed dependency graph with a computed ready
  frontier; atomic pull-based claiming with leases; reusable workflow
  templates (formulas), with ephemeral instances; external-condition gates
  (CI, PR, timer); a full versioned history of every work item; offline
  multi-writer sync with merge; context re-priming after compaction;
  long-lived backlog and memory across sessions and projects.

## Options

**(a) Adopt beads as the work store under Comebye.** Comebye would keep
session control, and the ledger's work items would become beads.
*Cost: high.* Comebye's entry is a session lifecycle (landing, marks, asks,
removal, callbacks, worktree checks) that beads doesn't model, so it would
end up in metadata or a parallel store anyway: two sources of truth.
Comebye is already hub-centred, so Dolt's offline merge buys little; the
hub would be the only writer (server mode or `bd serve`, which is
experimental). It adds a 154 MB, Dolt-based dependency with a schema
migration every few weeks, coordinated across hosts, to a project that
deploys a single small Go binary per host. Workers on peers would need a DB
connection or sync to read their task. *Gain:* history and a DAG for free.
**Not recommended.**

**(b) Borrow specific ideas.** *Cost: low to medium,* each piece on its
own, all inside the existing ledger. These fit the lifecycle work in
progress:
1. **Typed waits (gates)** on `--after`: besides "step reported", also a
   human OK, PR merged, CI green, a timer, and "step failed"
   (`conditional-blocks`). The hub evaluates them in `releaseQueued`, like
   `bd gate check`, but on its own loop.
2. **Report outcome**: `comebye report --outcome done|needs-input|blocked|failed`.
   Today `finished` cannot tell "done" from "stopped to ask"; beads keeps
   the two apart with status and close reason. Only `done` releases the
   steps waiting on it.
3. **Progress notes and history**: `comebye note "…"` appends a timestamped
   note to the worker's own entry, and the hub keeps an append-only event
   list per entry (status changes, prompts sent, notes, reports, rounds).
   That is the beads comment and `bd history` idea without Dolt. It also
   gives the planned `stalled` flag a signal better than idle time.
4. **Workflow templates** (formula-lite): a named type that sets the brief
   scaffold, report sections, whether it lands, whether it is standing or
   ephemeral (the wisp idea: swept straight after reading, kept out of the
   briefing's history), and the default waits.
5. **Re-priming**: `comebye task` prints the worker's own brief, plan
   position and the notes so far; a SessionStart hook in the worktree (the
   `bd prime` pattern) or a line in the template tells the worker to run it
   after compaction.

**(c) Integrate loosely.** *Cost: low.* `comebye handoff --ref <id|url>`
stores an opaque external reference (a bead ID, a GitHub issue) on the
entry, and the ledger, app and callback show it. Users who run beads in a
repo already get it in the worktree (beads finds the shared `.beads` from
linked worktrees), and the brief can say "close `bd-xyz` in your report
step". Comebye never runs `bd`. Later, an optional `.beads/hooks/on_close`
script could call `comebye` to wake a coordinator, but not now.

**(d) Don't adopt anything.** *Cost: none,* but the lifecycle work would
reinvent gates and history without the benefit of a design that has
already been through the edge cases (gate renames, stale leases,
idempotent IDs).

**Recommendation: (b), plus the one-field version of (c).** Comebye's value
is the live, human-gated control plane, which beads explicitly leaves to
someone else, and its central hub removes most of the reasons for beads'
storage design. Beads' best ideas are about *structure* (typed waits,
outcomes, templates, history), and they fit into `ledger.json` and the
existing release loop in a few hundred lines.

**Prototype first: typed waits.** Turn `Entry.After []string` into
`Waits []Wait{Kind: step|step_failed|human|pr_merged|ci_green|timer, Ref, Note}`,
evaluated in `releaseQueued`, with `step` behaving as today. Add
`comebye release --approve` (or the lifecycle's `land --note`) as the way a
`human` wait is satisfied, and `gh pr view` / `gh run view` on the owning
host for the PR and CI kinds. This one change gives review, watch and
deploy their triggers and makes landing approval a recorded gate. Second:
`--outcome` on `report`, because the waits need it to tell done from
needs-input.

## Suggested changes to the lifecycle design

For the coordinator to forward to `design-agent-lifecycle`:

1. **Report outcome.** Add `outcome` to `comebye report`
   (`done | needs-input | blocked | failed`). `finished` stays the state;
   the outcome decides whether `--after` successors start and whether the
   coordinator asks the user something before approving. It replaces the
   "a finished worker may have stopped to ask" guesswork.
2. **Generalise `--after` into typed waits (gates).** Kinds: `step`
   (today), `step_failed` (run a fixer only if the step failed),
   `human` (the user's OK, recorded with who, when and their words; landing
   approval is one), `pr_merged`, `ci_green`, `timer`. They are evaluated by
   the hub and shown on the waiting entry with the reason ("waits for PR
   #42 to merge").
3. **Workflow types as templates.** Each type (feature, test-and-report,
   research, review, watch, deploy) declares its brief scaffold, report
   sections, whether it lands (`land: main|pr|none`, which aligns with the
   planned land modes), whether it is task, standing or ephemeral, and its
   default waits. Ephemeral types (test-and-report, most watches) end at
   `finished` and are sweep-ready once the report is read.
4. **Per-entry event history and progress notes.** Append-only events
   (state changes with who, prompts sent, notes, reports, landing checks)
   kept with the entry, and every round's report kept; this subsumes the
   planned "rounds". Add `comebye note` for workers, and base the planned
   `stalled` flag on time since the last note or turn, not just idle time.
5. **Structured follow-ups.** Parse the report's Follow-ups into proposed
   items linked to the entry (`discovered-from`). The coordinator offers
   them to the user as handoffs and never starts them on its own (that
   rule is unchanged).
6. **Re-priming after compaction.** A worker can recover its task with
   `comebye task` (its brief, plan position, waits and notes); the template
   tells it to run that if it has lost context.
7. **Optional `--ref`** on a handoff for an external issue or bead ID,
   shown in the ledger, briefing and callback. No beads dependency.
8. **Leave out**: pull-based claiming, leases and heartbeats. Comebye
   assigns work and sees liveness directly from herdr, so they solve a
   problem it doesn't have.

## Design sketch: typed waits and a backlog

The user's reply to the report: typed waits and a long-lived backlog sound
useful. The user then decided that the backlog lives on the hub and is
owned by the coordinator, with beads at most as a later bridge. The two belong together. A backlog item is a handoff that hasn't
started yet, and waits decide when it *can* start. The backlog's "ready"
view is then beads' `bd ready`: approved items whose waits are all met.

### Today

`Entry.After []string` holds the ids of members of the same plan
(`internal/server/after.go`). `stepsState` returns `go` only when every
step reported explicitly (`report_source: explicit`), and `fail` when one
failed or was removed. `releaseQueued` runs on every ledger observation and
starts or fails `waiting` entries. `--after` requires `--plan` and accepts
at most 8 steps. The successor's prompt gets the steps' reports, fenced as
data (`stepContext`).

### Typed waits

```go
type Wait struct {
    Kind  string     // step | step_failed | human | pr_merged | ci_green | timer
    Ref   string     // entry id or branch; "owner/repo#42"; branch or run id; "2h" or an RFC 3339 time
    Note  string     // human: what is being approved
    Met   *time.Time // set once, never unset
    MetBy string     // human: who relayed it, and the user's words
    Check *WaitCheck // external kinds: last check time, result, error
}
```

- `Entry.Waits []Wait` replaces `After`. On load, `After` is migrated to
  `step` waits, and `After` stays readable for older files. `--after X`
  remains an alias for `--wait step:X`.
- **Semantics per kind:**
  - `step`: the step reported with outcome `done` (until outcomes exist:
    explicit report, as today). If the step failed, the wait fails, as
    today.
  - `step_failed`: the step reported `failed` or `blocked`. A `done`
    report *skips* the waiter rather than failing the plan. This is the
    "run a fixer only if the tests fail" edge (beads' `conditional-blocks`).
  - `human`: met only by the user's OK, through an **Approve** button on
    the waiting entry in the app, or by the coordinator relaying it with
    `comebye approve ID --note "<the user's words>"`. This matches how the
    lifecycle draft records a landing OK, and the playbook rule stays the
    same: never without the user.
  - `pr_merged`: `gh pr view <n> -R owner/repo --json state,mergedAt`.
    MERGED meets it; CLOSED without merging fails it.
  - `ci_green`: `gh run list --branch B --limit 1 --json status,conclusion`,
    or `gh run view <id>`. Success meets it; failure fails it unless the
    wait says `retry`.
  - `timer`: a duration from when the entry was created, or an absolute
    time.
- **Where checks run.** `gh` runs on the entry's host, which has the
  repository and its credentials, through a new peer endpoint
  (`/api/waitcheck`, beside `/api/checks`), with arguments passed as argv
  and `owner/repo` validated against a pattern. Each external wait is
  checked at most every 2 minutes. Errors (`gh` missing or not logged in)
  are shown on the wait, and a wait never counts as met because of one.
- **Without a plan.** `step` waits still need a plan, since they name
  members. External and `human` waits don't, so a watch handoff can simply
  wait for a PR.
- **Shown everywhere.** The ledger, the app and `comebye plan show` say
  why an entry waits: "waits for PR #42 to merge (checked 1 min ago)",
  "waits for your OK: deploy to prod". A `human` wait sends the user one
  push notification when it becomes the only thing left.
- **CLI.**
  `comebye handoff … --wait step:research/x --wait pr:owner/repo#42 --wait ci:main --wait human:"OK to deploy" --wait timer:2h`;
  `comebye approve ID --note …`; `comebye waits ID`.
- **What the workflow types get from it:**
  - review = `pr_merged` reversed: `--wait pr-ready:#42` is a possible
    later kind;
  - watch = `ci_green` or `pr_merged` on a standing entry;
  - deploy = `human` + `ci_green`;
  - test-and-fix = `step_failed`;
  - landing approval = `human` on the landing phase.

### A backlog: on the hub, owned by the coordinator

*Decided (the user, via the coordinator, 2026-11-02):* the backlog lives on
the hub and belongs to the coordinator. The coordinator uses it to keep
the user informed and to drive agents. beads is at most an optional bridge
later (below).

```go
type Item struct {
    ID, Title, Brief string
    Host, Repo, Branch string // optional until it is started
    Workflow string           // feature | test | research | review | watch | deploy
    Priority int              // 0..4, as in beads
    Waits    []Wait
    Ref      string           // optional external issue or bead id (opaque)
    Origin   string           // user | coordinator | follow-up of entry <id> (beads' discovered-from)
    Status   string           // proposed | approved | started | dropped | done
    EntryID  string           // the handoff it became (one per round)
    Events   []Event          // who changed what, when
}
```

- **Stored on the hub** as `backlog.json`, beside the ledger: one backlog
  for every host and repo, with no cap on open items. Done, dropped and
  started items are kept for history. Like the ledger and the briefing,
  it outlives any one coordinator: a new coordinator reads the backlog
  and carries on.
- **The coordinator owns it.** It is the only agent that writes it: `add`,
  `edit`, `prioritise`, `approve`, `start`, `drop`. Workers can't write
  it. They hand the coordinator follow-ups in their report, as today. The
  agent-role check would allow these routes for coordinators only. That
  enforcement waits on reliable caller identity (the deferred
  server-enforced coordinator role); until then it is a playbook rule, as
  "only coordinators hand off" is today.
- **Where items come from:**
  - the user's requests, which the coordinator records as `approved`
    items so nothing they asked for is lost between sessions;
  - the coordinator's own proposals;
  - the Follow-ups section of each report. The coordinator reads it and
    files the items as `proposed`, linked to the entry. These items are
    data from a worker: the playbook rule stays that nothing is started
    because a report says so.
- **Approval stays the user's.** `proposed` → `approved` only on the user's
  word: the coordinator relays it (`comebye backlog approve ID --note
  "<the user's words>"`, recorded like the land OK), or the user taps
  Approve in the app.
- **Driving agents.** Within approved items, the coordinator decides what
  starts when. `comebye backlog ready` lists approved items whose waits
  are met, by priority, and `comebye backlog start ID` fills in host, repo
  and branch and makes an ordinary handoff (`Item.EntryID` set, status
  `started`). When the handoff's work is landed, or finished for workflow
  types that don't land, the coordinator sets `done`. Items with `step`
  waits between them can be started as a plan (`backlog start --plan ID…`),
  so a chain runs while the coordinator is asleep.
- **Keeping the user informed.**
  - The briefing's "Next:" lines come from the ready and proposed items.
  - The coordinator answers "what's queued?" from `comebye backlog`.
  - When an item's waits are all met, the hub sends the coordinator a
    notice, as it does for a finished worker, so it can start the item or
    tell the user.
  - The app shows the backlog read-only, with Approve for proposed items.
    The user changes everything else by asking the coordinator.
- **Hub notices, not hub actions.** The hub stores the backlog, checks
  waits and sends notices. It never starts an item itself; the
  coordinator does.

### Optional later: a beads bridge

Not planned now. If a repository already keeps its work in beads, two
small tools would connect it without a dependency in the hub:
- `comebye backlog import --beads REPO` (run by the coordinator, which
  runs `bd ready --json` itself on that host) would copy beads into
  `proposed` items with `Ref` set;
- a worker whose item has a bead `Ref` could be told in its brief to
  `bd close` it.

Beads' storage, sync and gates stay out of Comebye.

### Order to build

1. `Waits` in the ledger, with `step`, `step_failed`, `human` and `timer`
   (no external calls), the `After` migration, `comebye approve`, and the
   app's "waits for" line and Approve button.
2. `pr_merged` and `ci_green` through `/api/waitcheck` on the owning host.
3. `comebye report --outcome`, so `step` and `step_failed` read the
   outcome rather than guess.
4. The backlog store and the coordinator's CLI
   (`backlog add/edit/list/ready/approve/start/drop/done`), the
   "waits met" notice to the coordinator, the playbook section, and
   briefing "Next:" from the backlog.
5. The app's read-only backlog view with Approve.

## What was verified

- **Read in source (Comebye):** `internal/playbook/playbook.md`,
  `docs/agents.md`, `docs/api.md`, `docs/proposals/agent-comms.md`,
  `internal/ledger/ledger.go` (Entry, Status, Callback, Mark, Ask),
  `internal/ledger/plan.go` (outline), `internal/server/handoff.go`
  (template), and the outlines of `after.go`, `promptq.go` and
  `callback.go`. The lifecycle draft on `design-agent-lifecycle` was read
  (not edited) for alignment.
- **Read in source or docs (beads, `22e9178`):** README; docs
  core-concepts (index, issues, sync), multi-agent (index, coordination),
  workflows (gates, wisps), integrations/claude-code, faq (worktrees),
  `engdocs/messaging.md`, `CLI_REFERENCE.md` (heartbeat, reclaim),
  `internal/types/types.go` (Issue, Status, DependencyType), `go.mod`,
  LICENSE, CHANGELOG release headings, `git log` dates.
- **Ran:** downloaded `beads_1.3.2_linux_amd64.tar.gz` with `gh release
  download` into a `mktemp -d` dir, checked it against the release
  `checksums.txt`, and ran it with `HOME` and `BEADS_DIR` pointing into
  that temp dir (`bd init --stealth --skip-agents`,
  `dolt.local-only true`). Created three beads chained with `blocks`
  dependencies: `bd ready` showed only the first. Claimed it as
  `worker-1`; a claim as `worker-2` was refused ("issue already claimed by
  worker-1"). Added a comment, closed it with a reason; `bd ready` then
  showed the next step, and `bd show` / `bd history` showed the comment,
  the reason and 10 history entries. Init took 1.4 s; the database was
  2.2 MB. Nothing was installed globally, and the user's config wasn't
  touched.
- **Inferred, not run:** server mode, `bd serve`/HTTP, Dolt push/pull
  between machines and merge behaviour, formulas and molecules, gate
  evaluation, `bd prime` hooks, the MCP server, `bd mail` (which needs an
  external delegate). Claims about these come from the docs.
