package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// landRebased lands the worker's branch the way a worker does after a rebase
// that changed its patch: someone else's commit lands next to the worker's
// line (inside the diff's context), the worker rebases onto it and main
// fast-forwards to the rebased commit. origin/w keeps the pre-rebase commit,
// which git cherry cannot match and whose tree is on no commit of main.
func landRebased(t *testing.T, r landedRepo) (landed string) {
	t.Helper()
	r.edit(t, r.repo, 5, "someone else's work, next to the worker's")
	git(t, r.repo, "push", "-q", "origin", "main")
	// A rebase a moment later: a newer committer date, the author date kept.
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(time.Minute).Format(time.RFC3339))
	git(t, r.wt, "rebase", "-q", "main")
	t.Setenv("GIT_COMMITTER_DATE", "")
	os.Unsetenv("GIT_COMMITTER_DATE")
	git(t, r.repo, "merge", "-q", "--ff-only", "w")
	git(t, r.repo, "push", "-q", "origin", "main")
	return git(t, r.repo, "rev-parse", "main")
}

func backupAt(t *testing.T, r landedRepo, ref string) string {
	t.Helper()
	out, _ := runGit(ctx(), r.repo, "rev-parse", "--verify", "--quiet", ref)
	return strings.TrimSpace(out)
}

func TestStaleBranchSupersededByItsRebaseIsDeletedWithABackup(t *testing.T) {
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	old := git(t, r.repo, "ls-remote", "--heads", "origin", "refs/heads/w")
	old = strings.Fields(old)[0]
	landed := landRebased(t, r)
	// The two older proofs do not hold: that is the case this is for.
	if c := git(t, r.repo, "cherry", "origin/main", old); !strings.HasPrefix(c, "+") {
		t.Fatalf("git cherry matched the pre-rebase commit: %q", c)
	}
	if treeOnBase(ctx(), r.repo, "refs/remotes/origin/main", old) {
		t.Fatal("the tree is on main")
	}
	got := checkStaleBranch(ctx(), r.repo, "w", "w", false)
	if got.State != staleInMain || got.Deleted || !strings.Contains(got.Proof, "superseded by its rebase") ||
		!strings.Contains(got.Proof, short(old)+" was rebased as "+short(landed)) || len(got.Superseded) != 1 || got.Superseded[0].As != landed {
		t.Fatalf("check = %+v", got)
	}
	if !remoteHas(t, r, "w") || backupAt(t, r, "refs/comebye/backup/w") != "" {
		t.Fatal("a check deleted the branch or wrote a backup")
	}
	got = checkStaleBranch(ctx(), r.repo, "w", "w", true)
	if got.State != staleInMain || !got.Deleted || got.Backup != "refs/comebye/backup/w" {
		t.Fatalf("delete = %+v", got)
	}
	if remoteHas(t, r, "w") {
		t.Fatal("still on origin")
	}
	if b := backupAt(t, r, got.Backup); b != old {
		t.Fatalf("backup ref = %q, want %s", b, old)
	}
	// Idempotent: gone now, and the backup stays as it was.
	if again := checkStaleBranch(ctx(), r.repo, "w", "w", true); again.State != staleAbsent || again.Deleted {
		t.Fatalf("again = %+v", again)
	}
	if backupAt(t, r, "refs/comebye/backup/w") != old {
		t.Fatal("the backup moved")
	}
	// The restore the log names puts it back as it was.
	git(t, r.repo, strings.Fields(restoreFromBackupCmd(got.Backup, "w"))[1:]...)
	if tip := strings.Fields(git(t, r.repo, "ls-remote", "--heads", "origin", "refs/heads/w"))[0]; tip != old {
		t.Fatalf("restored at %s, want %s", tip, old)
	}
}

func TestStaleBranchRebasedWithAnExtraCommitIsKept(t *testing.T) {
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	pre := git(t, r.wt, "rev-parse", "HEAD")
	landRebased(t, r)
	// One more commit on top of the pre-rebase commit, pushed to the backup,
	// that main does not have.
	git(t, r.wt, "checkout", "-q", "-b", "extra", pre)
	r.edit(t, r.wt, 20, "work that never landed")
	git(t, r.wt, "push", "-q", "-f", "origin", "extra:w")
	got := checkStaleBranch(ctx(), r.repo, "w", "w", true)
	if got.State != staleNotInMain || got.Deleted || !remoteHas(t, r, "w") || backupAt(t, r, "refs/comebye/backup/w") != "" {
		t.Fatalf("extra commit = %+v", got)
	}
}

func TestStaleBranchRebasedButNotTheSessionsOwnIsRefused(t *testing.T) {
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	landRebased(t, r)
	for _, own := range []string{"other", ""} {
		if got := checkStaleBranch(ctx(), r.repo, "w", own, true); got.State != staleRefused || got.Deleted {
			t.Fatalf("own %q = %+v", own, got)
		}
	}
	if !remoteHas(t, r, "w") {
		t.Fatal("deleted a branch that is not the session's own")
	}
}

// The same subject alone is no proof: a commit made after the land (an amend
// that keeps the author date), or another change under the same author, date and
// subject, is kept.
func TestStaleBranchSameSubjectIsNotEnough(t *testing.T) {
	stubOpenPRs(t, nil, nil)
	t.Run("amended after the land", func(t *testing.T) {
		r := backedUp(t)
		pre := git(t, r.wt, "rev-parse", "HEAD")
		landRebased(t, r)
		git(t, r.wt, "checkout", "-q", "-b", "late", pre)
		os.WriteFile(r.wt+"/late.txt", []byte("late\n"), 0o644)
		git(t, r.wt, "add", "late.txt")
		t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(time.Hour).Format(time.RFC3339))
		git(t, r.wt, "commit", "-q", "--amend", "--no-edit")
		git(t, r.wt, "push", "-q", "-f", "origin", "late:w")
		if got := checkStaleBranch(ctx(), r.repo, "w", "w", true); got.State != staleNotInMain || got.Deleted || !remoteHas(t, r, "w") {
			t.Fatalf("amended later = %+v", got)
		}
	})
	t.Run("another change under the same name", func(t *testing.T) {
		r := backedUp(t)
		pre := git(t, r.wt, "rev-parse", "HEAD")
		ad := git(t, r.wt, "log", "-1", "--format=%ad", "--date=raw", pre)
		// main gets an unrelated change with the worker's author date and subject.
		os.WriteFile(r.repo+"/other.txt", []byte(strings.Repeat("something else entirely\n", 20)), 0o644)
		git(t, r.repo, "add", "other.txt")
		t.Setenv("GIT_AUTHOR_DATE", ad)
		t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(time.Minute).Format(time.RFC3339))
		git(t, r.repo, "commit", "-q", "-m", "the worker's change")
		git(t, r.repo, "push", "-q", "origin", "main")
		if got := checkStaleBranch(ctx(), r.repo, "w", "w", true); got.State != staleNotInMain || got.Deleted || !remoteHas(t, r, "w") {
			t.Fatalf("same subject, other change = %+v", got)
		}
	})
}

func TestBackupRefNeverMovesAnother(t *testing.T) {
	r := backedUp(t)
	a := git(t, r.repo, "rev-parse", "main")
	b := git(t, r.wt, "rev-parse", "HEAD")
	git(t, r.repo, "update-ref", "refs/comebye/backup/w", a)
	ref, err := backupRef(ctx(), r.repo, "w", b)
	if err != nil || ref != "refs/comebye/backup/w-"+b[:12] || backupAt(t, r, ref) != b || backupAt(t, r, "refs/comebye/backup/w") != a {
		t.Fatalf("ref %q err %v", ref, err)
	}
	if again, err := backupRef(ctx(), r.repo, "w", b); err != nil || again != ref {
		t.Fatalf("again %q %v", again, err)
	}
}

// The sweep loop, for a gone session whose only leftover is the pre-rebase
// backup: it deletes the branch (keeping the backup ref), marks the item done
// by the hub, removes the session, and says so with the branch and SHA. A
// second run does nothing.
func TestSweepLoopCleansASupersededPreRebaseBranch(t *testing.T) {
	g := newFixLanded(t, func(t *testing.T, r landedRepo) { landRebased(t, r) }, "", goneID, "w9:p9", 12)
	tip := git(t, g.r.repo, "rev-parse", "refs/remotes/origin/w")
	now := time.Now()
	it := reviewOf(t, g.s, goneID)
	if it.AskReview == nil || !it.AskReview.StaleOnly || !strings.Contains(it.AskReview.Items[0].Now, "superseded by its rebase") {
		t.Fatalf("review = %+v", it.AskReview)
	}
	g.s.sweepLoopStep(ctx(), now)
	if remoteHas(t, g.r, "w") {
		t.Fatal("the superseded branch is still on origin")
	}
	if backupAt(t, g.r, "refs/comebye/backup/w") != tip {
		t.Fatal("no backup ref at the deleted tip")
	}
	e, _ := g.s.ledger.Get(g.e.ID)
	if e.Status != ledger.Removed || e.AutoRemoved == nil {
		t.Fatalf("entry = %s %+v", e.Status, e.AutoRemoved)
	}
	st := g.s.sweeps.get()
	var br *sweepDone
	for i, d := range st.Done {
		if d.Kind == doneBranch {
			br = &st.Done[i]
		}
	}
	if len(st.Done) != 2 || br == nil || br.Branch != "w" || br.Tip != tip || br.Backup != "refs/comebye/backup/w" {
		t.Fatalf("done = %+v", st.Done)
	}
	title, _, _ := g.s.sweepBoardLine()
	if !strings.Contains(title, "origin/w @ "+tip[:7]) || !strings.Contains(title, "removed 1 session") {
		t.Fatalf("board line = %q", title)
	}
	var logged string
	for _, ev := range g.s.events.query(time.Time{}, []string{evSweep}, 0) {
		logged += ev.Text + "\n"
	}
	if !strings.Contains(logged, tip) || !strings.Contains(logged, "refs/comebye/backup/w") {
		t.Fatalf("log = %q", logged)
	}
	if e.Removal == nil {
		t.Fatal("no removal record")
	}
	n := len(g.s.events.query(time.Time{}, []string{evSweep}, 0))
	g.s.sweepLoopStep(ctx(), now.Add(2*time.Hour))
	if got := len(g.s.sweeps.get().Done); got != 2 || len(g.s.events.query(time.Time{}, []string{evSweep}, 0)) != n {
		t.Fatalf("a second run did something: %d done", got)
	}
}
