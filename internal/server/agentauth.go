package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/agenttoken"
	"github.com/jhjaggars/comebye/internal/fleet"
)

// Agents as callers. The comebye CLI, run by an agent (a coordinator) on
// this host, sends the local agent token (agenttoken, in the state
// directory) instead of a Tailscale identity. Such requests get the
// "agent" role, which may only read (agents, hosts, transcripts, output,
// the ledger, inspections of an agent's git state, the briefing, roles,
// the playbook) and do the workflow's steps: hand off, prompt an agent,
// mark or unmark (proposing a discard included), publish the briefing and
// set its schedule (or dismiss a streak of missed scheduled briefings).
// Everything else (keys, answers, images, launching, workers, removals and
// their confirmation, push) is the user's alone: 403.
//
// On a peer with -hub, the agent role's requests are forwarded to the hub
// (which keeps the one ledger), marked X-Comebye-Forwarded; the hub
// authenticates the peer by its Tailscale identity and applies the agent
// role to the forwarded request again.
//
// This keeps a well-behaved coordinator inside the workflow and prevents
// accidents. It is not a boundary against a malicious local agent: a
// process with a shell can read the token file, forge the identity header
// on the loopback port, or run git itself. The user's confirmation in the
// app is what guards removals.

const (
	// HeaderAgentToken carries the local agent token.
	HeaderAgentToken = "X-Comebye-Agent-Token"
	// HeaderAgent is the calling agent's herdr pane id ($HERDR_PANE_ID).
	HeaderAgent = "X-Comebye-Agent"
	// HeaderAgentHost is the calling agent's host: set by a peer that
	// forwards to its hub (a local CLI's value is informational; the
	// server's own name is used).
	HeaderAgentHost = "X-Comebye-Agent-Host"
	// HeaderForwarded marks a request a peer forwarded for one of its
	// agents.
	HeaderForwarded = "X-Comebye-Forwarded"
)

type role int

const (
	roleUser role = iota
	roleAgent
)

// caller is who made a request.
type caller struct {
	role role
	// For the agent role: the agent's pane and host ("" when unknown),
	// and whether a peer forwarded the request.
	pane, host string
	forwarded  bool
}

type callerKey struct{}

// callerOf returns the request's caller (the user when not set).
func callerOf(r *http.Request) caller {
	c, _ := r.Context().Value(callerKey{}).(caller)
	return c
}

func (c caller) agent() bool { return c.role == roleAgent }

// idRE bounds the attribution headers to what herdr's pane ids and host
// names look like.
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,63}$`)

func cleanID(s string) string {
	s = strings.TrimSpace(s)
	if !idRE.MatchString(s) {
		return ""
	}
	return s
}

// identify authenticates a request: the agent token makes it the agent
// role (an invalid token is refused, never downgraded to anything else);
// otherwise the Tailscale identity, as before, with a peer's forwarded
// agent request turned into the agent role.
func (s *Server) identify(r *http.Request) (caller, bool) {
	if tok := r.Header.Get(HeaderAgentToken); tok != "" {
		if !agenttoken.Equal(tok, s.cfg.AgentToken) {
			return caller{}, false
		}
		return caller{role: roleAgent, pane: cleanID(r.Header.Get(HeaderAgent)), host: s.fleet.LocalName()}, true
	}
	if !s.authorized(r) {
		return caller{}, false
	}
	if r.Header.Get(HeaderForwarded) == "1" {
		return caller{role: roleAgent, pane: cleanID(r.Header.Get(HeaderAgent)), host: strings.ToLower(cleanID(r.Header.Get(HeaderAgentHost))), forwarded: true}, true
	}
	return caller{role: roleUser}, true
}

// access is what a route admits.
type access int

const (
	userOnly access = iota
	// agentOK: the agent role may call it; on a peer with -hub it is
	// forwarded to the hub.
	agentOK
	// agentLocal: the agent role may call it, always answered here.
	agentLocal
)

// guard wraps a handler with the common API headers, authentication and
// the role check for the route.
func (s *Server) guard(a access, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Every API response (proxied peer calls included) carries this
		// instance's app version, so a stale page notices a deploy.
		h.Set("X-App-Version", s.version)
		// API responses are never sniffed or stored. A handler that wants
		// revalidation instead (transcripts, with an ETag) overrides
		// Cache-Control.
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		c, ok := s.identify(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if c.agent() {
			switch {
			case a == userOnly:
				writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "agent_forbidden", "message": "agents cannot do this through Comebye; it is left to the user in the app"})
				return
			case a == agentOK && s.cfg.Upstream != nil && !c.forwarded:
				s.forwardToHub(w, r, c)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	}
}

// auth admits the user only.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc { return s.guard(userOnly, next) }

// authAgent admits the user and the agent role (forwarded to the hub on a
// peer with -hub).
func (s *Server) authAgent(next http.HandlerFunc) http.HandlerFunc { return s.guard(agentOK, next) }

// authAgentLocal admits the user and the agent role, answered here.
func (s *Server) authAgentLocal(next http.HandlerFunc) http.HandlerFunc {
	return s.guard(agentLocal, next)
}

// forwardToHub sends an agent's request to the hub. Per-agent calls on
// this host's own routes (/api/agents/{target}/...) become host-qualified
// ones, since the hub's bare routes mean its own agents.
func (s *Server) forwardToHub(w http.ResponseWriter, r *http.Request, c caller) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) >= 3 && segs[0] == "api" && segs[1] == "agents" {
		segs = append([]string{"api", "hosts", s.fleet.LocalName()}, segs[1:]...)
	}
	maxBody, timeout := int64(64<<10), 30*time.Second
	switch {
	case r.URL.Path == "/api/handoffs" || strings.HasPrefix(r.URL.Path, "/api/handoffs/"):
		maxBody, timeout = maxLaunchPrompt+64<<10, workerTimeout+30*time.Second
	case strings.HasSuffix(r.URL.Path, "/prompt"):
		maxBody, timeout = 1<<20, 45*time.Second
	case r.URL.Path == "/api/brief":
		maxBody = maxBriefBytes + 16<<10
	}
	// Only the query parameters the agent role's routes read.
	q := url.Values{}
	for _, k := range []string{"limit", "skip", "from", "lines", "source", "agent", "diff"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	req := fleet.UpstreamRequest{
		Method: r.Method, Segments: segs, Query: q, MaxBody: maxBody, Timeout: timeout,
		Header: http.Header{HeaderForwarded: {"1"}, HeaderAgentHost: {s.fleet.LocalName()}},
	}
	if c.pane != "" {
		req.Header.Set(HeaderAgent, c.pane)
	}
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "too_large", "message": "the request is too large"})
			return
		}
		req.Body = b
	}
	s.cfg.Upstream.Forward(r.Context(), w, req)
}

// agentRef is the calling agent's host-qualified id ("" when unknown).
func (c caller) agentRef() string {
	if c.pane == "" || c.host == "" {
		return ""
	}
	return c.host + "/" + c.pane
}

// by names who made a request, for the ledger's "from" and "by": the
// agent (its name and id) for the agent role, else the body's value or
// the Tailscale login.
func (s *Server) by(r *http.Request, given string) string {
	c := callerOf(r)
	if !c.agent() {
		return requester(r, given)
	}
	ref := c.agentRef()
	if ref == "" {
		host := c.host
		if host == "" {
			host = s.fleet.LocalName()
		}
		return "an agent on " + host
	}
	for _, a := range s.fleet.Agents() {
		if a.ID == ref && a.Name != "" {
			return a.Name + " (" + ref + ")"
		}
	}
	return ref
}

// handleWhoami tells the CLI how this instance sees it.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	resp := map[string]any{"host": s.fleet.LocalName(), "role": "user", "ledger": s.ledger != nil}
	switch {
	case s.ledger != nil:
		resp["hub"] = "self"
	case s.cfg.Upstream != nil:
		resp["hub"] = s.cfg.Upstream.URL()
	default:
		resp["hub"] = ""
	}
	if c.agent() {
		resp["role"] = "agent"
		resp["agent"] = c.agentRef()
		resp["by"] = s.by(r, "")
	}
	writeJSON(w, resp)
}
