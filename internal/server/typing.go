package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
)

// Holding a message while the user types.
//
// Comebye types into agent panes: hub notices, handoff briefs, queued
// prompts, `comebye prompt`. A pane's input box is shared with the user, and
// text typed over half a sentence — followed by an Enter — sends a message
// nobody wrote. So before anything is typed, the box is read; if it holds
// text, the message waits (input_busy) until the box is empty and has stayed
// empty for typingQuiet, so it does not go the instant the user deletes a
// word.
//
// herdr offers nothing to ask (no input contents, cursor or last-keystroke
// time), so the box is read from the screen: internal/dialog.DetectInput.
// An agent kind or screen it cannot read is not held (behaviour as before
// this check), and logged once.
//
// The check and the typing are two herdr calls, so a key pressed between
// them can still be missed; the gap is one socket round trip (a few ms).

const (
	// typingQuiet is how long the box must stay empty after the user last
	// had text in it.
	typingQuiet = 5 * time.Second
	// typingPoll spaces the retries of held messages: while any message is
	// held, one timer wakes the senders this often (each try reads the box).
	typingPoll = 2 * time.Second
	// typingMemory is how long a pane is remembered after the last time a
	// delivery looked at it.
	typingMemory = time.Minute
	// typingNotice is how long a held queued prompt waits before its
	// sender is told it is still held.
	typingNotice = 10 * time.Minute
)

// errInputBusy is returned, in place of typing, when the box holds text.
var errInputBusy = &herdr.Error{Code: "input_busy", Message: "held: the user is typing in that pane; the message waits until the input box is empty"}

// isInputBusy reports an input_busy refusal, including one a peer host
// relayed (its HTTP error body names the code).
func isInputBusy(err error) bool {
	if err == nil {
		return false
	}
	var he *herdr.Error
	if errors.As(err, &he) {
		return he.Code == "input_busy"
	}
	return strings.Contains(err.Error(), "input_busy")
}

// typingTracker remembers, per pane, when its box last held text, so the
// quiet period can be enforced across separate delivery attempts.
type typingTracker struct {
	mu     sync.Mutex
	panes  map[string]*typingPane
	held   map[string]*heldRec // agent id -> its held deliveries
	timer  *time.Timer         // the one retry wake, armed while anything is held
	wake   chan struct{}       // closed at each retry: wakes the senders that wait in place
	warned map[string]bool     // kinds already logged as unreadable
	quiet  time.Duration
	poll   time.Duration
	now    func() time.Time
}

// heldRec is what was held for one agent.
type heldRec struct {
	at      time.Time       // the last time a delivery was held
	senders map[string]bool // the watch steps that re-send (watch.go)
}

type typingPane struct {
	busySince  time.Time // box first seen with text in this stretch
	clearSince time.Time // box first seen empty after that; zero while busy
	touched    time.Time
}

func newTypingTracker() *typingTracker {
	return &typingTracker{panes: map[string]*typingPane{}, held: map[string]*heldRec{}, warned: map[string]bool{},
		wake: make(chan struct{}), quiet: typingQuiet, poll: typingPoll, now: time.Now}
}

// recentlyBusy reports that the pane's box held text within window (a pane
// is remembered for typingMemory after a delivery last looked at it).
func (t *typingTracker) recentlyBusy(pane string, window time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.panes[pane]
	return p != nil && t.now().Sub(p.touched) < window
}

// observe folds one reading of pane's box into its record and returns
// whether a message may be typed now.
func (t *typingTracker) observe(pane string, in dialog.Input) (ok bool, typingFor time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for k, p := range t.panes {
		if now.Sub(p.touched) > typingMemory {
			delete(t.panes, k)
		}
	}
	p := t.panes[pane]
	if in.Busy {
		if p == nil {
			p = &typingPane{busySince: now}
			t.panes[pane] = p
		}
		p.touched, p.clearSince = now, time.Time{}
		return false, now.Sub(p.busySince)
	}
	if p == nil {
		return true, 0 // empty and nobody typed lately
	}
	p.touched = now
	if p.clearSince.IsZero() {
		p.clearSince = now
	}
	if now.Sub(p.clearSince) < t.quiet {
		return false, now.Sub(p.busySince)
	}
	delete(t.panes, pane)
	return true, 0
}

// warnOnce reports true the first time kind is seen unreadable.
func (t *typingTracker) warnOnce(kind string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.warned[kind] {
		return false
	}
	t.warned[kind] = true
	return true
}

// inputGuard returns errInputBusy when the user has text in agent's input
// box (or only just stopped typing). It reads the pane right before the
// caller types. A box it cannot read is not held.
func (s *Server) inputGuard(ctx context.Context, agent herdr.AgentInfo) error {
	t := s.typing
	if t == nil {
		return nil
	}
	read, err := s.client.ReadPaneStyled(ctx, agent.PaneID, "visible", 0)
	if err != nil {
		if t.warnOnce("read:" + agent.Agent) {
			log.Printf("input guard: cannot read %s (%s): %v; sending without checking its input box", agent.PaneID, agent.Agent, err)
		}
		return nil
	}
	in := dialog.DetectInput(read.Text, agent.Agent)
	if !in.Known {
		if t.warnOnce(agent.Agent) {
			log.Printf("input guard: no input box found for a %q agent (%s); sending without checking it", agent.Agent, agent.PaneID)
		}
		return nil
	}
	if ok, _ := t.observe(agent.PaneID, in); !ok {
		log.Printf("input guard: held a message for %s (%s): box %s", agent.PaneID, agent.Agent, boxState(in))
		return inputBusyError(in)
	}
	return nil
}

// boxState describes a reading of the input box for the log.
func boxState(in dialog.Input) string {
	if in.Busy {
		return fmt.Sprintf("holds text %q", in.Text)
	}
	return "is empty but was not a few seconds ago"
}

// inputBusyError is errInputBusy saying what was found in the box, so the
// refusal can tell the user why (and what "Send anyway" would type after).
func inputBusyError(in dialog.Input) error {
	if in.Busy {
		return &herdr.Error{Code: "input_busy", Message: fmt.Sprintf("held: the input box in that pane has text in it (%q); a message sent now would be typed after it", in.Text)}
	}
	return &herdr.Error{Code: "input_busy", Message: "held: the input box in that pane was emptied a moment ago; wait a few seconds, or send anyway"}
}

// One sender path for held messages (#143, slice 3).
//
// Everything the hub types into a pane on its own — callbacks, queued
// prompts, to-do answers, blocked relays, loop nudges, the coordinator
// reset, and the asks and briefs that wait in place — goes through one
// typing guard (inputGuard, run by promptAgent and guardPrompt here and by
// the same code on a peer), and when that guard holds a message the sender
// calls noteHeld. Each sender keeps its own "send once" tag (the sent
// claims, a callback's notice key, a to-do's notice state, a relay
// episode's flags, a loop's spell): a held message is not counted as sent
// or as a failed try, so it goes once the box clears.
//
// The retry is event-driven: no poller runs. noteHeld arms one timer
// (typingPoll) while anything is held; when it fires, the watch pass
// (watch.go) re-runs the steps whose messages were held, and every sender
// waiting in place (promptHeld, a brief) tries again. A fleet update runs
// the event-driven steps as well. A delivery that is not held again within
// a few polls has gone out (or stopped waiting), and the timer is not armed
// again.

// noteHeld records that a delivery to agentID was held because of typing,
// by the watch step sender ("" for a sender that waits in place), and arms
// the retry. It returns a channel that is closed at that retry. The hub
// calls it for panes on any host (a peer's own guard does the check).
func (s *Server) noteHeld(agentID, sender string) <-chan struct{} {
	t := s.typing
	if t == nil {
		ch := make(chan struct{})
		time.AfterFunc(typingPoll, func() { close(ch) })
		return ch
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.held[agentID]
	if h == nil {
		h = &heldRec{senders: map[string]bool{}}
		t.held[agentID] = h
	}
	h.at = t.now()
	if sender != "" {
		h.senders[sender] = true
	}
	if t.timer == nil {
		t.timer = time.AfterFunc(t.poll, s.retryHeld)
	}
	return t.wake
}

// heldFor calls noteHeld when err is a typing hold, and reports it.
func (s *Server) heldFor(err error, agentID, sender string) bool {
	if !isInputBusy(err) {
		return false
	}
	s.noteHeld(agentID, sender)
	return true
}

// retryHeld is the retry timer: it wakes the senders of what is held, and
// arms itself again while anything was held lately.
func (s *Server) retryHeld() {
	t := s.typing
	t.mu.Lock()
	t.timer = nil
	now := t.now()
	senders := map[string]bool{}
	for id, h := range t.held {
		if now.Sub(h.at) > 3*t.poll+time.Second { // nothing was held lately: nobody is waiting
			delete(t.held, id)
			continue
		}
		for k := range h.senders {
			senders[k] = true
		}
		h.senders = map[string]bool{}
	}
	close(t.wake)
	t.wake = make(chan struct{})
	if len(t.held) > 0 {
		t.timer = time.AfterFunc(t.poll, s.retryHeld)
	}
	t.mu.Unlock()
	if s.watch == nil {
		return
	}
	for k := range senders {
		s.watch.request(whyRetry, k)
	}
}

// promptHeld is promptAgent that waits out a typing user, for senders with
// nowhere else to wait (a handoff's brief, a mark's ask, a sweep's notice):
// it tries again at each retry until the box is clear or ctx ends, and then
// reports the hold.
func (s *Server) promptHeld(ctx context.Context, a herdr.AgentInfo, text string) error {
	const heldMsg = "held: the user was typing in that pane the whole time and the message was not sent"
	wasBusy := false
	for {
		err := s.promptAgent(ctx, a, text)
		if !isInputBusy(err) {
			// The context can end while this attempt is in flight, which
			// fails it with a dial/i-o error. If the pane was busy before,
			// the honest answer is still "held".
			if err != nil && wasBusy && ctx.Err() != nil {
				return errors.New(heldMsg)
			}
			return err
		}
		wasBusy = true
		retry := s.noteHeld(a.ID, "")
		select {
		case <-ctx.Done():
			return errors.New(heldMsg)
		case <-retry:
		}
	}
}
