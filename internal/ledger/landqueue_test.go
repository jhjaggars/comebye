package ledger

import (
	"path/filepath"
	"testing"
	"time"
)

// queued adds a finished worker for repo+base and starts its landing.
func queued(t *testing.T, s *Store, id, repo, base string) Entry {
	t.Helper()
	e, err := s.Add(Entry{Host: "desk", Repo: repo, RepoName: filepath.Base(repo), Branch: id, Base: base, AgentID: id, AgentName: id, Status: Running, ReplyTo: &Ref{AgentID: "desk/w1:p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Report(id, "## Summary\nwork", time.Now().Add(-time.Hour)); !ok {
		t.Fatal("report refused")
	}
	got, ok, err := s.StartLanding(id, Approval{By: "c", Note: "ok", Base: base})
	if !ok || err != nil {
		t.Fatalf("start %s = %v %v", id, ok, err)
	}
	_ = e
	return got
}

func order(v QueueView) []string {
	var out []string
	for _, it := range v.Items {
		s := it.AgentID
		if it.Turn {
			s += "*"
		}
		out = append(out, s)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestQueueEnqueueOrderAndHandOff(t *testing.T) {
	s, _ := newStore(t)
	a := queued(t, s, "desk/a:p1", "/r/proj", "main")
	if !a.Approval.Queued || a.Approval.TurnAt == nil {
		t.Fatalf("the first landing in an empty queue is up at once: %+v", a.Approval)
	}
	b := queued(t, s, "desk/b:p1", "/r/proj", "main")
	c := queued(t, s, "desk/c:p1", "/r/proj", "main")
	if b.Approval.TurnAt != nil || c.Approval.TurnAt != nil {
		t.Fatal("later landings wait")
	}
	v, pos := s.QueueOf("desk/c:p1")
	if pos != 3 || !eq(order(v), []string{"desk/a:p1*", "desk/b:p1", "desk/c:p1"}) {
		t.Fatalf("queue = %v pos %d", order(v), pos)
	}
	// Nothing changes while the holder lands.
	if ch, _ := s.AdvanceQueues(time.Now(), time.Hour); len(ch.Granted)+len(ch.Ended) != 0 {
		t.Fatalf("advance with a holder = %+v", ch)
	}
	// The holder lands: the hub sees the commits on the base.
	if _, err := s.SetLandCheck(a.ID, a.FinishedAt, LandCheck{OK: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ch, err := s.AdvanceQueues(time.Now(), time.Hour)
	if err != nil || len(ch.Granted) != 1 || ch.Granted[0].AgentID != "desk/b:p1" {
		t.Fatalf("hand-off = %+v %v", ch, err)
	}
	if v, _ := s.QueueOf("desk/c:p1"); !eq(order(v), []string{"desk/b:p1*", "desk/c:p1"}) {
		t.Fatalf("after hand-off = %v", order(v))
	}
	// A retry of `comebye land` keeps the place.
	if r, ok, _ := s.StartLanding("desk/c:p1", Approval{By: "c", Note: "again", Base: "main"}); !ok || r.Approval.QueueSeq != c.Approval.QueueSeq {
		t.Fatalf("retry lost its place: %+v", r.Approval)
	}
}

func TestQueueReleasedOnFailureAndCancel(t *testing.T) {
	s, _ := newStore(t)
	a := queued(t, s, "desk/a:p1", "/r/proj", "main")
	queued(t, s, "desk/b:p1", "/r/proj", "main")
	queued(t, s, "desk/c:p1", "/r/proj", "main")
	// a's landing failed: the check sends it back to finished.
	if _, err := s.SetLandCheck(a.ID, a.FinishedAt, LandCheck{State: LandFailed, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.AdvanceQueues(time.Now(), time.Hour)
	if len(ch.Granted) != 1 || ch.Granted[0].AgentID != "desk/b:p1" {
		t.Fatalf("after failure = %+v", ch)
	}
	// b's landing is cancelled (its prompt could not be sent): c is next.
	if ok, _ := s.CancelLanding("desk/b:p1"); !ok {
		t.Fatal("cancel refused")
	}
	ch, _ = s.AdvanceQueues(time.Now(), time.Hour)
	if len(ch.Granted) != 1 || ch.Granted[0].AgentID != "desk/c:p1" {
		t.Fatalf("after cancel = %+v", ch)
	}
}

func TestQueueTurnTimeout(t *testing.T) {
	s, _ := newStore(t)
	queued(t, s, "desk/a:p1", "/r/proj", "main")
	queued(t, s, "desk/b:p1", "/r/proj", "main")
	now := time.Now()
	if ch, _ := s.AdvanceQueues(now.Add(44*time.Minute), 45*time.Minute); len(ch.Ended)+len(ch.Granted) != 0 {
		t.Fatalf("too early: %+v", ch)
	}
	ch, _ := s.AdvanceQueues(now.Add(46*time.Minute), 45*time.Minute)
	if len(ch.Ended) != 1 || ch.Ended[0].AgentID != "desk/a:p1" || ch.Ended[0].ReplyTo == nil || len(ch.Granted) != 1 || ch.Granted[0].AgentID != "desk/b:p1" {
		t.Fatalf("timeout = %+v", ch)
	}
	a, _ := s.ByAgent("desk/a:p1")
	if a.Status != Landing || a.Approval.Queued || a.Approval.QueueEnd == "" {
		t.Fatalf("a keeps its landing, out of the queue: %+v", a.Approval)
	}
	if _, pos := s.QueueOf("desk/a:p1"); pos != 0 {
		t.Fatal("a timed-out landing is not in the queue")
	}
	// Timeout 0 never releases.
	if ch, _ := s.AdvanceQueues(now.Add(1000*time.Hour), 0); len(ch.Ended) != 0 {
		t.Fatalf("timeout 0 ended %+v", ch)
	}
}

func TestQueueIsolatedPerRepoAndBase(t *testing.T) {
	s, _ := newStore(t)
	a := queued(t, s, "desk/a:p1", "/r/proj", "main")
	b := queued(t, s, "desk/b:p1", "/r/other", "main")
	c := queued(t, s, "desk/c:p1", "/r/proj", "release")
	d := queued(t, s, "desk/d:p1", "/r/proj", "main")
	for _, e := range []Entry{a, b, c} {
		if e.Approval.TurnAt == nil {
			t.Fatalf("%s should be up: its repo+base queue is empty", e.AgentID)
		}
	}
	if d.Approval.TurnAt != nil {
		t.Fatal("d waits behind a")
	}
	if qs := s.Queues(); len(qs) != 3 {
		t.Fatalf("queues = %+v", qs)
	}
}

func TestQueueMoveAndSkip(t *testing.T) {
	s, _ := newStore(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		queued(t, s, "desk/"+id+":p1", "/r/proj", "main")
	}
	// Move d to the front: the holder keeps it, so d goes second.
	v, ok, _ := s.MoveInQueue("desk/d:p1", 1)
	if !ok || !eq(order(v), []string{"desk/a:p1*", "desk/d:p1", "desk/b:p1", "desk/c:p1"}) {
		t.Fatalf("move = %v %v", order(v), ok)
	}
	v, _, _ = s.MoveInQueue("desk/d:p1", 4)
	if !eq(order(v), []string{"desk/a:p1*", "desk/b:p1", "desk/c:p1", "desk/d:p1"}) {
		t.Fatalf("move back = %v", order(v))
	}
	if _, ok, _ := s.MoveInQueue("desk/a:p1", 3); ok {
		t.Fatal("the holder is skipped, not moved")
	}
	// Skip the holder: the turn passes, a goes last.
	had, next, ok, _ := s.SkipInQueue("desk/a:p1", time.Now())
	if !ok || !had || next == nil || next.AgentID != "desk/b:p1" {
		t.Fatalf("skip = %v %v %v", had, next, ok)
	}
	v, _ = s.QueueOf("desk/a:p1")
	if !eq(order(v), []string{"desk/b:p1*", "desk/c:p1", "desk/d:p1", "desk/a:p1"}) {
		t.Fatalf("after skip = %v", order(v))
	}
	// Skipping a waiter only moves it back.
	had, next, _, _ = s.SkipInQueue("desk/c:p1", time.Now())
	if had || next != nil {
		t.Fatalf("waiter skip = %v %v", had, next)
	}
	// Alone in a queue, a skipped holder simply holds the turn again.
	s2, _ := newStore(t)
	queued(t, s2, "desk/z:p1", "/r/p", "main")
	if _, next, ok, _ := s2.SkipInQueue("desk/z:p1", time.Now()); !ok || next != nil {
		t.Fatal("lone skip")
	}
	if v, _ := s2.QueueOf("desk/z:p1"); !eq(order(v), []string{"desk/z:p1*"}) {
		t.Fatalf("lone = %v", order(v))
	}
}

func TestQueueSurvivesRestart(t *testing.T) {
	s, path := newStore(t)
	queued(t, s, "desk/a:p1", "/r/proj", "main")
	queued(t, s, "desk/b:p1", "/r/proj", "main")
	queued(t, s, "desk/c:p1", "/r/proj", "main")
	s.MoveInQueue("desk/c:p1", 2)
	want := []string{"desk/a:p1*", "desk/c:p1", "desk/b:p1"}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, pos := s2.QueueOf("desk/b:p1")
	if pos != 3 || !eq(order(v), want) {
		t.Fatalf("after restart = %v pos %d", order(v), pos)
	}
	// And the hand-off still works from the reloaded state.
	a, _ := s2.ByAgent("desk/a:p1")
	s2.SetLandCheck(a.ID, a.FinishedAt, LandCheck{OK: true, At: time.Now()})
	ch, _ := s2.AdvanceQueues(time.Now(), time.Hour)
	if len(ch.Granted) != 1 || ch.Granted[0].AgentID != "desk/c:p1" {
		t.Fatalf("hand-off after restart = %+v", ch)
	}
}
