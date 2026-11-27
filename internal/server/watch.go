package server

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// The agent-watch pass (#143, slice 2).
//
// The hub used to run seven loops that each looked at the same agent list
// on their own clock: the ledger pump (fleet events and a 30 s tick), auto
// to-do (events and 30 s), the blocked relay (events and 3 s), the quiet
// mark (1 m), the loop nudge (10 s), park suggest (1 m) and the coordinator
// reset (10 m). Now one pass runs their steps, in this order:
//
//  1. report: capture finished workers' reports, and the deliveries that
//     hang off the ledger (callbacks, queued prompts, to-do answers); every
//     30 s also auto-clean and auto-remove;
//  2. quiet: the "working but silent" mark, every quietTick;
//  3. autotodo: blocked and question to-dos, on events and every autoTick,
//     and once when the next agent crosses its threshold before that tick;
//  4. relay: the blocked relay, on events and the tick, and every relayTick
//     while an agent is blocked or an episode is open;
//  5. loop: the loop nudge, every loopTick;
//  6. park: park suggest and auto-park, every parkSugTick;
//  7. reset: the coordinator reset, at start and every resetEvery.
//
// What starts a pass: a fleet "agents" event (the steps that ran on events
// before: report, autotodo, relay), the one shared watchTick (each step whose
// own interval has passed), a one-shot wake a step asked for (the relay's 3 s
// while something is blocked), or a held delivery's retry (typing.go: the
// steps whose notice was held while the user typed). Requests that arrive
// while a pass runs are merged into the next one, which reads the fleet
// afresh.
//
// Each step is time-boxed: the pass waits for it at most box, then goes on
// to the next step while the slow one finishes on its own (its pane reads
// and sends keep their own timeouts). A step still running when the next
// pass reaches it is skipped, and run again once it is done; so a slow step
// never runs twice at once and never holds up the others for longer than
// its box. The thresholds and outcomes of each step are its own, as before;
// only the clocks are shared.

const (
	// watchTick is the one clock for time thresholds.
	watchTick = 10 * time.Second
	// watchBox is how long a pass waits for one step.
	watchBox = 5 * time.Second
)

// watchWhy is what asked for a pass (a set).
type watchWhy uint8

const (
	whyEvent watchWhy = 1 << iota // a fleet "agents" event
	whyTick                       // the shared clock
	whySoon                       // a one-shot wake a step asked for
	whyRetry                      // a held delivery (typing) may go now
)

// Senders, for the retry of a held delivery: each names the step that
// re-sends it.
const (
	senderLedger = "report" // callbacks, queued prompts, to-do answers
	senderRelay  = "relay"
	senderLoop   = "loop"
	senderReset  = "reset"
)

type watchFn func(ctx context.Context, agents []herdr.AgentInfo, now time.Time)

// watchStep is one step of the pass.
type watchStep struct {
	name string
	// on says the step's feature is enabled here.
	on func() bool
	// event runs on a fleet event (nil: the step does not follow events).
	event watchFn
	// every is the step's own interval on the shared clock; tick runs then
	// (nil: event). The first tick runs it at once when atStart is set.
	every   time.Duration
	tick    watchFn
	atStart bool
	// retry re-sends what this step had held for typing (nil: event, else
	// tick).
	retry watchFn

	// under watcher.mu
	last    time.Time // when tick last ran
	running bool
	missed  watchWhy // asked for while it was still running
	slow    bool     // logged as over its box in this run
}

func (st *watchStep) tickFn() watchFn {
	if st.tick != nil {
		return st.tick
	}
	return st.event
}

// retryFn re-sends a held delivery. A step's event or timed run covers its
// retry too (observeLedger delivers; loopStep and resetTick send), so the
// other branches of pass need not add it.
func (st *watchStep) retryFn() watchFn {
	switch {
	case st.retry != nil:
		return st.retry
	case st.event != nil:
		return st.event
	}
	return st.tickFn()
}

// watcher runs the pass. It exists on every server; RunWatch drives it
// from the fleet and the clock, and a held delivery's retry asks it
// directly (also where RunWatch is not running, as in tests).
type watcher struct {
	s     *Server
	steps []*watchStep
	box   time.Duration
	tick  time.Duration
	now   func() time.Time
	ctx   context.Context

	mu      sync.Mutex
	pending watchWhy
	held    map[string]bool // senders to retry
	soon    map[string]bool // steps that asked for a one-shot wake
	passing bool
	wake    *time.Timer // the one-shot wake (nil: none armed)
	wakeAt  time.Time
	// passes and boxed count passes and steps that went over their box
	// (tests and the log).
	passes, boxed int
}

func newWatcher(s *Server) *watcher {
	w := &watcher{s: s, box: watchBox, tick: watchTick, now: time.Now, ctx: context.Background(),
		held: map[string]bool{}, soon: map[string]bool{}}
	w.steps = []*watchStep{
		{
			name:  "report",
			on:    func() bool { return s.ledger != nil },
			event: func(_ context.Context, agents []herdr.AgentInfo, _ time.Time) { s.observeLedger(agents) },
			// Timed work that no fleet update triggers: the look at landed
			// entries.
			every: 30 * time.Second,
			tick: func(ctx context.Context, agents []herdr.AgentInfo, _ time.Time) {
				s.autoCleanup(agents)
				if s.autoRemove {
					s.spawn(func() { s.autoRemoveStep(ctx, time.Now()) })
				}
			},
			// A held delivery's retry (typing.go): the deliveries, not the whole
			// ledger look.
			retry: func(_ context.Context, agents []herdr.AgentInfo, _ time.Time) {
				s.deliverQueue(agents)
				s.deliverCallbacks(agents)
				s.deliverTodoNotices(agents)
			},
		},
		{
			name:  "quiet",
			on:    func() bool { return s.quiet != nil && s.todos != nil },
			every: quietTick,
			tick: func(ctx context.Context, agents []herdr.AgentInfo, _ time.Time) {
				s.quietStep(ctx, agents, s.quiet.now())
			},
		},
		{
			name: "autotodo",
			on:   func() bool { return s.auto != nil && s.todos != nil },
			event: func(ctx context.Context, agents []herdr.AgentInfo, _ time.Time) {
				now := time.Now()
				s.autoTodoStep(ctx, agents, now)
				if d := s.autoNextDue(agents, now); d > 0 && d < autoTick {
					w.wakeIn("autotodo", d+100*time.Millisecond) // past the threshold, not on it
				}
			},
			every: autoTick,
		},
		{
			name: "relay",
			on:   func() bool { return s.relay != nil },
			event: func(_ context.Context, agents []herdr.AgentInfo, _ time.Time) {
				s.relayStep(agents, time.Now())
				if s.relayLive() {
					w.wakeIn("relay", relayTick)
				}
			},
			every: relayTick, // under watchTick: every tick, and the wake above in between
		},
		{
			name:  "loop",
			on:    func() bool { return s.loops != nil },
			every: loopTick,
			tick:  func(ctx context.Context, agents []herdr.AgentInfo, _ time.Time) { s.loopStep(ctx, agents, time.Now()) },
		},
		{
			name:  "park",
			on:    func() bool { return s.parkSug != nil },
			every: parkSugTick,
			tick: func(ctx context.Context, agents []herdr.AgentInfo, _ time.Time) {
				s.parkSuggestStep(ctx, agents, s.parkSug.now())
			},
		},
		{
			name:    "reset",
			on:      func() bool { return s.resets != nil && s.cfg.CoordinatorResetTokens > 0 },
			every:   resetEvery,
			atStart: true,
			tick:    func(ctx context.Context, _ []herdr.AgentInfo, now time.Time) { s.resetTick(ctx, now) },
			// A reset held for typing: try the due one again, without
			// reading every coordinator's context.
			retry: func(ctx context.Context, _ []herdr.AgentInfo, now time.Time) {
				s.resetDue(ctx, now, s.coordinatorAgents())
			},
		},
	}
	return w
}

func (w *watcher) step(name string) *watchStep {
	for _, st := range w.steps {
		if st.name == name {
			return st
		}
	}
	return nil
}

// enabled reports whether any step has its feature on.
func (w *watcher) enabled() bool {
	for _, st := range w.steps {
		if st.on() {
			return true
		}
	}
	return false
}

// RunWatch drives the agent-watch pass from the fleet's events and the one
// shared clock until ctx is done. It returns at once where no step's
// feature is on (a peer: the steps need the hub's ledger).
func (s *Server) RunWatch(ctx context.Context) {
	w := s.watch
	if w == nil || !w.enabled() {
		return
	}
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
	events, cancel := s.fleet.Hub().Subscribe()
	defer cancel()
	tick := time.NewTicker(w.tick)
	defer tick.Stop()
	w.request(whyTick, "") // the steps that run at start (the reset)
	for {
		select {
		case <-ctx.Done():
			w.mu.Lock()
			w.stopWake()
			w.mu.Unlock()
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == "agents" {
				w.request(whyEvent, "")
			}
		case <-tick.C:
			w.request(whyTick, "")
		}
	}
}

// request asks for a pass. key names the sender (whyRetry) or the step
// (whySoon). A pass already running picks it up when it ends.
func (w *watcher) request(why watchWhy, key string) {
	w.mu.Lock()
	w.pending |= why
	switch why {
	case whyRetry:
		w.held[key] = true
	case whySoon:
		w.soon[key] = true
	}
	if w.passing {
		w.mu.Unlock()
		return
	}
	w.passing = true
	w.mu.Unlock()
	w.s.spawn(w.drain)
}

// wakeIn asks for a pass that runs step name in d, unless an earlier wake
// is armed (one timer, whatever asks).
func (w *watcher) wakeIn(name string, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx.Err() != nil {
		return
	}
	w.soon[name] = true
	at := time.Now().Add(d)
	if w.wake != nil && !w.wakeAt.After(at) {
		return
	}
	w.stopWake()
	w.wakeAt = at
	w.s.bg.Add(1) // counted like any background work (waitBackground)
	w.wake = time.AfterFunc(d, func() {
		defer w.s.bg.Done()
		w.mu.Lock()
		w.wake = nil
		w.mu.Unlock()
		w.request(whySoon, "")
	})
}

// stopWake disarms the one-shot wake. Callers hold w.mu.
func (w *watcher) stopWake() {
	if w.wake != nil && w.wake.Stop() {
		w.s.bg.Done() // it will not fire
	}
	w.wake = nil
}

// drain runs passes until no request is pending.
func (w *watcher) drain() {
	for {
		w.mu.Lock()
		why, held, soon := w.pending, w.held, w.soon
		if why == 0 {
			w.passing = false
			w.mu.Unlock()
			return
		}
		w.pending = 0
		if why&whyRetry != 0 {
			w.held = map[string]bool{}
		}
		if why&whySoon != 0 {
			w.soon = map[string]bool{}
		}
		w.passes++
		w.mu.Unlock()
		w.pass(why, held, soon)
	}
}

// pass runs the steps, in order, that why asks for.
func (w *watcher) pass(why watchWhy, held, soon map[string]bool) {
	agents := w.s.fleet.Agents()
	now := w.now()
	w.mu.Lock()
	ctx := w.ctx
	w.mu.Unlock()
	for _, st := range w.steps {
		if !st.on() {
			continue
		}
		w.mu.Lock()
		tickDue := why&whyTick != 0 && st.every > 0 && w.tickDue(st, now)
		var fn watchFn
		var asked watchWhy
		switch {
		case why&whyEvent != 0 && st.event != nil:
			fn, asked = st.event, whyEvent
			if tickDue && st.tick != nil {
				// Its timed work is due too: after the event part.
				ev, tk := st.event, st.tick
				fn = func(ctx context.Context, agents []herdr.AgentInfo, now time.Time) {
					ev(ctx, agents, now)
					tk(ctx, agents, now)
				}
			}
		case tickDue:
			fn, asked = st.tickFn(), whyTick
		case why&whySoon != 0 && soon[st.name] && st.event != nil:
			fn, asked = st.event, whySoon
		case why&whyRetry != 0 && held[st.name]:
			fn, asked = st.retryFn(), whyRetry
		}
		if fn == nil {
			w.mu.Unlock()
			continue
		}
		if st.running {
			// Still busy with an earlier pass: asked again once it is done
			// (a tick it misses waits for the next tick).
			st.missed |= asked
			if why&whyRetry != 0 && held[st.name] {
				st.missed |= whyRetry
			}
			w.mu.Unlock()
			continue
		}
		st.running, st.slow = true, false
		if tickDue {
			st.last = now
		}
		w.mu.Unlock()
		w.run(ctx, st, fn, agents, now)
	}
}

// tickDue says the shared clock has reached st's own interval. A step with
// an interval no longer than the clock's runs every tick; the others count
// from their first tick (as their own tickers did), except a step that ran
// at start. Callers hold w.mu.
func (w *watcher) tickDue(st *watchStep, now time.Time) bool {
	switch {
	case st.every <= w.tick:
		return true
	case st.last.IsZero() && st.atStart:
		return true
	case st.last.IsZero():
		st.last = now
		return false
	}
	// A second of slack: the clock's ticks are not exactly every.
	return now.Sub(st.last) >= st.every-time.Second
}

// run runs one step and waits for it at most the box.
func (w *watcher) run(ctx context.Context, st *watchStep, fn watchFn, agents []herdr.AgentInfo, now time.Time) {
	done := make(chan struct{})
	w.s.spawn(func() {
		defer close(done)
		fn(ctx, agents, now)
		w.mu.Lock()
		st.running = false
		missed := st.missed
		st.missed = 0
		slow := st.slow
		w.mu.Unlock()
		if slow {
			log.Printf("agent watch: step %s finished after its %s box", st.name, w.box)
		}
		if ctx.Err() == nil {
			// What came in while it ran: a fresh pass applies the latest
			// fleet (a tick it missed waits for the next tick).
			if missed&whyEvent != 0 {
				w.request(whyEvent, "")
			}
			if missed&whySoon != 0 {
				w.request(whySoon, st.name)
			}
			if missed&whyRetry != 0 {
				w.request(whyRetry, st.name)
			}
		}
	})
	t := time.NewTimer(w.box)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		w.mu.Lock()
		st.slow = true
		w.boxed++
		w.mu.Unlock()
		log.Printf("agent watch: step %s is over its %s box; the pass goes on without it", st.name, w.box)
	}
}
