package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// Seen: herdr's "done" means finished and not yet seen, and it clears when
// the pane is focused in herdr's own TUI. herdr has no call that marks an
// agent seen without moving the user's terminal focus, and viewing an agent
// in the app tells it nothing. So the hub keeps its own record: when the
// user's app has an agent's latest message on screen it posts to
// /api/agents/{target}/seen (or the host-qualified route), the hub notes the
// done spell it was shown, and every list the browsers get shows that agent
// as idle until it works and finishes again. Only the user can post it
// (s.auth), so an agent cannot mark itself or another read.

const seenFile = "agents-seen.json"

type seenRec struct {
	// Since is the StatusSince of the done spell the user saw: a done agent
	// whose StatusSince is no later than this shows as idle.
	Since time.Time `json:"since"`
	// At is when the user saw it.
	At time.Time `json:"at"`
}

type seenStore struct {
	mu      sync.Mutex
	path    string
	m       map[string]seenRec
	adopted map[string]bool // keys this process has looked at once
}

func (s *Server) seenStore() *seenStore {
	s.seenOnce.Do(func() {
		st := &seenStore{m: map[string]seenRec{}, adopted: map[string]bool{}}
		if s.cfg.StateDir != "" {
			st.path = filepath.Join(s.cfg.StateDir, seenFile)
			if b, err := os.ReadFile(st.path); err == nil {
				_ = json.Unmarshal(b, &st.m)
			}
		}
		s.seen = st
	})
	return s.seen
}

func seenKey(a *herdr.AgentInfo) string { return state.AgentKey(a) + "|" + a.Instance }

// save writes the map; the caller holds mu.
func (st *seenStore) save() {
	if st.path == "" {
		return
	}
	if b, err := json.Marshal(st.m); err == nil {
		tmp := st.path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, st.path)
		}
	}
}

// mark records that the user has seen the agent's current done spell. It
// reports whether the shown status changes.
func (st *seenStore) mark(a *herdr.AgentInfo, now time.Time) bool {
	if a.Status != herdr.StatusDone || a.StatusSince == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	k := seenKey(a)
	if r, ok := st.m[k]; ok && !a.StatusSince.After(r.Since) {
		return false
	}
	st.m[k] = seenRec{Since: *a.StatusSince, At: now}
	for o, r := range st.m { // forget agents long gone
		if now.Sub(r.At) > 30*24*time.Hour {
			delete(st.m, o)
			delete(st.adopted, o)
		}
	}
	st.adopted[k] = true
	st.save()
	return true
}

// apply turns "done" into "idle" for the agents whose done spell the user
// has seen. It works on the slice it is given (a copy), and forgets records
// of agents that are gone.
func (st *seenStore) apply(agents []herdr.AgentInfo) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range agents {
		a := &agents[i]
		k := seenKey(a)
		r, ok := st.m[k]
		if !ok {
			continue
		}
		if !st.adopted[k] {
			// First look since this process started. StatusSince is then a
			// baseline (the restart), not the time the agent finished, so a
			// seen done agent keeps its mark across a restart.
			st.adopted[k] = true
			if a.Status == herdr.StatusDone && a.StatusSince != nil && a.StatusSince.After(r.Since) {
				r.Since = *a.StatusSince
				st.m[k] = r
			}
		}
		if a.Status == herdr.StatusDone && a.StatusSince != nil && !a.StatusSince.After(r.Since) {
			a.Status = herdr.StatusIdle
		}
	}
}

// handleSeen is the user's app saying it has the agent's latest message on
// screen. The host route reaches here on the hub for a peer's agent too: the
// record is the hub's, so no peer is asked.
func (s *Server) handleSeen(w http.ResponseWriter, r *http.Request) {
	host, target := r.PathValue("host"), r.PathValue("target")
	var a *herdr.AgentInfo
	for _, x := range s.fleet.Agents() {
		hostOK := x.Host == host || host == "" && s.isLocalHost(x.Host)
		if hostOK && (x.PaneID == target || x.Name == target || x.ID == target) {
			x := x
			a = &x
			break
		}
	}
	if a == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	changed := s.seenStore().mark(a, time.Now())
	if changed {
		s.fleet.Changed()
	}
	writeJSON(w, map[string]bool{"changed": changed})
}
