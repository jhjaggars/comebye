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
| `--dir`, `COMEBYE_ARCHIVE_DIR` | Archive directory (the filesystem backend; the `Backend` interface in `internal/archive` is where an object store would plug in). |
| `--recipient`, `COMEBYE_ARCHIVE_RECIPIENT` | Who can read: `age1...` keys and/or `ssh-ed25519`/`ssh-rsa` public keys, comma or newline separated, or a file of them. |
| `--identity`, `COMEBYE_ARCHIVE_IDENTITY` | An age identity file (or an unencrypted OpenSSH key). Needed only to read: `verify`, `index`, `restore`, `rewrap`. |
| `--claude-dir`, `--pi-dir` | Session directories (default `~/.claude/projects`, `~/.pi/agent/sessions`). |
| `--index`, `COMEBYE_RECALL_INDEX` | The index file (default `~/.local/state/comebye/recall/index.db`). It holds plaintext, so it is created mode 0600. |
| `--host`, `COMEBYE_HOST_NAME` | This host's name inside archive keys. |
| server `-archive-dir`, `-archive-recipient` | Turns on the flush when a worker is removed. Off unless set. |

Indexing needs the `sqlite3` command-line tool with FTS5 on the host.

## Commands

```
comebye archive keygen --out DIR     # age identity (age-keygen format) + recipient.pub
comebye archive run                  # copy new bytes; safe to rerun and after a crash
comebye archive verify               # restore every stream and compare sha256 with the source
comebye archive status               # sessions, segments, raw and stored size
comebye archive index [--git=false] [--gh] [--ledger FILE|auto|none]
comebye archive restore SESSION --to DIR
comebye archive delete SESSION [--yes]       # dry run without --yes
comebye archive summary SESSION TEXT|-       # a short summary, kept in the archive
comebye archive rewrap --to-dir DIR --to-recipient KEYS

comebye recall ask "a vague description of the work"   # the short answer
comebye recall threads | todo | list | search WORDS | commit SHA
comebye recall show THREAD|SESSION [--text N]
```

Every `recall` command takes `--json` and filters `--since`, `--until`,
`--project`, `--branch`, `--kind`, `--ledger`, `--limit`.

## How it stores

One object per run per file, named by byte range:
`v1/<host>/<kind>/<path under the agent's root>/<offset>-<length>.seg`. A file
at any moment is the concatenation of its first N segments. Objects are
created once, atomically, and never rewritten, so a rerun or a crash cannot
damage what is stored. A run reads the store to see where to continue. A file
that shrinks is reported and left alone. A last line still being written
waits two minutes of quiet (the flush on removal ships it at once).

A segment is `CBA2`, a codec byte (2 = zstd), then an age file. Inside the
age file is a header line `cba2|host|kind|stream|offset|length` followed by
the zstd bytes; a segment at the wrong key is refused. To read one without
Comebye: drop the first 5 bytes, `age -d -i identity.key`, drop the first
line, `zstd -d`.

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

## Keys

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
  protect it with the keys above and with where its copies live. The index
  is plaintext.
- Hosts archive and index themselves. Threads span hosts only once each
  host's sessions are in one backend that the index reads.
- The git check needs the repository on the same host. `gh` results are
  cached until the next `--gh`.
- Decision lines are a keyword heuristic and miss some.
- No object-store backend, no scheduler, and no semantic (embedding) search.
