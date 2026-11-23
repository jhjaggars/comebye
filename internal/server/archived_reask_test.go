package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// Slice 7 of #86: when the hub's re-asks run out and the answer still says
// LEFT with only cleanup, other or followup items, the session is archived at
// once (no N-day wait), with those items on its record.

func TestReasksSpent(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		m    *ledger.Mark
		max  int
		want bool
	}{
		{"no mark", nil, 3, false},
		{"no answer", &ledger.Mark{}, 3, false},
		{"LEFT after the third re-ask", &ledger.Mark{Ask: &ledger.Ask{State: ledger.AskUnsafe, Reasks: 3}}, 3, true},
		{"LEFT after the second", &ledger.Mark{Ask: &ledger.Ask{State: ledger.AskUnsafe, Reasks: 2}}, 3, false},
		{"SAFE after the third", &ledger.Mark{Ask: &ledger.Ask{State: ledger.AskSafe, Reasks: 3}}, 3, false},
		{"a re-ask in flight", &ledger.Mark{Ask: &ledger.Ask{State: ledger.AskUnsafe, Reasks: 3, ReaskedAt: &now}}, 3, false},
		{"cleaning up", &ledger.Mark{Ask: &ledger.Ask{State: ledger.AskUnsafe, Reasks: 3, CleaningAt: &now}}, 3, false},
		{"marked as delivered", &ledger.Mark{Discard: true, Ask: &ledger.Ask{State: ledger.AskUnsafe, Reasks: 3}}, 3, false},
		{"re-asks off", &ledger.Mark{Ask: &ledger.Ask{State: ledger.AskUnsafe}}, 0, false},
	} {
		if got := reasksSpent(tc.m, tc.max); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLeftOnlyOwed(t *testing.T) {
	v := func(typ, state string) itemView {
		return itemView{AskItem: ledger.AskItem{Type: typ, Text: typ}, State: state}
	}
	unsafe := func(items ...itemView) *askReview { return &askReview{State: ledger.AskUnsafe, Items: items} }
	scratch := func(state string) itemView {
		return itemView{AskItem: ledger.AskItem{Type: ledger.ItemPath, Name: "/tmp/jfetch", Text: "343 MB of my scratch"}, State: state}
	}
	for _, tc := range []struct {
		name  string
		ar    *askReview
		left  int
		owned bool
	}{
		{"nil", nil, 0, false},
		{"SAFE", &askReview{State: ledger.AskSafe}, 0, false},
		{"cleanup only", unsafe(v(ledger.ItemCleanup, itemUnchecked)), 1, true},
		{"cleanup, other and followup", unsafe(v(ledger.ItemCleanup, itemUnchecked), v(ledger.ItemOther, itemUnchecked), v(ledger.ItemFollowup, itemUnchecked)), 3, true},
		{"a settled pr and a cleanup", unsafe(v(ledger.ItemPR, itemResolved), v(ledger.ItemCleanup, itemUnchecked)), 1, true},
		{"an open pr", unsafe(v(ledger.ItemPR, itemOpen), v(ledger.ItemCleanup, itemUnchecked)), 0, false},
		{"an open to-do", unsafe(v(ledger.ItemTodo, itemOpen)), 0, false},
		{"a decision", unsafe(v(ledger.ItemDecision, itemOpen)), 0, false},
		{"a branch the hub checks", unsafe(v(ledger.ItemBranch, itemOpen), v(ledger.ItemOther, itemUnchecked)), 0, false},
		{"all settled: the proof removes it", &askReview{State: ledger.AskUnsafe, Resolved: true, Items: []itemView{v(ledger.ItemPR, itemResolved)}}, 0, false},
		{"its own scratch path, still there", unsafe(scratch(itemOpen), v(ledger.ItemFollowup, itemUnchecked)), 2, true},
		{"its own scratch path, can't check", unsafe(scratch(itemUnchecked)), 0, false},
		{"a path not its scratch, still there", unsafe(v(ledger.ItemPath, itemOpen)), 0, false},
		{"a path it says to keep", unsafe(itemView{AskItem: ledger.AskItem{Type: ledger.ItemPath, Name: "/tmp/x", Text: "my clone, keep it"}, State: itemOpen}), 0, false},
		{"a stale branch: the sweep deletes it", &askReview{State: ledger.AskUnsafe, StaleOnly: true, Items: []itemView{v(ledger.ItemOther, itemOpen)}}, 0, false},
	} {
		left, ok := leftOnlyOwed(tc.ar)
		if ok != tc.owned || len(left) != tc.left {
			t.Errorf("%s: %d items, %v; want %d, %v", tc.name, len(left), ok, tc.left, tc.owned)
		}
	}
}

// The age gate is skipped for reasksOut, and only for it; the other guards stand.
func TestArchiveBlockReasksOut(t *testing.T) {
	now := time.Now().UTC()
	fin := now.Add(-2 * time.Hour)
	e := ledger.Entry{Status: ledger.ReadyToRemove, Branch: "feat/x", Worktree: "/wt/x", CreatedAt: now.Add(-day), FinishedAt: &fin,
		Mark: &ledger.Mark{At: now.Add(-time.Hour), Ask: &ledger.Ask{State: ledger.AskUnsafe, At: now.Add(-time.Minute), Reasks: 3}}}
	if why := archiveBlock(e, archiveFacts{knownHost: true}, now, 7*day, 3*day); why == "" {
		t.Fatal("a fresh LEFT answer is archived without reasksOut")
	}
	out := archiveFacts{knownHost: true, reasksOut: true}
	if why := archiveBlock(e, out, now, 7*day, 3*day); why != "" {
		t.Fatalf("re-asks out: blocked: %s", why)
	}
	out.coordinator = true
	if why := archiveBlock(e, out, now, 7*day, 3*day); why == "" {
		t.Fatal("a coordinator is archived")
	}
	out.coordinator, out.openTodo = false, true
	if why := archiveBlock(e, out, now, 7*day, 3*day); why == "" {
		t.Fatal("a session with an open to-do is archived")
	}
}

// The loop, end to end: the third LEFT answer with only cleanup and followup
// items is archived on the next step, with them on Archive.Left and no to-do
// filed; one that still lists an open pr waits; one with a re-ask to go waits.
func TestArchiveStepWhenReasksRunOut(t *testing.T) {
	now := time.Now().UTC()
	mark := func(r *archiveRig, e ledger.Entry, reasks int, items ...ledger.AskItem) {
		t.Helper()
		m := ledger.Mark{AgentID: e.AgentID, Host: "desk", PaneID: e.PaneID, Name: e.AgentName, Worktree: r.wt, By: AutoCreator, Reason: "finished", At: now.Add(-2 * time.Hour)}
		if _, err := r.s.ledger.SetMark(m); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.ledger.SetAsk(e.AgentID, m.At, ledger.Ask{State: ledger.AskUnsafe, At: now.Add(-10 * time.Minute), Reason: "left", Items: items, Reasks: reasks}); err != nil {
			t.Fatal(err)
		}
	}
	fin := now.Add(-3 * time.Hour)
	worker := func(r *archiveRig) ledger.Entry {
		return r.add(t, ledger.Entry{AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "stopper", Status: ledger.Finished, FinishedAt: &fin, FinalReport: "done", Branch: "feat/stop", CreatedAt: now.Add(-day)})
	}
	cleanup := ledger.AskItem{Type: ledger.ItemCleanup, Text: "stop the dev server I started"}
	followup := ledger.AskItem{Type: ledger.ItemFollowup, Text: "the user has to run the apply"}

	t.Run("only cleanup and followup: archived at once", func(t *testing.T) {
		r := newArchiveRig(t, "feat/stop")
		e := worker(r)
		mark(r, e, 3, cleanup, followup)
		if asked := r.s.reaskStep(context.Background(), now); len(asked) != 0 {
			t.Fatalf("re-asked past the cap: %v", asked)
		}
		done := r.s.archiveStep(context.Background(), now)
		if len(done) != 1 || done[0].Kind != doneArchived {
			t.Fatalf("done = %+v", done)
		}
		got, _ := r.s.ledger.Get(e.ID)
		a := got.Archive
		if got.Status != ledger.Archived || a == nil || got.Mark != nil {
			t.Fatalf("entry = %s %+v mark=%v", got.Status, a, got.Mark)
		}
		if len(a.Left) != 2 || a.Left[0].Type != ledger.ItemCleanup || a.Left[1].Type != ledger.ItemFollowup {
			t.Fatalf("left = %+v", a.Left)
		}
		if a.Mark == nil || a.Mark.Ask == nil || a.Mark.Ask.Reasks != 3 || a.From != ledger.Finished {
			t.Fatalf("kept mark = %+v from %s", a.Mark, a.From)
		}
		if !strings.Contains(a.Why, "re-asked 3 times") || !strings.Contains(a.Why, "2 things left") {
			t.Fatalf("why = %q", a.Why)
		}
		// Out of the removals review; no To do for the user.
		for _, it := range r.s.marked() {
			if it.EntryID == e.ID {
				t.Fatal("still in the removals review")
			}
		}
		if r.s.todos != nil {
			if open := r.s.todos.list(""); len(open) != 0 {
				t.Fatalf("filed %d to-dos: %+v", len(open), open)
			}
		}
		// The row says what is left.
		rows := r.s.archivedRows()
		if rows.Count != 1 || len(rows.Archived[0].Archive.Left) != 2 {
			t.Fatalf("rows = %+v", rows)
		}
	})

	t.Run("an open pr: waits", func(t *testing.T) {
		r := newArchiveRig(t, "feat/stop")
		e := worker(r)
		mark(r, e, 3, cleanup, ledger.AskItem{Type: ledger.ItemPR, Text: "watch the PR", Number: 4})
		if done := r.s.archiveStep(context.Background(), now); len(done) != 0 {
			t.Fatalf("archived with an open pr: %+v", done)
		}
		if got, _ := r.s.ledger.Get(e.ID); got.Status != ledger.ReadyToRemove {
			t.Fatalf("status = %s", got.Status)
		}
	})

	t.Run("a re-ask still to go: waits", func(t *testing.T) {
		r := newArchiveRig(t, "feat/stop")
		e := worker(r)
		mark(r, e, 2, cleanup)
		if done := r.s.archiveStep(context.Background(), now); len(done) != 0 {
			t.Fatalf("archived with a re-ask to go: %+v", done)
		}
	})
}
