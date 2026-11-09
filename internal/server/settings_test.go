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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// settingsRig is a launch server with a state directory (so it keeps
// settings.json) and the agent role enabled.
type settingsRig struct {
	s    *Server
	fh   *fakeLaunchHerdr
	root string
}

func newSettingsRig(t *testing.T, mod func(*Config)) *settingsRig {
	t.Helper()
	root, _ := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	cfg := Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, StateDir: t.TempDir(), AgentToken: testToken}
	if mod != nil {
		mod(&cfg)
	}
	s := New(cfg, client, nil)
	// pi, codex and cursor are installed; claude is not.
	s.findExec = func(name string) bool { return name == "pi" || name == "codex" || name == "cursor-agent" }
	return &settingsRig{s: s, fh: fh, root: root}
}

var setUser = map[string]string{"Tailscale-User-Login": "me@example.com"}

func (r *settingsRig) put(t *testing.T, body string) (int, string) {
	t.Helper()
	w := do(r.s, "PUT", "/api/settings", setUser, body)
	return w.Code, w.Body.String()
}

func (r *settingsRig) launch(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	w := do(r.s, "POST", "/api/launch", setUser, strings.Replace(body, "ROOT", r.root, 1))
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return w.Code, m
}

// startArgs are the arguments of the last agent.start herdr saw.
func (r *settingsRig) startArgs() string {
	var last map[string]any
	r.fh.set(func(f *fakeLaunchHerdr) {
		for _, c := range f.calls {
			if c.Method == "agent.start" {
				last = c.Params
			}
		}
	})
	var out []string
	if last != nil {
		out = append(out, last["kind"].(string))
		for _, a := range last["args"].([]any) {
			out = append(out, a.(string))
		}
	}
	return strings.Join(out, " ")
}

func warnings(m map[string]any) string {
	var out []string
	if ws, ok := m["warnings"].([]any); ok {
		for _, w := range ws {
			out = append(out, w.(string))
		}
	}
	return strings.Join(out, "; ")
}

func TestSettingsPrecedence(t *testing.T) {
	// Nothing set: the old guess (the first installed kind), source auto.
	r := newSettingsRig(t, nil)
	code, m := r.launch(t, `{"dir":"ROOT/repo"}`)
	if code != 200 || m["agent"].(map[string]any)["agent"] != "pi" || m["kind_source"] != "auto" {
		t.Fatalf("no settings: %d %v", code, m)
	}

	// The -default-kind seed beats the guess.
	r = newSettingsRig(t, func(c *Config) { c.DefaultKind = "codex" })
	code, m = r.launch(t, `{"dir":"ROOT/repo"}`)
	if code != 200 || m["agent"].(map[string]any)["agent"] != "codex" || m["kind_source"] != "flag" {
		t.Fatalf("flag seed: %d %v", code, m)
	}

	// The file beats the seed.
	if code, body := r.put(t, `{"default_kind":"pi"}`); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	code, m = r.launch(t, `{"dir":"ROOT/repo","name":"two"}`)
	if code != 200 || m["agent"].(map[string]any)["agent"] != "pi" || m["kind_source"] != "setting" || warnings(m) != "" {
		t.Fatalf("file over flag: %d %v", code, m)
	}

	// An explicit kind beats the file: it starts, with a warning.
	code, m = r.launch(t, `{"dir":"ROOT/repo","name":"three","kind":"codex"}`)
	if code != 200 || m["agent"].(map[string]any)["agent"] != "codex" || m["kind_source"] != "request" ||
		!strings.Contains(warnings(m), "codex was requested; this host's default is pi") {
		t.Fatalf("request over file: %d %v", code, m)
	}
	// Naming the default itself is not a warning.
	if _, m = r.launch(t, `{"dir":"ROOT/repo","name":"four","kind":"pi"}`); warnings(m) != "" || m["kind_source"] != "request" {
		t.Fatalf("request = default: %v", m)
	}
}

func TestSettingsModelDefaults(t *testing.T) {
	r := newSettingsRig(t, nil)
	if code, body := r.put(t, `{"default_kind":"codex","kinds":{"codex":{"model":"gpt-5"},"pi":{"thinking":"high"}}}`); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	// The default kind's model is used when the request gives none...
	code, m := r.launch(t, `{"dir":"ROOT/repo"}`)
	if code != 200 || m["model"] != "gpt-5" || r.startArgs() != "codex -m gpt-5" {
		t.Fatalf("default model: %d %v args %q", code, m, r.startArgs())
	}
	// ...and a request's model wins.
	code, m = r.launch(t, `{"dir":"ROOT/repo","name":"b","model":"o3"}`)
	if code != 200 || m["model"] != "o3" || r.startArgs() != "codex -m o3" {
		t.Fatalf("request model: %d %v args %q", code, m, r.startArgs())
	}
	// Another kind uses its own entry, not the default kind's: pi gets its
	// thinking level and no model.
	code, m = r.launch(t, `{"dir":"ROOT/repo","name":"c","kind":"pi"}`)
	if code != 200 || m["thinking"] != "high" || m["model"] != nil || r.startArgs() != "pi --thinking high" {
		t.Fatalf("pi entry: %d %v args %q", code, m, r.startArgs())
	}
	// A kind with no entry starts with the agent's own defaults.
	code, m = r.launch(t, `{"dir":"ROOT/repo","name":"d","kind":"cursor"}`)
	if code != 200 || m["model"] != nil || r.startArgs() != "cursor" {
		t.Fatalf("no entry: %d %v args %q", code, m, r.startArgs())
	}
	// The launch options carry what the app preselects.
	var o map[string]any
	decode(t, do(r.s, "GET", "/api/launch/options", setUser, ""), &o)
	if o["default_kind"] != "codex" || o["default_source"] != "setting" || o["default_model"] != "gpt-5" {
		t.Fatalf("options: %v", o)
	}
}

func TestSettingsStaleModelNamesItsSource(t *testing.T) {
	r := newSettingsRig(t, nil)
	// A value that no longer passes the check (a hand-edited file) fails the
	// start and says where it came from.
	st := hostSettings{Kinds: map[string]kindSettings{"pi": {Thinking: "loud"}}}
	if _, err := r.s.settings.put(st, nil, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	code, m := r.launch(t, `{"dir":"ROOT/repo","kind":"pi"}`)
	msg, _ := m["message"].(string)
	if code != 400 || m["code"] != "bad_thinking" || !strings.Contains(msg, "comes from this host's settings") {
		t.Fatalf("stale: %d %v", code, m)
	}
	// The same value in the request has no such remark.
	_, m = r.launch(t, `{"dir":"ROOT/repo","kind":"pi","thinking":"loud"}`)
	if msg, _ = m["message"].(string); strings.Contains(msg, "settings") {
		t.Fatalf("request value blamed on settings: %v", m)
	}
}

func TestSettingsAllowedKinds(t *testing.T) {
	r := newSettingsRig(t, func(c *Config) { c.AllowedKinds = []string{"pi"} })
	// The flag's allowed list limits the kinds offered and started.
	var o optionsBody
	decode(t, do(r.s, "GET", "/api/launch/options", setUser, ""), &o)
	if strings.Join(o.Kinds, ",") != "pi" || o.DefaultKind != "pi" {
		t.Fatalf("options: %+v", o)
	}
	code, m := r.launch(t, `{"dir":"ROOT/repo","kind":"codex"}`)
	if code != 400 || m["code"] != "kind_not_allowed" || !strings.Contains(m["message"].(string), "allowed: pi") {
		t.Fatalf("refused: %d %v", code, m)
	}
	if len(r.fh.methods()) != 0 {
		t.Fatalf("herdr was called for a refused kind: %v", r.fh.methods())
	}
	// A kind not installed is still bad_kind, not allowed-but-missing.
	if code, m = r.launch(t, `{"dir":"ROOT/repo","kind":"pi"}`); code != 200 {
		t.Fatalf("allowed kind: %d %v", code, m)
	}
	// The file replaces the flag's list; the default must be among them.
	if code, body := r.put(t, `{"allowed_kinds":["codex","cursor"],"default_kind":"pi"}`); code != 400 || !strings.Contains(body, "not among allowed_kinds") {
		t.Fatalf("default outside allowed: %d %s", code, body)
	}
	if code, body := r.put(t, `{"allowed_kinds":["codex","cursor"],"default_kind":"cursor"}`); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	code, m = r.launch(t, `{"dir":"ROOT/repo","name":"x"}`)
	if code != 200 || m["agent"].(map[string]any)["agent"] != "cursor" {
		t.Fatalf("after file: %d %v", code, m)
	}
	if code, m = r.launch(t, `{"dir":"ROOT/repo","name":"y","kind":"pi"}`); code != 400 || m["code"] != "kind_not_allowed" {
		t.Fatalf("pi after file: %d %v", code, m)
	}
	// A refusal holds for the agent role too.
	w := do(r.s, "POST", "/api/worker", map[string]string{HeaderAgentToken: testToken}, `{}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("agent role starting a worker: %d", w.Code)
	}
	// ...and for the worker path, which the handoff uses.
	if _, lerr := r.s.chooseAgent(context.Background(), "pi", "", "", nil); lerr == nil || lerr.code != "kind_not_allowed" {
		t.Fatalf("chooseAgent: %v", lerr)
	}
}

func TestSettingsUnavailableDefaultFallsBack(t *testing.T) {
	r := newSettingsRig(t, func(c *Config) { c.DefaultKind = "claude" }) // claude is not installed
	code, m := r.launch(t, `{"dir":"ROOT/repo"}`)
	if code != 200 || m["agent"].(map[string]any)["agent"] != "pi" || m["kind_source"] != "auto" ||
		!strings.Contains(warnings(m), "default agent kind claude is not available") {
		t.Fatalf("fallback: %d %v", code, m)
	}
	var v settingsView
	decode(t, do(r.s, "GET", "/api/settings", setUser, ""), &v)
	if !strings.Contains(v.Effective.Notice, "claude is not available") || v.Effective.DefaultSource != "flag" {
		t.Fatalf("view: %+v", v.Effective)
	}
}

// chooseAgent is what the worker path (handoff) calls: it follows the same
// rules without the launch dialog's request.
func TestChooseAgentForWorkers(t *testing.T) {
	r := newSettingsRig(t, nil)
	if code, body := r.put(t, `{"default_kind":"codex","kinds":{"codex":{"model":"gpt-5"}}}`); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	c, lerr := r.s.chooseAgent(context.Background(), "", "", "", nil)
	if lerr != nil || c.Kind != "codex" || c.KindSource != "setting" || strings.Join(c.Args, " ") != "-m gpt-5" || c.Model != "gpt-5" {
		t.Fatalf("worker default: %+v %v", c, lerr)
	}
	c, lerr = r.s.chooseAgent(context.Background(), "pi", "", "", nil)
	if lerr != nil || c.Kind != "pi" || c.KindSource != "request" || !strings.Contains(strings.Join(c.Warnings, ";"), "pi was requested") {
		t.Fatalf("worker override: %+v %v", c, lerr)
	}
}

func TestSettingsPutValidation(t *testing.T) {
	r := newSettingsRig(t, nil)
	for body, want := range map[string]string{
		`{"default_kind":"emacs"}`:                        "unknown agent kind",
		`{"allowed_kinds":["pi","vim"]}`:                  "unknown agent kind",
		`{"kinds":{"emacs":{"model":"x"}}}`:               "unknown agent kind",
		`{"kinds":{"pi":{"model":"bad model!"}}}`:         "not a model name",
		`{"kinds":{"claude":{"thinking":"high"}}}`:        "thinking level can only be chosen for pi",
		`{"kinds":{"pi":{"thinking":"loud"}}}`:            "is not one of",
		`{"kinds":{"cursor":{"model":"x"}}}`:              "not supported",
		`{"default_kind":"pi","allowed_kinds":["codex"]}`: "not among allowed_kinds",
		`not json`: "invalid body",
	} {
		if code, got := r.put(t, body); code != 400 || !strings.Contains(got, want) {
			t.Errorf("%s: %d %s (want %q)", body, code, got, want)
		}
	}
	// Valid: kinds and codes are cleaned up, an empty entry is dropped.
	code, got := r.put(t, `{"default_kind":" PI ","allowed_kinds":["pi","PI","codex"],"kinds":{"pi":{"model":" anthropic/sonnet ","thinking":"high"},"codex":{}}}`)
	var v settingsView
	json.Unmarshal([]byte(got), &v)
	if code != 200 || v.Settings.DefaultKind != "pi" || strings.Join(v.Settings.AllowedKinds, ",") != "pi,codex" ||
		len(v.Settings.Kinds) != 1 || v.Settings.Kinds["pi"].Model != "anthropic/sonnet" || v.Settings.Rev != 1 || v.Settings.UpdatedBy != "me@example.com" {
		t.Fatalf("put: %d %s", code, got)
	}
	// The revision guards against a lost update.
	if code, got = r.put(t, `{"default_kind":"codex","rev":0}`); code != http.StatusConflict || !strings.Contains(got, "rev_conflict") {
		t.Fatalf("stale rev: %d %s", code, got)
	}
	if code, got = r.put(t, `{"default_kind":"codex","rev":1}`); code != 200 || !strings.Contains(got, `"rev":2`) {
		t.Fatalf("current rev: %d %s", code, got)
	}
	// An empty document clears everything.
	if code, got = r.put(t, `{}`); code != 200 {
		t.Fatalf("clear: %d %s", code, got)
	}
	var after settingsView
	decode(t, do(r.s, "GET", "/api/settings", setUser, ""), &after)
	if after.Settings.DefaultKind != "" || after.Effective.DefaultSource != "auto" {
		t.Fatalf("after clear: %+v", after)
	}
}

func TestSettingsPersistAndSurviveACorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := settingsPath(dir)
	st := openSettings(path)
	if _, err := st.put(hostSettings{DefaultKind: "pi", Kinds: map[string]kindSettings{"pi": {Model: "m"}}}, nil, "me", time.Now()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	if got := openSettings(path).get(); got.DefaultKind != "pi" || got.Kinds["pi"].Model != "m" || got.Rev != 1 {
		t.Fatalf("reopened: %+v", got)
	}
	// A hand-edit that is not JSON leaves the host running with no settings.
	os.WriteFile(path, []byte("{oops"), 0o600)
	if got := openSettings(path).get(); got.DefaultKind != "" || got.Rev != 0 {
		t.Fatalf("corrupt: %+v", got)
	}
	// A failed write changes nothing.
	bad := openSettings(filepath.Join(dir, "nope", "x", "settings.json"))
	os.WriteFile(filepath.Join(dir, "nope"), []byte("a file, not a directory"), 0o600)
	if _, err := bad.put(hostSettings{DefaultKind: "pi"}, nil, "me", time.Now()); err == nil || bad.get().DefaultKind != "" {
		t.Fatalf("failed write: %v %+v", err, bad.get())
	}
}

// The auth matrix on one host: the user reads and writes; the agent role
// (a coordinator) reads and cannot write; nobody without an identity gets in.
func TestSettingsAuthMatrix(t *testing.T) {
	root, _ := launchTree(t)
	s := New(Config{AllowLogins: []string{"me@example.com"}, AllowHosts: testHosts, ReadLines: 100, HostName: "desk",
		LaunchRoots: []string{root}, StateDir: t.TempDir(), AgentToken: testToken}, nil, nil)
	h := s.Handler()
	send := func(method, path string, hdr map[string]string, body string) int {
		r, _ := http.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	me := map[string]string{"Tailscale-User-Login": "me@example.com"}
	agent := map[string]string{HeaderAgentToken: testToken}
	for _, c := range []struct {
		name, method, path string
		hdr                map[string]string
		want               int
	}{
		{"user reads", "GET", "/api/settings", me, 200},
		{"user reads by host", "GET", "/api/hosts/desk/settings", me, 200},
		{"user writes", "PUT", "/api/settings", me, 200},
		{"user writes by host", "PUT", "/api/hosts/desk/settings", me, 200},
		{"coordinator reads", "GET", "/api/settings", agent, 200},
		{"coordinator reads by host", "GET", "/api/hosts/desk/settings", agent, 200},
		{"coordinator cannot write", "PUT", "/api/settings", agent, 403},
		{"coordinator cannot write by host", "PUT", "/api/hosts/desk/settings", agent, 403},
		{"stranger cannot read", "GET", "/api/settings", nil, 401},
		{"stranger cannot write", "PUT", "/api/settings", nil, 401},
		{"cross-site write", "PUT", "/api/settings", map[string]string{"Tailscale-User-Login": "me@example.com", "Sec-Fetch-Site": "cross-site"}, 403},
		{"unknown host", "GET", "/api/hosts/nowhere/settings", me, 404},
	} {
		if got := send(c.method, c.path, c.hdr, `{}`); got != c.want {
			t.Errorf("%s: %s %s = %d, want %d", c.name, c.method, c.path, got, c.want)
		}
	}
}

// A tagged node reads only its own host's settings, and writes nothing.
func TestSettingsTaggedNodeReadsOnlyItsOwnHost(t *testing.T) {
	r := newCapRig(t, false)
	r.addPeer("w1", "https://w1.t.ts.net", "tag:worker")
	r.addPeer("w2", "https://w2.t.ts.net", "tag:worker")
	r.who.setHolder("w1.t.ts.net", "tag:worker")
	for _, p := range []string{"/api/settings", "/api/hosts/w2/settings", "/api/hosts/desk/settings"} {
		if code, body := r.serve("GET", p, "", nil); code != http.StatusForbidden || !strings.Contains(body, "read_forbidden") {
			t.Errorf("GET %s: %d %s", p, code, body)
		}
	}
	// Its own host's is let through to the host route (the stand-in peer
	// has no such route, so the hub passes its 404 along: not a refusal).
	if code, body := r.serve("GET", "/api/hosts/w1/settings", "", nil); code == http.StatusForbidden {
		t.Errorf("own host: %d %s", code, body)
	}
	for _, p := range []string{"/api/settings", "/api/hosts/w1/settings"} {
		if code, body := r.serve("PUT", p, `{}`, nil); code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("PUT %s: %d %s", p, code, body)
		}
	}
}

// settingsPeer stands in for a peer that has host settings.
type settingsPeer struct {
	mu   sync.Mutex
	hits []string
	body string
	hdr  http.Header
}

func (p *settingsPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/events":
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: agents\ndata: {\"type\":\"agents\",\"agents\":[]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case "/api/settings":
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.hits = append(p.hits, r.Method)
		p.body, p.hdr = string(b), r.Header.Clone()
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"host":"kube","settings":{"default_kind":"pi","rev":3},"effective":{"default_kind":"pi","default_source":"setting"}}`)
	default:
		http.NotFound(w, r)
	}
}

func TestSettingsThroughTheHub(t *testing.T) {
	peer := &settingsPeer{}
	ps := httptest.NewServer(peer)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fl.Run(ctx)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken, StateDir: t.TempDir()}, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req := func(method, path, body string, hdr map[string]string) (int, string) {
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	me := map[string]string{"Tailscale-User-Login": "me@example.com"}
	agent := map[string]string{HeaderAgentToken: testToken}

	// The user's read and write are forwarded to the peer, body intact.
	if code, body := req("GET", "/api/hosts/kube/settings", "", me); code != 200 || !strings.Contains(body, `"rev":3`) {
		t.Fatalf("user GET: %d %s", code, body)
	}
	put := `{"default_kind":"pi","allowed_kinds":["pi"],"rev":3}`
	if code, body := req("PUT", "/api/hosts/kube/settings", put, me); code != 200 || !strings.Contains(body, `"default_source":"setting"`) {
		t.Fatalf("user PUT: %d %s", code, body)
	}
	peer.mu.Lock()
	got, methods := peer.body, strings.Join(peer.hits, ",")
	peer.mu.Unlock()
	if got != put || methods != "GET,PUT" {
		t.Fatalf("the peer saw %q via %s", got, methods)
	}
	// A coordinator reads any host's; it cannot have a write forwarded.
	if code, _ := req("GET", "/api/hosts/kube/settings", "", agent); code != 200 {
		t.Errorf("coordinator GET: %d", code)
	}
	if code, body := req("PUT", "/api/hosts/kube/settings", put, agent); code != http.StatusForbidden {
		t.Errorf("coordinator PUT: %d %s", code, body)
	}
	peer.mu.Lock()
	n := len(peer.hits)
	peer.mu.Unlock()
	if n != 3 {
		t.Errorf("the peer saw %d calls, want 3 (the refused write must not reach it)", n)
	}
	// An unknown host is a 404 for both.
	for _, m := range []string{"GET", "PUT"} {
		if code, body := req(m, "/api/hosts/nowhere/settings", `{}`, me); code != 404 || !strings.Contains(body, "unknown_host") {
			t.Errorf("%s unknown host: %d %s", m, code, body)
		}
	}
	// The hub's own settings are its own.
	if code, body := req("GET", "/api/settings", "", me); code != 200 || strings.Contains(body, `"rev":3`) {
		t.Errorf("hub GET: %d %s", code, body)
	}
}

// A coordinator on a peer with -hub asks its own host's settings through
// the bare route: the request must reach the hub as that host's, never as
// the hub's own.
func TestSettingsBareRouteForwardsAsOwnHost(t *testing.T) {
	var mu sync.Mutex
	var path string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"host":"laptop"}`)
	}))
	defer hub.Close()
	up, err := fleet.NewUpstream(hub.URL, true, hub.Client())
	if err != nil {
		t.Skipf("no upstream: %v", err)
	}
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "laptop", AgentToken: testToken, Upstream: up}, nil, nil)
	w := do(s, "GET", "/api/settings", map[string]string{HeaderAgentToken: testToken}, "")
	mu.Lock()
	defer mu.Unlock()
	if w.Code != 200 || path != "/api/hosts/laptop/settings" {
		t.Fatalf("forwarded %d to %q", w.Code, path)
	}
}

// A handoff with no kind starts the host's default with the host's model,
// and the ledger records where the kind came from; a handoff that names a
// kind other than the default says so.
func TestHandoffFollowsHostSettings(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = worktreeHook(t, &wts, nil) })
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot, StateDir: t.TempDir()}, client, nil)
	s.findExec = func(name string) bool { return name == "pi" || name == "codex" }
	if _, err := s.settings.put(hostSettings{DefaultKind: "codex", Kinds: map[string]kindSettings{"codex": {Model: "gpt-5"}}}, nil, "me", time.Now()); err != nil {
		t.Fatal(err)
	}
	type reply struct {
		Entry  ledger.Entry `json:"entry"`
		Worker workerResult `json:"worker"`
	}
	handoff := func(branch, kind string) (int, reply) {
		body := `{"repo":"` + repo + `","branch":"` + branch + `","brief":"B"`
		if kind != "" {
			body += `,"kind":"` + kind + `"`
		}
		w := do(s, "POST", "/api/handoffs", nil, body+"}")
		var r reply
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r
	}
	code, r := handoff("feat/a", "")
	if code != 200 || r.Entry.Kind != "codex" || r.Entry.KindSource != "setting" || r.Entry.Model != "gpt-5" || len(r.Worker.Warnings) != 0 {
		t.Fatalf("default: %d %+v", code, r)
	}
	code, r = handoff("feat/b", "pi")
	if code != 200 || r.Entry.Kind != "pi" || r.Entry.KindSource != "request" || r.Entry.Model != "" ||
		len(r.Worker.Warnings) != 1 || !strings.Contains(r.Worker.Warnings[0], "pi was requested; this host's default is codex") {
		t.Fatalf("override: %d %+v", code, r)
	}
	// Outside allowed_kinds the handoff is refused and recorded as failed.
	if _, err := s.settings.put(hostSettings{AllowedKinds: []string{"codex"}}, nil, "me", time.Now()); err != nil {
		t.Fatal(err)
	}
	w := do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/c","kind":"pi","brief":"C"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kind_not_allowed") || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}
}
