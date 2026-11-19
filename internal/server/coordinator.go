package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/playbook"
)

// Coordinators: "coordinator" is a role, not a process. Any agent, of any
// kind, on any host can coordinate: its memory is the hub's ledger and
// briefing, and it acts through the comebye CLI (the agent role, see
// agentauth.go). The hub records which agents are coordinators (roles.json),
// so the app can pin them and guard their removal, and keeps the latest
// briefing a coordinator published (brief.json).

// maxBriefBytes caps the briefing's markdown.
const maxBriefBytes = 64 << 10

// Brief is the published briefing.
type Brief struct {
	Markdown string    `json:"markdown"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	// Since is set on a history entry that stands for several briefings
	// published close together: when the first of them was published (At is
	// the last). The window for merging runs from Since.
	Since time.Time `json:"since,omitempty"`
}

// briefStore keeps brief.json in the state directory.
type briefStore struct {
	mu   sync.Mutex
	path string
}

// get returns the briefing, nil when none was published.
func (b *briefStore) get() (*Brief, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, err := os.ReadFile(b.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out Brief
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", b.path, err)
	}
	return &out, nil
}

// maxBriefHistory bounds brief-history.json: the newest briefings, so the
// app can page back through earlier (say hourly) ones. About 3 MB at most.
const maxBriefHistory = 48

// briefMergeWindow: briefings published within this long of the history's
// newest entry are merged into it (the entry becomes the latest text), unless
// the "Needs you" section changed. A coordinator that publishes at every
// event would otherwise fill the 48 entries with near copies in a couple of
// hours. The current briefing (brief.json) is never merged: it is always the
// last one published.
const briefMergeWindow = 15 * time.Minute

// briefNeedsSame reports whether two briefings have the same "Needs you"
// section (or both have none), compared line by line.
func briefNeedsSame(a, b string) bool {
	ea, fa := briefNeedsEntries(a)
	eb, fb := briefNeedsEntries(b)
	if fa != fb || len(ea) != len(eb) {
		return false
	}
	for i := range ea {
		if strings.TrimSpace(ea[i]) != strings.TrimSpace(eb[i]) {
			return false
		}
	}
	return true
}

func (b *briefStore) historyPath() string {
	return filepath.Join(filepath.Dir(b.path), "brief-history.json")
}

func (b *briefStore) set(v Brief) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := writeFileAtomic(b.path, v); err != nil {
		return err
	}
	// The history is a convenience: the briefing is already published, so a
	// history that cannot be read or written does not fail the publish.
	h, _ := b.readHistory()
	if len(h) > 0 {
		anchor := h[0].At
		if !h[0].Since.IsZero() {
			anchor = h[0].Since
		}
		if d := v.At.Sub(anchor); d >= 0 && d < briefMergeWindow && briefNeedsSame(h[0].Markdown, v.Markdown) {
			v.Since = anchor
			h = h[1:]
		}
	}
	h = append([]Brief{v}, h...)
	if len(h) > maxBriefHistory {
		h = h[:maxBriefHistory]
	}
	_ = writeFileAtomic(b.historyPath(), h)
	return nil
}

// readHistory returns the kept briefings, newest first. Callers hold b.mu.
func (b *briefStore) readHistory() ([]Brief, error) {
	data, err := os.ReadFile(b.historyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Brief
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", b.historyPath(), err)
	}
	return out, nil
}

// history returns the kept briefings, newest first (at most limit when > 0).
func (b *briefStore) history(limit int) ([]Brief, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, err := b.readHistory()
	if limit > 0 && len(h) > limit {
		h = h[:limit]
	}
	return h, err
}

// writeFileAtomic writes v as JSON to path (temp file, fsync, rename),
// mode 0600.
func writeFileAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Role is an agent's recorded role. Name, when set, must still be the
// agent's name for the role to apply: herdr reuses pane ids.
type Role struct {
	Role string `json:"role"` // "coordinator" or "chat"
	Name string `json:"name,omitempty"`
	// Page is the Page a chat helper was started about.
	Page string    `json:"page,omitempty"`
	By   string    `json:"by,omitempty"`
	At   time.Time `json:"at"`
	// Main marks the main coordinator: the one events nobody owns go to
	// (route.go). At most one role has it. None marked: the first registered
	// live coordinator is main.
	Main bool `json:"main,omitempty"`
}

// RoleCoordinator marks an agent that coordinates.
const RoleCoordinator = "coordinator"

// RoleChat marks a read-only helper started to talk about a Page (chat.go).
// It is no worker: it has no ledger entry, so no sweep lists it.
const RoleChat = "chat"

// roleStore keeps roles.json: host-qualified agent id -> role.
type roleStore struct {
	mu   sync.Mutex
	path string
	data map[string]Role
}

func openRoles(path string) (*roleStore, error) {
	s := &roleStore{path: path, data: map[string]Role{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.data == nil {
		s.data = map[string]Role{}
	}
	return s, nil
}

// all returns a copy of the roles.
func (s *roleStore) all() map[string]Role {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Role, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// set records (role non-empty) or clears a role.
func (s *roleStore) set(id string, r Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Role == "" {
		if _, ok := s.data[id]; !ok {
			return nil
		}
		delete(s.data, id)
	} else {
		if _, ok := s.data[id]; !ok && len(s.data) >= maxRoles {
			// Make room: drop the oldest.
			ids := make([]string, 0, len(s.data))
			for k := range s.data {
				ids = append(ids, k)
			}
			sort.Slice(ids, func(i, j int) bool { return s.data[ids[i]].At.Before(s.data[ids[j]].At) })
			for _, k := range ids[:len(ids)-maxRoles+1] {
				delete(s.data, k)
			}
		}
		s.data[id] = r
	}
	s.ensureMainLocked()
	return writeFileAtomic(s.path, s.data)
}

// ensureMainLocked keeps exactly one coordinator marked main, durably: the
// first one registered becomes main and stays main until its role is removed;
// then the oldest remaining coordinator is. s.mu is held.
func (s *roleStore) ensureMainLocked() {
	first := ""
	for id, r := range s.data {
		if r.Role != RoleCoordinator {
			if r.Main {
				r.Main = false
				s.data[id] = r
			}
			continue
		}
		if r.Main {
			return
		}
		if o, ok := s.data[first]; first == "" || !ok || r.At.Before(o.At) || (r.At.Equal(o.At) && id < first) {
			first = id
		}
	}
	if first != "" {
		r := s.data[first]
		r.Main = true
		s.data[first] = r
	}
}

// maxRoles bounds roles.json; the oldest roles go first.
const maxRoles = 200

// isCoordinator reports whether the agent with id (named name, "" when
// unknown) is a coordinator.
func (s *Server) isCoordinator(id, name string) bool {
	if s.roles == nil {
		return false
	}
	s.roles.mu.Lock()
	defer s.roles.mu.Unlock()
	r, ok := s.roles.data[id]
	return ok && r.Role == RoleCoordinator && (r.Name == "" || name == "" || r.Name == name)
}

// liveRoles are the roles of the agents the fleet sees now whose names
// still match, plus the roles of agents not seen (a host may be down). An
// agent seen without a name keeps its role, as in isCoordinator: herdr
// clears a name for reasons that leave the agent as it was.
func (s *Server) liveRoles() map[string]Role {
	out := map[string]Role{}
	if s.roles == nil {
		return out
	}
	names := map[string]string{}
	for _, a := range s.fleet.Agents() {
		names[a.ID] = a.Name
	}
	for id, r := range s.roles.all() {
		if n, seen := names[id]; seen && r.Name != "" && n != "" && n != r.Name {
			continue // the pane now runs another agent
		}
		out[id] = r
	}
	// With no coordinator marked main, the first registered live one is
	// shown as main (the same one poolRoute takes).
	marked := false
	var first string
	var firstAt time.Time
	for id, r := range out {
		if r.Role != RoleCoordinator {
			continue
		}
		if r.Main {
			marked = true
		}
		if _, seen := names[id]; seen && (first == "" || r.At.Before(firstAt) || (r.At.Equal(firstAt) && id < first)) {
			first, firstAt = id, r.At
		}
	}
	if !marked && first != "" {
		r := out[first]
		r.Main = true
		out[first] = r
	}
	return out
}

// recordRole sets a role on the hub (best effort: logged by the caller).
func (s *Server) recordRole(id, name, role, by string) error {
	if s.roles == nil {
		return errors.New("this instance keeps no roles")
	}
	at, main := time.Now().UTC(), false
	if old, ok := s.roles.get(id); ok && old.Role == RoleCoordinator && role == RoleCoordinator {
		at, main = old.At, old.Main // registered again: still the same place in the order
	}
	return s.roles.set(id, Role{Role: role, Name: name, By: by, At: at, Main: main})
}

// handleRoles lists the coordinator roles: {roles: {id: role}}.
func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	writeJSON(w, map[string]any{"roles": s.liveRoles()})
}

// roleBody is the JSON body of a role change.
type roleBody struct {
	AgentID string `json:"agent_id"`
	Role    string `json:"role"` // "coordinator" or "" to clear
	// Page, with role "chat", marks the agent a chat helper about that Page.
	// Only a -test-mode hub takes it (the browser tests and the dev server have
	// no real launch to make helpers with); chat.go makes the real ones.
	Page string `json:"page,omitempty"`
	// Main, with role "coordinator", makes this the main coordinator (and
	// the one that was is no longer).
	Main bool `json:"main,omitempty"`
}

// handleSetRole makes an agent a coordinator or not: POST {agent_id,
// role}. The user's alone (the app's "Make coordinator").
func (s *Server) handleSetRole(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b roleBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.AgentID == "" || len(b.AgentID) > 200 {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if b.Role == RoleChat && s.cfg.TestMode {
		if s.pages == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "page_not_found", "message": "no pages here"})
			return
		}
		if _, ok := s.pages.get(b.Page); !ok {
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "page_not_found", "message": "no such page"})
			return
		}
	} else if b.Role != "" && b.Role != RoleCoordinator {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_role", "message": "the only role is coordinator"})
		return
	}
	name := ""
	found := false
	for _, a := range s.fleet.Agents() {
		if a.ID == b.AgentID {
			name, found = a.Name, true
			break
		}
	}
	if !found && b.Role != "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "agent_not_found", "message": "no such agent"})
		return
	}
	if b.Role == RoleChat {
		err := s.roles.set(b.AgentID, Role{Role: RoleChat, Name: name, Page: b.Page, By: s.by(r, ""), At: time.Now().UTC()})
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "roles_error", "message": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"roles": s.liveRoles()})
		return
	}
	if err := s.recordRole(b.AgentID, name, b.Role, s.by(r, "")); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "roles_error", "message": err.Error()})
		return
	}
	if b.Main && b.Role == RoleCoordinator {
		if err := s.roles.setMain(b.AgentID); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "roles_error", "message": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"roles": s.liveRoles()})
}

// handleGetBrief answers {brief: {markdown, by, at} | null, schedule}
// (the schedule as GET /api/brief/schedule has it, so the app polls once).
func (s *Server) handleGetBrief(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	b, err := s.brief.get()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "brief_error", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"brief": b, "schedule": s.briefSchedView(time.Now())})
}

// handleGetBriefHistory answers {briefs: [{markdown, by, at}, ...]}, the
// last briefings published (at most 48) newest first; ?limit=N takes fewer.
func (s *Server) handleGetBriefHistory(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h, err := s.brief.history(limit)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "brief_error", "message": err.Error()})
		return
	}
	if h == nil {
		h = []Brief{}
	}
	writeJSON(w, map[string]any{"briefs": h, "max": maxBriefHistory})
}

// handleSetBrief publishes a briefing: POST {markdown} (at most 64 KB).
func (s *Server) handleSetBrief(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		Markdown string `json:"markdown"`
		NoCheck  bool   `json:"no_check"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBriefBytes+16<<10)).Decode(&b); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "brief_too_large", "message": "the briefing is larger than 64 KB"})
			return
		}
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	switch {
	case strings.TrimSpace(b.Markdown) == "":
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_brief", "message": "the briefing is empty"})
		return
	case len(b.Markdown) > maxBriefBytes:
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "brief_too_large", "message": "the briefing is larger than 64 KB"})
		return
	}
	var warns []string
	if s.todos != nil && !b.NoCheck {
		var fail string
		if fail, warns = checkBrief(b.Markdown, s.todos.list("")); fail != "" {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{"code": "needs_you_unlinked", "message": fail})
			return
		}
	}
	v := Brief{Markdown: b.Markdown, By: s.by(r, ""), At: time.Now().UTC()}
	if err := s.brief.set(v); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "brief_error", "message": err.Error()})
		return
	}
	s.briefPublished(v.At)
	resp := map[string]any{"brief": v}
	if len(warns) > 0 {
		resp["warnings"] = warns
	}
	writeJSON(w, resp)
}

// handlePlaybook serves the coordinator playbook: ?harness=pi|claude|
// generic (default generic), as text/markdown.
func (s *Server) handlePlaybook(w http.ResponseWriter, r *http.Request) {
	text, err := playbook.Render(r.URL.Query().Get("harness"))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_harness", "message": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// selfGuard refuses an agent prompting itself (a coordinator that prompts
// its own pane talks to itself in a loop).
func (s *Server) selfGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		host := r.PathValue("host")
		if host == "" {
			host = s.fleet.LocalName()
		}
		if c.agent() && s.isSelf(c, host, r.PathValue("target")) {
			writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "self", "message": "an agent cannot do this to itself"})
			return
		}
		next(w, r)
	}
}

// isSelf reports whether target (a pane id or name) on host is the
// calling agent.
func (s *Server) isSelf(c caller, host, target string) bool {
	if c.pane == "" || host != c.host {
		return false
	}
	if target == c.pane {
		return true
	}
	for _, a := range s.fleet.Agents() {
		if a.Host == host && a.PaneID == c.pane && a.Name != "" && a.Name == target {
			return true
		}
	}
	return false
}

// pickCoordinator picks a coordinator for a notice nobody owns: the pool
// (poolCoordinator). A notice about an agent a coordinator started or
// prompted goes through routeCoordinator instead (route.go).
func (s *Server) pickCoordinator() (*herdr.AgentInfo, string) {
	return s.poolCoordinator()
}

// poolCoordinator is poolRoute without the label.
func (s *Server) poolCoordinator() (*herdr.AgentInfo, string) {
	r := s.poolRoute()
	return r.Agent, r.Why
}

// promptAgent sends text to an agent on any host.
func (s *Server) promptAgent(ctx context.Context, a herdr.AgentInfo, text string) error {
	if a.Host == "" || a.Host == s.fleet.LocalName() {
		if err := s.guardPrompt(ctx, a); err != nil {
			return err
		}
		s.recordTyped(ctx, a, text)
		_, err := s.client.Prompt(ctx, a.PaneID, text, herdr.PromptOptions{})
		return err
	}
	body, _ := json.Marshal(promptBody{Text: text})
	status, data, err := s.fleet.CallAgent(ctx, a.Host, a.PaneID, "prompt", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s answered %d: %s", a.Host, status, strings.TrimSpace(string(data)))
	}
	return nil
}
