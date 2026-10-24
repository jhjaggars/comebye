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
   when done, don't merge or push other branches (merging stays with you),
   and end with a final report in a fixed format: `## Summary`,
   `## Changes`, `## Verification`, `## Follow-ups`. A
   `handoff-template.md` in the state directory replaces the built-in
   template; the hub fills `{{repo}}`, `{{branch}}`, `{{brief}}`,
   `{{from}}` and `{{host}}`, the worker's host fills `{{worktree}}` and
   `{{base}}`.

**The ledger.** There is one ledger, on the hub (or on a single instance):
`ledger.json` in the state directory (mode 0600, rewritten atomically).
Each handoff is an entry: host, repository, branch, base, agent, worktree,
brief, who asked, and its status:

- `running`: the worker is at it;
- `finished`: it went idle or done after working; its last assistant
  message is kept as the final report (up to 20 kB). It goes back to
  `running` if it gets more work;
- `ready_to_remove`: you marked it (who, when, why);
- `removed`: you confirmed its removal (what was closed and removed is
  recorded);
- `failed`: the worker never started (the error is recorded).

The list shows a worker's status as a chip; "finished · merged" is a hint
that its worktree is clean and merged, but comebye never marks or removes
anything on its own.

**Pruning.** Any session can be marked ready to remove from the ⋮ menu of
its chat (with an optional reason), not only workers; unmark it the same
way. A "Ready to remove (N)" banner on the list opens the review: each
marked session with checks run on its host right then: uncommitted changes
(`git status --porcelain`, untracked files included), commits on its
branch that its base lacks (`git cherry` against the base and origin's copy
of it, so rebased or upstream-merged work counts as merged), and whether the
agent is working or blocked. Confirm each one ("Remove anyway…" when there
are warnings), or "Remove all safe" for those without warnings.

**Asking the session.** The git checks cannot see what only the session
knows: an open PR it is watching, a loop it runs, work it was told to
keep, a follow-up it owes you. So marking a session that is idle or done
(from the app or `comebye mark`) also sends it a short prompt asking
whether it is safe to remove, and to take no other action. It should reply
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
a schedule: off by default, set from the briefing card or a
coordinator's ⋮ menu ("Briefing schedule…", e.g. `24h at 08:00`), or by
a coordinator (`comebye brief schedule --every 24h --at 08:00`). Slots
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

**The CLI.** The same `comebye` binary runs the command line when its
first argument is a command (`comebye` with flags, or none, is the
server). Add `--json` for machine-readable output; the exit status is 0 on
success, 1 when a call failed, 2 for a usage error.

| Command | What it does |
| --- | --- |
| `comebye agents [--host H]` | Every agent on every host: id, name, kind, status, project, branch, role (coordinator, worker, home; "(you)" for the caller). |
| `comebye ledger [--active]` | The handoffs: status, host, repository, branch, worker, whether a final report was captured, marks, and hints (agent gone, uncommitted or unmerged, `candidate`). Then the other marked sessions. |
| `comebye read AGENT [--lines N]` | The agent's last N messages (default 20) as text; the terminal's output for agents without a transcript. |
| `comebye read AGENT --report` | A ledger worker's final report (AGENT may also be the entry id or branch); for any other agent, its last reply. |
| `comebye inspect AGENT [--diff]` | The agent's git state, checked on its own host: worktree, branch and tip, an unfinished rebase or merge, uncommitted files, commits not in the base, commits on no remote, the diff stat; `--diff` adds the diffs (128 KB each at most). It reports `merged:` (are the commits in the base?) separately from `pushed:` (are they on a remote?): pushed work is not merged, and a squash-merged PR still counts as not merged. |
| `comebye handoff --host H --repo PATH --branch B [--base X] [--kind K] [--name N] --brief FILE\|-` | Hand off (`POST /api/handoffs`); prints the entry, or the existing one. |
| `comebye prompt AGENT TEXT\|-` | Send an agent a message. |
| `comebye mark AGENT --reason R [--discard] [--no-ask] [--wait]` / `comebye unmark AGENT` | Propose a session for removal (a reason is required), or discarding its work as well; or withdraw it. Marking an idle session asks it whether it is safe to remove (see below); `--wait` waits for and prints the answer, `--no-ask` skips asking. |
| `comebye brief set FILE\|-` / `comebye brief get` | Publish or read the briefing. |
| `comebye brief schedule [--every D] [--at HH:MM\|none] [--tz ZONE] [--off] [--dismiss]` | Show the briefing schedule (next run, missed runs), set it (flags left out keep their values), turn it off, or dismiss the missed-runs note. |
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
output, the ledger, inspections of an agent's git state, the briefing,
roles, the playbook) and take the workflow's steps: hand off, prompt an
agent, mark and unmark (proposing a discard included), publish the
briefing and set its schedule. It cannot prompt or mark itself, must give a reason to mark,
and cannot mark a coordinator. Everything else is 403 for it: keys,
dialog answers, images, launching, the worker API, removals and their
confirmation, roles, push. **Confirming a removal is always yours**, in
the app.

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
coordinator on a peer thus sees and acts on the whole fleet. A peer
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
