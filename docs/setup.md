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

## Deployment options

Comebye runs the same way in all three; pick the smallest that fits.

1. **A single host (the default).** comebye and herdr on one machine, as a
   systemd user service ([steps 1-3](#2-run-as-a-systemd-user-service)). Nothing
   below is needed or changes: no hub-only mode, no probes to configure, no
   Kubernetes. (The state directory's `hub.lock` and the `/healthz` endpoint
   are present but invisible.)
2. **A host plus peers.** One of your hosts is the hub and the others run
   comebye as peers ([Multiple hosts](#multiple-hosts)). The hub still has its
   own herdr and agents.
3. **In Kubernetes (optional).** For a hub that should live in a cluster
   rather than on a machine you keep awake: a single-replica StatefulSet running
   comebye in [`-hub-only` mode](#a-hub-with-no-herdr-containers-kubernetes)
   (it hosts no agents; they run on peers), a `tailscaled` sidecar to publish it
   with Serve, a persistent volume for the state directory, probes on
   `/healthz` and `/readyz` ([Health probes](#health-probes)), and
   [backups](#backup-and-restore) of the volume. A generic starting point is in
   [`deploy/kubernetes/`](../deploy/kubernetes/). Nothing about the
   comebye server requires it: the same binary and state directory work on a
   plain host, and a state directory can move between the two (see
   [the state directory](#the-state-directory-what-is-in-it-and-moving-it)).

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
it: its workers could not `comebye report`. The way in is an **app capability
through Serve**: the worker uses the hub's normal Serve URL, with no extra port
or firewall opening, and gets a limited role. See
[below](#a-capability-through-serve). (The separate plain-HTTP peer listener on
port 8742, `-peer-listen` and `-ts-allow-tag`, was removed; the hub ignores
those settings with a warning, so delete them from its env.)

#### A capability through Serve

This needs Tailscale 1.92 or later on the hub. Add a grant to your tailnet
policy. An `app` block alone opens no port, so it also needs `ip`:

```jsonc
"grants": [
  {
    "src": ["tag:worker"],
    "dst": ["comebye-hub"],   // a host alias or tag for the hub
    "ip":  ["tcp:443"],
    "app": {"github.com/jhjaggars/comebye/cap/peer": [{"role": "worker"}]},
  },
],
```

The capability's name is yours to choose, as long as it matches the hub's
setting. Its values are not read: holding it is what counts.

On the hub, have Serve forward the capability, and name it to comebye:

```sh
tailscale serve --bg --accept-app-caps=github.com/jhjaggars/comebye/cap/peer 8741

COMEBYE_HUB_MODE=1
COMEBYE_TS_ALLOW_CAP=github.com/jhjaggars/comebye/cap/peer
COMEBYE_PEER_SELF_REGISTER=1         # optional: let workers register themselves
# COMEBYE_TS_SOCKET=/var/run/tailscale/tailscaled.sock   # LocalAPI, if elsewhere
```

On the tagged worker VM:

```sh
COMEBYE_HUB=https://hub.tailnet.ts.net       # the hub's normal Serve URL
COMEBYE_ANNOUNCE=1                           # register with the hub at startup
```

The rest of this section applies here too. Serve's header doesn't say who the caller is, so the
hub also asks WhoIs about the address Serve puts in `X-Forwarded-For`, and that
answer must show the capability too. A 403 `cap_not_held` means WhoIs disagrees
with the header; if the header never arrives (Serve lacks `--accept-app-caps`)
the worker just gets 401, and can still load the app's static files like any
node that can reach Serve. For the trust model, see
[Security](security.md#tagged-nodes-through-serve).

#### What a tagged node gets

- **What a tagged node can do:** less than a peer's forwarded agent: what its
  workers need (`comebye report`, `comebye prompt` to a coordinator or an agent
  on its own host, adding a to-do, the ledger in outline), attributed to the
  peer's name. See [what a tagged worker reaches](security.md#what-a-tagged-worker-reaches).
  **What it can't:** read other agents' output or transcripts, hand off, mark,
  anything the user alone can do (keys, answers, launching, removals, push, the
  peer registry), the web app, or anything as the user. Only `/api/*` is
  answered.
- **Two conditions, both checked on every request:** the node is tagged and
  holds the capability, and it is a **registered peer** - one of the peer's addresses
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
- **Trade-offs:** self-registering gives the node what any peer has: the hub
  sends it prompts and trusts its agent list, so only tag machines you trust.
  See [Security](security.md#tagged-nodes-through-serve).

### Which agent each host starts (host settings)

Each host decides which agent a start uses when nothing is asked for: the launch sheet, `comebye handoff` without `--kind`, a queued plan step. Set it once per host, for example pi on the VMs and the laptop and claude on the desktop:

- In the app: **Peers** (the Fleet menu, or the nav on a desktop) lists every host with its status and version. Open one to see its installed agents and models and to set the **default agent**, the **allowed agents**, and a **model and thinking level per agent**. Save writes the host's own `settings.json`; the hub reaches a peer's through its proxy, so you can edit every host from the hub.
- From a shell: `comebye settings --host csb-vm`, `comebye settings set --host csb-vm --default-kind pi --allow pi --model pi=anthropic/sonnet --thinking pi=high --login you@example.com`, `comebye peer show csb-vm`.
- At creation, for a host that is rebuilt (a spot worker): `COMEBYE_DEFAULT_KIND=pi` and `COMEBYE_ALLOWED_KINDS=pi` seed the values that `settings.json` does not have.

Order for one start: what the request names, then `settings.json`, then those flags, then the old guess (the kind most running agents use). A request that names another kind than the host's default still starts, with a warning, and the ledger records `kind_source: request`; a kind outside the allowed list is refused with `kind_not_allowed`, whoever asks. Only you change settings (the app, or the CLI with your login); coordinators read them (`comebye settings --host H`) and cannot write, and a tagged worker node reads only its own host's. The settings live on the host that starts the agent, so they follow it when the hub moves, work for peers set with `-peers`, and apply to a peer used without a hub. A peer on an older version has none: the Peers screen says to upgrade it, and its starts keep the old guess. A peer that is down cannot be edited until it is back.

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

## A hub with no herdr (containers, Kubernetes)

A hub does not have to run agents itself. `-hub-only` (`COMEBYE_HUB_ONLY=true`)
starts comebye as a **hub that hosts no agents**: it does not wait for a herdr
socket and never talks to one. `-hub-only` implies `-hub-mode` (peers can be
added live) and turns launching off on this host. An
**explicitly empty** `-sock=` also means hub-only. A merely missing socket
does not: a normal instance keeps waiting for herdr to appear, as before.

```sh
comebye -hub-only -addr 127.0.0.1:8741 -host-name hub \
  -ts-allow-login you@example.com -state-dir /var/lib/comebye
```

`-hub-only` refuses to start with `-hub` (it is the hub, not a peer) or
`-ledger=false`. Everything that is not about a pane on this machine works as
on any hub:

| Feature | In hub-only mode |
| --- | --- |
| Handoff ledger, plans, marks, reports, landings, queue | Work as on any hub. A worker runs on a **peer**; name the host when you hand off. |
| To do, briefing and its schedule, coordinator roles | Work. The scheduled briefing prompts a coordinator on any **peer**; with none reachable it waits, as for any busy coordinator. |
| Peers, the agent list, push and ntfy notifications, release | Work: the hub merges its peers' agents and notifies for them. |
| Archive and `recall` search | Searching works (it reads the index, `-recall-index`). The server ships a transcript copy only when a **local** agent is removed, so a hub-only hub ships none. |
| The app's host list | The hub's own host shows **"no agents here"** (`no_agents` in `/api/hosts`), not an error or a bare 0. Its peers' hosts are listed as usual. |
| Launching (the + button, Coordinator preset, `comebye launch`), workers, repo checks | Not on the hub itself: `403 launch_disabled`, and the app greys out the hub's host in the launch form and starts on a peer. On a peer they work through the hub. |
| Keys, answers, dialogs, images, output and prompts for a **local** pane | There are none: `409 no_herdr` ("this hub hosts no agents"). Peers' agents are reached through the proxy as before. |
| Daily repository fsck | Nothing local to check; peers report their own. |

## Health probes

`GET /healthz` and `GET /readyz` answer without a login and on **any `Host`
header** (a kubelet probes the pod IP, which the DNS-rebinding check would
otherwise refuse). They do no network I/O, and the answer carries no path, name
or count: only the version and a status per check.

```
$ curl -i localhost:8741/healthz
HTTP/1.1 200 OK
{"checks":{"state_dir":"ok"},"hub_only":true,"status":"ok","version":"v1.2.3"}
```

| Probe | Checks | Use for |
| --- | --- | --- |
| `/healthz` | The process is serving, and **the state directory is writable**: a file is written, synced and removed there (at most once every 5 s; a probe storm costs one write). 503 `"status":"fail"` otherwise. | Liveness. |
| `/readyz` | `/healthz`, plus **the single-hub lock is still held** (its heartbeat is being written, see below) and the server is **not shutting down** (it fails as soon as SIGTERM arrives, so traffic drains first). | Readiness. |

Both answer `GET` and `HEAD` only. Because the main listener is loopback-only
(Tailscale Serve must be its only client), a kubelet `httpGet` probe cannot
reach it. Either probe with `exec` (`curl -fs localhost:8741/readyz` inside the
container), or give the probes their own listener:
`-health-addr :8743` (`COMEBYE_HEALTH_ADDR`) serves **only** these two paths,
with no login and nothing else behind it.

## One server per state directory

Two comebye servers writing one state directory would overwrite each other's
ledger and to-dos. On start, before it reads anything, the server takes an
exclusive lock on **`hub.lock`** in the state directory and holds it until it
exits. The file says who holds it:

```
{"pid":4321,"host":"comebye-0","name":"hub","addr":"127.0.0.1:8741",
 "started":"2026-11-08T15:06:22-05:00","heartbeat":"2026-11-08T15:08:42-05:00"}
```

A second server on the same directory refuses to start and names the holder:

```
state directory is in use by another comebye: pid 4321 on comebye-0 (host-name hub), started 2026-11-08T15:06:22-05:00, last heartbeat 2026-11-08T15:08:42-05:00 (flock lock /var/lib/comebye/hub.lock). Only one server may use a state directory; stop that one first
```

`-lock-wait 30s` (`COMEBYE_LOCK_WAIT`) retries once a second for that long
before refusing, so a replacement pod waits for the old one to leave. The
default `0` refuses at once.

- **After a crash** there is nothing to clean up. The lock is an `flock(2)`
  that the kernel drops when the process dies, however it dies (`kill -9`, OOM,
  node loss); the leftover `hub.lock` is only a stale note. The next server
  starts at once and logs `the previous server on this machine did not shut down
  cleanly`. A clean exit empties the file. Do not delete `hub.lock` by hand to
  "unstick" a start: if the message names a live process, that process is real.
- **On shared storage** `flock` alone is not trusted. Some network filesystems
  (NFS without lock support, some FUSE and object-store mounts) do not carry it
  between machines, and then two servers on two machines could both "hold" it.
  So the holder also rewrites `heartbeat` every 5 s, and a server that wins the
  flock but finds a record from **another machine** (`host` differs) with a
  heartbeat younger than **20 s** refuses too (`lease` in the message). A
  record from another machine older than 20 s is taken over, with a log line
  naming the previous holder (a crash, or a node that went away). The cost is
  at most a 20 s wait after an unclean loss of a different machine; use
  `-lock-wait 30s` to ride it out. A record from the **same** machine needs no
  wait: there the flock is authoritative.
- **Longhorn ReadWriteOnce** (and any block volume) is mounted on one node at a
  time and is an ordinary local filesystem to the pod, so the `flock` is
  authoritative and the lease is a second line of defence. A ReadWriteMany or
  NFS volume is exactly the case the lease exists for; even so, run one
  replica (`replicas: 1`, a StatefulSet) and do not rely on the lock to make
  two replicas safe: it makes a mistake loud, it does not make two hubs
  correct.
- If the volume goes read-only or away while the hub runs, the heartbeat fails,
  `/readyz` returns 503 (`"lock":"fail"`) and the log says so; after 20 s
  another machine may take over, so stop the old one (the failing probes will).

## The state directory: what is in it, and moving it

`-state-dir` (default `~/.local/state/comebye`; in a container set it, for
example `/var/lib/comebye`, on the persistent volume). Files marked
**required** are the hub's memory: without them it starts empty and forgets.
**Rebuildable** ones come back by themselves or by a command. Nothing in the
directory is a secret except the files marked *secret*; keep the volume and its
backups private.

| Path | What it is | If lost |
| --- | --- | --- |
| `ledger.json` | The handoff ledger: workers, marks, plans, reports | **Required.** History and open work are gone. |
| `todo.json` | The To do list | **Required.** |
| `brief.json`, `brief-history.json`, `brief-schedule.json` | The briefing, its history and schedule | **Required** (history is lost; the briefing is rewritten by the next run). |
| `roles.json` | Which agents are coordinators | **Required**; coordinators must be marked again. |
| `queue.json` | Prompts waiting to be delivered | **Required** if prompts are queued; otherwise empty. |
| `peers.json` | Peers added live (`comebye peer add`) | **Required**, or re-add each peer. Peers in `-peers` need no file. |
| `vapid.pem` *(secret)* | The Web Push signing key | **Required.** A new key invalidates every phone's subscription: each must turn notifications on again. |
| `push-subscriptions.json` | Phones subscribed to push | **Required**, or each phone re-subscribes. |
| `handoff-template.md` | Your handoff template, if you made one | **Required** if present. |
| `settings.json` | The host's agent settings: default kind, model and thinking level per kind, allowed kinds (set in the app's Peers screen or with `comebye settings set`; the hub edits a peer's through the proxy) | Re-enter them (or seed with `-default-kind` / `-allowed-kinds`); starts fall back to the guess until then. |
| `kinds.json` | When each agent kind last worked here (orders the launch form) | Rebuildable; refills as agents work. |
| `agent-token` *(secret)* | The local CLI's token | Rebuildable: a new one is made at start. Only the `comebye` CLI run on this host needs to read the new one. |
| `repo-health.json` | Damaged repositories git reported | Rebuildable (the daily fsck refills it); meaningless on a hub-only hub. |
| `uploads/` | Images sent from the app for agents on this host | Disposable. |
| `recall/index.db` | The transcript search index (`comebye archive index` builds it) | Rebuildable from the archive. **Not under `-state-dir`**: `-recall-index` defaults to `~/.local/state/comebye/recall/index.db` whatever `-state-dir` says; point `-recall-index` at the volume if the hub should keep it. |
| `hub.lock`, `.healthz` | The lock (above) and the writability probe's scratch file | **Never restore.** Created at start. |
| `coordinator/`, `locks/`, `stt/` | Seen on a long-lived host: scratch the coordinator agents and the archive shipper keep (`locks/` holds the shipper's per-host `archive-<host>.lock`) | Not read by a hub-only hub. |

Every JSON file is written whole to a temp file, synced and renamed, so a
copy taken while the hub runs contains each file either before or after a
change, never half of one. A copy is not a snapshot across files (a worker
filed in `ledger.json` while `todo.json` was copied earlier). For a backup that
is also consistent across files, take it with the hub stopped (a StatefulSet
scaled to 0) or from a volume snapshot.

**What hard-codes the host name.** Agent ids are `<host>/<pane>`, and that id
is stored in `ledger.json` (`host`, `agent_id`), `todo.json` (who waits),
`queue.json`, `roles.json` and `brief-schedule.json`. The `-host-name` of the
hub that wrote them is therefore **part of the data**. When a hub moves:

1. Give the new hub a `-host-name` that is **not** the old hub's, and not the
   pod's hostname (a StatefulSet pod is `comebye-0`; the default host name is
   the machine's short hostname, so always set `-host-name` explicitly).
2. Register the machine that used to be the hub **as a peer under its old
   host name** (`comebye peer add OLDNAME https://...`), so the ids already in
   the ledger (`OLDNAME/w1:p1`) resolve to its agents again. A peer cannot
   take the hub's own name; that is why step 1 matters.
3. Keep the **tailnet name** if phones and peers know the hub by it: the
   browser's push subscriptions and the app's saved URL are tied to that origin,
   and peers' `-hub` URLs point at it.
4. The archive's per-host claim (`hosts/<host>.json`) records the machine that
   writes a host's transcripts, by machine id (else hostname). It matters
   only to a hub that ships transcripts, which a hub-only hub does not.

### Backup and restore

Backup (the hub may be running; see the caveat above):

```sh
tar -C /var/lib/comebye --exclude=./hub.lock --exclude=./.healthz \
    -czf comebye-state-$(date +%Y%m%d-%H%M%S).tgz .
chmod 600 comebye-state-*.tgz     # it holds vapid.pem and agent-token
```

Restore into an empty directory, with **no hub running on it**:

```sh
mkdir -m 700 /var/lib/comebye && tar -C /var/lib/comebye -xzf comebye-state-XXXX.tgz
comebye -hub-only -state-dir /var/lib/comebye -host-name hub -ts-allow-login you@example.com
```

Check it: `curl localhost:8741/readyz`, then `comebye todo list` and
`comebye brief get` with `COMEBYE_STATE_DIR=/var/lib/comebye` show your data. To
rehearse a restore without touching the live hub, extract the archive into a
scratch directory and start a second hub there on **another port** (`-addr
127.0.0.1:18761`) and its own `-state-dir`: the lock is per directory, so it
does not collide. Do this before you rely on a backup, and again after you
change anything about where the hub's state lives.

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

### Without systemd (sandbox hosts)

A host with no systemd user unit, such as an OpenShell sandbox whose init
loop keeps `comebye` running, upgrades with `--restart exit` (the default
when `systemctl` is missing or has no `comebye` unit; `--restart systemd`
forces the unit):

```sh
comebye upgrade --restart exit            # or just: comebye upgrade
comebye upgrade --restart exit --pid 123  # when several comebye servers run as this user
```

Steps 1 to 4 are the same: the same checksum, version and downgrade checks,
the same `comebye.prev`. The restart is different: instead of
`systemctl restart`, it sends the running server SIGTERM (it logs "shutting
down" and exits 0), waits for it to go, and relies on the supervisor to start
`~/.local/bin/comebye` again. The server is found by its command line: a
`comebye` process of this user with no subcommand (only flags). The smoke
check is the same minus the unit and journal checks: it waits (up to 30 s)
for the local server to answer, then runs the same `whoami`, `index.html`,
served-version, `agents` and `ledger` steps. If the server never comes back
(nothing restarts it), it restores `comebye.prev`, signals again and says so.
The supervisor must therefore start the binary by name from a PATH where
`~/.local/bin` comes first; see [sandbox hosts](sandbox-hosts.md).

`COMEBYE_UPGRADE_RESTART` sets the default for `--restart`.

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
| `-ts-allow-tag` | `COMEBYE_TS_ALLOW_TAGS` | | **Removed**: ignored with a warning. Use `-ts-allow-cap`. |
| `-ts-allow-cap` | `COMEBYE_TS_ALLOW_CAP` | (none) | On a hub: the app capability that admits a tagged node through Serve (run Serve with `--accept-app-caps`). See [A capability through Serve](#a-capability-through-serve). |
| `-peer-listen` | `COMEBYE_PEER_LISTEN` | | **Removed**: ignored with a warning. Tagged nodes use the Serve URL with `-ts-allow-cap`. |
| `-peer-self-register` | `COMEBYE_PEER_SELF_REGISTER` | `false` | On a hub: let a node holding the capability register itself as a peer. Needs `-ts-allow-cap`. |
| `-ts-socket` | `COMEBYE_TS_SOCKET` | `/var/run/tailscale/tailscaled.sock` | tailscaled's LocalAPI socket (WhoIs; this node's name for `-announce`). |
| `-announce` | `COMEBYE_ANNOUNCE` | `false` | On a peer with `-hub`: announce this node to the hub as a peer at startup. |
| `-announce-url` | `COMEBYE_ANNOUNCE_URL` | `https://<this node's name>` | The Serve URL to announce; must be this node. |
| `-peer-domains` | `COMEBYE_PEER_DOMAINS` | (none) | Extra tailnet domain suffixes (comma-separated) allowed for peers added at runtime; `*.ts.net` is always allowed. Not applied to `-peers`. |
| `-launch` | `COMEBYE_LAUNCH` | `true` | Let the app start new agents (see [Launching agents](agents.md#launching-agents)). |
| `-launch-root` | `COMEBYE_LAUNCH_ROOTS` | `~/code` | Directory inside which agents may be launched (symlinks resolved). Repeatable or comma-separated; roots that do not exist are ignored. |
| `-release-repo` | `COMEBYE_RELEASE_REPO` | empty (off) | On the hub: the comebye checkout the To do page's **Approve release** button tags from, using this server's own `git` and `gh` credentials. See [Releasing](releasing.md#from-the-app). |
| `-release-upgrade` | `COMEBYE_RELEASE_UPGRADE` | `false` | Let the To do page's **Upgrade** button run `comebye upgrade --version` on this host (needs `systemd-run`; on a peer it also needs `-hub`, and `-ledger=false` needs `-hub` with it). |
| `-release-dry-run` | `COMEBYE_RELEASE_DRY_RUN` | `false` | Both buttons do every check and change nothing: no tag, no install. For trying it out. |
| `-release-deploy-cmd` | `COMEBYE_RELEASE_DEPLOY_CMD` | empty (off) | On the host that owns `-release-repo`: a command (absolute program path and arguments, no shell) that a release item's second button runs once the build passed, with the version appended as the last argument and in `COMEBYE_RELEASE_VERSION`. Approve release never runs it. See [Releasing](releasing.md#deploying-a-release-your-own-command). |
| `-release-deploy-label` | `COMEBYE_RELEASE_DEPLOY_LABEL` | `Deploy` | The name of that button. |
| `-worktree-root` | `COMEBYE_WORKTREE_ROOT` | `~/.worktrees` | Where workers get their git worktrees (`<root>/<repo>/<branch>`); also allowed for worktree checks and removal. |
| `-public-url` | `COMEBYE_PUBLIC_URL` | (none) | The `https://` URL the hub is reached at (`https://hub.tailnet.ts.net`). `comebye publish` prints page links built from it (a peer's CLI uses its `-hub` URL instead); with `-pages-listen` it is also the one origin allowed to frame a page. |
| `-pages-listen` | `COMEBYE_PAGES_LISTEN` | (none) | A second loopback listener (e.g. `127.0.0.1:8742`) that serves only published pages (`/p/...`): no `/api`, no app, everything else 404. Publish it on its own Serve port (`tailscale serve --bg --https=8443 http://127.0.0.1:8742`), so pages get an origin of their own. Needs `-pages-url`; must differ from `-addr`; loopback when `-ts-allow-login` is set. |
| `-pages-url` | `COMEBYE_PAGES_URL` | (none) | That listener as the browser reaches it (`https://hub.tailnet.ts.net:8443`): the app frames pages from it (its Content-Security-Policy names it), and page links use it. |
| `-hub` | `COMEBYE_HUB` | (none) | On a peer: the hub's `https://` Serve URL (a tagged node uses it too). The coordinator CLI's requests are forwarded there and this instance keeps no ledger. Exclusive with `-peers` and `-hub-mode`. See [Coordinators](agents.md#coordinators). |
| `-ledger` | `COMEBYE_LEDGER` | `true` | Keep the handoff ledger, briefing and coordinator roles here (off on a peer with `-hub`). |
| `-default-kind` | `COMEBYE_DEFAULT_KIND` | (none) | The agent kind a start on this host uses when the request names none (a launch, a handoff, a plan step). A seed: a value saved in `settings.json` (the app's Peers screen, `comebye settings set`) wins. Empty: the kind most of the running agents use. Use it to give an ephemeral host (a spot worker) a rule at creation: `COMEBYE_DEFAULT_KIND=pi`. |
| `-allowed-kinds` | `COMEBYE_ALLOWED_KINDS` | (none) | The only agent kinds this host starts, comma-separated (`pi`, or `pi,codex`); anything else is refused (`kind_not_allowed`) and the launch sheet does not offer it. A seed that `settings.json` overrides. Empty: every installed kind. |
| `-coordinator-dir` | `COMEBYE_COORDINATOR_DIR` | `~` | Where the app's Coordinator preset starts a coordinator (may lie outside the launch roots; only that preset uses it). |
| `-coordinator-args` | `COMEBYE_COORDINATOR_ARGS` | none | Extra arguments the Coordinator preset passes per agent kind, `KIND=ARGS` (repeatable or `;`-separated), e.g. `pi=--auto` for an install whose Pi has that extension flag. |
| `-hub-only` | `COMEBYE_HUB_ONLY` | `false` | Run as a hub that hosts no agents: no herdr, nothing waits for a socket; implies `-hub-mode`, turns launching off here. An explicitly empty `-sock=` means the same. See [A hub with no herdr](#a-hub-with-no-herdr-containers-kubernetes). |
| `-lock-wait` | `COMEBYE_LOCK_WAIT` | `0` | When another comebye holds the state directory, retry this long (once a second) before refusing to start. See [One server per state directory](#one-server-per-state-directory). |
| `-health-addr` | `COMEBYE_HEALTH_ADDR` | (none) | Also serve `/healthz` and `/readyz` (nothing else, no login) on this address, e.g. `:8743`, for probes that cannot reach the loopback listener. |
| `-push` | `COMEBYE_PUSH` | `true` | Enable Web Push. Set `false` on peers. |
| `-state-dir` | `COMEBYE_STATE_DIR` | `$XDG_STATE_HOME/comebye`, else `~/.local/state/comebye` | Holds `vapid.pem`, `push-subscriptions.json`, the `agent-token`, and on the hub `peers.json` (peers added live), `ledger.json`, `brief.json` and `roles.json` (dir 0700, files 0600), and an optional `handoff-template.md`; `hub.lock` keeps a second server off it. The CLI reads the token from here too. Every file is listed, with what to do if it is lost, in [The state directory](#the-state-directory-what-is-in-it-and-moving-it). |
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
