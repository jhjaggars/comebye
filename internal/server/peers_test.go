package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
)

// peerRig is a hub server (hub mode, an empty registry) in front of a
// fake peer, driven over real HTTP.
type peerRig struct {
	t    *testing.T
	dir  string
	fl   *fleet.Fleet
	srv  *httptest.Server
	peer *httptest.Server
	stop context.CancelFunc
}

func newPeerRig(t *testing.T, dir string, hubMode bool) *peerRig {
	t.Helper()
	peer := httptest.NewServer(&hubPeer{})
	t.Cleanup(peer.Close)
	store, err := fleet.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	opt := fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}}
	peers := fleet.LoadDynamic(store, nil, "desk", opt, t.Logf)
	fl := fleet.New(nil, "desk", peers, fleet.Options{Version: "v1.0.0", HubMode: hubMode, Logf: func(string, ...any) {}})
	fl.AttachStore(store, opt)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: "tok", Version: "v1.0.0"}, nil, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &peerRig{t: t, dir: dir, fl: fl, srv: srv, peer: peer, stop: cancel}
}

func (r *peerRig) req(method, path, body string, hdr map[string]string) (int, string) {
	r.t.Helper()
	q, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	q.Header.Set("Tailscale-User-Login", "me@example.com")
	if body != "" {
		q.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		q.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(q)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *peerRig) agentsContain(id string) bool {
	_, body := r.req("GET", "/api/agents", "", nil)
	return strings.Contains(body, `"id":"`+id+`"`)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPeerRegistryAddAgentsAppearRemove(t *testing.T) {
	rig := newPeerRig(t, t.TempDir(), true)
	if code, body := rig.req("GET", "/api/peers", "", nil); code != 200 || !strings.Contains(body, `"peers":[]`) {
		t.Fatalf("empty list: %d %s", code, body)
	}
	code, body := rig.req("POST", "/api/peers", `{"name":"kube","urls":["`+rig.peer.URL+`"],"tag":"tag:worker"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	var info struct {
		Name, Source, Tag string
		AddedBy           string   `json:"added_by"`
		URLs              []string `json:"urls"`
	}
	json.Unmarshal([]byte(body), &info)
	if info.Name != "kube" || info.Source != "dynamic" || info.AddedBy != "me@example.com" || info.Tag != "tag:worker" || len(info.URLs) != 1 {
		t.Fatalf("added = %s", body)
	}
	waitUntil(t, "peer agents", func() bool { return rig.agentsContain("kube/wS:p1") })
	// Its agents are reachable through the hub like any peer's.
	if code, body := rig.req("GET", "/api/hosts/kube/agents/wS:p1/transcript?limit=5", "", nil); code != 200 || !strings.Contains(body, "hello from the peer") {
		t.Fatalf("transcript via new peer: %d %s", code, body)
	}
	if code, body := rig.req("GET", "/api/peers", "", nil); code != 200 || !strings.Contains(body, `"name":"kube"`) || !strings.Contains(body, `"source":"dynamic"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	// Re-adding is refused, not an upsert.
	if code, body := rig.req("POST", "/api/peers", `{"name":"kube","urls":["`+rig.peer.URL+`"]}`, nil); code != http.StatusConflict || !strings.Contains(body, "peer_exists") {
		t.Fatalf("re-add: %d %s", code, body)
	}
	if code, body := rig.req("POST", "/api/peers", `{"name":"x","urls":["https://evil.example.com"]}`, nil); code != http.StatusBadRequest || !strings.Contains(body, "tailnet") {
		t.Fatalf("non-tailnet: %d %s", code, body)
	}
	if code, body := rig.req("POST", "/api/peers", `{"name":"desk","urls":["https://d.t.ts.net"]}`, nil); code != http.StatusConflict || !strings.Contains(body, "peer_is_local") {
		t.Fatalf("local name: %d %s", code, body)
	}

	if code, _ := rig.req("DELETE", "/api/peers/kube", "", nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if rig.agentsContain("kube/wS:p1") {
		t.Fatal("removed peer's agents are still listed")
	}
	if code, body := rig.req("GET", "/api/hosts/kube/agents/wS:p1/transcript", "", nil); code != http.StatusNotFound {
		t.Fatalf("removed host route: %d %s", code, body)
	}
	if code, body := rig.req("DELETE", "/api/peers/kube", "", nil); code != http.StatusNotFound || !strings.Contains(body, "unknown_peer") {
		t.Fatalf("remove twice: %d %s", code, body)
	}
}

// Peers added at runtime survive a restart: a new hub on the same state
// directory has them, without being told again.
func TestPeerRegistryRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	rig := newPeerRig(t, dir, true)
	if code, body := rig.req("POST", "/api/peers", `{"name":"kube","urls":["`+rig.peer.URL+`"]}`, nil); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	rig.stop()
	rig.srv.Close()

	// Even without hub mode: stored peers make it a hub.
	again := newPeerRig(t, dir, false)
	// (its own fake peer is a different port, so only the registry
	// entry is checked, not that the old address answers)
	if code, body := again.req("GET", "/api/peers", "", nil); code != 200 || !strings.Contains(body, `"name":"kube"`) {
		t.Fatalf("after restart: %d %s", code, body)
	}
	if code, _ := again.req("DELETE", "/api/peers/kube", "", nil); code != http.StatusNoContent {
		t.Fatalf("remove after restart: %d", code)
	}
	third := newPeerRig(t, dir, true)
	if code, body := third.req("GET", "/api/peers", "", nil); code != 200 || strings.Contains(body, "kube") {
		t.Fatalf("removal not persisted: %d %s", code, body)
	}
}

// The registry is the user's alone, and only a hub has one.
func TestPeerRegistryRolesAndHubOnly(t *testing.T) {
	rig := newPeerRig(t, t.TempDir(), true)
	agent := map[string]string{"Tailscale-User-Login": "", HeaderAgentToken: "tok"}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/peers", ""},
		{"POST", "/api/peers", `{"name":"k","urls":["https://k.t.ts.net"]}`},
		{"DELETE", "/api/peers/k", ""},
	} {
		code, body := rig.req(c.method, c.path, c.body, agent)
		if code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("agent token %s %s: %d %s", c.method, c.path, code, body)
		}
	}
	// A forwarded agent request (a peer's coordinator) is the agent role too.
	if code, _ := rig.req("GET", "/api/peers", "", map[string]string{HeaderForwarded: "1"}); code != http.StatusForbidden {
		t.Errorf("forwarded agent: %d", code)
	}
	// No identity at all.
	anon, _ := http.NewRequest("GET", rig.srv.URL+"/api/peers", nil)
	if resp, err := http.DefaultClient.Do(anon); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: %v %v", resp, err)
	}

	// An instance that is not a hub refuses all three.
	s := newTest("me@example.com")
	h := map[string]string{"Tailscale-User-Login": "me@example.com", "Content-Type": "application/json"}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/peers", ""}, {"POST", "/api/peers", `{"name":"k","urls":["https://k.t.ts.net"]}`}, {"DELETE", "/api/peers/k", ""},
	} {
		if w := do(s, c.method, c.path, h, c.body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not_a_hub") {
			t.Errorf("non-hub %s: %d %s", c.method, w.Code, w.Body)
		}
	}
}

func TestStaticPeerCannotBeRemovedThroughAPI(t *testing.T) {
	peer := httptest.NewServer(&hubPeer{})
	defer peer.Close()
	store, _ := fleet.OpenStore(t.TempDir())
	opt := fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}}
	static, err := fleet.ParsePeers("kube="+peer.URL, fleet.ParseOptions{LocalName: "desk", AllowHTTPLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(nil, "desk", static, fleet.Options{Logf: func(string, ...any) {}})
	fl.AttachStore(store, opt)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl}, nil, nil)
	h := map[string]string{"Tailscale-User-Login": "me@example.com", "Content-Type": "application/json"}
	w := do(s, "DELETE", "/api/peers/kube", h, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "static_peer") || !strings.Contains(w.Body.String(), "COMEBYE_PEERS") {
		t.Fatalf("static removal: %d %s", w.Code, w.Body)
	}
	w = do(s, "POST", "/api/peers", h, `{"name":"kube","urls":["https://k.t.ts.net"]}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "COMEBYE_PEERS") {
		t.Fatalf("static name clash: %d %s", w.Code, w.Body)
	}
	w = do(s, "GET", "/api/peers", h, "")
	if !strings.Contains(w.Body.String(), `"source":"static"`) {
		t.Fatalf("list: %s", w.Body)
	}
}

func TestWhoamiAndHostsCarryVersion(t *testing.T) {
	s := New(Config{ReadLines: 100, HostName: "desk", Version: "v1.2.3"}, nil, nil)
	if w := do(s, "GET", "/api/whoami", nil, ""); !strings.Contains(w.Body.String(), `"version":"v1.2.3"`) {
		t.Fatalf("whoami: %s", w.Body)
	}
	w := do(s, "GET", "/api/hosts", nil, "")
	if !strings.Contains(w.Body.String(), `"version":"v1.2.3"`) || w.Header().Get("X-Comebye-Version") != "v1.2.3" {
		t.Fatalf("hosts: %s %v", w.Body, w.Header())
	}
}
