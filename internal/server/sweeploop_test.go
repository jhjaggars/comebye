package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

func sweepItemsOf(s *Server) (all []Todo, active []Todo) {
	for _, t := range s.todos.list("all") {
		if t.Source == SourceSweep {
			all = append(all, t)
			if t.active() {
				active = append(active, t)
			}
		}
	}
	return
}

func putSweepSetting(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	w := do(s, "PUT", "/api/settings", userHdr, body)
	return w.Code, w.Body.String()
}

func TestSweepLoopSchedule(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	ctx := context.Background()
	t0 := time.Now()
	if !s.sweepLoopStep(ctx, t0) {
		t.Fatal("the first step did not sweep")
	}
	if st := s.sweeps.get(); st.Runs != 1 || !st.LastRunAt.Equal(t0) {
		t.Fatalf("state after the first run = %+v", st)
	}
	if s.sweepLoopStep(ctx, t0.Add(59*time.Minute)) {
		t.Fatal("swept again within the hour")
	}
	if !s.sweepLoopStep(ctx, t0.Add(61*time.Minute)) {
		t.Fatal("did not sweep after an hour")
	}
	// The interval is a setting.
	if code, body := putSweepSetting(t, s, `{"sweep_loop":{"every_minutes":30}}`); code != 200 {
		t.Fatalf("put = %d %s", code, body)
	}
	if s.sweepLoopStep(ctx, t0.Add(61*time.Minute+29*time.Minute)) {
		t.Fatal("swept before the 30 minutes")
	}
	if !s.sweepLoopStep(ctx, t0.Add(61*time.Minute+31*time.Minute)) {
		t.Fatal("did not sweep after the 30 minutes")
	}
	// Off is "only marks": the loop still sweeps on its schedule.
	if code, body := putSweepSetting(t, s, `{"sweep_loop":{"enabled":false,"every_minutes":30}}`); code != 200 {
		t.Fatalf("put off = %d %s", code, body)
	}
	runs := s.sweeps.get().Runs
	if !s.sweepLoopStep(ctx, t0.Add(48*time.Hour)) || s.sweeps.get().Runs != runs+1 {
		t.Fatal("a sweep did not run while clean-up is off")
	}
	// The interval stayed when it was switched off.
	if v := s.sweepLoopView(); v.Enabled || v.EveryMinutes != 30 || v.Source != "setting" {
		t.Fatalf("view = %+v", v)
	}
}

func TestSweepLoopSettingsRoundTrip(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	if v := s.sweepLoopView(); !v.Enabled || v.EveryMinutes != 60 || v.Source != "default" {
		t.Fatalf("default view = %+v", v)
	}
	for _, bad := range []string{`{"sweep_loop":{"every_minutes":5}}`, `{"sweep_loop":{"every_minutes":100000}}`} {
		if code, _ := putSweepSetting(t, s, bad); code != 400 {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
	if code, body := putSweepSetting(t, s, `{"sweep_loop":{"enabled":false,"every_minutes":120}}`); code != 200 {
		t.Fatalf("put = %d %s", code, body)
	}
	// A client that does not know the setting (the CLI, an older app) saves the
	// rest and leaves it alone.
	if code, body := putSweepSetting(t, s, `{"default_kind":"","allowed_kinds":[],"kinds":{}}`); code != 200 {
		t.Fatalf("put without it = %d %s", code, body)
	}
	w := do(s, "GET", "/api/settings", userHdr, "")
	var v struct {
		SweepLoop sweepLoopView `json:"sweep_loop"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.SweepLoop.Enabled || v.SweepLoop.EveryMinutes != 120 || !v.SweepLoop.Hub {
		t.Fatalf("GET = %s (%v)", w.Body, err)
	}
}

// The sweep marks, the loop removes what has nothing to lose after the grace,
// leaves everything else, files no To do, and a second run does nothing.
func TestSweepLoopMarksAndNeverFilesAnItem(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	ctx := context.Background()
	t0 := time.Now()
	s.sweepLoopStep(ctx, t0)
	s.asking.Wait()
	if st := s.sweeps.get(); st.LastMarked == 0 {
		t.Fatalf("the first sweep marked nothing: %+v", st)
	}
	if got := statusOf(s, f.ids["merged"]); got != ledger.ReadyToRemove {
		t.Fatalf("merged = %s, want ready_to_remove", got)
	}
	e, _ := s.ledger.ByAgent(f.ids["merged"])
	if e.Mark == nil || e.Mark.By != AutoCreator || !strings.HasPrefix(e.Mark.Reason, "Sweep:") {
		t.Fatalf("mark = %+v", e.Mark)
	}
	// Hours on: still no item, in any state, and the sessions that need a look stay.
	for h := 1; h <= 26; h += 5 {
		s.sweepLoopStep(ctx, t0.Add(time.Duration(h)*time.Hour))
		s.asking.Wait()
	}
	if all, _ := sweepItemsOf(s); len(all) != 0 {
		t.Fatalf("the loop filed a To do: %+v", all)
	}
	for _, n := range []string{"pushed", "dirty", "coord", "marked"} {
		if st := statusOf(s, f.ids[n]); st == ledger.Removed {
			t.Errorf("%s was removed by the loop", n)
		}
	}
}

// An item an earlier version filed is closed, and no new one appears.
func TestSweepLoopClosesTheOldItem(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	it, _, err := s.todos.add(Todo{Question: "7 sessions are ready to remove", Kind: "action", CreatedBy: AutoCreator, Source: SourceSweep, AutoKey: SourceSweep + ":x"})
	if err != nil {
		t.Fatal(err)
	}
	s.sweepLoopStep(context.Background(), time.Now())
	if t2, ok := s.todos.get(it.ID); !ok || t2.active() {
		t.Fatalf("the old item is still open: %+v", t2)
	}
}

func TestSweepLoopBoardLineAndLog(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	if lane := s.hubDidLane(time.Now()); lane != nil {
		t.Fatalf("lane before any sweep = %+v", lane)
	}
	s.sweepLoopStep(context.Background(), time.Now())
	s.asking.Wait()
	lane := s.hubDidLane(time.Now())
	if lane == nil || len(lane.Items) < 2 {
		t.Fatalf("lane = %+v", lane)
	}
	first := lane.Items[0]
	if first.Kind != evSweep || !strings.HasPrefix(first.Title, "Sweep: marked ") || !strings.Contains(first.Title, "need a look") || first.Since == "" {
		t.Fatalf("board line = %+v", first)
	}
	if ev := lane.Items[1]; ev.Kind != evSweep || !strings.Contains(ev.Title, "ready to remove") {
		t.Fatalf("event = %+v", ev)
	}
	// A sweep that does nothing leaves the log alone and updates the line.
	n := len(s.events.query(time.Now().Add(-time.Hour), []string{evSweep}, 0))
	s.sweepLoopStep(context.Background(), time.Now().Add(2*time.Hour))
	if got := len(s.events.query(time.Now().Add(-time.Hour), []string{evSweep}, 0)); got != n {
		t.Fatalf("a sweep that did nothing logged an event (%d -> %d)", n, got)
	}
	if line, _, _ := s.sweepBoardLine(); !strings.HasPrefix(line, "Sweep: marked 0") && !strings.HasPrefix(line, "Sweep: nothing to clean up") {
		t.Fatalf("line = %q", line)
	}
}

func TestSweepLoopStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweeploop.json")
	l := openSweepLoop(path, false)
	l.mu.Lock()
	l.st = sweepLoopState{LastRunAt: time.Now().UTC().Truncate(time.Second), Runs: 3, LastMarked: 2, Done: []sweepDone{{At: time.Now().UTC().Truncate(time.Second), Kind: doneBranch, Name: "x", Branch: "w", Tip: "abc"}}}
	l.saveLocked()
	l.mu.Unlock()
	got := openSweepLoop(path, false).get()
	if got.Runs != 3 || got.LastMarked != 2 || len(got.Done) != 1 || got.Done[0].Tip != "abc" {
		t.Fatalf("reloaded = %+v", got)
	}
}
