package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
	"github.com/jhjaggars/comebye/internal/state"
)

// Suggest parking. An agent that has been idle for a day (-park-suggest-after) and
// passes every guard gets AgentInfo.ParkHint: the app shows a "can park" chip and a
// one-tap review. Nothing is parked by itself. The guards are live checks, none a
// memory: the agent is idle or done, a worker or an ad-hoc session in its own
// worktree (never a coordinator or a repository's home session), has an open to-do
// of its own that waits on the user nowhere, has no background task, nobody typing
// in its box, and a clean worktree (uncommitted edits block a suggestion; a manual
// park of a dirty tree is still allowed, after a confirm).
//
// A "Chat about this" helper (chat.go) has no checkout, so it needs no worktree
// and has none to be dirty: it is a candidate once it has been idle long enough
// and the other guards pass. Resuming it from Recently closed already works.
//
// The quiet-worker mark (quiet.go) is a signal for working agents; parking takes
// idle ones, so it is not an input here.

const (
	// DefaultParkSuggestAfter is how long an agent is idle before it is suggested.
	DefaultParkSuggestAfter = 24 * time.Hour
	// minParkSuggestAfter is the shortest idle time that can be configured.
	minParkSuggestAfter = time.Hour

	parkSugTick    = time.Minute
	parkSugRecheck = 10 * time.Minute // between worktree checks of one agent
	parkSugReads   = 6                // worktree checks one step may start

	// Automatic parking (mode "auto"): an agent that is a candidate, with a clean
	// worktree, and idle for a full day is parked by the hub.
	autoParkIdle    = 24 * time.Hour
	autoParkPerStep = 2         // parks one step may start
	autoParkPerHour = 10        // parks in any hour
	autoParkRetry   = time.Hour // before a refused agent is tried again
)

// Park modes.
const (
	ParkModeAuto    = "auto"
	ParkModeSuggest = "suggest"
	ParkModeOff     = "off"
)

type parkSugState struct {
	checked time.Time
	known   bool // the worktree was checked
	clean   bool
	ok      bool // every guard passed at the last step
	since   time.Time
	tried   time.Time // the last automatic park attempt
}

type parkSuggest struct {
	after time.Duration
	mu    sync.Mutex
	m     map[string]*parkSugState
	now   func() time.Time
	// auto parks by itself (mode auto); otherwise it only suggests.
	auto   bool
	parked []time.Time // when the hub parked, within the last hour
	// parkOne parks a candidate (tests replace it); it returns the park's
	// record id, or the reason it did not.
	parkOne func(ctx context.Context, a herdr.AgentInfo) (string, error)
	// checkClean answers whether the agent's worktree has no uncommitted edits
	// (tests replace it).
	checkClean func(ctx context.Context, a herdr.AgentInfo) (clean, known bool)
}

func (s *Server) initParkSuggest(after time.Duration) {
	s.initParkMode(after, ParkModeSuggest)
}

// initParkMode starts the monitor in mode: "auto" (the default when empty),
// "suggest" or "off".
func (s *Server) initParkMode(after time.Duration, mode string) {
	if after <= 0 || mode == ParkModeOff {
		return
	}
	if after < minParkSuggestAfter {
		after = minParkSuggestAfter
	}
	s.parkSug = &parkSuggest{after: after, m: map[string]*parkSugState{}, now: time.Now}
	s.parkSug.checkClean = s.worktreeClean
	s.parkSug.auto = (mode == "" || mode == ParkModeAuto) && s.fleet != nil && s.fleet.IsHub() // the hub parks, a peer only answers
	s.parkSug.parkOne = s.autoParkOne
}

// autoParkOne parks a on its own host.
func (s *Server) autoParkOne(ctx context.Context, a herdr.AgentInfo) (string, error) {
	body := parkBody{Agent: a.PaneID, Name: a.Name, Why: "auto"}
	var res parkResult
	if s.isLocalHost(a.Host) {
		r, lerr := s.parkAgent(ctx, body, "hub")
		if lerr != nil {
			return "", errors.New(lerr.message)
		}
		res = *r
	} else if lerr := s.callHost(ctx, a.Host, "park", nil, body, &res); lerr != nil {
		return "", errors.New(lerr.message)
	}
	return res.Record.ID, nil
}

func (s *Server) worktreeClean(ctx context.Context, a herdr.AgentInfo) (clean, known bool) {
	if a.Worktree == nil {
		return false, false
	}
	c, lerr := s.checksOn(ctx, a.Host, a.Worktree.CheckoutPath, "", "", checkOpts{})
	if lerr != nil || c == nil || !c.Exists || c.Error != "" {
		return false, false
	}
	return !c.Dirty, true
}

// stampPark sets ParkHint on the agents the monitor currently suggests.
func (s *Server) stampPark(agents []herdr.AgentInfo) {
	p := s.parkSug
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range agents {
		if st := p.m[state.AgentKey(&agents[i])]; st != nil && st.ok && (agents[i].Status == herdr.StatusIdle || agents[i].Status == herdr.StatusDone) {
			agents[i].ParkHint = &herdr.ParkHint{Since: st.since}
		}
	}
}

// Park suggest is step 6 of the agent-watch pass (watch.go):
// parkSuggestStep runs every parkSugTick.

// parkBlock says why a is not a candidate right now ("" when it is). The
// worktree check comes after, since it asks the host.
func (s *Server) parkBlock(a herdr.AgentInfo, after time.Duration, now time.Time, openTodos map[string]bool) string {
	switch {
	case !park.Supported(a.Agent):
		return "its kind cannot be parked"
	case a.Name == "":
		return "no name to resume it under"
	case !idleLongEnough(&a, after, now):
		return "not idle for long enough"
	case s.isCoordinator(a.ID, a.Name):
		return "a coordinator"
	case !s.isChatHelper(a) && (a.Worktree == nil || !a.Worktree.IsLinked):
		return "not in its own worktree (a home session)"
	case len(a.Background) > 0 || s.bgRunning(a):
		return "a background task is running"
	case openTodos[a.ID]:
		return "it has an open question for the user"
	case s.pq != nil && len(s.pq.list(a.ID, false)) > 0:
		return "a prompt is queued for it"
	case s.typing != nil && s.typing.recentlyBusy(a.PaneID, 10*time.Minute):
		return "someone is typing in its input box"
	}
	return ""
}

// isChatHelper: a is a "Chat about this" helper: the hub's role says so and it
// runs in a scratch directory named like one (chat.go). Such a helper lives
// outside any git checkout, so the worktree guards do not apply to it; the
// host that parks it checks the directory again (park.go).
func (s *Server) isChatHelper(a herdr.AgentInfo) bool {
	if s.roles == nil || a.ID == "" {
		return false
	}
	s.roles.mu.Lock()
	r, ok := s.roles.data[a.ID]
	s.roles.mu.Unlock()
	if !ok || r.Role != RoleChat || (r.Name != "" && a.Name != "" && r.Name != a.Name) {
		return false
	}
	if s.isLocalHost(a.Host) {
		return s.isChatDirName(a.CWD)
	}
	// A peer's state directory is not known here; its host checks the
	// directory properly when the hub asks it to park (park.go).
	return filepath.Base(filepath.Dir(a.CWD)) == "chats" && chatDirRE.MatchString(filepath.Base(a.CWD))
}

func (s *Server) bgRunning(a herdr.AgentInfo) bool {
	if s.isLocalHost(a.Host) {
		return len(s.backgroundOfAgent(a)) > 0
	}
	return false
}

// parkSuggestStep applies one fleet snapshot: it checks the worktree of each
// candidate that passes the cheap guards and sets or clears its suggestion.
func (s *Server) parkSuggestStep(ctx context.Context, agents []herdr.AgentInfo, now time.Time) {
	p := s.parkSug
	if p == nil {
		return
	}
	openTodos := map[string]bool{}
	if s.todos != nil {
		for _, t := range s.todos.list("") {
			if t.State == TodoOpen && t.CreatedByID != "" && t.Source != SourceBlocked {
				openTodos[t.CreatedByID] = true
			}
		}
	}
	live := map[string]bool{}
	reads := 0
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		key := state.AgentKey(&a)
		live[key] = true
		p.mu.Lock()
		st := p.m[key]
		p.mu.Unlock()
		if why := s.parkBlock(a, p.after, now, openTodos); why != "" {
			if st != nil {
				p.mu.Lock()
				delete(p.m, key)
				p.mu.Unlock()
			}
			continue
		}
		since := a.StatusSince
		if since == nil {
			since = a.LastActive
		}
		if st == nil {
			st = &parkSugState{}
			p.mu.Lock()
			p.m[key] = st
			p.mu.Unlock()
		}
		p.mu.Lock()
		stale := !st.known || now.Sub(st.checked) >= parkSugRecheck
		p.mu.Unlock()
		if stale && reads < parkSugReads {
			reads++
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			clean, known := true, true // a chat helper has no checkout: nothing to be dirty
			if !s.isChatHelper(a) {
				clean, known = p.checkClean(cctx, a)
			}
			cancel()
			p.mu.Lock()
			st.checked, st.known, st.clean = now, known, clean
			p.mu.Unlock()
		}
		p.mu.Lock()
		st.ok = st.known && st.clean
		if since != nil {
			st.since = *since
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	for k := range p.m {
		if !live[k] {
			delete(p.m, k)
		}
	}
	p.mu.Unlock()
}

// autoParkStep is the janitor's autopark stage (janitor.go), every
// parkSugTick: it parks the agents the suggestion found clean (parkSuggestStep,
// in the agent-watch pass) once they have been idle for autoParkIdle, at most
// autoParkPerStep a step and autoParkPerHour an hour. The cheap guards are
// asked again, since the fleet may have moved since the suggestion's look.
func (s *Server) autoParkStep(ctx context.Context, agents []herdr.AgentInfo, now time.Time) {
	p := s.parkSug
	if p == nil || !p.auto {
		return
	}
	openTodos := map[string]bool{}
	if s.todos != nil {
		for _, t := range s.todos.list("") {
			if t.State == TodoOpen && t.CreatedByID != "" && t.Source != SourceBlocked {
				openTodos[t.CreatedByID] = true
			}
		}
	}
	p.mu.Lock()
	cut := now.Add(-time.Hour)
	kept := p.parked[:0]
	for _, t := range p.parked {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	p.parked = kept
	p.mu.Unlock()
	auto := 0
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		key := state.AgentKey(&a)
		p.mu.Lock()
		st := p.m[key]
		p.mu.Unlock()
		if st == nil || s.parkBlock(a, p.after, now, openTodos) != "" {
			continue
		}
		since := a.StatusSince
		if since == nil {
			since = a.LastActive
		}
		p.mu.Lock()
		tryAuto := st.ok && since != nil && now.Sub(*since) >= autoParkIdle && now.Sub(st.tried) >= autoParkRetry &&
			auto < autoParkPerStep && len(p.parked) < autoParkPerHour
		if tryAuto {
			st.tried = now
		}
		p.mu.Unlock()
		if tryAuto {
			auto++
			s.autoPark(ctx, a, key, now)
		}
	}
}

// autoPark parks a for good reason: the audit entry and the digest line say so.
// A refusal leaves the suggestion where it was.
func (s *Server) autoPark(ctx context.Context, a herdr.AgentInfo, key string, now time.Time) {
	p := s.parkSug
	cctx, cancel := context.WithTimeout(ctx, removeTimeout)
	defer cancel()
	id, err := p.parkOne(cctx, a)
	if err != nil {
		log.Printf("park: automatic park of %s declined: %v", a.Name, err)
		return
	}
	p.mu.Lock()
	p.parked = append(p.parked, now)
	delete(p.m, key)
	p.mu.Unlock()
	why := "idle for a day, clean worktree"
	if s.isChatHelper(a) {
		why = "a chat helper idle for a day"
	}
	s.auditBy(ledger.AuditEvent{Action: "park.auto", By: "hub", OK: true, Host: a.Host,
		Detail: fmt.Sprintf("%s parked automatically after a day idle (%s) (record %s)", a.Name, why, id)})
	s.queueLine("park", id, id, fmt.Sprintf("%s was parked automatically (%s); resume it from Recently closed.", a.Name, why))
}
