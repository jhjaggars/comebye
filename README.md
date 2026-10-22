# Comebye

A phone-friendly web app for [herdr](https://herdr.dev) coding agents: list
your agents, watch their status live, read their output or chat transcript,
send prompts, and press Esc/Enter/Ctrl+C from your phone, over your
[Tailscale](https://tailscale.com) tailnet.

Comebye is a single Go binary with the web UI embedded. It talks to
herdr's local socket API (`~/.config/herdr/herdr.sock`), so the phone and
your desk terminal act on the **same** agent processes. There is no session
handoff and nothing to sync.

> **Read the [security model](#security-model) before running this.** It is
> effectively a remote shell in a browser.

## Features

- **Agent list with live status**: grouped by project (repository), the
  home agents first and then the workers, each project ordered by status
  and recent activity, with repository, branch and handoff status. Kept current through herdr's
  event stream, relayed to the browser over Server-Sent Events.
- **Chat view** for Pi and Claude Code sessions: the agent's session
  transcript rendered as a native chat, alongside the raw terminal output
  for any agent. Agent replies, thinking and compaction summaries are
  rendered as Markdown (CommonMark + GFM: headings, nested and task lists,
  tables, blockquotes, code blocks, links), sanitized; your own messages
  and tool output stay plain text. See
  [Claude Code support](#claude-code-support).
- **Send prompts**: refused (instead of typed into a dialog) when the agent
  is blocked waiting for input or has an unanswered question open.
- **Keys**: Esc, Enter, Ctrl+C, arrows and a few others, plus digits 1-9
  and y/n for answering dialogs, from a fixed whitelist.
- **Tap to answer**: when an agent is blocked on a dialog, or a session has
  an open question (Pi's `ask_user_question`, Claude Code's
  `AskUserQuestion`), the chat view shows a "Needs your answer" card with
  the question and one big button per option (Pi permission gate and
  selects, Pi questionnaires incl. multi-select, Claude Code permission
  prompts and questions, `[y/N]` confirms). The server re-reads the screen
  before sending the keys and refuses if the dialog changed. "More keys…"
  keeps a key pad and a live screen snapshot for anything it doesn't
  recognize.
- **Push notifications** when an agent becomes `blocked` (needs input) or
  `done`: native Web Push to the installed app (end-to-end encrypted;
  iOS 16.4+ from the Home Screen, Android, desktop), with
  [ntfy](https://ntfy.sh) as an optional fallback. Tapping one opens that
  agent's chat; nothing is sent for the agent you are looking at.
- **Launch new agents**: "+" in the list header picks a host, a directory
  under your launch roots (default `~/code`) and an agent kind, with an
  optional name and first prompt, and opens the new agent's chat. See
  [Launching agents](#launching-agents).
- **Hand work to workers**: start an agent on a new branch in its own git
  worktree with a brief; the hub keeps a ledger of handoffs, notices when
  a worker finishes and keeps its final report. Mark finished sessions
  ready to remove and confirm after safety checks (uncommitted changes,
  unmerged commits). See
  [Projects, workers and handoffs](#projects-workers-and-handoffs).
- **Several machines in one app** (optional): run comebye on each host
  and make one of them a hub that aggregates the others. See
  [Multiple hosts](#multiple-hosts).
- **Installable PWA**: add it to your home screen; it reconnects when
  resumed, and reloads itself after a server deploy (`GET /api/version`,
  also sent as `X-App-Version` on `/api` responses and in `agents` events).

## Requirements

- [herdr](https://herdr.dev) running on the same machine, with its socket
  API available (default `~/.config/herdr/herdr.sock`).
- [Tailscale](https://tailscale.com) on that machine and on your phone, with
  [HTTPS certificates](https://tailscale.com/kb/1153/enabling-https) enabled
  for the tailnet (needed by Tailscale Serve).
- Go 1.25 or newer to build. `go.mod` names a patched toolchain
  (`toolchain` line), which `go` downloads automatically unless you set
  `GOTOOLCHAIN=local`; release binaries should be built with it.
- Linux with systemd is assumed for the service setup below; anything that
  runs Go and herdr should work.

## Install

```sh
go install github.com/jhjaggars/comebye@latest
# or, from a checkout:
go build -o ~/.local/bin/comebye .
```

`go install` puts the binary in `$(go env GOPATH)/bin`; the systemd unit
below expects `~/.local/bin/comebye`, so adjust one or the other.

Quick check (herdr must be running; the server exits if it cannot reach the
socket, and refuses to start without a login allowlist):

```sh
comebye -ts-allow-login you@example.com   # serves http://127.0.0.1:8741
```

## Setup

### 1. Configure

The server reads flags and, for most of them, environment variables. The
systemd unit loads `~/.config/comebye/env` if it exists, which is the
recommended place for settings:

```sh
mkdir -p ~/.config/comebye
umask 077
cat > ~/.config/comebye/env <<'EOF'
COMEBYE_TS_ALLOW_LOGIN=you@example.com
# Web Push contact for Apple/Google/Mozilla (needed if your login is not an
# email address with a public domain, e.g. you@github):
# COMEBYE_PUSH_SUBJECT=mailto:you@example.com
# Optional ntfy fallback:
# COMEBYE_NTFY_TOPIC=herdr-<long-random-string>
# COMEBYE_NTFY_CLICK=https://machine.tailnet.ts.net
EOF
```

Use the Tailscale login that your phone is signed in as (shown by
`tailscale status` or in the admin console).

### 2. Run as a systemd user service

```sh
mkdir -p ~/.config/systemd/user
cp deploy/comebye.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now comebye
journalctl --user -u comebye -f
```

To keep it running while you are logged out, enable lingering:
`loginctl enable-linger "$USER"`.

### 3. Expose it on your tailnet with Tailscale Serve

```sh
tailscale serve --bg 8741
tailscale serve status
```

Open `https://machine.tailnet.ts.net` (your machine's MagicDNS name) on your
phone, then use the browser's share menu (iOS) or "Add to Home screen"
(Android) to install it.

**Never use `tailscale funnel` with this app.** Funnel publishes the port to
the public internet.

### 4. Notifications

**Web Push** is the primary channel and is on by default. Open the app, tap
the bell in the header, and choose **Enable notifications**. Use **Send
test** to check delivery. You are notified when an agent transitions
**into** `blocked` ("needs input") or `done` ("is done", or "has a
question" when its last message ends with `?`), at most once per agent and
kind per `-notify-throttle`. Tapping a notification opens that agent's chat
(`/#/agent/<pane>`). No notification is sent for an agent that some device
has open on screen right now.

- **iPhone / iPad**: needs iOS/iPadOS **16.4 or later**, and only works in
  the app **added to the Home Screen** (Safari: Share, then "Add to Home
  Screen"; open herdr from the Home Screen icon, then enable). A Safari tab
  cannot subscribe; the bell panel says so.
- **Android**: works in Chrome or Firefox, in a tab or installed.
- **Desktop browsers** work too.

How it works: the server keeps a VAPID key pair and the browser
subscriptions in its state directory (`-state-dir`, default
`$XDG_STATE_HOME/comebye` or `~/.local/state/comebye`; files are
mode 0600). It encrypts each notification to the subscribing browser
(RFC 8291), so Apple, Google and Mozilla only relay ciphertext. That is why
Web Push bodies include a short excerpt of the agent's last message by
default (`-push-excerpt 200`; `0` sends none). The server needs outbound
HTTPS to the push services, but nothing has to reach the server: the phone
does not need Tailscale up to receive a notification, only to open the app.
Back up the state directory with the rest of your config; deleting
`vapid.pem` invalidates every subscription (the app re-subscribes when it
next opens, if permission is still granted).

Push services require a contact in the VAPID `sub` claim. The default is
`mailto:` plus the first `-ts-allow-login` that is an email address with a
public domain, otherwise the project URL. Apple rejects contacts such as
`admin@localhost`, `.local` or undotted domains (`403 BadJwtToken`), so if
your Tailscale login is something like `you@github`, set
`-push-subject mailto:you@example.com` (or an `https://` URL you own).

**ntfy (optional fallback)**: install the ntfy app, choose a long random
topic name (anyone who knows it can read your notifications), subscribe to
it, and set `COMEBYE_NTFY_TOPIC`. Set `COMEBYE_NTFY_CLICK` to the
Serve URL so a tap opens the agent (`<ntfy-click>/#/agent/<pane>`). ntfy
notifications carry **no message text** by default, because on public
ntfy.sh anyone who knows the topic can read them and the server caches them
for hours; `-ntfy-excerpt 200` opts in, ideally only with a self-hosted
ntfy (`COMEBYE_NTFY_URL`). On iOS a tap on an ntfy notification opens
Safari rather than the installed app, another reason to prefer Web Push.

## Multiple hosts

To see agents from several machines in one app, run comebye on **every**
host (each talks only to its own herdr socket, exactly as above) and make
one of them the **hub**. The hub subscribes to each peer's event stream over
your tailnet, merges their agents into its own list, proxies opening,
prompting and keys to the owning peer, and sends the notifications for all
of them. The phone only ever talks to the hub.

1. **Install and run comebye on each host** (steps 1-3 above: binary,
   systemd unit, `tailscale serve --bg 8741`). Each host gets its own Serve
   URL, e.g. `https://vm.tailnet.ts.net`.
2. **Allowlist on every host.** Set `COMEBYE_TS_ALLOW_LOGIN` on each peer
   to include the Tailscale login that **owns the hub device** (usually your
   own login, the same one your phone uses). The hub's requests to a peer go
   through the peer's Tailscale Serve, which tags them with that identity,
   so no keys or tokens are needed. The hub device must be a user-owned
   node: a tagged device carries no user identity and every peer rejects it.
3. **Turn off Web Push on the peers**: `COMEBYE_PUSH=false` (and no ntfy
   topic). The hub notifies for every host; only the hub should have push
   subscriptions. A peer with push on never notifies on its own unless a
   browser subscribed on the peer's own URL, but if you did, that device
   gets everything twice (the hub logs a warning when a peer has push on).
4. **Point the hub at its peers** in its env file:

   ```sh
   COMEBYE_TS_ALLOW_LOGIN=you@example.com
   COMEBYE_PEERS=vm=https://vm.tailnet.ts.net,laptop=https://laptop.tailnet.ts.net
   ```

   The name before `=` is optional (it defaults to the first DNS label) and
   appears in the app, in agent IDs (`vm/wS:p1`) and in deep links
   (`/#/agent/vm/wS:p1`). Name the hub itself with `-host-name` if its
   short hostname is not what you want to see. A hub refuses to start
   without `-ts-allow-login`.
5. Open the **hub's** URL on your phone and enable notifications there.

With more than one host the list shows a small host chip on every agent and
the detail header names the host; a peer the hub cannot reach shows as one
"unreachable · last seen 3m ago" row and never affects the other hosts. A
peer is marked down when its stream misses pings for 45 s or a reconnect
fails; the hub retries with backoff (up to 30 s) and re-baselines statuses
on reconnect, so it does not notify for transitions that happened while the
peer was unreachable. A plain instance without `-peers` behaves exactly as
before.

**Trust model:** anyone allowed on the hub can drive every peer, because
the hub acts with its owner's identity. Keep the hub's allowlist as tight as
each peer's.

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
3. sends the first prompt, if any (`agent.prompt`, no wait),

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
are warnings), or "Remove all safe" for those without warnings. Removing:

- closes the agent's workspace, or only its pane when other agents live in
  that workspace or it is a repository's primary workspace;
- removes the worktree only if it is clean, fully merged, not the primary
  checkout, and no other agent or workspace uses it (`git worktree remove`
  without `--force`); otherwise the worktree stays and the review says why;
- **never deletes the branch**.

**Configuration.** Workers use the launch roots and agent kinds of
[Launching agents](#launching-agents) (the repository must be a primary
checkout inside a launch root). `-worktree-root` (`COMEBYE_WORKTREE_ROOT`,
default `~/.worktrees`) is where worktrees are created; checks and removal
may also touch paths inside it, nothing else outside the launch roots. The
ledger needs the state directory (`-state-dir`); a hub runs workers on its
peers through their own `/api/worker`, `/api/checks` and `/api/remove`, so
peers need launching on and the same version.

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

## Upgrading from herdr-phone

Comebye was called herdr-phone. An existing install keeps working after the
rename:

- `HERDRPHONE_*` environment variables are still read (each logs a
  deprecation note once); rename them to `COMEBYE_*`.
- On first start with the default state directory, `~/.local/state/herdr-phone`
  is moved to `~/.local/state/comebye`, so the VAPID key and push
  subscriptions carry over and phones keep their notifications.
- The unit reads `~/.config/herdr-phone/env` as well as
  `~/.config/comebye/env`. To switch over:

```sh
mkdir -p ~/.config/comebye && mv ~/.config/herdr-phone/env ~/.config/comebye/env
systemctl --user disable --now herdr-phone
cp deploy/comebye.service ~/.config/systemd/user/ && systemctl --user daemon-reload
systemctl --user enable --now comebye
```

The URL (your Tailscale Serve name) does not change, so an installed home
screen app keeps working; iOS shows the new name only after it is added
to the home screen again.

## Configuration

| Flag | Environment | Default | Description |
| --- | --- | --- | --- |
| `-addr` | | `127.0.0.1:8741` | Listen address. Keep it on loopback and use Tailscale Serve for TLS. |
| `-sock` | | `~/.config/herdr/herdr.sock` | herdr API socket path. |
| `-ts-allow-login` | `COMEBYE_TS_ALLOW_LOGIN` | (none) | Comma-separated Tailscale logins admitted via Tailscale Serve identity headers. Requires a loopback `-addr`. Required unless `-insecure-no-auth`. |
| `-insecure-no-auth` | | `false` | Start without `-ts-allow-login`, serving an unauthenticated API to anyone who reaches the port (your whole tailnet through Serve). For local testing only. |
| `-allow-host` | `COMEBYE_ALLOW_HOSTS` | (none) | Extra `Host` names the server answers to, besides `localhost`, loopback IPs and `*.ts.net`. Repeatable or comma-separated. Other hosts get 421 (DNS rebinding protection). |
| `-host-name` | `COMEBYE_HOST_NAME` | short hostname | This host's name in agent IDs (`<host>/<pane>`), URLs and the UI. Lowercase DNS label. |
| `-peers` | `COMEBYE_PEERS` | (none) | Makes this instance a hub: comma-separated `name=https://host.tailnet.ts.net` (name optional). `https` Serve URLs only; requires `-ts-allow-login`. |
| `-launch` | `COMEBYE_LAUNCH` | `true` | Let the app start new agents (see [Launching agents](#launching-agents)). |
| `-launch-root` | `COMEBYE_LAUNCH_ROOTS` | `~/code` | Directory inside which agents may be launched (symlinks resolved). Repeatable or comma-separated; roots that do not exist are ignored. |
| `-worktree-root` | `COMEBYE_WORKTREE_ROOT` | `~/.worktrees` | Where workers get their git worktrees (`<root>/<repo>/<branch>`); also allowed for worktree checks and removal. |
| `-push` | `COMEBYE_PUSH` | `true` | Enable Web Push. Set `false` on peers. |
| `-state-dir` | `COMEBYE_STATE_DIR` | `$XDG_STATE_HOME/comebye`, else `~/.local/state/comebye` | Holds `vapid.pem`, `push-subscriptions.json` and the handoff `ledger.json` (dir 0700, files 0600), and an optional `handoff-template.md`. |
| `-push-subject` | `COMEBYE_PUSH_SUBJECT` | `mailto:<first email login>`, else the project URL | VAPID contact, `mailto:` or `https://`; must be a public domain. |
| `-push-excerpt` | `COMEBYE_PUSH_EXCERPT` | `200` | Max characters of the agent's last message in Web Push bodies (end-to-end encrypted); `0` sends none. |
| `-notify-throttle` | | `30s` | Minimum interval between repeat notifications per agent and kind (both channels). |
| `-ntfy-url` | `COMEBYE_NTFY_URL` | `https://ntfy.sh` | ntfy server root URL; empty disables ntfy. |
| `-ntfy-topic` | `COMEBYE_NTFY_TOPIC` | (none) | ntfy topic; empty (the default) disables ntfy. |
| `-ntfy-click` | `COMEBYE_NTFY_CLICK` | (none) | URL opened when an ntfy notification is tapped. |
| `-ntfy-excerpt` | `COMEBYE_NTFY_EXCERPT` | `0` | Max characters of the agent's last message in ntfy notifications; `0` sends no message text. |
| `-ntfy-throttle` | | | Deprecated alias for `-notify-throttle`. |

Flags take precedence over environment variables. Run `comebye -h` for
the authoritative list.

## Security model

**This app is a remote shell in a browser.** Sending a prompt to a coding
agent is, in practice, arbitrary command execution on the host as your user.
Treat access to it like SSH access to that machine.

- **Loopback only.** The server listens on `127.0.0.1`. The only intended
  path from your phone is Tailscale Serve, which terminates HTTPS and is
  reachable only from inside your tailnet.
- **Identity allowlist.** Tailscale Serve adds a `Tailscale-User-Login`
  header to every request it proxies from a tailnet device and strips any
  client-supplied copy. With `-ts-allow-login`, only the listed logins get
  through; other tailnet users, tagged devices (which carry no user
  identity), and Funnel traffic are rejected with 401, and the app shows a
  "Not authorized" message. Matching is case-insensitive. The server
  refuses to start with an allowlist unless it listens on loopback, since
  any other client could forge the header. There are no passwords, tokens
  or login endpoints; static assets (the UI shell) are served without auth
  and every `/api/*` call requires an allowed login.
- **Cross-site requests are rejected.** The identity header is ambient,
  like a cookie, so the server wraps everything in Go's
  `http.CrossOriginProtection` (checks `Sec-Fetch-Site`/`Origin`) and
  requires `Content-Type: application/json` on POSTs. A web page the phone
  visits cannot drive your agents through Serve.
- **`-ts-allow-login` is required.** Without it the API would be
  unauthenticated and every device on your tailnet could drive your agents,
  so the server refuses to start unless you also pass `-insecure-no-auth`
  (local testing only; it logs a warning).
- **DNS rebinding is blocked.** Every request (UI and API) must be
  addressed to `localhost`, a loopback IP, a `*.ts.net` name (what
  Tailscale Serve forwards) or a name listed with `-allow-host`; anything
  else gets 421. Otherwise a web page in a browser on the host could
  re-resolve its own domain to `127.0.0.1` and, being same-origin with the
  loopback port, drive your agents with a forged identity header.
- **Local users are trusted.** Any process on the same host can connect to
  the loopback port and forge the identity header. Do not run this on a
  shared multi-user machine.
- **Never use Tailscale Funnel**, and do not put another reverse proxy in
  front of it.
- **Key input is whitelisted** (esc, enter, arrows, ctrl+c, and similar,
  plus the single characters 1-9, y and n for answering dialogs). No other
  printable characters; free text goes through the prompt endpoint, which
  herdr refuses while the agent is blocked (and the app holds back while a
  transcript question is pending).
- **Push endpoints are restricted.** The server POSTs to whatever endpoint
  a browser subscribed with, so endpoints must be `https` on the default
  port at a known push service (FCM, Mozilla, Apple, WNS), and the
  outbound dialer refuses private, loopback, link-local and CGNAT
  (tailnet) addresses after DNS resolution, with no redirects. Push
  payloads are encrypted end to end; logs show only the push service
  origin, never the endpoint path.
- **A hub is not an open proxy.** It only contacts the peers named in
  `-peers` (https only, no redirects followed, no proxy from the
  environment), and only for the per-agent calls on
  `/api/agents/<target>/…` (`output`, `transcript`, `prompt`, `keys`,
  `dialog`, `answer`, `image`) and the per-host launch calls on
  `/api/launch/…`, with a fixed set of query parameters, size-capped
  bodies and responses, and timeouts. An
  unknown `{host}` is a 404. None of the browser's headers are forwarded.
  The hub's own allowlist, cross-origin protection and JSON-only POST rule
  apply before anything is proxied, and every peer applies its own again.
  Anyone allowed on the hub can drive every peer.
- **Launching is confined to the launch roots.** Directory listings and
  launches resolve the path (symlinks included) and refuse anything
  outside `-launch-root` with 403, without saying whether it exists;
  relative paths are refused. Agent kinds are limited to the ones herdr
  supports and that are installed (or running) on the host, and agent
  names to herdr's `[a-z][a-z0-9_-]{0,31}`. `-launch=false` turns it off.
- Static files are served with a restrictive Content-Security-Policy
  (same-origin scripts only, no inline script, no remote images),
  `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`.
- **Agent text is untrusted.** Markdown from transcripts is parsed with
  [marked](https://github.com/markedjs/marked) and the resulting HTML is
  always passed through [DOMPurify](https://github.com/cure53/DOMPurify)
  with a small allowlist: no script, style, iframe, form, image or SVG
  elements, no event-handler, style or id attributes, only `http(s):` and
  `mailto:` links (opened with `rel="noopener noreferrer"`), and only
  read-only task-list checkboxes. Raw HTML tags outside a short list of
  formatting tags are shown as text; images are shown as links. Both
  libraries are vendored in `web/vendor/` at pinned versions, copied byte
  for byte from the npm tarballs with their hashes recorded in
  `web/vendor/README.md` (checked by `go test`), and served from the app's
  own origin; nothing is loaded from a CDN.

See [SECURITY.md](SECURITY.md) for the threat model and how to report a
vulnerability.

## Limitations

- Each comebye talks to one local herdr; other machines need their own
  comebye plus a hub (see [Multiple hosts](#multiple-hosts)). A hub
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

## API

The UI is built on a small JSON API, usable from scripts on the tailnet:

| Endpoint | Description |
| --- | --- |
| `GET /api/agents` | Current agent list (every host's on a hub; each agent has `host` and `id` = `<host>/<pane_id>`) and `hosts` (`name`, `local`, `status` `ok`/`connecting`/`down`, `error`, `last_seen`, `agents`). |
| `GET /api/hosts` | Just the `hosts` status list. |
| `GET/POST /api/hosts/{host}/agents/{target}/{output,transcript,prompt,keys,dialog,answer}` | Same as the routes below for a given host: the local host is served directly, a peer's agent is proxied to that peer (status and JSON errors pass through). |
| `GET /api/agents/{target}/output?lines=N` | Recent pane output; `&source=visible` reads the current screen instead (used for dialog snapshots). |
| `GET /api/agents/{target}/transcript?limit=N&skip=M` | Parsed session transcript (Pi or Claude Code; `format` says which), including any pending `ask` question. 404 `no_session` when there is none. |
| `POST /api/agents/{target}/prompt` | Body `{"text": "...", "wait": bool, "timeout_ms": int}`. |
| `POST /api/agents/{target}/keys` | Body `{"keys": ["esc"]}` (whitelisted). |
| `GET /api/agents/{target}/dialog` | `{"dialog": {...} \| null}`: the dialog the agent is waiting on (`kind`, `title`, `question`, `detail`, `options[]` with `label`/`description`/`checked`/`role`, `selected`, `multi`, `answerable`, `cancel_label`, `fingerprint`). Only while the agent is `blocked` or has a pending question while `working`. |
| `POST /api/agents/{target}/answer` | Body `{"action": "select"\|"toggle"\|"submit"\|"cancel", "index": N, "fingerprint": "..."}`. Re-detects the dialog; 409 `dialog_changed` (with the fresh `dialog`), `no_dialog` or `duplicate` (same dialog answered in the last 3 s), else sends the keys and returns `{"ok": true, "keys": [...]}`. |
| `GET /api/launch/options` | `{"enabled", "reason", "roots": [{path, label, git}], "kinds": [...], "default_kind", "recent": [{path, label, git}]}` for this host. |
| `GET /api/launch/dirs?path=<abs>` | Subdirectories of a directory inside a launch root: `{path, label, parent, entries: [{name, path, git}], truncated}` (at most 300; hidden ones left out). Without `path`, the roots. 403 outside the roots, 404 missing. |
| `POST /api/launch` | Body `{"dir": "<abs>", "kind": "pi", "name": "...", "prompt": "..."}` (`kind` defaults to `default_kind`, `name` to the directory's). Returns `{"agent": {id, host, pane_id, name, agent, ...}, "workspace_id"}`, plus `prompt_error` if the agent started but the prompt failed. Errors: `bad_kind`, `bad_name`, `agent_name_taken` (409), `outside_roots` (403), `launch_disabled` (403), or herdr's code with the workspace left open. |
| `GET/POST /api/hosts/{host}/launch[/options,/dirs]` | The same for a given host (proxied to a peer by a hub). |
| `GET /api/repos` | The primary git checkouts in this host's launch roots (a root, its children and grandchildren): `{"repos": [{name, path, label, default_branch}], "truncated"}`. Also `/api/hosts/{host}/repos`. |
| `GET /api/checks?path=<abs>&branch=&base=` | A worktree's safety checks on this host: `{path, exists, primary, branch, dirty, dirty_files, unmerged_commits, base_used, repo, error}` (`unmerged_commits` -1 when unknown). The path must be inside a launch root or the worktree root. Also `/api/hosts/{host}/checks`. |
| `POST /api/worker` | Starts a worker on this host: body `{"repo", "branch", "base", "kind", "name", "prompt"}` (`{{worktree}}` and `{{base}}` in the prompt are filled in). Returns `{agent, workspace_id, worktree, branch, base, repo, reused}`. Errors: `bad_branch`, `bad_base`, `not_a_repo`, `branch_in_primary`, `path_exists`, `no_free_pane` (409), plus the launch errors. The hub calls a peer's; browsers use `/api/handoffs`. |
| `POST /api/remove` | Removes a session on this host: body `{"agent": "<pane>", "name", "workspace_id", "worktree", "branch", "base", "remove_worktree": bool, "allow_working": bool}`. Returns `{closed: "workspace"\|"pane"\|"", worktree_removed, kept, checks}`; 409 `agent_working` or `agent_changed` closes nothing. |
| `GET /api/ledger` | The hub's handoffs (`entries`, each with `live` status, `checks` and a `candidate` hint; only the latest 20 removed or failed) and standalone `marks`. 503 `ledger_disabled` without a state directory. |
| `POST /api/handoffs` | Body `{"host", "repo", "branch", "base", "kind", "name", "brief", "from"}`: runs the worker on that host and records it. Returns `{entry, worker}`; a failure returns the error with the recorded `failed` entry. |
| `POST /api/marks`, `POST /api/marks/clear` | Body `{"agent_id": "<host>/<pane>", "reason", "by"}`: mark a session ready to remove, or unmark it. |
| `GET /api/removals` | The marked sessions, each with fresh `checks` from its host, `warnings`, `notes` and `safe`. |
| `POST /api/removals/confirm` | Body `{"ids": ["<host>/<pane>", ...], "allow_working": bool}`: removes each on its host; `{"results": [{id, ok, code, message, result}]}`. |
| `GET /api/events` | SSE stream of `agents` snapshots (with `hosts`), `resync` signals and a `ping` every 15 s. |
| `GET /api/push/key` | `{"enabled": bool, "key": "<VAPID public key, base64url>"}`. |
| `POST /api/push/subscribe` | Body: `PushSubscription.toJSON()` (`endpoint`, `keys.p256dh`, `keys.auth`), optional `replaces`. Stored with the caller's Tailscale login. |
| `POST /api/push/unsubscribe` | Body `{"endpoint": "..."}` (own subscriptions only). |
| `POST /api/push/test` | Body `{"endpoint": "..."}` or `{}`: sends a test notification to the caller's subscriptions. |
| `POST /api/push/presence` | Body `{"client": "<id>", "pane": "<agent id>", "visible": bool}`: which agent this app instance shows (its `id`, `<host>/<pane>`; a bare pane ID means a local agent); suppresses notifications for it for 45 s unless refreshed. |

POSTs must send `Content-Type: application/json` (see the security model).
`{target}` is a pane ID (for example `wS:p1`) or a live agent name; the
`/api/agents/{target}/…` routes always mean the local host. With
`"wait": true`, herdr starts a status wait in the same `agent.prompt` call,
so "send this and tell me when it's done" has no race.

## Development

```sh
go build ./... && go vet ./... && go test ./...
gofmt -l .
node --check web/app.js web/sw.js
# markdown rendering + sanitization in headless Chrome (Node 22+), against
# a running instance; --agent/--screenshot also check a chat at 360px:
node web/test/markdown.test.mjs http://127.0.0.1:8741/
```

The web UI is plain HTML/CSS/JS in `web/` (no build step), embedded into
the binary with `go:embed`. The only third-party code is the two vendored
browser libraries in `web/vendor/` (see its README for versions, hashes,
licenses and how to update them); the Go module has no dependencies. `go run tools/genicon/main.go` regenerates the
PWA icons.

Notes on herdr's socket protocol:

- The socket answers **one NDJSON request per connection** (then closes),
  except `events.subscribe`, which turns the connection into a long-lived
  push stream. Every API call dials a fresh connection, and exactly one
  subscription connection is kept alive.
- `pane.agent_status_changed` subscriptions need a concrete `pane_id` and
  the subscription set is fixed per connection, so the server resubscribes
  (new connection) whenever the set of panes changes.
- If the server falls behind, herdr sends `events_lost` and closes the
  stream; the server reconnects with backoff, re-reads state, and tells
  browsers to resync.
- Event bursts are debounced into a single `agent.list` refresh; a 30s poll
  is the safety net.
- `herdr api schema --json` prints the full JSON Schema for the protocol.

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines.

## License

Copyright 2026 Jesse Jaggars. Licensed under the
[Apache License, Version 2.0](LICENSE).

Bundled third-party code in `web/vendor/`: marked (MIT) and DOMPurify
(Apache-2.0, dual-licensed with MPL-2.0); their license texts are next to
them and are served with the app.
