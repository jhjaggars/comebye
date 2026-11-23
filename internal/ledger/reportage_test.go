package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

const wk = "desk/w9:p1"

func newWorker(t *testing.T, e Entry) (*Store, Entry) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.AgentID, e.Status = wk, Running
	if e.ReplyTo == nil {
		e.ReplyTo = &Ref{AgentID: "desk/w1:p1"}
	}
	got, err := s.Add(e)
	if err != nil {
		t.Fatal(err)
	}
	return s, got
}

func get(t *testing.T, s *Store, id string) Entry {
	t.Helper()
	e, ok := s.Get(id)
	if !ok {
		t.Fatalf("no entry %s", id)
	}
	return e
}

func at(t *testing.T, p *time.Time, want time.Time, what string) {
	t.Helper()
	if p == nil || !p.Equal(want.UTC()) {
		t.Fatalf("%s = %v, want %v", what, p, want.UTC())
	}
}

// `comebye report` stamps the report with when it was written, every time.
func TestReportAtExplicit(t *testing.T) {
	s, e := newWorker(t, Entry{})
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if _, ok, err := s.Report(wk, "## Summary\none", t0); !ok || err != nil {
		t.Fatal(ok, err)
	}
	at(t, get(t, s, e.ID).ReportAt, t0, "report_at")
	t1 := t0.Add(time.Hour)
	s.Report(wk, "## Summary\ntwo", t1)
	got := get(t, s, e.ID)
	at(t, got.ReportAt, t1, "report_at after report 2")
	if got.ReportSeq != 2 {
		t.Fatalf("report_seq = %d", got.ReportSeq)
	}
	// A progress note is not a report: the report keeps its time.
	s.Note(wk, "rebasing", "", t1.Add(time.Minute))
	at(t, get(t, s, e.ID).ReportAt, t1, "report_at after a progress note")
}

// A report the hub takes (the worker went idle without reporting) is
// stamped with its finish; a new round clears it with the report.
func TestReportAtCaptured(t *testing.T) {
	s, e := newWorker(t, Entry{})
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, t0)
	c := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t0.Add(time.Minute))
	if len(c) != 1 || c[0].PassCaptureAt != nil {
		t.Fatalf("capture = %+v, want the finish's report", c)
	}
	if err := s.SetReport(e.ID, c[0].FinishedAt, "did it"); err != nil {
		t.Fatal(err)
	}
	got := get(t, s, e.ID)
	at(t, got.ReportAt, t0.Add(time.Minute), "report_at")
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, t0.Add(time.Hour))
	if got := get(t, s, e.ID); got.FinalReport != "" || got.ReportAt != nil {
		t.Fatalf("new round kept the guessed report: %q %v", got.FinalReport, got.ReportAt)
	}
}

// A worker that finishes again without `comebye report` keeps its report
// (what the requester was told to read), and its last reply goes beside it
// as the newer one; a new report replaces both.
func TestRefinishKeepsReportAndAddsNewerReply(t *testing.T) {
	s, e := newWorker(t, Entry{})
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, t0)
	s.Report(wk, "## Summary\nCI flaked", t0)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t0.Add(time.Minute))
	// A new round, long after the report turn.
	t1 := t0.Add(2 * time.Hour)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, t1)
	c := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t1.Add(10*time.Minute))
	if len(c) != 1 || c[0].PassCaptureAt == nil {
		t.Fatalf("re-finish capture = %+v, want the turn's reply", c)
	}
	end := *c[0].PassCaptureAt
	if err := s.SetPassReport(e.ID, end, "CI is green now"); err != nil {
		t.Fatal(err)
	}
	got := get(t, s, e.ID)
	if got.Status != Finished || got.FinalReport != "## Summary\nCI flaked" || got.ReportSource != "explicit" || got.ReportSeq != 1 {
		t.Fatalf("report replaced: %+v", got)
	}
	at(t, got.ReportAt, t0, "report_at")
	if got.LastPassReport != "CI is green now" || !got.NewerReply() {
		t.Fatalf("newer reply = %q (newer %v)", got.LastPassReport, got.NewerReply())
	}
	at(t, got.LastPassReportAt, end, "last_pass_report_at")
	// A stale capture (for an older turn) is dropped.
	if err := s.SetPassReport(e.ID, t0, "old"); err != nil || get(t, s, e.ID).LastPassReport != "CI is green now" {
		t.Fatal("stale pass capture applied")
	}
	// A new report is the newest again.
	t2 := t1.Add(time.Hour)
	s.Report(wk, "## Summary\nall green", t2)
	got = get(t, s, e.ID)
	at(t, got.ReportAt, t2, "report_at after report 2")
	if got.LastPassReport != "" || got.LastPassReportAt != nil || got.NewerReply() || got.ReportSeq != 2 {
		t.Fatalf("after a new report: %+v", got)
	}
}

// The very same report filed again after a newer reply makes the report the
// current one again, without a new round or notice.
func TestSameReportAgainAfterNewerReply(t *testing.T) {
	s, e := newWorker(t, Entry{})
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Report(wk, "R", t0)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t0.Add(time.Minute))
	t1 := t0.Add(time.Hour)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, t1)
	c := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t1.Add(time.Minute))
	s.SetPassReport(e.ID, *c[0].PassCaptureAt, "later")
	t2 := t1.Add(time.Hour)
	s.Report(wk, "R", t2)
	got := get(t, s, e.ID)
	at(t, got.ReportAt, t2, "report_at")
	if got.NewerReply() || got.ReportSeq != 1 {
		t.Fatalf("same report again: %+v", got)
	}
}

// A loop worker's pass end captures the pass's last reply beside its report;
// the next pass replaces it, an empty reply keeps the one before, and the
// report itself is never touched.
func TestLoopPassReport(t *testing.T) {
	s, e := newWorker(t, Entry{Loop: "steward pass"})
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Report(wk, "## Summary\npass 27", t0)
	pass := func(start time.Time, reply string) {
		t.Helper()
		s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, start)
		c := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, start.Add(time.Minute))
		if len(c) != 1 || c[0].PassCaptureAt == nil {
			t.Fatalf("pass at %v: capture %+v", start, c)
		}
		if err := s.SetPassReport(e.ID, *c[0].PassCaptureAt, reply); err != nil {
			t.Fatal(err)
		}
	}
	p1 := t0.Add(time.Hour)
	pass(p1, "pass 28: nothing new")
	got := get(t, s, e.ID)
	if got.Status != Running || got.FinalReport != "## Summary\npass 27" || got.LastPassReport != "pass 28: nothing new" || !got.NewerReply() {
		t.Fatalf("after pass 28: %+v", got)
	}
	at(t, got.LastPassReportAt, p1.Add(time.Minute), "last_pass_report_at")
	at(t, got.LastPassAt, p1.Add(time.Minute), "last_pass_at")
	p2 := p1.Add(time.Hour)
	pass(p2, "pass 29: filed two to-dos")
	if got := get(t, s, e.ID); got.LastPassReport != "pass 29: filed two to-dos" {
		t.Fatalf("pass 29: %q", got.LastPassReport)
	}
	// An empty reply (transcript not flushed yet) keeps pass 29's and is
	// asked for again, up to the usual tries.
	p3 := p2.Add(time.Hour)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, p3)
	c := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, p3.Add(time.Minute))
	end := *c[0].PassCaptureAt
	for i := 0; i < maxReportTries; i++ {
		s.SetPassReport(e.ID, end, "")
		again := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, p3.Add(2*time.Minute))
		if want := i < maxReportTries-1; (len(again) == 1) != want {
			t.Fatalf("try %d: recapture %d, want %v", i, len(again), want)
		}
	}
	if got := get(t, s, e.ID); got.LastPassReport != "pass 29: filed two to-dos" || got.PassCaptureAt != nil {
		t.Fatalf("empty replies: %+v", got)
	}
}

// A loop worker that never reported has no report, but its pass reply is
// kept (and is "newer" than the report it does not have).
func TestLoopPassReportWithoutReport(t *testing.T) {
	s, e := newWorker(t, Entry{Loop: "watch the PR"})
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, t0)
	c := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t0.Add(time.Minute))
	s.SetPassReport(e.ID, *c[0].PassCaptureAt, "PR still open")
	got := get(t, s, e.ID)
	if got.FinalReport != "" || got.ReportAt != nil || got.LastPassReport != "PR still open" || !got.NewerReply() {
		t.Fatalf("%+v", got)
	}
}

// A landing turn that ends without a landing report keeps the report the
// landing was approved on, and the turn's reply goes beside it.
func TestLandingTurnWithoutReportKeepsReply(t *testing.T) {
	s, e := newWorker(t, Entry{})
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Report(wk, "## Summary\nready", t0)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t0.Add(time.Minute))
	if _, ok, err := s.StartLanding(wk, Approval{Base: "main", At: t0.Add(time.Hour)}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	t1 := t0.Add(time.Hour)
	s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusWorking)}, t1)
	c := s.Observe([]herdr.AgentInfo{agent(wk, herdr.StatusIdle)}, t1.Add(5*time.Minute))
	if len(c) != 1 || c[0].PassCaptureAt == nil {
		t.Fatalf("landing turn capture = %+v", c)
	}
	s.SetPassReport(e.ID, *c[0].PassCaptureAt, "pushed to main")
	got := get(t, s, e.ID)
	if got.FinalReport != "## Summary\nready" || got.LastPassReport != "pushed to main" || !got.NewerReply() {
		t.Fatalf("%+v", got)
	}
	at(t, got.ReportAt, t0, "report_at")
}

// Entries from before report_at load without one: their age is unknown,
// never guessed from FinishedAt (a re-finish moves that).
func TestReportAtOldEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	s, _ := Open(path)
	fin := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	e, _ := s.Add(Entry{AgentID: wk, Status: Finished, FinishedAt: &fin, FinalReport: "old", ReportSource: "explicit", ReportSeq: 3})
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, s2, e.ID); got.ReportAt != nil || got.ReportSeq != 3 || got.FinalReport != "old" {
		t.Fatalf("%+v", got)
	}
}
