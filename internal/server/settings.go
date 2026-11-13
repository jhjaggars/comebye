package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// Host settings: which agent kind, model and thinking level this host
// starts by default, and which kinds it may start at all.
//
// They live in settings.json in the state directory of the host that
// starts the agent, and are applied there (chooseAgent), so every way of
// starting one - the launch dialog, POST /api/launch, a handoff, a queued
// plan step - follows them without the hub doing anything. The hub reads
// and edits a peer's through the host-qualified routes. Only the user
// writes; agents read.
//
// Precedence for one start: the request, then the file, then the
// -default-kind / -allowed-kinds flags, then the old guess (the kind most
// running agents use).

// kindSettings is the model and thinking level a kind starts with.
type kindSettings struct {
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// hostSettings is the document in settings.json.
type hostSettings struct {
	DefaultKind  string                  `json:"default_kind,omitempty"`
	AllowedKinds []string                `json:"allowed_kinds,omitempty"`
	Kinds        map[string]kindSettings `json:"kinds,omitempty"`
	// MaxWorkersPerRepo is the most running workers this host keeps per
	// repository: nil is the default (defaultMaxWorkersPerRepo), 0 no cap.
	MaxWorkersPerRepo *int       `json:"max_workers_per_repo,omitempty"`
	Rev               int        `json:"rev"`
	UpdatedBy         string     `json:"updated_by,omitempty"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
}

// settingsStore keeps settings.json. Safe for concurrent use; a nil store
// holds nothing.
type settingsStore struct {
	path string
	mu   sync.Mutex
	cur  hostSettings
}

func settingsPath(stateDir string) string { return filepath.Join(stateDir, "settings.json") }

// openSettings reads the file. A file that cannot be read or parsed is
// logged and ignored: a bad edit must not stop the host from starting.
func openSettings(path string) *settingsStore {
	s := &settingsStore{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("settings: %v (starting with none)", err)
		}
		return s
	}
	var h hostSettings
	if err := json.Unmarshal(b, &h); err != nil {
		log.Printf("settings: %s: %v (starting with none)", path, err)
		return s
	}
	s.cur = normalizeSettings(h)
	return s
}

func normalizeSettings(h hostSettings) hostSettings {
	h.DefaultKind = strings.ToLower(strings.TrimSpace(h.DefaultKind))
	h.AllowedKinds = cleanKinds(h.AllowedKinds)
	kinds := map[string]kindSettings{}
	for k, v := range h.Kinds {
		v.Model, v.Thinking = strings.TrimSpace(v.Model), strings.TrimSpace(v.Thinking)
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && (v.Model != "" || v.Thinking != "") {
			kinds[k] = v
		}
	}
	if h.MaxWorkersPerRepo != nil {
		n := min(max(*h.MaxWorkersPerRepo, 0), maxWorkersCeiling)
		h.MaxWorkersPerRepo = &n
	}
	h.Kinds = nil
	if len(kinds) > 0 {
		h.Kinds = kinds
	}
	return h
}

func cleanKinds(in []string) []string {
	var out []string
	for _, k := range in {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// get returns a copy of the settings.
func (st *settingsStore) get() hostSettings {
	if st == nil {
		return hostSettings{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return copySettings(st.cur)
}

func copySettings(h hostSettings) hostSettings {
	if h.MaxWorkersPerRepo != nil {
		n := *h.MaxWorkersPerRepo
		h.MaxWorkersPerRepo = &n
	}
	h.AllowedKinds = slices.Clone(h.AllowedKinds)
	if h.Kinds != nil {
		m := make(map[string]kindSettings, len(h.Kinds))
		for k, v := range h.Kinds {
			m[k] = v
		}
		h.Kinds = m
	}
	return h
}

var errSettingsRev = fmt.Errorf("the settings changed since they were read")

// put replaces the settings (rev, when not nil, must be the current one),
// bumps the revision and writes the file; on a write error nothing changes.
func (st *settingsStore) put(n hostSettings, rev *int, by string, now time.Time) (hostSettings, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if rev != nil && *rev != st.cur.Rev {
		return copySettings(st.cur), errSettingsRev
	}
	n = normalizeSettings(n)
	n.Rev = st.cur.Rev + 1
	n.UpdatedBy, n.UpdatedAt = by, &now
	b, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return copySettings(st.cur), err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return copySettings(st.cur), err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".settings-*.tmp")
	if err != nil {
		return copySettings(st.cur), err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(b, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), st.path)
	}
	if err != nil {
		return copySettings(st.cur), err
	}
	st.cur = n
	return copySettings(n), nil
}

const (
	// defaultMaxWorkersPerRepo is the cap on running workers per repository
	// when the host's settings do not say; 0 in the settings means no cap.
	defaultMaxWorkersPerRepo = 4
	maxWorkersCeiling        = 1000
)

// maxWorkersPerRepo is the cap in force on this host (0: none) and
// whether the user set it.
func (s *Server) maxWorkersPerRepo() (n int, set bool) {
	if v := s.settings.get().MaxWorkersPerRepo; v != nil {
		return *v, true
	}
	return defaultMaxWorkersPerRepo, false
}

// effectiveSettings are the values in force: the file's, else the flags'.
type effectiveSettings struct {
	DefaultKind   string `json:"default_kind"`
	DefaultSource string `json:"default_source"` // setting, flag or auto
	AllowedKinds  []string
	AllowedSource string // setting, flag or none
}

// effective merges the file with the flags' seeds.
func (s *Server) effective() effectiveSettings {
	f := s.settings.get()
	e := effectiveSettings{DefaultSource: "auto", AllowedSource: "none"}
	switch {
	case f.DefaultKind != "":
		e.DefaultKind, e.DefaultSource = f.DefaultKind, "setting"
	case s.cfg.DefaultKind != "":
		e.DefaultKind, e.DefaultSource = s.cfg.DefaultKind, "flag"
	}
	switch {
	case len(f.AllowedKinds) > 0:
		e.AllowedKinds, e.AllowedSource = f.AllowedKinds, "setting"
	case len(s.cfg.AllowedKinds) > 0:
		e.AllowedKinds, e.AllowedSource = cleanKinds(s.cfg.AllowedKinds), "flag"
	}
	return e
}

// agentChoice is what a start resolved to.
type agentChoice struct {
	Kind        string
	KindSource  string // request, setting, flag or auto
	Remembered  bool
	Model       string
	Thinking    string
	ModelSource string // request, setting or "" (the agent's own default)
	Args        []string
	Warnings    []string
}

// chooseAgent resolves the kind, model and thinking level of a start on
// this host: the request's values, else the host's settings, else the
// agent's own defaults; the kind must be allowed and installed here, and
// the model and thinking level are checked as a request's are.
func (s *Server) chooseAgent(ctx context.Context, reqKind, reqModel, reqThinking string, running []herdr.AgentInfo) (*agentChoice, *launchError) {
	reqKind = strings.TrimSpace(reqKind)
	eff := s.effective()
	if reqKind != "" && len(eff.AllowedKinds) > 0 && !slices.Contains(eff.AllowedKinds, reqKind) {
		return nil, &launchError{status: http.StatusBadRequest, code: "kind_not_allowed",
			message: "agent kind " + reqKind + " is not allowed on this host (allowed: " + strings.Join(eff.AllowedKinds, ", ") + ")"}
	}
	kinds, rem := s.launchKindsDetail(running)
	c := &agentChoice{}
	var notice string
	switch {
	case reqKind != "":
		c.Kind, c.KindSource = reqKind, "request"
	case eff.DefaultKind != "" && slices.Contains(kinds, eff.DefaultKind):
		c.Kind, c.KindSource = eff.DefaultKind, eff.DefaultSource
	default:
		if eff.DefaultKind != "" {
			notice = "this host's default agent kind " + eff.DefaultKind + " is not available here; using " + defaultKind(kinds, running) + " instead"
		}
		c.Kind, c.KindSource = defaultKind(kinds, running), "auto"
	}
	if !slices.Contains(kinds, c.Kind) {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_kind", message: "agent kind " + c.Kind + " is not available on this host"}
	}
	c.Remembered = rem[c.Kind]
	if notice != "" {
		c.Warnings = append(c.Warnings, notice)
	}
	if c.KindSource == "request" && eff.DefaultKind != "" && eff.DefaultKind != c.Kind && slices.Contains(kinds, eff.DefaultKind) {
		c.Warnings = append(c.Warnings, c.Kind+" was requested; this host's default is "+eff.DefaultKind)
	}

	c.Model, c.Thinking = strings.TrimSpace(reqModel), strings.TrimSpace(reqThinking)
	fromSettings := false
	ks := s.settings.get().Kinds[c.Kind]
	if c.Model == "" && ks.Model != "" {
		c.Model, fromSettings = ks.Model, true
		c.ModelSource = "setting"
	} else if c.Model != "" {
		c.ModelSource = "request"
	}
	if c.Thinking == "" && ks.Thinking != "" && c.Kind == "pi" {
		c.Thinking, fromSettings = ks.Thinking, true
	}
	args, lerr := s.pickModel(ctx, c.Kind, c.Model, c.Thinking)
	if lerr != nil {
		if fromSettings {
			lerr.message += " (the value comes from this host's settings)"
		}
		return nil, lerr
	}
	c.Args = args
	return c, nil
}

// ---- API ----

// settingsView is the answer of GET /api/settings: the document, what is
// in force, and what the host can start.
type settingsView struct {
	Host      string       `json:"host"`
	Version   string       `json:"version,omitempty"`
	Settings  hostSettings `json:"settings"`
	Effective struct {
		DefaultKind   string   `json:"default_kind"`
		DefaultSource string   `json:"default_source"`
		AllowedKinds  []string `json:"allowed_kinds"`
		AllowedSource string   `json:"allowed_source"`
		// MaxWorkersPerRepo is the cap on running workers per repository
		// (0: none); its source is "setting" or "default".
		MaxWorkersPerRepo       int    `json:"max_workers_per_repo"`
		MaxWorkersPerRepoSource string `json:"max_workers_per_repo_source"`
		// Notice says the default in force cannot be used right now.
		Notice string `json:"notice,omitempty"`
	} `json:"effective"`
	InstalledKinds []string `json:"installed_kinds"`
	ModelKinds     []string `json:"model_kinds"`
	Launch         struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason,omitempty"`
	} `json:"launch"`
}

func (s *Server) settingsView() settingsView {
	v := settingsView{Host: s.fleet.LocalName(), Version: s.cfg.Version, Settings: s.settings.get(), ModelKinds: modelKinds}
	eff := s.effective()
	v.Effective.DefaultKind, v.Effective.DefaultSource = eff.DefaultKind, eff.DefaultSource
	v.Effective.AllowedKinds, v.Effective.AllowedSource = eff.AllowedKinds, eff.AllowedSource
	n, set := s.maxWorkersPerRepo()
	v.Effective.MaxWorkersPerRepo, v.Effective.MaxWorkersPerRepoSource = n, "default"
	if set {
		v.Effective.MaxWorkersPerRepoSource = "setting"
	}
	if v.Effective.AllowedKinds == nil {
		v.Effective.AllowedKinds = []string{}
	}
	running := s.localAgents()
	v.InstalledKinds = s.launchKinds(running)
	if v.InstalledKinds == nil {
		v.InstalledKinds = []string{}
	}
	if eff.DefaultKind != "" && !slices.Contains(v.InstalledKinds, eff.DefaultKind) {
		v.Effective.Notice = "the default " + eff.DefaultKind + " is not available on this host"
	}
	switch roots := s.launchRoots(); {
	case len(s.cfg.LaunchRoots) == 0:
		v.Launch.Reason = "launching is turned off on this host"
	case len(roots) == 0:
		v.Launch.Reason = "no launch root exists on this host"
	case len(v.InstalledKinds) == 0:
		v.Launch.Reason = "no supported agent is installed on this host"
	default:
		v.Launch.Enabled = true
	}
	return v
}

// handleGetSettings is GET /api/settings (and /api/hosts/{host}/settings).
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.settingsView())
}

// settingsGuard limits a tagged node to its own host's settings.
func (s *Server) settingsGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c := callerOf(r); c.agent() && c.tagged && !sameHost(s.hostOf(r), c.host) {
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "read_forbidden", "message": "a tagged worker may read only its own host's settings"})
			return
		}
		next(w, r)
	}
}

// settingsBody is the body of PUT /api/settings: the whole document.
type settingsBody struct {
	DefaultKind  string                  `json:"default_kind"`
	AllowedKinds []string                `json:"allowed_kinds"`
	Kinds        map[string]kindSettings `json:"kinds"`
	// MaxWorkersPerRepo: absent keeps the default; 0 lifts the cap.
	MaxWorkersPerRepo *int `json:"max_workers_per_repo"`
	// Rev, when given, must be the revision read; a later one is a 409.
	Rev *int `json:"rev"`
}

func badSettings(w http.ResponseWriter, msg string) {
	writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_settings", "message": msg})
}

// handlePutSettings is PUT /api/settings: the user's alone (the route is
// wrapped in auth). The kinds must be ones herdr knows, the default one
// of the allowed kinds, and models and thinking levels well formed.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"code": "no_settings", "message": "this host has no state directory to keep settings in"})
		return
	}
	var b settingsBody
	if !decodeBody(w, r, &b) {
		return
	}
	n := normalizeSettings(hostSettings{DefaultKind: b.DefaultKind, AllowedKinds: b.AllowedKinds, Kinds: b.Kinds, MaxWorkersPerRepo: b.MaxWorkersPerRepo})
	if b.MaxWorkersPerRepo != nil && (*b.MaxWorkersPerRepo < 0 || *b.MaxWorkersPerRepo > maxWorkersCeiling) {
		badSettings(w, fmt.Sprintf("max_workers_per_repo: want 0 (no cap) to %d", maxWorkersCeiling))
		return
	}
	known := func(k string) bool { return slices.Contains(herdr.AgentKinds, k) }
	for _, k := range n.AllowedKinds {
		if !known(k) {
			badSettings(w, "allowed_kinds: unknown agent kind "+plainText(k, 30))
			return
		}
	}
	if n.DefaultKind != "" {
		if !known(n.DefaultKind) {
			badSettings(w, "default_kind: unknown agent kind "+plainText(n.DefaultKind, 30))
			return
		}
		if len(n.AllowedKinds) > 0 && !slices.Contains(n.AllowedKinds, n.DefaultKind) {
			badSettings(w, "default_kind "+n.DefaultKind+" is not among allowed_kinds")
			return
		}
	}
	for k, v := range n.Kinds {
		if !known(k) {
			badSettings(w, "kinds: unknown agent kind "+plainText(k, 30))
			return
		}
		if _, merr := modelArgs(k, v.Model, v.Thinking, nil); merr != nil {
			badSettings(w, "kinds."+k+": "+merr.message)
			return
		}
	}
	got, err := s.settings.put(n, b.Rev, requester(r, ""), time.Now().UTC())
	switch {
	case err == errSettingsRev:
		writeJSONStatus(w, http.StatusConflict, map[string]any{"code": "rev_conflict", "message": err.Error(), "settings": got})
		return
	case err != nil:
		log.Printf("settings: %v", err)
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "settings_write", "message": "could not save the settings: " + errMessage(err)})
		return
	}
	writeJSON(w, s.settingsView())
}
