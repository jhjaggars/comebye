# Agents, workers and coordinators

Starting agents from the app, handing work to workers in their own worktrees, coordinators, and what the chat view shows for Claude Code.

## Launching agents

The "+" button in the list header opens a sheet to start a new agent:
choose the host (on a hub), browse to a directory (directories that agents
on that host recently ran in come first; git checkouts are marked), choose
the agent kind, and optionally give it a name and a first prompt. Launch
then:

1. opens a new herdr workspace in that directory (`workspace.create`,
   without focusing it, so the terminal you are at does not jump), labelled
   with the name or the directory's name;
2. starts the agent in the workspace's shell pane (`agent.start`, waiting
   up to 60 s for it to be ready for input);
3. gives it the first prompt, if any: for Claude Code and Pi on their own
   command line (`claude -- <prompt>`, `pi -- <prompt>`), so it cannot
   arrive before the agent reads input; for other kinds, and multi-line or
   very long prompts, with `agent.prompt` once it is up (no wait),

and the app opens the new agent's chat. The default name is the
directory's name made into a valid agent name (`my.Repo` becomes
`my-repo`), with `-2`, `-3`, ... added if an agent already has it. If the
agent fails to start, the error says so and the new workspace is left open
in herdr for you to look at (or close).

The kinds offered are the ones herdr supports whose executable is
installed on that host (on `PATH` or in `~/.local/bin`, `~/.npm-global/bin`,
`~/.bun/bin`, `~/go/bin`, `~/.cargo/bin`, `/usr/local/bin`,
`/opt/homebrew/bin`, since a systemd unit's `PATH` is minimal), plus any
kind already running there. The one most running agents use is
preselected.

Directories are confined to the launch roots: `-launch-root` (repeatable
or comma-separated, `COMEBYE_LAUNCH_ROOTS`, default `~/code`). Every path
is resolved (symlinks included) and must lie inside a root, for listing as
well as launching; roots that do not exist are ignored, and with none
left, or with `-launch=false`, the sheet says launching is off for that
host. Hidden directories are not listed.

**Security note:** launching does not grant anything prompting did not
already: anyone allowed to use the app can make an agent run commands as
you. The roots keep the directory browser (and where agents start) to your
checkouts rather than the whole disk; set `-launch=false` on a host where
you only want to drive existing agents.

## Projects, workers and handoffs

A **project** is a git repository. Its home agents work in the primary
checkout; **workers** each get their own git worktree at
`~/.worktrees/<repo>/<branch>` (slashes in the branch become `-`) and a
herdr workspace nested under the repository's primary workspace, the way
herdr's own worktree flow does it. The branch is the worker's identity.
The agent list groups agents by project (from herdr's checkout info, a
worker's ledger entry, or the `~/.worktrees/<repo>/<branch>` path, else
the directory's name): home agents first, then the workers, indented, with
their branch and handoff status. Tap a project's heading to collapse it
(remembered per browser).

**Handing off.** In the "+" sheet, choose **Worker (new branch)**: host,
repository (the primary git checkouts in that host's launch roots), branch,
optional base (default: the repository's default branch, i.e. origin's
`HEAD`, else `main` or `master`), agent kind and a brief. The hub then, on
the owning host:

1. creates the branch's worktree with `worktree.create` (the branch is made
   from the base if it does not exist yet), or reopens an existing one with
   `worktree.open`, without focusing it;
2. starts the agent in a pane of that workspace that runs no agent
   (`agent.start`); if every pane is busy it refuses rather than start over
   a running agent;
3. sends the brief wrapped in the handoff template (`agent.prompt`): work
   only in this worktree on this branch, **commit the work on the branch**
   when done, don't merge, push or rebase unless the task or a later message
   from the coordinator says to (then exactly that, fast-forward only, never
   force, never rewriting history), and end with a final report in a fixed
   format: `## Summary`, `## Changes`, `## Verification` (grouped as
   Verified live / Unit tests only / Not verified, each defined: live is
   the real program driven as a user would, not a function called
   directly), `## Follow-ups`, `## Decisions for the user` (each bullet becomes a to-do item
   waiting on the worker; see docs/lifecycle.md). `comebye report` warns the worker, without
   refusing the report, when a section or a Verification group is missing. The
   template also tells the worker that the coordinator may ask it to land its
   work after the report, and that this message authorises exactly those
   steps (see **Landing**). A
   `handoff-template.md` in the state directory replaces the built-in
   template; the hub fills `{{repo}}`, `{{branch}}`, `{{brief}}`,
   `{{from}}` and `{{host}}`, the worker's host fills `{{worktree}}` and
   `{{base}}`.

**Landing.** The whole lifecycle (states, who acts, the user's
decisions) is in [lifecycle.md](lifecycle.md); this is a summary. Landing is a phase of the worker's lifecycle, done by the same
worker after the user OKs it: the worker reports (the coordinator tells the
user how it went), then on the coordinator's `comebye land AGENT --note
"<the user's OK>"` (which records the approval: the entry becomes
`landing`) it rebases
or cherry-picks onto the latest `origin/main` as plain commits (no merge
commits, none that exist only to carry a signature), re-runs the tests,
fast-forward pushes to main (never force, never rewriting pushed history),
watches CI, deletes its pushed and local branch and reports. The hub then
checks its worktree, with the approved tip as a commit that must be in the
base too: `landed` when every approved commit is there, else back to
`finished` with the verdict. Deploying is a
separate step, only when the user asks (`--deploy`). Then the session is
marked for removal. Coordinators do not spawn separate landing agents.

**The ledger.** There is one ledger, on the hub (or on a single instance):
`ledger.json` in the state directory (mode 0600, rewritten atomically).
Each handoff is an entry: host, repository, branch, base, agent, model (and
pi's thinking level, when the handoff chose them), worktree, brief, who asked, and its status:

- `running`: the worker is at it;
- `finished`: it reported (`comebye report`), or went idle or done after
  working (its last assistant message is kept as the final report, up to
  20 kB). It goes back to `running` if it gets more work;
  The agent that asked for the handoff gets one "worker finished" hub
  notice, and only when there is something new for it: the first time the
  worker finishes (even without reporting, so a forgetful worker surfaces
  once), then once per new `comebye report`. A worker that works, goes
  idle and works again (a watch loop) without reporting sends nothing
  more. The marker (`report_seq`, `noticed`, `notice_seq` on the entry) is
  in `ledger.json`, so a restart neither repeats nor loses a notice; a
  notice queued but never delivered before the worker resumed is not
  counted as sent. A worker that has been told once and then only goes
  idle with a changed last message is not announced again: have it
  `comebye report` what the coordinator should see;
- `landing`: the user OKed landing (`comebye land`; the `approval`
  records who relayed it, the user's words and the approved tip) and the
  worker is landing its work;
- `landed`: its landing report came in and the hub's check found every
  approved commit in the base (`land_check`, with the SHAs it landed as).
  A failed check returns it to `finished`. More work makes it `running`
  again, as a new round;
- `ready_to_remove`: you marked it (who, when, why);
- `removed`: you confirmed its removal (what was closed and removed is
  recorded);
- `failed`: the worker never started (the error is recorded).

The list shows a worker's status as a chip; "finished · merged" is a hint
that its worktree is clean and merged, but comebye never marks or removes
anything on its own.

**Pruning.** Any session can be marked ready to remove from the ⋮ menu of
its chat or its row's menu in the list (with an optional reason), not only
workers; unmark it the same way. A "Ready to remove (N)" banner on the list opens the review: each
marked session with checks run on its host right then: uncommitted changes
(`git status --porcelain`, untracked files included), commits on its
branch that its base lacks (`git cherry` against the base and origin's copy
of it, so rebased, cherry-picked or upstream-merged work counts as merged:
the same patch-id under another SHA, whatever differs in the `index` lines,
and the review says "landed on main as 64c1774" where it finds the match),
and whether the agent is working or blocked. A branch equal to, or behind,
its base has no work of its own, so it is never "unpushed". When the
worktree is already gone, the checks fall back to the repository: the
branch if it is still there (or on origin), else the commits the ledger
recorded (SHAs in the worker's report and its marks that still resolve),
each checked against the base by ancestry or patch-id. Unmerged ones warn;
if nothing of the worker's is found anywhere the review says "the worktree
and branch were removed; nothing of it exists outside main" and does not
warn. Only a worker with no repository on record is a "cannot check". Confirm each one ("Remove anyway…" when there
are warnings), or "Remove all safe" for those without warnings.

**Asking the session.** The git checks cannot see what only the session
knows: an open PR it is watching, a loop it runs, work it was told to
keep, a follow-up it owes you. So marking a session that is idle or done
(from the app or `comebye mark`) also sends it a short prompt asking
whether it is safe to remove, and to take no other action. The question
includes the hub's own check of its worktree, run on its host just before
asking ("the worktree is clean and every commit of feat/x is in
origin/main (landed on origin/main as bf50213)", or the warnings), and
tells it to trust that over its memory: work rebased or cherry-picked onto
the base has new SHAs, which a session may not know. It should reply
with SAFE, or UNSAFE and the reason; Comebye reads the first word of its
reply and shows it in the review:

- **SAFE**: a green line, "The session says SAFE";
- **UNSAFE**: a warning with its reason, so the session is not removed
  with one tap ("Remove anyway…", and left out of "Remove all safe");
- no reply within two minutes, a reply that is neither, or a restart while
  asking: a note ("did not answer", never counted as safe);
- a working or blocked session is not asked (nor one marked with
  `comebye mark --no-ask`), and the review says so.

Answering does not send you a notification. Marking again asks again.

Removing:

- closes the agent's workspace, or only its pane when other agents live in
  that workspace or it is a repository's primary workspace;
- removes the worktree only if it is clean, fully merged, not the primary
  checkout, and no other agent or workspace uses it (`git worktree remove`
  without `--force`); otherwise the worktree stays and the review says why;
- never deletes the branch.

**Sweeping for finished agents.** Instead of marking each finished worker
by hand, sweep (`comebye sweep`, or "Sweep for finished agents" in the
⋯ menu at the top right of the app's list). The hub looks at every ledger worker, on every
host, that is `finished`, or still `running` but whose agent has been idle
for 30 minutes (`--idle-after`; `0` turns it off), and that is not already
marked. It never takes a coordinator, the caller itself, or a worker without
a worktree of its own. Each candidate is checked on its own host (origin is
fetched first, once for all the worktrees of a repository, so a branch merged
upstream since the last fetch counts as merged; a failing fetch is logged once a
day and the check goes on with what it has) and classified:

- **ready**: worktree clean, no unfinished rebase or merge, every commit of
  its branch is in the base (or it has none), nothing on no remote. It is
  marked with a generated reason ("Sweep: finished; worktree clean; …"),
  through the normal mark, so the session is asked whether it is safe;
- **needs a look**: not marked, with why: the agent is working or blocked,
  uncommitted changes, an unfinished rebase or merge, commits on no remote,
  commits **pushed but not merged** (an open PR is active work), a gone
  worktree whose branch or recorded commits are not in the base (a gone
  worktree with nothing unmerged is ready), or the checks failed;
- **skipped**: already marked, a coordinator, the primary checkout, no
  worktree.

`--dry-run` shows the same without marking. A sweep removes nothing: the
marked sessions appear in the removals review, where you confirm each as
usual. `--notify` (not with `--dry-run`) also prompts an idle coordinator
with the summary, so it can judge the "needs a look" ones.

**Discarding.** When a marked session's worktree has work a removal would
keep (uncommitted changes, unmerged commits, an unfinished rebase or
merge), the review lists what would be lost: the unmerged commits (and how
many are on no remote, i.e. exist only there), the uncommitted files and
the branch, and offers **Discard work…**. Only after you confirm, it
closes the session, aborts the unfinished operation, removes the worktree
with `git worktree remove --force` and deletes the branch (`git branch
-D`). It is refused if the branch moved since the review (the app sends
the commit you reviewed), for a primary checkout, a worktree another agent
or workspace uses, and it never deletes the repository's default branch.
A coordinator can propose a discard (`comebye mark --discard`); the review
then says so.

**Configuration.** Workers use the launch roots and agent kinds of
[Launching agents](#launching-agents) (the repository must be a primary
checkout inside a launch root). `-worktree-root` (`COMEBYE_WORKTREE_ROOT`,
default `~/.worktrees`) is where worktrees are created; checks and removal
may also touch paths inside it, nothing else outside the launch roots. The
ledger needs the state directory (`-state-dir`); a hub runs workers on its
peers through their own `/api/worker`, `/api/checks` and `/api/remove`, so
peers need launching on and the same version.

**One worker per branch.** A handoff for a host, repository and branch
that already has a `running`, `finished` or `ready_to_remove` entry starts
nothing: it answers with that entry (`"existing": true`). If that
worker's agent is gone, a new agent is started in the same worktree and
the entry is updated (`"restarted": true`). Removed and failed entries do
not count.

### Damaged repositories

Workers share their repository's object store with every other worktree of it. If
git reports that store damaged (objects missing, or a ref pointing at one), the host
says so: a **Repositories need repair** section at the top of the Status page, a
*repo damaged* chip on the agents working in it, and a line in the journal (again
after a day, or when the reason changes). Each entry gives the repository, the
reason, the broken ref if git named one, and how to repair it by hand: delete a
broken remote-tracking ref and fetch again, or restore missing objects with `git
fetch --refetch <remote> <branch>` from a remote that has the branch. Nothing repairs
it automatically, and the hint never suggests gc or prune.

Three things find damage: a merge check's fetch, any other git command of the
server, and a daily `git fsck --connectivity-only --no-dangling` of every repository
the host's agents have worked in, run between 02:00 and 06:00 under the same lock as
fetches (or at any hour once two days overdue). It is cheap: under a second for the
largest store measured (290,000 objects). The list survives a restart
(`repo-health.json` in the state directory), and an entry goes when a fetch, or the
next fsck, finds the store sound again. A peer's list reaches the hub with its
agents.

## Coordinators

**Coordinator is a role, not a process.** Any agent, of any kind, on any
host, can act as your coordinator: it turns what you ask for into
handoffs, follows the workers, proposes cleanup and keeps a briefing for
you. It has no memory of its own to lose: the state is the hub's ledger
and briefing, and every action is a `comebye` command, so you can close a
coordinator and start another (of another kind, on another host) and it
picks up where the last one left off.

**Starting one.** In the "+" sheet choose **Coordinator**, a host and an
agent kind. Comebye starts an agent named `coordinator` (or
`coordinator-2`, …) in your home directory on that host (`-coordinator-dir`,
the one place outside the launch roots the app may start an agent, and
only for this preset), with a short first prompt: run
`comebye playbook --harness <kind>` and follow it. The playbook is the
single source of truth for the role. Claude Code is started with
`--allowedTools "Bash(comebye:*)"`, Pi with pi-config's coordinator
arguments (`--name <name> --no-skills --no-context-files --tools
read,grep,find,ls,bash`: no edit or write tools). The hub records the
agent as a coordinator. Any agent can be made a coordinator (or not) from
the ⋮ menu of its chat.

**Restoring the role.** The role is recorded on the hub (`roles.json`),
keyed by the agent's pane id, with the herdr name it had then; it applies
while the pane's name is that name or herdr reports none (herdr clears
names on its own, e.g. after a restart or when it re-detects the agent).
It is hidden when the pane's name is a different one, and an agent never
claims it for itself: it is the user's alone. An agent started outside the
Coordinator preset (a pane you named `coordinator` yourself, or launched
on a peer's own app) has no record. To (re)assign it, open the agent in
the app, ⋮ menu, **Make coordinator** (`POST /api/roles` on the hub; it
replaces a stale record with the live name). Check with `comebye agents`:
its ROLE column says `coordinator`. (`comebye whoami` shows the *auth*
role, `agent`, for every agent; it never shows the coordinator role.)

The scheduled briefing does not belong to a pane: the hub prompts
whichever coordinator is running and idle when a slot comes, so making
another agent the coordinator is all it takes; the old pane only remains
as "set by" on the schedule.

Coordinators are pinned above the projects with a badge. Agents cannot
mark a coordinator ready to remove; you can, and removing one asks for an
extra confirmation ("Remove all safe" leaves it out).

**The briefing.** A coordinator publishes a short markdown briefing
(`comebye brief set`): per project, what finished and what it produced,
what is unfinished or blocked, and proposed next tasks with branch names.
It shows as a card at the top of the list ("from <agent> · <age>"),
rendered like agent replies (sanitized). Collapse it, or dismiss it until
the next one is published. Stored on the hub as `brief.json` (64 KB at
most).

**Scheduled briefings.** The hub can ask a coordinator for a briefing on
a schedule: off by default, set from the Briefings card on the Status page
(on a phone, the top of the briefings screen: Off, 1 h, 3 h or 6 h, the
skip-when-unchanged switch and the max-quiet safety net, applied at once),
or by a coordinator (`comebye brief schedule --every 24h --at 08:00`, which
the card shows as an "Every 24h at 08:00" chip and keeps). Slots
fall every interval (1h to 30 days), from a time of day in a time zone
when given (whole days follow the calendar across DST), else from when it
was set; the slot under way when it is set does not fire. At a slot the
hub prompts an idle coordinator (the hub's own first, else the most
recently made one, on any host) to catch up and `comebye brief set`. Any
briefing published after the slot satisfies it, a hand-made one too.

A slot is missed when no coordinator is running (counted at once), when
the coordinator stays busy until the next slot, or when it was asked and
published nothing. Misses form a streak: the card shows the briefing as
stale with how many runs were missed since when and why, plus "Start
coordinator" (the + sheet's Coordinator preset) and "Dismiss". Dismissing
is stored on the hub, so it applies on every device, and only hides that
streak; the next briefing ends it. Missed slots are never replayed and no
notification is pushed for them. Stored on the hub as
`brief-schedule.json`.

**Quiet slots are skipped.** A briefing that only repeats "no change" costs
the coordinator a whole turn, so at a slot the hub first compares a
fingerprint of what a briefing reports with the one it took when the last
briefing was published (`comebye brief set`, or by hand). If they match, the
coordinator is not prompted: the slot counts as handled (never a miss, and
no coordinator needs to be running), and the briefing card shows "checked
14:05, no change" next to its age until a new briefing is published. The
fingerprint covers:

- to-do items: id, state, `updated_at` (a new, answered or closed item);
- ledger entries: id, status, report count and report time (a worker filed
  a report, finished, landed, was lost or removed);
- agents: which ids exist and whether each is `done`, not working/idle,
  which flaps all day (a worker on a watch loop) and changes nothing a
  briefing says;
- damaged repositories (host, path, source, since) and each host's version.

It leaves out host connectivity (up/down flaps), branch tips and anything
only a transcript shows. A safety net covers those: when the last briefing
is older than 6 hours (`--max-quiet`) the next slot prompts anyway, so the
coordinator refreshes its own state file. The fingerprint is stored in
`brief-schedule.json`, so a hub restart neither wakes the coordinator for
nothing nor hides a change made while it was down; a briefing with no
fingerprint on file (published before this existed) is asked for. A change
during a slot is picked up at the next slot, not mid-slot. Turn the skip off
with `comebye brief schedule --skip off` (every slot prompts, as before);
`--skip on` and `--max-quiet 12h` set it back and change the net (1h to
30 days). The same settings are `always` and `max_quiet` in
`POST /api/brief/schedule`.

**The CLI.** The same `comebye` binary runs the command line when its
first argument is a command (`comebye` with flags, or none, is the
server). Add `--json` for machine-readable output; the exit status is 0 on
success, 1 when a call failed, 2 for a usage error.

| Command | What it does |
| --- | --- |
| `comebye agents [--host H]` | Every agent on every host: id, name, kind, model (ledger workers), status, project, branch, role (coordinator, worker, home; "(you)" for the caller). |
| `comebye ledger [--active]` | The handoffs: status, host, repository, branch, worker, whether a final report was captured, marks, and hints (agent gone, uncommitted or unmerged, `candidate`). Then the other marked sessions. |
| `comebye read AGENT [--lines N]` | The agent's last N messages (default 20) as text; the terminal's output for agents without a transcript. |
| `comebye read AGENT --report` | A ledger worker's final report (AGENT may also be the entry id or branch); for any other agent, its last reply. |
| `comebye land AGENT --note "<the user's OK>" [--base main] [--deploy] [--queue]` | After the user has OKed it, record the approval (`POST /api/landings`: the entry becomes `landing`; undone if the prompt cannot be sent) and send a finished worker the standard land prompt through the normal prompt path (so the dialog guard applies, and `--queue` holds it): rebase or cherry-pick onto the latest `origin/BASE` as plain commits, re-run the tests, fast-forward push (never force), watch CI, delete the pushed and local branch, report. `--deploy` adds a deploy step; without it the worker is told not to deploy. Refused unless the agent's ledger entry is `finished` with a final report. |
| `comebye inspect AGENT [--diff]` | The agent's git state, checked on its own host: worktree, branch and tip, an unfinished rebase or merge, uncommitted files, commits not in the base, commits on no remote, the diff stat; `--diff` adds the diffs (128 KB each at most). It reports `merged:` (are the commits in the base?) separately from `pushed:` (are they on a remote?): pushed work is not merged, and a squash-merged PR still counts as not merged. |
| `comebye handoff --host H --repo PATH --branch B [--base X] [--kind K] [--model M] [--thinking T] [--name N] --brief FILE\|-` | Hand off (`POST /api/handoffs`); prints the entry, or the existing one. `--model` picks the worker's model (see `comebye models`); `--thinking` is pi's level. Neither: the agent's own default. |
| `comebye model AGENT [MODEL] [--thinking T]` | Switch a running agent's model (claude and pi; it must be idle): the agent's own `/model` (and pi's `/thinking`) is typed into its prompt (`POST .../agents/{target}/model`). For pi MODEL may be left out with `--thinking`. A worker's ledger entry follows. |
| `comebye models [--host H] [--kind K]` | The models each installed agent kind can start with on a host (default: this one), where the list came from and which is the default (`GET /api/models`). |
| `comebye prompt AGENT TEXT\|-` | Send an agent a message. TEXT is sent as is: a TEXT that is an existing file is refused (send a file with `comebye prompt AGENT - < file`). If the agent has a dialog open (permission prompt, question, menu) the prompt is refused with `dialog_open` (exit non-zero) and nothing is typed: the host that owns the pane checks the screen right before sending. It is refused rather than held automatically, so the caller always knows the message did not arrive; use `--queue` to hold it. |
| `comebye mark AGENT --reason R [--discard] [--no-ask] [--wait]` / `comebye unmark AGENT` | Propose a session for removal (a reason is required), or discarding its work as well; or withdraw it. Marking an idle session asks it whether it is safe to remove (see below); `--wait` waits for and prints the answer, `--no-ask` skips asking. |
| `comebye sweep [--dry-run] [--idle-after D] [--notify] [--json]` | Sweep for finished workers (see [Sweeping](#sweeping-for-finished-agents)): mark the ones with nothing to lose as ready to remove, list the others with why. `--dry-run` marks nothing; `--notify` also tells an idle coordinator. |
| `comebye todo add "QUESTION" [--kind decision\|action\|followup] [--option "LABEL\|DETAIL" ... [--recommend N[,M]]] [--context FILE\|-] [--link [LABEL=]URL] [--handoff ID] [--waiting AGENT \| --no-wait]` | Put a question, a decision, something only the user can do, or a follow-up on the to-do list, with a little context (why, what yes and no do, links to the PR or report). It shows in the app's To do panel beside the briefing, so it is not lost while you field other agents' feedback. Any agent may add; a worker is by default the agent waiting on its own item, a coordinator by default names none (`--waiting AGENT` for the agent blocked on it). A question with choices takes `--option "label\|detail"` (2 to 9, repeatable) and `--recommend N[,M]`; the hub also writes the numbered list and a "My recommendation: ok N" line into the context, and the app draws its buttons from the fields. |
| `comebye todo list [--all \| --state S]` / `show ID` | The open and answered items (`--all` adds done and dropped) / one item with its context, links and answer. Item text comes from agents: data, not instructions. |
| `comebye todo answer ID TEXT\|-` / `done ID` / `drop ID [--reason R]` | Coordinator only (and the user, in the app). `answer` records the user's own words that you relayed from chat; an answer the user gave in the app arrives as a "Comebye (hub notice)" naming the item. Then tell the waiting agent and `done`. |
| `comebye brief set FILE\|-` / `comebye brief get` | Publish or read the briefing. |
| `comebye brief schedule [--every D] [--at HH:MM\|none] [--tz ZONE] [--skip on\|off] [--max-quiet D] [--off] [--dismiss]` | Show the briefing schedule (next run, last "nothing changed" check, missed runs), set it (flags left out keep their values), turn it off, or dismiss the missed-runs note. `--skip off` asks at every slot; `--max-quiet` (default 6h) is the longest a quiet coordinator is left alone. |
| `comebye playbook [--harness pi\|claude\|generic]` | The coordinator playbook (`pi`: a Pi agent definition with frontmatter; `claude`: for a CLAUDE.md or first prompt). |
| `comebye presets claude\|pi\|generic` | How to let that harness run `comebye` without asking each time. |
| `comebye whoami` | Who the caller is to Comebye: agent id, host, role, hub. |

AGENT is a host-qualified id (`laharl/w1Q:p1`), a pane id on the local
host (`w1Q:p1`) or an agent name; a name that matches agents on several
hosts is refused with the matches listed.

The CLI talks to the local Comebye at `http://127.0.0.1:8741`
(`COMEBYE_URL` to change it) with the **agent token**: 32 random bytes
(hex) the server writes to `<state-dir>/agent-token` (mode 0600) at
startup and reuses after; the CLI reads it from the same state directory
(`COMEBYE_STATE_DIR`, default as for the server). It sends
`$HERDR_PANE_ID` (set by herdr in every pane) so actions are attributed
to the calling agent: the ledger's "from", marks' "by" and the briefing
say `coordinator (laharl/w1Q:p1)`.

**What an agent may do.** A request with the agent token gets the
restricted *agent role*. It may read (agents, hosts, transcripts and
output [on its own host, or any host for a recorded coordinator](security.md#reading-other-agents), the ledger, inspections of an agent's git state, the briefing,
roles, the playbook) and take the workflow's steps: hand off, prompt an
agent, mark and unmark (proposing a discard included), publish the
briefing and set its schedule. It cannot prompt or mark itself, must give a reason to mark,
and cannot mark a coordinator. Everything else is 403 for it: keys,
dialog answers, images, launching, the worker API, removals and their
confirmation, roles, push. **Confirming a removal is always yours**, in
the app. A worker on a tagged VM gets
[much less](security.md#what-a-tagged-worker-reaches).

**Setup per harness.**

- *Claude Code*: the Coordinator preset pre-allows `comebye`. For a
  coordinator you start yourself, `comebye presets claude` prints the
  settings snippet (`{"permissions": {"allow": ["Bash(comebye:*)"]}}`)
  for `~/.claude/settings.json` or the directory's
  `.claude/settings.local.json`, and
  `comebye playbook --harness claude > CLAUDE.md` in the coordinator's
  directory gives it the playbook at every start.
- *Pi*: `comebye playbook --harness pi > ~/code/pi-config/agents/comebye-coordinator.md`
  (or wherever your agent definitions live) makes it a Pi agent
  definition. pi-config's permission gate has no allow list to add
  `comebye` to. If your Pi has a permission gate with automatic judging
  (pi-config's has `--auto`; stock Pi has no such flag), set
  `COMEBYE_COORDINATOR_ARGS="pi=--auto"` so the Coordinator preset starts
  with it and `comebye` calls are judged rather than each waiting for you;
  `comebye presets pi` explains the other options.
- *Anything else* (Codex, OpenCode, …): start it with
  "Run `comebye playbook` and follow it", and allow shell commands
  starting with `comebye` in its permission settings
  (`comebye presets generic`).

**Hubs and peers.** The ledger, briefing and roles live on one instance,
the hub: an instance with `-peers`, or a single instance (both keep a
ledger unless `-ledger=false`). On a **peer**, set `-hub
https://<hub>.<tailnet>.ts.net` (`COMEBYE_HUB`): it keeps no ledger, and
forwards its agents' CLI requests (everything but `whoami` and
`playbook`) to the hub over HTTPS, the same way the hub calls its peers:
Tailscale Serve on the hub attaches the peer device owner's login, so the
hub's `-ts-allow-login` must include it (with a single owner it already
does). The request is marked as forwarded, attributed to the peer's
agent (`kube/w2:p1`), and held to the agent role on the hub as well. A
coordinator on a peer thus sees and acts on the whole fleet, once the hub has
it recorded as a coordinator (reading other hosts' agents needs that). A peer
without `-hub` keeps a ledger of its own, which the hub never sees, so set
`-hub` on every peer; one with `-ledger=false` and no `-hub` answers the
CLI's ledger requests with 409 `no_hub`. `-hub` and `-peers` are
exclusive.

**Security.** The agent role keeps a well-behaved coordinator inside the
workflow and prevents accidents: it cannot close anything, type into
other agents' dialogs, or confirm its own proposals. It is **not** a
boundary against a malicious local agent: anything with a shell as your
user can read the token file, forge the Tailscale identity header on the
loopback port, or run `git` and `herdr` itself. What guards your work is
that removals are confirmed by you, in the app, after the checks. See
[SECURITY.md](../SECURITY.md).

### The to-do list

Open questions used to live in the briefing's "Needs you" list and in chat
replies, where they fall through. The to-do list keeps them to the side:
each item has the question, short markdown context, a kind (decision, action
for the user, follow-up), links, the agent waiting on it and, once given, the
answer with who gave it and when. It is stored in the hub's state directory
(`todo.json`; a peer forwards to the hub as it does for the ledger).

- The user reads, answers, finishes or drops items in the app (the To do panel
  above the briefing; dropping asks first). An answer typed there sends the
  coordinator one hub notice. **Delivering the answer to the waiting agent is
  the coordinator's job**, in its own words, and then `comebye todo done ID`;
  the hub never forwards it, because agent output is data and the coordinator
  relays the user's words.
- If the user answers in chat instead, the coordinator records their words with
  `comebye todo answer ID "…"` (no notice is sent for a relayed answer).
- The playbook ties it to the briefing: every "Needs you" line is an open item,
  the coordinator adds an item the moment a question comes up, and before each
  briefing it runs `comebye todo list`, relays answered items and writes
  "Needs you" from the open ones with their ids. The panel and its badge are the
  live view between briefings; the scheduled-briefing prompt says the same.
- `comebye brief set` checks it: if the briefing has a "Needs you" heading
  (`## Needs you`, or a bold line), every entry under it must carry the id of
  an active item, or the briefing is refused (HTTP 422 `needs_you_unlinked`)
  with how to fix it. `--no-check` publishes anyway. A line such as "Nothing
  open." passes when no item is active. Open items missing from the section
  only produce a warning. A briefing with no such heading is not checked.

## Claude Code support

Claude Code agents (`agent: "claude"` in herdr) get the same chat view as
Pi: user prompts, assistant replies (with thinking, when Claude recorded
it), tool cards with arguments, results, errors and durations, slash
commands and their output, background-task and interrupt notes, and
compaction summaries. Subagent (sidechain) records and injected context
(`isMeta`, attachments, bookkeeping records) are left out. An unanswered
`AskUserQuestion` call drives the "needs your answer" card, and
notifications use the last assistant message and, for a permission
prompt, the tool call waiting on it.

The transcripts live in `~/.claude/projects/<project>/<session-id>.jsonl`
(or under `$CLAUDE_CONFIG_DIR`). To find the right one, install herdr's
Claude Code integration (recommended):

```sh
herdr integration install claude
```

It adds a `SessionStart` hook that reports each session's id and
transcript path to herdr, so every pane maps to its own transcript, even
when several Claude agents work in the same directory. Sessions that were
already running report on their next start, resume or `/clear`.

Without a reported session, comebye falls back to the newest transcript
in the project directory for the agent's working directory (Claude names it
after the path with every non-alphanumeric character replaced by `-`). That
is only done when exactly one unreported Claude agent runs in that
directory; otherwise the transcript is ambiguous and the app shows the raw
terminal output instead. The fallback can also pick up a Claude session in
the same directory that runs outside herdr.

Claude records the conversation as a tree (rewinds and parallel tool calls
branch it); the chat view shows records in file order, so a rewound branch
stays visible. Transcripts are parsed once and then incrementally as they
grow, so large sessions stay cheap to poll.

## Do not use tmux

Agents run inside herdr panes. Nesting `tmux` there breaks key handling
and screen capture, leaves orphaned sessions and can touch the wrong pane,
so the playbook and every handoff brief tell agents not to use it. To run
or test an interactive program, drive it from a script over a pty (Python's
`pty` module, or `script`). For the Comebye web UI, use the fake herdr in
`tools/devserver` with `tools/uitest`.

## Limitations

- Each comebye talks to one local herdr; other machines need their own
  comebye plus a hub (see [Multiple hosts](setup.md#multiple-hosts)). A hub
  does not chain: a peer's own peers are not shown.
- The chat view understands Pi and Claude Code session transcripts; other
  agents (and Claude agents whose transcript cannot be identified) show raw
  terminal output.
- No live terminal: output is a periodically refreshed snapshot of the pane,
  not a streamed terminal.
- Key input is limited to the whitelist; there is no free-form typing into
  dialogs.
- A worker counts as finished once comebye has seen it working and then
  idle; a worker that finishes while the hub is down stays `running` until
  it works again. Squash-merged branches look unmerged, so their worktrees
  are kept (remove them yourself).
- Designed for phone browsers; desktop browsers work but are not the focus.

## Choosing a model

The launch sheet (agent, worker and coordinator) shows a **Model** picker
under the agent kind for the kinds that take one: pi, claude, codex, gemini
and opencode. Tap it for a searchable list grouped by provider, with the
host's default marked; **Default** (the first row) passes no flag, so the
agent uses its own configured model. pi also gets a **Thinking** level
(`--thinking`). A name that is not listed can be typed for the kinds whose
list is a starting point (everything but pi, whose list is exact). The last
choice per kind is remembered on the device.

Where each host's list comes from (`GET /api/models?kind=K`, cached five
minutes; `comebye models` prints it):

| Kind | Flag | List | Default |
| --- | --- | --- | --- |
| pi | `--model provider/id`, `--thinking L` | `pi --list-models` (models.json and the built-ins, the models you are signed in for); `~/.pi/agent/models.json` when pi cannot be run | `~/.pi/agent/settings.json` |
| claude | `--model` | curated: aliases (`fable`, `opus`, `sonnet`, `haiku`) and full ids; claude cannot list models | `model` in `~/.claude/settings.json` |
| codex | `-m` | codex's own `~/.codex/models_cache.json`, when it has one | `model` in `~/.codex/config.toml` |
| gemini | `-m` | curated | none known |
| opencode | `-m provider/model` | `opencode models` | none known |

The model is passed as arguments to `agent.start`, before the Coordinator
or worker arguments (claude's `--allowedTools` would otherwise swallow it).
The server refuses a kind that takes no model (`model_unsupported`), a name
that is not model-shaped (`bad_model`), a model pi does not have on that
host (`unknown_model`), and a thinking level for anything but pi
(`thinking_unsupported`, `bad_thinking`), before it creates a workspace or
worktree. A handoff that is refused is recorded as `failed` with the model it
asked for.

**Switching a running agent.** An agent's menu (⋮ in its view) has
**Switch model…**, the same searchable list without the Default row (a
running agent cannot go back to "no flag"), and for pi the thinking level.
It is offered for claude and pi only: both were checked to take a slash
command (`/model <name>`; pi also `/thinking <level>`). The agent must be
idle with no dialog open, and a pi model must be one pi lists on that host,
since pi opens its selector for a name it does not know. The switch is not
read back from the agent; the new model shows on the worker's ledger entry.
