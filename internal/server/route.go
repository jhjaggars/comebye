package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// Routing a notice to the coordinator that owns it (docs/agents.md,
// "Several coordinators").
//
// With more than one coordinator the hub must not treat them as a pool for
// a notice that belongs to one of them. A notice has an owner when it is
// about an agent that a coordinator started or prompted:
//
//   - a handoff: its ReplyTo (the agent that handed the work off);
//   - an agent a coordinator sent `comebye prompt` to that is not its
//     handoff (a user-started agent): the asker recorded here, until
//     another coordinator prompts it;
//   - a to-do item: the agent it waits on when that is a coordinator, else
//     the owner of the waiting agent.
//
// An owned notice goes to that coordinator and to nobody else: while it is
// busy the notice waits (the callers retry). Only when the owner is gone (its
// pane closed, or its role removed) does it fall back to the pool, and the
// notice then says whose it was. A notice nobody owns (a release, a sweep, a
// user-started agent no coordinator ever prompted) goes to the
// main coordinator: the first one registered, which stays main (durably, in
// roles.json) until its role is removed, then the oldest remaining one; the
// user may also mark another (comebye coordinator main). While main is busy
// or offline it waits; when main is not running any idle coordinator takes
// it, labelled as main's.

// askerMax bounds askers.json; the oldest records go first.
const askerMax = 200

// Asker records the coordinator that last prompted an agent that is not
// its handoff. Name is the target's name then (a reused pane id is another
// agent).
type Asker struct {
	Name  string     `json:"name,omitempty"`
	Coord ledger.Ref `json:"coord"`
	At    time.Time  `json:"at"`
}

// askerStore keeps askers.json: host-qualified target agent id -> Asker.
type askerStore struct {
	mu   sync.Mutex
	path string // "" keeps it in memory
	data map[string]Asker
}

func openAskers(path string) (*askerStore, error) {
	s := &askerStore{path: path, data: map[string]Asker{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	if s.data == nil {
		s.data = map[string]Asker{}
	}
	return s, nil
}

func (s *askerStore) get(id string) (Asker, bool) {
	if s == nil {
		return Asker{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.data[id]
	return a, ok
}

func (s *askerStore) set(id string, a Asker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.data[id]; ok && old.Coord == a.Coord && old.Name == a.Name {
		return nil
	}
	if _, ok := s.data[id]; !ok && len(s.data) >= askerMax {
		ids := make([]string, 0, len(s.data))
		for k := range s.data {
			ids = append(ids, k)
		}
		sort.Slice(ids, func(i, j int) bool { return s.data[ids[i]].At.Before(s.data[ids[j]].At) })
		for _, k := range ids[:len(ids)-askerMax+1] {
			delete(s.data, k)
		}
	}
	s.data[id] = a
	if s.path == "" {
		return nil
	}
	return writeFileAtomic(s.path, s.data)
}

func askersPath(dir string) string { return filepath.Join(dir, "askers.json") }

// isCoordinatorID: the agent id has a coordinator role (the name is not
// checked: use it to tell a coordinator that went away from an agent that
// never was one).
func (s *Server) isCoordinatorID(id string) bool {
	if s.roles == nil {
		return false
	}
	s.roles.mu.Lock()
	defer s.roles.mu.Unlock()
	r, ok := s.roles.data[id]
	return ok && r.Role == RoleCoordinator
}

// roleName is the name recorded with an agent's role.
func (s *Server) roleName(id string) string {
	if s.roles == nil {
		return ""
	}
	s.roles.mu.Lock()
	defer s.roles.mu.Unlock()
	return s.roles.data[id].Name
}

// recordAsker notes that coord prompted target, when that makes coord the
// target's owner: target is not a coordinator and not a handoff that has a
// requester (that requester stays its owner).
func (s *Server) recordAsker(coord ledger.Ref, target herdr.AgentInfo) {
	if s.askers == nil || coord.AgentID == "" || coord.AgentID == target.ID || !s.isCoordinator(coord.AgentID, coord.Name) || s.isCoordinatorID(target.ID) {
		return
	}
	if s.ledger != nil {
		if e, ok := s.ledger.ByAgent(target.ID); ok && e.ReplyTo != nil {
			return
		}
	}
	if err := s.askers.set(target.ID, Asker{Name: target.Name, Coord: coord, At: time.Now().UTC()}); err != nil {
		log.Printf("askers: %v", err)
	}
}

// ownerOfAgent is whoever owns the agent's notices: its handoff's
// requester (or its plan's creator), else the coordinator that last
// prompted it. Nil when nobody does.
func (s *Server) ownerOfAgent(id, name string) *ledger.Ref {
	if s.ledger != nil {
		if e, ok := s.ledger.ByAgent(id); ok {
			if e.ReplyTo != nil && e.ReplyTo.AgentID != "" && e.ReplyTo.AgentID != id {
				r := *e.ReplyTo
				return &r
			}
			if e.PlanID != "" {
				if p, ok := s.ledger.GetPlan(e.PlanID); ok && p.ReplyTo != nil && p.ReplyTo.AgentID != id {
					r := *p.ReplyTo
					return &r
				}
			}
		}
	}
	if a, ok := s.askers.get(id); ok && (a.Name == "" || name == "" || a.Name == name) {
		c := a.Coord
		return &c
	}
	return nil
}

// owningCoordinator follows owners up from ref (a worker that handed off a
// sub-worker is itself owned) to the coordinator at the top. Nil when the
// chain ends at something that is not a coordinator.
func (s *Server) owningCoordinator(ref *ledger.Ref) *ledger.Ref {
	for i := 0; ref != nil && i < 5; i++ {
		if s.isCoordinatorID(ref.AgentID) {
			return ref
		}
		ref = s.ownerOfAgent(ref.AgentID, ref.Name)
	}
	return nil
}

// Why a notice has nowhere to go now (coordRoute.Why).
const (
	routeNoCoordinator = "no_coordinator"
	routeBusy          = "busy"
	// routeUnknown: the hub cannot tell whether a coordinator is running (a
	// recorded one is not in the fleet, or the fleet is empty), so the
	// caller asks again later.
	routeUnknown = "unknown"
)

// coordRoute is where a notice goes. Agent nil means nobody now: Why says
// why (routeBusy waits for the owner, retried by the caller). Gone is the
// name of the owning coordinator when the notice fell back to the pool
// because that coordinator is gone.
type coordRoute struct {
	Agent *herdr.AgentInfo
	Why   string
	Gone  string
}

// routeCoordinator picks the coordinator for a notice owned by owner (nil:
// nobody owns it, so the pool, home first).
func (s *Server) routeCoordinator(owner *ledger.Ref) coordRoute {
	owner = s.owningCoordinator(owner)
	if owner == nil {
		return s.poolRoute()
	}
	agents := s.fleet.Agents()
	hostSeen := map[string]bool{}
	for _, a := range agents {
		hostSeen[a.Host] = true
		if a.ID != owner.AgentID {
			continue
		}
		if owner.Name != "" && a.Name != "" && a.Name != owner.Name {
			break // the pane now runs another agent: the coordinator is gone
		}
		if a.Status == herdr.StatusIdle || a.Status == herdr.StatusDone {
			a := a
			return coordRoute{Agent: &a}
		}
		return coordRoute{Why: routeBusy}
	}
	// Not in the fleet. A host that reports no agents may be down or still
	// starting: wait rather than call its coordinator gone.
	if len(agents) == 0 || !hostSeen[idHost(owner.AgentID)] {
		return coordRoute{Why: routeUnknown}
	}
	gone := owner.Name
	if gone == "" {
		gone = s.roleName(owner.AgentID)
	}
	if gone == "" {
		gone = owner.AgentID
	}
	rt := s.poolRoute()
	if rt.Agent != nil {
		rt.Gone = gone // the pool takes it for the coordinator that is gone
	}
	return rt
}

// goneNote is the sentence a notice that fell back to the pool gains.
func goneNote(gone string) string {
	if gone == "" {
		return ""
	}
	return " (This was meant for coordinator " + safeText(gone) + ", which is gone.)"
}

// goneMainNote labels a notice for the main coordinator that another took.
func goneMainNote(gone string) string {
	if gone == "" {
		return ""
	}
	return " (This is for the main coordinator, " + safeText(gone) + ", which is not running.)"
}

// noteAsker records the calling coordinator as the owner of the prompt's
// target when the prompt went through. It wraps the prompt routes on the hub,
// so a prompt forwarded to a peer's agent is recorded here too.
func (s *Server) noteAsker(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		if !c.agent() || c.pane == "" || s.askers == nil {
			next(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		next(sw, r)
		if sw.status != 0 && (sw.status < 200 || sw.status >= 300) {
			return
		}
		host := r.PathValue("host")
		if host == "" {
			host = s.fleet.LocalName()
		}
		target := r.PathValue("target")
		for _, a := range s.fleet.Agents() {
			if a.Host != host && !(a.Host == "" && host == s.fleet.LocalName()) {
				continue
			}
			if a.PaneID == target || (a.Name != "" && a.Name == target) {
				if ref := s.replyRef(r); ref != nil {
					s.recordAsker(*ref, a)
				}
				return
			}
		}
	}
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// get is the role recorded for id.
func (s *roleStore) get(id string) (Role, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[id]
	return r, ok
}

// isMain: the role of id is marked main.
func (s *roleStore) isMain(id string) bool {
	r, _ := s.get(id)
	return r.Main
}

// setMain marks id the main coordinator and clears the mark elsewhere.
func (s *roleStore) setMain(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[id]
	if !ok || r.Role != RoleCoordinator {
		return errors.New("not a coordinator")
	}
	for k, v := range s.data {
		if v.Main && k != id {
			v.Main = false
			s.data[k] = v
		}
	}
	r.Main = true
	s.data[id] = r
	return s.saveLocked()
}

// poolRoute picks the coordinator for a notice nobody owns: the main
// coordinator (marked by the user; else the first registered live one). When
// main is busy the notice waits (Why routeBusy; the callers retry). When main
// is not running, any idle coordinator takes it and Gone names main. Without
// a coordinator it says why: none running, or unknown (a recorded one is not
// in the fleet: its host may be down or still starting; never a miss).
func (s *Server) poolRoute() coordRoute {
	roles := s.liveRoles()
	agents := s.fleet.Agents()
	seen := map[string]bool{}
	hostSeen := map[string]bool{}
	type cand struct {
		a  herdr.AgentInfo
		at time.Time
	}
	var live []cand
	for _, a := range agents {
		seen[a.ID] = true
		hostSeen[a.Host] = true
		r, ok := roles[a.ID]
		if !ok || r.Role != RoleCoordinator || (r.Name != "" && a.Name != "" && a.Name != r.Name) {
			continue
		}
		live = append(live, cand{a, r.At})
	}
	ready := func(a herdr.AgentInfo) bool { return a.Status == herdr.StatusIdle || a.Status == herdr.StatusDone }
	local := s.fleet.LocalName()
	sort.SliceStable(live, func(i, j int) bool {
		if li, lj := live[i].a.Host == local, live[j].a.Host == local; li != lj {
			return li
		}
		if !live[i].at.Equal(live[j].at) {
			return live[i].at.Before(live[j].at)
		}
		return live[i].a.ID < live[j].a.ID
	})
	// Main: the marked one (even when it is not running), else the first
	// registered live one.
	mainID, mainName := "", ""
	for id, r := range roles {
		if r.Role == RoleCoordinator && r.Main {
			mainID, mainName = id, r.Name
		}
	}
	if mainID == "" && len(live) > 0 {
		mainID, mainName = live[0].a.ID, live[0].a.Name
	}
	for _, c := range live {
		if c.a.ID == mainID {
			if ready(c.a) {
				a := c.a
				return coordRoute{Agent: &a}
			}
			return coordRoute{Why: routeBusy}
		}
	}
	// Main is not running (or there is none).
	for _, c := range live {
		if ready(c.a) {
			a := c.a
			rt := coordRoute{Agent: &a}
			if mainID != "" {
				rt.Gone = orDefault(mainName, mainID)
			}
			return rt
		}
	}
	switch {
	case len(live) > 0:
		return coordRoute{Why: routeBusy}
	case len(agents) == 0:
		return coordRoute{Why: routeUnknown}
	}
	for id, r := range roles {
		if r.Role == RoleCoordinator && !seen[id] {
			return coordRoute{Why: routeUnknown}
		}
	}
	return coordRoute{Why: routeNoCoordinator}
}
