package server

import (
	"context"
	"net/http"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// Inspecting an agent's git state, for a coordinator deciding whether a
// worker's work can go (`comebye inspect`):
//
//	GET /api/inspect?agent=<host>/<pane>[&diff=1]
//
// The checks run on the agent's own host (through the hub for a peer's):
// its worktree for a ledger worker or an agent in a linked worktree, else
// the checkout around its working directory. Read-only, for the user and
// the agent role.

// inspection is the answer: who, and what the checks found.
type inspection struct {
	AgentID string            `json:"agent_id"`
	Host    string            `json:"host"`
	Name    string            `json:"name,omitempty"`
	Status  herdr.AgentStatus `json:"status,omitempty"` // "" when the agent is gone
	EntryID string            `json:"entry_id,omitempty"`
	Branch  string            `json:"branch,omitempty"`
	Base    string            `json:"base,omitempty"`
	// Model and Thinking: what the handoff asked for (empty: the default).
	Model    string          `json:"model,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	Checks   *worktreeChecks `json:"checks"`
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("agent")
	var agent *herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		if a.ID == id {
			agent = &a
			break
		}
	}
	var entry ledger.Entry
	isWorker := false
	if s.ledger != nil {
		entry, isWorker = s.ledger.ByAgent(id)
	}
	if agent == nil && !isWorker {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "agent_not_found", "message": "no such agent"})
		return
	}
	in := inspection{AgentID: id}
	var path string
	opts := checkOpts{Detail: true, Diff: r.URL.Query().Get("diff") == "1"}
	if isWorker {
		in.Host, in.Name, in.EntryID, in.Branch, in.Base, path = entry.Host, entry.AgentName, entry.ID, entry.Branch, entry.Base, entry.Worktree
		in.Model, in.Thinking = entry.Model, entry.Thinking
	}
	if agent != nil {
		in.Host, in.Status = agent.Host, agent.Status
		if agent.Name != "" {
			in.Name = agent.Name
		}
		if path == "" {
			if agent.Worktree != nil && agent.Worktree.CheckoutPath != "" {
				path = agent.Worktree.CheckoutPath
			} else {
				path, opts.Within = agent.CWD, true
			}
		}
	}
	if path == "" {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "no_checkout", "message": "the agent has no known checkout"})
		return
	}
	if !s.knownHost(in.Host) {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_host", "message": "no such host: " + in.Host})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	c, lerr := s.checksOn(ctx, in.Host, path, in.Branch, in.Base, opts)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	in.Checks = c
	writeJSON(w, in)
}
