# Proposal: one central transcript archive in Garage, with one index

Status: design decided; the first slice is approved and in progress. Written 2026-11-06 against main at `0c78560`.

The user's decisions: a dedicated bucket, **plaintext stays** (`encryption
none`), the bucket lives only in the homelab, storage is protected by Longhorn
backups to TrueNAS, one index for everything, and every session records which
host produced it. The later decisions are under [Decisions](#decisions).

## Design

- **A new single-node Garage** in its own namespace (`comebye-archive`), one
  bucket `comebye-archive`, on Longhorn with 3 replicas. It is not a bucket in
  the langfuse Garage.
- **Reached only through a Tailscale Ingress** (`tag:archive`). No Traefik host.
- **laharl and the hub write directly** with one S3 key each. The agents' session
  files are the buffer when Garage is down, so there is no local spool.
- **Spot workers hold no key.** `worker destroy` pulls their sessions over ssh to
  laharl, which uploads them under the worker's host name, before anything is
  deleted.
- **laharl builds the one index** from the whole bucket and publishes a snapshot
  to the bucket. The hub downloads it, so `comebye recall` keeps working the same
  way on both.
- **Durability:** Longhorn replicas for disk and node loss; the existing nightly
  `critical-backup` job to TrueNAS for everything else, and the user's encrypted
  Storj copy of that dataset for off-site.

```mermaid
flowchart LR
  L[laharl<br/>timer + flush] -->|S3 key| T
  H[hub<br/>timer + flush] -->|S3 key| T
  W[spot worker<br/>no key, no grant] -->|ssh tar at destroy| L
  T[Tailscale Ingress<br/>tag:archive] --> G[(Garage: comebye-archive<br/>v1/HOST/... notes/ tomb/ hosts/ index/)]
  G --> B[Longhorn 3 replicas<br/>nightly backup to TrueNAS]
  G -->|all hosts' segments| I[laharl: one index]
  I -->|index snapshot| G
  G -->|download snapshot| HI[hub: read-only copy]
```

## What the checks found

Some of these change the design, and some correct the brief.

| Finding | How it was checked | Consequence |
|---|---|---|
| **Garage v2.5.0 ignores `If-None-Match: *` on PUT.** A second PUT returned 200 and replaced the body. 40 of 40 concurrent writers "won", in each of 5 rounds. GET does honour the header (304). | Throwaway local container, with the header confirmed on the wire by `curl -v` | The comment in `backend.go` ("S3: If-None-Match: \*") does not hold on Garage. Create-only has to come from HEAD plus one writer per host. |
| **Permissions are per key and per bucket, and write includes overwrite and delete.** A write-only key could overwrite and delete any object, but could not List or Get. | Same container | A key cannot be limited to its own host's prefix. A write-only key would also break `archive run`, which lists to find where to resume. Hosts get read and write. |
| An interrupted PUT leaves nothing visible (HEAD 404). ETag equals the MD5 of the object. Keys with spaces, `+`, `=` and `ü` round-trip. | Same container | The `Put` atomicity contract holds. ETag gives an end-to-end integrity check. |
| `aws s3 sync` copied 2,600 synthetic segments in 2.8 s; a resync found 0 differences; listing them took 0.5 s. | Same container, on loopback | Migration needs no new code. Hourly listing cost is negligible. Loopback flatters the speed, so a real network will be slower. |
| **The langfuse Garage is not in-cluster only.** It has a Traefik Ingress, `garage.jaggars.cc`, and laharl gets a 403 from it. The name resolves to 192.168.1.100 even on 1.1.1.1. | `kubectl get ingress`, `curl`, `dig` | Corrects the brief. It is LAN-only: the user confirms there is no port forward, and the public DNS record points at a private address. |
| **rf 1 is not one physical copy.** All six langfuse Garage volumes have 3 Longhorn replicas and `critical-backup`. Last backup today, 08:00 to 08:02 UTC. | `kubectl get volumes.longhorn.io`, `backupvolumes` | Details under [Durability](#durability). |
| The recurring-job labels are live-only. They are on the Volume objects and appear nowhere in the homelab repo. The sccache volumes are not in the backup group. | `kubectl`, `grep` of the repo | A new Garage gets **no backup** unless this is done on purpose. |
| `threadSchema` starts with `DROP TABLE threads`, and `buildThreads` works on one host. | Read `thread.go` | Calling `Sync` once per host would leave only the last host's threads. Threads must become global. |
| `recall --host` already exists. `list` matches it exactly, `threads` as a substring (`--host lah` matches laharl), and `list` output does not print the host. | Ran the CLI | Small fixes, not new work. |
| **The app has no recall today:** no mention in `web/`, no `/api/recall`. | `grep` | "CLI and app" is CLI only for now. See [Index and search](#4-central-index-and-search). |
| Worker sessions live in `~/.claude` on the **boot disk** (the bootstrap only moves code and worktrees to `/data`). AWS spot and GCE run limits both *stop* the VM. | Read `tools/worker` | Sessions survive a reclaim or idle poweroff, and die at `destroy`. Pulling at destroy is enough, but a stopped VM must be started first. |
| The hub's direct tailnet path is a LAN address (192.168.1.222). | `tailscale status --json` | It should be fast. I could not log in to the hub (ssh denied), so its figures below are from the brief. |

## 1. S3 backend

Implement the existing `Backend` interface as `S3Backend` in `internal/archive`,
using the standard library with hand-written SigV4. It needs five calls (PUT, GET,
HEAD, DELETE, ListObjectsV2 with paging). `tools/worker` already talks to HTTP APIs
this way, and an SDK would be a large dependency for so little.

- **Put:** HEAD first, and a 200 returns `ErrExists`. Otherwise PUT with
  `Content-MD5`, and compare the returned ETag. It also sends `If-None-Match: *`
  and treats a 412 as `ErrExists`, so it stays correct if Garage starts honouring
  the header.
- **Why that is safe without a real conditional:** each host writes only under
  `v1/<host>/`. In `none` mode the same bytes at the same offset produce an
  identical object, so a lost race rewrites the same content.
- **The dangerous race is same host, different length:** the timer and the
  server's flush both start from offset X and cut different lengths. That leaves
  overlapping segments, and `shipFile` then refuses the stream forever. This
  exists today, because the timer's `flock` does not cover the server. Fix: take a
  host-wide lock inside `Archiver.run`.
- **Two machines claiming one host name:** the first run writes
  `hosts/<host>.json` with the machine id. A run from a different machine is
  refused unless `--adopt-host` is given.
- **Configuration:** `COMEBYE_ARCHIVE_S3=https://HOST/BUCKET`,
  `COMEBYE_ARCHIVE_S3_CREDENTIALS=FILE` (mode 0600), region `garage`, path-style,
  `https` only except on loopback. Setting both this and `--dir` is an error. The
  server's flush reads the same settings (`main.go:263`).
- **Plaintext gate:** `none` with an S3 backend also requires
  `COMEBYE_ARCHIVE_S3_PLAINTEXT=homelab`. A typo in the URL must not send
  plaintext to some other bucket.

### Fixes needed before the backend sits on a network

Each of these is harmless on a local disk and wrong over a network.

| Where | Today | Change |
|---|---|---|
| `ship.go`, `run` | A failed tombstone List is ignored, so a network blip **re-archives a session deleted on request** | Fail the run |
| `ship.go`, `Tombstoned` | Any error means "not deleted", and it is one GET per session per index run | List `tomb/` once and fail on error |
| `config.go`, `ReadMode` | Any Get error means "no marker" | Add `ErrNotExist`; other errors fail |
| `config.go`, `EnsureMode` | A lost first-write race returns `ErrExists` as a failure | Re-read and compare |
| `cli/archive.go`, `status` | Size comes from `FSBackend` | Optional size-aware listing on the backend |

### Tombstones, delete and verify

- **Delete becomes one operation.** `archive delete SESSION --host H --yes` writes
  `tomb/<host>/...` and removes the segments. The owning host's next run sees the
  tombstone and stops copying. This replaces the "do it in each place" step in the
  current doc. The index drops the session on its next sync, and the hub's copy on
  its next pull. **Longhorn backups keep it for up to 7 days**, until the last backup holding
  it is pruned. The Storj copy mirrors TrueNAS and keeps no older copies, so it is gone
  from off-site at the first Storj sync after that.
- **`archive verify`** works unchanged with `--host`: it compares the bucket with
  the session files still on that host.
- **New: `archive verify --all-hosts --no-source`** decodes every segment (magic,
  header against key, zstd, length) and reports gaps and overlaps. It is what proves
  a restored backup, and it is the only check for sessions whose source is gone.

## 2. Which Garage, and how it is reached

**A new Garage.** The cost is one small pod and a copy of `clusters/homelab/sccache/`.

| | New single-node Garage | Bucket in the langfuse Garage |
|---|---|---|
| Network path (decision 2) | Tailscale Ingress only | Already has a Traefik ingress, `garage.jaggars.cc`, reachable from the LAN. Decision 2 could not hold unless that is removed, and other clients of it would have to be checked first |
| Who can reach the transcripts through an admin credential | Only this Garage's own admin token | Holders of an admin token shared with langfuse and `cnpg-backups`. These are in SOPS now and were rotated today (homelab #312), after sitting in git in plaintext since 2026-05-29 |
| Tailnet exposure | Only this bucket's endpoint | The same endpoint as `cnpg-backups` |
| Node outage | The whole bucket waits for the volume to reattach | With rf 1 across 3 nodes, part of the key space fails until that pod returns |
| Risk to other things | None | A resize mistake stalls Flux and blocks `langfuse` (happened 2026-09-13); `meta-garage-0` is already at 2.5 of 4 GB (Longhorn's actual size) |
| Work | One homelab directory | One bucket and keys |

A key's write access covering its whole bucket is a separate point. It is the reason for
a dedicated bucket and for workers holding no key, wherever that bucket lives. It does
not favour one Garage over the other: a key on the langfuse Garage would not reach
`cnpg-backups` unless it was granted that bucket too.

Sizing: `longhorn-sata`, 40 Gi data and 5 Gi meta. Today's total is about 0.6 GB
stored; laharl's October was 1.9 GB raw and compresses about 5.5 times. That is
years of room at my rough estimate of up to 1 GB a month. Longhorn is thin
provisioned, and resizing is the two-step procedure that has already wedged Flux
once, so start large.

**Reach: Tailscale Ingress only**, as `llm-enmaas` does: `tag:archive`, no Traefik
host, no LAN name.

- Both hosts are devices of the user's own login, and the existing `*:*` rule
  already lets them reach a tagged node. **So no new grant is needed.**
- The policy PR adds `tagOwners` for `tag:archive` and a test that `tag:worker` is
  denied `tag:archive:443`.
- **A second tailnet user has `*:*`** too, so that account can reach the endpoint.
  It still needs an S3 key. The user accepted this with decision 2.
- **Keys:** `archive-laharl` and `archive-hub`, read and write, no owner, created
  from SOPS secrets as `sccache-key.yaml` does. There is no admin token on any host.

**Unverified risk:** SigV4 signs the Host header. Tailscale's source sets
`Out.Host = In.Host` (`ipn/ipnlocal/serve.go`), so this should work, but it has not
been run against Garage. Rollout step 2 tests it first, before any comebye cutover. If
it fails, a Traefik host like sccache's would work, but that reverses decision 2, so it
goes back to the user.

## 3. Encryption

None, by the user's decision. Nothing in the plan uses age, recipients or
identities, and `archive rewrap` is not needed. The `age` mode stays in the code
for anyone who wants it, and a later switch is still possible, because `rewrap`
copies to a new store.

One side effect: S3 object names and bodies are all plaintext, including project
paths and session ids. Access control is therefore the only protection, as the
wording below makes explicit.

**Proposed replacement for the rule in `docs/archive.md`:**

> A plaintext archive may live in an object-store bucket only if all of these hold:
>
> 1. The bucket is on a Garage inside the homelab, used for nothing else.
> 2. Its only network path is a Tailscale Ingress. No Traefik host, no public
>    name, no website access, and `tag:worker` is denied.
> 3. Its only credentials are one per archiving host, kept in mode-0600 files
>    outside any repository. Workers hold none.
> 4. Its volumes are in Longhorn's `critical-backup` group, and any copy that leaves
>    TrueNAS is encrypted by the TrueNAS job that sends it (today the Storj job on
>    `/mnt/Mirrored/longhorn-backup`). Nothing unencrypted goes to a cloud drive or to
>    another host's disk.
> 5. The plaintext index lives only on the indexing host and on hosts that download
>    its snapshot (mode 0600 in a 0700 directory), and in that same bucket. Never on
>    a worker.
>
> Transcripts contain tokens and keys. Anyone with an archive key, the index, or
> read access to the Longhorn backup dataset can read all of them.

## 4. Central index and search

**laharl builds it.** It has the disk (1.9 T, 42% used), the checkouts for the git
check, a ledger that already holds both hosts' entries (31 laharl, 25
jjaggars-kubevirt), and 20 days of uptime. The hub's `/home` is 84% full. In-cluster
would need the ledger and checkouts moved there as well as a new service, for no
gain now.

- **Reading all hosts:** `archive index --all-hosts` finds hosts from the `v1/`
  listing and runs the existing per-host `Sync` for each. Sync is already
  incremental (it compares `n_bytes`) and already scoped by host.
- **Threads become global.** `buildThreads` drops its host filter, and its per-host
  queries (about ten in `thread.go`) key on each session's own host. This is what
  makes a thread span hosts, as `docs/archive.md` already promises. Existing thread
  ids stay stable because the hash still uses the first session's host.
- **Git check:** `repoDir` tries the ledger's path and the cwd before `~/code/<repo>`,
  and those paths belong to another host. Only use them when the session is from this
  host. Also `git fetch origin` for each repo first, or work landed from the hub looks
  "not landed" against a stale local main.

**Queries: publish a snapshot; no new service.** After indexing, laharl writes a
consistent copy (`VACUUM INTO`) to `index/<time>.db.zst`, keeping the newest two.
The hub's timer downloads the newest and renames it into place. `comebye recall`
keeps opening a local SQLite file on both hosts and needs no change. Today's 149 MB
index compresses to 40 MB (zstd -3).

| | Snapshot (recommended) | HTTP API on laharl |
|---|---|---|
| Works with laharl off | Yes, up to an hour stale or older | No |
| New attack surface | None | A search API over plaintext secrets |
| Code | About one command | Routes, auth scoping, CLI client |
| Freshness | Hourly | Hourly (the index only updates hourly anyway) |
| Plaintext index on the hub | Yes, as today | No |

When the app gets a recall screen, a server endpoint can read that same local file.

**Writes still work from anywhere:** `archive summary` goes to the bucket as a note
and the next index picks it up. It needs `--host` for another host's session, and so
do `restore` and `delete`.

**Host metadata:**

- The host is already in every key (`v1/<host>/...`), in each segment's checked
  header, and in `sessions.host`, which is part of the primary key.
- `recall --host` already filters `list` and `search`. Add: an exact match for
  threads, a host column in `list` and `search` output, and the flag in the docs.
- `hosts/<host>.json` adds what the name alone cannot say: kind (workstation, hub or
  worker), provider, first seen. It feeds a small `hosts` table.
- Rollout step 4 checks that the hub's `COMEBYE_HOST_NAME` matches the ledger's
  `jjaggars-kubevirt`, because handoff links join on it.

## 5. Spot workers

**Pull at destroy, with no key on the worker.**

Minting per-worker keys, as the LLM key does, needs a Garage admin credential on
laharl and a `tag:worker` grant to the plaintext bucket. Since write means delete
for the whole bucket, any worker could then wipe every host's history. Pulling needs
none of that, and everything is sent over ssh that `destroy` already uses.

In `destroy`, after the agents stop and **before** sign-out and tailnet logout:

1. `ssh worker tar` of `~/.claude/projects` and `~/.pi/agent/sessions` into a mode-0700
   `mktemp` directory on laharl.
2. `comebye archive run --final --host NAME --claude-dir T/claude --pi-dir T/pi`.
   `--final` is new; it ships the last unterminated line.
3. `comebye archive verify --host NAME` against that directory, then delete it.
4. Continue the destroy.

- **Failure aborts before anything is deleted.** `--no-archive` overrides it.
- **A stopped VM is refused**, with a message to run `worker start` first, because
  the boot disk holds the sessions. `--no-archive` overrides this too.
- **`worker archive NAME`** runs steps 1 to 3 at any time. It is safe to repeat,
  because a run resumes from what is stored.
- **What it does not cover:** a VM deleted outside the tool, such as from a cloud
  console, loses its sessions. Workers also do not archive codex sessions, because the
  archive only knows Claude and pi.

## Durability

**What exists today**, read from the cluster:

- Job `critical-backup`: daily, retains 7, target
  `nfs://truenas.localdomain:/mnt/Mirrored/longhorn-backup`, available.
- All six langfuse Garage volumes are in it, last backed up today. The sccache
  volumes are not.
- Membership is a label on each Volume object. The `default` group label on these
  volumes does nothing, because no job uses that group.

**For the new volumes**, put it in git instead of a hand-applied label: a StorageClass
`longhorn-archive` (`diskSelector: sata`, 3 replicas,
`recurringJobSelector: [{"name":"critical-backup","isGroup":true}]`). Then check
`lastBackupAt` the next morning. Labelling by hand is the fallback, as documented in
`openclaw-shared-lab/AGENTS.md`.

**Off-site: the Storj copy.** The user's Storj job on TrueNAS copies
`/mnt/Mirrored/longhorn-backup` off-site, encrypted. So the plaintext rule holds, and the
new volumes are covered as soon as their backups land in that dataset.

- **It is a mirror, not history.** A delete on TrueNAS reaches Storj at the next sync, and
  Storj keeps no older copies. That is good for sessions deleted on request and bad for
  accidental deletes. **The 7 nightly Longhorn backups are the only history anywhere.**
- **It is configured on TrueNAS, not in git.** Nothing in the homelab repo describes it,
  and nothing in the cluster would notice if it stopped. Its schedule is unknown.
- **This design relies on the user's description of it.** I did not inspect the job, and
  the restore drill below does not exercise a restore from Storj.

**What replicationFactor 1 means here:**

| Event | Result |
|---|---|
| A disk or node fails | Fine: Longhorn has 2 more replicas and the pod reattaches. Writes wait, and hosts catch up on the next timer run. |
| Garage pod or volume is down | No reads or writes. A worker `destroy` aborts (nothing lost). Session files wait, though Claude deletes its own after 30 days. |
| A bad delete, a buggy client, a leaked key | **Replicas do not help:** all 3 copy the damage at once. Only the nightly backups do, so up to 24 h is lost, and it must be noticed within 7 days. Storj does not help: it mirrors the same backups. |
| Backups deleted on TrueNAS | The next Storj sync deletes them off-site too. Until that sync, Storj can restore them. Its schedule is unknown. |
| Cluster or Longhorn lost | Restore from TrueNAS. |
| TrueNAS lost as well | Restore from the Storj copy: the backups as of its last sync. It needs the job's encryption key, which is held outside this design. |

**Restores need a drill.** Longhorn backs up the meta and data volumes separately: about
2 minutes apart on the langfuse volumes. A restore can have metadata pointing at blocks
that were not captured, which only affects segments written in that window. Three
mitigations: set `metadata_auto_snapshot_interval` on Garage, schedule the timer away
from the backup time, and run `verify --all-hosts --no-source` after any restore. Rollout
step 7 makes the drill a gate.

## 6. Rollout

Each step can be undone on its own, except the last. Local archives stay until step 9.

| Step | What | Who | Undo |
|---|---|---|---|
| 1 | Build the comebye changes behind the new settings; test against a local Garage in CI or a script | Agent | Unset the settings: behaviour is unchanged |
| 2 | Homelab PR: `clusters/homelab/comebye-archive/`, StorageClass, Tailscale Ingress, SOPS keys. Tailscale PR: `tag:archive` and the worker-denied test. Then smoke-test signed requests through the Ingress | Agent drafts; **user merges and supplies keys** | Revert PRs |
| 3 | Check the first Longhorn backup exists | Agent | n/a |
| 4 | laharl: `aws s3 sync` the archive, compare ETags with local MD5s, then `archive verify` against the bucket. Same on the hub | Agent, with **user placing the key files** | Delete the objects |
| 5 | Switch both hosts' `archive.env` and server settings to S3 (the server needs a restart) | Agent, **user's OK to restart** | Restore the env file; the local archive resumes from the session files |
| 6 | laharl indexes all hosts and publishes; hub pulls. Check `recall --host` for both, and a thread spanning hosts | Agent | Rebuild the old per-host index |
| 7 | **Restore drill:** restore last night's backups to a scratch Garage, `verify --all-hosts --no-source` | Agent, read-only on the real volumes | Delete the scratch copy |
| 8 | `worker destroy` with the pull, on one disposable worker | Agent | `--no-archive` |
| 9 | After a week of clean runs, retire the local archive directories (hub first, for the space) | **User's OK**, because after this the bucket and its backups are the only copies of anything Claude has already cleaned up | Not reversible, except by syncing back from the bucket beforehand |

Retiring the hub's 206 MB will not fix its 84% full `/home`, and the index on the hub
grows. Expect roughly no net change there.

## Decisions

All decided by the user on 2026-11-06.

| # | Question | Decision |
|---|---|---|
| 1 | Which Garage | A new one, `comebye-archive` |
| 2 | How it is reached | Tailscale only, no LAN name |
| 3 | Does the backup dataset leave TrueNAS unencrypted | No. `/mnt/Mirrored/longhorn-backup` goes off-site encrypted, by the Storj job on TrueNAS |
| 4 | `destroy` when it cannot save a worker's sessions | It refuses; `--no-archive` overrides |
| 5 | A read-only copy of the all-hosts index on the hub | Yes |

## Build plan

### First slice: both hosts write to the bucket

**The goal is a real off-host copy.** laharl and the hub write to Garage, the volumes back
up to TrueNAS, and Storj carries that off-site. Each host keeps its own index for now, and
local archives stay. Workers do not change.

| Order | Change | Where | Who lands it | Needs |
|---|---|---|---|---|
| 1 | Add `tag:archive` to `tagOwners`, the same as `tag:llm`. Add tests that `tag:worker` and `tag:llm` are denied `tag:archive:443` | PR on `jhjaggars/tailscale` | **User merges** | Nothing. It must merge first, or the Ingress proxy cannot authenticate |
| 2 | `clusters/homelab/comebye-archive/`, modelled on `sccache/`: namespace, single-node Garage with the chart and image pinned (v2.5.0 is the version tested), bucket, SOPS secrets, Tailscale Ingress, no Traefik. StorageClass `longhorn-archive`. A row in `clusters/homelab/CLAUDE.md` | PR on `jhjaggars/homelab` | **User merges** | 1 |
| 3 | `fix/archive-network-safety` | comebye branch | Coordinator, after the user approves the build | Nothing. It is useful even without Garage |
| 4 | `feat/archive-s3` | comebye branch | Coordinator, after the user approves the build | 3 |

Comebye lands branches on main rather than through PRs, as its recent history does. 1 and 2
can run alongside 3 and 4. The cutover needs all four.

**What I would build first in comebye: branch 3**, because it fixes real bugs without touching
the homelab, and everything after it builds on the same error handling.

1. Backends return `ErrNotExist`. `ReadMode` and `EnsureMode` use it, so a network error is no
   longer read as "no marker", and a lost first-write race is no longer a failure.
2. Tombstones are listed once and a failure stops the run, so a blip cannot re-archive a session
   deleted on request. Tests use a backend wrapper that fails on demand.
3. A host-wide lock inside `Archiver.run`, with a test that runs a timer-style run and a flush
   together and finds no overlapping segments.

**Then branch 4:**

1. `S3Backend`, with one contract test suite that also runs against `FSBackend`. It runs against an
   `httptest` fake by default, and a real local Garage when a test setting names one. The contract
   includes the `If-None-Match` result from this design, so a Garage upgrade that changes it shows up.
2. `COMEBYE_ARCHIVE_S3` and its credentials file, the plaintext gate, https only, and the server's
   flush.
3. `hosts/<host>.json` and `--adopt-host`, and `status` sizes.
4. `docs/archive.md`: the new rule and configuration.

**Secrets in PR 2:** the agent generates them directly into SOPS-encrypted files and never prints them.
This covers the RPC secret, the admin token and the two S3 keys. Nobody runs `garage key create`: the
chart's configure Job imports each key from its Secret, as homelab #313 does for langfuse.

**After all four land** (rollout steps 2 to 5):

1. Smoke-test signed requests through the Ingress from laharl and the hub. If this fails, stop: see the
   risk in section 2.
2. **User:** put each host's key file in place, mode 0600.
3. Confirm the first Longhorn backup, and that it appears in the Storj copy.
4. Upgrade both hosts' comebye. Run `aws s3 sync`, compare ETags with local MD5s, and run `archive
   verify`. The hub's steps run through an agent on the hub, because ssh to it from this seat is denied.
5. **User:** approve the server restarts. Then switch both `archive.env` files to S3.

**The slice is done when** both hosts' timers have run clean against S3 for 24 hours, `archive verify` passes
on both, a flush from a removed worker pane lands in the bucket, and a nightly backup covers the new objects.

**Left for the user in this slice:** merging PRs 1 and 2, placing the two key files, and approving the restarts.

### Later slices

| Slice | Contents | Rollout steps |
|---|---|---|
| 2: one index | `--all-hosts`, global threads (the risky part), host-scoped git candidates and fetch, `--host` fixes, publish and pull, `verify --all-hosts --no-source`, the restore drill | 6, 7 |
| 3: workers | `run --final`, `--host` on restore, delete and summary, the pull in `destroy`, `worker archive`, refusal and `--no-archive`, `docs/workers.md` | 8 |
| 4: retire local archives | After a week of clean runs, with the user's OK | 9 |
