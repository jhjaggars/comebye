package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// What kept finished sessions in the removals review although nothing of theirs
// was left (2026-11-27), one rule each.

// markAsk marks the live worker of workerFix with ask.
func markAsk(t *testing.T, s *Server, r landedRepo, ask ledger.Ask) {
	t.Helper()
	if _, err := s.ledger.SetMark(ledger.Mark{AgentID: arAgent, Host: "desk", PaneID: "w2:p1", Name: "home", Worktree: r.wt,
		By: "desk/w1:p1", Reason: "landed", At: time.Now().UTC().Add(-time.Hour), Ask: &ask}); err != nil {
		t.Fatal(err)
	}
}

// An open to-do (the user's follow-up, filed by the session and waiting on it)
// and a follow-up note do not keep a landed, clean session: the sweep settles
// its answer, the automatic removal removes it, and the removal records both.
func TestOpenTodoAndFollowupDoNotHoldTheSession(t *testing.T) {
	s, r, e := workerFix(t)
	r.landCherryPick(t)
	td, _, err := s.todos.add(Todo{Question: "check it on a real iPhone", Kind: "followup", State: TodoOpen, CreatedBy: "home",
		Waiting: &TodoAgent{ID: arAgent, Name: "home"}})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-10 * time.Minute)
	markAsk(t, s, r, ledger.Ask{State: ledger.AskUnsafe, Reason: "left", At: at, AskedAt: &at, Items: []ledger.AskItem{
		{Type: ledger.ItemTodo, ID: td.ID, Text: "check on a real iPhone after the release; waiting on the user"},
		{Type: ledger.ItemFollowup, Text: "post the summary to the ticket (only the user can)"},
	}})
	it := reviewOf(t, s, arAgent)
	if it.AskReview == nil || !it.AskReview.Resolved {
		t.Fatalf("review = %+v warnings %v", it.AskReview, it.Warnings)
	}
	if n, _ := s.leftovers(ctx(), time.Now(), true); n != 1 {
		t.Fatalf("settled %d", n)
	}
	a := entryOf(t, s).Mark.Ask
	if a.State != ledger.AskSafe || !hubSettled(a) || len(a.Items) != 2 {
		t.Fatalf("ask = %+v", a)
	}
	if got := s.autoRemoveStep(ctx(), time.Now()); len(got) != 1 {
		t.Fatalf("removed = %v", got)
	}
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Removed || g.AutoRemoved == nil || len(g.AutoRemoved.Followups) != 2 ||
		!strings.Contains(g.AutoRemoved.Followups[0], td.ID) || !strings.Contains(g.AutoRemoved.Followups[1], "post the summary") {
		t.Fatalf("entry = %s %+v", g.Status, g.AutoRemoved)
	}
	var audited bool
	for _, x := range s.ledger.AuditLog("") {
		audited = audited || (x.Action == "worker.auto_remove" && strings.Contains(x.Detail, "Left for you") && strings.Contains(x.Detail, "post the summary"))
	}
	if !audited {
		t.Fatalf("the follow-ups are not in the audit: %+v", s.ledger.AuditLog(""))
	}
	if ls := s.events.query(time.Time{}, []string{"autoremove"}, 0); len(ls) != 1 || !strings.Contains(ls[0].Text, "2 follow-ups it left for you are kept on the removal record") || strings.Contains(ls[0].Text, "iPhone") {
		t.Fatalf("events = %+v", ls)
	}
	// The to-do stays in the user's list.
	if got, ok := s.todos.get(td.ID); !ok || got.State != TodoOpen {
		t.Fatalf("to-do = %+v", got)
	}
}

// A decision the session waits on still keeps it: the answer goes back to it.
func TestADecisionWaitingOnTheSessionStillHoldsIt(t *testing.T) {
	s, r, _ := workerFix(t)
	r.landCherryPick(t)
	td, _, _ := s.todos.add(Todo{Question: "which cluster?", Kind: "decision", State: TodoOpen, CreatedBy: "home", Waiting: &TodoAgent{ID: arAgent, Name: "home"}})
	at := time.Now().UTC().Add(-10 * time.Minute)
	markAsk(t, s, r, ledger.Ask{State: ledger.AskUnsafe, Reason: "left", At: at, AskedAt: &at, Items: []ledger.AskItem{{Type: ledger.ItemTodo, ID: td.ID}}})
	if it := reviewOf(t, s, arAgent); it.AskReview == nil || it.AskReview.Resolved {
		t.Fatalf("a decision the session waits on resolved: %+v", it.AskReview)
	}
	if n, _ := s.leftovers(ctx(), time.Now(), true); n != 0 {
		t.Fatalf("settled %d", n)
	}
	stays(t, s, r, "decision")
}

// inputzoom (2026-11-27): the sweep turned its answer into NOTHING LEFT after
// deleting its backup branch, but the answer kept the time the session gave it,
// hours before. The automatic removal called that too old to trust, and the
// re-ask that would refresh it was spent, so the session sat there for good. A
// NOTHING LEFT the hub settled itself is not held to the age limit.
func TestSafeSessionSettledByTheSweepIsRemoved(t *testing.T) {
	s, r, e := autoFix(t)
	now := time.Now().UTC()
	old := now.Add(-17 * time.Hour)
	settled := now.Add(-2 * time.Hour)
	ask := &ledger.Ask{State: ledger.AskSafe, At: old, AskedAt: &old, Reasks: 3,
		Reason: sweepSettledPrefix + "the sweep deleted origin/w-old (was abc)",
		Loop:   &ledger.LoopNote{At: settled, Text: "the sweep deleted origin/w-old"},
		Items:  []ledger.AskItem{{Type: ledger.ItemBranch, Remote: "origin", Branch: "w-old", Done: &ledger.ItemDone{At: settled, How: "deleted origin/w-old"}}}}
	// The control: the session's own answer that old is still not trusted, and
	// with its re-asks spent it is not asked again either.
	own := *ask
	own.Loop, own.Reason = nil, "nothing left"
	setAsk(t, s, &own)
	stays(t, s, r, "an old answer of the session's own")
	if a := entryOf(t, s).Mark.Ask; a.ReaskedAt != nil {
		t.Fatalf("asked past the cap: %+v", a)
	}
	setAsk(t, s, ask)
	s.coord.mu.Lock()
	delete(s.coord.calling, "autorm/"+arAgent)
	s.coord.mu.Unlock()
	if got := s.autoRemoveStep(ctx(), time.Now()); len(got) != 1 {
		t.Fatalf("removed = %v", got)
	}
	if g, _ := s.ledger.Get(e.ID); g.Status != ledger.Removed || g.AutoRemoved == nil {
		t.Fatalf("entry = %s %+v", g.Status, g.AutoRemoved)
	}
	if _, err := os.Stat(r.wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
}

// ci-time's shape: the session's commit landed under another SHA, by a rebase
// that changed its patch, so git cherry cannot match it. Each commit paired with
// the one it was rebased as (rebasedOnto), and no merge on the branch carrying
// work of its own, it counts as in main: the sweep marks it and the removal's
// proof holds. A merge with a change of its own keeps it.
func TestCommitLandedUnderAnotherSHA(t *testing.T) {
	stubOpenPRs(t, nil, nil)
	r := backedUp(t)
	old := git(t, r.wt, "rev-parse", "HEAD")
	landed := landRebased(t, r)
	git(t, r.wt, "reset", "-q", "--hard", old) // the session's worktree keeps the pre-rebase commit
	c := checkWorktree(ctx(), r.wt, "w", "main")
	if c.Unmerged != 1 || !strings.Contains(c.Equivalent, "rebased onto") || !strings.Contains(c.Equivalent, short(old)+" was rebased as "+short(landed)) {
		t.Fatalf("checks = unmerged %d, equivalent %q", c.Unmerged, c.Equivalent)
	}
	if ok, why := c.Removable(); !ok {
		t.Fatalf("not removable: %s", why)
	}
	if class, reason := sweepClassify(ledger.Entry{Status: ledger.Finished}, nil, &c, ""); class != SweepReady || !strings.Contains(reason, "rebased onto") {
		t.Fatalf("sweep = %s %q", class, reason)
	}
	checkOrigin(ctx(), &c)
	if o := c.Origin; o == nil || o.Unmerged != 1 || !strings.Contains(o.Rebased, "rebased onto origin/main") {
		t.Fatalf("origin = %+v", c.Origin)
	}
	c.Detail = &worktreeDetail{Unpushed: 0}
	if ok, why := workProven(&removal{EntryID: "e", Worktree: r.wt, Checks: &c}); !ok {
		t.Fatalf("not proven: %s", why)
	}
	if how := inBaseHow(&c); !strings.Contains(how, "rebased onto") {
		t.Fatalf("how = %q", how)
	}

	// A plain merge of main into the branch carries nothing of its own: still in.
	git(t, r.wt, "merge", "-q", "--no-edit", "origin/main")
	if c := checkWorktree(ctx(), r.wt, "w", "main"); !strings.Contains(c.Equivalent, "rebased onto") {
		t.Fatalf("clean merge: equivalent %q (unmerged %d)", c.Equivalent, c.Unmerged)
	}
	// A merge with a change slipped in (a hand-resolved conflict) is work: kept.
	os.WriteFile(r.wt+"/extra.txt", []byte("resolved by hand\n"), 0o644)
	git(t, r.wt, "add", "extra.txt")
	git(t, r.wt, "commit", "-q", "--amend", "--no-edit")
	c = checkWorktree(ctx(), r.wt, "w", "main")
	if c.Equivalent != "" {
		t.Fatalf("an evil merge counted as landed: %q", c.Equivalent)
	}
	if class, _ := sweepClassify(ledger.Entry{Status: ledger.Finished}, nil, &c, ""); class == SweepReady {
		t.Fatal("an evil merge was marked ready")
	}
	checkOrigin(ctx(), &c)
	c.Detail = &worktreeDetail{Unpushed: 1}
	if ok, _ := workProven(&removal{EntryID: "e", Worktree: r.wt, Checks: &c}); ok || c.Origin.Rebased != "" {
		t.Fatalf("an evil merge is proven: %+v", c.Origin)
	}
}

// A sweep with a stopped host and a hanging one does not wait for them: each
// is reported as "host unreachable" within the short per-host wait, the others
// on the same host are not asked again, and nothing is marked.
func TestSweepSkipsAnUnreachableHost(t *testing.T) {
	var calls atomic.Int32
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done() // accepts, never answers
	}))
	defer hang.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL, _ := url.Parse(closed.URL)
	closed.Close()
	hangURL, _ := url.Parse(hang.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "hang", URLs: []*url.URL{hangURL}}, {Name: "stopped", URLs: []*url.URL{closedURL}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := newServer(t, Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir(), HostName: "desk"}, nil, nil)
	old := sweepHostWait
	sweepHostWait = 300 * time.Millisecond
	t.Cleanup(func() { sweepHostWait = old })
	for _, w := range []struct{ host, id string }{{"hang", "hang/w1:p1"}, {"hang", "hang/w2:p1"}, {"hang", "hang/w3:p1"}, {"stopped", "stopped/w1:p1"}} {
		if _, err := s.ledger.Add(ledger.Entry{Host: w.host, Repo: "/r", RepoName: "proj", Branch: "b-" + w.id[len(w.id)-4:], AgentID: w.id, PaneID: w.id[len(w.host)+1:], AgentName: w.id, Worktree: "/wt/" + w.id, Status: ledger.Finished}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	res := s.sweep(context.Background(), sweepBody{DryRun: true}, "", "me")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the sweep took %s", d)
	}
	if len(res.NeedsLook) != 4 || len(res.Ready) != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, it := range res.NeedsLook {
		if !strings.HasPrefix(it.Reason, hostUnreachable) || it.Marked {
			t.Errorf("%s: %q marked %v", it.ID, it.Reason, it.Marked)
		}
	}
	if n := calls.Load(); n < 1 || n > 3 {
		t.Fatalf("the hanging host was asked %d times", n)
	}
	if e, _ := s.ledger.ByAgent("stopped/w1:p1"); e.Status != ledger.Finished {
		t.Fatalf("marked: %s", e.Status)
	}
}
