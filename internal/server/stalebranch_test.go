package server

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// stubOpenPRs makes gh's open pull requests for the test: none, or n.
func stubOpenPRs(t *testing.T, prs []ghPR, err error) {
	t.Helper()
	old := ghOpenPRs
	ghOpenPRs = func(context.Context, string, string) ([]ghPR, error) { return prs, err }
	t.Cleanup(func() { ghOpenPRs = old })
}

func remoteHas(t *testing.T, r landedRepo, branch string) bool {
	t.Helper()
	return git(t, r.repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch) != ""
}

// backedUp pushes the worker's branch w (the pre-rebase backup) with one commit.
func backedUp(t *testing.T) landedRepo {
	t.Helper()
	r := newLandedRepo(t)
	r.edit(t, r.wt, 3, "the worker's change")
	git(t, r.wt, "push", "-q", "origin", "w")
	return r
}

func TestStaleBranchProvablyInMain(t *testing.T) {
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	// Not landed yet: the backup has a commit main lacks.
	if got := checkStaleBranch(ctx(), r.repo, "w", "w", false); got.State != staleNotInMain || got.Ahead != 1 {
		t.Fatalf("unlanded = %+v", got)
	}
	// Landed by a cherry-pick (a rebase: other SHA, same patch).
	r.landCherryPick(t)
	got := checkStaleBranch(ctx(), r.repo, "w", "w", false)
	if got.State != staleInMain || !strings.Contains(got.Proof, "git cherry") || got.Tip == "" || got.Deleted {
		t.Fatalf("rebased = %+v", got)
	}
	if !remoteHas(t, r, "w") {
		t.Fatal("a check deleted the branch")
	}
	// One extra commit on the backup that main lacks: kept.
	r.edit(t, r.wt, 10, "one more")
	git(t, r.wt, "push", "-q", "origin", "w")
	if got := checkStaleBranch(ctx(), r.repo, "w", "w", true); got.State != staleNotInMain || got.Ahead != 1 || got.Deleted {
		t.Fatalf("one extra commit = %+v", got)
	}
	if !remoteHas(t, r, "w") {
		t.Fatal("a branch with a commit not in main was deleted")
	}
}

func TestStaleBranchKeepsWhatItCannotProve(t *testing.T) {
	r := backedUp(t)
	r.landCherryPick(t)
	// An open PR from it.
	stubOpenPRs(t, []ghPR{{Number: 7}}, nil)
	if got := checkStaleBranch(ctx(), r.repo, "w", "w", true); got.State != staleOpenPR || got.PR != 7 || got.Deleted || !remoteHas(t, r, "w") {
		t.Fatalf("open PR = %+v", got)
	}
	// gh broken: no proof, nothing deleted.
	stubOpenPRs(t, nil, errors.New("gh: not logged in"))
	if got := checkStaleBranch(ctx(), r.repo, "w", "w", true); got.State != staleUnknown || got.Deleted || !remoteHas(t, r, "w") {
		t.Fatalf("gh failing = %+v", got)
	}
	stubOpenPRs(t, nil, nil)
	// Somebody else's branch, and the default branch: refused.
	git(t, r.repo, "push", "-q", "origin", "main:refs/heads/other")
	for _, c := range []struct{ branch, own string }{{"other", "w"}, {"main", "main"}, {"w", ""}} {
		if got := checkStaleBranch(ctx(), r.repo, c.branch, c.own, true); got.State != staleRefused || got.Deleted {
			t.Errorf("%s as %q = %+v", c.branch, c.own, got)
		}
	}
	if !remoteHas(t, r, "other") || !remoteHas(t, r, "main") {
		t.Fatal("a branch that is not the session's own was deleted")
	}
}

func TestStaleBranchTreeEqualToMainCommit(t *testing.T) {
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	// Squash-merge: main gets one commit with the branch's exact tree (no equal patch
	// per commit when the branch has several).
	r.edit(t, r.wt, 5, "second change")
	git(t, r.wt, "push", "-q", "origin", "w")
	git(t, r.repo, "merge", "--squash", "-q", "w")
	git(t, r.repo, "commit", "-q", "-m", "squash")
	git(t, r.repo, "push", "-q", "origin", "main")
	got := checkStaleBranch(ctx(), r.repo, "w", "w", true)
	if got.State != staleInMain || !got.Deleted || !strings.Contains(got.Proof, "tree is identical") {
		t.Fatalf("squash = %+v", got)
	}
	if remoteHas(t, r, "w") {
		t.Fatal("the proven branch is still on origin")
	}
	// Gone from origin now.
	if got := checkStaleBranch(ctx(), r.repo, "w", "w", true); got.State != staleAbsent {
		t.Fatalf("again = %+v", got)
	}
}

func TestStaleBranchMovedBetweenCheckAndDeleteIsKept(t *testing.T) {
	// The lease: a tip that is not the one verified is never deleted.
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	r.landCherryPick(t)
	tip := git(t, r.repo, "ls-remote", "--heads", "origin", "refs/heads/w")
	tip = strings.Fields(tip)[0]
	r.edit(t, r.wt, 12, "late commit")
	git(t, r.wt, "push", "-q", "origin", "w")
	if _, err := runGit(ctx(), r.repo, "push", "--force-with-lease=refs/heads/w:"+tip, "origin", ":refs/heads/w"); err == nil {
		t.Fatal("the lease did not hold")
	}
	if !remoteHas(t, r, "w") {
		t.Fatal("deleted")
	}
}

func TestRemoteBranchItemWords(t *testing.T) {
	own := "feat/tool-runs-6-keyboard"
	for text, want := range map[string]bool{
		"Delete the backup branch `origin/feat/tool-runs-6-keyboard`. It still points at the pre-rebase commit 4e3219b, and the same content is already in main as 8551b4f. This waits on a go-ahead.": true,
		"delete remote branch " + own + " (already in origin/main)":                      true,
		"git push origin --delete " + own:                                                false, // no "branch"
		"delete the remote branch origin/feat/other-thing":                               false,
		"delete the local branch " + own:                                                 false, // not remote
		"delete branches origin/" + own + " and origin/feat/x, the remote branch copies": false,
		"stop the dev server and delete the remote branch " + own:                        true,
		"close PR o/r#3 then delete the remote branch " + own:                            false,
	} {
		if _, ok := remoteBranchItem(text, own); ok != want {
			t.Errorf("%q = %v, want %v", text, ok, want)
		}
	}
}

// ---- the loop ----

// goneFix is the user's example: a marked session whose agent is gone, whose
// one leftover is deleting its own pre-rebase backup branch on origin, and
// whose work is in main.
type goneFix struct {
	s *Server
	r landedRepo
	m ledger.Mark
	e ledger.Entry
}

const goneID = "desk/w9:p9"

func newGoneFix(t *testing.T, land bool, text string) *goneFix {
	t.Helper()
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	if land {
		r.landCherryPick(t)
	}
	s, fh, _ := coordServer(t)
	s.cfg.LaunchRoots = []string{filepath.Dir(r.repo)}
	s.probeTTL = 0
	// herdr knows no workspace for the gone agent: closing it is a no-op.
	fh.set(func(f *fakeLaunchHerdr) {
		f.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
			switch method {
			case "workspace.list":
				return map[string]any{"type": "workspace_list", "workspaces": []any{}}, nil, true
			case "worktree.list":
				return map[string]any{"type": "worktree_list", "source": map[string]any{}, "worktrees": []any{}}, nil, true
			case "workspace.close", "pane.close":
				return map[string]any{"type": "ok"}, nil, true
			}
			return nil, nil, false
		}
	})
	if text == "" {
		text = "Delete the backup branch `origin/w`. It still points at the pre-rebase commit, and the same content is already in main. This waits on a go-ahead."
	}
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: r.repo, RepoName: "proj", Branch: "w", Base: "main", AgentID: goneID, PaneID: "w9:p9", AgentName: "gone", WorkspaceID: "w9", Worktree: r.wt, Status: ledger.Finished})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-26 * time.Hour)
	m := ledger.Mark{AgentID: goneID, Host: "desk", PaneID: "w9:p9", Name: "gone", By: "coordinator", Reason: "landed, CI green", At: at.Add(-48 * time.Hour),
		Ask: &ledger.Ask{State: ledger.AskUnsafe, Reason: "delete backup", At: at, AskedAt: &at, Items: []ledger.AskItem{{Type: ledger.ItemCleanup, Text: text}}}}
	if _, err := s.ledger.SetMark(m); err != nil {
		t.Fatal(err)
	}
	return &goneFix{s: s, r: r, m: m, e: e}
}

func (f *goneFix) setAuto(t *testing.T, on bool) {
	t.Helper()
	f.s.settings.mu.Lock()
	f.s.settings.cur.SweepLoop = &sweepLoopSettings{AutoDeleteBranches: &on}
	f.s.settings.mu.Unlock()
}

func (f *goneFix) ask(t *testing.T) *ledger.Ask {
	t.Helper()
	e, ok := f.s.ledger.ByAgent(goneID)
	if !ok || e.Mark == nil {
		t.Fatal("no mark")
	}
	return e.Mark.Ask
}

func TestGoneSessionLeftoverShowsAsSafeWithTheDeletionNamed(t *testing.T) {
	f := newGoneFix(t, true, "")
	it := reviewOf(t, f.s, goneID)
	if !it.Safe || it.AskReview == nil || !it.AskReview.StaleOnly {
		t.Fatalf("review = safe %v, %+v warnings %v", it.Safe, it.AskReview, it.Warnings)
	}
	v := it.AskReview.Items[0]
	if !v.Stale || !strings.Contains(v.Now, "safe to delete") || !strings.Contains(v.Proof, "git cherry") {
		t.Fatalf("item = %+v", v)
	}
	if !strings.Contains(strings.Join(it.Notes, "\n"), "Confirming the removal deletes it") {
		t.Fatalf("notes = %v", it.Notes)
	}
	if !remoteHas(t, f.r, "w") {
		t.Fatal("looking deleted it")
	}
}

func TestGoneSessionLeftoverNotInMainStaysLeft(t *testing.T) {
	f := newGoneFix(t, false, "")
	it := reviewOf(t, f.s, goneID)
	if it.Safe || it.AskReview.StaleOnly || len(it.Warnings) == 0 || !strings.Contains(strings.Join(it.Warnings, "\n"), "is kept") {
		t.Fatalf("review = safe %v warnings %v", it.Safe, it.Warnings)
	}
	f.setAuto(t, true)
	if _, deleted := f.s.leftovers(ctx(), time.Now()); len(deleted) != 0 || !remoteHas(t, f.r, "w") {
		t.Fatalf("deleted %v", deleted)
	}
}

func TestSweepLeftoversDefaultModeDeletesNothingAndTheItemNamesIt(t *testing.T) {
	f := newGoneFix(t, true, "")
	s := f.s
	if n, deleted := s.leftovers(ctx(), time.Now()); n != 0 || len(deleted) != 0 {
		t.Fatalf("default mode settled %d, deleted %v", n, deleted)
	}
	if !remoteHas(t, f.r, "w") {
		t.Fatal("default mode deleted the branch")
	}
	// The day's item: the session is in it, with the branch marked for deletion.
	s.sweepLoopStep(ctx(), time.Now())
	_, active := sweepItemsOf(s)
	if len(active) != 1 {
		t.Fatalf("items = %v", active)
	}
	c := active[0].Context
	if !strings.Contains(c, "delete* origin/w") || !strings.Contains(c, "also deletes 1 stale remote branch") {
		t.Fatalf("context = %q", c)
	}
	t.Logf("%s\n%s", active[0].Question, c)
	// The one confirm removes the session and deletes the branch first.
	w := do(s, "POST", "/api/removals/confirm", userHdr, `{"ids":["`+goneID+`"]}`)
	if w.Code != 200 {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	if remoteHas(t, f.r, "w") {
		t.Fatalf("the confirm did not delete the branch: %s", w.Body)
	}
	if e, ok := s.ledger.Get(f.e.ID); !ok || e.Status != ledger.Removed {
		t.Fatalf("entry = %+v: %s", e, w.Body)
	}
}

func TestConfirmKeepsTheBranchWhenItGotNewWork(t *testing.T) {
	f := newGoneFix(t, true, "")
	// A commit main lacks lands on the backup after the review.
	f.r.edit(t, f.r.wt, 12, "late work")
	git(t, f.r.wt, "push", "-q", "origin", "w")
	w := do(f.s, "POST", "/api/removals/confirm", userHdr, `{"ids":["`+goneID+`"]}`)
	// Nothing is lost: the branch stays on origin, and the ordinary removal keeps
	// the worktree whose branch has commits main lacks.
	if !remoteHas(t, f.r, "w") || !strings.Contains(w.Body.String(), `"worktree_removed":false`) {
		t.Fatalf("a branch with new work was deleted or its worktree removed: %d %s", w.Code, w.Body)
	}
}

func TestSweepLeftoversAutoModeDeletesOnceAndSaysSo(t *testing.T) {
	f := newGoneFix(t, true, "")
	f.setAuto(t, true)
	s := f.s
	now := time.Now()
	n, deleted := s.leftovers(ctx(), now)
	if n != 1 || len(deleted) != 1 || deleted[0] != "origin/w" {
		t.Fatalf("settled %d, deleted %v", n, deleted)
	}
	if remoteHas(t, f.r, "w") {
		t.Fatal("still on origin")
	}
	a := f.ask(t)
	if a.State != ledger.AskSafe || a.Loop == nil || !strings.Contains(a.Loop.Text, "deleted origin/w") || a.Loop.At.Before(now.Add(-time.Second)) {
		t.Fatalf("ask = %+v loop %+v", a, a.Loop)
	}
	// Idempotent: a second run does nothing new.
	if n, deleted := s.leftovers(ctx(), now.Add(time.Hour)); n != 0 || len(deleted) != 0 {
		t.Fatalf("second run settled %d deleted %v", n, deleted)
	}
	// The sweep loop's own run records it, in the board line and the state.
	g := newGoneFix(t, true, "")
	g.setAuto(t, true)
	g.s.sweepLoopStep(ctx(), now)
	st := g.s.sweeps.get()
	if len(st.LastDeleted) != 1 || st.LastSettled != 1 {
		t.Fatalf("state = %+v", st)
	}
	if title, _, _ := g.s.sweepBoardLine(); !strings.Contains(title, "deleted 1 stale remote branch") {
		t.Fatalf("board line = %q", title)
	}
	// The gone session is now plain "ready": it is in the day's item, deleting nothing more.
	g.s.sweepLoopStep(ctx(), now.Add(2*time.Hour))
	if it := reviewOf(t, g.s, goneID); !it.Safe || it.AskReview != nil && it.AskReview.Loop == nil {
		t.Fatalf("review = %+v", it)
	}
}

func TestSweepLeftoversOtherCleanupIsLeftAlone(t *testing.T) {
	f := newGoneFix(t, true, "Stop the dev server I started (pid 4242).")
	f.setAuto(t, true)
	if n, deleted := f.s.leftovers(ctx(), time.Now()); n != 0 || len(deleted) != 0 {
		t.Fatalf("settled %d deleted %v", n, deleted)
	}
	a := f.ask(t)
	if a.State != ledger.AskUnsafe || a.Loop != nil {
		t.Fatalf("ask = %+v", a)
	}
	if it := reviewOf(t, f.s, goneID); it.Safe {
		t.Fatalf("a process to stop was treated as done: %v", it.Warnings)
	}
}

func TestSweepLeftoversRecheckedAnswerBecomesNothingLeft(t *testing.T) {
	// An item that resolved on its own (the branch is already gone from origin).
	f := newGoneFix(t, true, "")
	git(t, f.r.repo, "push", "-q", "origin", ":refs/heads/w")
	n, deleted := f.s.leftovers(ctx(), time.Now())
	if n != 1 || len(deleted) != 0 {
		t.Fatalf("settled %d deleted %v", n, deleted)
	}
	a := f.ask(t)
	if a.State != ledger.AskSafe || a.Loop == nil || !strings.Contains(a.Reason, "re-checked by the sweep") {
		t.Fatalf("ask = %+v", a)
	}
}

func TestSweepLeftoversSkipsASessionWhoseAgentIsLive(t *testing.T) {
	f := newGoneFix(t, true, "")
	f.setAuto(t, true)
	// Make the agent live: it is asked, not acted for.
	it := &removal{ID: goneID, Host: "desk", EntryID: f.e.ID, Branch: "w", Worktree: f.r.wt, Mark: f.m}
	it.Live.Present = true
	if names, err := f.s.deleteStaleFor(ctx(), it, "the sweep", time.Now()); names != nil || err != nil || !remoteHas(t, f.r, "w") {
		t.Fatalf("a live session's branch: %v %v", names, err)
	}
}
