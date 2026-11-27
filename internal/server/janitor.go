package server

import (
	"context"
	"log"
	"sync"
	"time"
)

// The janitor (#143, slice 4).
//
// The hub used to tidy up on six clocks of its own: auto-clean and
// auto-remove on the ledger pump's 30 s tick (and every fleet event), the
// sweep loop (1 m, with the sweep hourly, sweep-clean every 20 m and the
// archive of finished work after each sweep), park retention (1 h), the
// re-ask of stale UNSAFE answers (2 m) and the scratch prune (1 h); and
// outside the binary a desktop timer shipped session files to the archive
// every 5 minutes while the hub pod's sidecar rebuilt the recall index
// hourly. Now one loop runs them as stages, in this order, on one tick
// (janitorTick, a minute):
//
//  1. autoclean: mark landed, CI-green, clean handoffs ready to remove;
//  2. autoremove: remove marked workers that answered SAFE;
//  3. sweep: the sweep (hourly) and sweep-clean (every 20 m, and after a
//     sweep);
//  4. archive: put away finished work idle for 7 days (3 for research-type
//     work), after each sweep;
//  5. autopark: park agents idle for a day with a clean worktree;
//  6. parkretain: propose removing parked agents past retention (hourly);
//  7. reask: ask again where an UNSAFE answer went stale (every 2 m);
//  8. scratch: prune the temporary directory (hourly);
//  9. ship: copy idle session files to the archive and, on the hub, build
//     the all-hosts recall index (archiveship.go).
//
// Each stage keeps its own cadence and guards; only the clock is shared. A
// stage is time-boxed as the agent-watch pass's steps are: the pass waits
// for it at most box, then goes on while the slow one finishes by itself; a
// stage still running when the next pass reaches it is skipped, so no stage
// runs twice at once or holds up the others for longer than its box.
//
// One host review per candidate per pass: auto-remove and sweep-clean both
// review a marked session on its host right before removing it. In one pass
// the second of them reuses the first one's review (reviewOnce) rather than
// asking the host again, as long as it is fresh (janitorReviewFresh); the
// removal itself still goes through the host's own checks.

const (
	// janitorTick is the one clock (-test-mode makes it a second).
	janitorTick = sweepLoopTick
	// janitorBox is how long a pass waits for one stage.
	janitorBox = 10 * time.Second
	// janitorReviewFresh is how old a review a later stage of the same pass
	// may reuse; older, the session is reviewed again.
	janitorReviewFresh = 2 * time.Minute
)

type janitorFn func(ctx context.Context, now time.Time)

// janStage is one stage of the janitor.
type janStage struct {
	name string
	// on says the stage's feature is enabled here.
	on func() bool
	// every is the stage's own interval (0: every tick); first is the wait
	// before its first run (0: every, so a stage starts as its own ticker
	// did).
	every, first time.Duration
	run          janitorFn
	// long: the stage normally runs past its box (a sweep, the archive
	// copy); that is not logged.
	long bool
	// after names a stage this one never overlaps: while it runs, this one
	// waits for a later pass (the archive of finished work and the sweep's
	// clean-up both act on marked sessions, and ran one after the other).
	after string

	// under janitor.mu
	next    time.Time
	running bool
	slow    bool
	runs    int
}

// janitor runs the stages. RunJanitor drives it from the clock; tests call
// pass directly.
type janitor struct {
	s      *Server
	stages []*janStage
	box    time.Duration
	tick   time.Duration
	now    func() time.Time

	mu      sync.Mutex
	passing bool
	// swept: a sweep ran since the archive stage last looked.
	swept bool
	// passes and boxed count passes and stages that went over their box;
	// reviews counts host reviews made through reviewOnce, reused those
	// that a stage took from an earlier one in the same pass (tests).
	passes, boxed, reviews, reused int
}

func newJanitor(s *Server) *janitor {
	j := &janitor{s: s, box: janitorBox, tick: janitorTick, now: time.Now}
	if s.cfg.TestMode {
		j.tick = time.Second // as the sweep loop's clocks there
	}
	ledger := func() bool { return s.ledger != nil }
	j.stages = []*janStage{
		{name: "autoclean", on: ledger, run: func(context.Context, time.Time) { s.autoCleanup(s.fleet.Agents()) }},
		{
			name: "autoremove",
			on:   func() bool { return s.ledger != nil && s.autoRemove },
			run:  func(ctx context.Context, now time.Time) { s.autoRemoveStep(ctx, now) },
		},
		{
			name: "sweep", long: true,
			on: func() bool { return s.ledger != nil && s.sweeps != nil },
			run: func(ctx context.Context, now time.Time) {
				if s.sweepLoopStep(ctx, now) {
					j.mu.Lock()
					j.swept = true
					j.mu.Unlock()
				}
			},
		},
		{
			name: "archive", long: true, after: "sweep",
			on: func() bool { return s.ledger != nil && s.sweeps != nil && s.sweepLoopConf().On },
			run: func(ctx context.Context, now time.Time) {
				// After each sweep, as it always ran (or every tick when the dev
				// harness shortens the clocks).
				j.mu.Lock()
				due := j.swept || s.sweeps.archiveAge > 0
				j.swept = false
				j.mu.Unlock()
				if due {
					s.recordDone(now, s.archiveStep(ctx, now))
				}
			},
		},
		{
			name:  "autopark",
			on:    func() bool { return s.parkSug != nil && s.parkSug.auto },
			every: parkSugTick,
			run: func(ctx context.Context, _ time.Time) {
				s.autoParkStep(ctx, s.fleet.Agents(), s.parkSug.now())
			},
		},
		{
			name:  "parkretain",
			on:    func() bool { return s.ledger != nil && s.cfg.ParkRetention > 0 },
			every: parkRetentionTick,
			run:   func(_ context.Context, now time.Time) { s.parkRetentionStep(now) },
		},
		{
			name:  "reask",
			on:    func() bool { return s.ledger != nil && s.reaskEvery > 0 },
			every: s.reaskEvery,
			run:   func(ctx context.Context, now time.Time) { s.reaskStep(ctx, now) },
		},
		{
			name:  "scratch",
			on:    func() bool { return s.cfg.ScratchDir != "" && s.cfg.ScratchEvery > 0 && s.client != nil },
			every: s.cfg.ScratchEvery, first: scratchFirstRun,
			run: func(ctx context.Context, _ time.Time) { s.scratchStep(ctx) },
		},
		{
			name: "ship", long: true,
			on:  func() bool { return s.ship != nil },
			run: func(ctx context.Context, now time.Time) { s.ship.step(ctx, now) },
		},
	}
	return j
}

func (j *janitor) stage(name string) *janStage {
	for _, st := range j.stages {
		if st.name == name {
			return st
		}
	}
	return nil
}

func (j *janitor) enabled() bool {
	for _, st := range j.stages {
		if st.on() {
			return true
		}
	}
	return false
}

// RunJanitor runs a pass on every tick until ctx is done. It returns at once
// where no stage's feature is on.
func (s *Server) RunJanitor(ctx context.Context) {
	j := s.janitor
	if j == nil || !j.enabled() {
		return
	}
	tick := time.NewTicker(j.tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			j.mu.Lock()
			busy := j.passing
			j.passing = true
			j.mu.Unlock()
			if busy {
				continue // the last pass is still waiting on a box: the next tick
			}
			j.pass(ctx)
			j.mu.Lock()
			j.passing = false
			j.mu.Unlock()
		}
	}
}

// pass runs the stages that are due, in order.
func (j *janitor) pass(ctx context.Context) {
	now := j.now()
	p := &janPass{reviews: map[string]*janReview{}, j: j}
	ctx = context.WithValue(ctx, janPassKey{}, p)
	j.mu.Lock()
	j.passes++
	j.mu.Unlock()
	for _, st := range j.stages {
		if ctx.Err() != nil {
			return
		}
		if !st.on() {
			continue
		}
		j.mu.Lock()
		if st.next.IsZero() && st.every > 0 {
			first := st.first
			if first <= 0 {
				first = st.every
			}
			st.next = now.Add(first)
		}
		// A second of slack: the ticks are not exactly a minute apart.
		due := st.every <= 0 || !now.Add(time.Second).Before(st.next)
		if !due || st.running || (st.after != "" && j.stage(st.after) != nil && j.stage(st.after).running) {
			j.mu.Unlock()
			continue
		}
		st.running, st.slow = true, false
		st.runs++
		if st.every > 0 {
			st.next = now.Add(st.every)
		}
		j.mu.Unlock()
		j.run(ctx, st, now)
	}
}

// run runs one stage and waits for it at most the box.
func (j *janitor) run(ctx context.Context, st *janStage, now time.Time) {
	done := make(chan struct{})
	j.s.spawn(func() {
		defer close(done)
		st.run(ctx, now)
		j.mu.Lock()
		st.running = false
		slow := st.slow
		j.mu.Unlock()
		if slow && !st.long {
			log.Printf("janitor: stage %s finished after its %s box", st.name, j.box)
		}
	})
	t := time.NewTimer(j.box)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		j.mu.Lock()
		st.slow = true
		j.boxed++
		j.mu.Unlock()
		if !st.long {
			log.Printf("janitor: stage %s is over its %s box; the pass goes on without it", st.name, j.box)
		}
	}
}

// janPass is what the stages of one pass share: the host reviews made so
// far.
type janPass struct {
	j       *janitor
	mu      sync.Mutex
	reviews map[string]*janReview
}

type janReview struct {
	done chan struct{}
	it   *removal
	at   time.Time
}

type janPassKey struct{}

// reviewOnce reviews a marked session on its host, with the comparison
// against origin, and returns the reviewed item. Inside a janitor pass a
// second stage asking for the same session (and the same mark) gets the
// first one's review, waiting for it if it is still running, unless it is
// older than janitorReviewFresh. Outside a pass it is plain review.
func (s *Server) reviewOnce(ctx context.Context, it *removal) *removal {
	it.opts.Origin = true
	p, _ := ctx.Value(janPassKey{}).(*janPass)
	if p == nil {
		s.review(ctx, it)
		return it
	}
	key := it.ID + "\x00" + it.Mark.At.UTC().Format(time.RFC3339Nano)
	p.mu.Lock()
	if r, ok := p.reviews[key]; ok {
		p.mu.Unlock()
		select {
		case <-r.done:
		case <-ctx.Done():
			return it // not reviewed: no verdict can pass on it
		}
		if time.Since(r.at) < janitorReviewFresh {
			p.j.mu.Lock()
			p.j.reused++
			p.j.mu.Unlock()
			// A copy: a stage sets fields of its own (recordPark) on the item.
			c := *r.it
			c.recordPark = ""
			return &c
		}
		p.mu.Lock()
	}
	r := &janReview{done: make(chan struct{}), it: it}
	p.reviews[key] = r
	p.mu.Unlock()
	s.review(ctx, it)
	r.at = time.Now()
	p.j.mu.Lock()
	p.j.reviews++
	p.j.mu.Unlock()
	close(r.done)
	return it
}
