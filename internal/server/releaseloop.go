package server

import (
	"context"
	"log"
	"sync"
	"time"
)

// The release loop (#143, slice 6).
//
// Three loops used to drive a release, each on its own clock and each
// asking every peer for its release status (relHosts) on its own: the
// rollout engine (5 s, with a done job's catch-up every minute and the
// hub's retry), the automatic rollout (a minute, with its own catch-up at
// most every 5 minutes), and the release sweep (30 s). Now one loop runs
// them as ordered steps, on one clock:
//
//  1. rollout: the engine's tick (rolloutEngine.tick): a running job
//     advances (canary, verify, soak, the rest, the hub), a job stopped at
//     the hub retries it once the release host answers, and otherwise the
//     one catch-up looks for hosts behind (rolloutEngine.catchUp, with the
//     automatic rollout's say in it: Server.catchUpPlan);
//  2. auto: start the automatic rollout of a green, approved tag
//     (autoRolloutTick), every minute;
//  3. sweep: close release items whose rollout finished (sweepReleases),
//     every 30 s.
//
// The loop ticks every 5 s while a rollout is live and every 30 s
// otherwise; each step keeps its own cadence, and the catch-up keeps its
// own clock (a minute while a skipped host is away, 5 minutes otherwise). A
// step is time-boxed as the janitor's stages are: the pass waits for it at
// most releaseBox, then goes on while the slow one finishes by itself, and
// a step still running when the next pass reaches it is skipped.
//
// The steps share one fleet answer: relHostsCached keeps the last relHosts
// answer for relHostsTTL, a little under the live tick, so each tick of a
// live rollout asks the peers once and afresh, and the steps after the
// engine's use the same answer. Starting an upgrade or a deploy drops it
// (dropRelHosts): what the fleet said before is out of date at once.

const (
	// releaseIdleTick: the loop's clock while no rollout is live.
	releaseIdleTick = releaseSweepEvery
	// releaseBox is how long a pass waits for one step.
	releaseBox = 10 * time.Second
	// relHostsTTL: how long a relHosts answer is reused (-test-mode: a
	// little under its tick).
	relHostsTTL = 4 * time.Second
)

type relStep struct {
	name  string
	every time.Duration // 0: every pass
	run   func(ctx context.Context)

	// under releaseLoop.mu
	next    time.Time
	running bool
	runs    int
}

// releaseLoop runs the steps. RunRelease drives it from the clock; tests
// call pass directly.
type releaseLoop struct {
	s     *Server
	steps []*relStep
	box   time.Duration
	// live and idle are the clock while a rollout is live and otherwise.
	live, idle time.Duration
	now        func() time.Time

	mu sync.Mutex
	// passes and boxed count passes and steps that went over their box (tests).
	passes, boxed int
}

func newReleaseLoop(s *Server) *releaseLoop {
	l := &releaseLoop{s: s, box: releaseBox, live: rolloutTick, idle: releaseIdleTick, now: time.Now}
	auto := autoRolloutEvery
	if s.cfg.TestMode {
		auto = autoRolloutTestEvery
	}
	if s.rollout != nil && s.rollout.tickEvery > 0 {
		l.live = s.rollout.tickEvery
	}
	if s.cfg.TestMode {
		l.idle = l.live // the browser tests wait seconds, not half a minute
	}
	l.steps = []*relStep{
		{name: "rollout", run: func(ctx context.Context) {
			if s.rollout != nil {
				s.rollout.tick(ctx)
			}
		}},
		{name: "auto", every: auto, run: func(ctx context.Context) { s.autoRolloutTick(ctx) }},
		{name: "sweep", every: releaseSweepEvery, run: func(ctx context.Context) { s.sweepReleases(ctx) }},
	}
	return l
}

func (l *releaseLoop) step(name string) *relStep {
	for _, st := range l.steps {
		if st.name == name {
			return st
		}
	}
	return nil
}

// RunRelease runs the release loop until ctx ends. Only the ledger host has
// a rollout engine: elsewhere it returns at once.
func (s *Server) RunRelease(ctx context.Context) {
	l := s.relLoop
	if l == nil || s.rollout == nil || s.fleet == nil || s.todos == nil {
		return
	}
	s.rollout.resumeAfterRestart()
	t := time.NewTimer(l.every())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.pass(ctx)
			t.Reset(l.every())
		}
	}
}

// every is the time to the next pass: the live tick while a rollout is
// live, else the idle one.
func (l *releaseLoop) every() time.Duration {
	// Peek: a tick holds the engine's lock across its calls out, and the
	// clock must not wait for it (the file is written atomically).
	if j, ok := l.s.rollout.Peek(); ok && j.live() {
		return l.live
	}
	return l.idle
}

// pass runs the steps that are due, in order.
func (l *releaseLoop) pass(ctx context.Context) {
	now := l.now()
	l.mu.Lock()
	l.passes++
	l.mu.Unlock()
	for _, st := range l.steps {
		if ctx.Err() != nil {
			return
		}
		l.mu.Lock()
		if st.next.IsZero() && st.every > 0 {
			st.next = now.Add(st.every) // a step starts as its own ticker did
		}
		// A second of slack: the passes are not exactly on time.
		due := st.every <= 0 || !now.Add(time.Second).Before(st.next)
		if !due || st.running {
			l.mu.Unlock()
			continue
		}
		st.running = true
		st.runs++
		if st.every > 0 {
			st.next = now.Add(st.every)
		}
		l.mu.Unlock()
		l.run(ctx, st)
	}
}

// run runs one step and waits for it at most the box.
func (l *releaseLoop) run(ctx context.Context, st *relStep) {
	done := make(chan struct{})
	l.s.spawn(func() {
		defer close(done)
		defer func() {
			l.mu.Lock()
			st.running = false
			l.mu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("release loop: %s: %v", st.name, r)
			}
		}()
		st.run(ctx)
	})
	t := time.NewTimer(l.box)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		l.mu.Lock()
		l.boxed++
		l.mu.Unlock()
	}
}

// ---- the one fleet answer ----

// relHostsCache is the last relHosts answer the release loop's steps share.
type relHostsCache struct {
	fetch sync.Mutex // one fan-out at a time: a second caller waits and reuses it
	mu    sync.Mutex
	ttl   time.Duration
	gen   uint64 // bumped by drop: an answer asked for before it is not kept
	at    time.Time
	hosts []relHost
	asks  int // fan-outs made (tests)
}

func (c *relHostsCache) get(now time.Time) ([]relHost, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosts != nil && now.Sub(c.at) < c.ttl {
		return cloneHosts(c.hosts), c.gen, true
	}
	return nil, c.gen, false
}

func cloneHosts(h []relHost) []relHost { return append([]relHost(nil), h...) }

// relHostsCached is relHosts, reused for relHostsTTL: the steps of one
// release-loop pass ask every peer once between them. The caller gets its
// own copy, which it may mark.
func (s *Server) relHostsCached(ctx context.Context) []relHost {
	c := &s.relCache
	if h, _, ok := c.get(time.Now()); ok {
		return h
	}
	c.fetch.Lock()
	defer c.fetch.Unlock()
	h, gen, ok := c.get(time.Now())
	if ok {
		return h // another caller asked while this one waited
	}
	hosts := s.relHosts(ctx)
	c.mu.Lock()
	c.asks++
	if c.gen == gen && ctx.Err() == nil {
		c.hosts, c.at = cloneHosts(hosts), time.Now()
	}
	c.mu.Unlock()
	return hosts
}

// dropRelHosts forgets the cached answer: an upgrade or a deploy was just
// started, so the next step asks the fleet afresh.
func (s *Server) dropRelHosts() {
	c := &s.relCache
	c.mu.Lock()
	c.gen++
	c.hosts = nil
	c.mu.Unlock()
}
