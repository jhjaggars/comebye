package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

type quietRig struct {
	t     *testing.T
	s     *Server
	mtime time.Time
	bg    []transcript.BackgroundTask
	reads int
}

func newQuietRig(t *testing.T) *quietRig {
	t.Helper()
	s, _, _ := todoServer(t)
	s.initQuiet(30 * time.Minute)
	r := &quietRig{t: t, s: s, mtime: autoT0}
	s.latest = func(context.Context, string, string) *transcript.Latest {
		r.reads++
		return &transcript.Latest{Mtime: r.mtime.Format(time.RFC3339Nano), Background: r.bg, BackgroundKnown: true}
	}
	if _, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "fix/x", AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "feat-x", Status: ledger.Running}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *quietRig) step(at time.Duration, st herdr.AgentStatus, since time.Duration) []herdr.AgentInfo {
	now := autoT0.Add(at)
	a := rAgent("w9:p1", "feat-x", st, autoT0.Add(since))
	r.s.todos.now = func() time.Time { return now }
	agents := []herdr.AgentInfo{a}
	r.s.quietStep(context.Background(), agents, now)
	r.s.stampQuiet(agents)
	return agents
}

func (r *quietRig) open() []Todo {
	var out []Todo
	for _, it := range r.s.todos.list("") {
		if it.Source == SourceQuiet {
			out = append(out, it)
		}
	}
	return out
}

func TestQuietMarkAndOneTodo(t *testing.T) {
	r := newQuietRig(t)
	// 20 minutes into a long turn: under the threshold, and no read at all.
	if a := r.step(20*time.Minute, herdr.StatusWorking, 0); a[0].QuietSince != nil || r.reads != 0 || len(r.open()) != 0 {
		t.Fatalf("20 min: quiet=%v reads=%d items=%d", a[0].QuietSince, r.reads, len(r.open()))
	}
	// 35 minutes, nothing written since the start: quiet, one item.
	a := r.step(35*time.Minute, herdr.StatusWorking, 0)
	if a[0].QuietSince == nil || !a[0].QuietSince.Equal(autoT0) {
		t.Fatalf("35 min: QuietSince = %v", a[0].QuietSince)
	}
	got := r.open()
	if len(got) != 1 || got[0].Waiting.ID != "desk/w9:p1" || got[0].CreatedBy != "hub" || got[0].State != TodoOpen {
		t.Fatalf("items = %+v", got)
	}
	// Later passes (and a long spell) file nothing more.
	for _, m := range []time.Duration{37, 60, 120} {
		r.step(m*time.Minute, herdr.StatusWorking, 0)
	}
	if n := len(r.s.todos.list("all")); n != 1 {
		t.Fatalf("todos after repeated passes = %d, want 1", n)
	}
	// The worker writes: the mark and the item clear.
	r.mtime = autoT0.Add(125 * time.Minute)
	a = r.step(127*time.Minute, herdr.StatusWorking, 0)
	if a[0].QuietSince != nil {
		t.Fatalf("still quiet after a write: %v", a[0].QuietSince)
	}
	if open := r.s.todos.list(""); len(open) != 0 {
		t.Fatalf("item not closed: %+v", open)
	}
	closed := r.s.todos.list("all")
	if len(closed) != 1 || closed[0].DropReason != "the agent wrote again" {
		t.Fatalf("closed = %+v", closed)
	}
}

func TestQuietSkips(t *testing.T) {
	// A background task the session records: not quiet.
	r := newQuietRig(t)
	r.bg = []transcript.BackgroundTask{{Tool: "Bash", Desc: "go test ./...", Cmd: true}}
	if a := r.step(60*time.Minute, herdr.StatusWorking, 0); a[0].QuietSince != nil || len(r.open()) != 0 {
		t.Fatal("quiet with a background task running")
	}
	// Idle or blocked is not "working".
	for _, st := range []herdr.AgentStatus{herdr.StatusIdle, herdr.StatusBlocked, herdr.StatusDone} {
		r = newQuietRig(t)
		if a := r.step(60*time.Minute, st, 0); a[0].QuietSince != nil || len(r.open()) != 0 || r.reads != 0 {
			t.Fatalf("%s: marked quiet", st)
		}
	}
	// A recent write, though herdr has said working for hours (LastActive would say busy; so does the file).
	r = newQuietRig(t)
	r.mtime = autoT0.Add(170 * time.Minute)
	if a := r.step(180*time.Minute, herdr.StatusWorking, 0); a[0].QuietSince != nil {
		t.Fatal("quiet with a write 10 minutes ago")
	}
	// A session with no mtime (cannot tell) is never called quiet.
	r = newQuietRig(t)
	r.s.latest = func(context.Context, string, string) *transcript.Latest { return &transcript.Latest{} }
	if a := r.step(60*time.Minute, herdr.StatusWorking, 0); a[0].QuietSince != nil {
		t.Fatal("quiet without an mtime")
	}
	// Not a handoff worker: no mark.
	r = newQuietRig(t)
	now := autoT0.Add(60 * time.Minute)
	other := rAgent("w5:p1", "mine", herdr.StatusWorking, autoT0)
	r.s.quietStep(context.Background(), []herdr.AgentInfo{other}, now)
	if r.reads != 0 {
		t.Fatal("read a session that is no handoff worker's")
	}
}

func TestQuietItemClosesWhenWorkerLeavesWorking(t *testing.T) {
	r := newQuietRig(t)
	r.step(40*time.Minute, herdr.StatusWorking, 0)
	if len(r.open()) != 1 {
		t.Fatal("no item")
	}
	r.step(41*time.Minute, herdr.StatusIdle, 41*time.Minute)
	if len(r.s.todos.list("")) != 0 {
		t.Fatal("item left open after the worker went idle")
	}
	// Working again for 31 minutes with the same stale file is a new spell,
	// but the same write time: the dedupe key keeps it from filing twice.
	r.step(80*time.Minute, herdr.StatusWorking, 42*time.Minute)
	if n := len(r.s.todos.list("all")); n != 2 {
		t.Fatalf("todos = %d, want one per spell (2)", n)
	}
}

func TestLatestOfReportsMtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","content":"hi"}}`+"\n"), 0o600)
	want := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	os.Chtimes(p, want, want)
	l, err := transcript.LatestOf(p)
	if err != nil || l == nil {
		t.Fatal(err)
	}
	got, perr := time.Parse(time.RFC3339Nano, l.Mtime)
	if perr != nil || !got.Equal(want) {
		t.Fatalf("Mtime = %q (%v), want %v", l.Mtime, perr, want)
	}
}
