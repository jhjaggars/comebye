# Setup

Installing and running Comebye on a host, notifications, several hosts, and every setting.

## Requirements

- [herdr](https://herdr.dev) running on the same machine, with its socket
  API available (default `~/.config/herdr/herdr.sock`; set `-sock` or
  `COMEBYE_SOCK` for another location).
- [Tailscale](https://tailscale.com) on that machine and on your phone, with
  [HTTPS certificates](https://tailscale.com/kb/1153/enabling-https) enabled
  for the tailnet (needed by Tailscale Serve).
- Go 1.25 or newer, only to build from source. `go.mod` names a patched
  toolchain (`toolchain` line), which `go` downloads automatically unless you set
  `GOTOOLCHAIN=local`; release binaries should be built with it.
- Linux with systemd is assumed for the service setup below; anything that
  runs Go and herdr should work.

## Install

Each [release](https://github.com/jhjaggars/comebye/releases) has static
binaries for Linux and macOS (amd64, arm64), packed with the systemd unit,
and a `SHA256SUMS` file:

```sh
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
mkdir -p ~/.local/bin
curl -fsSL "https://github.com/jhjaggars/comebye/releases/latest/download/comebye_${os}_${arch}.tar.gz" \
  | tar -xz --strip-components=1 -C ~/.local/bin "comebye_${os}_${arch}/comebye"
comebye -version
```

Or build from source:

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
4. **Give the hub its peers**, either live (next section) or, for a fixed
   setup, in its env file:

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

### Adding and removing peers live

Peers do not have to be in the env file. Start the hub with
`COMEBYE_HUB_MODE=1` (alongside `COMEBYE_TS_ALLOW_LOGIN`; `COMEBYE_PEERS` is
then optional) and manage peers while it runs, with no restart:

```sh
comebye peer add vm https://vm.tailnet.ts.net        # several: URL1|URL2
comebye peer add w1 https://w1.tailnet.ts.net --tag tag:worker
comebye peer list                                     # status, agents, version
comebye peer remove vm
```

A new peer is connected and health-checked at once and its agents show up
in the app and in `comebye agents`; a removed one disappears the same way
(requests in flight to it fail cleanly). Run these **on the hub host**, as
the user: `comebye peer` acts with a Tailscale login (`--login`, or
`COMEBYE_LOGIN`, or the first of `COMEBYE_TS_ALLOW_LOGIN` when your shell has
it) rather than the agent token, because the registry is the user's alone -
an agent's token gets 403. The same operations are `GET/POST /api/peers` and
`DELETE /api/peers/{name}` ([API](api.md#peers)).

- Peers added live are stored in `peers.json` in the state directory
  (written atomically, mode 0600) and merged with `COMEBYE_PEERS` at
  startup. A peer set in `COMEBYE_PEERS` is listed as `static`; it cannot be
  removed through the API (the error says so: edit the env file and restart)
  and a live peer may not reuse its name.
- **Addresses added live must be tailnet hosts:** `https` URLs under
  `*.ts.net` (or the domains in `COMEBYE_PEER_DOMAINS`, comma-separated, for
  a custom tailnet domain), and a tailnet IP (100.64.0.0/10,
  fd7a:115c:a1e0::/48) only as a fallback address next to a name. A peer is
  sent the hub's requests (agents' prompts, transcript reads) and is trusted
  with the agents it reports, so the API will not point the hub at an
  arbitrary host. The static `COMEBYE_PEERS` is not restricted: it is set by
  whoever controls the hub's environment.
- Names follow the same rule as host names (a lowercase DNS label). Each add
  and remove is logged with who did it.
- `comebye peer list` and `comebye agents` show each host's **version**
  (read from the `X-Comebye-Version` header it sends; a peer too old to send
  it shows as `unknown`). When a peer runs a different version than the hub,
  `comebye agents` prints a warning line and the app shows a small badge on
  that host's chip: upgrade the one that is behind
  (`comebye upgrade` on that host).

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
unreachable. A plain instance without `-peers` or `-hub-mode` behaves
exactly as before.

### Tagged workers (`tag:worker` VMs)

A VM that joins the tailnet with an ACL tag (`tag:worker`, e.g. from an auth
key created for the tag) has **no user login**, so Tailscale Serve sends no
`Tailscale-User-Login` for it and the hub's `-ts-allow-login` check rejects
it: its workers could not `comebye report`. A tagged node therefore talks to
the hub over a separate **peer listener**, where the hub identifies the
caller with tailscaled's WhoIs on the connection's tailnet source address.

On the hub (off unless you set these):

```sh
COMEBYE_HUB_MODE=1
COMEBYE_TS_ALLOW_TAGS=tag:worker     # tags admitted on the peer listener
COMEBYE_PEER_LISTEN=8742             # a port; binds to the hub's tailnet IP only
COMEBYE_PEER_SELF_REGISTER=1         # optional: let workers register themselves
# COMEBYE_TS_SOCKET=/var/run/tailscale/tailscaled.sock   # LocalAPI, if elsewhere
```

On the tagged worker VM:

```sh
COMEBYE_HUB=http://hub.tailnet.ts.net:8742   # the peer listener, not the Serve URL
COMEBYE_ANNOUNCE=1                           # register with the hub at startup
# COMEBYE_ANNOUNCE_URL=https://w1.tailnet.ts.net   # default: https://<this node's name>
```

(The worker also serves itself with `tailscale serve --bg 8741` as any peer
does; the hub reaches it through that URL as usual.)

- **What a tagged node can do:** exactly what a peer's forwarded agent can:
  the agent-role requests of its workers (`comebye report`, `handoff`,
  `prompt`, `mark`, the ledger and briefing reads - [Coordinators](agents.md#coordinators)),
  attributed to the peer's name. **What it can't:** anything the user alone
  can do (keys, answers, launching, removals, push, the peer registry), the
  web app, or anything as the user. The listener serves `/api/*` only.
- **Two conditions, both checked on every request:** the node carries an
  allowed tag, and it is a **registered peer** - one of the peer's addresses
  is the node itself (its MagicDNS name). A tagged node that is not
  registered gets 403 `not_a_peer`. A peer added with `--tag` must still
  carry that tag.
- **Registering:** either the user runs `comebye peer add w1
  https://w1.tailnet.ts.net --tag tag:worker` on the hub, or, with
  `COMEBYE_PEER_SELF_REGISTER=1`, the worker announces itself
  (`COMEBYE_ANNOUNCE=1`; retried until the hub answers). The announced
  address must be the caller's own node, so a worker cannot register another
  machine. Announcing again from the same node changes nothing; a name held
  by another node, or a second name for a registered node, is refused (409).
  The hub's usual rule applies: `https` under `*.ts.net`. Each registration
  is logged and recorded with `added_by` `tag:worker@<node>` and the tag.
- **Removal is the user's alone** (`comebye peer remove`); a tagged node can
  not unregister itself or anyone else.
- **Trade-offs:** the peer listener is plain HTTP (the hop is encrypted and
  authenticated by WireGuard). It listens only on the hub's tailnet address;
  tailscaled must be reachable for WhoIs (the listener waits for it at
  start). **Restrict it in your tailnet ACLs** to the tagged workers (and
  your own devices if you want): see
  [Security](security.md#tagged-nodes-and-the-peer-listener). Self-registering
  gives the node what any peer has: the hub sends it prompts and trusts its
  agent list, so only tag machines you trust.

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
second `|`-separated address, above, so the next rename does not bite). A
peer added with `comebye peer add` is simply removed and added again under
its new name.
The hub picks the peer up again on its next retry (at most 30 s) or its
next health check; no restart needed.

**Trust model:** anyone allowed on the hub can drive every peer, because
the hub acts with its owner's identity. Keep the hub's allowlist as tight as
each peer's.

## Upgrading

`comebye upgrade` replaces the installed binary and restarts the user
service, on this host only. Run it on each host (the peer first, then the
hub): it deliberately does not reach into other hosts, so it needs no extra
trust.

```sh
comebye upgrade --dry-run          # download + verify the latest release, change nothing
comebye upgrade                    # install the latest release
comebye upgrade --version v0.2.0   # a specific release
comebye upgrade --from-source --ref origin/main   # unreleased: build in a temp worktree of the current checkout (runs go vet + go test)
comebye upgrade --rollback         # put comebye.prev back
```

What it does:

1. Reads `comebye -version` of `~/.local/bin/comebye` (override: `--bin`).
2. Release (the default): downloads `comebye_<os>_<arch>.tar.gz` and
   `SHA256SUMS` from the GitHub release (`COMEBYE_RELEASES_URL` overrides the
   base), verifies the checksum and that the binary reports the tag. No Go
   toolchain needed. `--from-source` instead fetches, builds the commit
   with the version stamped in, and runs `go vet` and `go test`.
3. Same version: nothing to do. An older version is refused unless
   `--force` (use `--rollback` to go back to the previous binary).
4. Copies the old binary to `comebye.prev`, renames the new one over it
   (atomic, so the running process is unaffected), and runs
   `systemctl --user restart comebye`. The unit file and its drop-ins are
   never touched.
5. Smoke check, using the new binary and this host only: the unit is
   active; `whoami` reaches the local server; its `index.html` is served;
   the server's `X-Comebye-Version` header (on any `/api` response) equals
   the version just installed, else it is a failure (a target as old as
   v0.2.0, which does not send the header, passes with a note);
   `agents --host <this host>` runs; on a hub `ledger`, `queue` and
   `sweep --dry-run` exit 0 (a sweep that fails only because another host
   is unreachable is a warning); the journal since the restart has no
   error-level lines. On a peer, `ledger`, `queue` and `sweep` go through
   the hub, so a hub that is down or restarting is a warning, never a
   failure.
6. If the restart or the smoke check fails, it restores `comebye.prev`,
   restarts, and exits non-zero saying so. On success it prints the
   versions and the manual rollback command
   (`cp ~/.local/bin/comebye.prev ~/.local/bin/comebye && systemctl --user restart comebye`).

A binary that predates `comebye upgrade` cannot run it. On such a host,
bootstrap once from the release tarball, then use the command from then on.
The tarball extracts to a `comebye_<os>_<arch>/` directory (the binary,
`comebye.service`, LICENSE and README), not to a bare `./comebye`:

```sh
d=$(mktemp -d) && cd "$d"
curl -fsSLO https://github.com/jhjaggars/comebye/releases/latest/download/comebye_linux_amd64.tar.gz
curl -fsSLO https://github.com/jhjaggars/comebye/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS && tar -xzf comebye_linux_amd64.tar.gz
comebye_linux_amd64/comebye upgrade
```

The binary runs from the extracted directory, and `upgrade` installs to
`~/.local/bin/comebye` whatever its own path. Swap `linux_amd64` for the
host's os and arch.

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
| `-sock` | `COMEBYE_SOCK`, then `HERDR_SOCKET_PATH` | `~/.config/herdr/herdr.sock` | herdr API socket path, for a herdr listening somewhere other than its default. `~` is expanded. herdr sets `HERDR_SOCKET_PATH` in its panes, so a comebye started in one finds that herdr. |
| `-ts-allow-login` | `COMEBYE_TS_ALLOW_LOGIN` | (none) | Comma-separated Tailscale logins admitted via Tailscale Serve identity headers. Requires a loopback `-addr`. Required unless `-insecure-no-auth`. |
| `-insecure-no-auth` | | `false` | Start without `-ts-allow-login`, serving an unauthenticated API to anyone who reaches the port (your whole tailnet through Serve). For local testing only. |
| `-allow-host` | `COMEBYE_ALLOW_HOSTS` | (none) | Extra `Host` names the server answers to, besides `localhost`, loopback IPs and `*.ts.net`. Repeatable or comma-separated. Other hosts get 421 (DNS rebinding protection). |
| `-host-name` | `COMEBYE_HOST_NAME` | short hostname | This host's name in agent IDs (`<host>/<pane>`), URLs and the UI. Lowercase DNS label. |
| `-peers` | `COMEBYE_PEERS` | (none) | Makes this instance a hub: comma-separated `name=https://host.tailnet.ts.net` (name optional; several addresses per peer, tried in order, `|`-separated). `https` Serve URLs only; requires `-ts-allow-login`. |
| `-hub-mode` | `COMEBYE_HUB_MODE` | `false` | Makes this instance a hub even with no peers yet, so they can be added at runtime (`comebye peer add`). Implied by `-peers` or by stored peers. Requires `-ts-allow-login`. |
| `-ts-allow-tag` | `COMEBYE_TS_ALLOW_TAGS` | (none) | On a hub: comma-separated tags (`tag:worker`) admitted on the peer listener. Needs `-peer-listen`. See [Tagged workers](#tagged-workers-tagworker-vms). |
| `-peer-listen` | `COMEBYE_PEER_LISTEN` | (off) | On a hub: a port (or `tailnet-ip:port`) for the peer listener, bound only to the hub's tailnet address. Needs `-ts-allow-tag` and a hub. |
| `-peer-self-register` | `COMEBYE_PEER_SELF_REGISTER` | `false` | On a hub: let a node with an allowed tag register itself as a peer. Needs `-peer-listen`. |
| `-ts-socket` | `COMEBYE_TS_SOCKET` | `/var/run/tailscale/tailscaled.sock` | tailscaled's LocalAPI socket (WhoIs; this node's name for `-announce`). |
| `-announce` | `COMEBYE_ANNOUNCE` | `false` | On a peer with `-hub`: announce this node to the hub as a peer at startup. |
| `-announce-url` | `COMEBYE_ANNOUNCE_URL` | `https://<this node's name>` | The Serve URL to announce; must be this node. |
| `-peer-domains` | `COMEBYE_PEER_DOMAINS` | (none) | Extra tailnet domain suffixes (comma-separated) allowed for peers added at runtime; `*.ts.net` is always allowed. Not applied to `-peers`. |
| `-launch` | `COMEBYE_LAUNCH` | `true` | Let the app start new agents (see [Launching agents](agents.md#launching-agents)). |
| `-launch-root` | `COMEBYE_LAUNCH_ROOTS` | `~/code` | Directory inside which agents may be launched (symlinks resolved). Repeatable or comma-separated; roots that do not exist are ignored. |
| `-worktree-root` | `COMEBYE_WORKTREE_ROOT` | `~/.worktrees` | Where workers get their git worktrees (`<root>/<repo>/<branch>`); also allowed for worktree checks and removal. |
| `-hub` | `COMEBYE_HUB` | (none) | On a peer: the hub's `https://` Serve URL (or, for a tagged node, `http://<hub>.<tailnet>.ts.net:<port>`, the hub's peer listener). The coordinator CLI's requests are forwarded there and this instance keeps no ledger. Exclusive with `-peers` and `-hub-mode`. See [Coordinators](agents.md#coordinators). |
| `-ledger` | `COMEBYE_LEDGER` | `true` | Keep the handoff ledger, briefing and coordinator roles here (off on a peer with `-hub`). |
| `-coordinator-dir` | `COMEBYE_COORDINATOR_DIR` | `~` | Where the app's Coordinator preset starts a coordinator (may lie outside the launch roots; only that preset uses it). |
| `-coordinator-args` | `COMEBYE_COORDINATOR_ARGS` | none | Extra arguments the Coordinator preset passes per agent kind, `KIND=ARGS` (repeatable or `;`-separated), e.g. `pi=--auto` for an install whose Pi has that extension flag. |
| `-push` | `COMEBYE_PUSH` | `true` | Enable Web Push. Set `false` on peers. |
| `-state-dir` | `COMEBYE_STATE_DIR` | `$XDG_STATE_HOME/comebye`, else `~/.local/state/comebye` | Holds `vapid.pem`, `push-subscriptions.json`, the `agent-token`, and on the hub `peers.json` (peers added live), `ledger.json`, `brief.json` and `roles.json` (dir 0700, files 0600), and an optional `handoff-template.md`. The CLI reads the token from here too. |
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
