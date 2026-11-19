package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// coordinatorInfo is one coordinator in GET /api/coordinators: who it is,
// whether it is main, and the open work that is its own (the ids, so a
// coordinator's digest can keep to them). Work nobody owns, and work whose
// owner is gone, counts as main's, the same rule the notices follow
// (route.go).
type coordinatorInfo struct {
	ID     string    `json:"id"`
	Host   string    `json:"host,omitempty"`
	PaneID string    `json:"pane_id,omitempty"`
	Name   string    `json:"name,omitempty"`
	Main   bool      `json:"main"`
	Status string    `json:"status,omitempty"` // the agent's status; "" when the hub does not see it now
	At     time.Time `json:"at"`               // when it was registered

	Handoffs []string `json:"handoffs"`
	// Closed are the ids of its newest handoffs and to-dos that are no
	// longer open (landed, removed, answered and done), so its digest can
	// list what happened to them.
	Closed []string `json:"closed"`
	Todos  []string `json:"todos"`
	Plans  []string `json:"plans"`
}

// openHandoff is a ledger status that still counts as the coordinator's work
// in hand (the digest lists the same ones).
func openHandoff(st ledger.Status) bool {
	switch st {
	case ledger.Waiting, ledger.Running, ledger.Finished, ledger.Landing:
		return true
	}
	return false
}

// ownerOfEntry is the coordinator an entry's notices go to: its requester,
// else its plan's creator, else the coordinator that last prompted its agent.
// Nil when nobody owns it.
func (s *Server) ownerOfEntry(e ledger.Entry) *ledger.Ref {
	ref := e.ReplyTo
	if ref == nil && e.PlanID != "" && s.ledger != nil {
		if p, ok := s.ledger.GetPlan(e.PlanID); ok {
			ref = p.ReplyTo
		}
	}
	if ref == nil && e.AgentID != "" && s.askers != nil {
		if a, ok := s.askers.get(e.AgentID); ok && (a.Name == "" || e.AgentName == "" || a.Name == e.AgentName) {
			c := a.Coord
			ref = &c
		}
	}
	return s.owningCoordinator(ref)
}

// maxClosedHandoffs bounds how many closed handoffs (and as many closed to-dos) are listed.
const maxClosedHandoffs = 200

// coordinatorList builds the list: every coordinator the hub knows, main
// first, then by registration.
func (s *Server) coordinatorList() []coordinatorInfo {
	roles := s.liveRoles()
	agents := map[string]coordinatorInfo{}
	for _, a := range s.fleet.Agents() {
		agents[a.ID] = coordinatorInfo{Host: a.Host, PaneID: a.PaneID, Status: string(a.Status)}
	}
	var out []coordinatorInfo
	idx := map[string]int{}
	mainID := ""
	for id, r := range roles {
		if r.Role != RoleCoordinator {
			continue
		}
		ci := agents[id]
		ci.ID, ci.Name, ci.Main, ci.At = id, r.Name, r.Main, r.At
		ci.Handoffs, ci.Closed, ci.Todos, ci.Plans = []string{}, []string{}, []string{}, []string{}
		if ci.Host == "" {
			ci.Host = idHost(id)
		}
		if r.Main {
			mainID = id
		}
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Main != out[j].Main {
			return out[i].Main
		}
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	for i, c := range out {
		idx[c.ID] = i
	}
	// who: the coordinator's index for an owner, main's when nobody owns it
	// or the owner is no longer a coordinator.
	who := func(o *ledger.Ref) (int, bool) {
		if o != nil {
			if i, ok := idx[o.AgentID]; ok {
				return i, true
			}
		}
		i, ok := idx[mainID]
		return i, ok
	}
	if s.ledger != nil {
		entries := s.ledger.Entries() // oldest first
		closed := 0
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			open := openHandoff(e.Status)
			if !open {
				if closed++; closed > maxClosedHandoffs {
					continue
				}
			}
			if c, ok := who(s.ownerOfEntry(e)); ok {
				if open {
					out[c].Handoffs = append(out[c].Handoffs, e.ID)
				} else {
					out[c].Closed = append(out[c].Closed, e.ID)
				}
			}
		}
		for _, p := range s.ledger.Plans() {
			if p.Outcome != "" {
				continue
			}
			if i, ok := who(s.owningCoordinator(p.ReplyTo)); ok {
				out[i].Plans = append(out[i].Plans, p.ID)
			}
		}
	}
	if s.todos != nil {
		items := s.todos.list("all")
		closed := 0
		for i := len(items) - 1; i >= 0; i-- { // newest last
			it := items[i]
			open := it.active()
			if !open {
				if closed++; closed > maxClosedHandoffs {
					continue
				}
			}
			var owner *ledger.Ref
			if it.Waiting != nil {
				owner = s.owningCoordinator(&ledger.Ref{AgentID: it.Waiting.ID, Name: it.Waiting.Name})
			}
			if c, ok := who(owner); ok {
				if open {
					out[c].Todos = append(out[c].Todos, it.ID)
				} else {
					out[c].Closed = append(out[c].Closed, it.ID)
				}
			}
		}
	}
	return out
}

// handleCoordinators is GET /api/coordinators: {coordinators: [...]}.
func (s *Server) handleCoordinators(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	writeJSON(w, map[string]any{"coordinators": s.coordinatorList()})
}
