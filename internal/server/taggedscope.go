package server

import (
	"net/http"
	"strings"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// Who may read another agent's text, and what a tagged node reaches.
//
// Output and transcripts hold tool output and possibly secrets. The user
// reads any of them. An agent reads those on its own host, and a recorded
// coordinator reads any host's. A tagged node reads none, its own included:
// authAgent refuses it on these routes before readGuard is reached, and
// nothing it runs needs them.
//
// The rule is checked where the fleet's roles are: the hub. A peer with -hub
// forwards its agents' reads there first (guard), so a read of a peer's own
// agent also passes through the hub; the hub then calls the peer as itself.
//
// The rest of this file limits the routes open to a tagged node (authWorker).
// A prompt or a handoff could otherwise ask another agent for the same text,
// or run something on another host, so its writes are limited too: see
// promptGuard, and handoffs are refused outright by authAgent. A handoff's
// --after would also paste the reports it waits on into the new worker's
// prompt.

// sameHost compares host names; an unknown host is nobody's.
func sameHost(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

// hostOf is the host a per-agent route names: bare routes mean this one.
func (s *Server) hostOf(r *http.Request) string {
	if h := r.PathValue("host"); h != "" {
		return h
	}
	return s.fleet.LocalName()
}

func readForbidden(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "read_forbidden", "message": "agents may read other agents' output only on their own host; the user and a recorded coordinator read any"})
}

func promptForbidden(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "prompt_forbidden", "message": "a tagged worker may prompt only a coordinator and agents on its own host"})
}

// readGuard applies the read rule to a per-agent route.
func (s *Server) readGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		if c.agent() && (c.tagged || (!sameHost(s.hostOf(r), c.host) && !s.callerIsCoordinator(c))) {
			readForbidden(w)
			return
		}
		next(w, r)
	}
}

// promptGuard limits a tagged node's prompts to the coordinators and to
// agents on its own host. Other callers pass.
func (s *Server) promptGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		host := s.hostOf(r)
		if c.tagged && !sameHost(host, c.host) && !s.targetIsCoordinator(host, r.PathValue("target")) {
			promptForbidden(w)
			return
		}
		next(w, r)
	}
}

// targetIsCoordinator reports whether target (a pane id or name) on host is
// a coordinator, which the fleet must see it as: a name that also fits
// another agent there is not good enough, since the host picks among them.
func (s *Server) targetIsCoordinator(host, target string) bool {
	found := false
	for _, a := range s.fleet.Agents() {
		if a.Host != host || (a.PaneID != target && a.Name != target) {
			continue
		}
		if !s.isCoordinator(a.ID, a.Name) {
			return false
		}
		found = true
	}
	return found
}

// taggedEntries is the ledger as a tagged node on host sees it: the entries
// on its host in full, the reports of the steps those wait on, and of the
// rest only what says what is running where. Fields are listed, not removed,
// so a new one stays hidden until someone decides otherwise.
func taggedEntries(views []entryView, host string) []entryView {
	after := map[string]bool{}
	for _, v := range views {
		if sameHost(v.Host, host) {
			for _, id := range v.After {
				after[id] = true
			}
		}
	}
	out := make([]entryView, 0, len(views))
	for _, v := range views {
		if sameHost(v.Host, host) {
			out = append(out, v)
			continue
		}
		e := v.Entry
		outline := ledger.Entry{
			ID: e.ID, Host: e.Host, RepoName: e.RepoName, Branch: e.Branch, Base: e.Base, Kind: e.Kind,
			AgentID: e.AgentID, PaneID: e.PaneID, AgentName: e.AgentName, Status: e.Status,
			CreatedAt: e.CreatedAt, FinishedAt: e.FinishedAt, RemovedAt: e.RemovedAt, After: e.After, PlanID: e.PlanID,
		}
		if after[e.ID] {
			outline.FinalReport = e.FinalReport
		}
		out = append(out, entryView{Entry: outline, Live: v.Live})
	}
	return out
}

// taggedMarks keeps the marks on host: a mark holds the session's own answer
// to whether it is safe to remove.
func taggedMarks(marks []ledger.Mark, host string) []ledger.Mark {
	out := []ledger.Mark{}
	for _, m := range marks {
		if sameHost(m.Host, host) {
			out = append(out, m)
		}
	}
	return out
}

// idHost is the host of a host-qualified agent id.
func idHost(id string) string {
	h, _, _ := strings.Cut(id, "/")
	return h
}

// taggedQueue keeps the prompts to or from host.
func taggedQueue(items []QueuedPrompt, host string) []QueuedPrompt {
	out := []QueuedPrompt{}
	for _, it := range items {
		if sameHost(idHost(it.Agent), host) || sameHost(idHost(it.FromID), host) {
			out = append(out, it)
		}
	}
	return out
}

// queuedBy reports whether host queued the prompt id.
func (s *Server) queuedBy(id, host string) bool {
	for _, it := range s.pq.list("", true) {
		if it.ID == id {
			return sameHost(idHost(it.FromID), host)
		}
	}
	return false
}

// todoVisible: a tagged node on host sees the items created there or waiting
// on an agent there.
func todoVisible(it Todo, host string) bool {
	return sameHost(idHost(it.CreatedByID), host) || (it.Waiting != nil && sameHost(idHost(it.Waiting.ID), host))
}

func taggedTodos(items []Todo, host string) []Todo {
	out := []Todo{}
	for _, it := range items {
		if todoVisible(it, host) {
			out = append(out, it)
		}
	}
	return out
}
