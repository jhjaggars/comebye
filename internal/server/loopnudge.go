package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/state"
)

// The loop nudge: a worker handed off with `comebye handoff --loop "..."`
// is meant to keep looping (a PR watcher: look at the PR, sleep, look
// again). When it goes idle with nothing running, nothing will ever wake
// it, and until now the coordinator had to notice by hand.
//
// What "idle" means depends on the harness, and was checked on a live pi
// worker (see the slice's report): a pi worker that runs `sleep 60` inside
// its turn is "working" to herdr for the whole minute, and "idle" the
// moment the turn ends. pi has no background task that outlives a turn, so
// for pi "idle" is exactly "no loop running". The hub reads background
// tasks only for Claude Code sessions (AgentInfo.Background); a Claude
// worker that is idle with a task running is not idle here.
//
// The clock, per running handoff with a loop:
//
//   - it starts when the worker is idle (or done) with no background task;
//   - after nudgeAfter (10 minutes) the hub types the first nudge;
//   - nudgeAgain (20 minutes) after the first nudge, if it is still idle,
//     the second;
//   - nudgeAfter after the second, if it is still idle, one message to the
//     requester (else the coordinator): the loop is dead. Then the hub stops.
//
// Anything that shows the worker works again resets it: working for a
// while, or a background task. A short spell of working right after a nudge
// is the nudge's own answer ("ok, restarting") and does not reset; a worker
// that answers and goes idle again has not restarted its loop. The clock
// stops for good when the worker has reported (comebye report), or the
// handoff landed, was removed or lost (see loopWatched) and pauses while the agent is blocked at a dialog, is
// gone from the fleet (parked) or its user is typing in its box. It lives in
// memory: a hub restart starts the count again.
//
// A standing loop (`comebye loop set NAME --every 1h`: a worker that runs a
// pass on a schedule, such as an hourly steward) is not nudged: the hub is
// its scheduler. When a pass is due the hub sends the loop's text as a
// prompt through the shared sender path (deliverOrQueue or promptNotice,
// behind the one typing guard), and the worker runs the pass and stops; it
// schedules nothing of its own (no ScheduleWakeup, no cron), so a pi worker
// can run one too. A pass is due an interval after the end of the last pass
// (the ledger's LastPassAt, so a hub restart does not reset it; before the
// first pass, after the hub first saw it idle). Passes never stack: none
// goes while the worker works, is blocked or has a background task, while a
// prompt for it waits in the queue, or within an interval of the last pass
// sent; a pass the worker did not take is sent again an interval later.

const (
	// DefaultLoopNudgeAfter is how long a looping worker may sit idle
	// before its first nudge (and the grace after its second).
	DefaultLoopNudgeAfter = 10 * time.Minute
	// DefaultLoopNudgeAgain is the wait between the first and the second nudge.
	DefaultLoopNudgeAgain = 20 * time.Minute

	loopTick = 10 * time.Second
	// loopWorkMax is the longest spell of working after a nudge that still
	// counts as the nudge's own answer; a longer one is real work.
	loopWorkMax = 2 * time.Minute
	// loopRetry spaces the attempts to send one message.
	loopRetry   = 30 * time.Second
	loopSpecMax = 200
)

// loopEveryText says how often a loop worker runs its passes, for a line.
func loopEveryText(e ledger.Entry) string {
	if e.LoopEveryMinutes <= 0 {
		return "a loop, no schedule"
	}
	return "every " + shortDur(e.LoopEvery())
}

// shortDur prints 1h, 90m, 1h30m, 2d without zero units.
func shortDur(d time.Duration) string {
	s := d.Round(time.Minute).String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// cleanLoop reduces a loop spec to one short plain line: the hub types it
// into an agent's pane, so it carries no markup or control characters.
func cleanLoop(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '.', r == ',', r == '_', r == '-', r == '/', r == '#', r == ':', r == '(', r == ')', r == '\'':
			b.WriteRune(r)
		case r == '\n' || r == '\t':
			b.WriteRune(' ')
		default:
			b.WriteRune('?')
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > loopSpecMax {
		out = out[:loopSpecMax]
	}
	return out
}

// loopSt is the clock of one looping worker.
type loopSt struct {
	start    time.Time // idle since (the spell the clock counts from)
	nudges   int       // nudges sent: 0, 1 or 2
	last     time.Time // when the last nudge went out
	told     bool      // the requester has been told: nothing more
	workFrom time.Time // working since, after a nudge (zero: not working)
	tried    time.Time // last failed attempt
	passAt   time.Time // a standing loop: when the hub last sent a pass
}

type loopMon struct {
	after, again time.Duration
	mu           sync.Mutex
	m            map[string]*loopSt // by entry id

	// seams, replaced by tests
	send    func(ctx context.Context, a herdr.AgentInfo, text string) error
	notify  func(ctx context.Context, e ledger.Entry, text string) (string, error)
	workMax time.Duration
}

// initLoopNudge starts the loop clock. after or again zero turns the nudge
// of a loop with no schedule off; a standing loop's passes go out anyway.
func (s *Server) initLoopNudge(after, again time.Duration) {
	if s.ledger == nil {
		return
	}
	if after <= 0 || again <= 0 {
		after, again = 0, 0
	}
	s.loops = &loopMon{
		after: after, again: again, m: map[string]*loopSt{}, workMax: min(loopWorkMax, again/2),
		send: func(ctx context.Context, a herdr.AgentInfo, text string) error {
			if s.isLocalAgent(a) {
				// Held for typing comes back at once (the pass does not
				// wait in place): the shared retry sends it (typing.go).
				_, err := s.deliverOrQueue(withNoTypingWait(ctx), a.ID, a.Name, text)
				return err
			}
			return s.promptNotice(ctx, a, text)
		},
		notify: s.loopNotifyRequester,
	}
}

// The loop nudge is step 5 of the agent-watch pass (watch.go): loopStep runs
// every loopTick.

// loopWatched says whether the clock runs for e: a handoff with a loop whose
// worker has not said it is done. A worker whose turn ended without a
// report reads as finished to the ledger (the hub took its last reply), and
// that is exactly a dead loop; one that filed its report, landed, was
// removed or is lost is over.
func loopWatched(e ledger.Entry) bool {
	if e.Loop == "" {
		return false
	}
	switch e.Status {
	case ledger.Running:
		return true
	case ledger.Finished:
		return e.ReportSource != "explicit"
	}
	return false
}

func loopNudgeText(e ledger.Entry, n int) string {
	lead := "Comebye (hub nudge): you are idle with nothing running, but your brief says to keep this going: "
	if n == 2 {
		lead = "Comebye (hub nudge, second): you are still idle with nothing running, and the first nudge did not restart your loop. Your brief says to keep this going: "
	}
	return lead + cleanLoop(e.Loop) + ". Start it again now and keep it running between checks. If you are done, file your report with `comebye report -`."
}

// loopPassLead starts every pass the hub sends a standing loop.
const loopPassLead = "Comebye (hub, loop pass): "

// loopPassText is the prompt that starts one pass of a standing loop.
func loopPassText(e ledger.Entry) string {
	return loopPassLead + "your next pass is due (you run " + loopEveryText(e) + "). Run one pass now: " + cleanLoop(e.Loop) +
		". When the pass is done, just stop: the hub sends the next one, so do not schedule a wakeup or a cron. If the loop's work is over, file your report with `comebye report -`."
}

func loopDeadText(e ledger.Entry, idle time.Duration) string {
	return "Comebye (hub notice): the loop of the worker on branch " + safeText(e.Branch) + " in " + safeText(e.RepoName) + " on " + safeText(e.Host) +
		" (handoff " + safeText(e.ID) + ") is dead: it was idle with nothing running for " + idle.Round(time.Minute).String() +
		" and did not restart after two nudges. Its loop was: " + cleanLoop(e.Loop) + ". " +
		"Look at it with `comebye read " + safeText(e.ID) + "`, restart it with `comebye prompt` or remove it. The hub sends no more nudges for it."
}

// loopNotifyRequester tells whoever asked for the handoff (else a coordinator
// that can take it now) that the loop is dead. It returns who was told; ""
// with a nil error means nobody could take it yet.
func (s *Server) loopNotifyRequester(ctx context.Context, e ledger.Entry, text string) (string, error) {
	agents := s.fleet.Agents()
	to := e.ReplyTo
	if to == nil && e.PlanID != "" {
		if p, ok := s.ledger.GetPlan(e.PlanID); ok {
			to = p.ReplyTo
		}
	}
	var target *herdr.AgentInfo
	found := false
	if to != nil {
		for i := range agents {
			a := agents[i]
			if a.ID == to.AgentID && (to.Name == "" || a.Name == "" || a.Name == to.Name) {
				found = true
				if canTakePrompt(a) {
					target = &a
				}
				break
			}
		}
	}
	if found && target == nil {
		return "", nil // the requester is busy: wait for it, not another coordinator
	}
	gone := ""
	if target == nil {
		rt := s.routeCoordinator(to)
		target, gone = rt.Agent, rt.Gone
	}
	if target == nil {
		return "", nil
	}
	err := s.promptNotice(ctx, *target, text+goneNote(gone))
	s.heldFor(err, target.ID, senderLoop)
	return target.ID, err
}

// loopStep applies one fleet snapshot.
func (s *Server) loopStep(ctx context.Context, agents []herdr.AgentInfo, now time.Time) {
	l := s.loops
	if l == nil {
		return
	}
	byID := map[string]herdr.AgentInfo{}
	for _, a := range agents {
		byID[state.AgentKey(&a)] = a
	}
	live := map[string]bool{}
	for _, e := range s.ledger.Entries() {
		if ctx.Err() != nil {
			return
		}
		if !loopWatched(e) {
			continue
		}
		live[e.ID] = true
		a, ok := byID[e.AgentID]
		l.mu.Lock()
		st := l.m[e.ID]
		l.mu.Unlock()
		switch {
		case !ok:
			// Gone from the fleet (parked, or its host is away): nothing to
			// nudge, and a worker that comes back starts again.
			l.forget(e.ID)
			continue
		case a.Status == herdr.StatusBlocked:
			continue // at a dialog: the user's to answer
		case a.Status == herdr.StatusWorking:
			if st == nil {
				continue
			}
			l.mu.Lock()
			switch {
			case st.nudges == 0:
				delete(l.m, e.ID) // worked before any nudge: the loop is alive
			case st.workFrom.IsZero():
				st.workFrom = now
			case now.Sub(st.workFrom) >= l.workMax:
				delete(l.m, e.ID) // worked past the nudge's own answer
				log.Printf("loop nudge: %s (%s) works again, reset", e.Branch, e.ID)
			}
			l.mu.Unlock()
			continue
		case !canTakePrompt(a) || len(a.Background) > 0:
			l.forget(e.ID) // unknown status, or a background task keeps it going
			continue
		}
		// Idle, nothing running.
		l.mu.Lock()
		if st == nil {
			st = &loopSt{start: now}
			l.m[e.ID] = st
		}
		st.workFrom = time.Time{}
		cp := *st
		l.mu.Unlock()
		if cp.told || now.Sub(cp.tried) < loopRetry {
			continue
		}
		if every := e.LoopEvery(); every > 0 {
			s.loopPass(ctx, l, e, a, every, now)
			continue
		}
		if l.after <= 0 {
			continue // the nudge is off
		}
		switch {
		case cp.nudges == 0 && now.Sub(cp.start) >= l.after:
			s.loopNudge(ctx, l, e, a, 1, now)
		case cp.nudges == 1 && now.Sub(cp.last) >= l.again:
			s.loopNudge(ctx, l, e, a, 2, now)
		case cp.nudges == 2 && now.Sub(cp.last) >= l.after:
			s.loopTell(ctx, l, e, now.Sub(cp.start), now)
		}
	}
	l.mu.Lock()
	for id := range l.m {
		if !live[id] {
			delete(l.m, id) // finished, landed, removed or lost: the clock is over
		}
	}
	l.mu.Unlock()
}

func (l *loopMon) forget(id string) {
	l.mu.Lock()
	delete(l.m, id)
	l.mu.Unlock()
}

func (s *Server) loopNudge(ctx context.Context, l *loopMon, e ledger.Entry, a herdr.AgentInfo, n int, now time.Time) {
	l.mu.Lock()
	st := l.m[e.ID]
	subject := "nudge/" + e.ID + "/" + itoa(n) + "/" + itoa(int(st.start.Unix()))
	l.mu.Unlock()
	if !s.sent.claim(subject, a.ID) {
		// Sent already (the hub restarted inside this spell): count it.
		l.mu.Lock()
		st.nudges, st.last = n, now
		l.mu.Unlock()
		return
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := l.send(sctx, a, loopNudgeText(e, n)); err != nil {
		s.sent.release(subject)
		if s.heldFor(err, a.ID, senderLoop) {
			// Someone is typing in its box: the clock pauses, and the
			// nudge goes once the box clears (typing.go), not a failed try.
			return
		}
		log.Printf("loop nudge %d to %s (%s): %v", n, e.Branch, a.ID, err)
		l.mu.Lock()
		st.tried = now
		l.mu.Unlock()
		return
	}
	s.sent.confirm(subject)
	log.Printf("loop nudge %d sent to %s (%s): idle since %s", n, e.Branch, e.ID, st.start.Format(time.RFC3339))
	l.mu.Lock()
	st.nudges, st.last = n, now
	l.mu.Unlock()
}

// loopPass sends a standing loop its next pass when one is due (see the
// clock above). The send-once subject is the due time, so a restarted hub
// does not send the same pass twice.
func (s *Server) loopPass(ctx context.Context, l *loopMon, e ledger.Entry, a herdr.AgentInfo, every time.Duration, now time.Time) {
	l.mu.Lock()
	st := l.m[e.ID]
	base := st.start
	if e.LastPassAt != nil {
		base = *e.LastPassAt
	}
	if st.passAt.After(base) {
		base = st.passAt // sent, and no pass ended since: not before another interval
	}
	l.mu.Unlock()
	due := base.Add(every)
	if now.Before(due) {
		return
	}
	if s.pq != nil && len(s.pq.list(a.ID, false)) > 0 {
		return // a prompt for it waits in the queue: it is not free for a pass yet
	}
	subject := "pass/" + e.ID + "/" + itoa(int(due.Unix()))
	if !s.sent.claim(subject, a.ID) {
		l.mu.Lock()
		st.passAt = now // sent already (the hub restarted): count it
		l.mu.Unlock()
		return
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := l.send(sctx, a, loopPassText(e)); err != nil {
		s.sent.release(subject)
		if s.heldFor(err, a.ID, senderLoop) {
			return // someone is typing in its box: the shared retry sends it once it clears
		}
		log.Printf("loop pass to %s (%s): %v", e.Branch, a.ID, err)
		l.mu.Lock()
		st.tried = now
		l.mu.Unlock()
		return
	}
	s.sent.confirm(subject)
	log.Printf("loop pass sent to %s (%s): %s, due %s", e.Branch, e.ID, loopEveryText(e), due.UTC().Format(time.RFC3339))
	l.mu.Lock()
	st.passAt = now
	l.mu.Unlock()
}

func (s *Server) loopTell(ctx context.Context, l *loopMon, e ledger.Entry, idle time.Duration, now time.Time) {
	l.mu.Lock()
	st := l.m[e.ID]
	subject := "loopdead/" + e.ID + "/" + itoa(int(st.start.Unix()))
	l.mu.Unlock()
	if !s.sent.claim(subject, e.ID) {
		l.mu.Lock()
		st.told = true
		l.mu.Unlock()
		return
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	to, err := l.notify(sctx, e, loopDeadText(e, idle))
	if err != nil || to == "" {
		s.sent.release(subject)
		if isInputBusy(err) {
			return // held for typing: loopNotifyRequester noted it (typing.go)
		}
		if err != nil {
			log.Printf("loop dead notice for %s: %v", e.ID, err)
		}
		l.mu.Lock()
		st.tried = now
		l.mu.Unlock()
		return
	}
	s.sent.confirm(subject)
	log.Printf("loop dead: %s (%s) did not restart after two nudges; told %s", e.Branch, e.ID, to)
	l.mu.Lock()
	st.told = true
	l.mu.Unlock()
}

// Loop registration bounds: a pass interval is a minute to a week.
const (
	loopEveryMin = time.Minute
	loopEveryMax = 7 * 24 * time.Hour
)

// findLoopTarget picks the active handoff a `comebye loop` names: its id,
// its worker's name or agent id, or its branch. A name more than one
// active handoff answers to is refused.
func (s *Server) findLoopTarget(name string) (ledger.Entry, string) {
	if e, ok := s.ledger.Get(name); ok {
		return e, ""
	}
	var hits []ledger.Entry
	for _, e := range s.ledger.Entries() {
		if e.Active() && (e.AgentName == name || e.AgentID == name || e.Branch == name) {
			hits = append(hits, e)
		}
	}
	switch len(hits) {
	case 0:
		return ledger.Entry{}, "no active handoff is called " + name + " (see `comebye ledger`)"
	case 1:
		return hits[0], ""
	}
	return ledger.Entry{}, name + " names " + itoa(len(hits)) + " active handoffs: use the handoff id (see `comebye ledger`)"
}

// handleSetLoop registers an existing handoff as a loop worker, or takes
// its loop off (body: {"loop": "...", "every": "1h"} or {"clear": true}).
func (s *Server) handleSetLoop(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		Loop  string `json:"loop"`
		Every string `json:"every"`
		Clear bool   `json:"clear"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	e, msg := s.findLoopTarget(r.PathValue("id"))
	if msg != "" {
		writeJSONStatus(w, http.StatusNotFound, apiErr("no_handoff", msg))
		return
	}
	text, every := "", time.Duration(0)
	if !b.Clear {
		text = cleanLoop(b.Loop)
		if text == "" {
			text = e.Loop
		}
		if text == "" {
			text = "keep running your passes on schedule, as your brief says"
		}
		if strings.TrimSpace(b.Every) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(b.Every))
			if err != nil || d < loopEveryMin || d > loopEveryMax {
				writeJSONStatus(w, http.StatusBadRequest, apiErr("bad_every", "every must be a duration from 1m to 168h, such as 1h or 90m"))
				return
			}
			every = d.Truncate(time.Minute)
		}
	}
	got, err := s.ledger.SetLoop(e.ID, text, every)
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, apiErr("not_active", err.Error()))
		return
	}
	if s.loops != nil {
		s.loops.forget(e.ID) // a new schedule starts a new clock
	}
	what := "set, " + loopEveryText(got)
	if b.Clear {
		what = "cleared"
	}
	log.Printf("loop: %s (%s) %s by %s", e.Branch, e.ID, what, s.by(r, ""))
	writeJSON(w, map[string]any{"entry": got})
}
