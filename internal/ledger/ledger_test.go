package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func agent(id string, st herdr.AgentStatus) herdr.AgentInfo {
	return herdr.AgentInfo{ID: id, Status: st}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "ledger.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Add(Entry{Host: "desk", AgentID: "desk/w9:p1", Branch: "fix", Brief: "do it", Status: Running})
	if err != nil || !strings.HasPrefix(e.ID, "h") || e.CreatedAt.IsZero() {
		t.Fatalf("add = %+v %v", e, err)
	}
	if _, err := s.SetMark(Mark{AgentID: "desk/w1:p1", Reason: "old"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ledger file: %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".ledger-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if es := again.Entries(); len(es) != 1 || es[0].ID != e.ID || es[0].Brief != "do it" {
		t.Fatalf("reloaded = %+v", es)
	}
	if ms := again.Marks(); len(ms) != 1 || ms[0].Reason != "old" {
		t.Fatalf("marks = %+v", ms)
	}
	// A corrupt file is an error, never silently replaced.
	os.WriteFile(path, []byte("{nope"), 0o600)
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt ledger opened")
	}
}

func TestObserveFinishes(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	e, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running})
	now := time.Now()
	// Idle before it ever worked (the brief is still being submitted): not
	// finished.
	if c := s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now); len(c) != 0 {
		t.Fatalf("capture = %+v", c)
	}
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now)
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusBlocked)}, now)
	if got, _ := s.Get(e.ID); got.Status != Running || !got.SawWorking {
		t.Fatalf("working = %+v", got)
	}
	c := s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusDone)}, now)
	got, _ := s.Get(e.ID)
	if len(c) != 1 || got.Status != Finished || got.FinishedAt == nil {
		t.Fatalf("finished = %+v %+v", got, c)
	}
	if err := s.SetReport(e.ID, c[0].FinishedAt, "## Summary\nok"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(e.ID); got.FinalReport != "## Summary\nok" {
		t.Fatalf("report = %q", got.FinalReport)
	}
	// Back to work: running again, and a stale report capture is dropped.
	s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}, now)
	if got, _ := s.Get(e.ID); got.Status != Running || got.FinalReport != "" {
		t.Fatalf("rerun = %+v", got)
	}
	s.SetReport(e.ID, c[0].FinishedAt, "late")
	if got, _ := s.Get(e.ID); got.FinalReport != "" {
		t.Fatal("stale report stored")
	}
	// An empty report is looked for again, a bounded number of times.
	c = s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now)
	for i := 0; i < maxReportTries; i++ {
		s.SetReport(e.ID, c[0].FinishedAt, "")
		c2 := s.Observe([]herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}, now)
		if want := i < maxReportTries-1; (len(c2) == 1) != want {
			t.Fatalf("try %d: capture %v", i, c2)
		}
	}
	// Gone agents leave entries alone.
	if c := s.Observe(nil, now); len(c) != 0 {
		t.Fatal("capture without an agent")
	}
}

func TestMarks(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	e, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running})
	got, err := s.SetMark(Mark{AgentID: "desk/w9:p1", By: "me", Reason: "done"})
	if err != nil || got == nil || got.Status != ReadyToRemove || got.Mark.By != "me" || got.Mark.At.IsZero() {
		t.Fatalf("mark worker = %+v %v", got, err)
	}
	if len(s.Marks()) != 0 {
		t.Fatal("a worker's mark went to the standalone marks")
	}
	// Unmarking a worker that never finished: running again.
	if ok, _ := s.ClearMark("desk/w9:p1"); !ok {
		t.Fatal("unmark")
	}
	if g, _ := s.Get(e.ID); g.Status != Running || g.Mark != nil {
		t.Fatalf("unmarked = %+v", g)
	}
	// A finished one goes back to finished.
	t0 := time.Now()
	s.Update(e.ID, func(e *Entry) { e.Status, e.FinishedAt = Finished, &t0 })
	s.SetMark(Mark{AgentID: "desk/w9:p1"})
	s.ClearMark("desk/w9:p1")
	if g, _ := s.Get(e.ID); g.Status != Finished {
		t.Fatalf("unmarked finished = %+v", g)
	}
	// Any other agent: a standalone mark, replaced when marked again.
	s.SetMark(Mark{AgentID: "kube/w1:p1", Reason: "a"})
	s.SetMark(Mark{AgentID: "kube/w1:p1", Reason: "b"})
	if ms := s.Marks(); len(ms) != 1 || ms[0].Reason != "b" {
		t.Fatalf("marks = %+v", ms)
	}
	if ok, _ := s.ClearMark("nobody/x"); ok {
		t.Fatal("cleared a mark that was not there")
	}
	// Removal: the entry is removed, the standalone mark dropped.
	s.SetMark(Mark{AgentID: "desk/w9:p1"})
	if r, err := s.Removed("desk/w9:p1", json.RawMessage(`{"closed":"workspace"}`), time.Now()); err != nil || r == nil || r.Status != Removed || r.RemovedAt == nil {
		t.Fatalf("removed = %+v %v", r, err)
	}
	s.Removed("kube/w1:p1", nil, time.Now())
	if len(s.Marks()) != 0 {
		t.Fatal("mark kept after removal")
	}
	// A removed worker is no longer the agent's entry.
	if _, ok := s.ByAgent("desk/w9:p1"); ok {
		t.Fatal("removed entry still active")
	}
}

func TestTruncate(t *testing.T) {
	if Truncate("abc", 5) != "abc" {
		t.Fatal("short")
	}
	got := Truncate("aé", 2) // é is two bytes: cut before it
	if !strings.HasPrefix(got, "a\n") {
		t.Fatalf("got %q", got)
	}
}

// A session's answer lands on the mark it was asked for, and copies do
// not share it.
func TestSetAsk(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	s.SetMark(Mark{AgentID: "desk/w5:p1", At: at, Ask: &Ask{State: AskAsking}})
	if ok, err := s.SetAsk("desk/w5:p1", at.Add(time.Second), Ask{State: AskSafe}); ok || err != nil {
		t.Fatalf("another mark's answer applied: %v %v", ok, err)
	}
	if ok, _ := s.SetAsk("desk/w5:p1", at, Ask{State: AskUnsafe, Reason: "PR open"}); !ok {
		t.Fatal("answer not applied")
	}
	m := s.Marks()[0]
	if m.Ask.State != AskUnsafe || m.Ask.At.IsZero() {
		t.Fatalf("ask = %+v", m.Ask)
	}
	m.Ask.State = "changed"
	if s.Marks()[0].Ask.State != AskUnsafe {
		t.Fatal("Marks shares the ask")
	}
	// A worker's mark too, and not after it is cleared.
	e, _ := s.Add(Entry{Host: "desk", AgentID: "desk/w9:p1", Branch: "fix", Status: Finished})
	s.SetMark(Mark{AgentID: e.AgentID, At: at})
	if ok, _ := s.SetAsk(e.AgentID, at, Ask{State: AskSafe}); !ok {
		t.Fatal("worker answer not applied")
	}
	s.ClearMark(e.AgentID)
	if ok, _ := s.SetAsk(e.AgentID, at, Ask{State: AskSafe}); ok {
		t.Fatal("answer applied to a cleared mark")
	}
}

func TestCallbackQueue(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	asked, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, ReplyTo: &Ref{AgentID: "desk/w1:p1", Name: "coordinator"}})
	plain, _ := s.Add(Entry{AgentID: "desk/w9:p2", Status: Running}) // started by the user
	now := time.Now()
	work := []herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking), agent("desk/w9:p2", herdr.StatusWorking)}
	idle := []herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle), agent("desk/w9:p2", herdr.StatusIdle)}
	s.Observe(work, now)
	if len(s.PendingCallbacks()) != 0 {
		t.Fatal("callback before finishing")
	}
	s.Observe(idle, now)
	p := s.PendingCallbacks()
	if len(p) != 1 || p[0].ID != asked.ID || p[0].Callback.State != CallbackPending || p[0].ReplyTo.Name != "coordinator" {
		t.Fatalf("pending = %+v", p)
	}
	if got, _ := s.Get(plain.ID); got.Callback != nil {
		t.Fatal("callback for a handoff nobody asked for")
	}

	// Back to work drops it; finishing again queues a new one, and a
	// settle with the old queue time is ignored.
	old := p[0].Callback.At
	s.Observe(work, now)
	if got, _ := s.Get(asked.ID); got.Callback != nil {
		t.Fatalf("callback kept: %+v", got.Callback)
	}
	later := now.Add(time.Minute)
	s.Observe(idle, later)
	if err := s.CallbackDone(asked.ID, old, CallbackDelivered, "desk/w1:p1", later); err != nil {
		t.Fatal(err)
	}
	if len(s.PendingCallbacks()) != 1 {
		t.Fatal("stale settle applied")
	}
	cur := s.PendingCallbacks()[0].Callback.At
	s.CallbackTried(asked.ID, cur)
	s.CallbackDone(asked.ID, cur, CallbackDelivered, "desk/w1:p1", later)
	got, _ := s.Get(asked.ID)
	if got.Callback.State != CallbackDelivered || got.Callback.To != "desk/w1:p1" || got.Callback.Tries != 2 || len(s.PendingCallbacks()) != 0 {
		t.Fatalf("delivered = %+v", got.Callback)
	}
	// It survives a reload.
	s2, _ := Open(s.path)
	if got, _ := s2.Get(asked.ID); got.Callback == nil || got.ReplyTo == nil {
		t.Fatalf("reload = %+v", got)
	}
}

func TestExplicitReport(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	asked, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, ReplyTo: &Ref{AgentID: "desk/w1:p1"}})
	now := time.Now()
	work := []herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}
	idle := []herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusIdle)}
	if _, ok, _ := s.Report("desk/nobody", "x", now); ok {
		t.Fatal("report for an agent without an entry")
	}
	// Reporting needs no earlier sighting of the worker working.
	e, ok, err := s.Report(asked.AgentID, "## Summary\nexplicit", now)
	if !ok || err != nil || e.Status != Finished || e.ReportSource != "explicit" || e.Callback == nil || e.Callback.State != CallbackPending {
		t.Fatalf("report = %+v %v %v", e, ok, err)
	}
	// The turn that ran the report is still working: it stays finished.
	if c := s.Observe(work, now); len(c) != 0 {
		t.Fatalf("capture = %+v", c)
	}
	if got, _ := s.Get(asked.ID); got.Status != Finished || got.FinalReport != "## Summary\nexplicit" {
		t.Fatalf("working after report = %+v", got)
	}
	// Idle: no guess replaces the report.
	if c := s.Observe(idle, now); len(c) != 0 {
		t.Fatalf("capture at idle = %+v", c)
	}
	s.SetReport(asked.ID, e.FinishedAt, "a guess")
	if got, _ := s.Get(asked.ID); got.FinalReport != "## Summary\nexplicit" || got.AwaitIdle {
		t.Fatalf("idle = %+v", got)
	}
	// A second report while finished replaces the text, not the callback.
	at := e.Callback.At
	e2, _, _ := s.Report(asked.AgentID, "second", now.Add(time.Minute))
	if e2.FinalReport != "second" || !e2.Callback.At.Equal(at) {
		t.Fatalf("second = %+v", e2)
	}
	s.Observe(idle, now)
	// New work after that is new work.
	s.Observe(work, now)
	if got, _ := s.Get(asked.ID); got.Status != Running || got.FinalReport != "" || got.ReportSource != "" || got.Callback != nil {
		t.Fatalf("rerun = %+v", got)
	}
}

// The turn that ran `comebye report` is absorbed only briefly: a worker
// seen working long after it reported has been given more to do.
func TestExplicitReportGrace(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	e, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running})
	now := time.Now()
	s.Report(e.AgentID, "done", now)
	work := []herdr.AgentInfo{agent("desk/w9:p1", herdr.StatusWorking)}
	s.Observe(work, now.Add(30*time.Second))
	if g, _ := s.Get(e.ID); g.Status != Finished {
		t.Fatalf("reopened inside the grace: %+v", g)
	}
	s.Observe(work, now.Add(reportTurnGrace+time.Second))
	if g, _ := s.Get(e.ID); g.Status != Running || g.FinalReport != "" || g.ReportSource != "" {
		t.Fatalf("not reopened after the grace: %+v", g)
	}
}

// A failed handoff handed off again is replaced in its plan, not counted
// twice.
func TestPlanSupersedesFailedMember(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Now()
	p, _ := s.AddPlan(Plan{Title: "x", Size: 1})
	s.Add(Entry{Host: "desk", RepoName: "r", Branch: "b", Status: Failed, PlanID: p.ID, CreatedAt: now.Add(-time.Minute)})
	if got := s.EvalPlans(now); len(got) != 1 || got[0].Outcome != PlanPartial {
		t.Fatalf("failed alone = %+v", got)
	}
	again, _ := s.Add(Entry{Host: "desk", RepoName: "r", Branch: "b", AgentID: "desk/w1:p1", Status: Running, PlanID: p.ID, CreatedAt: now})
	if m := s.Members(p.ID); len(m) != 1 || m[0].ID != again.ID {
		t.Fatalf("members = %+v", m)
	}
	s.Observe([]herdr.AgentInfo{agent(again.AgentID, herdr.StatusWorking)}, now)
	s.Observe([]herdr.AgentInfo{agent(again.AgentID, herdr.StatusIdle)}, now)
	if got := s.EvalPlans(now); len(got) != 1 || got[0].Outcome != PlanDone {
		t.Fatalf("after the retry = %+v", got)
	}
}
