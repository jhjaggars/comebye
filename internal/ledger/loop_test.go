package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// A loop worker's pass end is "between passes": it stays running and no
// finished notice is queued for its requester (risk R3: an hourly steward
// read as finished after every pass).
func TestLoopPassEndIsNotAFinish(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	e, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, Loop: "run a steward pass", ReplyTo: &Ref{AgentID: "desk/w1:p1"}})
	now := time.Now()
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now)
	c := s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now.Add(time.Minute))
	got, _ := s.Get(e.ID)
	// The capture is the pass's last reply, kept beside the report (fresh reports).
	if got.Status != Running || got.FinishedAt != nil || got.Callback != nil || len(c) != 1 || c[0].PassCaptureAt == nil {
		t.Fatalf("pass end: status=%s finished=%v callback=%+v capture=%d; want running, no notice, the pass's reply captured", got.Status, got.FinishedAt, got.Callback, len(c))
	}
}

func TestLoopBetweenPassesAndSetLoop(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	e, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, Loop: "pass", ReplyTo: &Ref{AgentID: "desk/w1:p1"}})
	now := time.Now()
	if got, _ := s.Get(e.ID); got.BetweenPasses() {
		t.Fatal("before its first pass it is starting, not between passes")
	}
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now)
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now.Add(time.Minute))
	got, _ := s.Get(e.ID)
	if !got.BetweenPasses() || got.LastPassAt == nil {
		t.Fatalf("after a pass: %+v", got)
	}
	// The next pass: in a pass, not between.
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now.Add(time.Hour))
	if got, _ := s.Get(e.ID); got.BetweenPasses() || got.Status != Running {
		t.Fatalf("in a pass: %+v", got)
	}
	// Its own report finishes it.
	if _, _, err := s.Report("desk/w9:p1", "## Summary\ndone", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(e.ID); got.Status != Finished || got.BetweenPasses() {
		t.Fatalf("reported: %+v", got)
	}
}

// Registering a worker the ledger already read as finished by a pass end
// (the live steward) puts it back between passes and drops the pending
// finished notice; clearing the loop takes it off.
func TestSetLoopOnAFinishedPassEnd(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	e, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, ReplyTo: &Ref{AgentID: "desk/w1:p1"}})
	now := time.Now()
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now)
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now.Add(time.Minute))
	if got, _ := s.Get(e.ID); got.Status != Finished || got.Callback == nil {
		t.Fatalf("without a loop a pass end finishes: %+v", got)
	}
	got, err := s.SetLoop(e.ID, "run a steward pass", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Running || got.FinishedAt != nil || got.Callback != nil || got.LastPassAt == nil || !got.BetweenPasses() || got.LoopEveryMinutes != 60 {
		t.Fatalf("set: %+v", got)
	}
	got, err = s.SetLoop(e.ID, "", 0)
	if err != nil || got.Loop != "" || got.LoopEveryMinutes != 0 || got.BetweenPasses() {
		t.Fatalf("clear: %+v %v", got, err)
	}
	if _, err := s.SetLoop("nope", "x", 0); err == nil {
		t.Fatal("unknown id accepted")
	}
}
