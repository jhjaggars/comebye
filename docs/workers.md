# Worker VMs

`tools/worker` builds a worker VM on Google Compute Engine **or AWS EC2**
(`provider = gcp|aws`), puts it on your tailnet as a **tagged, persistent** node (`tag:worker`) and lets it join the
comebye hub. After that, comebye does the rest: launching agents, handoffs
and repo checkouts go through the hub like any other peer. The tool does no
agent orchestration and clones no repositories.

It is a separate Go program in this module, not a `comebye` subcommand: the
server binary is a phone app that every host runs, and does not need cloud and
Tailscale-admin code in it. It shells out to `gcloud` or `aws` and `ssh`, and
speaks the Tailscale API over HTTPS with the standard library. The cloud part
sits behind one `Provider` interface; enrolment, the bootstrap scripts, the hub
join and the order of `destroy` are the same for both clouds.

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

1. **A cloud account**, one of:
   - **GCP:** a project with the Compute API enabled and `gcloud` logged in
     (`gcloud auth login`). The tool creates a dedicated VPC, subnet and a
     firewall rule that allows tcp:22 from **one /32** (yours), and nothing else.
   - **AWS:** the `aws` CLI configured (a named profile via `aws_profile`, or
     the usual environment variables). The tool never reads or stores your
     credentials; it only passes `--profile NAME --region R` to the CLI. The
     identity needs EC2 (VPC, instance, volume, security group, console output)
     and read access to the public SSM parameter
     `/aws/service/debian/release/12/latest/amd64`. It creates a dedicated VPC
     (`10.77.0.0/16`), a subnet in `aws_az`, an internet gateway, a route
     table and a security group with one ingress rule: tcp:22 from your /32.
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
   required keys are `provider` (default `gcp`), then for GCP the project and
   zone, or for AWS `aws_region` and `aws_az`; and for both your workstation's
   `ssh_source_cidr`,
   `ts_allow_login` (the login that owns the hub device, which the worker
   admits the hub's requests as), `hub_url` (the hub's Serve URL, used to read
   its version and remove the peer entry) and `hub_peer_url` (the peer
   listener, `http://hub.<tailnet>.ts.net:8742`, which becomes `COMEBYE_HUB` on
   the worker). Everything else has a default: `e2-standard-4` (GCP) or `t3.xlarge` (AWS),
   20 GB boot, 50 GB data disk (`pd-balanced` or `gp3`), Debian 12 amd64
   (Arm/Graviton types are refused), auto-stop after 120 idle minutes.
6. **The hub must run a release** (`vX.Y.Z`). The worker installs the hub's
   version from the release tarball; a hub on a development build is refused
   (pass `--comebye-version` or pin `hub_version`).

## What `create` does

1. **Cloud resources**, each created only if missing and marked
   `managed-by=comebye-worker` (a tag on AWS; in the description on GCP, whose
   networks and firewall rules have no labels). The tool refuses to stop, start,
   log in to or destroy an instance without that label.
   - **GCP:** network, subnet, firewall rule; an ed25519 key for this tool; a
     separate `NAME-data` disk (mounted at `/data`, formatted only if blank,
     never auto-deleted); then the instance: no service account, no scopes,
     shielded VM, OS Login and the serial port off, project ssh keys blocked, and
     by default a 24 h `max-run-duration` backstop.
   - **AWS:** the VPC pieces above; a separate encrypted `gp3` data volume
     (`NAME-data`, never deleted by terminating the instance); then the
     instance with **no IAM instance profile**, IMDSv2 required with hop limit 1,
     an encrypted root volume, a public IPv4, and
     `InstanceInitiatedShutdownBehavior=stop` so the idle poweroff stops
     rather than terminates. The data volume is attached once the instance
     runs. The AMI comes from the public SSM parameter (or `image_id`).
2. **Host-key-verified ssh.** No ssh connection ever trusts the network.
   - **GCP:** the VM's startup script publishes its ssh host key to GCE guest
     attributes, which the tool reads through the authenticated GCE API.
   - **AWS** has no guest attributes. The user-data (cloud-config; the worker
     user, **your public key only**, and a boot script) makes the VM print
     `COMEBYE-HOSTKEY ssh-ed25519 ...` to its serial console on every boot. The
     tool reads it with `aws ec2 get-console-output` (IAM-authenticated, so the
     same trust anchor as GCE's; it appears after a minute or two and is polled)
     and pins it per instance id. User-data is readable through IMDS and
     `DescribeInstanceAttribute`, so **no secret ever goes in it**: the
     Tailscale key still travels over ssh stdin.

   Either way the tool writes a private `ssh_config` and `known_hosts` with
   `StrictHostKeyChecking yes`.
3. Over that ssh: packages (`git jq bubblewrap socat curl`), Tailscale from its
   apt repository, linger, and the idle timer. Every five minutes it powers the
   VM off after 120 minutes with no ssh session and no herdr agent `working`
   (a guest poweroff stops the instance on both clouds; `start` brings it back).
   **Max run time:** GCP enforces `max_run_duration` itself. EC2 has no such
   cap, so on AWS a **guest-side systemd timer** powers the VM off that long
   after each boot. It is **advisory**: root on the VM can defeat it, so the
   idle poweroff is the real backstop. Set `max_run_duration=` (empty) to turn
   either off.
4. **Tailnet.** With the OAuth client: delete an *offline* device with the same
   hostname and `tag:worker` (so the new node is not renamed `NAME-1`; an
   untagged device is never touched), then mint a **single-use, pre-authorized,
   `tag:worker` key that expires in 10 minutes**. The key goes to
   `tailscale up --auth-key=file:/dev/stdin` over ssh **stdin**: it is never in
   an argument list, cloud metadata or user-data, a file, the repo or a log
   (only its id is printed). Then `tailscale serve --bg 8741`.
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

## What `destroy` removes

After the typed confirmation: the agents are signed out, the node leaves the
tailnet (`tailscale logout`, then the API delete of the device with that exact
hostname **and** tag), the hub's peer entry is removed, and the instance is
deleted (terminated, on AWS). The **data disk or volume is kept** unless you
pass `--delete-data-disk`.

Then the tool cleans up the network it made, on both clouds, but only when
**every** piece carries the `managed-by=comebye-worker` marker and **nothing
else uses the network**: no other instance (managed or not) and, on AWS, no
network interface. Otherwise it deletes nothing and prints why
(`network ... kept: instance other still uses it`, or `... is not marked ...
(not made by this tool)`). A network from before this marker existed is
therefore left alone. Nothing outside the marked set is ever deleted.

## Cost

Approximate list prices; check your own region and account.

| | GCP (us-central1) | AWS (us-east-1) |
|---|---|---|
| Default VM | e2-standard-4: about **$0.134/h** | t3.xlarge: about **$0.166/h** |
| Public IPv4 | about $0.005/h while running | $0.005/h while running |
| Disks (20 GB boot + 50 GB data) | pd-balanced, about $7/month | gp3 at $0.08/GB-month, about **$5.60/month** |
| While stopped | disks only | EBS only (no compute charge) |
| Network pieces | free | VPC, subnet, IGW, security group: free (no NAT gateway is made) |

A first smoke test (one worker for 1 to 2 hours, then `destroy
--delete-data-disk`) costs about **$0.30 on GCP** and about **$0.35 on AWS**.
A forgotten running worker is the real cost, which is what the idle timer and
the max-run cap are for. `worker status` (no name) lists everything the tool
created.

## Limits

- The bootstrap is written for Debian and Ubuntu images (apt), amd64 only.
- On AWS, the host key comes from the console, which can lag by a few minutes
  after boot; `create` and `start` poll for it.
- The herdr, Claude and Codex installers are third-party scripts. Pin herdr's
  with `herdr_install_sha256`; the agent installers are not pinned.
- `destroy` signs the agents out only if the VM is running. A stopped VM is
  deleted with its node removed through the API.
