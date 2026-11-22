package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// goneWorktree removes the worker's worktree from under it (its branch stays),
// as when a session cleaned up after itself.
func goneWorktree(t *testing.T, r landedRepo) {
	t.Helper()
	git(t, r.repo, "worktree", "remove", "--force", r.wt)
}

// steps runs the automatic removal n times, each past autoRemoveEvery.
func steps(s *Server, n int) (removed []string) {
	now := time.Now()
	for i := 0; i < n; i++ {
		s.asking.Wait()
		removed = append(removed, s.autoRemoveStep(context.Background(), now.Add(time.Duration(i)*(autoRemoveEvery+time.Second)))...)
	}
	s.asking.Wait()
	return removed
}

// A SAFE session whose worktree is gone, and whose commits are all in
// origin/main, is removed: nothing is left on disk, and the gone check proves
// the work landed (the same rule the review uses for its note).
func TestAutoRemoveGoneWorktreeLanded(t *testing.T) {
	s, r, e := autoFix(t)
	// Only origin/main has the landing: the checkout's own main is stale.
	git(t, r.repo, "reset", "-q", "--hard", r.base)
	goneWorktree(t, r)
	got := s.autoRemoveStep(context.Background(), time.Now())
	if len(got) != 1 || got[0] != arAgent {
		t.Fatalf("removed = %v (entry %+v)", got, entryOf(t, s).Mark)
	}
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Removed || g.AutoRemoved == nil || g.AutoRemoved.Kept != "" ||
		!strings.Contains(g.AutoRemoved.Landed, "already gone") || !strings.Contains(g.AutoRemoved.Landed, "origin/main") {
		t.Fatalf("entry = %s %+v", g.Status, g.AutoRemoved)
	}
	ls := s.events.query(time.Time{}, []string{"autoremove"}, 0)
	if len(ls) != 1 || ls[0].Text != "removed home automatically: landed, clean, SAFE." {
		t.Fatalf("events = %+v", ls)
	}
}

// The commits being only in the checkout's own main is not enough.
func TestAutoRemoveGoneWorktreeOnlyInLocalMain(t *testing.T) {
	s, r, _ := workerFix(t)
	git(t, r.repo, "cherry-pick", "w") // landed locally, never pushed
	markSafe(t, s, r)
	goneWorktree(t, r)
	it := s.marked()[0]
	it.opts.Origin = true
	s.review(context.Background(), it)
	if ok, why := autoRemoveVerdict(it, s.autoFactsOf(it)); ok || !strings.Contains(why, "origin") {
		t.Fatalf("verdict = %v %q (checks %+v)", ok, why, it.Checks)
	}
	if got := steps(s, 2); len(got) != 0 {
		t.Fatalf("removed = %v", got)
	}
}

// A SAFE session whose worktree is gone with a commit that is not in origin is
// kept for the user, and its old SAFE answer is not asked again: a fresh
// answer cannot prove what the host's checks do not.
func TestAutoRemoveGoneWorktreeUnmergedKeptWithoutReasks(t *testing.T) {
	s, r, _ := workerFix(t) // the branch's commit is in neither main
	markSafe(t, s, r)
	goneWorktree(t, r)
	old := time.Now().UTC().Add(-2 * time.Hour)
	setAsk(t, s, &ledger.Ask{State: ledger.AskSafe, At: old})
	if got := steps(s, 6); len(got) != 0 {
		t.Fatalf("removed = %v", got)
	}
	e := entryOf(t, s)
	if e.Status != ledger.ReadyToRemove {
		t.Fatalf("status = %s", e.Status)
	}
	if a := e.Mark.Ask; a.Reasks != 0 || a.ReaskedAt != nil || a.AskedAt != nil {
		t.Fatalf("asked again: %+v", a)
	}
	// The review warns, as before.
	w := do(s, "GET", "/api/removals", userHdr, "")
	var rv struct{ Removals []removal }
	json.Unmarshal(w.Body.Bytes(), &rv)
	if len(rv.Removals) != 1 || !strings.Contains(strings.Join(rv.Removals[0].Warnings, "|"), "not in") {
		t.Fatalf("review = %s", w.Body)
	}
}

// An old SAFE answer is asked again only while the hub's re-asks are not used
// up: never more than reaskMax times.
func TestAutoRemoveTTLReaskRespectsTheCap(t *testing.T) {
	s, r, _ := autoFix(t)
	// Landed and clean: only the age of the answer stands in the way, but
	// the re-asks are used up.
	old := time.Now().UTC().Add(-2 * time.Hour)
	setAsk(t, s, &ledger.Ask{State: ledger.AskSafe, At: old, Reasks: s.reaskMax})
	if got := steps(s, 3); len(got) != 0 {
		t.Fatalf("removed on an old answer: %v", got)
	}
	if a := entryOf(t, s).Mark.Ask; a.Reasks != s.reaskMax || a.ReaskedAt != nil || a.AskedAt != nil {
		t.Fatalf("asked past the cap: %+v", a)
	}
	stays(t, s, r, "re-asks used up")
	// One left: asked once, counted.
	setAsk(t, s, &ledger.Ask{State: ledger.AskSafe, At: old, Reasks: s.reaskMax - 1})
	s.autoRemoveStep(context.Background(), time.Now().Add(time.Hour))
	if a := entryOf(t, s).Mark.Ask; a.Reasks != s.reaskMax {
		t.Fatalf("reasks = %d, want %d", a.Reasks, s.reaskMax)
	}
}

// The verdict's re-ask rule, on its own: only the age of the answer may stand
// in the way.
func TestTTLReaskDue(t *testing.T) {
	s, _, _ := coordServer(t)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	proven := &worktreeChecks{Exists: true, Unmerged: 0, Origin: &originCheck{Ref: "origin/main", Fetched: true, Records: true}, Detail: &worktreeDetail{}}
	item := func(c *worktreeChecks, errText string, reasks int) *removal {
		return &removal{EntryID: "e", Worktree: "/w", Checks: c, ChecksError: errText, Live: liveInfo{Present: true},
			Mark: ledger.Mark{Ask: &ledger.Ask{State: ledger.AskSafe, At: old, Reasks: reasks}}}
	}
	f := autoFacts{AnswerTTL: time.Hour, Now: now}
	for _, tc := range []struct {
		name string
		it   *removal
		want bool
	}{
		{"only the age", item(proven, "", 0), true},
		{"cap spent", item(proven, "", s.reaskMax), false},
		{"host unreachable", item(nil, "the host could not be reached", 0), false},
		{"gone, unmerged", item(&worktreeChecks{Gone: true, Unmerged: 1, Records: []string{"branch w"}, BaseUsed: "main"}, "", 0), false},
		{"gone, nothing on record to check", item(&worktreeChecks{Unmerged: -1}, "", 0), false},
	} {
		if got := s.ttlReaskDue(tc.it, f); got != tc.want {
			t.Errorf("%s: due = %v, want %v", tc.name, got, tc.want)
		}
	}
	young := item(proven, "", 0)
	young.Mark.Ask.At = now
	if s.ttlReaskDue(young, f) {
		t.Error("a young answer is not due")
	}
}

// Opening the review asks a stale UNSAFE answer again only while the hub's
// re-asks are not used up, and counts it.
func TestReviewReaskRespectsTheCap(t *testing.T) {
	s, fh := askServer(t, "SAFE")
	other := ledger.AskItem{Type: "other", Text: "remote branch research/x still exists"}
	m := markUnsafe(t, s, 2*time.Hour, other)
	s.ledger.UpdateAsk(m.AgentID, m.At, func(a *ledger.Ask) bool { a.Reasks = s.reaskMax; return true })
	for i := 0; i < 3; i++ {
		do(s, "GET", "/api/removals", userHdr, "")
	}
	if promptCount(fh) != 0 {
		t.Fatalf("asked past the cap (%d prompts)", promptCount(fh))
	}
	m = markUnsafe(t, s, 2*time.Hour, other)
	do(s, "GET", "/api/removals", userHdr, "")
	waitFor(t, func() bool { a := markOf(t, s, m.AgentID).Ask; return a.ReaskedAt == nil && a.State == ledger.AskSafe })
	if a := markOf(t, s, m.AgentID).Ask; promptCount(fh) != 1 || a.Reasks != 1 {
		t.Fatalf("prompts = %d, reasks = %d", promptCount(fh), a.Reasks)
	}
}

// GET /api/removals?dry=1 shows what the automatic removal would decide and
// asks nobody again.
func TestRemovalsDryShowsVerdictsWithoutAsking(t *testing.T) {
	s, fh := askServer(t, "SAFE")
	other := ledger.AskItem{Type: "other", Text: "remote branch research/x still exists"}
	markUnsafe(t, s, 2*time.Hour, other)
	w := do(s, "GET", "/api/removals?dry=1", userHdr, "")
	var rv struct{ Removals []removal }
	if json.Unmarshal(w.Body.Bytes(), &rv); len(rv.Removals) != 1 || rv.Removals[0].AutoVerdict == nil {
		t.Fatalf("review = %s", w.Body)
	}
	if v := rv.Removals[0].AutoVerdict; v.OK || v.Why == "" {
		t.Fatalf("verdict = %+v", v)
	}
	if promptCount(fh) != 0 || markOf(t, s, "desk/w2:p1").Ask.ReaskedAt != nil {
		t.Fatalf("a dry review asked again (%d prompts)", promptCount(fh))
	}
}
