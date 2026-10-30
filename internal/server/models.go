package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Choosing a model at launch: which kinds take one, how it reaches the
// agent's command line, which models a host offers, and the check that a
// request names one that exists.
//
// "Default" is always "no flag": an empty model leaves the agent to use
// its own configured default.

// modelCatalogTTL is how long a kind's model list is reused. pi's list
// costs a process start; a host's models change when someone edits a
// config file, not by the minute.
const modelCatalogTTL = 5 * time.Minute

// modelListTimeout bounds the command that lists a kind's models.
const modelListTimeout = 20 * time.Second

// thinkingLevels are pi's --thinking values.
var thinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// modelRe is what a model name may look like. The name is typed into the
// agent's pane by a shell, so it stays within characters no shell treats
// specially.
var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// modelFlag is the flag each supporting kind takes its model with.
var modelFlag = map[string]string{
	"pi":       "--model",
	"claude":   "--model",
	"codex":    "-m",
	"gemini":   "-m",
	"opencode": "-m",
}

// modelKinds lists the kinds that take a model, in the order they are shown.
var modelKinds = []string{"pi", "claude", "codex", "gemini", "opencode"}

// modelError is a request's model that cannot be used, ready to answer.
type modelError struct{ code, message string }

func (e *modelError) launchError() *launchError {
	return &launchError{status: http.StatusBadRequest, code: e.code, message: e.message}
}

// modelArgs checks a request's model and thinking level for kind and
// returns the arguments that select them (nil for the defaults). catalog
// is the host's model list for kind, consulted for the kinds whose list is
// exact (pi); pass nil to check only the syntax.
func modelArgs(kind, model, thinking string, catalog *modelCatalog) ([]string, *modelError) {
	model, thinking = strings.TrimSpace(model), strings.TrimSpace(thinking)
	if model == "" && thinking == "" {
		return nil, nil
	}
	if _, ok := modelFlag[kind]; !ok {
		return nil, &modelError{"model_unsupported", "choosing a model is not supported for " + kind + " (supported: " + strings.Join(modelKinds, ", ") + ")"}
	}
	var args []string
	if model != "" {
		if !modelRe.MatchString(model) {
			return nil, &modelError{"bad_model", "model " + plainText(model, 60) + " is not a model name (letters, digits and . _ : / @ + - only)"}
		}
		if kind == "pi" && catalog != nil && len(catalog.Models) > 0 && !catalog.has(model) {
			return nil, &modelError{"unknown_model", "pi has no model " + model + " on this host (see `comebye models --kind pi`)"}
		}
		args = append(args, modelFlag[kind], model)
	}
	if thinking != "" {
		if kind != "pi" {
			return nil, &modelError{"thinking_unsupported", "a thinking level can only be chosen for pi, not " + kind}
		}
		known := false
		for _, l := range thinkingLevels {
			known = known || l == thinking
		}
		if !known {
			return nil, &modelError{"bad_thinking", "thinking level " + plainText(thinking, 20) + " is not one of " + strings.Join(thinkingLevels, ", ")}
		}
		args = append(args, "--thinking", thinking)
	}
	return args, nil
}

// pickModel is modelArgs for a request to this host: it looks the host's
// catalog up where the check needs it.
func (s *Server) pickModel(ctx context.Context, kind, model, thinking string) ([]string, *launchError) {
	if strings.TrimSpace(model) == "" && strings.TrimSpace(thinking) == "" {
		return nil, nil
	}
	var cat *modelCatalog
	if kind == "pi" {
		cat = s.modelCatalog(ctx, kind)
	}
	args, merr := modelArgs(kind, model, thinking, cat)
	if merr != nil {
		return nil, merr.launchError()
	}
	return args, nil
}

// modelInfo is one selectable model.
type modelInfo struct {
	ID       string `json:"id"` // what is passed to the agent
	Provider string `json:"provider,omitempty"`
	Name     string `json:"name,omitempty"`
	// Thinking: the model reasons (pi only).
	Thinking bool `json:"thinking,omitempty"`
	// Note is a hint shown beside the model.
	Note string `json:"note,omitempty"`
}

// modelCatalog is what a host offers for one kind.
type modelCatalog struct {
	Kind   string      `json:"kind"`
	Models []modelInfo `json:"models"`
	// Default is the model the agent uses without a flag, when known.
	Default string `json:"default,omitempty"`
	// DefaultThinking is pi's default thinking level, when known.
	DefaultThinking string `json:"default_thinking,omitempty"`
	// Thinking lists the levels that can be chosen (pi only).
	Thinking []string `json:"thinking_levels,omitempty"`
	// Source says where the list came from; Exact: the list is every model
	// there is (pi), else a starting point and any name may be typed.
	Source string `json:"source"`
	Exact  bool   `json:"exact"`
	Error  string `json:"error,omitempty"`

	at time.Time
}

func (c *modelCatalog) has(id string) bool {
	for _, m := range c.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

type modelCache struct {
	mu   sync.Mutex
	data map[string]*modelCatalog
}

// modelCatalog returns kind's models on this host, cached for
// modelCatalogTTL. A failed listing is cached shorter so a broken tool is
// not run on every keystroke.
func (s *Server) modelCatalog(ctx context.Context, kind string) *modelCatalog {
	s.models.mu.Lock()
	defer s.models.mu.Unlock()
	if c, ok := s.models.data[kind]; ok {
		ttl := modelCatalogTTL
		if c.Error != "" {
			ttl = 30 * time.Second
		}
		if time.Since(c.at) < ttl {
			return c
		}
	}
	lister := s.listModels
	if lister == nil {
		lister = discoverModels
	}
	c := lister(ctx, kind)
	c.at = time.Now()
	if s.models.data == nil {
		s.models.data = map[string]*modelCatalog{}
	}
	s.models.data[kind] = c
	return c
}

// handleModels is GET /api/models?kind=K (and /api/hosts/{host}/models):
// the models this host offers for a kind, or for every kind that takes one
// and is installed here when kind is omitted.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	ctx, cancel := context.WithTimeout(r.Context(), modelListTimeout+5*time.Second)
	defer cancel()
	if kind != "" {
		if _, ok := modelFlag[kind]; !ok {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "model_unsupported", "message": "choosing a model is not supported for " + kind + " (supported: " + strings.Join(modelKinds, ", ") + ")"})
			return
		}
		writeJSON(w, map[string]any{"host": s.fleet.LocalName(), "catalogs": []*modelCatalog{s.modelCatalog(ctx, kind)}})
		return
	}
	var out []*modelCatalog
	have := map[string]bool{}
	for _, k := range s.launchKinds(s.localAgents()) {
		have[k] = true
	}
	for _, k := range modelKinds {
		if have[k] {
			out = append(out, s.modelCatalog(ctx, k))
		}
	}
	if out == nil {
		out = []*modelCatalog{}
	}
	writeJSON(w, map[string]any{"host": s.fleet.LocalName(), "catalogs": out})
}

// ---- discovery ----

func discoverModels(ctx context.Context, kind string) *modelCatalog {
	home, _ := os.UserHomeDir()
	switch kind {
	case "pi":
		return discoverPi(ctx, home)
	case "claude":
		return discoverClaude(home)
	case "codex":
		return discoverCodex(home)
	case "gemini":
		return &modelCatalog{Kind: kind, Source: "curated", Thinking: nil, Models: []modelInfo{
			{ID: "gemini-2.5-pro", Provider: "Gemini"}, {ID: "gemini-2.5-flash", Provider: "Gemini"}, {ID: "gemini-2.5-flash-lite", Provider: "Gemini"},
		}}
	case "opencode":
		return discoverOpencode(ctx)
	}
	return &modelCatalog{Kind: kind, Source: "none", Error: "no model list for " + kind}
}

// runList runs an agent's own listing command (found like launchable
// agents are, PATH or the usual per-user bin directories).
func runList(ctx context.Context, name string, args ...string) ([]byte, error) {
	path := lookExec(name)
	if path == "" {
		return nil, errors.New(name + " is not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()
	return exec.CommandContext(ctx, path, args...).Output()
}

// parsePiModels reads `pi --list-models`: a table whose header starts
// "provider model", preceded by whatever banners extensions print.
func parsePiModels(out []byte) []modelInfo {
	var models []modelInfo
	inTable := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if !inTable {
			inTable = len(f) >= 2 && f[0] == "provider" && f[1] == "model"
			continue
		}
		if len(f) < 2 {
			continue
		}
		m := modelInfo{Provider: f[0], Name: f[1], ID: f[0] + "/" + f[1]}
		// columns: context max-out thinking images
		if len(f) >= 5 {
			m.Thinking = f[len(f)-2] == "yes"
		}
		if modelRe.MatchString(m.ID) {
			models = append(models, m)
		}
	}
	return models
}

func discoverPi(ctx context.Context, home string) *modelCatalog {
	c := &modelCatalog{Kind: "pi", Source: "pi --list-models", Exact: true, Thinking: thinkingLevels}
	out, err := runList(ctx, "pi", "--list-models")
	if err == nil {
		c.Models = parsePiModels(out)
	}
	if len(c.Models) == 0 {
		// pi did not answer: fall back to the models its config file
		// defines, which are not exact (built-in models are missing).
		c.Source, c.Exact = "~/.pi/agent/models.json", false
		c.Models = piConfigModels(filepath.Join(home, ".pi", "agent", "models.json"))
		if err != nil {
			c.Error = "pi --list-models failed: " + err.Error()
		}
	}
	var set struct {
		Provider string `json:"defaultProvider"`
		Model    string `json:"defaultModel"`
		Thinking string `json:"defaultThinkingLevel"`
	}
	if b, e := os.ReadFile(filepath.Join(home, ".pi", "agent", "settings.json")); e == nil && json.Unmarshal(b, &set) == nil {
		if set.Model != "" {
			c.Default = set.Model
			if set.Provider != "" {
				c.Default = set.Provider + "/" + set.Model
			}
		}
		c.DefaultThinking = set.Thinking
	}
	sortModels(c.Models)
	return c
}

// piConfigModels reads the models a pi models.json defines.
func piConfigModels(path string) []modelInfo {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg struct {
		Providers map[string]struct {
			Models []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Reasoning bool   `json:"reasoning"`
			} `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	var out []modelInfo
	for p, prov := range cfg.Providers {
		for _, m := range prov.Models {
			if id := p + "/" + m.ID; modelRe.MatchString(id) {
				out = append(out, modelInfo{ID: id, Provider: p, Name: m.ID, Thinking: m.Reasoning})
			}
		}
	}
	return out
}

func sortModels(ms []modelInfo) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Provider != ms[j].Provider {
			return ms[i].Provider < ms[j].Provider
		}
		return ms[i].ID < ms[j].ID
	})
}

// claudeModels is curated: claude has no command that lists models. The
// aliases follow the newest model of each family; the full ids pin one.
var claudeModels = []modelInfo{
	{ID: "fable", Provider: "Aliases", Note: "newest Fable"},
	{ID: "opus", Provider: "Aliases", Note: "newest Opus"},
	{ID: "sonnet", Provider: "Aliases", Note: "newest Sonnet"},
	{ID: "haiku", Provider: "Aliases", Note: "newest Haiku"},
	{ID: "claude-fable-5-1", Provider: "Full ids"},
	{ID: "claude-opus-5-5", Provider: "Full ids"},
	{ID: "claude-sonnet-5-5", Provider: "Full ids"},
	{ID: "claude-haiku-5-5", Provider: "Full ids"},
}

func discoverClaude(home string) *modelCatalog {
	c := &modelCatalog{Kind: "claude", Source: "curated", Models: append([]modelInfo(nil), claudeModels...)}
	var set struct {
		Model string `json:"model"`
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil && json.Unmarshal(b, &set) == nil {
		c.Default = set.Model
	}
	return c
}

var tomlModelRe = regexp.MustCompile(`(?m)^model\s*=\s*"([^"]+)"`)

func discoverCodex(home string) *modelCatalog {
	c := &modelCatalog{Kind: "codex", Source: "~/.codex"}
	if b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml")); err == nil {
		if m := tomlModelRe.FindSubmatch(b); m != nil {
			c.Default = string(m[1])
		}
	}
	// codex keeps the catalog its last run fetched.
	if b, err := os.ReadFile(filepath.Join(home, ".codex", "models_cache.json")); err == nil {
		var cache struct {
			Models []struct {
				Slug       string `json:"slug"`
				Display    string `json:"display_name"`
				Visibility string `json:"visibility"`
			} `json:"models"`
		}
		if json.Unmarshal(b, &cache) == nil {
			for _, m := range cache.Models {
				if m.Visibility == "hide" || !modelRe.MatchString(m.Slug) {
					continue
				}
				c.Models = append(c.Models, modelInfo{ID: m.Slug, Provider: "OpenAI", Name: m.Display})
			}
		}
	}
	if len(c.Models) == 0 {
		c.Source = "none"
	}
	return c
}

func discoverOpencode(ctx context.Context) *modelCatalog {
	c := &modelCatalog{Kind: "opencode", Source: "opencode models"}
	out, err := runList(ctx, "opencode", "models")
	if err != nil {
		c.Source, c.Error = "none", "opencode models failed: "+err.Error()
		return c
	}
	for _, l := range strings.Fields(string(out)) {
		if prov, name, ok := strings.Cut(l, "/"); ok && modelRe.MatchString(l) {
			c.Models = append(c.Models, modelInfo{ID: l, Provider: prov, Name: name})
		}
	}
	sortModels(c.Models)
	return c
}
