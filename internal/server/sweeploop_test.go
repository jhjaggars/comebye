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
	// Off means off: nothing runs, however late it is.
	if code, body := putSweepSetting(t, s, `{"sweep_loop":{"enabled":false,"every_minutes":30}}`); code != 200 {
		t.Fatalf("put off = %d %s", code, body)
	}
	runs := s.sweeps.get().Runs
	if s.sweepLoopStep(ctx, t0.Add(48*time.Hour)) || s.sweeps.get().Runs != runs {
		t.Fatal("a sweep ran while the setting is off")
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

// One batched item a day, filed once the marks are old enough and a review
// finds them removable; a second sweep marks nothing new; nothing is removed.
func TestSweepLoopMarksFilesOneItemAndRemovesNothing(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	ctx := context.Background()
	t0 := time.Now()
	s.sweepLoopStep(ctx, t0)
	s.asking.Wait()
	st := s.sweeps.get()
	if st.LastMarked == 0 {
		t.Fatalf("the first sweep marked nothing: %+v", st)
	}
	first := st.LastMarked
	if got := statusOf(s, f.ids["merged"]); got != ledger.ReadyToRemove {
		t.Fatalf("merged = %s, want ready_to_remove", got)
	}
	// A fresh mark is still being asked and may be removed by the hub itself:
	// no item yet.
	if _, all := sweepItemsOf(s); len(all) != 0 {
		t.Fatalf("an item was filed at once: %v", all)
	}
	e, _ := s.ledger.ByAgent(f.ids["merged"])
	if e.Mark == nil || e.Mark.By != AutoCreator || !strings.HasPrefix(e.Mark.Reason, "Sweep:") {
		t.Fatalf("mark = %+v", e.Mark)
	}

	// An hour on: the second sweep marks nothing new, and the item is filed.
	t1 := t0.Add(61 * time.Minute)
	if !s.sweepLoopStep(ctx, t1) {
		t.Fatal("no second sweep")
	}
	s.asking.Wait()
	if st := s.sweeps.get(); st.LastMarked != 0 || st.Runs != 2 {
		t.Fatalf("second sweep = %+v, want it to mark nothing", st)
	}
	all, active := sweepItemsOf(s)
	if len(all) != 1 || len(active) != 1 {
		t.Fatalf("items after the second sweep = %d (%d open), want one", len(all), len(active))
	}
	it := active[0]
	if !strings.Contains(it.Question, "ready to remove") || it.Kind != "action" || it.CreatedBy != AutoCreator {
		t.Fatalf("item = %+v", it)
	}
	if len(it.Links) != 1 || it.Links[0].URL != SweepReviewLink {
		t.Fatalf("links = %+v", it.Links)
	}
	if !strings.Contains(it.Context, "merged") || !strings.Contains(it.Context, "Remove all safe") {
		t.Fatalf("context = %q", it.Context)
	}
	t.Logf("filed: %q\n%s", it.Question, it.Context)
	_ = first

	// More sweeps over the next day: still one item, nothing new marked.
	for h := 2; h <= 22; h += 4 {
		s.sweepLoopStep(ctx, t0.Add(time.Duration(h)*time.Hour))
		s.asking.Wait()
	}
	if all, active = sweepItemsOf(s); len(all) != 1 || len(active) != 1 {
		t.Fatalf("items after a day of sweeps = %d (%d open), want one", len(all), len(active))
	}
	// Never removed: the session is marked, its worktree is there.
	if got := statusOf(s, f.ids["merged"]); got != ledger.ReadyToRemove {
		t.Fatalf("merged = %s after the loop, want ready_to_remove", got)
	}
	for n, id := range f.ids {
		if st := statusOf(s, id); st == ledger.Removed {
			t.Errorf("%s was removed by the loop", n)
		}
	}

	// The user closes the item: nothing is filed again for a day, then one is.
	if _, err := s.todos.close(it.ID, TodoDone, "me", ""); err != nil {
		t.Fatal(err)
	}
	s.sweepLoopStep(ctx, t0.Add(24*time.Hour))
	if all, _ = sweepItemsOf(s); len(all) != 1 {
		t.Fatalf("an item was filed again within a day of the last: %d", len(all))
	}
	s.sweepLoopStep(ctx, t0.Add(26*time.Hour))
	if all, active = sweepItemsOf(s); len(all) != 2 || len(active) != 1 {
		t.Fatalf("a day later: %d items (%d open), want a second one", len(all), len(active))
	}
}

func TestSweepLoopItemFollowsTheCount(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	ctx := context.Background()
	t0 := time.Now()
	s.sweepLoopStep(ctx, t0)
	s.asking.Wait()
	later := t0.Add(time.Hour + 2*time.Minute)
	s.sweepLoopStep(ctx, later)
	_, active := sweepItemsOf(s)
	if len(active) != 1 {
		t.Fatalf("no item: %v", active)
	}
	// The user takes the session off the list another way: the item closes.
	for _, n := range []string{"merged", "marked", "home"} {
		if _, err := s.ledger.ClearMark(f.ids[n]); err != nil {
			t.Fatal(err)
		}
	}
	s.sweepLoopStep(ctx, later.Add(sweepTodoLook+time.Minute))
	if all, active := sweepItemsOf(s); len(active) != 0 || len(all) != 1 || all[0].State != TodoDone || all[0].ClosedBy != AutoCreator {
		t.Fatalf("item after nothing is left = %+v", all)
	}
}

func TestSweepTodoText(t *testing.T) {
	mk := func(n int) []*removal {
		var out []*removal
		for i := 0; i < n; i++ {
			out = append(out, &removal{ID: "h/w" + itoa(i), Name: "worker-" + itoa(i), Repo: "proj", Branch: "feat/x" + itoa(i), Mark: ledger.Mark{Reason: "Sweep: finished; worktree clean"}})
		}
		return out
	}
	q, c := sweepTodoText(mk(1))
	if q != "1 session is ready to remove" || !strings.Contains(c, "worker-0") {
		t.Fatalf("one: %q %q", q, c)
	}
	q, c = sweepTodoText(mk(23))
	if q != "23 sessions are ready to remove" || !strings.Contains(c, "and 3 more") || strings.Contains(c, "worker-22") {
		t.Fatalf("many: %q %q", q, c)
	}
	if strings.Count(c, "\n- ") != sweepTodoList+1 {
		t.Fatalf("lines = %d", strings.Count(c, "\n- "))
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
	if first.Kind != evSweep || !strings.HasPrefix(first.Title, "Last sweep: marked ") || !strings.Contains(first.Title, "need a look") || first.Since == "" {
		t.Fatalf("board line = %+v", first)
	}
	if ev := lane.Items[1]; ev.Kind != evSweep || !strings.Contains(ev.Title, "ready to remove") {
		t.Fatalf("event = %+v", ev)
	}
	// A sweep that marks nothing leaves the log alone and updates the line.
	n := len(s.events.query(time.Now().Add(-time.Hour), []string{evSweep}, 0))
	s.sweepLoopStep(context.Background(), time.Now().Add(2*time.Hour))
	if got := len(s.events.query(time.Now().Add(-time.Hour), []string{evSweep}, 0)); got != n {
		t.Fatalf("a sweep that marked nothing logged an event (%d -> %d)", n, got)
	}
	if line, _, _ := s.sweepBoardLine(); !strings.HasPrefix(line, "Last sweep: marked nothing new") {
		t.Fatalf("line = %q", line)
	}
}

func TestSweepLoopStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweeploop.json")
	l := openSweepLoop(path)
	l.mu.Lock()
	l.st = sweepLoopState{LastRunAt: time.Now().UTC().Truncate(time.Second), Runs: 3, LastMarked: 2, TodoID: "t0123456789", TodoAt: time.Now().UTC().Truncate(time.Second)}
	l.saveLocked()
	l.mu.Unlock()
	got := openSweepLoop(path).get()
	if got.Runs != 3 || got.LastMarked != 2 || got.TodoID != "t0123456789" || got.TodoAt.IsZero() {
		t.Fatalf("reloaded = %+v", got)
	}
}
