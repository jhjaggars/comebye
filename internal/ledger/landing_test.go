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
	// A landing is followed from the start, report or not: the work may
	// reach the base first.
	if due := s.LandingsToCheck(); len(due) != 1 || due[0].FinishedAt != nil {
		t.Fatalf("due before the report = %+v", due)
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
	if ok, _ := s.SetLandCheck(e.ID, &now, LandCheck{OK: true}); ok {
		t.Fatal("stale check applied")
	}
	if ok, _ := s.SetLandCheck(e.ID, due[0].FinishedAt, LandCheck{OK: true, Base: "origin/main", Verdict: "clean", Landed: []string{"def5678"}}); !ok {
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
	s.SetLandCheck(e.ID, due[0].FinishedAt, LandCheck{OK: false, Verdict: "1 commits not in origin/main"})
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

// A progress note is kept for the card and does nothing else.
func TestProgressNoteFinishesNothing(t *testing.T) {
	s, e, _ := reported(t)
	s.CallbackDone(e.ID, e.Callback.At, CallbackDelivered, "desk/w1:p1", time.Now())
	s.StartLanding("desk/w9:p1", Approval{Base: "main", Tip: "abc1234"})
	now := time.Now()
	g, ok, err := s.Note("desk/w9:p1", "  tests running; I will push when they pass  ", "tests", now)
	if !ok || err != nil || g.Status != Landing || g.FinishedAt != nil || g.Progress == nil || g.Progress.Phase != "tests" || g.Progress.Text != "tests running; I will push when they pass" {
		t.Fatalf("note = %+v %v %v", g, ok, err)
	}
	if g.ReportSeq != e.ReportSeq || (g.Callback != nil && g.Callback.State == CallbackPending) {
		t.Fatalf("a note counted as a report: %+v", g)
	}
	if long, _, _ := s.Note("desk/w9:p1", string(make([]byte, 5000)), "", now); len(long.Progress.Text) > MaxProgress+20 {
		t.Fatal("a note is capped")
	}
	if _, ok, _ := s.Note("desk/nobody:p1", "x", "", now); ok {
		t.Fatal("a note from a non-worker")
	}
	if !ValidPhase("") || !ValidPhase("ci") || ValidPhase("sleep") {
		t.Fatal("phases")
	}
	// A new landing starts clean.
	s.SetLandCheck(e.ID, nil, LandCheck{State: LandPending, Verdict: "1 commits not in origin/main", Unmerged: 1})
	if g, _ := s.Get(e.ID); g.Status != Landing || g.LandCheck == nil || g.LandCheck.State != LandPending || g.Progress == nil {
		t.Fatalf("pending check changed the entry: %+v", g)
	}
	// An unchanged pending check only moves the time and reason.
	t0 := time.Now().Add(time.Minute)
	s.SetLandCheck(e.ID, nil, LandCheck{At: t0, State: LandPending, Verdict: "1 commits not in origin/main", Unmerged: 1, Reason: "the worker is working"})
	if g, _ := s.Get(e.ID); !g.LandCheck.At.Equal(t0) || g.LandCheck.Reason != "the worker is working" {
		t.Fatalf("pending refresh = %+v", g.LandCheck)
	}
}

func TestLandedBeforeReportAndFailureStates(t *testing.T) {
	s, e, _ := reported(t)
	s.CallbackDone(e.ID, e.Callback.At, CallbackDelivered, "desk/w1:p1", time.Now())
	s.StartLanding("desk/w9:p1", Approval{Base: "main", Tip: "abc1234"})
	// The hub finds the work on the base before any report.
	if ok, _ := s.SetLandCheck(e.ID, nil, LandCheck{OK: true, Base: "origin/main", Verdict: "clean"}); !ok {
		t.Fatal("not applied")
	}
	g, _ := s.Get(e.ID)
	if g.Status != Landed || !g.AwaitLandReport || g.LandCheck.State != LandLanded || g.Callback.State != CallbackPending {
		t.Fatalf("early landing = %+v", g)
	}
	// Working on the landing turn is not a new round, even long after.
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, time.Now().Add(30*time.Minute))
	if g, _ = s.Get(e.ID); g.Status != Landed {
		t.Fatalf("landing turn became %s", g.Status)
	}
	// Its report completes the turn; a later one is a new round's.
	lr, ok, _ := s.Report("desk/w9:p1", "## Summary\nlanded", time.Now())
	if !ok || lr.Status != Landed || lr.AwaitLandReport || lr.ReportSeq != g.ReportSeq {
		t.Fatalf("landing report = %+v", lr)
	}
	// After landReportWait with no report the worker is an ordinary reported one again.
	s2, e2, _ := reported(t)
	s2.StartLanding("desk/w9:p1", Approval{Base: "main", Tip: "abc1234"})
	s2.SetLandCheck(e2.ID, nil, LandCheck{OK: true, Base: "origin/main"})
	s2.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, time.Now().Add(landReportWait+time.Minute))
	if g, _ := s2.Get(e2.ID); g.AwaitLandReport {
		t.Fatal("the wait for the landing report never ends")
	}
	// Failed: a worker that stopped. Back to finished, told, progress gone.
	s3, e3, _ := reported(t)
	s3.CallbackDone(e3.ID, e3.Callback.At, CallbackDelivered, "desk/w1:p1", time.Now())
	s3.StartLanding("desk/w9:p1", Approval{Base: "main", Tip: "abc1234"})
	s3.Note("desk/w9:p1", "tests", "tests", time.Now())
	s3.SetLandCheck(e3.ID, nil, LandCheck{State: LandFailed, Verdict: "x", Reason: "the worker stopped"})
	f, _ := s3.Get(e3.ID)
	if f.Status != Finished || f.FinishedAt == nil || f.Progress != nil || f.Callback.State != CallbackPending || f.LandCheck.StateOf() != LandFailed {
		t.Fatalf("failed = %+v", f)
	}
}
