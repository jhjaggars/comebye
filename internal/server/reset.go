package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/digest"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// The coordinator's in-place context reset (docs/coordinator-reset.md).
//
// A coordinator's context only grows. When it is large, the hub sends
// `/clear` into the coordinator's own pane, as typed input, and a
// SessionStart hook (installed with `comebye coordinator install-hook`)
// puts a digest into the new session. The pane, the agent id, the name and
// the role stay; only the session id changes (checked live: herdr follows
// the new session, but drops the agent's name, which the hub gives back).
//
// The hub decides at a briefing: the first briefing published after the
// coordinator's context passed the threshold marks a reset due, and the
// reset runs as soon as the coordinator is idle with nothing queued for it.
// The user can also start one by hand ("Fresh start", `comebye coordinator
// reset`). Neither ever runs while the coordinator is working or blocked,
// while a dialog or typed text is on its screen, or while the hub holds a
// notice or prompt for it.

// DefaultResetTokens is the context size at which a briefing marks the
// coordinator's reset due.
const DefaultResetTokens = 150000

const (
	// resetDueTTL is how long a due reset waits for a quiet moment before
	// the next briefing decides again.
	resetDueTTL = 3 * time.Hour
	// resetMinGap is the least time between two automatic resets of one
	// coordinator: a reset that does not shrink the context (the hook is
	// missing, say) must not loop.
	resetMinGap = 30 * time.Minute
	// resetHistory is how many past resets are kept.
	resetHistory = 20
	// hookMarker is what the coordinator hook leaves in a session at its
	// start (a short note), and what the hub looks for before a reset: the
	// session was started with the hook, so the digest will be injected.
	hookMarker = "comebye-hook:v1"
)

// ResetRecord is one reset.
type ResetRecord struct {
	At      time.Time `json:"at"`
	AgentID string    `json:"agent_id"`
	Name    string    `json:"name,omitempty"`
	By      string    `json:"by"`
	// Trigger is "briefing" (the hub's own) or "manual".
	Trigger       string `json:"trigger"`
	Tokens        int    `json:"tokens"`
	SessionBefore string `json:"session_before,omitempty"`
	SessionAfter  string `json:"session_after,omitempty"`
	Renamed       bool   `json:"renamed,omitempty"`
	DigestLanded  bool   `json:"digest_landed"`
}

// resetDue is a reset waiting for a quiet moment.
type resetDue struct {
	Since  time.Time `json:"since"`
	Tokens int       `json:"tokens"`
	Name   string    `json:"name,omitempty"`
	// Why is the latest reason it did not run yet.
	Why string `json:"why,omitempty"`
}

type resetFile struct {
	Last    *ResetRecord  `json:"last,omitempty"`
	History []ResetRecord `json:"history,omitempty"` // newest first
	// CheckedBrief is the briefing whose context size was last looked at.
	CheckedBrief time.Time           `json:"checked_brief,omitzero"`
	Due          map[string]resetDue `json:"due,omitempty"`
}

// resetStore keeps coordinator-reset.json in the state directory.
type resetStore struct {
	mu   sync.Mutex
	path string
	data resetFile
	// busy: agents with a reset under way (not persisted).
	busy map[string]bool
}

func openResets(path string) (*resetStore, error) {
	s := &resetStore{path: path, busy: map[string]bool{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return s, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

func (s *resetStore) get() resetFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.data
	out.Due = make(map[string]resetDue, len(s.data.Due))
	for k, v := range s.data.Due {
		out.Due[k] = v
	}
	out.History = append([]ResetRecord(nil), s.data.History...)
	return out
}

func (s *resetStore) update(fn func(*resetFile)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	next.Due = make(map[string]resetDue, len(s.data.Due))
	for k, v := range s.data.Due {
		next.Due[k] = v
	}
	fn(&next)
	if len(next.Due) == 0 {
		next.Due = nil
	}
	if err := writeFileAtomic(s.path, next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// begin marks a reset of id under way; false when one already is.
func (s *resetStore) begin(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return false
	}
	s.busy[id] = true
	return true
}

func (s *resetStore) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, id)
}

// ---- on the coordinator's host ----

// resetBody is POST /api/agents/{target}/reset (the hub's call to the host
// that runs the coordinator).
type resetBody struct {
	// MinTokens, when positive, refuses a reset of a smaller context.
	MinTokens int    `json:"min_tokens"`
	Trigger   string `json:"trigger,omitempty"`
}

// contextInfo is GET /api/agents/{target}/context.
type contextInfo struct {
	Tokens  int    `json:"tokens"`
	Model   string `json:"model,omitempty"`
	Session string `json:"session,omitempty"`
	Status  string `json:"status"`
	// HookActive: this session was started with the coordinator hook.
	HookActive bool `json:"hook_active"`
}

func failJSON(status int, code, msg string) (int, any) {
	return status, map[string]string{"code": code, "message": msg}
}

func sessionOf(a herdr.AgentInfo) string {
	if a.AgentSession != nil {
		return a.AgentSession.Value
	}
	return ""
}

// contextLocal reads an agent's context size on this host.
func (s *Server) contextLocal(target string) (int, any) {
	a := s.resolveAgent(target)
	if a == nil {
		return failJSON(http.StatusNotFound, "agent_not_found", "no such agent")
	}
	if a.Agent != "claude" {
		return failJSON(http.StatusConflict, "reset_unsupported", "only Claude Code agents report a context size (this one is "+a.Agent+")")
	}
	path, err := transcript.SessionPath(*a, s.manager.Agents())
	if err != nil {
		return failJSON(http.StatusNotFound, "no_session", err.Error())
	}
	c, ok, err := transcript.LastContext(path)
	if err != nil {
		return failJSON(http.StatusNotFound, "no_session", err.Error())
	}
	if !ok {
		return failJSON(http.StatusNotFound, "no_usage", "the session has no request with usage yet")
	}
	return http.StatusOK, contextInfo{Tokens: c.Tokens, Model: c.Model, Session: sessionOf(*a), Status: string(a.Status), HookActive: hookActive(path)}
}

// hookActive reports whether the session at path holds what the hook leaves.
func hookActive(path string) bool {
	for _, m := range []string{hookMarker, digest.Marker} {
		if ok, _ := transcript.FileContains(path, []byte(m)); ok {
			return true
		}
	}
	return false
}

// resetLocal clears an agent's context on this host: checks it, sends
// /clear into its pane, waits for the new session, gives back the name
// herdr dropped and checks that the digest landed.
func (s *Server) resetLocal(ctx context.Context, target string, b resetBody) (int, any) {
	a := s.resolveAgent(target)
	if a == nil {
		return failJSON(http.StatusNotFound, "agent_not_found", "no such agent")
	}
	if a.Agent != "claude" {
		return failJSON(http.StatusConflict, "reset_unsupported", "an in-place reset needs Claude Code (this agent is "+a.Agent+")")
	}
	if a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone {
		return failJSON(http.StatusConflict, "agent_busy", "the agent is "+string(a.Status)+": a fresh start waits until it is idle")
	}
	path, err := transcript.SessionPath(*a, s.manager.Agents())
	if err != nil {
		return failJSON(http.StatusNotFound, "no_session", err.Error())
	}
	c, ok, err := transcript.LastContext(path)
	if err != nil {
		return failJSON(http.StatusNotFound, "no_session", err.Error())
	}
	if !ok {
		return failJSON(http.StatusNotFound, "no_usage", "the session has no request with usage yet")
	}
	if b.MinTokens > 0 && c.Tokens < b.MinTokens {
		return failJSON(http.StatusConflict, "below_threshold", fmt.Sprintf("the context is %d tokens, under %d", c.Tokens, b.MinTokens))
	}
	// Without the hook the new session would start blank.
	if !hookActive(path) {
		return failJSON(http.StatusConflict, "hook_not_installed", "this session was not started with the coordinator hook, so a cleared session would get no digest: run `comebye coordinator install-hook`, then start the coordinator again (the app's Coordinator preset passes the hook's settings)")
	}
	if err := s.guardPrompt(ctx, *a); err != nil {
		var he *herdr.Error
		if errors.As(err, &he) && (he.Code == "dialog_open" || he.Code == "input_busy") {
			return failJSON(http.StatusConflict, he.Code, err.Error())
		}
		return failJSON(http.StatusBadGateway, "herdr_error", err.Error())
	}
	before, name := sessionOf(*a), a.Name
	// The state may have moved since the list was cached.
	if live, err := s.client.GetAgent(ctx, a.PaneID); err == nil {
		if live.Status != herdr.StatusIdle && live.Status != herdr.StatusDone {
			return failJSON(http.StatusConflict, "agent_busy", "the agent is "+string(live.Status))
		}
		if sessionOf(live) != "" {
			before = sessionOf(live)
		}
	}
	if _, err := s.client.Prompt(ctx, a.PaneID, "/clear", herdr.PromptOptions{}); err != nil {
		return failJSON(http.StatusBadGateway, "reset_failed", "could not send /clear: "+errMessage(err))
	}
	// Wait for the new session: another id, and the agent ready again.
	var after herdr.AgentInfo
	deadline := time.Now().Add(s.resetWait)
	for {
		live, err := s.client.GetAgent(ctx, a.PaneID)
		if err == nil && sessionOf(live) != "" && sessionOf(live) != before && (live.Status == herdr.StatusIdle || live.Status == herdr.StatusDone) {
			after = live
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return failJSON(http.StatusGatewayTimeout, "clear_not_confirmed", "sent /clear but no new session showed up in time; look at the pane")
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.resetPoll):
		}
	}
	renamed := false
	if name != "" && after.Name == "" {
		if err := s.client.RenameAgent(ctx, a.PaneID, name); err != nil {
			log.Printf("coordinator reset: could not give %s its name %q back: %v", a.PaneID, name, err)
		} else {
			renamed = true
		}
	}
	landed := false
	for {
		if p, err := transcript.SessionPath(after, s.manager.Agents()); err == nil {
			if ok, _ := transcript.FileContains(p, []byte(digest.Marker)); ok {
				landed = true
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.resetPoll):
		}
	}
	return http.StatusOK, map[string]any{
		"agent": map[string]string{"pane_id": a.PaneID, "name": name}, "tokens": c.Tokens,
		"session_before": before, "session_after": sessionOf(after), "renamed": renamed, "digest_landed": landed,
	}
}

// handleContext is GET /api/agents/{target}/context (the user's, which the
// hub's own calls to a peer are).
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	status, payload := s.contextLocal(r.PathValue("target"))
	writeJSONStatus(w, status, payload)
}

// handleResetAgent is POST /api/agents/{target}/reset: the local half. The
// hub's checks (what is queued for the coordinator) are not made here; the
// hub's own route is POST /api/coordinator/reset.
func (s *Server) handleResetAgent(w http.ResponseWriter, r *http.Request) {
	var b resetBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.resetWait+30*time.Second)
	defer cancel()
	status, payload := s.resetLocal(ctx, r.PathValue("target"), b)
	writeJSONStatus(w, status, payload)
}

// ---- on the hub ----

// coordinatorAgents are the live coordinators that are Claude Code agents.
func (s *Server) coordinatorAgents() []herdr.AgentInfo {
	roles := s.liveRoles()
	var out []herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		if r, ok := roles[a.ID]; ok && r.Role == RoleCoordinator && a.Agent == "claude" {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// callAgentHost runs an op on the agent's host: here, or a peer's through
// CallAgent. local is the handler's own logic.
func (s *Server) callAgentHost(ctx context.Context, a herdr.AgentInfo, op string, body []byte, local func() (int, any)) (int, []byte, error) {
	if a.Host == "" || a.Host == s.fleet.LocalName() {
		status, payload := local()
		data, _ := json.Marshal(payload)
		return status, data, nil
	}
	return s.fleet.CallAgent(ctx, a.Host, a.PaneID, op, body)
}

// coordinatorContext reads a coordinator's context size wherever it runs.
func (s *Server) coordinatorContext(ctx context.Context, a herdr.AgentInfo) (contextInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, data, err := s.callAgentHost(ctx, a, "context", nil, func() (int, any) { return s.contextLocal(a.PaneID) })
	if err != nil {
		return contextInfo{}, err
	}
	if status != http.StatusOK {
		return contextInfo{}, fmt.Errorf("%s", apiMessage(data, status))
	}
	var ci contextInfo
	if err := json.Unmarshal(data, &ci); err != nil {
		return contextInfo{}, err
	}
	return ci, nil
}

// apiMessage is the message of an error answer.
func apiMessage(data []byte, status int) string {
	var m struct{ Code, Message string }
	if json.Unmarshal(data, &m) == nil && m.Message != "" {
		return m.Message
	}
	return fmt.Sprintf("HTTP %d", status)
}

// queuedFor lists what the hub holds for the coordinator a: prompts in the
// queue, notices of answered to-dos, worker callbacks, and blocked-agent
// notices. A reset waits for all of it to go out (the coordinator would
// otherwise lose the message with its context).
func (s *Server) queuedFor(a herdr.AgentInfo) []string {
	var out []string
	if s.pq != nil {
		if n := len(s.pq.list(a.ID, false)); n > 0 {
			out = append(out, fmt.Sprintf("%d queued prompt(s)", n))
		}
	}
	if s.todos != nil {
		if n := len(s.todos.pendingNotices()); n > 0 {
			out = append(out, fmt.Sprintf("%d to-do answer notice(s) not delivered", n))
		}
	}
	if s.ledger != nil {
		live := map[string]bool{}
		for _, x := range s.fleet.Agents() {
			live[x.ID] = true
		}
		n := 0
		for _, e := range s.ledger.PendingCallbacks() {
			// Told to the requester, or to a coordinator in its place when
			// the requester is gone: either may be this one.
			if e.ReplyTo == nil || e.ReplyTo.AgentID == a.ID || !live[e.ReplyTo.AgentID] {
				n++
			}
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("%d worker notice(s) not delivered", n))
		}
	}
	if s.relay != nil {
		s.relay.mu.Lock()
		n := 0
		for _, ep := range s.relay.eps {
			if ep.to.AgentID == a.ID && !ep.done && (ep.owed || ep.inFlight) {
				n++
			}
		}
		s.relay.mu.Unlock()
		if n > 0 {
			out = append(out, fmt.Sprintf("%d blocked-agent notice(s) not delivered", n))
		}
	}
	return out
}

// freshStart resets coordinator a: the hub's guards, the reset on its host,
// then the record. minTokens > 0 refuses a smaller context.
func (s *Server) freshStart(ctx context.Context, a herdr.AgentInfo, trigger, by string, minTokens int) (int, map[string]any) {
	fail := func(status int, code, msg string) (int, map[string]any) {
		return status, map[string]any{"code": code, "message": msg}
	}
	if s.resets == nil {
		return fail(http.StatusServiceUnavailable, "ledger_disabled", "this instance keeps no coordinator state")
	}
	if !s.resets.begin(a.ID) {
		return fail(http.StatusConflict, "reset_running", "a fresh start of "+a.ID+" is already under way")
	}
	defer s.resets.end(a.ID)
	if q := s.queuedFor(a); len(q) > 0 {
		return fail(http.StatusConflict, "queued", "the hub holds messages for the coordinator ("+strings.Join(q, "; ")+"): a fresh start waits until they are delivered")
	}
	body, _ := json.Marshal(resetBody{MinTokens: minTokens, Trigger: trigger})
	ctx, cancel := context.WithTimeout(ctx, s.resetWait+45*time.Second)
	defer cancel()
	status, data, err := s.callAgentHost(ctx, a, "reset", body, func() (int, any) {
		return s.resetLocal(ctx, a.PaneID, resetBody{MinTokens: minTokens, Trigger: trigger})
	})
	if err != nil {
		return fail(http.StatusBadGateway, "host_unreachable", err.Error())
	}
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	if out == nil {
		out = map[string]any{}
	}
	if status != http.StatusOK {
		if _, ok := out["code"]; !ok {
			out["code"], out["message"] = "reset_failed", apiMessage(data, status)
		}
		return status, out
	}
	rec := ResetRecord{At: time.Now().UTC(), AgentID: a.ID, Name: a.Name, By: by, Trigger: trigger}
	var ans struct {
		Tokens        int    `json:"tokens"`
		SessionBefore string `json:"session_before"`
		SessionAfter  string `json:"session_after"`
		Renamed       bool   `json:"renamed"`
		DigestLanded  bool   `json:"digest_landed"`
	}
	_ = json.Unmarshal(data, &ans)
	rec.Tokens, rec.SessionBefore, rec.SessionAfter, rec.Renamed, rec.DigestLanded = ans.Tokens, ans.SessionBefore, ans.SessionAfter, ans.Renamed, ans.DigestLanded
	if err := s.resets.update(func(f *resetFile) {
		f.Last = &rec
		f.History = append([]ResetRecord{rec}, f.History...)
		if len(f.History) > resetHistory {
			f.History = f.History[:resetHistory]
		}
		delete(f.Due, a.ID)
	}); err != nil {
		log.Printf("coordinator reset: %v", err)
	}
	detail := fmt.Sprintf("%s (%s) cleared at %dk tokens (%s); new session %s; digest landed: %v", a.Name, a.ID, rec.Tokens/1000, trigger, shortID(rec.SessionAfter), rec.DigestLanded)
	s.auditBy(ledger.AuditEvent{By: by, Action: "coordinator.reset", Host: a.Host, Detail: detail, OK: rec.DigestLanded})
	if !rec.DigestLanded {
		log.Printf("coordinator reset: WARNING: %s was cleared but the digest was not seen in the new session; it may have started blank", a.ID)
	}
	s.fleet.Changed()
	out["record"] = rec
	return http.StatusOK, out
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ---- the hub's routes ----

// coordinatorState is GET /api/coordinator.
type coordinatorState struct {
	// Threshold is the context size, in tokens, that marks a reset due at
	// the next briefing (0: the automatic reset is off).
	Threshold int                 `json:"threshold"`
	Last      *ResetRecord        `json:"last,omitempty"`
	History   []ResetRecord       `json:"history,omitempty"`
	Due       map[string]resetDue `json:"due,omitempty"`
	// Coordinators are the live Claude coordinators; with ?refresh=1 each
	// has its context size.
	Coordinators []coordinatorView `json:"coordinators"`
}

type coordinatorView struct {
	ID      string       `json:"id"`
	Name    string       `json:"name,omitempty"`
	Host    string       `json:"host"`
	Status  string       `json:"status"`
	Context *contextInfo `json:"context,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// handleCoordinator answers the coordinator's reset state: the last reset
// (the digest's "since"), the threshold and any reset due. ?refresh=1 also
// reads each coordinator's context size.
func (s *Server) handleCoordinator(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	f := s.resets.get()
	st := coordinatorState{Threshold: s.cfg.CoordinatorResetTokens, Last: f.Last, History: f.History, Due: f.Due, Coordinators: []coordinatorView{}}
	agents := s.coordinatorAgents()
	views := make([]coordinatorView, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		views[i] = coordinatorView{ID: a.ID, Name: a.Name, Host: a.Host, Status: string(a.Status)}
		if r.URL.Query().Get("refresh") == "1" && !callerOf(r).agent() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ci, err := s.coordinatorContext(r.Context(), a); err != nil {
					views[i].Error = err.Error()
				} else {
					views[i].Context = &ci
				}
			}()
		}
	}
	wg.Wait()
	st.Coordinators = append(st.Coordinators, views...)
	writeJSON(w, st)
}

// handleCoordinatorReset is POST /api/coordinator/reset {agent_id?}: the
// user's "Fresh start". It names the coordinator, or takes the only one.
func (s *Server) handleCoordinatorReset(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	var target *herdr.AgentInfo
	cs := s.coordinatorAgents()
	for i := range cs {
		if (b.AgentID == "" && len(cs) == 1) || cs[i].ID == b.AgentID {
			target = &cs[i]
		}
	}
	if target == nil {
		msg := "no such coordinator"
		if b.AgentID == "" && len(cs) > 1 {
			msg = "several coordinators are running: name one (agent_id)"
		} else if len(cs) == 0 {
			msg = "no Claude Code coordinator is running"
		}
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_coordinator", "message": msg})
		return
	}
	status, out := s.freshStart(r.Context(), *target, "manual", s.by(r, ""), 0)
	writeJSONStatus(w, status, out)
}

// ---- the briefing-driven reset ----

// resetTick decides, once per new briefing, whether a coordinator's
// context is over the threshold (marking its reset due), and runs a due
// reset as soon as the coordinator can take it. The hub only; the scheduler
// calls it every tick.
func (s *Server) resetTick(ctx context.Context, now time.Time) {
	if s.resets == nil || s.brief == nil || s.cfg.CoordinatorResetTokens <= 0 {
		return
	}
	b, err := s.brief.get()
	if err != nil || b == nil {
		return
	}
	f := s.resets.get()
	agents := s.coordinatorAgents()
	if b.At.After(f.CheckedBrief) {
		// A briefing is the moment to look: the coordinator has just caught
		// up and written its notes, so its context is as useful as it gets.
		due := map[string]resetDue{}
		for _, a := range agents {
			ci, err := s.coordinatorContext(ctx, a)
			if err != nil {
				log.Printf("coordinator reset: reading %s's context: %v", a.ID, err)
				continue
			}
			if ci.Tokens >= s.cfg.CoordinatorResetTokens {
				if _, already := f.Due[a.ID]; !already {
					log.Printf("coordinator reset: %s is at %dk tokens (threshold %dk): a fresh start is due", a.ID, ci.Tokens/1000, s.cfg.CoordinatorResetTokens/1000)
				}
				due[a.ID] = resetDue{Since: now.UTC(), Tokens: ci.Tokens, Name: a.Name}
			}
		}
		if err := s.resets.update(func(f *resetFile) {
			f.CheckedBrief = b.At
			for id, d := range due {
				if old, ok := f.Due[id]; ok {
					d.Since = old.Since
				}
				if f.Due == nil {
					f.Due = map[string]resetDue{}
				}
				f.Due[id] = d
			}
		}); err != nil {
			log.Printf("coordinator reset: %v", err)
			return
		}
		f = s.resets.get()
	}
	for id, d := range f.Due {
		var a *herdr.AgentInfo
		for i := range agents {
			if agents[i].ID == id {
				a = &agents[i]
			}
		}
		if a == nil || now.Sub(d.Since) > resetDueTTL {
			_ = s.resets.update(func(f *resetFile) { delete(f.Due, id) })
			continue
		}
		if f.Last != nil && f.Last.AgentID == id && now.Sub(f.Last.At) < resetMinGap {
			continue
		}
		status, out := s.freshStart(ctx, *a, "briefing", "the hub", s.cfg.CoordinatorResetTokens)
		if status == http.StatusOK {
			continue
		}
		why, _ := out["message"].(string)
		code, _ := out["code"].(string)
		switch code {
		case "below_threshold":
			// It shrank by itself (a manual compaction): nothing to do.
			_ = s.resets.update(func(f *resetFile) { delete(f.Due, id) })
		case "hook_not_installed", "reset_unsupported":
			// Will not change by waiting: say so once, and stop.
			log.Printf("coordinator reset: %s: %s", id, why)
			_ = s.resets.update(func(f *resetFile) { delete(f.Due, id) })
		default:
			if d.Why != why {
				_ = s.resets.update(func(f *resetFile) {
					if cur, ok := f.Due[id]; ok {
						cur.Why = why
						f.Due[id] = cur
					}
				})
			}
		}
	}
}
