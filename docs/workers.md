# Worker VMs

`tools/worker` builds a worker VM on Google Compute Engine **or AWS EC2**
(`provider = gcp|aws`), or a sandbox on an **OpenShell gateway**
(`provider = openshell`, see [OpenShell sandboxes](#openshell-sandboxes)), puts it on your tailnet as a **tagged, persistent** node (`tag:worker`) and lets it join the
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
| `create NAME [--dry-run] [--comebye-version vX.Y.Z] [--spec FILE\|-] [--events]` | Builds the VM, enrols it, bootstraps it, waits for the hub. Safe to re-run: it resumes where it stopped and never mints a second key for a node that is already enrolled. `--dry-run` prints the plan and touches nothing. `--spec` and `--events` are for a caller that is a program: see [Driving the tool from a program](#driving-the-tool-from-a-program). |
| `status [NAME]` | One worker: GCE state, tailnet address, comebye version and units, the tailnet device, the hub's view (with a version-mismatch warning). Without a name: every labelled instance and disk and every `tag:worker` device. |
| `stop NAME` / `start NAME` | Stop or start the VM. The tailnet node and the hub's peer entry stay (the hub shows it unreachable). `start` re-reads the host key from GCE (the external IP changes) and waits for comebye. |
| `archive NAME` / `archive --all` | Copies a running worker's sessions to the transcript archive, and verifies them. Safe to repeat, and leaves the agents running. `--all` covers every running worker and skips stopped ones. See [Sessions and the archive](#sessions-and-the-archive). |
| `destroy NAME [--delete-data-disk] [--no-archive] [--yes]` | Asks you to type the name (`--yes`: the caller has already confirmed, so it does not ask). Saves the worker's sessions to the archive, and **refuses** if it cannot. Then signs the agents out, `tailscale logout`, deletes the tailnet device (only one with the exact hostname **and** the tag), removes the hub's peer entry, deletes the VM. The data disk is kept unless you pass the flag. `--no-archive` skips the save, and the sessions are lost. |
| `preflight [--json]` | Read-only. Are the credentials this config needs usable: `aws`, `gcp`, `tailnet`, `openshell`, `ssh`, each `ok`, `missing`, `expired` or `error` with a one-line note (a profile or gateway name, never a key, token or account id). |
| `catalog [--kind aws] [--region R] [--families f1,f2] [--types t1,t2] [--score t1,t2] [--refresh] [--json]` | Read-only. The instance table: vCPU, memory, arch, GPUs, spot price per zone, on-demand price, AWS Spot Advisor interruption band, and an estimate (price + $0.005/h IPv4 + disks at $0.08/GB-month). Arm rows are listed but `selectable: false`. See [Prices](#prices-and-the-read-only-commands). |
| `status --json [NAME]` | Read-only, cloud API only: state, launch time, last stop, type, mode (spot/on-demand) and the caps, for one worker or all of them. |
| `login NAME claude\|codex` | Runs the agent's own sign-in on the VM over ssh. You open the printed URL in your browser; the credential is created on the VM and nothing is copied from your machine. |

## Driving the tool from a program

Everything below is opt-in; with none of these flags the tool behaves as before.

- **`--events`** (create, destroy): stdout carries one JSON line per stage
  transition and nothing else; the text a person reads moves to stderr.
  `{"v":1,"seq":12,"stage":"instance","step":"4/9","state":"running","msg":"..."}`.
  `state` is `running`, `ok`, `failed`, or `planned` (a `--dry-run` lists every
  stage that would run). The stages of a VM are `network`, `ssh_key`,
  `data_disk`, `instance`, `ssh`, `bootstrap`, `tailnet`, `agents`, `hub`; a
  sandbox has `sandbox`, `tailnet`, `settings`, `hub`. A `failed` event carries a
  `class` the caller can act on: `spot_capacity` (with the `zone`),
  `creds_expired`, `quota`, `bootstrap_failed`, `needs_input` or `other`. Only
  those fields exist, every message is one short line, and keys, tokens and
  bearer strings are removed before a line is written. A problem found before
  any stage (a bad spec or config) is a `failed` event for the stage `config`.
- **`--spec FILE|-`** (create): one JSON request laid over the config. Fields
  (all optional): `kind` (`gcp`, `aws`, `openshell`), `name`, `type`, `spot`,
  `zone`, `boot_disk_gb`, `data_disk_gb`, `idle_minutes`, `max_run` (`"24h"`, or
  `""` for none), `agents`, `llm_key` (`false` leaves the gateway key out; `true`
  needs `llm_gateway_url` in the config), `budget_cap_usd`,
  `destroy_after_hours`. **Unknown fields are refused**, and the result passes the
  same checks as the config file. What is per node stays in the config: profile,
  project, `hub_url`, `ts_allow_login`, `ssh_source_cidr`. The name may come from
  the spec (`create --spec -`); if both are given they must match. A sandbox has no
  machine, zone or disks, so those fields are refused for `kind: openshell`.
- **A zone per request (AWS).** `zone` need not be `aws_az`. The zone the config
  names keeps its subnet `comebye-worker-subnet` (`subnet_range`); any other zone
  gets its own subnet, made on first use, named `comebye-worker-subnet-<zone>` in
  a /24 inside `vpc_range` (needs a /17 or larger; the default /16 is fine). A data
  disk is bound to its zone, so a host built in one zone resumes there. `destroy`
  removes every subnet the tool made once nothing uses the network.
- **`--no-prompt`** (global, or after the command): nothing reads a terminal.
  The destroy confirmation, the hidden Tailscale-key prompt and the interactive
  `login` become a `needs_input` error. `destroy --yes` is the way for a caller
  that did its own confirmation; with neither flag the prompts work as before.

## Prices and the read-only commands

`preflight`, `catalog` and `status --json` make only read calls, so a node can
use them with a narrow IAM policy: `tools/worker/iam/readonly-policy.json` is a
draft (nothing in this repo creates it). `catalog` needs no AWS login for its
main columns, which come from public data and are labelled so in the app:
specs and on-demand prices from AWS's Price List bulk file for the region
(streamed: a few hundred MB are downloaded and only Linux shared-tenancy
instances of the allowed families are kept; 24 h cache), the region-wide spot
price from the feed behind AWS's spot-pricing page (`spot.js`, undocumented, so
the column is empty rather than wrong if its shape changes; 15 min) and the
public Spot Advisor data file (24 h; also not an API with a contract). With a
login it adds `ec2 describe-spot-price-history` for per-zone prices (15 min)
and, with `--score`, `ec2 get-spot-placement-scores` for a shortlist of at most
10 types (30 min); those are optional sources, and their failure does not mark
the catalog degraded. Each source is cached in `<state_dir>/provision/catalog-aws-<region>.json`
with its own `fetched_at`; a refresh that fails (login expired, no network)
serves the cache and sets `degraded` (`creds_expired` or `error`) with the
source marked `stale`; with no cache at all it fails. One refresh runs at a
time. Which families are priced: `--families`, else `allowed_families`, else a
short default list.

`tools/worker/testdata/aws-shim/aws` is a recording shim: put its directory on
`PATH` with `AWS_SHIM_LOG=FILE` and it logs every call, refuses (exit 99) any
call that is not a read, and passes the rest to the real `aws` (or, with
`AWS_SHIM_FIXTURES=DIR`, answers from canned files, which is how the tests and
a no-account run use it).

With `budget_cap_usd` or `destroy_after_hours` in a spec, `create` also writes
the caps and the launch price as tags on the AWS instance
(`comebye-budget-cap-usd`, `comebye-destroy-after-hours`,
`comebye-launch-price-usd-hour`), and `status --json` reads them back. The
price is read just before launch and left out if it cannot be read.

## Browsing prices from the app (a provisioner)

Peers → Prices in the app shows what a provisioner can build and what it
would cost, read-only. Nothing there creates a cloud resource; adding a host
from the app comes later.

- **The provisioner** is a peer that holds the cloud login. Build the tool
  (`go build -o ~/.local/bin/comebye-worker ./tools/worker`), give it a config
  (`~/.config/comebye-worker/config`, see `tools/worker/config.example`:
  `aws_profile` should be a dedicated narrow profile, `aws_region` its own
  region, `allowed_regions` the regions the app may ask about), and start the
  peer's comebye with `-provision-config ~/.config/comebye-worker/config`
  (`COMEBYE_PROVISION_CONFIG`; `-provision-worker` or `COMEBYE_PROVISION_WORKER`
  names the binary, default `comebye-worker` on `PATH`). It needs `-ledger=false`
  and `-hub` like every peer, and `-ts-allow-login`. The tool runs there as a
  subprocess with a filtered environment (PATH, HOME, `AWS_*`, `CLOUDSDK_*`
  and the like; none of comebye's own variables), in its own process group,
  with a timeout.
- **The hub** sets `-provision-host NAME` (`COMEBYE_PROVISION_HOST`), the default
  provisioner, which must be a peer it already knows (as for `-release-host`).
  Every other peer that answers is listed too and can be picked in the app;
  with no setting and no peer that answers, the Prices tab is hidden. The hub
  holds no cloud credential.
- **What the app shows**: per credential on the chosen node, `present`,
  `missing`, `expired` or `could not check` with a one-line note (a profile or
  gateway name), never a value; a region picker (the node's own region first,
  then `allowed_regions`); the instance table with the price of each type, spot
  and on-demand, the AWS Spot Advisor band and an estimate per hour. Sizes
  under 2 vCPU or 4 GiB are hidden unless "Show small sizes" is ticked, and a
  row opens to its spot price per zone and a "Check capacity" button (AWS's
  placement score for that type). Arm types are listed and marked "needs work".
- **Caching**: the node caches each source on disk (types 24 h, spot 15 min,
  on-demand 7 d, advisor 24 h; see above) and the hub caches each answer for 5
  minutes. If the node cannot refresh (an expired login), the app keeps showing
  the last prices with their age and says what to run (`aws login` on that
  node). "Recheck" and "Refresh prices" ask again now (a POST with the page's
  CSRF token; one forced refresh per 20 s).
- **Who may ask**: the user role only. An agent token, a peer forwarding for an
  agent and a tagged node get 403 on every route; the peer answers only its hub.
  See [security.md](security.md).

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
   in the path. A worker can do what its workers need (`comebye report`,
   `comebye prompt` to a coordinator or an agent on its own host, adding a
   to-do, the ledger in outline) and register itself; see
   [what a tagged worker reaches](security.md#what-a-tagged-worker-reaches). It
   cannot read other agents' output or transcripts, hand off or mark, or do
   anything the user alone can (keys, answers, launching, removals, the peer
   registry), which Serve answers with 403 or 401. A coordinator cannot run on a
   worker VM.

   Without `COMEBYE_PEER_SELF_REGISTER=1` the tool finishes with the exact
   `comebye peer add NAME https://NAME.<tailnet>.ts.net --tag tag:worker` to
   run on the hub.

   The old peer listener (`COMEBYE_PEER_LISTEN`, `COMEBYE_TS_ALLOW_TAGS`, port
   8742 and its firewall opening) was removed. A worker config with
   `hub_peer_url=http://...:8742` is refused with a message saying so.
   A hub that is unreachable at install time fails the worker's
   `comebye upgrade` smoke check, which asks the hub for the worker's own
   agents.
5. **Config** at `~/.config/comebye-worker/config` (copy
   [`tools/worker/config.example`](../tools/worker/config.example)). The
   required keys are `provider` (default `gcp`), then for GCP the project and
   zone, or for AWS `aws_region` and `aws_az`; and for both your workstation's
   `ssh_source_cidr`,
   `ts_allow_login` (the login that owns the hub device, which the worker
   admits the hub's requests as), `hub_url` (the hub's Serve URL, used to read
   its version and remove the peer entry) and `hub_peer_url`, which becomes
   `COMEBYE_HUB` on the worker: the same Serve URL
   (`https://hub.<tailnet>.ts.net`). Everything else has a default: `e2-standard-4` (GCP) or `t3.xlarge` (AWS),
   20 GB boot, 50 GB data disk (`pd-balanced` or `gp3`), Debian 12 (amd64, or
   arm64 for a Graviton type such as `t4g.medium`), auto-stop after 120 idle minutes.
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
   VM off after `idle_minutes` with no interactive ssh session and no herdr agent
   `working` (a guest poweroff stops the instance on both clouds; `start` brings it back).
   What counts as activity: an ssh session with a pty (`ssh -t`, `worker login`),
   and an agent whose herdr state is `working`. Bare ssh connections do not count,
   so herdr's saved-machine connection (a ControlMaster or bridge, no pty) cannot
   keep the VM up; neither does a herdr TUI merely attached (herdr exposes no
   "client attached" signal; typing makes the agent `working`). `idle`, `done`,
   `unknown` and `blocked` agents do not count: a blocked agent waits on you, and
   `start` brings the VM back. If herdr is up but does not answer, the VM stays
   up. Each tick logs its decision: `journalctl -t comebye-idle` on the VM. Run
   `sudo WK_IDLE_DRY=1 /usr/local/sbin/comebye-idle-check` to see the decision
   without changing anything. Re-running `create NAME` re-installs the script
   on an existing worker (step 6 always runs), so a worker built earlier gets
   this rule that way.
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
   It also runs `herdr integration install` for pi, claude and codex. These are
   hooks only; they are what lets herdr report an agent's session file and state,
   without which the comebye app shows raw terminal output. herdr refuses unless
   the agent's config directory exists, so the bootstrap creates `~/.pi/agent/extensions`,
   `~/.claude` and `~/.codex` first. A failure fails the step. `worker status NAME`
   shows each integration's state (`current (vN)` or `not installed`).
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
sessions, but what it wrote is still untrusted text when `recall` shows it. `destroy`
finishes by starting `comebye-archive.service`, if this machine has it, so `recall`
and the app's search find the sessions within minutes. Otherwise they appear after
the next `archive index`.

Only this machine uploads a given worker: a second machine is refused, because the
archive's safety depends on one writer per host.

**What this does not cover:** a VM deleted outside this tool, for example in the
cloud console, loses whatever was not yet copied. `worker archive --all` on a timer
limits that loss. It is not installed for you:

```ini
# ~/.config/systemd/user/comebye-worker-archive.service
[Service]
Type=oneshot
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin
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
at this rate (an estimate; most have far less). A pull is not an interactive session, so it does not
delay the idle poweroff; a tick that lands mid-pull on an otherwise idle worker past its limit could stop the VM, which `archive` reports as an ssh failure to repeat.
Codex sessions are not archived: the archive knows only Claude and pi.

## OpenShell sandboxes

`provider = openshell` makes the worker a **sandbox** on an [OpenShell](https://github.com/NVIDIA/OpenShell)
gateway instead of a VM. The same commands work (`create`, `status`, `stop`, `start`,
`archive`, `login`, `destroy`); the tailnet join, the hub registration, the archive pull and
the order of `destroy` are the same code. Only the resource steps and the transport differ.

**Two planes.** The gateway carries the *management plane*: create, exec, stop, start, delete.
The tool shells out to the `openshell` CLI (JSON output), which holds *your* login; the tool never
reads, stores or prints the gateway credential, and the sandbox never holds one. comebye's *own
traffic* stays on the tailnet: the sandbox runs a stock tailscaled in userspace mode as a
`tag:worker` node, so the hub, ACLs, `tsauth` and the archive host name work as for a VM.
A sandbox has no ssh and no sudo; every step is `openshell sandbox exec`, with secrets on stdin.

**Requirements**

- The gateway must use the **Kubernetes driver**. A podman sandbox has only a loopback interface
  and cannot join the tailnet.
- The image, the init (`comebye-sandbox-init`) and the policy template come from `tools/worker/openshell/`:
  [README.md](../tools/worker/openshell/README.md) is the contract between the init and this tool (the
  settings file and its keys, what the init runs and what the tool runs); [RUNBOOK.md](../tools/worker/openshell/RUNBOOK.md)
  covers the gateway, the providers and the Claude login.
  The image must hold the comebye release the hub runs; `sandbox_image` takes `{version}`.
- comebye in the image must be able to reach the hub through tailscaled's SOCKS5 listener
  (`127.0.0.1:1055`): the sandbox cannot resolve tailnet names itself.
- `openshell gateway login NAME` on a user-owned tailnet node, then `status` shows
  `Authentication: Ok`. When the login has lapsed, the tool says so and names the command.
- The gateway providers the sandbox uses (`sandbox_providers`: model keys, a GitHub token) are
  made once by an admin. They hold the real secrets; the sandbox sees placeholders.
- `sandbox_policy` is **required**: render the image's `policy.yaml.tmpl` with
  `tools/worker/openshell/render-policy.sh > policy.yaml`. The
  gateway's default policy denies the Tailscale hosts and the PTY paths, and only the *network* part
  of a policy can be changed on a live sandbox (`openshell policy set`), so a sandbox created
  without it can never join the tailnet or run herdr panes: delete it and create it again.

```ini
provider=openshell
openshell_gateway=homelab
openshell_workspace=openshell-sandboxes      # required; the gateway may refuse "default"
sandbox_image=registry.example/comebye-sandbox:{version}
sandbox_policy=~/.config/comebye-worker/sandbox-policy.yaml
sandbox_providers=anthropic-claude,github-push
sandbox_cpu=2
sandbox_memory=6Gi
idle_minutes=120
ts_allow_login=you@example.com
hub_url=https://hub.your-tailnet.ts.net
hub_peer_url=https://hub.your-tailnet.ts.net
```

`project`, `zone` and `ssh_source_cidr` do not apply. Use `openshell_endpoint=https://...`
instead of `openshell_gateway` to connect to a gateway that is not registered with the CLI.
`llm_gateway_url` is refused: a sandbox cannot reach the tailnet's LLM gateway, so give it a
provider on the OpenShell gateway.

**What `create` does** (four steps, resumable):

1. `openshell sandbox create --name NAME --from IMAGE --label managed-by=comebye-worker
   --policy P [--provider X]... [--cpu C] [--memory M] --detach -- comebye-sandbox-init`, then
   waits for the phase `Ready` and for exec to answer. The gateway creates the volume. A sandbox
   of that name that lacks the label is never touched.
2. Waits for tailscaled's socket, clears a stale offline `tag:worker` device of that name, mints a
   single-use, pre-authorized `tag:worker` key (10 minutes; `ts_key_minutes`, and
   `ts_key_ephemeral` for tests) and runs `tailscale up` over exec **stdin**. Tailscale Serve is the
   init's job.
3. Writes `~/.config/comebye/env` over exec stdin (mode 600, written atomically; the init waits for
   the file): `COMEBYE_HOST_NAME`, `COMEBYE_TS_ALLOW_LOGIN`, `COMEBYE_HUB`, `COMEBYE_ANNOUNCE=1`,
   `COMEBYE_PUSH=false`, the launch and worktree roots under the sandbox's `$HOME`, and
   `SBX_IDLE_MINUTES`.
4. Waits for the hub to list the worker, and tells the hub the **sandbox id** as its instance.

**Differences from a VM**

| | VM | Sandbox |
|---|---|---|
| Archive host | `NAME.INSTANCE_ID` | `NAME.SANDBOX_ID` (the gateway's immutable id; it survives stop and start) |
| Idle stop | poweroff timer | the init exits 0 after `idle_minutes` with no working agent; the sandbox goes to `Completed`. `status` shows it as stopped, and `start` resumes it |
| `stop` / `start` | VM stop / start | `openshell sandbox stop` / `start`. `/sandbox` (the volume) is kept; only that is promised, not `/tmp`. The init brings the whole stack back and the node keeps its identity |
| `destroy` | the VM; the data disk is kept | the sandbox **and its volume**; `--delete-data-disk` is refused. A `Completed` or stopped sandbox needs `start` first, or `--no-archive`, because its sessions are on the volume |
| Agent stop for the final archive | `systemctl --user stop` | `herdr server stop`; the init restarts herdr, so a failed archive leaves the sandbox as it was |
| `login` | ssh `-t` | `openshell sandbox exec --tty` |

`destroy` runs in the VM's order: save the sessions (agents running), stop the agents, save again,
sign the agents out, `tailscale logout`, delete the tailnet device (exact hostname **and** tag),
remove the hub's peer entry, then `openshell sandbox delete`, which removes the volume too.

**Known limits.** Every request between the hub and a sandbox goes through a DERP relay (UDP is not
available in a sandbox): fine for comebye's polling and prompts, not for bulk transfer. The CLI and
the gateway are 0.1.x and change; the tool depends on `sandbox list/start/stop/delete/exec/create`
and the `-o json` fields `id`, `name`, `labels`, `phase` and `created_at`.

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

- The bootstrap is written for Debian and Ubuntu images (apt). On AWS a
  Graviton type (`t4g`, `m6g`, `c7g` ...) boots the arm64 Debian 12 image
  (`/aws/service/debian/release/12/latest/arm64`); any other type boots amd64.
  GCE stays amd64.
- On AWS, the host key comes from the console, which can lag by a few minutes
  after boot; `create` and `start` poll for it.
- The herdr, Claude and Codex installers are third-party scripts. Pin herdr's
  with `herdr_install_sha256`; the agent installers are not pinned.
- `destroy` signs the agents out only if the VM is running. A stopped VM (which
  takes `--no-archive`) is deleted with its node removed through the API.

## Creating a host from the app (dry run first)

The app's Add host (Peers → Prices) sends a closed spec (`kind`, `name`, `type`,
`spot`, `zone`, `boot_disk_gb`, `data_disk_gb`, `idle_minutes`, `max_run`,
`budget_cap_usd`, `destroy_after_hours`) to the provisioner, which validates it
again, holds it to its ceilings (`allowed_families`, `max_budget_cap_usd`, and
on the comebye side `-provision-max-usd-hour` from its own price list) and runs
`worker create --events --spec -` as a subprocess, one job at a time, in its
own process group with a 30 minute limit. The journal of each job (the stage
events and the tool's log lines, secrets removed) is a file in the state
directory. **Until the provisioner is started with `-provision-create` (or
`COMEBYE_PROVISION_CREATE=true`) every job runs `create --dry-run`**: the plan
as events, nothing created, no credential needed. The limits live in comebye
and the worker tool, not in AWS: the provisioner uses the node's existing AWS
profile, so there is no separate IAM policy. For a real create the guards are:
every resource the tool makes is tagged `managed-by=comebye-worker` and
destroy refuses anything without it; the instance family must be in
`allowed_families` and the region is the node's own (a request naming another
is refused); the budget cap is required and held to `max_budget_cap_usd`; the
estimated cost per hour is checked against `-provision-max-usd-hour` from the
node's own price list, and a cost that cannot be priced is refused; at most
`-provision-max-hosts` (default 3) real hosts per provisioner; one job at a
time; and the app's review step shows the cost and needs an acknowledgement
that the hub requires again server-side (`ack`). Turning the real path on
(`-provision-create`) is the user's separate decision.

### Spend and Destroy…

A real create puts the host on the provisioner's books (`<state-dir>/provision/hosts.json`)
as soon as the instance exists, and keeps the record after destroy for the final
bill. Every five minutes the provisioner asks the cloud (`worker status --json`)
for each host's launch time, last stop time and the launch price recorded in the
instance's tags, and the app's Prices tab shows, per host and in total, what it has
cost: the running hours (launch to stop, as the cloud reports them) times the
compute price plus the public IPv4, plus the disks from creation to destroy. It
is an estimate labelled "about", never the cloud's bill (Cost Explorer lags a
day and each call costs). After a destroy the figure is frozen, and a data disk
that was kept is shown as still costing about its monthly price (not part of the
bill).

**Destroy…** asks for the host's name to be typed, then the node that built the
host runs `worker destroy NAME --yes --events`: it saves the worker's sessions
to the archive first and refuses, changing nothing, if it cannot; then it revokes
the LLM key, leaves the tailnet, removes the peer entry and deletes the instance.
The data disk is kept unless "also delete" is ticked. A stopped host cannot be
read, so its sessions can only be saved by starting it first or by ticking
"do not save its sessions" (they are lost). A node refuses a name it did not build.

### Safety timers (budget cap, idle, destroy after stopped)

Every host built from the app carries its limits in three places, and each one
is enough on its own:

| Control | Runs on | When the provisioner is offline |
|---|---|---|
| Stop after idle (default 2 h) | the host (a systemd timer) | works |
| Longest single run (24 h) | the host's guest timer | works |
| Budget backstop | the host's guest timer: at create the tool sets the max run to the smaller of `max_run` and cap ÷ the on-demand price + IPv4 (the highest rate a host of that size can pay; per boot, at least 30 min) | works |
| Budget cap, exactly: to-do at 80%, **stop** at 100% | the provisioner, every 5 minutes, from its spend ledger; the cap is read from the instance's tag `comebye-budget-cap-usd` (the lower of the tag and what was asked for) | only the backstop applies; the hub raises a to-do after 15 minutes of silence |
| Destroy after stopped (default 3 days) | the provisioner (`stopped_at` comes from the cloud's own stop time); it starts the host, saves its sessions, then destroys it, and does nothing if it cannot start | waits, and catches up on return |

A stop never loses data (the disks stay); only destroy does, and a budget cap
never destroys. A timer destroy that fails (the host would not start, the archive
refused) leaves the host and is not repeated for 6 hours, since each try starts
the host. The provisioner writes `provision.budget_warn`, `provision.budget_stop`
and `provision.auto_destroy` lines to its journal. The hub reads what the
provisioners recorded and raises one to-do each for: 80% of a cap, a stop at the
cap, an automatic destroy (done or failed), and a provisioner with live hosts
that has not answered for 15 minutes. A restart of the hub does not raise them again.

### OpenShell sandboxes from the app

The Add host wizard offers a second kind, **OpenShell sandbox**, when the node's
preflight says its gateway login works (`openshell` ok, plus the tailnet client; a
sandbox needs no ssh key). A sandbox has no machine type, disks, zone, spot price or
budget cap, so the spec holds only `kind`, `name`, `idle_minutes` and
`destroy_after_hours` (the tool is handed exactly those four fields and refuses
anything more), and the wizard's size step shows instead what the node will make it
from, read from its own worker config through `preflight --json`: the gateway,
workspace, image, the policy file's *name* (never its path), CPU and memory. The
job is the same one a VM goes through (plan, journal, retry, clean-up) with the four
sandbox stages (`sandbox`, `tailnet`, `settings`, `hub`); Destroy… and the destroy
after stopped clock work as for a VM, and the sandbox's sessions are saved first.
Nothing is priced: the Hosts list says "not priced" and the plan says there is no
per-hour price.

**No capacity figure.** After a gateway login (read-only, 2026-11-12) the
`openshell` CLI reports the gateway's health, version, auth, compute driver and
extensions (`status`, `gateway info`) and the sandboxes of the workspace
(`sandbox list`), and nothing about capacity, quota or free resources. The app
therefore shows the one load figure there is, how many sandboxes are already on
the gateway (from the preflight), and says it has no capacity number; a gateway
that is full fails the create and the failure shows the gateway's own error. The
create, plan and destroy of this kind were exercised against the tool's dry run
and stand-ins; no sandbox was created on the live gateway.

### Google Cloud VMs from the app

A node whose `gcp` preflight is ok (and which has the tailnet client and an ssh
key) offers **Google Cloud VM** in the wizard. It builds in the config's `zone`
(its region is `preflight.gcp_zone` without the zone letter; a request for
another is refused, as with AWS), prices come from `catalog --kind gcp`, and the
job, the stages, the safety timers, spend and Destroy… are the VM path's. The
spec is the VM's: a machine type like `e2-standard-4`, `spot` (a **Spot VM**,
`--provisioning-model=SPOT` with `--instance-termination-action=STOP`, so a
reclaim stops the VM and keeps its disks, like a persistent AWS spot request),
`zone`, the disks, `idle_minutes`, `max_run` (the platform's own
`--max-run-duration`), the budget cap and `destroy_after_hours`. The config key
`gce_spot` is the default for a request that does not say.

**The prices are estimates, not live.** `catalog --kind gcp` lists the
machine types of the zone with `gcloud compute machine-types list` (read-only)
and prices them from a table built into the tool: Google's list prices for
us-central1 for e2, n2, n2d and t2d, per vCPU and per GiB, a Spot VM at about
40% (35% for n2) of on-demand, pd-balanced disks at $0.10 per GB-month and
$0.005 an hour for the external IPv4. Google's real price list needs the Cloud
Billing API and a login the tool does not ask for, and a Spot VM's price moves
without notice, so the app says "estimated" and the catalog's `sources` say "not
live". There is no capacity score (Google reports none), no budget tag on the
instance (the cap is enforced from the spec and the provisioner's ledger), and
spend uses the estimated launch price. Types of other families, and Arm
(`t2a`, `c4a`), are not listed.

This kind was built against the dry run and recorded `gcloud` output only: the
gcloud login on the machine it was developed on had expired (`invalid_grant`), so
nothing was created on Google Cloud.

### Limits live in comebye; a narrow AWS profile is optional

The provisioner uses the node's existing AWS profile (`aws_profile`, the default
one if unset). The decision (2026-11-12) is not to require a narrower IAM profile,
so the limits are comebye's own, and each has a test: `-provision-max-usd-hour`
(priced from the node's own list, a cost that cannot be priced is refused), the
allowed instance families (`allowed_families`), the region (the node's own),
`max_budget_cap_usd` and the required budget cap, at most `-provision-max-hosts`
real hosts, the `managed-by=comebye-worker` tag that destroy, stop and cleanup
insist on (`checkOwned`), one job at a time, and the typed name for Destroy.
Preflight never asks for a particular profile.

As an **optional extra layer**, `tools/worker/iam/write-policy.json` (with
`readonly-policy.json`) is a draft IAM policy for a dedicated profile: tagged
creates only, `RunInstances` only for the allowed families, changes only to tagged
resources, us-east-1 only (edit the region and `ACCOUNT_ID`). It was checked with
`aws iam simulate-custom-policy` (a read-only call) but has not been attached to
anything, and nothing here depends on it.
