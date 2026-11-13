package server

import (
	"github.com/jhjaggars/comebye/internal/scratch"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// Browsers get a slim view of the agent list: only the fields the app
// reads. herdr's full records (session paths, terminal ids, revisions,
// focus) are about three times larger and change without anything on the
// phone changing, which costs radio time and redraws on every update. A
// hub reading a peer asks for the full records with ?full=1: it needs the
// session to fetch notification excerpts.

type browserAgent struct {
	ID            string              `json:"id,omitempty"`
	Host          string              `json:"host,omitempty"`
	PaneID        string              `json:"pane_id"`
	Name          string              `json:"name,omitempty"`
	Agent         string              `json:"agent,omitempty"`
	DisplayAgent  string              `json:"display_agent,omitempty"`
	Status        herdr.AgentStatus   `json:"agent_status"`
	StateLabels   map[string]string   `json:"state_labels,omitempty"`
	CWD           string              `json:"cwd,omitempty"`
	TerminalTitle string              `json:"terminal_title_stripped,omitempty"`
	Worktree      *herdr.WorktreeInfo `json:"worktree,omitempty"`
	LastActive    *time.Time          `json:"last_active,omitempty"`
	StatusSince   *time.Time          `json:"status_since,omitempty"`
	// Instance is the host's incarnation (the hub's peer registry).
	Instance string `json:"instance,omitempty"`
	// BlockedOn says what a blocked agent is blocked on, once the hub has
	// read its dialog.
	BlockedOn *blockedOn `json:"blocked_on,omitempty"`
	// StatusNote: herdr said blocked, the screen shows no dialog and a
	// running spinner, so the agent is reported as working (state.verifyBlocked).
	StatusNote string `json:"status_note,omitempty"`
	// Background: an idle or done agent's running background tasks (a test
	// run, a CI watch, a Monitor), which will wake it when they end.
	Background []bgBrief `json:"background,omitempty"`
	// QuietSince: a working handoff worker has written nothing for a long time (quiet.go).
	QuietSince *time.Time `json:"quiet_since,omitempty"`
	// ParkHint: the hub suggests parking this agent (parksuggest.go).
	ParkHint *herdr.ParkHint `json:"park_hint,omitempty"`
}

// browserHost drops LastSeen, which changes with every peer ping and is
// not shown. Class and Since (the hub's diagnosis of a down peer and
// when it went down) are kept; so is Addr when the peer has more than
// one address (which one answered).
type browserHost struct {
	Name   string     `json:"name"`
	Local  bool       `json:"local,omitempty"`
	Status string     `json:"status"`
	Error  string     `json:"error,omitempty"`
	Class  string     `json:"class,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
	Addr   string     `json:"addr,omitempty"`
	// Version is the host's binary version ("" = unknown); VersionMismatch
	// marks a peer that differs from the hub, for the app's badge.
	Version         string             `json:"version,omitempty"`
	VersionMismatch bool               `json:"version_mismatch,omitempty"`
	Agents          int                `json:"agents"`
	NoAgents        bool               `json:"no_agents,omitempty"`
	RepoDamage      []state.RepoDamage `json:"repo_damage,omitempty"`
	Tmp             *scratch.Usage     `json:"tmp,omitempty"`
}

type browserEvent struct {
	Type    string         `json:"type"`
	Reason  string         `json:"reason,omitempty"`
	Agents  []browserAgent `json:"agents,omitempty"`
	Hosts   []browserHost  `json:"hosts,omitempty"`
	Version string         `json:"version,omitempty"`
}

func (s *Server) slimAgents(agents []herdr.AgentInfo) []browserAgent {
	out := make([]browserAgent, len(agents))
	agents = append([]herdr.AgentInfo(nil), agents...) // stamped below; the fleet's copy stays as it is
	s.stampQuiet(agents)
	s.stampPark(agents)
	for i, a := range agents {
		out[i] = browserAgent{
			ID: a.ID, Host: a.Host, PaneID: a.PaneID, Name: a.Name,
			Agent: a.Agent, DisplayAgent: a.DisplayAgent, Status: a.Status,
			StateLabels: a.StateLabels, CWD: a.CWD, TerminalTitle: a.TitleStripped,
			Worktree: a.Worktree, LastActive: a.LastActive, StatusSince: a.StatusSince, Instance: a.Instance,
			BlockedOn: s.blockedOnOf(a), StatusNote: a.StatusNote, Background: s.backgroundOfAgent(a),
			QuietSince: a.QuietSince, ParkHint: a.ParkHint,
		}
	}
	return out
}

func slimHosts(hosts []state.HostStatus) []browserHost {
	out := make([]browserHost, len(hosts))
	for i, h := range hosts {
		out[i] = browserHost{Name: h.Name, Local: h.Local, Status: h.Status, Error: h.Error, Class: h.Class, Since: h.Since, Addr: h.Addr, Version: h.Version, VersionMismatch: h.VersionMismatch, Agents: h.Agents, NoAgents: h.NoAgents, RepoDamage: h.RepoDamage, Tmp: h.Tmp}
	}
	return out
}

func (s *Server) slimEvent(ev state.Event) browserEvent {
	out := browserEvent{Type: ev.Type, Reason: ev.Reason, Version: ev.Version}
	if ev.Agents != nil {
		out.Agents = s.slimAgents(ev.Agents)
	}
	if ev.Hosts != nil {
		out.Hosts = slimHosts(ev.Hosts)
	}
	return out
}
