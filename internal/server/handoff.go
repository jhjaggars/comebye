package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Handoffs and pruning, on the hub (or a single instance): the ledger
// (internal/ledger) records each handoff, the hub runs the worker on the
// owning host (locally or through fleet.CallHost), follows the worker until
// it finishes and keeps its final report. Sessions can be marked ready to
// remove; the review list shows each with fresh safety checks from its
// host, and a confirmed removal runs there too. Comebye never marks or
// removes anything on its own.

// defaultHandoffTemplate wraps a handoff's brief. A handoff-template.md in
// the state directory replaces it. The hub fills {{repo}}, {{branch}},
// {{brief}}, {{from}} and {{host}}; the worker's host fills {{worktree}} and
// {{base}}.
const defaultHandoffTemplate = `You are a worker agent on {{repo}}, handed this task by {{from}}.

## Your task

{{brief}}

## How to work

- Work only in this git worktree, {{worktree}} (your current directory), on branch ` + "`{{branch}}`" + ` (started from ` + "`{{base}}`" + `). Do not check out other branches or edit other checkouts.
- When you are done, commit your work on ` + "`{{branch}}`" + `. Do not merge, rebase or push other branches: merging is left to the user.
- End by running ` + "`comebye report -`" + ` with your final report on its standard input (a quoted heredoc is easiest), in exactly this markdown format, and then stop. That is how {{from}} learns you are done; if the command is not available, make the report your last message instead.

## Summary
<what you did and the outcome, in a short paragraph>

## Changes
<the commits and the files changed, as a list>

## Verification
<what you ran (tests, builds, checks) and the results>

## Follow-ups
<what is left to do, risks or open questions; "None." if nothing>
`

const (
	// maxTemplate caps handoff-template.md.
	maxTemplate = 64 << 10
	// maxBrief leaves room for the template within the prompt cap.
	maxBrief = maxLaunchPrompt - maxTemplate
	// checksTTL is how long cached checks back the ledger's hints.
	checksTTL = 5 * time.Minute
	// reportRetry spaces attempts to find an empty final report.
	reportRetry = 20 * time.Second
	// removedShown caps the removed and failed entries the ledger lists.
	removedShown = 20
)

// coord is the hub's handoff state besides the ledger file.
type coord struct {
	mu        sync.Mutex
	checks    map[string]cachedChecks // by entry id
	checking  map[string]bool
	reporting map[string]time.Time // entry id -> last capture attempt
	starting  map[string]bool      // handoffs being started, by host, repo and branch
	started   map[string]time.Time // entry id -> when its worker was (re)started
	quiet     map[string]time.Time // agent id -> no notifications until
	calling   map[string]time.Time // entry id -> last callback attempt
}

type cachedChecks struct {
	c  worktreeChecks
	at time.Time
}

// handoffTemplate is the state directory's handoff-template.md, else the
// built-in template.
func (s *Server) handoffTemplate() string {
	if s.cfg.StateDir != "" {
		f, err := os.Open(filepath.Join(s.cfg.StateDir, "handoff-template.md"))
		if err == nil {
			defer f.Close()
			b := make([]byte, maxTemplate+1)
			n, _ := f.Read(b)
			if n > 0 && n <= maxTemplate {
				return string(b[:n])
			}
			if n > maxTemplate {
				log.Printf("handoff-template.md is larger than %d bytes; using the built-in template", maxTemplate)
			}
		}
	}
	return defaultHandoffTemplate
}

// ledgerOff answers when this instance keeps no ledger (no state
// directory, or a peer that is not the hub). An agent is told that this
// instance has no hub to forward to (409); the app gets 503 and hides the
// ledger's features.
func (s *Server) ledgerOff(w http.ResponseWriter, r *http.Request) bool {
	if s.ledger != nil {
		return false
	}
	if callerOf(r).agent() {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "no_hub", "message": "no hub configured: this Comebye keeps no ledger; start it with -hub https://<hub>.<tailnet>.ts.net (COMEBYE_HUB) to forward to the hub"})
		return true
	}
	writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"code": "ledger_disabled", "message": "this instance keeps no handoff ledger"})
	return true
}

// startGrace is how long a worker just started counts as running even if
// the (debounced, or a peer's streamed) agent list does not show it yet.
const startGrace = 2 * time.Minute

// agentPresent reports whether a ledger worker's agent is still running:
// asked of herdr itself on this host; from the fleet's view for a peer,
// assumed when the hub cannot see that peer right now. A worker started a
// moment ago counts as running.
func (s *Server) agentPresent(ctx context.Context, e ledger.Entry) bool {
	if e.AgentID == "" {
		return false
	}
	s.coord.mu.Lock()
	at, ok := s.coord.started[e.ID]
	s.coord.mu.Unlock()
	if ok && time.Since(at) < startGrace {
		return true
	}
	if e.Host == s.fleet.LocalName() && s.client != nil {
		if live, err := s.client.ListAgents(ctx); err == nil {
			for _, a := range live {
				if state.QualifiedID(e.Host, a.PaneID) == e.AgentID {
					return true
				}
			}
			return false
		}
	}
	for _, h := range s.fleet.Hosts() {
		if h.Name == e.Host && !h.Local && h.Status != fleet.StatusOK {
			return true
		}
	}
	for _, a := range s.fleet.Agents() {
		if a.ID == e.AgentID {
			return true
		}
	}
	return false
}

// knownHost reports whether host is this instance or one of its peers.
func (s *Server) knownHost(host string) bool {
	return host == s.fleet.LocalName() || s.fleet.HasPeer(host)
}

// opFailure decodes a host's {code, message} error answer.
func opFailure(status int, body []byte) *launchError {
	var e struct{ Code, Message string }
	_ = json.Unmarshal(body, &e)
	if e.Code == "" {
		e.Code = "host_error"
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	return &launchError{status: status, code: e.Code, message: e.Message}
}

// callHost runs a worker op on a peer and decodes a 200 answer into out.
func (s *Server) callHost(ctx context.Context, host, op string, query url.Values, in, out any) *launchError {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return &launchError{status: http.StatusInternalServerError, code: "encode", message: err.Error()}
		}
	}
	status, data, err := s.fleet.CallHost(ctx, host, op, query, body)
	if err != nil {
		return &launchError{status: http.StatusBadGateway, code: "host_unreachable", message: err.Error()}
	}
	if status != http.StatusOK {
		return opFailure(status, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &launchError{status: http.StatusBadGateway, code: "bad_answer", message: host + " sent an unexpected answer"}
	}
	return nil
}

// workerOn starts a worker on host.
func (s *Server) workerOn(ctx context.Context, host string, b workerBody) (*workerResult, *launchError) {
	if host == s.fleet.LocalName() {
		return s.startWorker(ctx, b)
	}
	var res workerResult
	if e := s.callHost(ctx, host, "worker", nil, b, &res); e != nil {
		return nil, e
	}
	return &res, nil
}

// checksOn runs a worktree's checks on host.
func (s *Server) checksOn(ctx context.Context, host, path, branch, base string, opts checkOpts) (*worktreeChecks, *launchError) {
	if host == s.fleet.LocalName() {
		return s.localChecks(ctx, path, branch, base, opts)
	}
	var c worktreeChecks
	q := opts.query()
	q.Set("path", path)
	q.Set("branch", branch)
	q.Set("base", base)
	if e := s.callHost(ctx, host, "checks", q, nil, &c); e != nil {
		return nil, e
	}
	return &c, nil
}

// removeOn removes a session on host.
func (s *Server) removeOn(ctx context.Context, host string, b removeBody) (*removeResult, *launchError) {
	if host == s.fleet.LocalName() {
		return s.removeSession(ctx, b)
	}
	var res removeResult
	if e := s.callHost(ctx, host, "remove", nil, b, &res); e != nil {
		return nil, e
	}
	return &res, nil
}

// handoffBody is the JSON body of a handoff.
type handoffBody struct {
	Host   string `json:"host"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Brief  string `json:"brief"`
	From   string `json:"from"`
}

// requester names who asked: the body's from, else the Tailscale login.
func requester(r *http.Request, from string) string {
	from = strings.TrimSpace(from)
	if from == "" {
		from = strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
	}
	if from == "" {
		from = "the user"
	}
	if len(from) > 100 {
		from = from[:100]
	}
	return from
}

// handleHandoff starts a worker on a host and records it: POST {host,
// repo, branch, base?, kind?, name?, brief, from?}. A worker that fails to
// start is recorded as failed.
func (s *Server) handleHandoff(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b handoffBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLaunchPrompt+64<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if b.Host == "" {
		b.Host = s.fleet.LocalName()
	}
	switch {
	case !s.knownHost(b.Host):
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_host", "message": "no such host"})
		return
	case strings.TrimSpace(b.Brief) == "":
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_brief", "message": "a brief is required"})
		return
	case len(b.Brief) > maxBrief:
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "prompt_too_large", "message": "the brief is too long"})
		return
	case !validBranch(strings.TrimSpace(b.Branch)):
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_branch", "message": "invalid branch name"})
		return
	case !filepath.IsAbs(b.Repo):
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_path", "message": "the repository must be an absolute path"})
		return
	}
	branch := strings.TrimSpace(b.Branch)
	from := s.by(r, b.From)
	repoName := filepath.Base(filepath.Clean(b.Repo))

	// One worker per host, repository and branch: a second handoff while
	// the first is running or finished answers with it (the worktree's
	// path, <root>/<repo>/<branch>, is that identity too). If its agent is
	// gone, a new agent is started in the same worktree and the entry is
	// updated.
	key := b.Host + "\x00" + repoName + "\x00" + branch
	s.coord.mu.Lock()
	if s.coord.starting[key] {
		s.coord.mu.Unlock()
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "handoff_in_progress", "message": "a handoff for this branch is starting right now"})
		return
	}
	s.coord.starting[key] = true
	s.coord.mu.Unlock()
	defer func() {
		s.coord.mu.Lock()
		delete(s.coord.starting, key)
		s.coord.mu.Unlock()
	}()
	prev, hasPrev := s.ledger.Find(func(e *ledger.Entry) bool {
		return e.Host == b.Host && e.RepoName == repoName && (e.Branch == branch || slices.Contains(e.PreviousBranches, branch)) &&
			(e.Status == ledger.Running || e.Status == ledger.Finished || e.Status == ledger.ReadyToRemove)
	})
	ctx, cancel := context.WithTimeout(r.Context(), workerTimeout+15*time.Second)
	defer cancel()
	if hasPrev && s.agentPresent(ctx, prev) {
		writeJSON(w, map[string]any{"entry": prev, "existing": true})
		return
	}

	prompt := strings.NewReplacer(
		"{{repo}}", repoName, "{{branch}}", branch, "{{brief}}", strings.TrimSpace(b.Brief),
		"{{from}}", from, "{{host}}", b.Host,
	).Replace(s.handoffTemplate())

	// The entry's worktree is the worker's identity: the worker may have
	// renamed its branch since, which startWorker follows.
	hint := ""
	if hasPrev && prev.Worktree != "" {
		hint = prev.Worktree
		if other, ok := s.ledger.Find(func(e *ledger.Entry) bool {
			return e.ID != prev.ID && e.Host == prev.Host && e.Worktree == hint &&
				(e.Status == ledger.Running || e.Status == ledger.Finished || e.Status == ledger.ReadyToRemove)
		}); ok {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"code": "path_taken", "message": hint + " belongs to another worker (" + other.Branch + ")", "entry": prev})
			return
		}
	}
	res, lerr := s.workerOn(ctx, b.Host, workerBody{Repo: b.Repo, Branch: branch, Base: b.Base, Kind: b.Kind, Name: b.Name, Worktree: hint, Prompt: prompt})
	if hasPrev && lerr != nil {
		// Restarting the earlier worker failed: its entry stays as it was
		// (no second, failed entry for the same branch).
		writeJSONStatus(w, lerr.status, map[string]any{"code": lerr.code, "message": "the earlier worker's agent is gone and a new one could not be started: " + lerr.message, "entry": prev})
		return
	}
	if hasPrev {
		// The earlier worker's agent is gone: the new one takes over its
		// entry (and its worktree, which startWorker reopened).
		entry, _, err := s.ledger.Update(prev.ID, func(e *ledger.Entry) {
			e.Status, e.Error = ledger.Running, res.PromptError
			e.AgentID, e.PaneID, e.AgentName, e.Kind = res.Agent.ID, res.Agent.PaneID, res.Agent.Name, res.Agent.Agent
			e.WorkspaceID, e.Worktree, e.Reused = res.WorkspaceID, res.Worktree, true
			if res.Branch != "" && res.Branch != e.Branch {
				// The worker renamed its branch; either name finds the entry.
				e.PreviousBranches = append(e.PreviousBranches, e.Branch)
				e.Branch = res.Branch
			}
			e.Brief, e.From, e.ReplyTo, e.Callback = strings.TrimSpace(b.Brief), from, s.replyRef(r), nil
			e.SawWorking, e.FinishedAt, e.FinalReport, e.ReportTries, e.Mark = false, nil, "", 0, nil
		})
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"code": "ledger_error", "message": "the worker started, but the ledger could not be saved: " + err.Error(), "worker": res})
			return
		}
		s.coord.mu.Lock()
		delete(s.coord.checks, prev.ID)
		s.coord.started[prev.ID] = time.Now()
		s.coord.mu.Unlock()
		writeJSON(w, map[string]any{"entry": entry, "worker": res, "existing": true, "restarted": true})
		return
	}
	e := ledger.Entry{
		Host: b.Host, Repo: filepath.Clean(b.Repo), RepoName: repoName, Branch: branch, Base: strings.TrimSpace(b.Base),
		Kind: b.Kind, Brief: strings.TrimSpace(b.Brief), From: from, ReplyTo: s.replyRef(r),
	}
	if lerr != nil {
		e.Status, e.Error = ledger.Failed, lerr.code+": "+lerr.message
		e.WorkspaceID = lerr.workspace
		entry, err := s.ledger.Add(e)
		if err != nil {
			log.Printf("ledger: %v", err)
		}
		body := map[string]any{"code": lerr.code, "message": lerr.message, "entry": entry}
		writeJSONStatus(w, lerr.status, body)
		return
	}
	e.Status = ledger.Running
	e.Repo, e.RepoName, e.Base, e.Kind = res.Repo, filepath.Base(res.Repo), res.Base, res.Agent.Agent
	e.AgentID, e.PaneID, e.AgentName = res.Agent.ID, res.Agent.PaneID, res.Agent.Name
	e.WorkspaceID, e.Worktree, e.Reused = res.WorkspaceID, res.Worktree, res.Reused
	if res.PromptError != "" {
		e.Error = res.PromptError
	}
	entry, err := s.ledger.Add(e)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"code": "ledger_error", "message": "the worker started, but the ledger could not be saved: " + err.Error(), "worker": res})
		return
	}
	s.coord.mu.Lock()
	for id, at := range s.coord.started { // keep the map small
		if time.Since(at) > startGrace {
			delete(s.coord.started, id)
		}
	}
	s.coord.started[entry.ID] = time.Now()
	s.coord.mu.Unlock()
	writeJSON(w, map[string]any{"entry": entry, "worker": res})
}

// observeLedger applies an agent list to the ledger and captures the
// final reports of workers that finished.
func (s *Server) observeLedger(agents []herdr.AgentInfo) {
	if s.ledger == nil {
		return
	}
	captures := s.ledger.Observe(agents, time.Now())
	defer s.deliverCallbacks(agents)
	for _, e := range captures {
		s.coord.mu.Lock()
		last, busy := s.coord.reporting[e.ID]
		if busy && time.Since(last) < reportRetry {
			s.coord.mu.Unlock()
			continue
		}
		s.coord.reporting[e.ID] = time.Now()
		s.coord.mu.Unlock()
		go s.captureReport(e)
	}
}

// captureReport stores the worker's last assistant message as its final
// report.
func (s *Server) captureReport(e ledger.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	latest := s.latestOf(ctx, e.Host, e.PaneID)
	text := ""
	if latest != nil {
		text = latest.Text
	}
	if err := s.ledger.SetReport(e.ID, e.FinishedAt, text); err != nil {
		log.Printf("ledger: %v", err)
	}
}

// latestOf reads an agent's latest assistant message on any host (nil
// when there is none or it cannot be read). Tests replace it with
// s.latest.
func (s *Server) latestOf(ctx context.Context, host, pane string) *transcript.Latest {
	if s.latest != nil {
		return s.latest(ctx, host, pane)
	}
	if host == "" || host == s.fleet.LocalName() {
		if a := s.resolveAgent(pane); a != nil && s.manager != nil {
			if path, err := transcript.SessionPath(*a, s.manager.Agents()); err == nil {
				latest, _ := transcript.LatestOf(path)
				return latest
			}
		}
		return nil
	}
	return s.fleet.FetchLatest(ctx, host, pane)
}

// RunLedger follows agent updates for the ledger (finished workers) until
// ctx is done. Ledger reads observe too, so this only makes it prompt.
func (s *Server) RunLedger(ctx context.Context) {
	if s.ledger == nil {
		return
	}
	events, cancel := s.fleet.Hub().Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == "agents" {
				s.observeLedger(ev.Agents)
			}
		}
	}
}

// liveInfo is an entry's agent as the fleet sees it now.
type liveInfo struct {
	Present bool              `json:"present"`
	Status  herdr.AgentStatus `json:"agent_status,omitempty"`
}

// entryView is a ledger entry with live hints.
type entryView struct {
	ledger.Entry
	Live liveInfo `json:"live"`
	// Candidate hints that the entry could be marked ready to remove: it
	// finished with a report, and its worktree is clean and merged (per
	// checks at most checksTTL old). Comebye never marks on its own.
	Candidate bool            `json:"candidate"`
	Checks    *worktreeChecks `json:"checks,omitempty"`
}

// handleLedger lists the handoffs (removed and failed ones: the latest
// few) and the standalone marks.
func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	agents := s.fleet.Agents()
	s.observeLedger(agents)
	live := map[string]herdr.AgentInfo{}
	for _, a := range agents {
		live[a.ID] = a
	}
	entries := s.ledger.Entries()
	var done []int
	for i, e := range entries {
		if e.Status == ledger.Removed || e.Status == ledger.Failed {
			done = append(done, i)
		}
	}
	skip := map[int]bool{}
	if len(done) > removedShown {
		for _, i := range done[:len(done)-removedShown] {
			skip[i] = true
		}
	}
	views := []entryView{}
	for i, e := range entries {
		if skip[i] {
			continue
		}
		v := entryView{Entry: e}
		if a, ok := live[e.AgentID]; ok && e.AgentID != "" {
			v.Live = liveInfo{Present: true, Status: a.Status}
		}
		if e.Status == ledger.Finished && e.Worktree != "" {
			if c := s.cachedChecks(e); c != nil {
				v.Checks = c
				v.Candidate = e.FinalReport != "" && c.Exists && !c.Primary && !c.Dirty && c.Unmerged == 0 && c.Error == ""
			}
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{"entries": views, "marks": s.ledger.Marks(), "roles": s.liveRoles()})
}

// cachedChecks returns an entry's cached checks, refreshing them in the
// background when missing or older than checksTTL.
func (s *Server) cachedChecks(e ledger.Entry) *worktreeChecks {
	s.coord.mu.Lock()
	defer s.coord.mu.Unlock()
	c, ok := s.coord.checks[e.ID]
	if (!ok || time.Since(c.at) > checksTTL) && !s.coord.checking[e.ID] {
		s.coord.checking[e.ID] = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, lerr := s.checksOn(ctx, e.Host, e.Worktree, e.Branch, e.Base, checkOpts{})
			s.coord.mu.Lock()
			defer s.coord.mu.Unlock()
			delete(s.coord.checking, e.ID)
			if lerr == nil {
				s.coord.checks[e.ID] = cachedChecks{*res, time.Now()}
			}
		}()
	}
	if !ok {
		return nil
	}
	cc := c.c
	return &cc
}

// markBody is the JSON body of a mark or unmark.
type markBody struct {
	AgentID string `json:"agent_id"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
	// Discard proposes throwing the work away (see ledger.Mark).
	Discard bool `json:"discard"`
	// NoAsk skips asking the session whether it is safe to remove.
	NoAsk bool `json:"no_ask"`
}

// handleMark marks an agent ready to remove: POST {agent_id, reason?,
// by?}. A worker's ledger entry is marked; any other agent must be live.
func (s *Server) handleMark(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b markBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.AgentID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if len(b.Reason) > 500 {
		b.Reason = b.Reason[:500]
	}
	m := ledger.Mark{AgentID: b.AgentID, By: s.by(r, b.By), Reason: strings.TrimSpace(b.Reason), Discard: b.Discard}
	entry0, isWorker := s.ledger.ByAgent(b.AgentID)
	var agent *herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		if a.ID == b.AgentID {
			agent = &a
			break
		}
	}
	if agent == nil && !isWorker {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "agent_not_found", "message": "no such agent"})
		return
	}
	if c := callerOf(r); c.agent() {
		name := entry0.AgentName
		if agent != nil {
			name = agent.Name
		}
		switch {
		case m.Reason == "":
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "reason_required", "message": "say why the session can go (--reason)"})
			return
		case b.AgentID == c.agentRef():
			writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "self", "message": "an agent cannot mark itself"})
			return
		case s.isCoordinator(b.AgentID, name):
			writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "coordinator", "message": "agents cannot mark a coordinator; only the user can remove one"})
			return
		}
	}
	if agent != nil {
		m.Host, m.PaneID, m.Name, m.WorkspaceID, m.CWD = agent.Host, agent.PaneID, agent.Name, agent.WorkspaceID, agent.CWD
		if agent.Worktree != nil && agent.Worktree.IsLinked {
			m.Worktree = agent.Worktree.CheckoutPath
		}
	}
	m.At = time.Now().UTC()
	ask := !b.NoAsk
	if ask {
		// Recorded as asking first, so the asking goroutine finds its mark.
		m.Ask = &ledger.Ask{State: ledger.AskAsking, At: m.At}
	}
	entry, err := s.ledger.SetMark(m)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "ledger_error", "message": err.Error()})
		return
	}
	if ask {
		m.Ask = s.startAsk(m, agent)
		if _, err := s.ledger.SetAsk(m.AgentID, m.At, *m.Ask); err != nil {
			log.Printf("ledger: %v", err)
		}
		if entry != nil {
			entry.Mark = &m
		}
	}
	writeJSON(w, map[string]any{"mark": m, "entry": entry})
}

// handleUnmark clears a mark: POST {agent_id}.
func (s *Server) handleUnmark(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b markBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.AgentID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	found, err := s.ledger.ClearMark(b.AgentID)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "ledger_error", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"cleared": found})
}

// removal is one session marked ready to remove, as the review list shows
// it.
type removal struct {
	ID          string          `json:"id"` // the host-qualified agent id
	Host        string          `json:"host"`
	PaneID      string          `json:"pane_id,omitempty"`
	Name        string          `json:"name,omitempty"`
	EntryID     string          `json:"entry_id,omitempty"` // a ledger worker's entry
	Repo        string          `json:"repo,omitempty"`
	Branch      string          `json:"branch,omitempty"`
	Base        string          `json:"base,omitempty"`
	WorkspaceID string          `json:"workspace_id,omitempty"`
	Worktree    string          `json:"worktree,omitempty"`
	Mark        ledger.Mark     `json:"mark"`
	Live        liveInfo        `json:"live"`
	Checks      *worktreeChecks `json:"checks,omitempty"`
	ChecksError string          `json:"checks_error,omitempty"`
	FinalReport string          `json:"final_report,omitempty"`
	Warnings    []string        `json:"warnings"`
	Notes       []string        `json:"notes"`
	Safe        bool            `json:"safe"`
	// Discardable: the worktree has work a plain removal keeps (uncommitted
	// changes, unmerged commits, an unfinished rebase); the user may
	// confirm discarding it, with the tip in checks.tip.
	Discardable bool `json:"discardable,omitempty"`
	// Coordinator: the agent is a coordinator; removing it needs its id
	// in the confirmation's coordinators as well.
	Coordinator bool              `json:"coordinator,omitempty"`
	Status      herdr.AgentStatus `json:"-"`
}

// marked lists the marked sessions (without checks).
func (s *Server) marked() []*removal {
	live := map[string]herdr.AgentInfo{}
	for _, a := range s.fleet.Agents() {
		live[a.ID] = a
	}
	var out []*removal
	for _, e := range s.ledger.Entries() {
		if e.Status != ledger.ReadyToRemove || e.Mark == nil {
			continue
		}
		out = append(out, &removal{
			ID: e.AgentID, Host: e.Host, PaneID: e.PaneID, Name: e.AgentName, EntryID: e.ID, Repo: e.RepoName,
			Branch: e.Branch, Base: e.Base, WorkspaceID: e.WorkspaceID, Worktree: e.Worktree, Mark: *e.Mark, FinalReport: e.FinalReport,
		})
	}
	for _, m := range s.ledger.Marks() {
		out = append(out, &removal{ID: m.AgentID, Host: m.Host, PaneID: m.PaneID, Name: m.Name, WorkspaceID: m.WorkspaceID, Worktree: m.Worktree, Mark: m})
	}
	for _, it := range out {
		name := it.Name
		if a, ok := live[it.ID]; ok {
			it.Live = liveInfo{Present: true, Status: a.Status}
			if it.Repo == "" && a.Worktree != nil {
				it.Repo = a.Worktree.RepoName
			}
			name = a.Name
		}
		it.Coordinator = s.isCoordinator(it.ID, name)
	}
	return out
}

// review fills in an item's checks (on its host) and its warnings.
func (s *Server) review(ctx context.Context, it *removal) {
	it.Warnings, it.Notes = []string{}, []string{}
	if it.Coordinator {
		it.Warnings = append(it.Warnings, "this agent is a coordinator: removing it needs an extra confirmation")
	}
	switch {
	case !it.Live.Present:
		it.Notes = append(it.Notes, "the agent is no longer running")
	case it.Live.Status == herdr.StatusWorking || it.Live.Status == herdr.StatusBlocked:
		it.Warnings = append(it.Warnings, "the agent is "+string(it.Live.Status)+": closing it interrupts it")
	}
	s.findWorktree(ctx, it)
	if it.Worktree == "" {
		it.Notes = append(it.Notes, "no worktree of its own: only the session is closed")
	} else if !s.knownHost(it.Host) {
		it.ChecksError = "unknown host " + it.Host
	} else {
		c, lerr := s.checksOn(ctx, it.Host, it.Worktree, it.Branch, it.Base, checkOpts{Detail: true})
		if lerr != nil {
			it.ChecksError = lerr.message
		} else {
			it.Checks = c
			if it.Branch == "" {
				it.Branch = c.Branch
			}
			switch {
			case !c.Exists:
				it.Notes = append(it.Notes, "the worktree is already gone")
			case c.Primary:
				it.Notes = append(it.Notes, "this is the repository's primary checkout: it is never removed")
			default:
				if c.InProgress != "" {
					it.Warnings = append(it.Warnings, "a "+c.InProgress+" is in progress: the worktree will be kept")
				}
				if c.Dirty {
					it.Warnings = append(it.Warnings, "uncommitted changes ("+itoa(c.DirtyFiles)+" files): the worktree will be kept")
				}
				if c.Unmerged > 0 {
					it.Warnings = append(it.Warnings, itoa(c.Unmerged)+" commits not in "+c.BaseUsed+": the worktree will be kept")
				} else if c.Unmerged < 0 || c.Error != "" {
					it.Warnings = append(it.Warnings, "could not check the branch ("+c.Error+"): the worktree will be kept")
				}
				it.Discardable = c.Tip != "" && (c.Dirty || c.InProgress != "" || c.Unmerged != 0)
			}
		}
	}
	if it.ChecksError != "" {
		it.Warnings = append(it.Warnings, "checks failed: "+it.ChecksError)
	}
	if it.Mark.Discard {
		it.Notes = append(it.Notes, "proposed: discard the work (worktree and branch)")
	}
	if a := it.Mark.Ask; a != nil {
		switch a.State {
		case ledger.AskUnsafe:
			why := a.Reason
			if why == "" {
				why = "(no reason given)"
			}
			it.Warnings = append(it.Warnings, "the session says UNSAFE: "+why)
		case ledger.AskAsking:
			it.Notes = append(it.Notes, "asking the session whether it is safe to remove…")
		case ledger.AskNoAnswer:
			it.Notes = append(it.Notes, "the session did not answer whether it is safe to remove"+parenthesized(a.Reason))
		case ledger.AskUnclear:
			it.Notes = append(it.Notes, "the session answered neither SAFE nor UNSAFE: "+a.Reason)
		case ledger.AskNotAsked:
			it.Notes = append(it.Notes, "the session was not asked whether it is safe to remove"+parenthesized(a.Reason))
		}
	}
	it.Safe = len(it.Warnings) == 0
}

func parenthesized(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// findWorktree fills in the worktree of a marked session that recorded
// none (herdr did not report one for the agent) from its working
// directory: a linked worktree around it is the session's own; a primary
// checkout is not.
func (s *Server) findWorktree(ctx context.Context, it *removal) {
	if it.Worktree != "" || it.Mark.CWD == "" || !s.knownHost(it.Host) {
		return
	}
	c, lerr := s.checksOn(ctx, it.Host, it.Mark.CWD, "", "", checkOpts{Within: true})
	if lerr == nil && c.Exists && !c.Primary {
		it.Worktree = c.Path
	}
}

// handleRemovals lists the marked sessions with fresh checks from their
// hosts.
func (s *Server) handleRemovals(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	items := s.marked()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, it := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.review(ctx, it)
		}()
	}
	wg.Wait()
	sort.SliceStable(items, func(i, j int) bool { return items[i].Mark.At.Before(items[j].Mark.At) })
	if items == nil {
		items = []*removal{}
	}
	writeJSON(w, map[string]any{"removals": items})
}

// confirmBody is the JSON body of a removal confirmation.
type confirmBody struct {
	IDs []string `json:"ids"`
	// AllowWorking closes agents that went back to work since the review.
	AllowWorking bool `json:"allow_working"`
	// Coordinators lists the coordinators among IDs the user confirmed a
	// second time; any other coordinator is refused.
	Coordinators []string `json:"coordinators"`
	// Discard maps the IDs whose work the user confirmed throwing away to
	// the tip they reviewed (checks.tip).
	Discard map[string]string `json:"discard"`
}

// removalOutcome is one confirmed item's result.
type removalOutcome struct {
	ID      string        `json:"id"`
	OK      bool          `json:"ok"`
	Code    string        `json:"code,omitempty"`
	Message string        `json:"message,omitempty"`
	Result  *removeResult `json:"result,omitempty"`
}

// handleConfirmRemovals removes the given marked sessions, each on its
// host: POST {ids, allow_working?}. Items not marked are refused.
func (s *Server) handleConfirmRemovals(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b confirmBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil || len(b.IDs) == 0 || len(b.IDs) > 100 {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	byID := map[string]*removal{}
	for _, it := range s.marked() {
		byID[it.ID] = it
	}
	out := make([]removalOutcome, len(b.IDs))
	ctx, cancel := context.WithTimeout(r.Context(), removeTimeout+30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, id := range b.IDs {
		out[i].ID = id
		it := byID[id]
		if it == nil {
			out[i].Code, out[i].Message = "not_marked", "that session is not marked ready to remove"
			continue
		}
		if it.Coordinator && !slices.Contains(b.Coordinators, id) {
			out[i].Code, out[i].Message = "coordinator", "this agent is a coordinator: confirm its removal explicitly"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = s.confirmOne(ctx, it, b.AllowWorking, b.Discard[id])
		}()
	}
	wg.Wait()
	writeJSON(w, map[string]any{"results": out})
}

// confirmOne removes one marked session on its host and records it;
// with discardTip, discarding its work.
func (s *Server) confirmOne(ctx context.Context, it *removal, allowWorking bool, discardTip string) removalOutcome {
	o := removalOutcome{ID: it.ID}
	if !s.knownHost(it.Host) {
		o.Code, o.Message = "unknown_host", "no such host: "+it.Host
		return o
	}
	s.findWorktree(ctx, it)
	body := removeBody{
		Agent: it.PaneID, Name: it.Name, WorkspaceID: it.WorkspaceID, Worktree: it.Worktree,
		Branch: it.Branch, Base: it.Base, RemoveWorktree: it.Worktree != "", AllowWorking: allowWorking,
		Discard: discardTip != "", DiscardTip: discardTip,
	}
	res, lerr := s.removeOn(ctx, it.Host, body)
	if lerr != nil {
		o.Code, o.Message = lerr.code, lerr.message
		return o
	}
	raw, _ := json.Marshal(res)
	if _, err := s.ledger.Removed(it.ID, raw, time.Now()); err != nil {
		log.Printf("ledger: %v", err)
	}
	if it.Coordinator {
		if err := s.recordRole(it.ID, "", "", ""); err != nil {
			log.Printf("roles: %v", err)
		}
	}
	s.coord.mu.Lock()
	delete(s.coord.checks, it.EntryID)
	s.coord.mu.Unlock()
	o.OK, o.Result = true, res
	return o
}

// errNoLedger is returned by OpenLedger without a state directory.
var errNoLedger = errors.New("no state directory")

// openLedger opens the state directory's ledger.json.
func openLedger(dir string) (*ledger.Store, error) {
	if dir == "" {
		return nil, errNoLedger
	}
	return ledger.Open(filepath.Join(dir, "ledger.json"))
}
