# Setup

Installing and running Comebye on a host, notifications, several hosts, and every setting.

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

## 1. Configure

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

## 2. Run as a systemd user service

```sh
mkdir -p ~/.config/systemd/user
cp deploy/comebye.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now comebye
journalctl --user -u comebye -f
```

To keep it running while you are logged out, enable lingering:
`loginctl enable-linger "$USER"`.

## 3. Expose it on your tailnet with Tailscale Serve

```sh
tailscale serve --bg 8741
tailscale serve status
```

Open `https://machine.tailnet.ts.net` (your machine's MagicDNS name) on your
phone, then use the browser's share menu (iOS) or "Add to Home screen"
(Android) to install it.

**Never use `tailscale funnel` with this app.** Funnel publishes the port to
the public internet.

## 4. Notifications

**Web Push** is the primary channel and is on by default. Open the app, tap
the bell in the header, and choose **Enable notifications**. Use **Send
test** to check delivery. You are notified when an agent transitions
**into** `blocked` ("needs input", after `-notify-blocked-delay`, 15s by
default, if it is still blocked) or `done` ("is done", or "has a
question" when its last message ends with `?`), at most once per agent and
kind per `-notify-throttle`. Tapping a notification opens that agent's chat
(`/#/agent/<pane>`). No notification is sent for an agent that some device
has open on screen right now.

**Choosing what notifies.** With many agents, "done" adds up. On the
instance that sends notifications (the hub, with several hosts):

- `-notify-kinds` (`COMEBYE_NOTIFY_KINDS`) picks the kinds: `blocked`
  (needs input), `question` (finished on a question), `done` (finished
  otherwise) and `plan` (all workers of a plan finished, or it needs a
  look); default all four, `none` for none.
  `COMEBYE_NOTIFY_KINDS=blocked,question` mutes plain "done" but still
  tells you when an agent finished by asking you something (Comebye reads
  its last message to tell, even with excerpts off).
- `-notify-done-digest` (`COMEBYE_NOTIFY_DONE_DIGEST`), a duration, sends
  "done" as a digest: the first one starts a window of that length,
  and at its end one notification lists every agent that finished in it
  ("3 agents finished", names and repositories, no message text; it opens
  the list). A lone agent gets its usual notification. Agents you opened
  in the meantime, or that went back to work, are left out. The window
  does not slide, so nothing waits longer than the digest; questions are
  never delayed.
- `-notify-blocked-delay` (`COMEBYE_NOTIFY_BLOCKED_DELAY`, default `15s`)
  holds a "needs input" that long and sends nothing if the agent stopped
  being blocked meanwhile (it worked through the block, or you answered
  at the terminal); `0` notifies at once.

Both apply to Web Push and ntfy. After a notification, an agent gets no
more until you open it in the app or it starts working again (or 12 hours
pass), so the noise is many agents finishing, not one repeating.

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

   A peer may list **several addresses**, separated by `|`, tried in order
   with the first that answers used (the hub logs and reports which one):

   ```sh
   COMEBYE_PEERS=laharl=https://laharl-1.tailnet.ts.net|https://100.101.102.103
   ```

   A MagicDNS name plus the peer's tailnet IP survives a rename; two names
   (an old and a new one) work too. One caveat: the TLS certificate
   Tailscale Serve presents is issued for the peer's **name**, not its IP,
   so an IP address can fail the handshake with a certificate error even
   though the peer is fine - prefer names, and treat an IP as a fallback
   that trades the certificate check for reachability (the hub reports
   this case as `tls` with exactly that explanation).
5. Open the **hub's** URL on your phone and enable notifications there.

With more than one host the list shows a small host chip on every agent and
the detail header names the host; a peer the hub cannot reach shows as one
"unreachable: dns (since 3h ago)" row and never affects the other hosts.
The hub health-checks every peer at startup and every few minutes
(GET /api/whoami) and classifies the failure - `dns` (the name does not
resolve; a Tailscale rename?), `tcp` (no connection), `tls` (the handshake
failed; a stale `tailscale serve`?) or `http` (it answered, but rejected
the hub) - so the row, `comebye agents` and the error tooltip say what
broke and since when, with the fix where one is known. A peer is marked
down when its stream misses pings for 45 s or a reconnect fails; the hub
retries with backoff (up to 30 s) and re-baselines statuses on reconnect,
so it does not notify for transitions that happened while the peer was
unreachable. A plain instance without `-peers` behaves exactly as before.

### Re-pointing a peer after a Tailscale rename

When a host re-registers in the tailnet it gets a new name (`laharl` ->
`laharl-1`) and `tailscale serve` keeps serving under its **old** config:
the hub then sees either `dns` (the old name is gone) or `tls` (Serve
answers TLS with an internal error alert because its config names a host
that no longer exists). Nothing on the hub needs to change for the second
case; fix the peer:

```sh
# on the renamed peer, as the user running comebye:
tailscale serve reset && tailscale serve --bg 8741
```

Then update the hub's `COMEBYE_PEERS` to the new name (or add it as a
second `|`-separated address, above, so the next rename does not bite).
The hub picks the peer up again on its next retry (at most 30 s) or its
next health check; no restart needed.

**Trust model:** anyone allowed on the hub can drive every peer, because
the hub acts with its owner's identity. Keep the hub's allowlist as tight as
each peer's.

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
| `-peers` | `COMEBYE_PEERS` | (none) | Makes this instance a hub: comma-separated `name=https://host.tailnet.ts.net` (name optional; several addresses per peer, tried in order, `|`-separated). `https` Serve URLs only; requires `-ts-allow-login`. |
| `-launch` | `COMEBYE_LAUNCH` | `true` | Let the app start new agents (see [Launching agents](agents.md#launching-agents)). |
| `-launch-root` | `COMEBYE_LAUNCH_ROOTS` | `~/code` | Directory inside which agents may be launched (symlinks resolved). Repeatable or comma-separated; roots that do not exist are ignored. |
| `-worktree-root` | `COMEBYE_WORKTREE_ROOT` | `~/.worktrees` | Where workers get their git worktrees (`<root>/<repo>/<branch>`); also allowed for worktree checks and removal. |
| `-hub` | `COMEBYE_HUB` | (none) | On a peer: the hub's `https://` Serve URL. The coordinator CLI's requests are forwarded there and this instance keeps no ledger. Exclusive with `-peers`. See [Coordinators](agents.md#coordinators). |
| `-ledger` | `COMEBYE_LEDGER` | `true` | Keep the handoff ledger, briefing and coordinator roles here (off on a peer with `-hub`). |
| `-coordinator-dir` | `COMEBYE_COORDINATOR_DIR` | `~` | Where the app's Coordinator preset starts a coordinator (may lie outside the launch roots; only that preset uses it). |
| `-coordinator-args` | `COMEBYE_COORDINATOR_ARGS` | none | Extra arguments the Coordinator preset passes per agent kind, `KIND=ARGS` (repeatable or `;`-separated), e.g. `pi=--auto` for an install whose Pi has that extension flag. |
| `-push` | `COMEBYE_PUSH` | `true` | Enable Web Push. Set `false` on peers. |
| `-state-dir` | `COMEBYE_STATE_DIR` | `$XDG_STATE_HOME/comebye`, else `~/.local/state/comebye` | Holds `vapid.pem`, `push-subscriptions.json`, the `agent-token`, and on the hub `ledger.json`, `brief.json` and `roles.json` (dir 0700, files 0600), and an optional `handoff-template.md`. The CLI reads the token from here too. |
| `-push-subject` | `COMEBYE_PUSH_SUBJECT` | `mailto:<first email login>`, else the project URL | VAPID contact, `mailto:` or `https://`; must be a public domain. |
| `-push-excerpt` | `COMEBYE_PUSH_EXCERPT` | `200` | Max characters of the agent's last message in Web Push bodies (end-to-end encrypted); `0` sends none. |
| `-notify-throttle` | `COMEBYE_NOTIFY_THROTTLE` | `30s` | Minimum interval between repeat notifications per agent and kind (both channels). |
| `-notify-kinds` | `COMEBYE_NOTIFY_KINDS` | `blocked,question,done,plan` | Which notifications to send: `blocked`, `question` (finished on a question), `done`, `plan` (every worker of a plan finished, or it needs a look); `none` for none. |
| `-notify-done-digest` | `COMEBYE_NOTIFY_DONE_DIGEST` | `0` | Collect "done" notifications for this long after the first and send one digest; `0` sends each at once. |
| `-notify-blocked-delay` | `COMEBYE_NOTIFY_BLOCKED_DELAY` | `15s` | Wait this long before a "needs input" notification and drop it if the agent is no longer blocked; `0` notifies at once. |
| `-ntfy-url` | `COMEBYE_NTFY_URL` | `https://ntfy.sh` | ntfy server root URL; empty disables ntfy. |
| `-ntfy-topic` | `COMEBYE_NTFY_TOPIC` | (none) | ntfy topic; empty (the default) disables ntfy. |
| `-ntfy-click` | `COMEBYE_NTFY_CLICK` | (none) | URL opened when an ntfy notification is tapped. |
| `-ntfy-excerpt` | `COMEBYE_NTFY_EXCERPT` | `0` | Max characters of the agent's last message in ntfy notifications; `0` sends no message text. |
| `-ntfy-throttle` | | | Deprecated alias for `-notify-throttle`. |

Flags take precedence over environment variables. Run `comebye -h` for
the authoritative list.
