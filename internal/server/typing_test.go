package server

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// Holding messages while the user types. Screens are the recorded captures
// in internal/dialog/testdata/input (real scratch Claude Code and pi).

func capture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../dialog/testdata/input/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// clock is a settable time source for the tracker.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func fakeClock(s *Server) *clock {
	c := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	s.typing.now = c.now
	return c
}

func TestTypingTrackerQuietPeriod(t *testing.T) {
	tr := newTypingTracker()
	c := &clock{t: time.Unix(1000, 0)}
	tr.now = c.now
	busy, empty := dialog.Input{Known: true, Busy: true}, dialog.Input{Known: true}
	if ok, _ := tr.observe("p", empty); !ok {
		t.Fatal("an untouched empty box held a message")
	}
	if ok, _ := tr.observe("p", busy); ok {
		t.Fatal("a box with text did not hold")
	}
	c.add(time.Minute)
	if ok, _ := tr.observe("p", busy); ok {
		t.Fatal("still typing, but not held")
	}
	// Cleared: the quiet period starts at the first empty reading.
	if ok, _ := tr.observe("p", empty); ok {
		t.Fatal("sent the instant the box cleared")
	}
	c.add(typingQuiet - time.Second)
	if ok, _ := tr.observe("p", empty); ok {
		t.Fatal("sent before the quiet period ended")
	}
	// Typing again restarts it.
	if ok, _ := tr.observe("p", busy); ok {
		t.Fatal("typing again did not hold")
	}
	if ok, _ := tr.observe("p", empty); ok {
		t.Fatal("no new quiet period after typing again")
	}
	c.add(typingQuiet)
	if ok, _ := tr.observe("p", empty); !ok {
		t.Fatal("not released after the quiet period")
	}
	// Other panes are unaffected.
	if ok, _ := tr.observe("other", empty); !ok {
		t.Fatal("another pane was held")
	}
}

func TestPromptRefusedWhileUserTypes(t *testing.T) {
	for name, c := range map[string]struct{ pane, target, typed, empty string }{
		"claude": {"w1:p1", "coordinator", "claude_typed.ansi", "claude_empty.ansi"},
		"pi":     {"w2:p1", "home", "pi_typed.ansi", "pi_empty2.ansi"},
	} {
		s, fh, _ := coordServer(t)
		clk := fakeClock(s)
		showScreen(fh, c.pane, capture(t, c.typed))
		path := "/api/agents/" + c.target + "/prompt"
		w := do(s, "POST", path, userHdr, `{"text":"hello"}`)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "input_busy") || !strings.Contains(w.Body.String(), "held") {
			t.Errorf("%s: prompt = %d %s, want 409 input_busy", name, w.Code, w.Body)
		}
		if fh.call("agent.prompt") != nil {
			t.Errorf("%s: text was typed over the user's", name)
		}
		// The box clears: still held for the quiet period, then it goes.
		showScreen(fh, c.pane, capture(t, c.empty))
		if w = do(s, "POST", path, userHdr, `{"text":"hello"}`); w.Code != http.StatusConflict {
			t.Errorf("%s: sent the instant the box cleared: %d", name, w.Code)
		}
		clk.add(typingQuiet + time.Second)
		if w = do(s, "POST", path, userHdr, `{"text":"hello"}`); w.Code != http.StatusOK || fh.call("agent.prompt") == nil {
			t.Errorf("%s: after the quiet period: %d %s, prompt sent = %v", name, w.Code, w.Body, fh.call("agent.prompt") != nil)
		}
	}
}

// An empty box (the placeholder only), and a screen or kind the detector
// cannot read, behave as before: the prompt is typed.
func TestPromptSentWhenBoxEmptyOrUnreadable(t *testing.T) {
	s, fh, _ := coordServer(t)
	showScreen(fh, "w1:p1", capture(t, "claude_empty.ansi"))
	if w := do(s, "POST", "/api/agents/coordinator/prompt", userHdr, `{"text":"hello"}`); w.Code != http.StatusOK {
		t.Fatalf("empty box: %d %s", w.Code, w.Body)
	}
	showScreen(fh, "w2:p1", "some screen with no input box at all\n")
	if w := do(s, "POST", "/api/agents/home/prompt", userHdr, `{"text":"hello"}`); w.Code != http.StatusOK {
		t.Fatalf("unreadable screen: %d %s", w.Code, w.Body)
	}
}

// A queued prompt is held while the user types, shows why, and goes out
// by itself once the box has been empty for the quiet period.
func TestQueuedPromptHeldThenDelivered(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.typing.quiet, s.typing.poll = 150*time.Millisecond, 25*time.Millisecond
	showScreen(fh, "w1:p1", capture(t, "claude_typed.ansi"))
	body := `{"agent":"desk/w1:p1","name":"coordinator","text":"please rebase"}`
	if w := do(s, "POST", "/api/queue", agentHdr("w2:p1"), body); w.Code != http.StatusAccepted {
		t.Fatalf("queue = %d %s", w.Code, w.Body)
	}
	waitFor(t, func() bool { l := s.pq.list("", false); return len(l) == 1 && l[0].Held != "" })
	time.Sleep(400 * time.Millisecond) // several retry rounds while the user keeps typing
	if fh.call("agent.prompt") != nil {
		t.Fatal("typed over the user's text")
	}
	it := s.pq.list("", false)[0]
	if it.State != QueuePending || it.HeldSince == nil || !strings.Contains(it.Held, "typing") {
		t.Fatalf("item = %+v", it)
	}
	// The user submits or clears the box.
	showScreen(fh, "w1:p1", capture(t, "claude_empty.ansi"))
	waitFor(t, func() bool { return fh.call("agent.prompt") != nil })
	p := fh.call("agent.prompt").Params
	if !strings.Contains(p["text"].(string), "please rebase") {
		t.Fatalf("prompt = %v", p)
	}
	waitFor(t, func() bool { return len(s.pq.list("", false)) == 0 })
}

// A hub notice to a coordinator whose user is mid-sentence waits too, and
// is not counted as a failed attempt.
func TestCallbackHeldWhileRequesterTypes(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.typing.quiet, s.typing.poll = 100*time.Millisecond, 25*time.Millisecond
	showScreen(fh, "w1:p1", capture(t, "claude_typed.ansi"))
	idle := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, idle)

	s.deliverCallbacks([]herdr.AgentInfo{idle})
	time.Sleep(300 * time.Millisecond)
	if fh.call("agent.prompt") != nil {
		t.Fatal("hub notice typed over the user's text")
	}
	if g, _ := s.ledger.Get(e.ID); g.Callback.State != ledger.CallbackPending {
		t.Fatalf("callback = %+v", g.Callback)
	}
	showScreen(fh, "w1:p1", capture(t, "claude_empty.ansi"))
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	if text, _ := fh.call("agent.prompt").Params["text"].(string); !strings.Contains(text, "hub notice") {
		t.Fatalf("prompt = %q", text)
	}
}

// A brief for a fresh agent waits for the box instead of failing.
func TestDeliverPromptWaitsForBox(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.typing.quiet, s.typing.poll = 60*time.Millisecond, 20*time.Millisecond
	showScreen(fh, "w2:p1", capture(t, "pi_typed.ansi"))
	done := make(chan error, 1)
	go func() { done <- s.deliverPrompt(t.Context(), "home", "do the thing") }()
	time.Sleep(200 * time.Millisecond)
	if fh.call("agent.prompt") != nil {
		t.Fatal("brief typed over the user's text")
	}
	showScreen(fh, "w2:p1", capture(t, "pi_empty2.ansi"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deliverPrompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("brief never delivered")
	}
	if fh.call("agent.prompt") == nil {
		t.Fatal("brief not sent")
	}
}

func TestPromptHeldGivesUpWhenContextEnds(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.typing.poll = 10 * time.Millisecond
	showScreen(fh, "w2:p1", capture(t, "pi_typed.ansi"))
	// Short on purpose: the context ends mid-poll, and promptHeld must still
	// report "held" even when the in-flight attempt fails with an i/o error.
	ctx, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	err := s.promptHeld(ctx, herdr.AgentInfo{ID: "desk/w2:p1", Host: "desk", PaneID: "w2:p1", Name: "home", Agent: "pi"}, "x")
	if err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("err = %v", err)
	}
	if fh.call("agent.prompt") != nil {
		t.Fatal("typed over the user's text")
	}
}
