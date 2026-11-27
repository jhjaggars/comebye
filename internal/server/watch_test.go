package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The agent-watch pass (watch.go) and the one sender path for held
// messages (typing.go).

func on() bool { return true }

// A slow step (a pane read that hangs) holds up the steps after it for no
// longer than its box; it is not run twice at once, and what it missed
// while it ran is run again once it is done.
func TestWatchSlowStepDoesNotHoldTheOthers(t *testing.T) {
	s, _, _ := coordServer(t)
	w := s.watch
	w.box = 100 * time.Millisecond
	release := make(chan struct{})
	var mu sync.Mutex
	slowRuns, fastRuns := 0, 0
	var fastAt []time.Time
	w.steps = []*watchStep{
		{name: "slow", on: on, event: func(context.Context, []herdr.AgentInfo, time.Time) {
			mu.Lock()
			slowRuns++
			mu.Unlock()
			<-release
		}},
		{name: "fast", on: on, event: func(context.Context, []herdr.AgentInfo, time.Time) {
			mu.Lock()
			fastRuns++
			fastAt = append(fastAt, time.Now())
			mu.Unlock()
		}},
	}
	counts := func() (int, int) { mu.Lock(); defer mu.Unlock(); return slowRuns, fastRuns }
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	t0 := time.Now()
	w.request(whyEvent, "")
	waitFor(t, func() bool { _, f := counts(); return f == 1 })
	mu.Lock()
	took := fastAt[0].Sub(t0)
	mu.Unlock()
	if took > w.box+time.Second {
		t.Fatalf("the fast step ran %s after the pass began; its box is %s", took, w.box)
	}
	if sl, _ := counts(); sl != 1 {
		t.Fatalf("slow runs = %d", sl)
	}

	// The slow step is still running: the next pass skips it and the fast
	// step runs at once (no box to wait for).
	t1 := time.Now()
	w.request(whyEvent, "")
	waitFor(t, func() bool { _, f := counts(); return f == 2 })
	mu.Lock()
	took = fastAt[1].Sub(t1)
	mu.Unlock()
	if sl, _ := counts(); sl != 1 {
		t.Fatalf("a running step was started again: slow runs = %d", sl)
	}
	if took > w.box {
		t.Fatalf("a pass waited %s for a step that was still running", took)
	}
	w.mu.Lock()
	boxed := w.boxed
	w.mu.Unlock()
	if boxed != 1 {
		t.Fatalf("boxed = %d, want 1", boxed)
	}

	// Done: the event it missed runs it again, on a fresh pass.
	close(release)
	waitFor(t, func() bool { sl, _ := counts(); return sl == 2 })
}

// Each step keeps its own clock on the shared tick: over ten minutes of
// 10 s ticks, each runs as often as its own ticker did.
func TestWatchStepsKeepTheirClocks(t *testing.T) {
	s, _, _ := coordServer(t)
	w := s.watch
	real := newWatcher(s).steps
	var mu sync.Mutex
	runs := map[string]int{}
	w.steps = nil
	for _, st := range real {
		name := st.name
		count := func(context.Context, []herdr.AgentInfo, time.Time) { mu.Lock(); runs[name]++; mu.Unlock() }
		cp := &watchStep{name: name, on: on, every: st.every, atStart: st.atStart}
		if st.event != nil {
			cp.event = count
		}
		if st.tick != nil {
			cp.tick = count
		}
		w.steps = append(w.steps, cp)
	}
	t0 := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	for i := 0; i <= 60; i++ { // the start, then a tick every 10 s for 10 minutes
		now := t0.Add(time.Duration(i) * watchTick)
		w.now = func() time.Time { return now }
		w.pass(whyTick, nil, nil)
	}
	want := map[string]int{
		"report":   20, // every 30 s
		"quiet":    10, // every minute
		"autotodo": 20, // every 30 s (and on events)
		"relay":    61, // every tick (and every 3 s while something is blocked)
		"loop":     61, // every 10 s
		"park":     10, // every minute
		"reset":    2,  // at start, then every 10 minutes
	}
	mu.Lock()
	defer mu.Unlock()
	for name, n := range want {
		if runs[name] != n {
			t.Errorf("%s ran %d times in 10 minutes, want %d", name, runs[name], n)
		}
	}
	// Fleet events run only the steps that followed events before.
	for k := range runs {
		delete(runs, k)
	}
	mu.Unlock()
	w.pass(whyEvent, nil, nil)
	mu.Lock()
	if len(runs) != 3 || runs["report"] != 1 || runs["autotodo"] != 1 || runs["relay"] != 1 {
		t.Errorf("an event ran %v, want report, autotodo and relay", runs)
	}
}

// heldSenders lists the watch steps a held message waits on.
func heldSenders(s *Server) map[string]bool {
	t := s.typing
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]bool{}
	for _, h := range t.held {
		for k := range h.senders {
			out[k] = true
		}
	}
	return out
}

// No sender types into a box with text in it: callbacks, queued prompts,
// to-do answers, blocked relays, loop nudges and the asks that wait in place
// are all held by the one guard, and each is noted for the one retry.
func TestEverySenderHeldWhileUserTypes(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.typing.poll = time.Hour // no retry inside this test: only the holds
	showScreen(fh, "w1:p1", capture(t, "claude_typed.ansi"))
	coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	if err := s.recordRole(coord.ID, "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}

	// A callback.
	e := cbFinished(t, s, &ledger.Ref{AgentID: coord.ID, Name: "coordinator"}, coord)
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	// A queued prompt.
	if _, _, err := s.pq.add(QueuedPrompt{Agent: coord.ID, Name: "coordinator", Text: "please rebase", From: "comebye"}); err != nil {
		t.Fatal(err)
	}
	s.deliverQueue([]herdr.AgentInfo{coord})
	// A to-do answer.
	it := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"q?","no_wait":true}`).Body.Bytes())
	if w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"yes"}`); w.Code != 200 {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
	s.deliverTodoNotices([]herdr.AgentInfo{coord})
	// A blocked relay.
	s.initBlockedRelay(DefaultRelayBlocked)
	ep := &relayEp{id: "desk/w2:p1", to: ledger.Ref{AgentID: coord.ID}, inFlight: true, tries: time.Now()}
	s.relaySend(ep, coord, "Comebye (hub notice): the worker is blocked", false)
	// A loop nudge (a local agent: deliverOrQueue) and a dead-loop notice.
	s.initLoopNudge(DefaultLoopNudgeAfter, DefaultLoopNudgeAgain)
	start := time.Now()
	nerr := s.loops.send(t.Context(), coord, "Comebye (hub notice): your loop is idle")
	if !isInputBusy(nerr) {
		t.Errorf("loop nudge: err = %v, want input_busy", nerr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the loop nudge waited in place for %s", d)
	}
	if _, err := s.loopNotifyRequester(t.Context(), ledger.Entry{ReplyTo: &ledger.Ref{AgentID: coord.ID}}, "Comebye (hub notice): the loop is dead"); !isInputBusy(err) {
		t.Errorf("dead-loop notice: err = %v, want input_busy", err)
	}
	// An ask or a brief, waiting in place.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := s.promptHeld(ctx, coord, "please answer"); err == nil || !strings.Contains(err.Error(), "held") {
		t.Errorf("promptHeld: err = %v", err)
	}

	waitFor(t, func() bool {
		g, _ := s.todos.get(it.ID)
		l := s.pq.list("", false)
		return len(l) == 1 && l[0].Held != "" && g.Notice != nil && heldSenders(s)[senderLedger]
	})
	time.Sleep(100 * time.Millisecond) // the callback and to-do senders run in the background
	if c := fh.call("agent.prompt"); c != nil {
		t.Fatalf("typed over the user's text: %v", c.Params)
	}
	if g, _ := s.ledger.Get(e.ID); g.Callback.State != ledger.CallbackPending || g.Callback.Tries != 0 {
		t.Errorf("callback = %+v", g.Callback)
	}
	if g, _ := s.todos.get(it.ID); g.Notice.State != todoNoticePending {
		t.Errorf("to-do notice = %+v", g.Notice)
	}
	if ep.openSent || !ep.tries.IsZero() {
		t.Errorf("relay episode = %+v: a held notice is neither sent nor a failed try", ep)
	}
	got := heldSenders(s)
	for _, k := range []string{senderLedger, senderRelay, senderLoop} {
		if !got[k] {
			t.Errorf("sender %s is not waiting on the retry: %v", k, got)
		}
	}
}

// A held message goes out by itself once the box clears, with no fleet
// update and no tick: the retry timer wakes the watch step that holds it.
// When nothing is held any more, the timer is not armed again.
func TestHeldRetryDeliversOnceTheBoxClears(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.typing.quiet, s.typing.poll = 100*time.Millisecond, 25*time.Millisecond
	showScreen(fh, "w1:p1", capture(t, "claude_typed.ansi"))
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	it := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"q?","no_wait":true}`).Body.Bytes())
	do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"yes"}`)
	waitFor(t, func() bool { return heldSenders(s)[senderLedger] })
	time.Sleep(300 * time.Millisecond) // several retries while the user keeps typing
	if fh.call("agent.prompt") != nil {
		t.Fatal("typed over the user's text")
	}
	showScreen(fh, "w1:p1", capture(t, "claude_empty.ansi"))
	waitFor(t, func() bool { g, _ := s.todos.get(it.ID); return g.Notice != nil && g.Notice.State == todoNoticeSent })
	if text, _ := fh.call("agent.prompt").Params["text"].(string); !strings.Contains(text, it.ID) {
		t.Fatalf("prompt = %q", text)
	}
	// Nothing held: the retry stops by itself.
	waitFor(t, func() bool {
		s.typing.mu.Lock()
		defer s.typing.mu.Unlock()
		return s.typing.timer == nil && len(s.typing.held) == 0
	})
}

// A coordinator reset held for typing is tried again once the box clears,
// not at the next resetEvery.
func TestResetHeldWhileTypingThenRetried(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	r.s.typing.quiet, r.s.typing.poll = 100*time.Millisecond, 25*time.Millisecond
	showScreen(r.fh, "w1:p1", capture(t, "claude_typed.ansi"))
	r.s.resetTick(context.Background(), time.Now())
	if r.clears() != 0 {
		t.Fatal("cleared a coordinator whose user was typing")
	}
	if d, ok := r.s.resets.get().Due["desk/w1:p1"]; !ok || !strings.Contains(d.Why, "held") {
		t.Fatalf("due = %+v (%v)", d, ok)
	}
	showScreen(r.fh, "w1:p1", capture(t, "claude_empty.ansi"))
	waitFor(t, func() bool { return r.clears() == 1 })
	waitFor(t, func() bool { return len(r.s.resets.get().Due) == 0 })
}
