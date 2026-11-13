package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestAutoPark(t *testing.T) {
	r := newSugRig(t)
	var parked []string
	fail := map[string]bool{}
	r.s.parkSug.auto = true
	r.s.parkSug.parkOne = func(_ context.Context, a herdr.AgentInfo) (string, error) {
		if fail[a.Name] {
			return "", errors.New("a loop is scheduled")
		}
		parked = append(parked, a.Name)
		return "pk_" + a.Name, nil
	}
	dirty := r.idleFor("w7:p1", "dirty", 30*time.Hour)
	r.dirty[dirty.ID] = true
	queued := r.idleFor("w8:p1", "queued", 30*time.Hour)
	if _, _, err := r.s.pq.add(QueuedPrompt{Agent: queued.ID, Text: "later"}); err != nil {
		t.Fatal(err)
	}
	loopy := r.idleFor("w9:p1", "loopy", 30*time.Hour)
	fail["loopy"] = true
	r.hinted(r.idleFor("w1:p1", "ok", 25*time.Hour), r.idleFor("w2:p1", "young", 5*time.Hour), dirty, queued, loopy)
	if len(parked) != 1 || parked[0] != "ok" {
		t.Fatalf("parked %v, want only ok", parked)
	}
	// The refused one stays suggested and is not retried at once.
	if !r.hinted(loopy)["loopy"] {
		t.Error("a refused agent should stay suggested")
	}
	r.hinted(loopy)
	delete(fail, "loopy")
	r.hinted(loopy)
	if len(parked) != 1 {
		t.Errorf("retried too soon: %v", parked)
	}
	r.now = r.now.Add(2 * time.Hour)
	loopy.StatusSince = ptrTime(r.now.Add(-30 * time.Hour))
	r.hinted(loopy)
	if len(parked) != 2 {
		t.Errorf("no retry after the wait: %v", parked)
	}
	// Suggest mode parks nothing.
	r2 := newSugRig(t)
	n := 0
	r2.s.parkSug.parkOne = func(context.Context, herdr.AgentInfo) (string, error) { n++; return "x", nil }
	r2.hinted(r2.idleFor("w1:p1", "ok", 48*time.Hour))
	if n != 0 {
		t.Error("suggest mode parked an agent")
	}
	// The per-step limit.
	r3 := newSugRig(t)
	r3.s.parkSug.auto = true
	m := 0
	r3.s.parkSug.parkOne = func(context.Context, herdr.AgentInfo) (string, error) { m++; return "x", nil }
	r3.hinted(r3.idleFor("a:p1", "a", 30*time.Hour), r3.idleFor("b:p1", "b", 30*time.Hour), r3.idleFor("c:p1", "c", 30*time.Hour))
	if m != autoParkPerStep {
		t.Errorf("parked %d in one step, want %d", m, autoParkPerStep)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestPendingSchedule(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	use := func(ts time.Time, name, input string) string {
		return `{"type":"assistant","timestamp":"` + ts.Format(time.RFC3339) + `","message":{"content":[{"type":"tool_use","name":"` + name + `","input":` + input + `}]}}` + "\n"
	}
	old := now.Add(-30 * time.Hour)
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"nothing":        {"{\"type\":\"user\"}\n", false},
		"cron":           {use(old, "CronCreate", `{"cron":"*/5 * * * *","prompt":"x"}`), true},
		"cron deleted":   {use(old, "CronCreate", `{"cron":"* * * * *"}`) + use(old, "CronDelete", `{"id":"1"}`), false},
		"one-shot cron":  {use(old, "CronCreate", `{"recurring":false}`), false},
		"wakeup fired":   {use(old, "ScheduleWakeup", `{"delaySeconds":1200}`), false},
		"wakeup pending": {use(now.Add(-time.Minute), "ScheduleWakeup", `{"delaySeconds":1200}`), true},
		"loop stopped":   {use(now.Add(-time.Minute), "ScheduleWakeup", `{"delaySeconds":1200}`) + use(now, "ScheduleWakeup", `{"stop":true}`), false},
	} {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := pendingSchedule(p, now)
		if err != nil || got != tc.want {
			t.Errorf("%s: pending=%v err=%v, want %v", name, got, err, tc.want)
		}
	}
	if _, err := pendingSchedule(filepath.Join(t.TempDir(), "none"), now); err == nil {
		t.Error("a missing file should be an error (uncertain)")
	}
}
