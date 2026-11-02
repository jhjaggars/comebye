package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// reported is a store with one worker that reported (explicitly) an hour ago.
func reported(t *testing.T) (*Store, Entry, time.Time) {
	t.Helper()
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	e, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, ReplyTo: &Ref{AgentID: "desk/w1:p1"}})
	t0 := time.Now().Add(-time.Hour)
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, t0)
	if _, ok, _ := s.Report("desk/w9:p1", "## Summary\nwork", t0); !ok {
		t.Fatal("report refused")
	}
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, t0.Add(time.Second))
	got, _ := s.Get(e.ID)
	return s, got, t0
}

func TestLandingLifecycle(t *testing.T) {
	s, e, _ := reported(t)
	if e.Status != Finished || e.Callback == nil {
		t.Fatalf("entry = %+v", e)
	}
	s.CallbackDone(e.ID, e.Callback.At, CallbackDelivered, "desk/w1:p1", time.Now())

	l, ok, err := s.StartLanding("desk/w9:p1", Approval{By: "coordinator", Note: "user: land it", Base: "main", Tip: "abc1234"})
	if !ok || err != nil || l.Status != Landing || l.FinishedAt != nil || l.Approval == nil || l.Approval.ReportedAt == nil {
		t.Fatalf("start = %+v %v %v", l, ok, err)
	}
	if !l.Active() {
		t.Fatal("a landing is active")
	}
	// Working while landing is not a new round: the report stays.
	now := time.Now()
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now)
	if g, _ := s.Get(e.ID); g.Status != Landing || g.FinalReport == "" {
		t.Fatalf("working landing = %+v", g)
	}
	if len(s.LandingsToCheck()) != 0 {
		t.Fatal("nothing to check before the landing report")
	}
	lr, ok, _ := s.Report("desk/w9:p1", "landed as def5678", now.Add(time.Minute))
	if !ok || lr.Status != Landing || lr.FinishedAt == nil || lr.FinalReport != "landed as def5678" {
		t.Fatalf("landing report = %+v", lr)
	}
	if lr.Callback.State != CallbackDelivered {
		t.Fatal("the requester is told once the check settles, not on the report")
	}
	due := s.LandingsToCheck()
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	// A check for an older report is ignored.
	if ok, _ := s.SetLandCheck(e.ID, now, LandCheck{OK: true}); ok {
		t.Fatal("stale check applied")
	}
	if ok, _ := s.SetLandCheck(e.ID, *due[0].FinishedAt, LandCheck{OK: true, Base: "origin/main", Verdict: "clean", Landed: []string{"def5678"}}); !ok {
		t.Fatal("check not applied")
	}
	g, _ := s.Get(e.ID)
	if g.Status != Landed || g.LandCheck == nil || !g.LandCheck.OK || g.Callback == nil || g.Callback.State != CallbackPending {
		t.Fatalf("landed = %+v", g)
	}
	if len(s.PendingCallbacks()) != 1 {
		t.Fatal("a landed entry's callback is delivered")
	}
	// Mark and unmark: back to landed.
	s.SetMark(Mark{AgentID: "desk/w9:p1", Reason: "landed"})
	s.ClearMark("desk/w9:p1")
	if g, _ := s.Get(e.ID); g.Status != Landed {
		t.Fatalf("unmarked = %s", g.Status)
	}
	// More work after the grace: a new round, running, keeping the record.
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now.Add(2*time.Minute))
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now.Add(time.Hour))
	g, _ = s.Get(e.ID)
	if g.Status != Running || g.LandCheck == nil || g.Approval == nil {
		t.Fatalf("follow-up = %+v", g)
	}
	if _, ok, _ := s.StartLanding("desk/w9:p1", Approval{}); ok {
		t.Fatal("a running worker cannot be landed")
	}
}

func TestLandingCheckFails(t *testing.T) {
	s, e, _ := reported(t)
	s.StartLanding("desk/w9:p1", Approval{Base: "main"})
	now := time.Now()
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now)
	// Idle after working without `comebye report` is the landing report too.
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now.Add(time.Minute))
	due := s.LandingsToCheck()
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	s.SetLandCheck(e.ID, *due[0].FinishedAt, LandCheck{OK: false, Verdict: "1 commits not in origin/main"})
	g, _ := s.Get(e.ID)
	if g.Status != Finished || g.LandCheck.OK || g.LandCheck.Verdict == "" {
		t.Fatalf("failed landing = %+v", g)
	}
	// It may be landed again (after a fix, or a retry).
	if _, ok, _ := s.StartLanding("desk/w9:p1", Approval{}); !ok {
		t.Fatal("a failed landing cannot be retried")
	}
	// Unmarking a finished entry whose check failed keeps it finished.
}

func TestCancelAndRetryLanding(t *testing.T) {
	s, e, _ := reported(t)
	s.StartLanding("desk/w9:p1", Approval{Note: "first"})
	// Not yet started: a retry replaces the approval.
	if l, ok, _ := s.StartLanding("desk/w9:p1", Approval{Note: "second"}); !ok || l.Approval.Note != "second" || l.Approval.ReportedAt == nil {
		t.Fatalf("retry = %+v %v", l, ok)
	}
	if ok, _ := s.CancelLanding("desk/w9:p1"); !ok {
		t.Fatal("cancel refused")
	}
	g, _ := s.Get(e.ID)
	if g.Status != Finished || g.FinishedAt == nil || g.Approval != nil {
		t.Fatalf("cancelled = %+v", g)
	}
	// Once it is working, a landing is not cancelled.
	s.StartLanding("desk/w9:p1", Approval{})
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, time.Now())
	if ok, _ := s.CancelLanding("desk/w9:p1"); ok {
		t.Fatal("cancelled a landing under way")
	}
	if MemberOutcome(g) != MemberOK {
		t.Fatal("a finished member is ok")
	}
	if g, _ := s.Get(e.ID); MemberOutcome(g) != MemberOK {
		t.Fatalf("a landing member is ok: %s", g.Status)
	}
}
