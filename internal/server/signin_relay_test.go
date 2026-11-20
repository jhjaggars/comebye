package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
)

// signinHub is a hub whose only peer, "kube", is a real server (signing in
// with the fake claude) reached over HTTP as the user's login, as a tailnet
// peer sees its hub. It returns the hub, the peer's HOME and state dir, and
// the hub's own state dir.
func signinHub(t *testing.T, mode string) (hub, peer *Server, peerHome, peerDir, hubDir string) {
	t.Helper()
	peer, peerHome, peerDir = signinHost(t, mode) // sets HOME and PATH for the fake claude
	peer.cfg.HostName = "kube"
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		r.RemoteAddr = "100.64.0.1:1234"
		peer.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(ps.Close)
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	hubDir = t.TempDir()
	hub = newServer(t, Config{AllowLogins: []string{"me@example.com"}, Fleet: fl, StateDir: hubDir, AgentToken: testToken}, nil, nil)
	return
}

func TestSigninRelayThroughHub(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })

	hub, _, peerHome, peerDir, hubDir := signinHub(t, "echo")
	var seen bytes.Buffer
	const sentinel = "RELAY-SENTINEL-4f9a"

	w := do(hub, "POST", "/api/hosts/kube/signin", userHdr, `{"agent":"claude"}`)
	seen.Write(w.Body.Bytes())
	if w.Code != 200 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	start := jsonOf(t, w)
	id := start["id"].(string)
	if start["state"] != "waiting_code" || !strings.HasPrefix(start["url"].(string), "https://claude.com/") {
		t.Fatalf("start = %v", start)
	}
	// A second start attaches (the peer's 409 passes through).
	if w := do(hub, "POST", "/api/hosts/kube/signin", userHdr, `{}`); w.Code != http.StatusConflict || jsonOf(t, w)["session"].(map[string]any)["id"] != id {
		t.Fatalf("second start = %d %s", w.Code, w.Body)
	}
	if m := jsonOf(t, do(hub, "GET", "/api/hosts/kube/signin/"+id, userHdr, "")); m["state"] != "waiting_code" {
		t.Fatalf("get = %v", m)
	}
	w = do(hub, "POST", "/api/hosts/kube/signin/"+id+"/code", userHdr, `{"code":"`+sentinel+`"}`)
	seen.Write(w.Body.Bytes())
	if w.Code != 200 {
		t.Fatalf("code = %d %s", w.Code, w.Body)
	}
	// The wrong code ends it; the hub reports what the peer says.
	deadline := 0
	for {
		w := do(hub, "GET", "/api/hosts/kube/signin/"+id, userHdr, "")
		seen.Write(w.Body.Bytes())
		if m := jsonOf(t, w); m["state"] == "failed" {
			break
		}
		if deadline++; deadline > 100 {
			t.Fatalf("never failed: %s", w.Body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Cancel through the hub, on a fresh session.
	id2 := jsonOf(t, do(hub, "POST", "/api/hosts/kube/signin", userHdr, `{}`))["id"].(string)
	if w := do(hub, "DELETE", "/api/hosts/kube/signin/"+id2, userHdr, ""); w.Code != 200 || jsonOf(t, w)["reason"] != "cancelled" {
		t.Fatalf("cancel = %d %s", w.Code, w.Body)
	}
	// Unknown id on the peer: its 404 passes through; a hostile id never leaves the hub.
	if w := do(hub, "GET", "/api/hosts/kube/signin/nope", userHdr, ""); w.Code != 404 {
		t.Errorf("unknown id = %d", w.Code)
	}
	if w := do(hub, "GET", "/api/hosts/kube/signin/a%2Fb", userHdr, ""); w.Code != 400 {
		t.Errorf("slash id = %d", w.Code)
	}
	if w := do(hub, "POST", "/api/hosts/nowhere/signin", userHdr, `{}`); w.Code != 404 {
		t.Errorf("unknown host = %d", w.Code)
	}

	// The code is nowhere on the hub: not in an answer, its log, or its state;
	// and on the peer it is only in what the fake claude echoed (not on disk).
	if strings.Contains(seen.String(), sentinel) {
		t.Errorf("the code is in an answer: %s", seen.String())
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Errorf("the code is in the log: %s", logs.String())
	}
	for _, root := range []string{hubDir, peerDir, peerHome} {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), sentinel) {
					t.Errorf("the code is in %s", p)
				}
			}
			return nil
		})
	}
	// The hub keeps no state of its own for it.
	if ents, _ := filepath.Glob(filepath.Join(hubDir, "*signin*")); len(ents) != 0 {
		t.Errorf("hub state: %v", ents)
	}
}

func TestSigninRelayGoodCode(t *testing.T) {
	hub, _, home, _, _ := signinHub(t, "")
	id := jsonOf(t, do(hub, "POST", "/api/hosts/kube/signin", userHdr, `{}`))["id"].(string)
	if w := do(hub, "POST", "/api/hosts/kube/signin/"+id+"/code", userHdr, `{"code":"GOOD-CODE"}`); w.Code != 200 {
		t.Fatalf("code = %d %s", w.Code, w.Body)
	}
	for i := 0; ; i++ {
		m := jsonOf(t, do(hub, "GET", "/api/hosts/kube/signin/"+id, userHdr, ""))
		if m["state"] == "signed_in" {
			break
		}
		if i > 100 {
			t.Fatalf("state = %v", m)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "fake-ok")); err != nil {
		t.Fatal(err)
	}
}

// The hub and the peer both refuse the agent role, a forwarded agent request
// and a tagged node, and a refusal by the hub never reaches the peer.
func TestSigninRelayUserOnly(t *testing.T) {
	hub, peer, _, _, _ := signinHub(t, "")
	routes := []struct{ method, path, body string }{
		{"POST", "/api/hosts/kube/signin", `{}`},
		{"GET", "/api/hosts/kube/signin/si_x", ""},
		{"POST", "/api/hosts/kube/signin/si_x/code", `{"code":"GOOD-CODE"}`},
		{"DELETE", "/api/hosts/kube/signin/si_x", ""},
		{"POST", "/api/signin", `{}`},
		{"GET", "/api/signin/si_x", ""},
		{"POST", "/api/signin/si_x/code", `{"code":"GOOD-CODE"}`},
		{"DELETE", "/api/signin/si_x", ""},
	}
	fwd := map[string]string{HeaderAgentToken: testToken, HeaderForwarded: "1", HeaderAgent: "w1:p1", HeaderAgentHost: "kube"}
	for name, s := range map[string]*Server{"hub": hub, "peer": peer} {
		tagged := func(method, path, body string) int {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.RemoteAddr = "100.64.0.9:1"
			r = r.WithContext(context.WithValue(r.Context(), taggedKey{}, tagged{peer: "worker-3", node: "worker-3", tag: "tag:worker"}))
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			return w.Code
		}
		for _, rt := range routes {
			if w := do(s, rt.method, rt.path, agentHdr("w1:p1"), rt.body); w.Code != 403 {
				t.Errorf("%s agent %s %s = %d", name, rt.method, rt.path, w.Code)
			}
			if w := do(s, rt.method, rt.path, fwd, rt.body); w.Code != 403 {
				t.Errorf("%s forwarded %s %s = %d", name, rt.method, rt.path, w.Code)
			}
			if c := tagged(rt.method, rt.path, rt.body); c != 403 {
				t.Errorf("%s tagged %s %s = %d", name, rt.method, rt.path, c)
			}
		}
	}
	if _, err := peer.signin.Get("si_x"); err == nil {
		t.Errorf("a refused call reached the sign-in manager")
	}
}
