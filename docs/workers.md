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
| `archive NAME` / `archive --all` | Copies a running worker's sessions to the transcript archive, and verifies them. Safe to repeat, and leaves the agents running. `--all` covers every running worker and skips stopped ones. See [Sessions and the archive](#sessions-and-the-archive). |
| `destroy NAME [--delete-data-disk] [--no-archive]` | Asks you to type the name. Saves the worker's sessions to the archive, and **refuses** if it cannot. Then signs the agents out, `tailscale logout`, deletes the tailnet device (only one with the exact hostname **and** the tag), removes the hub's peer entry, deletes the VM. The data disk is kept unless you pass the flag. `--no-archive` skips the save, and the sessions are lost. |
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

   Without that file the tool needs a single-use key tagged `tag:worker` that
   you minted by hand. It reads it from a mode-0600 file (`ts_authkey_file`,
   default `~/.config/comebye-worker/authkey`), else a hidden prompt, and with no
   terminal it announces that it is waiting and polls for the file for up to 20
   minutes. Write the file from your own shell so the key stays out of any
   transcript: `umask 077; read -rs k; printf %s "$k" > ~/.config/comebye-worker/authkey; unset k`.
   The file is deleted as soon as the key has been used. It then cannot clear stale
   devices or delete the node on `destroy` (it tells you to).
3. **Tailnet policy** (you apply it; the tool never edits the policy). A
   worker joins the hub through the hub's normal Tailscale Serve URL, admitted
   by an **app capability** granted to `tag:worker`; no extra port and no hub
   firewall change:

   ```jsonc
   {
     "tagOwners": { "tag:worker": ["autogroup:admin"] },
     "grants": [
       // you, your phone and the hub reach a worker's Tailscale Serve (the hub drives it)
       { "src": ["autogroup:member"], "dst": ["tag:worker"], "ip": ["tcp:443"] },
       // a worker reaches ONLY the hub's Serve, and holds the comebye capability there.
       // An app block alone opens no port, so it also needs ip.
       {
         "src": ["tag:worker"],
         "dst": ["comebye-hub"],   // a host alias or tag for the hub
         "ip":  ["tcp:443"],
         "app": { "github.com/jhjaggars/comebye/cap/peer": [{ "role": "worker" }] }
       }
     ]
   }
   ```

   The capability's name is yours to choose, as long as it matches the hub's
   `COMEBYE_TS_ALLOW_CAP`. Remove or narrow any allow-all rule: with one, a
   compromised worker reaches your whole tailnet. There is no rule from one
   worker to another. Needs Tailscale 1.92 or later on the hub.
4. **Hub setup** (its `~/.config/comebye/env`, then restart the hub's comebye
   once). See [Tagged workers](setup.md#tagged-workers-tagworker-vms) and
   [Security](security.md#tagged-nodes-through-serve). Have Serve forward the
   capability, and name it to comebye:

   ```sh
   tailscale serve --bg --accept-app-caps=github.com/jhjaggars/comebye/cap/peer 8741

   COMEBYE_HUB_MODE=1
   COMEBYE_TS_ALLOW_LOGIN=you@example.com
   COMEBYE_TS_ALLOW_CAP=github.com/jhjaggars/comebye/cap/peer
   COMEBYE_PEER_SELF_REGISTER=1
   ```

   Serve on 443 is terminated by tailscaled, so the hub's host firewall is not
   in the path. A worker can do the agent-role requests of its workers
   (`comebye report`, `handoff`, `prompt`, `mark`, the ledger and briefing
   reads) and register itself; it cannot do anything the user alone can (keys,
   answers, launching, removals, the peer registry), which Serve answers with
   403 or 401.

   Without `COMEBYE_PEER_SELF_REGISTER=1` the tool finishes with the exact
   `comebye peer add NAME https://NAME.<tailnet>.ts.net --tag tag:worker` to
   run on the hub.

   **Legacy: the peer listener.** Before the capability path, workers reached
   the hub on a second socket. It still works and can run alongside Serve:

   ```sh
   COMEBYE_TS_ALLOW_TAGS=tag:worker
   COMEBYE_PEER_LISTEN=8742
   ```

   with `hub_peer_url=http://hub.<tailnet>.ts.net:8742` in the worker config,
   an ACL grant `{ "src": ["tag:worker"], "dst": ["you@example.com"], "ip":
   ["tcp:8742"] }` (the port equal to `COMEBYE_PEER_LISTEN`), and **a host
   firewall on the hub that allows that port on `tailscale0`.** The ACL only
   decides what the tailnet permits; the hub's own firewall still sees the
   packets. On firewalld hosts (Fedora), `tailscale0` is in no zone and falls
   into the default zone, which usually allows only ssh, so the worker's
   `announce` fails with "connection refused" (instantly, unlike an ACL drop,
   which hangs) while Serve on 443 keeps working. Give the interface a zone
   that opens only the listener port:

   ```sh
   sudo firewall-cmd --permanent --new-zone=tailscale
   sudo firewall-cmd --permanent --zone=tailscale --add-interface=tailscale0
   sudo firewall-cmd --permanent --zone=tailscale --add-port=8742/tcp   # = COMEBYE_PEER_LISTEN
   sudo firewall-cmd --reload
   ```

   Moving the interface out of its old zone also drops whatever that zone
   allowed over the tailnet (for example ssh): add those services to the new
   zone if you use them. Check with `ss -ltn | grep 8742` on the hub (the
   listener is up) and `curl http://<hub tailnet ip>:8742/api/whoami` from a
   worker (reachable). A hub that is unreachable at install time also fails
   the worker's `comebye upgrade` smoke check, which asks the hub for the
   worker's own agents.
5. **Config** at `~/.config/comebye-worker/config` (copy
   [`tools/worker/config.example`](../tools/worker/config.example)). The
   required keys are `provider` (default `gcp`), then for GCP the project and
   zone, or for AWS `aws_region` and `aws_az`; and for both your workstation's
   `ssh_source_cidr`,
   `ts_allow_login` (the login that owns the hub device, which the worker
   admits the hub's requests as), `hub_url` (the hub's Serve URL, used to read
   its version and remove the peer entry) and `hub_peer_url`, which becomes
   `COMEBYE_HUB` on the worker: the same Serve URL
   (`https://hub.<tailnet>.ts.net`) for the capability path, or, for the legacy
   peer listener, `http://hub.<tailnet>.ts.net:8742`. Everything else has a default: `e2-standard-4` (GCP) or `t3.xlarge` (AWS),
   20 GB boot, 50 GB data disk (`pd-balanced` or `gp3`), Debian 12 amd64
   (Arm/Graviton types are refused), auto-stop after 120 idle minutes.
6. **The hub must run a release** (`vX.Y.Z`). The worker installs the hub's
   version from the release tarball; a hub on a development build is refused
   (pass `--comebye-version` or pin `hub_version`).

## pi and the LLM gateway

The default agent on a worker is [pi](https://github.com/earendil-works/pi-mono),
installed at the version in `pi_version` (pinned; bump it on purpose) and never
signed in. `agents=pi,claude,codex` adds the others.

With `llm_gateway_url` set (a LiteLLM proxy on the tailnet, `https://*.ts.net`),
`create` also gives the worker its own way to reach models, with no provider
account on the VM:

1. Before anything is built, the tool checks the gateway answers
   (`/health/liveliness`) and that `llm_provisioner_key_file` is mode 0600. A
   gateway that is down stops `create` with nothing created.
2. It mints a **per-worker virtual key** with `/key/generate`: alias
   `worker-NAME`, `user_id comebye-worker-provisioner`, and **always** an
   explicit `models` list, `max_budget`, `budget_duration` and `rpm_limit` (a
   LiteLLM key without `models` can use every model, so the tool refuses to mint
   without all four). The key has no expiry and is never renewed: it lives from `create` to
   `destroy`, and `stop`/`start` keep it. The provisioning key only manages keys and stays on your
   workstation.
3. The key goes over ssh **stdin** into the worker's `~/.pi/agent/auth.json`
   (mode 0600, provider `homelab-enmaas`, `type: api_key`), merged with whatever
   is there. It is never in an argument list, user-data, a log line, the repo or
   this tool's state. `models.json` (provider `homelab-enmaas` →
   `<gateway>/v1`, `openai-completions`, your models, no `maxTokens`: reasoning
   models spend a small output limit on thinking and return nothing) and
   `settings.json` (default provider and model) carry no secret.
4. `destroy` deletes the key **first** (by alias) and, if it can read the key
   back from the running VM, checks that the gateway now answers 401 to it. If
   the delete fails it warns, finishes the rest, and leaves the key for
   `worker keys --prune`.

`worker keys` lists the `worker-*` keys with whether a VM exists for each;
`--prune` deletes the keys whose VM is gone. It only touches keys it minted
(`created_by` and a scope of provider and region or project in the metadata), so
another operator's workers are never judged by your cloud, and it never deletes
a key whose VM it could not check.

Anything pi can use, the model's shell tool on that VM can read. The key is
therefore per worker, capped in budget, rate and models, and revoked at
`destroy`; the gateway's tailnet policy (`tag:worker` to the gateway on tcp:443
only) is what limits where it works from.

`worker login NAME pi` opens pi over ssh for `/login` to a provider of your own
(not needed for gateway workers).

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

## Sessions and the archive

A worker's sessions are on its boot disk (`~/.pi/agent/sessions`, and
`~/.claude/projects` if Claude is installed), which survives a stop or a spot
reclaim and is deleted with the VM. A worker holds **no archive key** and
`tag:worker` is denied the bucket, because an archive key can delete every host's
history. So the machine running this tool pulls the sessions over ssh and uploads
them itself, using the settings in `archive_env` (default
`~/.config/comebye/archive.env`, the file the [archive timer](archive.md) reads).

**The archive host is `NAME.INSTANCE_ID`**, for example `w1.i-0abc123def4567890`.
Worker names get reused, but an instance id does not, and a stop, start or reclaim
keeps it. `worker status NAME` prints it. The worker still calls itself `w1` to the
hub and in the ledger.

`destroy` does this after the typed confirmation, before anything else:

1. Pull and upload with the agents still running. If the bucket or the VM cannot be
   reached, it refuses here, with nothing changed.
2. Stop the agents.
3. Pull and upload again, now including any unfinished last line, and verify every
   file by sha256. If this fails, it restarts the agents, leaves the VM running and
   refuses. If the restart fails too, it says so.

| Situation | What `destroy` does | `--no-archive` |
|---|---|---|
| VM running, archive works | Saves, then destroys | Destroys; the sessions are lost |
| Bucket unreachable, `archive_env` missing or wrong | Refuses | Destroys; the sessions are lost |
| VM stopped, or reclaimed by AWS | Refuses: run `worker start NAME` first | Destroys; the sessions are lost |
| VM already gone | Continues: there is nothing to save | Same |

The tool treats the worker as untrusted while pulling. It unpacks only regular files
under the two session directories, refuses other paths, makes no links, and stops at
10 GiB or a million files. The copy sits in a mode-0700 directory under the state
directory and is removed after the upload. A worker can only affect its own host's
sessions, but what it wrote is still untrusted text when `recall` shows it. `recall`
on this machine finds the sessions after the next `archive index`.

Only this machine uploads a given worker: a second machine is refused, because the
archive's safety depends on one writer per host.

**What this does not cover:** a VM deleted outside this tool, for example in the
cloud console, loses whatever was not yet copied. `worker archive --all` on a timer
limits that loss. It is not installed for you:

```ini
# ~/.config/systemd/user/comebye-worker-archive.service
[Service]
Type=oneshot
ExecStart=%h/.local/bin/comebye-worker archive --all

# ~/.config/systemd/user/comebye-worker-archive.timer
[Timer]
OnCalendar=hourly
RandomizedDelaySec=300
Persistent=true

[Install]
WantedBy=timers.target
```

Each run sends the whole session directory, compressed, from every running worker.
On AWS, at $0.09/GB egress, a worker with 200 MB of sessions costs about $0.09 a day
at this rate (an estimate; most have far less). A pull overlapping the idle check's
5-minute tick counts as ssh activity, so it can only rarely delay the idle poweroff.
Codex sessions are not archived: the archive knows only Claude and pi.

## What `destroy` removes

After the sessions are saved: the agents are signed out, the node leaves the
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

### Spot instances on AWS (opt-in)

Set `aws_spot=true` (default `false`, on-demand) to launch the worker as a
**persistent spot instance with interruption behaviour `stop`**. Spot is
typically 60 to 70% cheaper than on-demand (check `aws ec2
describe-spot-price-history` for your type and zone; no price cap is set, so
you never pay more than on-demand).

- **Reclaim.** AWS gives a two-minute warning, then stops the instance. The
  root volume, the data volume and the Tailscale node state survive, and AWS
  restarts the instance by itself when capacity returns (the node keeps its
  identity; the public IP changes, the tailnet name does not). A running
  agent session does not survive the stop, so spot suits short or resumable
  work, not long unattended runs. `worker status` shows `stopped by AWS` when
  that is what happened.
- **`start` waits** for AWS to finish disabling the spot request after a stop
  (about 2 to 3 minutes; AWS refuses `start-instances` with
  `IncorrectSpotRequestState` until then), so a `start` right after a `stop`
  takes that long.
- **`start` may fail** with `InsufficientInstanceCapacity` or a spot quota
  error. Wait and run it again. The tool does not silently fall back to
  on-demand.
- **`destroy` cancels the spot request first**, then terminates; otherwise
  the persistent request would launch a replacement. A request that is not
  this instance's own is never touched.
- Spot has its own vCPU quota, separate from on-demand; a new account may
  start low.
- Everything else (IMDSv2, no instance profile, encrypted volumes, host-key
  pinning, idle poweroff) is unchanged. `provider=gcp` rejects `aws_spot`.

## Limits

- The bootstrap is written for Debian and Ubuntu images (apt), amd64 only.
- On AWS, the host key comes from the console, which can lag by a few minutes
  after boot; `create` and `start` poll for it.
- The herdr, Claude and Codex installers are third-party scripts. Pin herdr's
  with `herdr_install_sha256`; the agent installers are not pinned.
- `destroy` signs the agents out only if the VM is running. A stopped VM (which
  takes `--no-archive`) is deleted with its node removed through the API.
