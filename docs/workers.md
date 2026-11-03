# Worker VMs

`tools/worker` builds a worker VM on Google Compute Engine, puts it on your
tailnet as a **tagged, persistent** node (`tag:worker`) and lets it join the
comebye hub. After that, comebye does the rest: launching agents, handoffs
and repo checkouts go through the hub like any other peer. The tool does no
agent orchestration and clones no repositories.

It is a separate Go program in this module, not a `comebye` subcommand: the
server binary is a phone app that every host runs, and does not need GCP and
Tailscale-admin code in it. It shells out to `gcloud` and `ssh`, and speaks
the Tailscale API over HTTPS with the standard library.

```sh
go run ./tools/worker <command>      # or: go build -o ~/.local/bin/comebye-worker ./tools/worker
```

| Command | What it does |
|---|---|
| `create NAME [--dry-run] [--comebye-version vX.Y.Z]` | Builds the VM, enrols it, bootstraps it, waits for the hub. Safe to re-run: it resumes where it stopped and never mints a second key for a node that is already enrolled. `--dry-run` prints the plan and touches nothing. |
| `status [NAME]` | One worker: GCE state, tailnet address, comebye version and units, the tailnet device, the hub's view (with a version-mismatch warning). Without a name: every labelled instance and disk and every `tag:worker` device. |
| `stop NAME` / `start NAME` | Stop or start the VM. The tailnet node and the hub's peer entry stay (the hub shows it unreachable). `start` re-reads the host key from GCE (the external IP changes) and waits for comebye. |
| `destroy NAME [--delete-data-disk]` | Asks you to type the name. Signs the agents out, `tailscale logout`, deletes the tailnet device (only one with the exact hostname **and** the tag), removes the hub's peer entry, deletes the VM. The data disk is kept unless you pass the flag. |
| `login NAME claude\|codex` | Runs the agent's own sign-in on the VM over ssh. You open the printed URL in your browser; the credential is created on the VM and nothing is copied from your machine. |

## Before the first run

1. **GCP project** with the Compute API enabled and `gcloud` logged in
   (`gcloud auth login`). The tool creates a dedicated VPC, subnet and a
   firewall rule that allows tcp:22 from **one /32** (yours), and nothing else.
2. **Tailscale OAuth client** (admin console, Settings, OAuth clients) with
   the scopes `auth_keys` (write) and `devices:core` (write), limited to
   `tag:worker`. Put it in a file outside the repo, mode 0600:

   ```sh
   umask 077; mkdir -p ~/.config/comebye-worker
   printf 'client_id=...\nclient_secret=...\n' > ~/.config/comebye-worker/tailscale-oauth
   ```

   Without that file the tool asks, at a hidden prompt, for a single-use key
   tagged `tag:worker` that you minted by hand. It then cannot clear stale
   devices or delete the node on `destroy` (it tells you to).
3. **Tailnet ACLs** (you apply them; the tool never edits the policy):

   ```jsonc
   {
     "tagOwners": { "tag:worker": ["autogroup:admin"] },
     "grants": [
       // you, your phone and the hub reach a worker's Tailscale Serve (the hub drives it)
       { "src": ["autogroup:member"], "dst": ["tag:worker"], "ip": ["tcp:443"] },
       // a worker reaches ONLY the hub's peer listener (report, handoff, self-registration)
       { "src": ["tag:worker"], "dst": ["you@example.com"], "ip": ["tcp:8742"] }
     ]
   }
   ```

   Use your login for `dst` (the hub is a node you own), or an IP-set alias
   for the hub alone if you have other devices. Remove or narrow any
   allow-all rule: with one, a compromised worker reaches your whole tailnet.
   There is no rule from one worker to another. Keep the port equal to the
   hub's `COMEBYE_PEER_LISTEN`.
4. **Hub environment** (its `~/.config/comebye/env`, then restart the hub's
   comebye once). See [Tagged workers](setup.md#tagged-workers-tagworker-vms)
   and [Security](security.md#tagged-nodes-and-the-peer-listener):

   ```sh
   COMEBYE_HUB_MODE=1
   COMEBYE_TS_ALLOW_LOGIN=you@example.com
   COMEBYE_TS_ALLOW_TAGS=tag:worker
   COMEBYE_PEER_LISTEN=8742
   COMEBYE_PEER_SELF_REGISTER=1
   ```

   Without `COMEBYE_PEER_SELF_REGISTER=1` the tool finishes with the exact
   `comebye peer add NAME https://NAME.<tailnet>.ts.net --tag tag:worker` to
   run on the hub.
5. **Config** at `~/.config/comebye-worker/config` (copy
   [`tools/worker/config.example`](../tools/worker/config.example)). The
   required keys are the project, zone, your workstation's `ssh_source_cidr`,
   `ts_allow_login` (the login that owns the hub device, which the worker
   admits the hub's requests as), `hub_url` (the hub's Serve URL, used to read
   its version and remove the peer entry) and `hub_peer_url` (the peer
   listener, `http://hub.<tailnet>.ts.net:8742`, which becomes `COMEBYE_HUB` on
   the worker). Everything else has a default: `e2-standard-4`, 20 GB boot, 50
   GB data disk, Debian 12, auto-stop after 120 idle minutes.
6. **The hub must run a release** (`vX.Y.Z`). The worker installs the hub's
   version from the release tarball; a hub on a development build is refused
   (pass `--comebye-version` or pin `hub_version`).

## What `create` does

1. Network, subnet, firewall rule, an ed25519 key for this tool, a separate
   `NAME-data` disk (mounted at `/data`, formatted only if blank, never
   auto-deleted), then the instance: no service account, no scopes, shielded
   VM, OS Login and the serial port off, project ssh keys blocked, a
   `managed-by=comebye-worker` label (the tool refuses to touch anything
   without it) and, by default, a 24 h `max-run-duration` backstop.
2. **Host-key-verified ssh.** The VM's startup script publishes its ssh host
   key to GCE guest attributes. The tool reads it through the authenticated GCE
   API and writes a private `ssh_config` and `known_hosts` with
   `StrictHostKeyChecking yes`, so the first connection never trusts the
   network.
3. Over that ssh: packages (`git jq bubblewrap socat curl`), Tailscale from its
   apt repository, linger, and the idle timer. Every five minutes it powers the
   VM off after 120 minutes with no ssh session and no herdr agent `working`
   (a guest poweroff stops a GCE instance; `start` brings it back).
4. **Tailnet.** With the OAuth client: delete an *offline* device with the same
   hostname and `tag:worker` (so the new node is not renamed `NAME-1`; an
   untagged device is never touched), then mint a **single-use, pre-authorized,
   `tag:worker` key that expires in 10 minutes**. The key goes to
   `tailscale up --auth-key=file:/dev/stdin` over ssh **stdin**: it is never in
   an argument list, GCE metadata, a file, the repo or a log (only its id is
   printed). Then `tailscale serve --bg 8741`.
5. As the worker user: herdr as a user service, `claude` and `codex`
   installed but **not signed in**, and comebye **at the hub's version** from
   its release tarball with `SHA256SUMS` checked, then `comebye upgrade
   --version` (which restarts the unit and smoke-checks). The env file sets
   `COMEBYE_HUB`, `COMEBYE_ANNOUNCE=1`, `COMEBYE_PUSH=false`,
   `COMEBYE_TS_ALLOW_LOGIN`, `COMEBYE_LAUNCH_ROOTS=/data/code` and
   `COMEBYE_WORKTREE_ROOT=/data/worktrees`, so repos and worktrees live on the
   data disk and survive a rebuilt boot disk.
6. Waits for the hub to list the worker.

Sign in with `worker login NAME claude` (and `codex`), then use comebye as
usual. Git push credentials are yours to arrange (a per-repo deploy key, or a
temporary `gh auth login --web` on the VM).

## Cost

Approximate us-central1 list prices: the VM about **$0.134/h** while running
(e2-standard-4), the external IPv4 about $0.005/h while running, and the disks
(20 GB boot + 50 GB data, pd-balanced) about **$7/month, billed while the VM
is stopped**. A first test of an hour or two is well under a dollar. A forgotten
running worker is the real cost, which is what the idle timer and the 24 h cap
are for. `worker status` (no name) lists everything the tool created.

## Limits

- The bootstrap is written for Debian and Ubuntu images (apt).
- The herdr, Claude and Codex installers are third-party scripts. Pin herdr's
  with `herdr_install_sha256`; the agent installers are not pinned.
- `destroy` signs the agents out only if the VM is running. A stopped VM is
  deleted with its node removed through the API.
