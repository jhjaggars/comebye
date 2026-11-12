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
| `--opencode-dir` (`COMEBYE_OPENCODE_MIRROR_DIR`) | Where OpenCode sessions are mirrored (default `~/.local/state/comebye/opencode-sessions`). OpenCode keeps sessions in SQLite; the server writes each live session there as a Pi-format file from `opencode export <id>` (whole messages only, append-only), and the archive, recall, usage and the chat view read it as kind `opencode`. |
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
comebye archive index --all-hosts    # every host in the store
comebye archive index --all-hosts --if-changed [--git-every 1h]   # the same, for a frequent timer: skips if nothing changed
comebye archive restore SESSION --to DIR [--host H]
comebye archive delete SESSION [--yes] [--host H]    # dry run without --yes
comebye archive summary SESSION TEXT|- [--host H]    # a short summary, kept in the archive
comebye archive rewrap --to-dir DIR --to-encryption age --to-recipient KEYS   # plaintext or age, to age
comebye archive rewrap --to-dir DIR --to-encryption none                      # age to plaintext (needs the identity)

comebye recall ask "a vague description of the work"   # the short answer
comebye recall threads | todo | list | search WORDS | commit SHA
comebye recall produced REF       # which session made a commit, Page, PR, issue, release or image
comebye recall show THREAD|SESSION [--text N]   # a session also lists what it produced
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

- **`aws s3 cp` downloads and `aws s3 rm` deletes fail** with `signed header
  content-length is not present`: the CLI signs that header, and it does not survive
  the Ingress. Check an upload with `archive verify` and ETags instead. Comebye's own
  reads, `aws s3api get-object`, listing and uploads are not affected.
  - **To delete a session**, use `archive delete SESSION --host H --yes`. It goes
    through Comebye's own client, which does not sign that header, and it writes the
    tombstone that keeps the session from coming back.
  - **To delete any other object**, use a client that does not sign
    `content-length`, or reach Garage without the Ingress, for example with `kubectl
    port-forward` to its S3 port.
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
5. The plaintext index lives only on the hosts that build it (mode 0600 in a 0700
   directory). It never goes in the bucket or any other store, and never on a worker.

Anyone with an archive key, the index, or read access to the Longhorn backup
dataset can read every transcript, tokens and keys included. Setting
`COMEBYE_ARCHIVE_S3_PLAINTEXT=homelab` is the statement that these hold.

## Recall

`archive index` reads the archive (it needs the identity), joins each session
to its handoff from `comebye ledger --json`, and groups sessions into
**threads**: one piece of work, linked by handoff, plan, backlog ref, or repo
plus a non-default branch. A thread's outcome comes from structured sources
and every line names its source: the ledger (status, report, discard marks,
commits unmerged at removal), git (is each commit on main: by ancestry, then by a
commit on main with the same patch-id, because landing rebases), `gh` (PR state, and
which PR carried each commit, only with `--gh`, cached),
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

### What a session produced

The index keeps a `produced` table: one row per thing a session made, tied to the
message that shows it (`seq`, and the subagent, tool call and time). The kinds:
`commit`, `page` (from `comebye publish`), `pr` and `issue` (the URL `gh pr create` /
`gh issue create` prints), `comment` (`gh pr|issue comment`), `release` (`gh release
create|edit`) and `image` (a pushed image's digest: `build.sh --push`, `docker push`,
`podman push`). It is read from the transcript only, so nothing is written into any
repository, and a Page, PR or issue is only counted when the command that made it
succeeded and printed what only that command prints. A URL that is mentioned in prose,
a `gh pr view`, and a failed call make nothing. A release whose output went to
`/dev/null` is read from the command, and says so. The old `commits` table is now a
view of the `commit` rows. This was schema version 3, so the first `archive index`
after the upgrade rebuilds the index aside from the archive.

### Where a commit ended up

A session records a commit's SHA when it makes it, and landing rewrites commits. Each
commit is asked, in order, and the answer names its source:

1. **Ancestry:** is it an ancestor of main?
2. **Patch-id:** is a commit on main the same change? The patch-id is computed here
   from git when the index is built (`git patch-id --stable`, with no context lines, so
   a rebase over other work still matches), over main's newest 3,000 commits and no
   further back than the oldest commit asked about. A rebase or a reworded message
   matches; a squash does not. It is not stored in the repository; the index keeps it
   on the `produced` row.
3. **GitHub:** with `--gh`, which PR contains the commit (`gh api
   repos/OWNER/REPO/commits/SHA/pulls`, against the checkout's `origin` and `upstream`
   remotes, at most 300 lookups a pass)? A merged PR into main settles it, and its merge
   commit is shown as the commit on main. An open PR, or one closed without merging, is
   "not in main". This is how a squash merge is found. A PR is named by its base
   repository: on a fork the lookup also returns the parent's PRs. It sends commit SHAs
   and repository names to GitHub, as well as the branch names it sent before.

   Each answer is kept in the index (`gh_commit_prs`), so that a pass spends its 300
   lookups on commits it has not asked about. A merged PR is kept for good. "No PR", and
   a PR that is open or closed unmerged, are believed for a day and then asked again,
   because an open PR may be squash-merged meanwhile. "GitHub never had the commit" (a
   422: it was never pushed) is believed until a remote-tracking ref of the checkout
   contains the commit. An error, such as the network or a rate limit, is not kept. The
   cache does not survive an index rebuild (a schema change).

Otherwise the answer is **unknown**: the commit may be unmerged, or squashed with no PR,
and git cannot tell which. "Not in main" is only said when GitHub names an unmerged PR.
It is the same in every repository: no trailer, note or hook is written anywhere. It
cannot place a squash with no PR, a commit rewritten and dropped before this machine
fetched it, or a rewrite that changes the diff (a conflict resolved, a fixup that edits
code). Without `--gh` it says so. A commit made outside an agent session, or by a script
whose output the agent never saw, is not in any transcript, so it has no row at all.

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
in those checkouts and needs credentials that work without a terminal. Each host checks
its own checkouts, so two hosts can disagree on a commit's verdict, where only one has
the repository or a fresher one. Session search reads the same segments and does not
differ.

A fetch takes a lock, `comebye-fetch.lock` in the repository's git directory, which the
server's merge checks take as well. Two fetches into one object store at once can leave
a remote-tracking ref pointing at an object that never arrived, and after that every
fetch fails and leaves a temporary pack behind (`.git/objects/pack/tmp_pack_*`). The
git pass waits up to five minutes for the lock, then says the fetch was skipped. A fetch
runs at most five minutes (an ordinary one takes seconds) and is then stopped with SIGTERM,
so that git removes its temporary pack, and killed only if it is still running 15 seconds
later. Fetches use `--prune`, so the ref of a branch deleted upstream goes rather than
pointing at objects nothing keeps.

### One index for every host

With a bucket, each machine builds its own index of every host from the segments, so
`comebye recall` and the app read a local file and search finds every host's sessions.
The index itself never leaves the machine: it is not in the bucket or its backups. A
machine needs only its archive key, and the ledger for handoff links.

**The timer.** Run this every 5 minutes on each machine, on a timer like
`OnCalendar=*:0/5`:

```
comebye archive run && comebye archive index --all-hosts --if-changed
```

Start it under `flock -n`, so that a tick finding the last one still going exits at
once. `flock -n -E 0` makes that exit count as success, which keeps the unit from
reading as failed.

**`--all-hosts`** finds hosts in the `v1/` listing and runs the same incremental
per-host sync on each, tombstones included. It also covers any host the index still
has rows for, so that the rows of a host whose last session was deleted go too. One
host that cannot be read keeps its rows and is named on stderr, and the others carry
on.

**`--if-changed`** first compares a hash of the `v1/`, `notes/`, `tomb/` and `hosts/`
listings and of the ledger with what the index was last built from. On a match it
records that it looked and stops: about 0.3 s, with no index work. A segment's name
holds its offset and length, so the listing is the store's whole state. It compares
state rather than waiting for an event, so a missed event loses nothing: the next tick
sees the difference.

Three things run on their own schedules, because they cost far more than the rest:

| What | When | Why |
|---|---|---|
| Indexing what changed, and rebuilding threads | Any tick with a change | About 15 s and 400 MB. |
| The git check, with `git fetch` | Hourly, with or without a change (`--git-every`) | About 30 s, on the network. A fetch can change a verdict with no new bytes, which is why it runs even when nothing else changed. Between passes a commit keeps its verdict, and a new commit is checked locally with no fetch. So a commit landed from elsewhere reads as not landed for up to an hour. |
| FTS `optimize` | Daily | It rewrites the whole full-text index: about 170 MB of disk writes on a 500 MB index, which at every active tick would be tens of GB a day. Without it, 144 session rewrites grew that index by 19 MB and left searches at the same speed. |

`--gh` also runs only with the git check. If the ledger cannot be read, new sessions are
still indexed, but threads wait until it can, because rebuilding them without it would
drop their reports and other ledger outcomes. A plain `archive index`, without
`--if-changed`, does everything every time.

**How fresh it is.** A session reaches another host's search after that host's `archive
run` and the other's next tick: about 5 to 10 minutes. Agents are active nearly all the
time, so expect most ticks to index something: in one 8-day sample, 73% of 5-minute
windows held a message, and 97% of 30-minute windows. `archive status` shows both times:

```
index: built 4m0s ago, last checked 1m0s ago; 1570 sessions on jjaggars-kubevirt, laharl
```

`built` is when the content last changed. `last checked` is the last tick, which only
`--if-changed` records. The app shows the later of the two and warns (`state: stale`)
after 30 minutes with neither, so a stopped timer or an unreachable store shows up
quickly. An index built without `--if-changed` has no check time and keeps the old
limit of a little over two hours, which suits an hourly timer.

**Delete on request.** `archive delete SESSION --host H --yes` works from any machine
with a key. It removes the segments and that machine's index rows, and writes a
tombstone that every other machine's next tick sees, so the session leaves every index
within minutes. It stays in Longhorn backups of the bucket until those are pruned.

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

The index is as fresh as its last build: see [How fresh it is](#one-index-for-every-host).
`comebye-worker destroy` also starts a run as it finishes, so that a destroyed worker's
sessions are searchable at once. The app shows the age of the index and warns when it
has stopped updating.

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
- Without `--all-hosts`, a host indexes itself and the workers it uploaded, and
  cannot search the others.
- For other hosts' sessions, the git check needs the repository at `~/code/<repo>`
  on the machine building the index; without it their commits read as unchecked. `gh` results
  are cached until the next `--gh`.
- Decision lines are a keyword heuristic and miss some.
- No scheduler and no semantic (embedding) search.
