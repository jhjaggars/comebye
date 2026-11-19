package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The reproduction: a finish was announced to the requester; the same
// finish comes up again for the coordinator as a stand-in (the requester
// is gone), with other words and another target, so the old per-agent,
// per-text dedup let it through. The coordinator got two notices.
func TestFinishIsNotAnnouncedTwiceViaStandIn(t *testing.T) {
	s, fh, _ := coordServer(t)
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	spawner := cbAgent("w5:p1", "spawner", herdr.StatusIdle)
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w5:p1", Name: "spawner"}, coord, spawner)

	s.deliverCallbacks([]herdr.AgentInfo{coord, spawner})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	if n := promptCount(fh); n != 1 {
		t.Fatalf("prompts = %d", n)
	}

	// The same finish is queued again (same report round), the requester is
	// gone and the fallback wait is over: the coordinator is the target.
	s.ledger.Update(e.ID, func(en *ledger.Entry) {
		en.Callback = &ledger.Callback{State: ledger.CallbackPending, At: time.Now().Add(-callbackFallback - time.Minute), Seq: en.ReportSeq}
	})
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	time.Sleep(50 * time.Millisecond)
	if n := promptCount(fh); n != 1 {
		t.Fatalf("the finish reached an agent %d times, want once", n)
	}
}

// A send that failed releases its claim: the retry goes out.
func TestSentClaimReleasedAfterFailedSend(t *testing.T) {
	s, fh, _ := coordServer(t)
	idle := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, idle)
	fh.set(func(f *fakeLaunchHerdr) {
		f.hook = func(m string, _ map[string]any) (any, *herdr.Error, bool) {
			if m == "agent.prompt" {
				return nil, &herdr.Error{Code: "boom", Message: "write failed"}, true
			}
			return nil, nil, false
		}
	})
	s.deliverCallbacks([]herdr.AgentInfo{idle})
	waitFor(t, func() bool { return fh.call("agent.prompt") != nil })
	waitFor(t, func() bool { return !s.sentHas(finishSubjectOf(t, s, e.ID)) })
	if g, _ := s.ledger.Get(e.ID); g.Callback.State != ledger.CallbackPending {
		t.Fatalf("callback = %+v", g.Callback)
	}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = nil })
	s.coord.mu.Lock()
	delete(s.coord.calling, e.ID) // the retry spacing is over
	s.coord.mu.Unlock()
	s.deliverCallbacks([]herdr.AgentInfo{idle})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
}

func finishSubjectOf(t *testing.T, s *Server, id string) string {
	g, _ := s.ledger.Get(id)
	return finishSubject(g)
}

func (s *Server) sentHas(subject string) bool { _, ok := s.sent.get(subject); return ok }

func TestSentSurvivesRestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sent.json")
	a := openSent(p)
	if !a.claim("x/1", "desk/w1:p1") {
		t.Fatal("first claim refused")
	}
	a.confirm("x/1")
	b := openSent(p)
	if b.claim("x/1", "desk/w2:p1") {
		t.Fatal("claimed again after a restart")
	}
	r, ok := b.get("x/1")
	if !ok || r.To != "desk/w1:p1" || r.At.IsZero() {
		t.Fatalf("record = %+v", r)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}

func TestSentPrunesByAge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sent.json")
	a := openSent(p)
	now := time.Now()
	a.now = func() time.Time { return now.Add(-sentKeep - time.Hour) }
	a.claim("old", "t")
	a.now = func() time.Time { return now }
	a.claim("new", "t")
	if _, ok := a.get("old"); ok {
		t.Fatal("old record kept in memory")
	}
	b := openSent(p)
	if _, ok := b.get("old"); ok {
		t.Fatal("old record kept on disk")
	}
	if _, ok := b.get("new"); !ok {
		t.Fatal("new record lost")
	}
	if !b.claim("old", "t") {
		t.Fatal("a pruned subject cannot be claimed again")
	}
}

func TestSentReleaseAndNil(t *testing.T) {
	a := openSent("")
	a.claim("s", "t")
	a.release("s")
	if !a.claim("s", "t") {
		t.Fatal("released claim not free")
	}
	var n *sentStore
	if !n.claim("s", "t") {
		t.Fatal("nil store must not block")
	}
}
