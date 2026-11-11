package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

func digestServer(t *testing.T) (*Server, *fakeLaunchHerdr, herdr.AgentInfo) {
	t.Helper()
	s, fh, _ := coordServer(t)
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	s.digestEvery = 15 * time.Minute
	s.digests = &digestStore{}
	s.outbox = openOutbox("")
	return s, fh, cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
}

// A finished worker with no decisions is a routine line: nothing is sent
// until the digest is due, and then one notice carries every line.
func TestRoutineNoticesAreBatchedIntoADigest(t *testing.T) {
	s, fh, coord := digestServer(t)
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, coord)
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	if g, _ := s.ledger.Get(e.ID); g.Callback.To != "digest" {
		t.Fatalf("callback = %+v", g.Callback)
	}
	if n := promptCount(fh); n != 0 {
		t.Fatalf("a routine notice went out at once: %d", n)
	}
	// A second line, a landed one; the first line's age makes it due.
	s.queueLine("landed", "h2", "h2", "handoff h2 landed")
	s.flushOutbox([]herdr.AgentInfo{coord})
	time.Sleep(100 * time.Millisecond)
	if n := promptCount(fh); n != 0 {
		t.Fatalf("digest sent before it was due: %d", n)
	}
	s.outbox.mu.Lock()
	for i := range s.outbox.data.Lines {
		s.outbox.data.Lines[i].At = time.Now().Add(-16 * time.Minute)
	}
	s.outbox.mu.Unlock()
	s.flushOutbox([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { return promptCount(fh) == 1 })
	text, _ := fh.call("agent.prompt").Params["text"].(string)
	if !strings.Contains(text, "digest") || !strings.Contains(text, "finished with no decisions") || !strings.Contains(text, "handoff h2 landed") || strings.Contains(text, "IGNORE") {
		t.Fatalf("digest = %q", text)
	}
	waitFor(t, func() bool { return len(s.digests.all()) == 1 && len(s.outbox.pending()) == 0 })
	if d := s.digests.all()[0]; len(d.Lines) != 2 {
		t.Fatalf("history = %+v", d)
	}
}

func TestUrgentNoticesStayImmediate(t *testing.T) {
	s, fh, coord := digestServer(t)
	s.latest = func(context.Context, string, string) *transcript.Latest {
		return &transcript.Latest{Text: "## Summary\nx\n## Decisions for the user\n1. Ship it?\n   1. Yes\n   2. No\n   Recommend: 1\n"}
	}
	e, _ := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "feat/x", AgentID: "desk/w9:p1", PaneID: "w9:p1", Status: ledger.Running, ReplyTo: &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}})
	w := cbAgent("w9:p1", "feat-x", herdr.StatusWorking)
	s.observeLedger([]herdr.AgentInfo{w, coord})
	w.Status = herdr.StatusIdle
	s.observeLedger([]herdr.AgentInfo{w, coord})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.FinalReport != "" })
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { return promptCount(fh) == 1 })
	if len(s.outbox.pending()) != 0 {
		t.Fatalf("an urgent notice was queued: %+v", s.outbox.pending())
	}
}

func TestRoutineLineClassification(t *testing.T) {
	base := ledger.Entry{ID: "h1", Branch: "b", RepoName: "r", Host: "desk"}
	landed := base
	landed.Status, landed.LandCheck = ledger.Landed, &ledger.LandCheck{OK: true, Base: "origin/main", CI: &ledger.CI{State: ledger.CISuccess}}
	if _, _, l, ok := routineLine(landed); !ok || !strings.Contains(l, "CI is green") {
		t.Fatalf("landed: %v %q", ok, l)
	}
	failedCI := landed
	failedCI.LandCheck = &ledger.LandCheck{OK: true, CI: &ledger.CI{State: ledger.CIFailure}}
	if _, _, _, ok := routineLine(failedCI); ok {
		t.Fatal("landed with failed CI is urgent")
	}
	lf := base
	lf.Status, lf.LandCheck = ledger.Finished, &ledger.LandCheck{State: ledger.LandFailed}
	lf.FinalReport = "## Summary\nx"
	if _, _, _, ok := routineLine(lf); ok {
		t.Fatal("a landing failure is urgent")
	}
	fin := base
	fin.Status, fin.FinalReport = ledger.Finished, "## Summary\nx\n## Decisions for the user\nNone.\n"
	if _, _, _, ok := routineLine(fin); !ok {
		t.Fatal("finished with no decisions is routine")
	}
	fin.FinalReport = "## Summary\nx\n## Decisions for the user\n1. A?\n"
	if _, _, _, ok := routineLine(fin); ok {
		t.Fatal("a report with decisions is urgent")
	}
}

func TestOutboxRepeatsAndCleanupSupersede(t *testing.T) {
	o := openOutbox("")
	o.add(outLine{Key: "landed/h1", Kind: "landed", Entry: "h1", Text: "landed", At: time.Now()})
	o.add(outLine{Key: "landed/h1", Kind: "landed", Entry: "h1", Text: "landed (CI is green)", At: time.Now()})
	if p := o.pending(); len(p) != 1 || !strings.Contains(p[0].Text, "green") {
		t.Fatalf("repeat should update: %+v", p)
	}
	o.add(outLine{Key: "cleanup/h1", Kind: "cleanup", Entry: "h1", Text: "cleaned", At: time.Now()})
	if p := o.pending(); len(p) != 1 || p[0].Kind != "cleanup" {
		t.Fatalf("cleanup should replace landed: %+v", p)
	}
	o.settle(o.pending())
	if o.add(outLine{Key: "cleanup/h1", Kind: "cleanup", Entry: "h1", Text: "again"}) || len(o.pending()) != 0 {
		t.Fatal("a sent line must not be queued again")
	}
}
