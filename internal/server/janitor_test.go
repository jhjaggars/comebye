package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// onlyStages keeps the janitor's stages named, in their order.
func onlyStages(j *janitor, names ...string) {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	var out []*janStage
	for _, st := range j.stages {
		if keep[st.name] {
			out = append(out, st)
		}
	}
	j.stages = out
}

// One host review per candidate per pass: a session both auto-remove and
// sweep-clean look at in one pass is reviewed on its host once. Here
// auto-remove reviews it first and declines (its agent is gone, so there is
// no session to keep a record of); sweep-clean then takes that review and
// removes it, since the hub's own proof decides for a gone agent.
func TestJanitorReviewsEachCandidateOncePerPass(t *testing.T) {
	f := newGoneFix(t, true, "")
	if _, _, err := f.s.ledger.Update(f.e.ID, func(e *ledger.Entry) {
		e.Mark.Ask = &ledger.Ask{State: ledger.AskSafe, Reason: "nothing left", At: time.Now().UTC()}
	}); err != nil {
		t.Fatal(err)
	}
	f.s.autoRemove = true
	j := newJanitor(f.s)
	onlyStages(j, "autoremove", "sweep")
	j.pass(ctx())
	waitStages(t, j)
	if j.reviews != 1 || j.reused != 1 {
		t.Fatalf("host reviews = %d, reused = %d; want 1 and 1", j.reviews, j.reused)
	}
	if f.status(t) != ledger.Removed {
		t.Fatalf("status = %s, want removed by the sweep's clean-up", f.status(t))
	}
	// The next pass reviews afresh (nothing is left to review here).
	j.pass(ctx())
	waitStages(t, j)
	if j.reviews != 1 {
		t.Fatalf("host reviews = %d after a pass with nothing marked", j.reviews)
	}
}

// Outside a janitor pass (the review screen, `comebye sweep`), reviewOnce
// is a plain review every time.
func TestReviewOnceOutsideAPassReviews(t *testing.T) {
	f := newGoneFix(t, true, "")
	j := newJanitor(f.s)
	f.s.janitor = j
	for range 2 {
		for _, it := range f.s.marked() {
			if got := f.s.reviewOnce(ctx(), it); got != it || got.Checks == nil {
				t.Fatalf("not reviewed: %+v", got)
			}
		}
	}
	if j.reviews != 0 || j.reused != 0 {
		t.Fatalf("counted %d/%d outside a pass", j.reviews, j.reused)
	}
}

func waitStages(t *testing.T, j *janitor) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		j.mu.Lock()
		for _, st := range j.stages {
			busy = busy || st.running
		}
		j.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a stage is still running")
}

// A slow stage holds up the others no longer than its box, is not started
// again while it runs, and runs again once it is done.
func TestJanitorSlowStageStaysInItsBox(t *testing.T) {
	s, _, _ := coordServer(t)
	j := newJanitor(s)
	j.box = 50 * time.Millisecond
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	slowRuns := 0
	on := func() bool { return true }
	j.stages = []*janStage{
		{name: "slow", on: on, run: func(context.Context, time.Time) {
			mu.Lock()
			slowRuns++
			order = append(order, "slow")
			mu.Unlock()
			<-release
		}},
		{name: "next", on: on, run: func(context.Context, time.Time) {
			mu.Lock()
			order = append(order, "next")
			mu.Unlock()
		}},
	}
	t0 := time.Now()
	j.pass(context.Background())
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("the pass waited %s for a slow stage, box %s", d, j.box)
	}
	j.pass(context.Background()) // slow is still running: skipped
	mu.Lock()
	if slowRuns != 1 || len(order) != 3 || order[1] != "next" || order[2] != "next" {
		t.Fatalf("runs = %d, order = %v", slowRuns, order)
	}
	mu.Unlock()
	if j.boxed != 1 {
		t.Fatalf("boxed = %d", j.boxed)
	}
	close(release)
	waitStages(t, j)
	j.pass(context.Background())
	waitStages(t, j)
	mu.Lock()
	defer mu.Unlock()
	if slowRuns != 2 {
		t.Fatalf("slow ran %d times after it finished, want 2", slowRuns)
	}
}

// Each stage keeps its own cadence on the one-minute clock.
func TestJanitorStagesKeepTheirClocks(t *testing.T) {
	s, _, _ := coordServer(t)
	s.cfg.ParkRetention = 24 * time.Hour
	s.cfg.ScratchDir, s.cfg.ScratchEvery = t.TempDir(), time.Hour
	j := newJanitor(s)
	runs := map[string]int{}
	var mu sync.Mutex
	for _, st := range j.stages {
		name := st.name
		st.run = func(context.Context, time.Time) { mu.Lock(); runs[name]++; mu.Unlock() }
		st.on = func() bool { return true }
	}
	t0 := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	for i := 0; i <= 120; i++ { // the start, then a tick a minute for two hours
		now := t0.Add(time.Duration(i) * time.Minute)
		j.now = func() time.Time { return now }
		j.pass(context.Background())
		waitStages(t, j)
	}
	want := map[string]int{
		"autoclean":  121, // every tick (each entry at most every 3 minutes)
		"autoremove": 121, // every tick (each worker at most every 3 minutes)
		"sweep":      121, // every tick: the sweep is hourly and the clean-up every 20 minutes inside it
		"archive":    121, // every tick: it acts after a sweep
		"autopark":   120, // every parkSugTick, from its first minute
		"parkretain": 2,   // hourly
		"reask":      60,  // every 2 minutes
		"scratch":    2,   // 2 minutes after the start, then hourly
		"ship":       121, // every tick: each child keeps its own interval (archiveship.go)
	}
	mu.Lock()
	defer mu.Unlock()
	for name, n := range want {
		if runs[name] != n {
			t.Errorf("%s ran %d times in two hours, want %d", name, runs[name], n)
		}
	}
}

// The archive of finished work never runs while the sweep stage (its clean-up)
// is still going: it waits for a later pass.
func TestJanitorArchiveWaitsForTheSweep(t *testing.T) {
	s, _, _ := coordServer(t)
	j := newJanitor(s)
	j.box = 20 * time.Millisecond
	release := make(chan struct{})
	ran := map[string]int{}
	var mu sync.Mutex
	on := func() bool { return true }
	j.stages = []*janStage{
		{name: "sweep", on: on, run: func(context.Context, time.Time) { mu.Lock(); ran["sweep"]++; mu.Unlock(); <-release }},
		{name: "archive", on: on, after: "sweep", run: func(context.Context, time.Time) { mu.Lock(); ran["archive"]++; mu.Unlock() }},
	}
	j.pass(context.Background())
	j.pass(context.Background())
	mu.Lock()
	if ran["sweep"] != 1 || ran["archive"] != 0 {
		t.Fatalf("ran %v while the sweep was going", ran)
	}
	mu.Unlock()
	close(release)
	waitStages(t, j)
	j.pass(context.Background())
	waitStages(t, j)
	mu.Lock()
	defer mu.Unlock()
	if ran["archive"] != 1 {
		t.Fatalf("archive ran %d times once the sweep was done", ran["archive"])
	}
}
