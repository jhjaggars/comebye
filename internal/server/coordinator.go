package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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

func (b *briefStore) set(v Brief) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return writeFileAtomic(b.path, v)
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
	Role string    `json:"role"` // "coordinator"
	Name string    `json:"name,omitempty"`
	By   string    `json:"by,omitempty"`
	At   time.Time `json:"at"`
}

// RoleCoordinator is the one role there is.
const RoleCoordinator = "coordinator"

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
	return writeFileAtomic(s.path, s.data)
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
// still match, plus the roles of agents not seen (a host may be down).
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
		if n, seen := names[id]; seen && r.Name != "" && n != r.Name {
			continue // the pane now runs another agent
		}
		out[id] = r
	}
	return out
}

// recordRole sets a role on the hub (best effort: logged by the caller).
func (s *Server) recordRole(id, name, role, by string) error {
	if s.roles == nil {
		return errors.New("this instance keeps no roles")
	}
	return s.roles.set(id, Role{Role: role, Name: name, By: by, At: time.Now().UTC()})
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
	if b.Role != "" && b.Role != RoleCoordinator {
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
	if err := s.recordRole(b.AgentID, name, b.Role, s.by(r, "")); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "roles_error", "message": err.Error()})
		return
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

// handleSetBrief publishes a briefing: POST {markdown} (at most 64 KB).
func (s *Server) handleSetBrief(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		Markdown string `json:"markdown"`
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
	v := Brief{Markdown: b.Markdown, By: s.by(r, ""), At: time.Now().UTC()}
	if err := s.brief.set(v); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "brief_error", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"brief": v})
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
