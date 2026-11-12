package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/state"
)

// The quiet mark: a worker herdr calls "working" that has written nothing to
// its session file for a long time. herdr's status says the agent is mid-turn,
// not that it is producing anything, and AgentInfo.LastActive is when comebye
// first saw the status, so a worker stuck on a hung tool call or a lost
// connection reads as busy for hours. The session file's mtime is the
// better sign: every message and tool result the harness records touches it.
//
// A running handoff worker that is working, whose session file is older than
// the threshold (and older than its current status, so a fresh turn is never
// quiet) and that has no background task the session records, gets
//
//   - AgentInfo.QuietSince, which the app and `comebye agents` show, and
//   - exactly one auto to-do ("Is <worker> stuck?") for its coordinator and
//     the user, which closes itself when the worker writes again, leaves
//     working, or ends.
//
// No takeover and no keys are sent: the coordinator or the user decides. It
// runs on the hub (it needs the ledger), and reads a peer's agent through the
// same latest route the ledger uses.

const (
	// DefaultQuietAfter is how long a working worker may leave its session
	// file untouched before it counts as quiet.
	DefaultQuietAfter = 30 * time.Minute

	// SourceQuiet marks an item filed because a worker went quiet.
	SourceQuiet = "hub-quiet"

	quietTick    = time.Minute
	quietRecheck = time.Minute // between reads of one agent's session file
	quietReads   = 8           // reads one step may start
)

type quietState struct {
	checked time.Time
	mtime   time.Time
	quiet   bool
	// since is the time the mark was last set: the last write the hub saw.
	since time.Time
	note  string // what the session is waiting on, when it says
}

type quietMon struct {
	after time.Duration
	mu    sync.Mutex
	m     map[string]*quietState
	// seams
	now func() time.Time
}

func (s *Server) initQuiet(after time.Duration) {
	if after <= 0 || s.ledger == nil {
		return
	}
	s.quiet = &quietMon{after: after, m: map[string]*quietState{}, now: time.Now}
}

// stampQuiet sets QuietSince on the agents the monitor currently holds as quiet.
func (s *Server) stampQuiet(agents []herdr.AgentInfo) {
	q := s.quiet
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range agents {
		if st := q.m[state.AgentKey(&agents[i])]; st != nil && st.quiet && agents[i].Status == herdr.StatusWorking {
			t := st.since
			agents[i].QuietSince = &t
		}
	}
}

// RunQuiet follows a clock until ctx is done.
func (s *Server) RunQuiet(ctx context.Context) {
	if s.quiet == nil || s.todos == nil {
		return
	}
	tick := time.NewTicker(quietTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.quietStep(ctx, s.fleet.Agents(), s.quiet.now())
		}
	}
}

func quietKey(id string, since time.Time) string {
	return SourceQuiet + ":" + id + "@" + since.UTC().Format(time.RFC3339)
}

// quietStep applies one fleet snapshot: it re-reads the working workers that
// have been working past the threshold, sets and clears the mark, files the
// to-do for a new quiet spell and closes the ones that are over.
func (s *Server) quietStep(ctx context.Context, agents []herdr.AgentInfo, now time.Time) {
	q := s.quiet
	if q == nil {
		return
	}
	byID := map[string]herdr.AgentInfo{}
	for _, a := range agents {
		byID[state.AgentKey(&a)] = a
	}
	changed := false
	reads := 0
	for id, a := range byID {
		if ctx.Err() != nil {
			return
		}
		q.mu.Lock()
		st := q.m[id]
		q.mu.Unlock()
		// Only a running handoff worker, working for longer than the threshold
		// (a fresh turn has written its prompt a moment ago).
		e, ok := s.ledger.ByAgent(id)
		candidate := ok && e.Status == ledger.Running && a.Status == herdr.StatusWorking &&
			a.StatusSince != nil && now.Sub(*a.StatusSince) >= q.after
		if !candidate {
			if st != nil {
				q.mu.Lock()
				delete(q.m, id)
				q.mu.Unlock()
				changed = changed || st.quiet
			}
			continue
		}
		if st != nil && now.Sub(st.checked) < quietRecheck {
			continue
		}
		if reads >= quietReads {
			continue
		}
		reads++
		rctx, cancel := context.WithTimeout(ctx, autoReadTimeout)
		l := s.latestOf(rctx, a.Host, a.PaneID)
		cancel()
		if l == nil {
			continue // cannot tell: keep what we had
		}
		mt, err := time.Parse(time.RFC3339Nano, l.Mtime)
		if err != nil {
			continue // a source that reports no mtime is never called quiet
		}
		// The later of the last write and the start of this working spell.
		ref := mt
		if a.StatusSince.After(ref) {
			ref = *a.StatusSince
		}
		isQuiet := now.Sub(ref) >= q.after && !(l.BackgroundKnown && len(l.Background) > 0)
		note := ""
		if isQuiet && len(l.Pending) > 0 {
			p := l.Pending[len(l.Pending)-1]
			note = p.Tool
			if p.Args != "" {
				note += " " + p.Args
			}
		}
		q.mu.Lock()
		if st == nil {
			st = &quietState{}
			q.m[id] = st
		}
		was := st.quiet
		st.checked, st.mtime, st.quiet, st.note = now, mt, isQuiet, note
		if isQuiet {
			st.since = ref
		}
		q.mu.Unlock()
		changed = changed || was != isQuiet
		if isQuiet && !was {
			s.quietFile(a, e, ref, now, note)
		}
	}
	s.quietClose(byID)
	// A mark on an agent that left the fleet.
	q.mu.Lock()
	for id, st := range q.m {
		if _, ok := byID[id]; !ok {
			changed = changed || st.quiet
			delete(q.m, id)
		}
	}
	q.mu.Unlock()
	if changed && s.fleet != nil {
		s.fleet.Changed()
	}
}

// quietFile files the one to-do of a quiet spell.
func (s *Server) quietFile(a herdr.AgentInfo, e ledger.Entry, since, now time.Time, note string) {
	id := state.AgentKey(&a)
	key := quietKey(id, since)
	if !s.todos.wantsAuto(key, id) {
		return // filed already, or somebody has an item about this agent
	}
	name := autoName(a)
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** is marked working but has written nothing to its session since %s (%s ago).",
		mdSafe(name), since.Local().Format("15:04"), roundMinutes(now.Sub(since)))
	if note != "" {
		fmt.Fprintf(&b, " Its last tool call has no result yet: `%s`.", mdSafe(oneLine(note, 100)))
	}
	b.WriteString(" The hub knows of no background task it is waiting on.\n\nOpen it to see whether it is stuck (a hung command, a lost connection) or just slow. Nothing was sent to it. This item closes when the worker writes again.")
	if e.Branch != "" {
		fmt.Fprintf(&b, "\n\nHandoff %s on `%s`.", e.ID, mdSafe(e.Branch))
	}
	it := Todo{
		Question:  oneLine("Is "+name+" stuck? It has been quiet for "+roundMinutes(now.Sub(since)), todoMaxQuestion),
		Context:   block(b.String(), todoMaxContext),
		Kind:      "action",
		Links:     []TodoLink{{Label: "Open " + oneLine(name, 60), URL: s.autoAgentLink(a)}},
		Waiting:   &TodoAgent{ID: id, Name: a.Name, Instance: s.hostInstance(a.Host)},
		CreatedBy: AutoCreator,
		Source:    SourceQuiet,
		AutoKey:   key,
	}
	out, err := s.todos.addAuto(it)
	switch {
	case err == nil:
		log.Printf("quiet: %s for %s (no write since %s)", out.ID, id, since.Format(time.RFC3339))
	case errors.Is(err, errAutoCap), errors.Is(err, errAutoHuman), errors.Is(err, errAutoDup), errors.Is(err, errTodoFull):
	default:
		log.Printf("quiet: %s: %v", id, err)
	}
}

// quietClose closes the open quiet items whose worker is no longer quiet.
func (s *Server) quietClose(byID map[string]herdr.AgentInfo) {
	q := s.quiet
	for _, it := range s.todos.list("") {
		if it.State != TodoOpen || it.Source != SourceQuiet || it.Waiting == nil {
			continue
		}
		reason := ""
		q.mu.Lock()
		st := q.m[it.Waiting.ID]
		q.mu.Unlock()
		a, ok := byID[it.Waiting.ID]
		switch {
		case !ok:
			reason = "the agent is gone"
		case a.Status != herdr.StatusWorking:
			reason = "the agent is no longer working"
		case st != nil && !st.quiet:
			reason = "the agent wrote again"
		case st == nil:
			// Not read yet since a restart: leave it while it is still a
			// running worker (the next read decides), close it otherwise.
			if e, ok := s.ledger.ByAgent(it.Waiting.ID); !ok || e.Status != ledger.Running {
				reason = "the agent is no longer a running worker"
			}
		}
		if reason != "" {
			if _, err := s.todos.close(it.ID, TodoDropped, AutoCreator, reason); err != nil && !errors.Is(err, errTodoClosed) {
				log.Printf("quiet: close %s: %v", it.ID, err)
			}
		}
	}
}
