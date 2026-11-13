package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
)

type sugRig struct {
	s     *Server
	now   time.Time
	dirty map[string]bool
	reads int
}

func newSugRig(t *testing.T) *sugRig {
	t.Helper()
	s, _, _ := coordServer(t)
	s.initParkSuggest(24 * time.Hour)
	r := &sugRig{s: s, now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), dirty: map[string]bool{}}
	s.parkSug.checkClean = func(_ context.Context, a herdr.AgentInfo) (bool, bool) { r.reads++; return !r.dirty[a.ID], true }
	return r
}

// idleFor is a worker idle for d in its own worktree.
func (r *sugRig) idleFor(pane, name string, d time.Duration) herdr.AgentInfo {
	since := r.now.Add(-d)
	return herdr.AgentInfo{ID: "desk/" + pane, Host: "desk", PaneID: pane, Name: name, Agent: "claude", Status: herdr.StatusIdle, StatusSince: &since,
		Worktree: &herdr.WorktreeInfo{RepoName: "proj", CheckoutPath: "/wt/" + name, IsLinked: true}}
}

func (r *sugRig) hinted(agents ...herdr.AgentInfo) map[string]bool {
	r.s.parkSuggestStep(context.Background(), agents, r.now)
	out := map[string]bool{}
	cp := append([]herdr.AgentInfo(nil), agents...)
	r.s.stampPark(cp)
	for _, a := range cp {
		out[a.Name] = a.ParkHint != nil
	}
	return out
}

func TestParkSuggestGuards(t *testing.T) {
	r := newSugRig(t)
	home := r.idleFor("w2:p1", "home", 30*time.Hour)
	home.Worktree.IsLinked = false // a repository's own checkout: a home session
	bg := r.idleFor("w3:p1", "bg", 30*time.Hour)
	bg.Background = []herdr.BackgroundTask{{Kind: "bash", Label: "tests"}}
	working := r.idleFor("w4:p1", "working", 30*time.Hour)
	working.Status = herdr.StatusWorking
	codex := r.idleFor("w5:p1", "codex", 30*time.Hour)
	codex.Agent = "codex"
	coord := r.idleFor("w1:p1", "coordinator", 30*time.Hour)
	r.s.recordRole(coord.ID, "coordinator", RoleCoordinator, "test")
	asker := r.idleFor("w6:p1", "asker", 30*time.Hour)
	if _, _, err := r.s.todos.add(Todo{Question: "Which one?", Kind: "decision", CreatedBy: "asker", CreatedByID: asker.ID, Source: "agent"}); err != nil {
		t.Fatal(err)
	}
	dirty := r.idleFor("w7:p1", "dirty", 30*time.Hour)
	r.dirty[dirty.ID] = true
	got := r.hinted(
		r.idleFor("w8:p1", "ok", 25*time.Hour), r.idleFor("w9:p1", "recent", 2*time.Hour),
		home, bg, working, codex, coord, asker, dirty)
	for name, want := range map[string]bool{"ok": true, "recent": false, "home": false, "bg": false, "working": false, "codex": false, "coordinator": false, "asker": false, "dirty": false} {
		if got[name] != want {
			t.Errorf("%s: suggested=%v, want %v", name, got[name], want)
		}
	}
	// Typing in its box blocks a suggestion too.
	typer := r.idleFor("w10:p1", "typer", 30*time.Hour)
	r.s.typing.observe("w10:p1", dialog.Input{Known: true, Busy: true})
	if r.hinted(typer)["typer"] {
		t.Error("suggested an agent with text in its input box")
	}
}

func TestParkSuggestFollowsTheWorktree(t *testing.T) {
	r := newSugRig(t)
	a := r.idleFor("w8:p1", "ok", 25*time.Hour)
	if !r.hinted(a)["ok"] || r.reads != 1 {
		t.Fatalf("first step: reads=%d", r.reads)
	}
	// Not asked again within the recheck window.
	r.now = r.now.Add(time.Minute)
	r.hinted(a)
	if r.reads != 1 {
		t.Fatalf("rechecked after a minute: %d", r.reads)
	}
	// Edits appear: after the window the suggestion goes.
	r.dirty[a.ID] = true
	r.now = r.now.Add(parkSugRecheck + time.Minute)
	if r.hinted(a)["ok"] {
		t.Fatal("still suggested with edits")
	}
	// It starts working: no suggestion, and the state is dropped.
	r.dirty[a.ID] = false
	r.now = r.now.Add(parkSugRecheck + time.Minute)
	w := a
	w.Status = herdr.StatusWorking
	if r.hinted(w)["ok"] || len(r.s.parkSug.m) != 0 {
		t.Fatalf("working agent kept state: %v", r.s.parkSug.m)
	}
}

func TestParkSuggestConfig(t *testing.T) {
	s, _, _ := coordServer(t)
	s.initParkSuggest(5 * time.Minute)
	if s.parkSug == nil || s.parkSug.after != time.Hour {
		t.Fatalf("threshold = %+v (the minimum is an hour)", s.parkSug)
	}
	s.parkSug = nil
	s.initParkSuggest(0)
	if s.parkSug != nil {
		t.Fatal("0 must turn suggestions off")
	}
	s.stampPark([]herdr.AgentInfo{{}}) // off: no panic
}

func TestParkSuggestReachesBrowsersAndTheBriefing(t *testing.T) {
	r := newSugRig(t)
	a := r.idleFor("w8:p1", "ok", 25*time.Hour)
	q := time.Now().Add(-time.Hour)
	a.QuietSince = &q // a working worker's mark must reach browsers as well (it did not before)
	r.hinted(a)
	out := r.s.slimAgents([]herdr.AgentInfo{a})
	if out[0].ParkHint == nil || !out[0].ParkHint.Since.Equal(*a.StatusSince) || out[0].QuietSince == nil {
		t.Fatalf("slim = %+v", out[0])
	}
	if a.ParkHint != nil {
		t.Fatal("slimAgents stamped the caller's copy")
	}
	if got := parkLine(0); got != "" {
		t.Fatalf("line for none = %q", got)
	}
	if got := parkLine(1); !strings.Contains(got, "1 agent can be parked") || !strings.Contains(got, "Do not park them yourself") {
		t.Fatalf("line = %q", got)
	}
	if got := parkLine(6); !strings.Contains(got, "6 agents can be parked") {
		t.Fatalf("line = %q", got)
	}
}
