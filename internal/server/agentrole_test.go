package server

import (
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
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/state"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// agentHdr is a CLI request's headers from pane (no Tailscale login).
func agentHdr(pane string) map[string]string {
	h := map[string]string{HeaderAgentToken: testToken}
	if pane != "" {
		h[HeaderAgent] = pane
	}
	return h
}

var userHdr = map[string]string{"Tailscale-User-Login": "me@example.com"}

// managerWith runs a state manager over the fake herdr named host and
// waits until it lists n agents.
func managerWith(t *testing.T, client *herdr.Client, host string, n int) *state.Manager {
	t.Helper()
	m := state.New(client, nil)
	m.SetHost(host)
	waitAgents(t, m, n)
	return m
}

func waitAgents(t *testing.T, m *state.Manager, n int) {
	t.Helper()
	m.Refresh()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Agents()) != n {
		if time.Now().After(deadline) {
			t.Fatalf("manager has %d agents, want %d", len(m.Agents()), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// coordServer is a hub (single instance) with a fake herdr running a
// coordinator (w1:p1) and a home agent (w2:p1), an allowlist, the agent
// token and a state directory.
func coordServer(t *testing.T) (*Server, *fakeLaunchHerdr, string) {
	t.Helper()
	fh, client := startFakeLaunchHerdr(t)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{
			{Name: "coordinator", PaneID: "w1:p1", Agent: "claude", Status: herdr.StatusIdle},
			{Name: "home", PaneID: "w2:p1", Agent: "pi", Status: herdr.StatusIdle},
		}
	})
	m := managerWith(t, client, "desk", 2)
	dir := t.TempDir()
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: dir, AgentToken: testToken, LaunchRoots: []string{t.TempDir()}}, client, m)
	s.findExec = func(name string) bool { return name == "pi" || name == "claude" }
	return s, fh, dir
}

func TestAgentRoleAllowlist(t *testing.T) {
	s, _, _ := coordServer(t)
	forbidden := []struct{ method, path, body string }{
		{"POST", "/api/agents/w2:p1/keys", `{"keys":["esc"]}`},
		{"GET", "/api/agents/w2:p1/dialog", ""},
		{"POST", "/api/agents/w2:p1/answer", `{}`},
		{"POST", "/api/agents/w2:p1/image", `{}`},
		{"POST", "/api/hosts/desk/agents/w2:p1/keys", `{"keys":["esc"]}`},
		{"GET", "/api/hosts/desk/agents/w2:p1/dialog", ""},
		{"POST", "/api/hosts/desk/agents/w2:p1/answer", `{}`},
		{"POST", "/api/hosts/desk/agents/w2:p1/image", `{}`},
		{"GET", "/api/launch/options", ""},
		{"GET", "/api/launch/dirs", ""},
		{"POST", "/api/launch", `{"coordinator":true}`},
		{"GET", "/api/hosts/desk/launch/options", ""},
		{"GET", "/api/hosts/desk/launch/dirs", ""},
		{"POST", "/api/hosts/desk/launch", `{}`},
		{"GET", "/api/repos", ""},
		{"GET", "/api/checks?path=/x", ""},
		{"POST", "/api/worker", `{}`},
		{"POST", "/api/remove", `{}`},
		{"GET", "/api/hosts/desk/repos", ""},
		{"GET", "/api/hosts/desk/checks", ""},
		{"GET", "/api/removals", ""},
		{"POST", "/api/removals/confirm", `{"ids":["desk/w2:p1"]}`},
		{"POST", "/api/roles", `{"agent_id":"desk/w2:p1","role":"coordinator"}`},
		{"GET", "/api/events", ""},
		{"GET", "/api/push/key", ""},
		{"POST", "/api/push/subscribe", `{}`},
		{"POST", "/api/push/unsubscribe", `{}`},
		{"POST", "/api/push/test", `{}`},
		{"POST", "/api/push/presence", `{}`},
	}
	for _, c := range forbidden {
		w := do(s, c.method, c.path, agentHdr("w1:p1"), c.body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "agent_forbidden") {
			t.Errorf("agent %s %s = %d %s, want 403", c.method, c.path, w.Code, w.Body)
		}
	}
	allowed := []struct {
		method, path, body string
		code               int
	}{
		{"GET", "/api/version", "", 200},
		{"GET", "/api/whoami", "", 200},
		{"GET", "/api/playbook?harness=pi", "", 200},
		{"GET", "/api/agents", "", 200},
		{"GET", "/api/hosts", "", 200},
		{"GET", "/api/agents/w2:p1/transcript", "", 404}, // no session file: but allowed
		{"GET", "/api/hosts/desk/agents/w2:p1/transcript", "", 404},
		{"GET", "/api/hosts/desk/agents/w2:p1/subagents/abc/transcript", "", 404},
		{"POST", "/api/agents/home/prompt", `{"text":"hi"}`, 200},
		{"POST", "/api/hosts/desk/agents/w2:p1/prompt", `{"text":"hi"}`, 200},
		{"GET", "/api/ledger", "", 200},
		{"POST", "/api/handoffs", `{"repo":"/x","branch":"b","brief":""}`, 400},
		{"POST", "/api/marks/clear", `{"agent_id":"desk/w2:p1"}`, 200},
		{"GET", "/api/brief", "", 200},
		{"POST", "/api/brief", `{"markdown":"# hi"}`, 200},
		{"GET", "/api/roles", "", 200},
	}
	for _, c := range allowed {
		if w := do(s, c.method, c.path, agentHdr("w1:p1"), c.body); w.Code != c.code {
			t.Errorf("agent %s %s = %d %s, want %d", c.method, c.path, w.Code, w.Body, c.code)
		}
	}
	// A wrong token is refused, not downgraded to the user (even with a
	// login header); no login and no token is refused too.
	bad := map[string]string{HeaderAgentToken: strings.Repeat("f", 64), "Tailscale-User-Login": "me@example.com"}
	if w := do(s, "GET", "/api/agents", bad, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", w.Code)
	}
	if w := do(s, "GET", "/api/agents", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", w.Code)
	}
	// Without a configured token, the agent role does not exist.
	s2 := New(Config{AllowLogins: []string{"me@example.com"}, HostName: "desk"}, nil, nil)
	if w := do(s2, "GET", "/api/agents", agentHdr(""), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token configured: %d", w.Code)
	}
	// The user keeps every route.
	if w := do(s, "GET", "/api/removals", userHdr, ""); w.Code != 200 {
		t.Errorf("user removals: %d %s", w.Code, w.Body)
	}
}

func TestAgentRoleThroughHandler(t *testing.T) {
	// The CLI's requests (loopback Host, no Origin, JSON) pass the Host
	// check and CSRF protection.
	s, _, _ := coordServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/brief", strings.NewReader(`{"markdown":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderAgentToken, testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("brief through the handler: %d", resp.StatusCode)
	}
}

func TestAgentAttributionAndGuards(t *testing.T) {
	s, fh, _ := coordServer(t)
	w := do(s, "GET", "/api/whoami", agentHdr("w1:p1"), "")
	var who map[string]any
	json.Unmarshal(w.Body.Bytes(), &who)
	if who["role"] != "agent" || who["agent"] != "desk/w1:p1" || who["by"] != "coordinator (desk/w1:p1)" || who["hub"] != "self" || who["host"] != "desk" {
		t.Fatalf("whoami = %s", w.Body)
	}
	if w := do(s, "GET", "/api/whoami", userHdr, ""); !strings.Contains(w.Body.String(), `"role":"user"`) {
		t.Fatalf("user whoami = %s", w.Body)
	}

	// An agent cannot prompt itself, by pane or by name, on either route.
	for _, p := range []string{"/api/agents/w1:p1/prompt", "/api/agents/coordinator/prompt", "/api/hosts/desk/agents/w1:p1/prompt"} {
		if w := do(s, "POST", p, agentHdr("w1:p1"), `{"text":"hi"}`); w.Code != http.StatusConflict {
			t.Errorf("self prompt %s: %d", p, w.Code)
		}
	}
	if fh.call("agent.prompt") != nil {
		t.Fatal("a self prompt reached herdr")
	}

	// Marks: a reason is required; not itself; not a coordinator.
	if w := do(s, "POST", "/api/marks", agentHdr("w1:p1"), `{"agent_id":"desk/w2:p1"}`); w.Code != http.StatusBadRequest {
		t.Errorf("mark without reason: %d", w.Code)
	}
	if w := do(s, "POST", "/api/marks", agentHdr("w1:p1"), `{"agent_id":"desk/w1:p1","reason":"x"}`); w.Code != http.StatusConflict {
		t.Errorf("mark self: %d", w.Code)
	}
	if w := do(s, "POST", "/api/roles", userHdr, `{"agent_id":"desk/w1:p1","role":"coordinator"}`); w.Code != 200 {
		t.Fatalf("set role: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/marks", agentHdr("w2:p1"), `{"agent_id":"desk/w1:p1","reason":"x"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"coordinator"`) {
		t.Errorf("agent marks coordinator: %d %s", w.Code, w.Body)
	}
	w = do(s, "POST", "/api/marks", agentHdr("w1:p1"), `{"agent_id":"desk/w2:p1","reason":"done","by":"the user"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"by":"coordinator (desk/w1:p1)"`) {
		t.Fatalf("mark = %d %s", w.Code, w.Body)
	}
	// The user may mark a coordinator (and is asked again on removal).
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w1:p1"}`); w.Code != 200 {
		t.Fatalf("user marks coordinator: %d %s", w.Code, w.Body)
	}
	w = do(s, "GET", "/api/removals", userHdr, "")
	var rv struct {
		Removals []removal `json:"removals"`
	}
	json.Unmarshal(w.Body.Bytes(), &rv)
	var coordItem *removal
	for i := range rv.Removals {
		if rv.Removals[i].ID == "desk/w1:p1" {
			coordItem = &rv.Removals[i]
		}
	}
	if coordItem == nil || !coordItem.Coordinator || coordItem.Safe || !strings.Contains(strings.Join(coordItem.Warnings, " "), "coordinator") {
		t.Fatalf("coordinator removal item = %+v", coordItem)
	}
	w = do(s, "POST", "/api/removals/confirm", userHdr, `{"ids":["desk/w1:p1"]}`)
	if !strings.Contains(w.Body.String(), `"code":"coordinator"`) {
		t.Fatalf("unconfirmed coordinator removal = %s", w.Body)
	}
	if fh.call("pane.close") != nil || fh.call("workspace.close") != nil {
		t.Fatal("closed a coordinator without the extra confirmation")
	}
	// Confirmed explicitly, it goes on to the removal itself (which this
	// fake herdr cannot do: it only has to get past the guard).
	w = do(s, "POST", "/api/removals/confirm", userHdr, `{"ids":["desk/w1:p1"],"coordinators":["desk/w1:p1"]}`)
	if strings.Contains(w.Body.String(), `"code":"coordinator"`) || fh.call("workspace.list") == nil {
		t.Fatalf("confirmed coordinator removal = %s", w.Body)
	}
}

func TestRolesAPI(t *testing.T) {
	s, fh, dir := coordServer(t)
	if w := do(s, "POST", "/api/roles", userHdr, `{"agent_id":"desk/nope","role":"coordinator"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown agent: %d", w.Code)
	}
	if w := do(s, "POST", "/api/roles", userHdr, `{"agent_id":"desk/w2:p1","role":"boss"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad role: %d", w.Code)
	}
	w := do(s, "POST", "/api/roles", userHdr, `{"agent_id":"desk/w2:p1","role":"coordinator"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"desk/w2:p1":{"role":"coordinator","name":"home"`) {
		t.Fatalf("set = %d %s", w.Code, w.Body)
	}
	if fi, err := os.Stat(filepath.Join(dir, "roles.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("roles.json: %v %v", fi, err)
	}
	// The ledger carries the roles for the app.
	if w := do(s, "GET", "/api/ledger", userHdr, ""); !strings.Contains(w.Body.String(), `"roles":{"desk/w2:p1"`) {
		t.Fatalf("ledger roles = %s", w.Body)
	}
	// herdr reuses pane ids: another agent in that pane is not a
	// coordinator.
	fh.set(func(f *fakeLaunchHerdr) { f.agents[1].Name = "other" })
	s.manager.Refresh()
	waitFor(t, func() bool { return s.fleet.Agents()[1].Name == "other" })
	if w := do(s, "GET", "/api/roles", agentHdr("w1:p1"), ""); strings.Contains(w.Body.String(), "w2:p1") {
		t.Fatalf("stale role listed: %s", w.Body)
	}
	// Reopened from disk; cleared by the user.
	s2 := New(Config{HostName: "desk", StateDir: dir}, nil, nil)
	if !s2.isCoordinator("desk/w2:p1", "home") {
		t.Fatal("roles not persisted")
	}
	if w := do(s, "POST", "/api/roles", userHdr, `{"agent_id":"desk/w2:p1","role":""}`); w.Code != 200 || strings.Contains(w.Body.String(), "w2:p1") {
		t.Fatalf("clear = %d %s", w.Code, w.Body)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBriefStorage(t *testing.T) {
	s, _, dir := coordServer(t)
	if w := do(s, "GET", "/api/brief", userHdr, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"brief":null}` {
		t.Fatalf("empty = %d %s", w.Code, w.Body)
	}
	w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), `{"markdown":"## proj\n**Finished:** feat/x"}`)
	if w.Code != 200 {
		t.Fatalf("set = %d %s", w.Code, w.Body)
	}
	var got struct{ Brief Brief }
	json.Unmarshal(do(s, "GET", "/api/brief", userHdr, "").Body.Bytes(), &got)
	if got.Brief.Markdown != "## proj\n**Finished:** feat/x" || got.Brief.By != "coordinator (desk/w1:p1)" || time.Since(got.Brief.At) > time.Minute {
		t.Fatalf("brief = %+v", got.Brief)
	}
	if fi, err := os.Stat(filepath.Join(dir, "brief.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("brief.json: %v %v", fi, err)
	}
	// The user can publish too.
	if w := do(s, "POST", "/api/brief", userHdr, `{"markdown":"mine"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"by":"me@example.com"`) {
		t.Fatalf("user brief = %d %s", w.Code, w.Body)
	}
	// 64 KB cap; empty refused; the old briefing stays.
	big, _ := json.Marshal(map[string]string{"markdown": strings.Repeat("x", maxBriefBytes+1)})
	if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), string(big)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", w.Code)
	}
	exact, _ := json.Marshal(map[string]string{"markdown": strings.Repeat("y", maxBriefBytes)})
	if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), string(exact)); w.Code != 200 {
		t.Errorf("64 KB exactly: %d", w.Code)
	}
	huge, _ := json.Marshal(map[string]string{"markdown": strings.Repeat("x", 200<<10)})
	if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), string(huge)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge: %d", w.Code)
	}
	if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), `{"markdown":"  "}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty: %d", w.Code)
	}
	b, _ := s.brief.get()
	if b == nil || len(b.Markdown) != maxBriefBytes {
		t.Fatalf("kept brief = %v", b)
	}
}

func TestNoHub(t *testing.T) {
	// A peer that keeps no ledger and has no hub: agents get 409 no_hub,
	// the app 503 (it hides the ledger's features).
	s := New(Config{AllowLogins: []string{"me@example.com"}, HostName: "kube", StateDir: t.TempDir(), NoLedger: true, AgentToken: testToken}, nil, nil)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/ledger", ""}, {"POST", "/api/handoffs", `{}`}, {"POST", "/api/marks", `{"agent_id":"x"}`},
		{"POST", "/api/marks/clear", `{"agent_id":"x"}`}, {"GET", "/api/brief", ""}, {"POST", "/api/brief", `{"markdown":"x"}`}, {"GET", "/api/roles", ""},
	} {
		if w := do(s, c.method, c.path, agentHdr("w1:p1"), c.body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no hub configured") {
			t.Errorf("agent %s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		if w := do(s, c.method, c.path, userHdr, c.body); w.Code != http.StatusServiceUnavailable {
			t.Errorf("user %s %s: %d", c.method, c.path, w.Code)
		}
	}
	if w := do(s, "GET", "/api/whoami", agentHdr("w1:p1"), ""); !strings.Contains(w.Body.String(), `"hub":""`) {
		t.Errorf("whoami = %s", w.Body)
	}
}

// fakeHub records what a peer forwards.
type fakeHub struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
	code int
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.reqs = append(h.reqs, r)
	h.body = append(h.body, string(b))
	code := h.code
	h.mu.Unlock()
	if code == 0 {
		code = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, `{"from":"hub","path":"`+r.URL.Path+`"}`)
}

func (h *fakeHub) last() (*http.Request, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.reqs) == 0 {
		return nil, ""
	}
	return h.reqs[len(h.reqs)-1], h.body[len(h.body)-1]
}

func TestPeerForwardsToHub(t *testing.T) {
	hub := &fakeHub{}
	hs := httptest.NewServer(hub)
	defer hs.Close()
	up, err := fleet.NewUpstream(hs.URL, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.NewUpstream(hs.URL, false, nil); err == nil {
		t.Fatal("plain http hub accepted without the test switch")
	}
	s := New(Config{AllowLogins: []string{"me@example.com"}, HostName: "kube", StateDir: t.TempDir(), Upstream: up, AgentToken: testToken}, nil, nil)
	if s.ledger != nil {
		t.Fatal("a peer with -hub keeps a ledger")
	}
	cases := []struct{ method, path, body, hubPath string }{
		{"GET", "/api/ledger", "", "/api/ledger"},
		{"POST", "/api/handoffs", `{"host":"kube","repo":"/r","branch":"b","brief":"x"}`, "/api/handoffs"},
		{"POST", "/api/marks", `{"agent_id":"kube/w2:p1","reason":"done"}`, "/api/marks"},
		{"POST", "/api/marks/clear", `{"agent_id":"kube/w2:p1"}`, "/api/marks/clear"},
		{"GET", "/api/brief", "", "/api/brief"},
		{"POST", "/api/brief", `{"markdown":"x"}`, "/api/brief"},
		{"GET", "/api/roles", "", "/api/roles"},
		{"GET", "/api/agents", "", "/api/agents"},
		// This host's own per-agent routes become host-qualified ones.
		{"POST", "/api/agents/w2:p1/prompt", `{"text":"hi"}`, "/api/hosts/kube/agents/w2:p1/prompt"},
		{"GET", "/api/agents/w2:p1/transcript?limit=5&evil=1", "", "/api/hosts/kube/agents/w2:p1/transcript"},
		{"POST", "/api/hosts/desk/agents/w3:p1/prompt", `{"text":"hi"}`, "/api/hosts/desk/agents/w3:p1/prompt"},
	}
	for _, c := range cases {
		w := do(s, c.method, c.path, agentHdr("w1:p1"), c.body)
		r, body := hub.last()
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"from":"hub"`) || r == nil || r.URL.Path != c.hubPath || r.Method != c.method {
			t.Fatalf("%s %s -> %d %s (hub saw %v)", c.method, c.path, w.Code, w.Body, r)
		}
		if r.Header.Get(HeaderForwarded) != "1" || r.Header.Get(HeaderAgent) != "w1:p1" || r.Header.Get(HeaderAgentHost) != "kube" ||
			r.Header.Get(HeaderAgentToken) != "" || r.Header.Get("Tailscale-User-Login") != "" {
			t.Fatalf("%s: forwarded headers = %v", c.path, r.Header)
		}
		if c.body != "" && body != c.body {
			t.Fatalf("%s: forwarded body = %q", c.path, body)
		}
		if strings.Contains(c.path, "limit=5") && r.URL.RawQuery != "limit=5" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
	}
	n := len(hub.reqs)
	// Local answers: whoami, playbook; the user's requests and the agent
	// role's forbidden ones are never forwarded.
	if w := do(s, "GET", "/api/whoami", agentHdr("w1:p1"), ""); !strings.Contains(w.Body.String(), `"hub":"`+hs.URL+`"`) {
		t.Errorf("whoami = %s", w.Body)
	}
	if w := do(s, "GET", "/api/playbook", agentHdr("w1:p1"), ""); w.Code != 200 {
		t.Errorf("playbook = %d", w.Code)
	}
	if w := do(s, "GET", "/api/ledger", userHdr, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("user ledger on a peer = %d", w.Code)
	}
	if w := do(s, "POST", "/api/removals/confirm", agentHdr("w1:p1"), `{"ids":["x"]}`); w.Code != http.StatusForbidden {
		t.Errorf("forbidden route = %d", w.Code)
	}
	if len(hub.reqs) != n {
		t.Fatalf("forwarded %d more requests", len(hub.reqs)-n)
	}
	// A hub that rejects the peer: 502 with a hint, not a 401 for the CLI.
	hub.mu.Lock()
	hub.code = http.StatusUnauthorized
	hub.mu.Unlock()
	if w := do(s, "GET", "/api/ledger", agentHdr("w1:p1"), ""); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "-ts-allow-login") {
		t.Errorf("rejected = %d %s", w.Code, w.Body)
	}
}

func TestHubTreatsForwardedAsAgent(t *testing.T) {
	// A real hub behind the peer: the forwarded request gets the agent
	// role there (attributed to the peer's agent) and its restrictions.
	hubDir := t.TempDir()
	hubSrv := New(Config{InsecureNoAuth: true, HostName: "desk", StateDir: hubDir}, nil, nil)
	hs := httptest.NewServer(hubSrv.Handler())
	defer hs.Close()
	up, _ := fleet.NewUpstream(hs.URL, true, nil)
	peer := New(Config{AllowLogins: []string{"me@example.com"}, HostName: "kube", Upstream: up, AgentToken: testToken}, nil, nil)
	if w := do(peer, "POST", "/api/brief", agentHdr("w1:p1"), `{"markdown":"from kube"}`); w.Code != 200 {
		t.Fatalf("brief via peer = %d %s", w.Code, w.Body)
	}
	b, _ := hubSrv.brief.get()
	if b == nil || b.Markdown != "from kube" || b.By != "kube/w1:p1" {
		t.Fatalf("hub brief = %+v", b)
	}
	// Directly at the hub, a forwarded request is held to the agent role.
	fwd := map[string]string{HeaderForwarded: "1", HeaderAgent: "w1:p1", HeaderAgentHost: "kube"}
	if w := do(hubSrv, "POST", "/api/removals/confirm", fwd, `{"ids":["x"]}`); w.Code != http.StatusForbidden {
		t.Errorf("forwarded confirm = %d", w.Code)
	}
	if w := do(hubSrv, "GET", "/api/whoami", fwd, ""); !strings.Contains(w.Body.String(), `"agent":"kube/w1:p1"`) {
		t.Errorf("forwarded whoami = %s", w.Body)
	}
	// A hub never forwards a forwarded request again (no loops), even if
	// it is misconfigured with -hub itself.
	loop := New(Config{InsecureNoAuth: true, HostName: "x", Upstream: up}, nil, nil)
	if w := do(loop, "GET", "/api/ledger", fwd, ""); w.Code != http.StatusConflict {
		t.Errorf("loop = %d %s", w.Code, w.Body)
	}
}

func TestIdempotentHandoff(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh.set(func(f *fakeLaunchHerdr) {
		f.hook = worktreeHook(t, &wts, nil)
		f.agents = []herdr.AgentInfo{{Name: "coordinator", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle}}
	})
	m := managerWith(t, client, "desk", 1)
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot, StateDir: t.TempDir(), AgentToken: testToken}, client, m)
	s.findExec = func(name string) bool { return name == "pi" }
	body := `{"host":"desk","repo":"` + repo + `","branch":"feat/a","kind":"pi","brief":"Do A."}`
	starts := func() int {
		n := 0
		for _, c := range fh.methods() {
			if c == "agent.start" {
				n++
			}
		}
		return n
	}

	w := do(s, "POST", "/api/handoffs", agentHdr("w1:p1"), body)
	var r1 struct {
		Entry    ledger.Entry `json:"entry"`
		Existing bool         `json:"existing"`
	}
	json.Unmarshal(w.Body.Bytes(), &r1)
	if w.Code != 200 || r1.Existing || r1.Entry.AgentID != "desk/w9:p1" || r1.Entry.From != "coordinator (desk/w1:p1)" {
		t.Fatalf("first = %d %s", w.Code, w.Body)
	}
	wt := filepath.Join(wtRoot, "proj", "feat-a")
	fh.set(func(f *fakeLaunchHerdr) {
		wts = append(wts, map[string]any{"path": wt, "branch": "feat/a", "is_linked_worktree": true})
	})
	// Right away, before the agent list caught up: still the same worker.
	if w := do(s, "POST", "/api/handoffs", agentHdr("w1:p1"), body); !strings.Contains(w.Body.String(), `"existing":true`) || starts() != 1 {
		t.Fatalf("immediate repeat = %d %s", w.Code, w.Body)
	}
	waitAgents(t, m, 2)
	noGrace := func() {
		s.coord.mu.Lock()
		clear(s.coord.started)
		s.coord.mu.Unlock()
	}
	noGrace()

	// Again while the worker runs: the existing entry, nothing started.
	w = do(s, "POST", "/api/handoffs", agentHdr("w1:p1"), body)
	var r2 struct {
		Entry     ledger.Entry `json:"entry"`
		Existing  bool         `json:"existing"`
		Restarted bool         `json:"restarted"`
	}
	json.Unmarshal(w.Body.Bytes(), &r2)
	if w.Code != 200 || !r2.Existing || r2.Restarted || r2.Entry.ID != r1.Entry.ID || starts() != 1 {
		t.Fatalf("second = %d %s (starts %d)", w.Code, w.Body, starts())
	}
	// Finished counts as well.
	s.ledger.Update(r1.Entry.ID, func(e *ledger.Entry) { e.Status = ledger.Finished })
	if w := do(s, "POST", "/api/handoffs", agentHdr("w1:p1"), body); !strings.Contains(w.Body.String(), `"existing":true`) || starts() != 1 {
		t.Fatalf("finished = %s", w.Body)
	}

	// The worker's agent is gone: a new agent in the same worktree, the
	// same entry updated.
	fh.set(func(f *fakeLaunchHerdr) { f.agents = f.agents[:1] })
	waitAgents(t, m, 1)
	w = do(s, "POST", "/api/handoffs", agentHdr("w1:p1"), strings.Replace(body, "Do A.", "Do A again.", 1))
	var r3 struct {
		Entry     ledger.Entry `json:"entry"`
		Existing  bool         `json:"existing"`
		Restarted bool         `json:"restarted"`
		Worker    workerResult `json:"worker"`
	}
	json.Unmarshal(w.Body.Bytes(), &r3)
	if w.Code != 200 || !r3.Restarted || r3.Entry.ID != r1.Entry.ID || r3.Entry.Status != ledger.Running ||
		r3.Entry.AgentID != "desk/w9:p2" || r3.Entry.Worktree != wt || r3.Entry.Brief != "Do A again." || !r3.Worker.Reused || starts() != 2 {
		t.Fatalf("restart = %d %s", w.Code, w.Body)
	}
	if fh.call("worktree.open") == nil {
		t.Fatal("the worktree was not reopened")
	}
	if n := len(s.ledger.Entries()); n != 1 {
		t.Fatalf("%d entries, want 1", n)
	}

	// Gone again, and the new agent fails to start: an error, and the
	// entry stays as it was (no failed duplicate).
	noGrace()
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = f.agents[:1]
		f.startErr = []*herdr.Error{{Code: "agent_start_failed", Message: "nope"}}
	})
	waitAgents(t, m, 1)
	w = do(s, "POST", "/api/handoffs", agentHdr("w1:p1"), body)
	if w.Code == 200 || !strings.Contains(w.Body.String(), "a new one could not be started") || len(s.ledger.Entries()) != 1 {
		t.Fatalf("failed restart = %d %s", w.Code, w.Body)
	}
	if e, _ := s.ledger.Get(r1.Entry.ID); e.Status != ledger.Running || e.AgentID != "desk/w9:p2" {
		t.Fatalf("entry after a failed restart = %+v", e)
	}

	// Removed (or failed) entries do not count: a new handoff starts.
	s.ledger.Update(r1.Entry.ID, func(e *ledger.Entry) { e.Status = ledger.Removed })
	fh.set(func(f *fakeLaunchHerdr) { f.agents = f.agents[:1] })
	waitAgents(t, m, 1)
	w = do(s, "POST", "/api/handoffs", agentHdr("w1:p1"), body)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"existing":true`) || len(s.ledger.Entries()) != 2 {
		t.Fatalf("after removal = %d %s", w.Code, w.Body)
	}
}

func TestCoordinatorLaunchPreset(t *testing.T) {
	root, _ := launchTree(t)
	coordDir, _ := filepath.EvalSymlinks(t.TempDir())
	fh, client := startFakeLaunchHerdr(t)
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, CoordinatorDir: coordDir, StateDir: t.TempDir(),
		// Per-install extras (here a permission gate's --auto) are appended
		// to the preset's built-in pi arguments.
		CoordinatorArgs: map[string][]string{"pi": {"--auto"}}}, client, nil)
	s.findExec = func(name string) bool { return name == "pi" || name == "claude" || name == "codex" }

	var o optionsBody
	w := do(s, "GET", "/api/launch/options", nil, "")
	json.Unmarshal(w.Body.Bytes(), &o)
	if !strings.Contains(w.Body.String(), `"coordinator_dir":"`+homeLabel(coordDir)+`"`) {
		t.Fatalf("options = %s", w.Body)
	}

	// The dir in the body is ignored: the coordinator directory is used.
	w = do(s, "POST", "/api/launch", nil, `{"coordinator":true,"kind":"claude","dir":"/etc"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"coordinator":true`) {
		t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	if ws := fh.call("workspace.create"); ws.Params["cwd"] != coordDir || ws.Params["label"] != "coordinator" {
		t.Fatalf("workspace.create = %v", ws.Params)
	}
	st := fh.call("agent.start")
	args, _ := json.Marshal(st.Params["args"])
	// The first prompt rides on the command line, after "--" so the
	// variadic --allowedTools cannot take it; nothing is typed in after.
	var argv []string
	json.Unmarshal(args, &argv)
	if st.Params["name"] != "coordinator" || st.Params["kind"] != "claude" || len(argv) != 4 ||
		argv[0] != "--allowedTools" || argv[1] != "Bash(comebye:*)" || argv[2] != "--" {
		t.Fatalf("agent.start = %v", st.Params)
	}
	if text := argv[3]; !strings.Contains(text, "comebye playbook --harness claude") || len(text) > 400 {
		t.Fatalf("first prompt = %q", text)
	}
	if fh.call("agent.prompt") != nil {
		t.Fatal("first prompt typed in as well")
	}
	if !s.isCoordinator("desk/w9:p1", "coordinator") {
		t.Fatal("role not recorded")
	}

	// Taken name: coordinator-2; pi gets pi-coordinator's arguments plus
	// the configured extras.
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	w = do(s, "POST", "/api/launch", nil, `{"coordinator":true,"kind":"pi"}`)
	st = fh.call("agent.start")
	args, _ = json.Marshal(st.Params["args"])
	if w.Code != 200 || st.Params["name"] != "coordinator-2" ||
		!strings.HasPrefix(string(args), `["--name","coordinator-2","--no-skills","--no-context-files","--tools","read,grep,find,ls,bash","--auto","--","`) ||
		!strings.Contains(string(args), "--harness pi") {
		t.Fatalf("pi launch = %d %s %v", w.Code, w.Body, st.Params)
	}
	// Other kinds: no extra arguments, the generic playbook.
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	do(s, "POST", "/api/launch", nil, `{"coordinator":true,"kind":"codex"}`)
	if st := fh.call("agent.start"); st.Params["args"] != nil || !strings.Contains(fh.call("agent.prompt").Params["text"].(string), "--harness generic") {
		t.Fatalf("codex = %v", st.Params)
	}
	// Without the preset, the coordinator directory is outside the roots.
	if w := do(s, "POST", "/api/launch", nil, `{"dir":"`+coordDir+`","kind":"pi"}`); w.Code != http.StatusForbidden {
		t.Fatalf("plain launch in the coordinator dir = %d", w.Code)
	}
	// Launching off: no coordinator either.
	off := New(Config{HostName: "desk", CoordinatorDir: coordDir}, client, nil)
	if w := do(off, "POST", "/api/launch", nil, `{"coordinator":true,"kind":"pi"}`); w.Code != http.StatusForbidden {
		t.Fatalf("launch off = %d", w.Code)
	}
}

func TestCoordinatorLaunchOnPeer(t *testing.T) {
	var got []byte
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/launch":
			got, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"agent":{"id":"kube/wS:p3","host":"kube","pane_id":"wS:p3","name":"coordinator","agent":"pi","agent_status":"idle"},"workspace_id":"wS","coordinator":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	pu, _ := url.Parse(peer.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URL: pu}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir()}, nil, nil)
	w := do(s, "POST", "/api/hosts/kube/launch", nil, `{"coordinator":true,"kind":"pi"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "wS:p3") || !strings.Contains(string(got), `"coordinator":true`) {
		t.Fatalf("peer launch = %d %s (peer got %s)", w.Code, w.Body, got)
	}
	if !s.isCoordinator("kube/wS:p3", "coordinator") {
		t.Fatal("hub did not record the peer coordinator's role")
	}
	if w := do(s, "POST", "/api/hosts/nope/launch", nil, `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown host = %d", w.Code)
	}
}
