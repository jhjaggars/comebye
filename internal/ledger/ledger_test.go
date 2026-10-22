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
