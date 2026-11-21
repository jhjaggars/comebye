package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
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

// liveID is the idle agent "home" of coordServer.
const liveID = "desk/w2:p1"

func newGoneFix(t *testing.T, land bool, text string) *goneFix {
	t.Helper()
	return newFix(t, land, text, goneID, "w9:p9", 0)
}

// newFix seeds a marked session whose answer lists one cleanup item and has been
// re-asked reasks times. id is its agent: goneID (not in herdr) or liveID (idle there).
func newFix(t *testing.T, land bool, text, id, pane string, reasks int) *goneFix {
	t.Helper()
	var how func(*testing.T, landedRepo)
	if land {
		how = func(t *testing.T, r landedRepo) { r.landCherryPick(t) }
	}
	return newFixLanded(t, how, text, id, pane, reasks)
}

// newFixLanded is newFix with the landing done by land (nil: not landed).
func newFixLanded(t *testing.T, land func(*testing.T, landedRepo), text, id, pane string, reasks int) *goneFix {
	t.Helper()
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	if land != nil {
		land(t, r)
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
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: r.repo, RepoName: "proj", Branch: "w", Base: "main", AgentID: id, PaneID: pane, AgentName: "gone", WorkspaceID: "w9", Worktree: r.wt, Status: ledger.Finished})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-26 * time.Hour)
	m := ledger.Mark{AgentID: id, Host: "desk", PaneID: pane, Name: "gone", By: "coordinator", Reason: "landed, CI green", At: at.Add(-48 * time.Hour),
		Ask: &ledger.Ask{State: ledger.AskUnsafe, Reason: "delete backup", At: at, AskedAt: &at, Reasks: reasks, Items: []ledger.AskItem{{Type: ledger.ItemCleanup, Text: text}}}}
	if _, err := s.ledger.SetMark(m); err != nil {
		t.Fatal(err)
	}
	return &goneFix{s: s, r: r, m: m, e: e}
}

func (f *goneFix) setAuto(t *testing.T, on bool) {
	t.Helper()
	f.s.settings.mu.Lock()
	f.s.settings.cur.SweepLoop = &sweepLoopSettings{Enabled: &on}
	f.s.settings.mu.Unlock()
}

func (f *goneFix) ask(t *testing.T) *ledger.Ask {
	t.Helper()
	e, ok := f.s.ledger.ByAgent(f.m.AgentID)
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
	if _, deleted := f.s.leftovers(ctx(), time.Now(), true); len(deleted) != 0 || !remoteHas(t, f.r, "w") {
		t.Fatalf("deleted %v", deleted)
	}
}

// Clean-up off: the loop only marks. It deletes no branch, removes no session
// and files no To do; the user's own confirm still does both.
func TestSweepSettingOffDeletesNothingAndFilesNoItem(t *testing.T) {
	f := newGoneFix(t, true, "")
	f.setAuto(t, false)
	s := f.s
	if n, deleted := s.leftovers(ctx(), time.Now(), false); n != 0 || len(deleted) != 0 {
		t.Fatalf("off settled %d, deleted %v", n, deleted)
	}
	s.sweepLoopStep(ctx(), time.Now())
	if !remoteHas(t, f.r, "w") {
		t.Fatal("the setting is off and the branch was deleted")
	}
	if e, _ := s.ledger.Get(f.e.ID); e.Status != ledger.ReadyToRemove {
		t.Fatalf("the setting is off and the session was %s", e.Status)
	}
	if all, _ := sweepItemsOf(s); len(all) != 0 {
		t.Fatalf("a To do was filed: %+v", all)
	}
	if title, _, _ := s.sweepBoardLine(); !strings.Contains(title, "automatic clean-up is off") {
		t.Fatalf("board line = %q", title)
	}
	// The one confirm in the review removes the session and deletes the branch first.
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
	s := f.s
	now := time.Now()
	tip := git(t, f.r.repo, "rev-parse", "refs/remotes/origin/w")
	n, deleted := s.leftovers(ctx(), now, true)
	if n != 1 || len(deleted) != 1 || deleted[0].Kind != doneBranch || deleted[0].Branch != "w" || deleted[0].Tip != tip ||
		deleted[0].Restore != "git push origin "+tip+":refs/heads/w" || deleted[0].Backup != "refs/comebye/backup/w" {
		t.Fatalf("settled %d, deleted %+v (tip %s)", n, deleted, tip)
	}
	if remoteHas(t, f.r, "w") {
		t.Fatal("still on origin")
	}
	a := f.ask(t)
	if a.State != ledger.AskSafe || a.Loop == nil || !strings.Contains(a.Loop.Text, "deleted origin/w") || !strings.Contains(a.Loop.Text, tip) || a.Loop.At.Before(now.Add(-time.Second)) {
		t.Fatalf("ask = %+v loop %+v", a, a.Loop)
	}
	// Idempotent: a second run does nothing new.
	if n, deleted := s.leftovers(ctx(), now.Add(time.Hour), true); n != 0 || len(deleted) != 0 {
		t.Fatalf("second run settled %d deleted %v", n, deleted)
	}
}

// The loop's own run deletes the branch and removes the session with nothing
// to lose, says so on the board and in the log with the SHA, files no To do,
// and a second run does nothing.
func TestSweepLoopCleansAGoneSessionAndSaysSo(t *testing.T) {
	g := newGoneFix(t, true, "")
	now := time.Now()
	tip := git(t, g.r.repo, "rev-parse", "refs/remotes/origin/w")
	g.s.sweepLoopStep(ctx(), now)
	st := g.s.sweeps.get()
	if st.LastSettled != 1 || len(st.Done) != 2 {
		t.Fatalf("state = %+v", st)
	}
	if e, _ := g.s.ledger.Get(g.e.ID); e.Status != ledger.Removed || e.AutoRemoved == nil || !strings.Contains(e.AutoRemoved.Reason, "nothing to lose") {
		t.Fatalf("entry = %+v", e)
	}
	if _, err := os.Stat(g.r.wt); err == nil {
		t.Fatal("the worktree is still there")
	}
	if remoteHas(t, g.r, "w") {
		t.Fatal("the stale branch is still on origin")
	}
	title, _, _ := g.s.sweepBoardLine()
	if !strings.Contains(title, "removed 1 session (gone)") || !strings.Contains(title, "deleted 1 stale remote branch (origin/w @ "+tip[:7]) {
		t.Fatalf("board line = %q", title)
	}
	var logged string
	for _, ev := range g.s.events.query(time.Time{}, []string{evSweep}, 0) {
		logged += ev.Text + "\n"
	}
	if !strings.Contains(logged, tip) || !strings.Contains(logged, "Sweep removed 1 session") {
		t.Fatalf("log = %q", logged)
	}
	if all, _ := sweepItemsOf(g.s); len(all) != 0 {
		t.Fatalf("a To do was filed: %+v", all)
	}
	// A second run does nothing, and logs nothing.
	n := len(g.s.events.query(time.Time{}, []string{evSweep}, 0))
	g.s.sweepLoopStep(ctx(), now.Add(2*time.Hour))
	if got := len(g.s.sweeps.get().Done); got != 2 || len(g.s.events.query(time.Time{}, []string{evSweep}, 0)) != n {
		t.Fatalf("a second run did something: %d done", got)
	}
}

// pidRunning makes pid 4242 the one process alive (a "pid" leftover is checked).
func pidRunning(t *testing.T, alive bool) {
	t.Helper()
	old := probePIDAlive
	probePIDAlive = func(pid int) bool { return alive && pid == 4242 }
	t.Cleanup(func() { probePIDAlive = old })
}

func TestSweepLeftoversOtherCleanupIsLeftAlone(t *testing.T) {
	f := newGoneFix(t, true, "Stop the dev server I started (pid 4242).")
	pidRunning(t, true)
	f.setAuto(t, true)
	if n, deleted := f.s.leftovers(ctx(), time.Now(), true); n != 0 || len(deleted) != 0 {
		t.Fatalf("settled %d deleted %v", n, deleted)
	}
	a := f.ask(t)
	if a.State != ledger.AskUnsafe || a.Loop != nil {
		t.Fatalf("ask = %+v", a)
	}
	if it := reviewOf(t, f.s, goneID); it.Safe {
		t.Fatalf("a process to stop was treated as done: %v", it.Warnings)
	}
	// Once the process is gone, the line (re-typed as a pid) has resolved.
	pidRunning(t, false)
	if n, _ := f.s.leftovers(ctx(), time.Now(), true); n != 1 || f.ask(t).State != ledger.AskSafe {
		t.Fatalf("a stopped process: settled %d, ask %+v", n, f.ask(t))
	}
}

func TestSweepLeftoversRecheckedAnswerBecomesNothingLeft(t *testing.T) {
	// An item that resolved on its own (the branch is already gone from origin).
	f := newGoneFix(t, true, "")
	git(t, f.r.repo, "push", "-q", "origin", ":refs/heads/w")
	n, deleted := f.s.leftovers(ctx(), time.Now(), true)
	if n != 1 || len(deleted) != 0 {
		t.Fatalf("settled %d deleted %v", n, deleted)
	}
	a := f.ask(t)
	if a.State != ledger.AskSafe || a.Loop == nil || !strings.Contains(a.Reason, "re-checked by the sweep") {
		t.Fatalf("ask = %+v", a)
	}
}

func TestSweepLeftoversSkipsAWorkingAgent(t *testing.T) {
	f := newGoneFix(t, true, "")
	f.setAuto(t, true)
	// A working agent is left alone.
	it := &removal{ID: goneID, Host: "desk", EntryID: f.e.ID, Branch: "w", Worktree: f.r.wt, Mark: f.m}
	it.Live.Present, it.Live.Status = true, herdr.StatusWorking
	if names, err := f.s.deleteStaleFor(ctx(), it, "the sweep", time.Now()); names != nil || err != nil || !remoteHas(t, f.r, "w") {
		t.Fatalf("a working session's branch: %v %v", names, err)
	}
}

// ---- a live agent that keeps answering "waiting on a go-ahead" ----

const layeredText = "Delete the remote branch `w` on origin. PR 38 is now CLOSED, but the branch still holds the old pre-rebase commits, whose SHAs differ from the ones on main. Waiting on a go-ahead."

func quietReasks(s *Server) {
	s.latest = func(context.Context, string, string) *transcript.Latest {
		return &transcript.Latest{Text: "waiting", Time: "t"}
	}
}

func TestLiveAgentRebasedBranchIsSettledNotReasked(t *testing.T) {
	f := newFix(t, true, layeredText, liveID, "w2:p1", 12)
	quietReasks(f.s)
	it := reviewOf(t, f.s, liveID)
	if !it.Live.Present || !it.Safe || it.AskReview == nil || !it.AskReview.StaleOnly || !it.AskReview.Items[0].Stale {
		t.Fatalf("review = live %v safe %v %+v warnings %v", it.Live.Present, it.Safe, it.AskReview, it.Warnings)
	}
	// Twelve re-asks are enough: the hub does not ask again, and the open review did not either.
	if asked := f.s.reaskStep(ctx(), time.Now().Add(48*time.Hour)); len(asked) != 0 {
		t.Fatalf("asked again: %v", asked)
	}
	if got := f.ask(t).ReaskedAt; got != nil {
		t.Fatalf("the review re-asked: %v", got)
	}
	// Clean-up off: nothing deleted.
	if n, deleted := f.s.leftovers(ctx(), time.Now(), false); n != 0 || len(deleted) != 0 || !remoteHas(t, f.r, "w") {
		t.Fatalf("off: settled %d, deleted %v", n, deleted)
	}
	// On: the loop gives the go-ahead itself, once.
	n, deleted := f.s.leftovers(ctx(), time.Now(), true)
	if n != 1 || len(deleted) != 1 || remoteHas(t, f.r, "w") {
		t.Fatalf("auto settled %d, deleted %v", n, deleted)
	}
	if a := f.ask(t); a.State != ledger.AskSafe || a.Reasks != 12 || a.Loop == nil {
		t.Fatalf("ask = %+v", a)
	}
	if n, deleted := f.s.leftovers(ctx(), time.Now(), true); n != 0 || len(deleted) != 0 {
		t.Fatalf("second run settled %d deleted %v", n, deleted)
	}
}

func TestLiveAgentBranchWithUnmergedWorkIsNeverDeleted(t *testing.T) {
	f := newFix(t, false, layeredText, liveID, "w2:p1", 12)
	quietReasks(f.s)
	f.setAuto(t, true)
	if n, deleted := f.s.leftovers(ctx(), time.Now(), true); n != 0 || len(deleted) != 0 || !remoteHas(t, f.r, "w") {
		t.Fatalf("settled %d deleted %v", n, deleted)
	}
}

func TestReaskStopsAndTheLeftoverStaysInTheReviewWithNoToDo(t *testing.T) {
	const stop = "Stop the dev server I started (pid 4242)."
	// Under the cap the live agent is asked again (the control)...
	pidRunning(t, true)
	c := newFix(t, true, stop, liveID, "w2:p1", 0)
	quietReasks(c.s)
	if asked := c.s.reaskStep(ctx(), time.Now().Add(48*time.Hour)); len(asked) != 1 {
		t.Fatalf("control: asked %v", asked)
	}
	c.s.asking.Wait()
	// ...at the cap it is not, and the day's item lists what is left.
	f := newFix(t, true, stop, liveID, "w2:p1", 12)
	quietReasks(f.s)
	if asked := f.s.reaskStep(ctx(), time.Now().Add(48*time.Hour)); len(asked) != 0 {
		t.Fatalf("asked past the cap: %v", asked)
	}
	// The loop neither files a To do nor removes it: the pid is not the hub's to stop.
	f.s.sweepLoopStep(ctx(), time.Now())
	if all, _ := sweepItemsOf(f.s); len(all) != 0 {
		t.Fatalf("a To do was filed: %+v", all)
	}
	if e, _ := f.s.ledger.Get(f.e.ID); e.Status != ledger.ReadyToRemove {
		t.Fatalf("the session was %s", e.Status)
	}
	if remoteHas(t, f.r, "w") == false {
		t.Fatal("the branch was touched")
	}
}
