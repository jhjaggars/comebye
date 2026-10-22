package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/state"
)

func TestSingleHostAPIShape(t *testing.T) {
	s := New(Config{ReadLines: 100, HostName: "desk"}, nil, nil)
	w := do(s, "GET", "/api/agents", nil, "")
	var body struct {
		Agents []json.RawMessage  `json:"agents"`
		Hosts  []state.HostStatus `json:"hosts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	if body.Agents == nil || len(body.Hosts) != 1 || body.Hosts[0].Name != "desk" || !body.Hosts[0].Local || body.Hosts[0].Status != "ok" {
		t.Fatalf("body = %s", w.Body)
	}
	// The host-qualified route for the local host reaches the local handlers.
	if w := do(s, "GET", "/api/hosts/desk/agents/wS:p1/output?source=detection", nil, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("local host route: %d", w.Code)
	}
	if w := do(s, "POST", "/api/hosts/desk/agents/wS:p1/keys", nil, `{"keys":["rm"]}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_key") {
		t.Fatalf("local keys: %d %s", w.Code, w.Body)
	}
	// Unknown hosts are not proxied anywhere.
	if w := do(s, "GET", "/api/hosts/evil.example.com/agents/wS:p1/output", nil, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_host") {
		t.Fatalf("unknown host: %d %s", w.Code, w.Body)
	}
}

// hubPeer is a stand-in peer instance.
type hubPeer struct {
	mu   sync.Mutex
	hits []string
}

func (p *hubPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	hit := r.Method + " " + r.URL.EscapedPath()
	if r.URL.RawQuery == "full=1" {
		hit += "?full=1"
	}
	p.hits = append(p.hits, hit)
	p.mu.Unlock()
	switch r.URL.Path {
	case "/api/events":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: agents`+"\n"+`data: {"type":"agents","version":"peer0version","agents":[{"pane_id":"wS:p1","name":"remote","agent_status":"idle"}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case "/api/agents/wS:p1/transcript":
		w.Header().Set("ETag", `W/"peer1"`)
		if r.Header.Get("If-None-Match") == `W/"peer1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"messages":[{"id":"a","kind":"assistant","text":"hello from the peer"}],"total":1}`)
	case "/api/agents/wS:p1/subagents/a1234567/transcript":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"messages":[{"id":"s","kind":"assistant","text":"subagent steps %s"}],"total":1}`, r.URL.RawQuery)
	case "/api/agents/wS:p1/keys":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	case "/api/launch/dirs":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q,"query":%q,"entries":[]}`, r.URL.Query().Get("path"), r.URL.RawQuery)
	case "/api/launch":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agent":{"id":"kube/wS:p9","host":"kube","pane_id":"wS:p9","name":"x"},"workspace_id":"w9"}`)
	default:
		http.NotFound(w, r)
	}
}

func (p *hubPeer) count(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, h := range p.hits {
		if strings.HasPrefix(h, prefix) {
			n++
		}
	}
	return n
}

func TestHubServesPeers(t *testing.T) {
	peer := &hubPeer{}
	ps := httptest.NewServer(peer)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URL: pu}}, fleet.Options{Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fl.Run(ctx)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl}, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req := func(method, path, body string, hdr map[string]string) (int, string) {
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		r.Header.Set("Tailscale-User-Login", "me@example.com")
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

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := req("GET", "/api/agents", "", nil)
		if strings.Contains(body, `"id":"kube/wS:p1"`) && strings.Contains(body, `"name":"kube","status":"ok"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer agents never showed up: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The SSE stream's first event carries the merged list and host status.
	r, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	r.Header.Set("Tailscale-User-Login", "me@example.com")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	var first string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			first = sc.Text()
			break
		}
	}
	resp.Body.Close()
	if !strings.Contains(first, `"id":"kube/wS:p1"`) || !strings.Contains(first, `"hosts":[{"name":"desk","local":true`) {
		t.Fatalf("first SSE event = %s", first)
	}
	// The page was served by the hub, so the hub's version is what counts,
	// never a peer's.
	if !strings.Contains(first, `"version":"`+s.version+`"`) || strings.Contains(first, "peer0version") {
		t.Fatalf("hub event version: %s (want %s)", first, s.version)
	}
	// Proxied calls carry the hub's version header too.
	pr, _ := http.NewRequest("GET", srv.URL+"/api/hosts/kube/agents/wS:p1/dialog", nil)
	pr.Header.Set("Tailscale-User-Login", "me@example.com")
	if resp, err := http.DefaultClient.Do(pr); err != nil || resp.Header.Get("X-App-Version") != s.version {
		t.Fatalf("proxied X-App-Version: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	if code, body := req("GET", "/api/hosts/kube/agents/wS:p1/transcript?limit=5", "", nil); code != 200 || !strings.Contains(body, "hello from the peer") {
		t.Fatalf("transcript via hub: %d %s", code, body)
	}
	// A transcript poll that the peer answers 304 stays a bodiless 304.
	cr, _ := http.NewRequest("GET", srv.URL+"/api/hosts/kube/agents/wS:p1/transcript?limit=5", nil)
	cr.Header.Set("Tailscale-User-Login", "me@example.com")
	cr.Header.Set("If-None-Match", `W/"peer1"`)
	if resp, err := http.DefaultClient.Do(cr); err != nil || resp.StatusCode != http.StatusNotModified || resp.Header.Get("ETag") != `W/"peer1"` {
		t.Fatalf("conditional transcript via hub: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	// A subagent's transcript: only the transcript query parameters pass,
	// and only a well-formed subagent id is forwarded.
	if code, body := req("GET", "/api/hosts/kube/agents/wS:p1/subagents/a1234567/transcript?limit=5&from=x&evil=1", "", nil); code != 200 ||
		!strings.Contains(body, "subagent steps from=x&limit=5\"") {
		t.Fatalf("subagent transcript via hub: %d %s", code, body)
	}
	if code, _ := req("GET", "/api/hosts/kube/agents/wS:p1/subagents/..%2Fx/transcript", "", nil); code != http.StatusBadRequest {
		t.Fatalf("bad subagent id via hub: %d", code)
	}
	if n := peer.count("GET /api/agents/wS:p1/subagents/"); n != 1 {
		t.Fatalf("peer got %d subagent GETs, want 1", n)
	}
	if n := peer.count("GET /api/events?full=1"); n == 0 {
		t.Fatal("the hub must read the peer's stream with full=1")
	}
	if code, body := req("POST", "/api/hosts/kube/agents/wS:p1/keys", `{"keys":["esc"]}`, map[string]string{"Content-Type": "application/json"}); code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("keys via hub: %d %s", code, body)
	}
	// The hub's own protections apply before anything is proxied.
	if code, _ := req("POST", "/api/hosts/kube/agents/wS:p1/keys", `{"keys":["esc"]}`, map[string]string{
		"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("cross-site POST via hub: %d", code)
	}
	if code, _ := req("POST", "/api/hosts/kube/agents/wS:p1/keys", `{"keys":["esc"]}`, map[string]string{"Content-Type": "text/plain"}); code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain POST via hub: %d", code)
	}
	anon, _ := http.NewRequest("GET", srv.URL+"/api/hosts/kube/agents/wS:p1/transcript", nil)
	if resp, err := http.DefaultClient.Do(anon); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %v %v", resp, err)
	}
	// Launch calls for a peer go to that peer's /api/launch endpoints.
	if code, body := req("GET", "/api/hosts/kube/launch/dirs?path=/home/me/code&evil=1", "", nil); code != 200 || !strings.Contains(body, `"query":"path=%2Fhome%2Fme%2Fcode"`) {
		t.Fatalf("launch dirs via hub: %d %s", code, body)
	}
	if code, body := req("POST", "/api/hosts/kube/launch", `{"dir":"/home/me/code/x","kind":"pi"}`, map[string]string{"Content-Type": "application/json"}); code != 200 || !strings.Contains(body, "kube/wS:p9") {
		t.Fatalf("launch via hub: %d %s", code, body)
	}
	if code, _ := req("POST", "/api/hosts/kube/launch", `{}`, map[string]string{
		"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("cross-site launch via hub: %d", code)
	}
	if n := peer.count("POST "); n != 2 {
		t.Fatalf("peer got %d POSTs, want 2", n)
	}
	if n := peer.count("GET /api/agents/wS:p1/transcript"); n != 2 {
		t.Fatalf("peer got %d transcript GETs, want 2", n)
	}
}
