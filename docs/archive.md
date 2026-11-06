# Transcript archive and recall

`comebye archive` keeps every agent session (Claude Code and pi) forever, as
byte-exact, append-only, encrypted copies of the agents' own files.
`comebye recall` searches an index built from that archive, to answer
"we worked on something like this once; what was the outcome?".

Nothing here changes how agents run. It only reads their session files. It
is off until you configure it; Comebye installs no timer or service for it.

## Why a copy at all

Claude Code deletes its own transcripts after 30 days (`cleanupPeriodDays`).
Claude's main file is only part of a session: subagent files, `tool-results/`
text and workflow files hold most of the bytes, so the archive takes
everything under a session. Per-project `memory/` files are skipped. Pi
sessions are plain `.jsonl` files. Removing a worker in Comebye does not
delete its transcript, but the app can no longer find the file once the pane
is gone, so the server flushes a session to the archive just before removal.

## Configuration

| Flag / environment | Meaning |
|---|---|
| `--encryption`, `COMEBYE_ARCHIVE_ENCRYPTION` | **Required, no default:** `age` (every segment is age-encrypted; recipients required) or `none` (plaintext zstd; see [Where the archive may live](#where-the-archive-may-live)). Recorded in the archive on first use; a different mode later is refused. |
| `--dir`, `COMEBYE_ARCHIVE_DIR` | Archive directory (the filesystem backend). |
| `--s3`, `COMEBYE_ARCHIVE_S3` | Archive bucket instead of a directory: `https://HOST/BUCKET`, path-style. Plain `http` is refused except on loopback. Setting both this and a directory is an error. |
| `--s3-credentials`, `COMEBYE_ARCHIVE_S3_CREDENTIALS` | A mode-0600 file of `key_id=` and `secret_key=` lines. A file others can read is refused. |
| `--s3-region`, `COMEBYE_ARCHIVE_S3_REGION` | Default `garage`. |
| `COMEBYE_ARCHIVE_S3_PLAINTEXT` | Must be `homelab` to use `none` with a bucket: it states that the bucket meets [the rule](#where-the-archive-may-live). |
| `--adopt-host` | Write under a host name that another machine claimed in the bucket. |
| `--recipient`, `COMEBYE_ARCHIVE_RECIPIENT` | With `age`: who can read: `age1...` keys and/or `ssh-ed25519`/`ssh-rsa` public keys, comma or newline separated, or a file of them. |
| `--identity`, `COMEBYE_ARCHIVE_IDENTITY` | With `age`: an age identity file (or an unencrypted OpenSSH key). Needed only to read: `verify`, `index`, `restore`, `rewrap`. |
| `--claude-dir`, `--pi-dir` | Session directories (default `~/.claude/projects`, `~/.pi/agent/sessions`). |
| `--index`, `COMEBYE_RECALL_INDEX` | The index file (default `~/.local/state/comebye/recall/index.db`). It holds plaintext, so it is created mode 0600. |
| `--host`, `COMEBYE_HOST_NAME` | The host's name inside archive keys. `restore`, `delete`, `summary` and `verify` reach another host's sessions with it: `comebye recall list` shows each session's host. |
| server `-archive-dir` or `-archive-s3` and `-archive-s3-credentials`, `-archive-encryption`, `-archive-recipient` | Turns on the flush when a worker is removed. Off unless one of those locations is set. With a bucket, the server starts even if the store is unreachable; the checks wait for the first flush, and a failed flush is retried by the next `archive run` because the session files are still on disk. |

Indexing needs the `sqlite3` command-line tool with FTS5 on the host.

## Commands

```
comebye archive keygen --out DIR     # age identity (age-keygen format) + recipient.pub
comebye archive run [--final]        # copy new bytes; safe to rerun and after a crash
                                     # --final: the sessions have ended, take each last line too
comebye archive verify               # restore every stream and compare sha256 with the source
comebye archive verify --no-source [--all-hosts]   # check the store by itself: decode every segment, report gaps and overlaps
comebye archive status               # sessions, segments, sizes, and how old the index is
comebye archive index [--git=false] [--gh] [--ledger FILE|auto|none]
                                     # this host and the workers it uploaded
comebye archive index --all-hosts [--publish]      # every host in the store; --publish copies the result to the store
comebye archive index --pull         # do not index: download the newest copy and move it into place
comebye archive restore SESSION --to DIR [--host H]
comebye archive delete SESSION [--yes] [--host H]    # dry run without --yes
comebye archive summary SESSION TEXT|- [--host H]    # a short summary, kept in the archive
comebye archive rewrap --to-dir DIR --to-encryption age --to-recipient KEYS   # plaintext or age, to age
comebye archive rewrap --to-dir DIR --to-encryption none                      # age to plaintext (needs the identity)

comebye recall ask "a vague description of the work"   # the short answer
comebye recall threads | todo | list | search WORDS | commit SHA
comebye recall show THREAD|SESSION [--text N]
```

Every `recall` command takes `--json` and filters `--since`, `--until`,
`--host`, `--project`, `--branch`, `--kind`, `--ledger`, `--limit`. `--host` takes
a host's exact name, for sessions and for threads (a thread matches if any of its
sessions is on that host); `--host w1` also finds every VM that was called `w1`
(see [Spot workers](#spot-workers)). `list`, `search` and `show` print each
session's host.

## How it stores

One object per run per file, named by byte range:
`v1/<host>/<kind>/<path under the agent's root>/<offset>-<length>.seg`. A file
at any moment is the concatenation of its first N segments. Objects are
created once, atomically, and never rewritten, so a rerun or a crash cannot
damage what is stored. A run reads the store to see where to continue. A file
that shrinks is reported and left alone. A last line still being written
waits two minutes of quiet (the flush on removal ships it at once).

Everything on a machine that writes one host's segments (`run`, the server's
flush, `delete`) takes a lock file under `~/.local/state/comebye/locks`.
Without it, two writers can cut different lengths from one offset, and the
overlap makes that stream refuse further segments.

### In a bucket

The object keys are the same as the file paths in a directory, so `aws s3 sync`
or `rclone copy` moves an archive into a bucket, and `archive verify` then checks
it against the session files. Tested against Garage v2.5.0.

Two things about the `aws` CLI through a Tailscale Ingress:

- **`aws s3 cp` downloads fail** with `signed header content-length is not
  present`. Check an upload with `archive verify` and ETags instead. Comebye's own
  reads, `aws s3api get-object` and uploads are not affected.
- **Set `multipart_threshold = 64MB` under `s3 =` in the `aws` config before
  migrating.** Above the default 8 MB threshold, `aws s3 sync` uploads in parts, and
  the object's ETag (`...-2`) cannot be compared with a local MD5. Below the
  threshold, the ETag is the file's MD5.

- **Create-only is done by HEAD then PUT.** Garage v2.5.0 ignores `If-None-Match`
  on PUT: a second PUT replaces the object. That window is safe because each host
  writes only under its own `v1/<host>/` prefix, its writers take the lock above,
  and the first run writes `hosts/<host>.json`, which refuses a second machine
  using the same name. Do not point two machines at one host name with
  `--adopt-host`.
- **A key's write permission covers the whole bucket**, including overwrite and
  delete. Garage has no prefix scoping, and a write-only key cannot list, which
  `run` needs in order to resume. Give a key only to a host you would trust with
  every other host's history.
- **Object names are not encrypted in any mode.** They hold host names, project
  paths and session ids.
- **To re-run the contract test against a real store,** for example before a Garage
  upgrade: `COMEBYE_TEST_S3_URL=URL COMEBYE_TEST_S3_CREDENTIALS=FILE go test
  ./internal/archive/ -run RealStore`. It writes only under `contract-test/` and
  cleans up.

A segment is `CBA2`, a codec byte (2 = zstd), then an age file. In the
`none` mode the magic is `CBP2` and the rest is the same header and zstd
bytes without the age layer. Inside the
age file is a header line `cba2|host|kind|stream|offset|length` followed by
the zstd bytes; a segment at the wrong key is refused. To read one without
Comebye: drop the first 5 bytes, `age -d -i identity.key`, drop the first
line, `zstd -d`.

## Where the archive may live

Two modes, chosen explicitly and never mixed (an archive records its mode;
a reader or writer in the other mode is refused, and `archive rewrap`
converts either way):

- **`age`:** safe wherever a copy ends up. Needs the key handling below.
- **`none`:** plaintext zstd, with the same append-only segments and sha256
  `verify`. Transcripts contain tokens and keys, so this mode is only for an
  archive whose every copy sits somewhere that is encrypted by something
  else. The archive directory and the index directory are created mode 0700
  (the index file 0600), and `archive verify` warns if the archive or index
  is group- or world-accessible.

**Intended setup with plaintext** (the user's offsite backups are encrypted
by the NAS's cloud sync, so the archive itself need not be):

1. The archive lives on the workstation (later also on the hub), in a
   private directory, with `--encryption none`.
2. A second copy goes to a dataset on the NAS that is included in an
   **encrypted** cloud sync task. The offsite copy is therefore encrypted
   by the NAS. What the user sets up there: a dataset for the archive, a
   share on it limited to the hosts that need it with owner-only access,
   and a cloud sync task covering that dataset with encryption on.
3. Copy only new objects, never with delete: segments are written once and
   never change, so `rsync -a --ignore-existing SRC/ DEST/` (or `rclone copy
   --immutable`) is safe and cheap. Do not use `--delete`: a lost or empty
   source would erase the copy.
4. **Delete on request** must be done in each place: run `archive delete
   SESSION --yes` against the archive directory and again against the copy
   (`--dir` pointing at the mounted dataset). The tombstone is copied by the
   same rsync, but the already-copied segments stay until deleted there.
5. The copy on the NAS's local network is plaintext, so the share and
   dataset permissions are what protect it.

**The rule: nothing unencrypted goes anywhere that the encrypted cloud sync
does not cover.** No plaintext copy in a sync folder, a cloud drive or another
host's disk. If a copy has to go somewhere else, convert it with `archive rewrap
--to-encryption age` first, or switch the archive to `age`.

**The one exception is a homelab bucket.** A plaintext archive may live in an
object-store bucket only if all of these hold:

1. The bucket is on a Garage inside the homelab, used for nothing else.
2. Its only network path is a Tailscale Ingress: no Traefik host, no public name,
   no website access, and workers (`tag:worker`) are denied.
3. Its only credentials are one per archiving host, kept in mode-0600 files
   outside any repository. Workers hold none.
4. Its volumes are in Longhorn's `critical-backup` group, and any copy that leaves
   TrueNAS is encrypted by the TrueNAS job that sends it. Nothing unencrypted goes
   to a cloud drive or to another host's disk.
5. The plaintext index lives only on hosts that index or download it (mode 0600 in
   a 0700 directory), and in that same bucket. Never on a worker.

Anyone with an archive key, the index, or read access to the Longhorn backup
dataset can read every transcript, tokens and keys included. Setting
`COMEBYE_ARCHIVE_S3_PLAINTEXT=homelab` is the statement that these hold.

## Recall

`archive index` reads the archive (it needs the identity), joins each session
to its handoff from `comebye ledger --json`, and groups sessions into
**threads**: one piece of work, linked by handoff, plan, backlog ref, or repo
plus a non-default branch. A thread's outcome comes from structured sources
and every line names its source: the ledger (status, report, discard marks,
commits unmerged at removal), git (is each commit on main, by ancestry or by
subject, because landing rebases), `gh` (PR state, only with `--gh`, cached),
and the tool calls (docs written). A deploy branch and the user's decision
lines are labelled heuristics; "how it ended" with no ledger report is the
agent's own last reply, labelled as such. Verdicts: landed, abandoned, open,
not landed, reported, unknown. "Abandoned" comes only from the ledger or a
closed PR.

`recall ask` reduces a question to its topic words and matches any of them
against each thread's digest (title, brief, outcome, report, keywords,
summaries) and against the raw messages, then fuses the two rankings. An
agent that wants more reads the top threads with `recall show`.
`recall todo` lists threads with no summary, for an agent to summarise with
`archive summary`.

A thread can span hosts: its sessions are grouped across the whole index, whichever
hosts they ran on. A thread's id comes from its first session's host, kind and id,
so a thread keeps its id when later sessions join it. If an earlier session joins, as
when two threads turn out to share a branch, the merged thread takes the earlier
session's id, and the later one's id stops resolving.

The git check asks a checkout on the machine doing the indexing. For that host's own
sessions it tries the ledger's checkout, then the session's directory, then
`~/code/<repo>`. For any other host's session it only tries `~/code/<repo>`, because
those paths name directories over there. It runs `git fetch origin` once per
repository first, so that work landed from another host does not read as not landed.
A failed fetch is reported on stderr and the check goes on against the last-known
origin. Fetching is what `--git` costs on a timer: it writes to `refs/remotes/origin`
in those checkouts and needs credentials that work without a terminal.

### One index for every host

With a bucket, one machine builds the index of every host, and the others download
it. `comebye recall` and the app read a local file on both, so neither changes.

| Machine | Its timer runs | What it needs |
|---|---|---|
| The indexing machine (laharl) | `comebye archive run && comebye archive index --git --all-hosts --publish` | The checkouts for the git check, `gh` if you add `--gh`, and the ledger holding both hosts' entries. |
| Every other machine (the hub) | `comebye archive run && comebye archive index --pull` | Room for two copies of the index while it swaps them. |

**Run `--publish` on one machine only.** Snapshots are named by time and the newest
wins, and two publishers would each prune the other's.

**`--all-hosts`** finds hosts in the `v1/` listing and runs the same incremental
per-host sync on each, tombstones included. It also covers any host the index still
has rows for, so that the rows of a host whose last session was deleted go too. One
host that cannot be read keeps its rows and is named on stderr, and the others carry
on. `--publish` then refuses, so that a copy missing a host never replaces a good one.

**The snapshot.** `--publish` makes a consistent copy with `VACUUM INTO`, checks it,
and writes it to `index/<time>.db.zst`, keeping the newest two. Other objects under
`index/` are left alone. In the `none` mode it is an ordinary zstd file. In the `age`
mode it is sealed like everything else, and pulling needs the identity.

**The pull.** `--pull` downloads the newest snapshot if it is newer than the one the
local file came from, and checks it: zstd's checksum, SQLite's `quick_check`, the
format version, a build time, and at least one session. Only then does it move it
over the index file, mode 0600. It refuses a directory that others can read. Any
failure leaves the current file as it was and exits non-zero, so the timer's unit
shows failed. The hub's first pull replaces the index it built itself, including that
index's PR cache.

**Swapping under the server.** The server opens the file for each query, so a move
into place needs no restart. A query in flight finishes on the old file and the next
one reads the new. Snapshots are not in WAL mode, because SQLite applies a `-wal` file
it finds beside a database whatever that database's own mode says. So `--pull` first
closes any log the old file has, and if it cannot, within a few seconds, it does not
replace the file.

**Do not run a plain `archive index` on a pulling machine.** It would work, adding to
the copy, but the next pull discards the result. It also would not re-read other
hosts, so it gains nothing.

**How fresh it is.** `archive status` prints the index's age and where it came from:

```
index: built 40m0s ago, pulled from snapshot 20261106T140000Z.db.zst; 1560 sessions on jjaggars-kubevirt, laharl
newest snapshot in the store: index/20261106T140000Z.db.zst
```

The age is measured from when the indexing machine built it, not from when it was
pulled. So if laharl stops indexing, or the hub stops pulling, the hub's `status` and
its app both show the true age, and the app warns after two missed builds
(`state: stale`). A session reaches the other host's search after up to three hourly
steps in the worst case: the host's own `archive run`, the indexer's run, then the
pull. Comparing the two `status` lines shows which side has stopped: an old index with
a newer snapshot in the store means the pull is failing, and an old snapshot means
publishing is.

**Delete on request.** `archive delete SESSION --host H --yes` works from any machine
with a key. It removes the segments and the local index's rows, and the indexing
machine drops the session at its next run. Until then a pull can bring it back into
the local copy. The session stays in the older snapshots until they are replaced,
about two hours, and in Longhorn backups until those are pruned. If it matters, run
the indexer and the pull by hand after deleting.

**To prove a restored backup,** point a binary at the restored store and run `archive
verify --all-hosts --no-source`. It decodes every segment (magic, header against key,
zstd, length) and lists gaps, overlaps and damage as `fault:` lines, with session ids
and offsets only, and exits non-zero if there are any. It only reads a store that
already has its mode marker.

### What the index keeps

- **Secret-looking text is masked as it is indexed.** Known token shapes (API and
  tailnet keys, GitHub and cloud tokens, PEM private keys, JWTs, `Bearer` values,
  `password=` and the like, passwords in URLs) become `••••••`, before clipping, in
  messages, tool calls, tool output, titles, handoff briefs, reports and summaries.
  A session with any is flagged, and the app warns about it. The archive stays
  byte-exact. This is a safety net and patterns miss things, so the index still
  counts as holding secrets and its access rules stay as they are. A masked value
  cannot be searched for: use `archive restore` for that. New patterns take effect
  when a session is next indexed.
- **Long text is clipped.** A message keeps its first 4,000 bytes and a tool's output
  its first and last 1,500, because errors tend to come last. Each message records
  its stream and byte offset in the archive, and its original length.
- **The format has a version.** `archive index` replaces an index in an older
  format with a complete rebuild from the archive, into `index.db.rebuild`, and moves
  it into place when it is done. Until then `recall` needs the new file and the app
  says the index is out of date. A binary older than the format cannot read the new
  file: to roll back, delete the index and let the old binary rebuild it.

### Searching from the app

The app's search icon opens **Transcripts**: search what agents said, ran and got
back, and read a session with its tool calls collapsed. Before anything is typed it
lists the newest sessions, with their host, project and time, and the host, time, agent
and project filters narrow that list. The server reads the file
named by `-recall-index` (`COMEBYE_RECALL_INDEX`, by default the path above) with
`sqlite3 -readonly -safe`, one process per query, so a file moved into place needs no
restart. It serves whatever that host's index holds and is [for the user
alone](security.md#searching-transcripts). The session view shows the indexed text
with a notice where it is clipped.

The index is as fresh as its last build: hourly on the timer, plus a run that
`comebye-worker destroy` starts as it finishes, so that a destroyed worker's sessions
are searchable within minutes. The app shows the age of the index and warns after
two missed builds.

## Spot workers

A worker holds no archive key. `comebye-worker archive` and `destroy` pull its
sessions over ssh and upload them from the machine running the tool:
[Sessions and the archive](workers.md#sessions-and-the-archive).

- **Its host is `NAME.INSTANCE_ID`**, because worker names get reused.
  `hosts/<host>.json` records the uploading machine, as for any host, plus `kind:
  worker`, the worker's name, provider, instance id and creation time.
- **`archive index` also covers the workers this machine uploaded.** They have no
  timer of their own, so the existing one picks them up. A blip reading `hosts/`
  fails the run rather than dropping them. A worker's thread stays separate from
  its coordinator's unless something ties them, such as working on one branch.
- **Handoff links survive the worker.** Removing a peer does not touch the ledger.
  The ledger knows the worker by its bare name, so the index joins on that name, and
  takes a handoff only if it was created during this VM's life and no more than 5
  minutes after the session started. A reused name and worktree path therefore do
  not link to another VM's handoff.
- **To upload by hand** when the tool cannot: copy the session directories off the
  VM, then `comebye archive run --final --host NAME.INSTANCE_ID --claude-dir DIR
  --pi-dir DIR`. Without the tool's claim, a normal `archive index` does not pick
  that host up, so `recall` will not find it.

## Keys (the `age` mode)

- The archive has its own age identity. It does not depend on sops or GPG.
- Archiving hosts need only the **public** recipients. The identity is for
  reading, so a stolen archiving host cannot read the archive.
- List **two** recipients from the start: a working identity and a recovery
  identity kept offline. Losing the only identity loses the archive.
- **Add a recipient later** (a new laptop, a shared identity) by adding its
  public key to the list. Later segments include it; older ones stay readable
  by the original identity, which must be kept while they exist.
- **Make old segments readable by a new identity, or retire one:**
  `archive rewrap --to-dir NEW --to-recipient KEYS`, then `archive verify`
  against `NEW` with the new identity, then switch `COMEBYE_ARCHIVE_DIR`. It
  copies (about 30 s per 1.8 GB) and never rewrites in place. It is
  resumable. Rotation protects future data only: a copy of the old store
  stays readable with the old identity.
- **Never use the cluster's sops decryption key as the archive identity.**
  Whatever can reach that key could read every transcript. If sops later
  moves to age, add the user's personal age identity as a recipient instead.
- SSH public keys work as extra recipients. Passphrase-protected SSH
  identities, and plugin identities (for example a YubiKey), are not
  supported yet; a plugin key needs a touch per segment, so it only suits a
  recovery recipient.
- Keep identity files away from sops' default age key location.

## Limits

- Transcripts contain secrets. The archive is byte-exact on purpose;
  protect it with the keys above (age mode) or with where its copies live
  (plaintext mode). The index is plaintext in both modes, and masking does not
  change that: treat it as holding secrets.
- Without `--all-hosts` and a pull, a host indexes itself and the workers it
  uploaded, and cannot search the others.
- The git check needs the repository at `~/code/<repo>` on the indexing machine for
  other hosts' sessions; without it their commits read as unchecked. `gh` results
  are cached until the next `--gh`.
- Decision lines are a keyword heuristic and miss some.
- No scheduler and no semantic (embedding) search.
