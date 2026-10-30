package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

func TestModelArgs(t *testing.T) {
	pi := &modelCatalog{Kind: "pi", Models: []modelInfo{{ID: "homelab/fast"}, {ID: "openai-codex/gpt-5.5"}}}
	cases := []struct {
		kind, model, thinking string
		cat                   *modelCatalog
		want                  []string
		code                  string
	}{
		{"pi", "", "", pi, nil, ""},
		{"zzz", "", "", nil, nil, ""}, // defaults never need support
		{"pi", "homelab/fast", "", pi, []string{"--model", "homelab/fast"}, ""},
		{"pi", "homelab/fast", "high", pi, []string{"--model", "homelab/fast", "--thinking", "high"}, ""},
		{"pi", "", "low", pi, []string{"--thinking", "low"}, ""},
		{"pi", "homelab/nope", "", pi, nil, "unknown_model"},
		{"pi", "homelab/nope", "", nil, []string{"--model", "homelab/nope"}, ""}, // no catalog: syntax only
		{"pi", "homelab/fast", "ultra", pi, nil, "bad_thinking"},
		{"claude", "opus", "", nil, []string{"--model", "opus"}, ""},
		{"claude", "opus", "high", nil, nil, "thinking_unsupported"},
		{"codex", "gpt-5.5", "", nil, []string{"-m", "gpt-5.5"}, ""},
		{"gemini", "gemini-2.5-pro", "", nil, []string{"-m", "gemini-2.5-pro"}, ""},
		{"cursor", "x", "", nil, nil, "model_unsupported"},
		{"claude", "opus; rm -rf ~", "", nil, nil, "bad_model"},
		{"claude", "$(id)", "", nil, nil, "bad_model"},
		{"claude", "-x", "", nil, nil, "bad_model"},
		{"claude", "opus[1m]", "", nil, nil, "bad_model"},
	}
	for _, c := range cases {
		got, err := modelArgs(c.kind, c.model, c.thinking, c.cat)
		switch {
		case c.code != "" && (err == nil || err.code != c.code):
			t.Errorf("modelArgs(%q,%q,%q) = %v, %v; want error %s", c.kind, c.model, c.thinking, got, err, c.code)
		case c.code == "" && err != nil:
			t.Errorf("modelArgs(%q,%q,%q): %v", c.kind, c.model, c.thinking, err.message)
		case c.code == "" && !reflect.DeepEqual(got, c.want):
			t.Errorf("modelArgs(%q,%q,%q) = %q, want %q", c.kind, c.model, c.thinking, got, c.want)
		}
	}
}

func TestParsePiModels(t *testing.T) {
	out := []byte("📊 Langfuse: Tracing enabled → https://example\n" +
		"provider      model                context  max-out  thinking  images\n" +
		"homelab       fast                 188.4K   8.2K     no        yes   \n" +
		"openai-codex  gpt-5.4              272K     128K     yes       yes   \n\n")
	got := parsePiModels(out)
	want := []modelInfo{
		{ID: "homelab/fast", Provider: "homelab", Name: "fast"},
		{ID: "openai-codex/gpt-5.4", Provider: "openai-codex", Name: "gpt-5.4", Thinking: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePiModels = %+v", got)
	}
	if parsePiModels([]byte("Error: something\n")) != nil {
		t.Fatal("parsed a non-table")
	}
}

func TestPiConfigModelsAndDefaults(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "models.json"), []byte(`{"providers":{"homelab":{"models":[{"id":"fast"},{"id":"thinking","reasoning":true}]}}}`), 0o600)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"defaultProvider":"homelab","defaultModel":"thinking","defaultThinkingLevel":"minimal"}`), 0o600)
	t.Setenv("PATH", t.TempDir()) // no pi: the config file is the fallback
	c := discoverPi(context.Background(), home)
	if c.Exact || c.Source != "~/.pi/agent/models.json" || len(c.Models) != 2 || c.Models[1].ID != "homelab/thinking" || !c.Models[1].Thinking ||
		c.Default != "homelab/thinking" || c.DefaultThinking != "minimal" || len(c.Thinking) != len(thinkingLevels) {
		t.Fatalf("catalog = %+v", c)
	}
}

func TestClaudeAndCodexCatalogs(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"model":"opus"}`), 0o600)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"gpt-5.5\"\n[x]\nmodel = \"no\"\n"), 0o600)
	os.WriteFile(filepath.Join(home, ".codex", "models_cache.json"), []byte(`{"models":[{"slug":"gpt-5.5","display_name":"GPT 5.5"},{"slug":"hidden","visibility":"hide"}]}`), 0o600)
	if c := discoverClaude(home); c.Default != "opus" || len(c.Models) != len(claudeModels) || c.Source != "curated" {
		t.Fatalf("claude = %+v", c)
	}
	if c := discoverCodex(home); c.Default != "gpt-5.5" || len(c.Models) != 1 || c.Models[0].ID != "gpt-5.5" {
		t.Fatalf("codex = %+v", c)
	}
}

func TestModelsEndpoint(t *testing.T) {
	var runs atomic.Int32
	s := newTest("me@example.com")
	s.findExec = func(name string) bool { return name == "pi" || name == "claude" || name == "cursor-agent" }
	s.listModels = func(ctx context.Context, kind string) *modelCatalog {
		runs.Add(1)
		return &modelCatalog{Kind: kind, Source: "test", Models: []modelInfo{{ID: kind + "-model"}}}
	}
	type resp struct {
		Host     string         `json:"host"`
		Catalogs []modelCatalog `json:"catalogs"`
		Code     string         `json:"code"`
	}
	get := func(path string) (int, resp) {
		w := do(s, "GET", path, map[string]string{"Tailscale-User-Login": "me@example.com"}, "")
		var r resp
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r
	}
	code, r := get("/api/models?kind=pi")
	if code != 200 || len(r.Catalogs) != 1 || r.Catalogs[0].Kind != "pi" || r.Catalogs[0].Models[0].ID != "pi-model" {
		t.Fatalf("pi = %d %+v", code, r)
	}
	get("/api/models?kind=pi")
	if runs.Load() != 1 {
		t.Fatalf("listed %d times, want the second answer cached", runs.Load())
	}
	// No kind: every installed kind that takes a model (not cursor).
	code, r = get("/api/models")
	if code != 200 || len(r.Catalogs) != 2 || r.Catalogs[0].Kind != "pi" || r.Catalogs[1].Kind != "claude" {
		t.Fatalf("all = %d %+v", code, r)
	}
	if code, r = get("/api/models?kind=cursor"); code != 400 || r.Code != "model_unsupported" {
		t.Fatalf("cursor = %d %+v", code, r)
	}
	if code, r = get("/api/hosts/nowhere/models?kind=pi"); code != 404 {
		t.Fatalf("unknown host = %d", code)
	}
	if code, r = get("/api/hosts/" + s.fleet.LocalName() + "/models?kind=claude"); code != 200 || r.Catalogs[0].Kind != "claude" {
		t.Fatalf("local host = %d %+v", code, r)
	}
}

func TestModelsViaPeer(t *testing.T) {
	var path atomic.Value
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: agents\ndata: {\"type\":\"agents\",\"agents\":[]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		path.Store(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"host":"kube","catalogs":[{"kind":"pi","source":"test","models":[{"id":"kube/m"}]}]}`)
	}))
	defer peer.Close()
	pu, _ := url.Parse(peer.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{ReadLines: 100, Fleet: fl}, nil, nil)
	w := do(s, "GET", "/api/hosts/kube/models?kind=pi", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "kube/m") || path.Load() != "/api/models?kind=pi" {
		t.Fatalf("peer models = %d %s (peer saw %v)", w.Code, w.Body, path.Load())
	}
}

func startedArgs(t *testing.T, fh *fakeLaunchHerdr) []string {
	t.Helper()
	call := fh.call("agent.start")
	if call == nil {
		t.Fatal("no agent.start")
	}
	var out []string
	for _, a := range call.Params["args"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func TestLaunchWithModel(t *testing.T) {
	root, _ := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	s := newLaunchServer(t, client, nil, root)
	s.findExec = func(name string) bool { return name == "pi" || name == "claude" || name == "cursor-agent" }
	s.listModels = func(ctx context.Context, kind string) *modelCatalog {
		return &modelCatalog{Kind: kind, Exact: true, Models: []modelInfo{{ID: "homelab/fast"}}}
	}

	code, r := launch(t, s, `{"dir":"`+root+`/repo","kind":"pi","model":"homelab/fast","thinking":"high","prompt":"go"}`)
	if code != 200 {
		t.Fatalf("launch = %d %+v", code, r)
	}
	// The model arguments come before the first prompt's "--".
	if got := startedArgs(t, fh); !reflect.DeepEqual(got, []string{"--model", "homelab/fast", "--thinking", "high", "--", "go"}) {
		t.Fatalf("args = %q", got)
	}

	// Refusals happen before a workspace is created.
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	for body, want := range map[string]string{
		`{"dir":"` + root + `/repo","kind":"pi","model":"homelab/missing"}`:               "unknown_model",
		`{"dir":"` + root + `/repo","kind":"pi","model":"homelab/fast","thinking":"zzz"}`: "bad_thinking",
		`{"dir":"` + root + `/repo","kind":"claude","thinking":"high"}`:                   "thinking_unsupported",
		`{"dir":"` + root + `/repo","kind":"cursor","model":"x"}`:                         "model_unsupported",
		`{"dir":"` + root + `/repo","kind":"claude","model":"a b"}`:                       "bad_model",
	} {
		if code, r := launch(t, s, body); code != 400 || r.Code != want {
			t.Errorf("%s: %d %+v, want %s", body, code, r, want)
		}
	}
	if fh.call("workspace.create") != nil {
		t.Fatal("a refused model still created a workspace")
	}
}

// claude's --allowedTools is variadic: the model must come before it.
func TestLaunchCoordinatorModelBeforeAllowedTools(t *testing.T) {
	root, _ := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	s := newLaunchServer(t, client, nil, root)
	s.findExec = func(name string) bool { return name == "claude" }
	s.cfg.CoordinatorDir = root
	if code, r := launch(t, s, `{"coordinator":true,"kind":"claude","model":"opus"}`); code != 200 {
		t.Fatalf("launch = %d %+v", code, r)
	}
	got := startedArgs(t, fh)
	if len(got) < 3 || got[0] != "--model" || got[1] != "opus" || got[2] != "--allowedTools" {
		t.Fatalf("args = %q", got)
	}
}

func TestWorkerAndHandoffWithModel(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = worktreeHook(t, &wts, nil) })
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot, StateDir: t.TempDir()}, client, nil)
	s.findExec = func(name string) bool { return name == "pi" || name == "claude" }

	w := do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/m","kind":"claude","model":"sonnet","brief":"Do it."}`)
	var r struct {
		Entry ledger.Entry `json:"entry"`
	}
	json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != 200 || r.Entry.Model != "sonnet" || r.Entry.Thinking != "" {
		t.Fatalf("handoff = %d %s", w.Code, w.Body)
	}
	got := startedArgs(t, fh)
	if len(got) < 3 || got[0] != "--model" || got[1] != "sonnet" || got[2] != "--allowedTools" {
		t.Fatalf("worker args = %q", got)
	}
	if e, ok := s.ledger.Get(r.Entry.ID); !ok || e.Model != "sonnet" {
		t.Fatalf("ledger entry = %+v", e)
	}

	// A bad model is a failed handoff, recorded with what was asked for.
	w = do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/n","kind":"claude","model":"opus","thinking":"high","brief":"x"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "thinking_unsupported") || !strings.Contains(w.Body.String(), `"model":"opus"`) {
		t.Fatalf("bad handoff = %d %s", w.Code, w.Body)
	}

	// The ledger and inspect show it.
	w = do(s, "GET", "/api/ledger", nil, "")
	if !strings.Contains(w.Body.String(), `"model":"sonnet"`) {
		t.Fatalf("ledger = %s", w.Body)
	}
	w = do(s, "GET", "/api/inspect?agent="+url.QueryEscape(r.Entry.AgentID), nil, "")
	if !strings.Contains(w.Body.String(), `"model":"sonnet"`) {
		t.Fatalf("inspect = %d %s", w.Code, w.Body)
	}
}
