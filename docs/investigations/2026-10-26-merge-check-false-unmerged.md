# Investigation: "merged: NO" false negatives for jjaggars-kubevirt sessions w22:p2 and w23:p2

Date: 2026-10-26 · Read-only investigation; no Comebye code changed, no sessions touched.

## TL;DR

The user is right: **removing both sessions loses nothing.** Every commit on both
branches is on `origin` (GitHub). Comebye reported "merged: NO … N commits not in
main" because (a) the *displayed* commit list is computed against the **local**
`main`, which is 60+ commits stale on that host and never fast-forwarded, so other
authors' already-merged PRs show up as "unmerged", and (b) Comebye **never runs
`git fetch`**, so its `origin/main` remote-tracking ref also lagged behind the
real `origin/main` — for w23 it predated the squash-merge of PR #758, which is
why even the *count* (which does consider `origin/main`) said unmerged.

## Verdict per session

### w23:p2 `managed-workspace-mode` (feat/gateway-managed-workspace-mode, tip e4faeca0)

**SAFE to remove — work is fully upstream.**

- Branch has exactly **1 commit** (`e4faeca0`), pushed to
  `origin/feat/gateway-managed-workspace-mode` (tips identical).
- PR #758 is **MERGED** (gh: state `MERGED`, mergedAt 2026-10-26T16:50:40Z) as
  squash commit `0b5138c8` on `origin/main`.
- Squash-merge verified by patch-id: `git show e4faeca0 | git patch-id` and
  `git show 0b5138c8 | git patch-id` both give `77408c41…` (identical, stable
  and unstable). Post-fetch, `git cherry -v origin/main HEAD` marks it `-`
  (equivalent upstream).
- Worktree clean, no stash, no in-progress operation.
- **Commits that exist only on this host: none.**

### w22:p2 `sa-claims-investigation` (investigate/service-account-claims, tip 01c70588)

**SAFE to remove — no data loss, though genuinely not merged.**

- Branch has **7 commits** (`f5f59cae … 01c70588`), all pushed:
  `origin/investigate/service-account-claims` tip is identical to the local tip.
- PR #749 is **OPEN** (awaiting review). Not merged into main — but "merged" is
  not the data-loss question. `git cherry origin/main HEAD` post-fetch: all 7
  `+` (expected, PR open), yet `rev-list --count HEAD --not --remotes` = 0.
- Worktree clean, no stash, no in-progress operation.
- **Commits that exist only on this host: none.** Even a *discard*
  (`worktree remove --force` + `branch -D`) would lose nothing; only deleting
  the remote branch (which Comebye never does) could.

## What actually happened, git-level

State of the shared hypershell repo on jjaggars-kubevirt at inspect time:

| ref | value | how stale |
|---|---|---|
| local `main` | `5556262b` (#617) | ~60 commits behind real main; last moved long ago |
| `origin/main` (remote-tracking) | `29d4cd0b` (#759) | last fetched 2026-10-23 18:19 EDT |
| real `origin/main` | `88e3a937` (#785) | — |

- PR #758's squash commit `0b5138c8` landed on main 2026-10-26 16:46 UTC —
  *after* the last fetch — so Comebye's `origin/main` did not contain it.
- Both branches were created from local `main` (`5556262b`) on 2026-10-23 and
  then rebased onto newer upstream commits (final bases `04b5007d` / `29d4cd0b`).
- The "~18 unmerged commits" the user saw (`bf4f19df` #640 … `91c905de` #668)
  are exactly the 18 commits in `5556262b..91c905de` — other authors' PRs
  between stale local main and the branches' early base. They are the *shared
  prefix* of both branches' `git cherry -v main <tip>` lists (49 and 56 entries
  respectively, of which only 1 and 7 are the branches' own work).

## Root cause in Comebye (`internal/server/gitops.go`)

Two compounding defects:

1. **The displayed commit list uses only the local base.**
   `checkWorktree` (line 269) computes `c.Unmerged` as the *minimum* of
   `git cherry` counts over `base` and `refs/remotes/origin/<base>`
   (loop at **line 314**, min at **line 328**) — good. But
   `inspectWorktree` (**line 364**) builds the "Unmerged commits" list with
   `git cherry -v c.BaseUsed tip` at **line 375**, where `c.BaseUsed` is the
   *local* branch name only. So whenever local `main` is behind, the list shows
   other authors' merged PRs as "unmerged commits", even when the count logic
   knows better. This is what produced the identical ~18-commit lists.

2. **No fetch before comparing.** Nothing in `checkWorktree` /
   `inspectWorktree` ever runs `git fetch` (the code is deliberately read-only
   per the file's header comment, lines 13–20). `refs/remotes/origin/main` is
   therefore only as fresh as the last fetch performed by anyone in that repo.
   On this host it predated the squash-merge of #758, so even the min'd count
   for w23 was 1 instead of 0, and `Removable()` (**line 260**,
   "its branch has %d commits not in %s") blocked removal of a fully-merged
   branch.

Note: squash merges **are** detected when refs are fresh — `git cherry`'s
patch-id comparison caught the #758 squash immediately once `origin/main`
contained `0b5138c8`. The mechanism is sound; the inputs are stale.

## Proposed fix (needs user approval — not implemented)

In `internal/server/gitops.go`:

1. **List commits against the base that produced the count.** In the
   `checkWorktree` loop (line 314), record which candidate (`base` or
   `refs/remotes/origin/<base>`) yielded the minimum (add a field, e.g.
   unexported `baseRef`), and have `inspectWorktree` line 375 cherry against
   that ref instead of `c.BaseUsed`. Smallest change, kills the misleading
   other-author commit lists immediately.
2. **Fetch before checking (bounded, failure-tolerant).** Run
   `git fetch --quiet --no-tags origin` (with `GIT_TERMINAL_PROMPT=0`, already
   set in `runGit`) at the start of `checkWorktree`, or once per repo per
   removal/inspect sweep; ignore failures (offline → fall back to current
   remote-tracking refs, i.e. today's behaviour). A fetch only updates
   remote-tracking refs; it never touches working trees or local branches.
   Design decisions for the user: fetch on every check vs per-sweep caching,
   and whether workers may make network calls to remotes at all.
3. **Report which base was used and what "unmerged" means.** The
   `merged:`/`pushed:` lines (rendered in `internal/cli/cli.go` lines 803–824)
   should distinguish:
   - "N commits only here (no remote has them)" — the real data-loss risk
     (already computed as `Unpushed`, gitops.go line 411);
   - "N commits on origin/<branch>, not in <base>" — safe to remove;
   - "changes are in <base> (squash-merged PR detected via patch-id)" — safe
     to remove.
   Also print the actual ref compared (`origin/main, fetched <time>`) instead
     of the bare local name, and surface "0 unpushed" prominently in the
     removal gate: `Removable()` (line 260) could allow removal when
     `Unmerged > 0` but `Unpushed == 0`, or at least downgrade the refusal to
     a warning.

### Test plan

- `internal/server/worker_test.go` has the harness (`TestRemoveKeepsDirtyOrUnmerged`,
  line ~285, asserts `"1 commits not in main"`). Add:
  1. repo with local `main` at A, `origin/main` at B (> A); branch off B with
     one own commit; squash-merge the patch onto `origin/main` under a new sha;
     assert `unmerged_commits == 0` (cherry catches the squash through fresh
     refs) and that the detail commit list is empty / names the origin base;
  2. same but *without* the squash commit on `origin/main`: assert count 1 and
     the list contains only the branch's own commit — never the A..B commits of
     other authors;
  3. fetch failure (unreachable remote): assert behaviour falls back to stale
     refs and still reports the base used.
- Manual: re-run `comebye inspect` for both kubevirt sessions after a fetch —
  w23 should read "merged: yes · every commit is in main", w22 "merged: NO ·
  7 commits …" with only its own 7 commits listed and "nothing only here".

## Human decisions needed

- ~~Whether Comebye workers should fetch remotes~~ — **resolved:** yes, fetch before the merge check (bounded, failure-tolerant).
- ~~Whether "unmerged but fully pushed" should block removal~~ — **resolved:** it stays a hard block: an open PR is active work and the agent should stay to answer review. Only the *inputs* were fixed, not the gate.

## Addendum: fix implemented (same day)

On branch `debug/merge-check-false-unmerged`, with the removal gate unchanged:

1. **Fetch before checking** — `fetchOrigin` (`internal/server/gitops.go`) runs
   `git fetch --quiet --no-tags origin` at the start of `checkWorktree`.
   A fetch writes no working tree and no local branch, only remote-tracking
   refs; failures (no origin, offline) are ignored and the checks fall back
   to the refs they have, as before.
2. **Count, base name and detail against the fresher base** — `checkWorktree`
   records which candidate (`main` or `refs/remotes/origin/main`) produced
   the minimum cherry count: `BaseUsed` now reports `origin/main` when that
   ref was used (so "N commits not in origin/main" is honest), and
   `inspectWorktree` runs its cherry/merge-base/rev-list against that same
   ref — the "Unmerged commits" list now shows only the branch's own work,
   never other authors' merged PRs the stale local main lacks.
3. **CLI note reworded** — a clean squash merge is now detected, so the
   parenthetical reads "a squash-merged PR whose patch differs still
   counts: check the PR".

Tests: `TestCheckWorktreeSquashMergedUpstream` (stale local main *and*
stale remote-tracking refs; squash merged upstream → `unmerged 0`, base
`origin/main`, removable, empty commit list) and
`TestCheckWorktreeOpenPRAgainstFresherOrigin` (open PR against fresher
origin/main → `unmerged 1`, only the branch's own commit listed,
`unpushed 0`, removal still blocked). Full `go test ./...` passes.
