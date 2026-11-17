package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The answer relay. When the user answers a to-do in the app, the hub
// passes the answer to the agent that waits on it, labelled as the user's
// own words, and closes the item. The coordinator used to do this by hand
// (show, prompt, done) for every answer. It still does for the gates:
//
//   - the waiting agent is a coordinator (it has to act on the answer);
//   - a release, approval or review item (they keep their own paths);
//   - a decision set that carries a plan, and an item about a handoff the
//     ledger shows as awaiting landing (the answer approves it);
//   - an answer, or the option it chose, whose text says land, merge,
//     push, approve, go ahead or start (it starts work).
//
// An item nobody waits on is not relayed either. If in doubt it is a gate.
//
// A decision set relays the decisions the user settled ahead of Submit at
// once (the item stays open) and the final answer on Submit (the item
// closes).
//
// What happens to the send:
//   - the agent is live, working or idle: the relay is typed into its pane
//     (an agent that is working takes it as queued input); the item
//     closes and an event "relay" is logged;
//   - the agent sits at a dialog, or is parked: the relay goes on the prompt
//     queue and is typed when the dialog is answered or the agent resumed.
//     The item stays answered until the queue delivers it. A queued prompt
//     expires after 2 hours (7 days when parked); then the hub gives up and
//     tells the coordinator, as for a failed send;
//   - the agent is gone, or three sends failed: the hub gives up. The item
//     stays answered (not closed), a "relay-failed" event is logged and
//     the coordinator gets the usual "the user answered" note, with the
//     reason, so it relays by hand.
//
// A relay claims its subject in the sent record before it sends, so the
// answer is typed once, however many fleet updates or restarts pass.

const (
	relayMaxTries = 3

	relayQueued = "queued" // on the prompt queue, waiting for delivery
	relayGaveUp = "gaveup" // the hub gave up: the coordinator relays it

	evRelay       = "relay"        // an answer relayed to the waiting agent
	evRelayFailed = "relay-failed" // the hub could not; the coordinator was told
)

// TodoRelay is where the hub's relay of one answer stands. Subject names
// the answer (or the early decisions) it is about, so a later answer on the
// same item starts afresh.
type TodoRelay struct {
	Subject string    `json:"subject"`
	State   string    `json:"state,omitempty"`
	QueueID string    `json:"queue_id,omitempty"`
	Tries   int       `json:"tries,omitempty"`
	Why     string    `json:"why,omitempty"`
	At      time.Time `json:"at"`
}

var (
	// gateWordRE finds the words that start or approve work. They are
	// matched at a word start, so "landing" and "approved" count.
	gateWordRE = regexp.MustCompile(`(?i)\b(land|merge|approv|start|push|go ahead)`)
	// okNRE reads the choice the option buttons send ("ok 2", "ok 1+3").
	okNRE = regexp.MustCompile(`(?i)^\s*ok\s+(\d+(?:\s*\+\s*\d+)*)\s*$`)
)

// relaySubject names what a relay is about, "" when there is nothing to relay.
func relaySubject(it Todo) string {
	if it.earlyPending() {
		ids := append([]string(nil), it.Set.EarlyPending...)
		sort.Strings(ids)
		return "relay/" + it.ID + "/early/" + strings.Join(ids, ",")
	}
	if it.State == TodoAnswered && it.Answer != nil {
		return "relay/" + it.ID + "/" + strconv.FormatInt(it.Answer.At.UnixNano(), 10)
	}
	return ""
}

// chosenOptions is the text of the options an answer picked ("ok 2").
func chosenOptions(it Todo) string {
	if it.Answer == nil {
		return ""
	}
	m := okNRE.FindStringSubmatch(it.Answer.Text)
	if m == nil {
		return ""
	}
	var sb strings.Builder
	for _, f := range strings.Split(m[1], "+") {
		n, _ := strconv.Atoi(strings.TrimSpace(f))
		if n >= 1 && n <= len(it.Options) {
			sb.WriteString(it.Options[n-1].Label + " " + it.Options[n-1].Detail + "\n")
		}
	}
	return sb.String()
}

// setChoices is the text of the options a set's answers chose: the
// decisions settled early when early is set, else all of them.
func setChoices(it Todo, early bool) string {
	if it.Set == nil {
		return ""
	}
	var sb strings.Builder
	for _, d := range it.Set.Decisions {
		if early && !contains(it.Set.EarlyPending, d.ID) {
			continue
		}
		if c := it.Set.Draft.Choices[d.ID]; c.Option >= 1 && c.Option <= len(d.Options) {
			sb.WriteString(d.Options[c.Option-1].Label + " " + d.Options[c.Option-1].Detail + "\n")
		}
	}
	return sb.String()
}

// relayGate says why an answer is not the hub's to relay; "" means it is.
func (s *Server) relayGate(it Todo) string {
	switch {
	case it.Waiting == nil:
		return "nobody waits on it"
	case it.Kind != "decision" && it.Kind != "action" && it.Kind != "followup":
		return "a " + it.Kind + " item has its own path"
	case it.Release != nil || it.Approval != nil || it.Review != nil:
		return "a release, approval or review"
	case s.isCoordinator(it.Waiting.ID, it.Waiting.Name):
		return "the coordinator is the one waiting"
	case it.Set != nil && it.Set.Plan != "":
		return "it answers a plan"
	case it.Answer == nil && !it.earlyPending():
		return "no answer"
	}
	for _, l := range it.Links {
		if l.Handoff == "" || s.ledger == nil {
			continue
		}
		if e, ok := s.ledger.Get(l.Handoff); ok && (e.Status == ledger.Finished || e.Status == ledger.Landing) {
			return "it is about " + e.ID + ", which awaits landing"
		}
	}
	var text string
	switch {
	case it.earlyPending():
		text = setChoices(it, true)
	case it.Set != nil:
		text = it.Answer.Text + "\n" + setChoices(it, false)
	default:
		text = it.Answer.Text + "\n" + chosenOptions(it)
	}
	if m := gateWordRE.FindString(text); m != "" {
		return "the answer says \"" + strings.ToLower(m) + "\""
	}
	return ""
}

// relayable reports whether the hub relays this pending notice itself: it
// is not a gate, and the hub has not already given up on this answer.
func (s *Server) relayable(it Todo) bool {
	sub := relaySubject(it)
	if sub == "" || s.todos == nil || s.sent == nil {
		return false
	}
	if r := it.Relay; r != nil && r.Subject == sub && r.State == relayGaveUp {
		return false
	}
	return s.relayGate(it) == ""
}

// relayNote is the line a coordinator notice gains for an answer the hub
// tried to relay and could not.
func relayNote(it Todo) string {
	r := it.Relay
	if r == nil || r.State != relayGaveUp || r.Subject != relaySubject(it) {
		return ""
	}
	return fmt.Sprintf(" The hub tried to relay %s to the waiting agent itself and could not (%s): relay it by hand.", safeText(it.ID), oneLine(r.Why, 160))
}

// relayText is what the waiting agent is sent.
func relayText(it Todo) string {
	q := oneLine(it.Question, 160)
	if it.earlyPending() {
		var sb strings.Builder
		for _, d := range it.Set.Decisions {
			if c := it.Set.Draft.Choices[d.ID]; contains(it.Set.EarlyPending, d.ID) && c.Option >= 1 && c.Option <= len(d.Options) {
				fmt.Fprintf(&sb, "- %s %s: %s\n", oneLine(d.ID, 20), oneLine(d.Title, 100), oneLine(d.Options[c.Option-1].Label, 120))
			}
		}
		return fmt.Sprintf("Comebye (hub relay): the user settled some decisions of your to-do set %s (%q) in the app, ahead of Submit.\nTheir choices so far:\n%s"+
			"These are the user's own choices, typed by the hub. The rest follows when they submit; carry on with what these unblock.",
			safeText(it.ID), q, sb.String())
	}
	var tag string
	if opt := chosenOptions(it); opt != "" {
		tag = "   [" + oneLine(strings.ReplaceAll(strings.TrimSpace(opt), "\n", "; "), 200) + "]"
	}
	return fmt.Sprintf("Comebye (hub relay): the user answered your to-do %s (%q) in the app.\nTheir words: %s%s\nThis is the user's own answer, typed by the hub. Carry on.",
		safeText(it.ID), q, strconv.Quote(block(it.Answer.Text, todoMaxAnswer)), tag)
}

// relayStart starts one relay in the background, unless one is running for
// the item or a failed one is waiting out its pause.
func (s *Server) relayStart(it Todo, agents []herdr.AgentInfo) {
	t := s.todos
	t.mu.Lock()
	if t.relaying == nil {
		t.relaying, t.relayFail = map[string]bool{}, map[string]time.Time{}
	}
	if t.relaying[it.ID] || time.Since(t.relayFail[it.ID]) < callbackRetry {
		t.mu.Unlock()
		return
	}
	t.relaying[it.ID] = true
	t.mu.Unlock()
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		gaveUp := s.relayOne(it, agents)
		t.mu.Lock()
		delete(t.relaying, it.ID)
		t.mu.Unlock()
		if gaveUp {
			s.deliverTodoNotices(s.fleet.Agents()) // the coordinator is told now
		}
	}()
}

// relayOne passes one answer on. It reports that it gave up, which sends
// the item down the coordinator path.
func (s *Server) relayOne(it Todo, agents []herdr.AgentInfo) bool {
	subject := relaySubject(it)
	name := ""
	if it.Waiting != nil {
		name = it.Waiting.Name
	}
	rel := it.Relay
	if rel != nil && rel.Subject != subject {
		rel = nil
	}
	if rel != nil && rel.State == relayQueued {
		return s.relayQueued(it, subject, rel.QueueID)
	}
	cur := s.todoGone(it) // adds Gone and Parked
	w := cur.Waiting
	if w.Gone {
		return s.relayGiveUp(it, subject, "the waiting agent is gone")
	}
	if !s.sent.claim(subject, w.ID) {
		// Claimed before and the item is still not closed: the send, or
		// the closing, was cut short (a restart). It may have arrived.
		return s.relayGiveUp(it, subject, "an earlier relay of this answer was started and may or may not have arrived")
	}
	text := relayText(it)
	if w.Parked {
		return s.relayEnqueue(it, subject, *w, text)
	}
	a, replaced := findAgent(agents, w.ID, w.Name)
	if a == nil || replaced {
		s.sent.release(subject)
		return s.relayGiveUp(it, subject, "the waiting agent is not in the fleet any more")
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	send := s.todos.relaySend
	if send == nil {
		send = s.promptNotice
	}
	err := send(ctx, *a, text)
	switch {
	case err == nil:
		s.relayDone(it, subject, name)
		return false
	case isDialogErr(err):
		// A dialog: type nothing over it; the queue sends when it is answered.
		return s.relayEnqueue(it, subject, *w, text)
	}
	s.sent.release(subject) // a retry may send
	tries := 1
	if rel != nil {
		tries = rel.Tries + 1
	}
	log.Printf("relay of %s to %s: %v", it.ID, name, err)
	if tries >= relayMaxTries {
		return s.relayGiveUp(it, subject, fmt.Sprintf("%d sends failed: %v", tries, err))
	}
	s.todos.mutate(it.ID, func(x *Todo) error {
		x.Relay = &TodoRelay{Subject: subject, Tries: tries, Why: oneLine(err.Error(), 160), At: time.Now()}
		return nil
	})
	s.todos.mu.Lock()
	s.todos.relayFail[it.ID] = time.Now()
	s.todos.mu.Unlock()
	return false
}

// isDialogErr: the pane refused because a dialog is open or the agent is blocked.
func isDialogErr(err error) bool {
	var he *herdr.Error
	return errors.Is(err, errDialogOpen) || (errors.As(err, &he) && (he.Code == "dialog_open" || he.Code == "agent_blocked"))
}

// relayEnqueue puts the relay on the prompt queue (a dialog is open, or the
// agent is parked) and records the queue id on the item.
func (s *Server) relayEnqueue(it Todo, subject string, w TodoAgent, text string) bool {
	if s.pq == nil {
		s.sent.release(subject)
		return s.relayGiveUp(it, subject, "the agent cannot take it now and there is no prompt queue")
	}
	p, _, err := s.pq.add(QueuedPrompt{Agent: w.ID, Name: w.Name, Text: text, From: "comebye"})
	if err != nil {
		s.sent.release(subject)
		return s.relayGiveUp(it, subject, "the agent cannot take it now and it could not be queued: "+err.Error())
	}
	s.todos.mutate(it.ID, func(x *Todo) error {
		x.Relay = &TodoRelay{Subject: subject, State: relayQueued, QueueID: p.ID, At: time.Now()}
		return nil
	})
	s.logEvent(evRelay, it.ID, w.Name, "", fmt.Sprintf("queued %s for %s until it can take it (%s)", it.ID, oneLine(w.Name, 40), queueWhy(w)))
	return false
}

func queueWhy(w TodoAgent) string {
	if w.Parked {
		return "parked"
	}
	return "a dialog is open"
}

// relayQueued follows a relay that waits on the prompt queue.
func (s *Server) relayQueued(it Todo, subject, qid string) bool {
	name := ""
	if it.Waiting != nil {
		name = it.Waiting.Name
	}
	if s.pq == nil {
		return s.relayGiveUp(it, subject, "the prompt queue is gone")
	}
	for _, p := range s.pq.list("", true) {
		if p.ID != qid {
			continue
		}
		switch p.State {
		case QueuePending:
			return false
		case QueueDelivered:
			s.relayDone(it, subject, name)
			return false
		case QueueExpired:
			why := "the queued relay expired after 2 hours without the agent taking it"
			if p.Parked {
				why = "the queued relay expired after 7 days: the agent was parked and not resumed"
			}
			return s.relayGiveUp(it, subject, why)
		}
		return s.relayGiveUp(it, subject, "the queued relay was "+p.State+" and not delivered")
	}
	return s.relayGiveUp(it, subject, "the queued relay is no longer on the queue")
}

// relayDone records a relay that reached the agent: the item closes (an
// early relay on a set only clears what it told), and the event is logged.
func (s *Server) relayDone(it Todo, subject, name string) {
	s.sent.confirm(subject)
	line := ""
	if it.earlyPending() {
		told := it.Set.EarlyPending
		s.todos.mutate(it.ID, func(x *Todo) error {
			if x.Set == nil {
				return nil
			}
			ts := x.Set.clone()
			var rest []string
			for _, id := range ts.EarlyPending {
				if !contains(told, id) {
					rest = append(rest, id)
				}
			}
			ts.EarlyPending = rest
			x.Set = ts
			if len(rest) == 0 && x.Notice != nil {
				n := *x.Notice
				n.State, n.At = todoNoticeSent, time.Now()
				x.Notice = &n
			}
			x.Relay = nil
			return nil
		})
		line = fmt.Sprintf("relayed %s decision(s) %s → %s, item stays open", it.ID, strings.Join(told, ", "), oneLine(name, 40))
	} else {
		if _, err := s.todos.close(it.ID, TodoDone, "hub", ""); err != nil && !errors.Is(err, errTodoClosed) {
			log.Printf("relay of %s: closing: %v", it.ID, err)
		}
		line = fmt.Sprintf("relayed %s → %s (%q), closed", it.ID, oneLine(name, 40), oneLine(it.Answer.Text, 80))
	}
	s.logEvent(evRelay, it.ID, name, subject, line)
}

// relayGiveUp stops the hub's own relay of this answer: the item stays
// answered and the coordinator is told, with the reason.
func (s *Server) relayGiveUp(it Todo, subject, why string) bool {
	name := ""
	if it.Waiting != nil {
		name = it.Waiting.Name
	}
	s.todos.mutate(it.ID, func(x *Todo) error {
		x.Relay = &TodoRelay{Subject: subject, State: relayGaveUp, Why: oneLine(why, 200), At: time.Now()}
		return nil
	})
	s.logEvent(evRelayFailed, it.ID, name, subject, fmt.Sprintf("could not relay %s → %s: %s; the coordinator was told", it.ID, oneLine(name, 40), oneLine(why, 160)))
	return true
}
