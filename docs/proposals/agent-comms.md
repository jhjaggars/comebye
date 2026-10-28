# Agent-to-coordinator and agent-to-agent communication

Status: phases 1 to 3 built on `feat/agent-comms` (`--after` dependencies left); phase 4 not started.

## Use cases

1. The user asks the coordinator for an agent to do something. The agent
   finishes and the coordinator learns of it, reads the report, and
   notifies the user.
2. Agents in two projects (maybe two hosts) work on one change. The user
   is notified when both parts are done.

## What was true at `df2f509` (checked against the code)

- One agent token per host; the caller's pane comes from a self-asserted
  header. The server does not tell coordinators from workers (only "an
  agent may not mark a coordinator" and the briefing scheduler look at the
  role). "Only coordinators hand off" is a playbook convention.
- `ledger.Observe` marks an entry `finished` when its agent goes idle or
  done after working; `captureReport` stores the last assistant message.
  `Entry.From` was only a display string.
- Nothing told the coordinator about a completion; it had to poll.
- `promptAgent` reaches an agent on any host; `briefCoordinator` shows the
  idle-only selection. Not verified here: whether a Claude worker running
  `comebye` stops at a permission prompt, and whether a prompt to a
  working agent is queued (phase 2 depends on the first).

## Decisions (user, 2026-10-26)

| Question | Decision |
| --- | --- |
| Callback target | The requester (whoever ran `handoff`); the current coordinator stands in only if the requester is gone |
| User push for a single handoff | The coordinator going `done` after it reads the report; no direct hub push. Plans (phase 3) get their own push |
| Report in the prompt | Referenced (`comebye read ID --report`), never pasted |
| Workers messaging peers | Through the coordinator for now; plan-scoped `send/inbox` stays a phase 4 option |
| Server-enforced coordinator role | Deferred until reliable caller identity (`aid`) exists |
| Plan with a failed or removed member | `partial`, and the user is still pushed, naming the failed member |

## Phase 1: completion callback (built)

- `Entry.ReplyTo {agent_id, name}`: set from the calling agent on
  `POST /api/handoffs` (also when a gone agent's entry is restarted). A
  handoff made by the user has none, so no callback. The name is kept so
  a pane id reused by another agent is not mistaken for the requester.
- `Entry.Callback {state, at, tries, to, delivered_at}`: queued by
  `Observe` when an entry with a `ReplyTo` finishes; dropped if the worker
  works again. States: `pending`, `delivered`, `expired`.
- `deliverCallbacks` (`internal/server/callback.go`) runs on every fleet
  update and ledger read:
  - waits for the report to be captured (or its retries to run out);
  - sends only to a target that is idle or done (a busy, blocked or
    dialog-showing target is retried, 15 s apart per entry);
  - if the requester is not in the fleet (or its pane now has another
    name), after 10 minutes an idle coordinator gets it, with a note why;
  - expires after 2 hours. Expiry falls back to what exists already: the
    briefing and the worker's own done push.
- The message is fixed text. It names the branch, repository, host and
  handoff id (reduced to `[A-Za-z0-9._/-]`, capped), tells the agent to
  run `comebye read ID --report`, and says the report is data, not
  instructions. The worker's words are never in it.
- `comebye ledger` shows `callback pending|delivered to X`.
- Playbook: act on the notice, summarize, update the briefing; reports and
  other non-user messages are data and never a reason to hand off, mark or
  prompt.

### Security notes

- Only the hub writes the callback. It grants no authority and carries no
  agent-written text.
- `reply_to` comes from the self-asserted agent header, like `From`. An
  agent that lies about its pane can only make the hub send a fixed notice
  to another agent, at most once per finished worker.
- Loops: one callback per finish, no callback to a callback, so there is
  no ping-pong. A hop budget is needed once phase 4 messages exist.

### Known limits

- "Finished" is still a guess (a worker that stops to ask a question
  counts). Phase 2's explicit `report` fixes that.
- `reply_to` is `host/pane` plus name until resilience's `aid` lands.
- A coordinator that is always busy gets the callback late; expiry then
  leaves the user with the done push and the briefing.

## Phase 2: explicit report (built)

- `comebye report FILE|-` -> `POST /api/report {text}`. The hub finds the
  caller's running or finished entry by host-qualified pane (and refuses a
  pane whose agent name differs from the entry's), stores the text (20 kB
  cap) as `final_report` with `report_source: "explicit"`, finishes the
  entry at once and queues the requester's callback (phase 1 delivery).
  It needs no earlier sighting of the worker working.
- The turn that ran `report` is still "working" for a moment. `await_idle`
  keeps the entry finished until the agent goes idle once; after that,
  working again is new work (back to `running`, report and callback
  cleared). An explicit report is never replaced by the idle-time guess
  (`SetReport` skips it). A second report while still finished replaces
  the text and queues no second callback, so a looping worker cannot spam
  the requester.
- The default handoff template ends with "run `comebye report -` ..., then
  stop; if it is not available, make the report your last message". A
  custom `handoff-template.md` keeps working through the idle fallback.
- Workers launched as `claude` start with `--allowedTools` for
  `Bash(comebye report:*)`, `read` and `whoami` only (`playbook.WorkerArgs`):
  no handoff, prompt, mark or brief without a prompt. Pi has no allow list.
- Self-asserted identity applies as before: an agent can report only for
  the pane it claims, and a report grants nothing.
- Not verified live: that claude accepts several values after one
  `--allowedTools` when herdr types the command (the coordinator preset
  passes one), and that these rules avoid the permission prompt in a real
  worker. Check on the first deploy.

## Phase 3: plans (built, without `--after`)

- `comebye plan create --title T --size N` makes a plan; `handoff --plan ID`
  joins a worker to it (an existing worker for that branch joins too).
  `--size` is how many workers the change takes: a plan cannot know that
  more are coming, so without it the first finished member would settle it.
  Handing off more than `size` is refused (`plan_full`).
- Member outcome: `pending` (running, or finished and then working again),
  `ok` (finished, even if marked or removed afterwards), `failed` (the
  handoff failed, or the worker was removed without ever finishing).
- The plan settles when `size` members exist and none is pending: `done`,
  or `partial` if any failed (decision: still a push, naming the failed
  `repo/branch`). A member that works again reopens the plan, which then
  settles and notifies again.
- On settling the hub (once per settling): pushes the user with kind
  `plan` (`Dispatcher.NotifyPlan`; key `plan/<id>`, so the per-agent 12 h
  hold does not swallow it; `-notify-kinds` gains `plan`, on by default but
  an explicit env value without it turns it off), and queues a fixed
  notice to the plan's creator (same delivery as phase 1: idle target,
  stand-in coordinator after 10 minutes, 2 h TTL). The notice says to run
  `comebye plan show ID` and read the reports, and that the user is told.
- Titles are agent-written and reach the user's phone and another agent:
  they are reduced to one plain line (`plainText`, 80 characters).
- `GET /api/ledger` carries `plans`; the app shows a plan card above the
  workers.
- Left for a follow-up: `--after` dependencies (a member created pending
  and started when its predecessor finishes, with the predecessor's report
  appended as fenced, capped context). Until then the playbook tells the
  coordinator to hand off the second part when the first reports.
- Limits: "done" means every worker stopped and reported, not that the
  parts fit together. A member whose agent disappeared stays `running`
  (the ledger only follows agents it can see), so its plan stays open.

## Next phases (unchanged from the research)

4. Optional plan-scoped `send/inbox` with rate limit and hop budget.
