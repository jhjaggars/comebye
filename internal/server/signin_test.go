package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/signin"
)

// signinHost is a server whose sign-ins run the fake claude in a fresh HOME.
func signinHost(t *testing.T, mode string) (*Server, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake, err := filepath.Abs("../signin/testdata")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	if mode != "" {
		os.WriteFile(filepath.Join(home, "fake-mode"), []byte(mode), 0o600)
	}
	dir := t.TempDir()
	s := newServer(t, Config{AllowLogins: []string{"me@example.com"}, HostName: "desk", StateDir: dir, AgentToken: testToken,
		Signin: signin.Config{ResultWait: 2 * time.Second}}, nil, nil)
	t.Cleanup(s.signin.Close)
	return s, home, dir
}

func jsonOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not json: %q", w.Body)
	}
	return m
}

func signinWait(t *testing.T, s *Server, id string, want string) map[string]any {
	t.Helper()
	var m map[string]any
	for i := 0; i < 200; i++ {
		w := do(s, "GET", "/api/hosts/desk/signin/"+id, userHdr, "")
		m = jsonOf(t, w)
		if m["state"] == want {
			return m
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("state = %v, want %s", m["state"], want)
	return nil
}

func TestSigninRoutesHappyPathAndSentinelCode(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })

	s, home, dir := signinHost(t, "echo") // the child prints the code back
	var seen bytes.Buffer                 // every answer the routes gave

	w := do(s, "POST", "/api/hosts/desk/signin", userHdr, `{"agent":"claude"}`)
	seen.Write(w.Body.Bytes())
	if w.Code != 200 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	start := jsonOf(t, w)
	id := start["id"].(string)
	if start["state"] != "waiting_code" || !strings.HasPrefix(start["url"].(string), "https://claude.com/") {
		t.Fatalf("start = %v", start)
	}

	// Wrong code: a sentinel that must be found nowhere afterwards.
	const sentinel = "SENTINEL-CODE-7c1e9a"
	w = do(s, "POST", "/api/hosts/desk/signin/"+id+"/code", userHdr, `{"code":"`+sentinel+`"}`)
	seen.Write(w.Body.Bytes())
	if w.Code != 200 || jsonOf(t, w)["state"] != "checking" {
		t.Fatalf("code = %d %s", w.Code, w.Body)
	}
	m := signinWait(t, s, id, "failed")
	b, _ := json.Marshal(m)
	seen.Write(b)
	if m["reason"] != "code_rejected" {
		t.Fatalf("result = %v", m)
	}
	// A second code is refused, without echoing it.
	w = do(s, "POST", "/api/hosts/desk/signin/"+id+"/code", userHdr, `{"code":"`+sentinel+`"}`)
	seen.Write(w.Body.Bytes())
	if w.Code != http.StatusConflict {
		t.Fatalf("second code = %d", w.Code)
	}
	// A bad shape, too.
	w = do(s, "POST", "/api/hosts/desk/signin/"+id+"/code", userHdr, `{"code":"`+sentinel+`\n;"}`)
	seen.Write(w.Body.Bytes())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad code = %d", w.Code)
	}
	// A body that is not JSON, with the code in it.
	w = do(s, "POST", "/api/hosts/desk/signin/"+id+"/code", userHdr, sentinel)
	seen.Write(w.Body.Bytes())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("not json = %d", w.Code)
	}
	if do(s, "DELETE", "/api/hosts/desk/signin/"+id, userHdr, "").Code != 200 {
		t.Fatal("delete")
	}

	// Nowhere: not in an answer, the log, any state file, or the child's home.
	if strings.Contains(seen.String(), sentinel) {
		t.Errorf("the code is in an answer: %s", seen.String())
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Errorf("the code is in the log: %s", logs.String())
	}
	for _, root := range []string{dir, home} {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), sentinel) {
					t.Errorf("the code is in %s", p)
				}
			}
			return nil
		})
	}
}

func TestSigninRoutesSignedIn(t *testing.T) {
	s, home, _ := signinHost(t, "")
	start := jsonOf(t, do(s, "POST", "/api/hosts/desk/signin", userHdr, ``))
	id := start["id"].(string)
	if start["agent"] != "claude" {
		t.Fatalf("default agent: %v", start)
	}
	// A second start attaches.
	w := do(s, "POST", "/api/hosts/desk/signin", userHdr, `{"agent":"claude"}`)
	if w.Code != http.StatusConflict || jsonOf(t, w)["session"].(map[string]any)["id"] != id {
		t.Fatalf("second start = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/hosts/desk/signin/"+id+"/code", userHdr, `{"code":"GOOD-CODE"}`); w.Code != 200 {
		t.Fatalf("code = %d %s", w.Code, w.Body)
	}
	m := signinWait(t, s, id, "signed_in")
	if m["method"] != "claude.ai" {
		t.Fatalf("result = %v", m)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "fake-ok")); err != nil {
		t.Fatal(err)
	}
}

func TestSigninRoutesCancel(t *testing.T) {
	s, _, _ := signinHost(t, "")
	id := jsonOf(t, do(s, "POST", "/api/hosts/desk/signin", userHdr, `{}`))["id"].(string)
	w := do(s, "DELETE", "/api/hosts/desk/signin/"+id, userHdr, "")
	if w.Code != 200 || jsonOf(t, w)["reason"] != "cancelled" {
		t.Fatalf("cancel = %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/hosts/desk/signin/nope", userHdr, ""); w.Code != 404 {
		t.Fatalf("unknown = %d", w.Code)
	}
}

func TestSigninRoutesRefusals(t *testing.T) {
	s, _, _ := signinHost(t, "")
	if w := do(s, "POST", "/api/hosts/desk/signin", userHdr, `{"agent":"pi"}`); w.Code != 400 {
		t.Errorf("unknown agent = %d", w.Code)
	}
	if w := do(s, "POST", "/api/hosts/elsewhere/signin", userHdr, `{}`); w.Code != 404 {
		t.Errorf("unknown host = %d", w.Code)
	}
	if w := do(s, "POST", "/api/hosts/desk/signin", nil, `{}`); w.Code != 401 {
		t.Errorf("no login = %d", w.Code)
	}
	if w := do(s, "POST", "/api/hosts/desk/signin", userHdr, strings.Repeat("x", 4096)); w.Code != 400 {
		t.Errorf("huge body = %d", w.Code)
	}
}

func TestSigninNotInstalled(t *testing.T) {
	s, _, _ := signinHost(t, "")
	t.Setenv("PATH", t.TempDir())
	w := do(s, "POST", "/api/hosts/desk/signin", userHdr, `{}`)
	if w.Code != http.StatusUnprocessableEntity || jsonOf(t, w)["code"] != "not_installed" {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
}

func TestSigninUserOnly(t *testing.T) {
	s, _, _ := signinHost(t, "")
	id := jsonOf(t, do(s, "POST", "/api/hosts/desk/signin", userHdr, `{}`))["id"].(string)
	routes := []struct{ method, path, body string }{
		{"POST", "/api/hosts/desk/signin", `{}`},
		{"GET", "/api/hosts/desk/signin/" + id, ""},
		{"POST", "/api/hosts/desk/signin/" + id + "/code", `{"code":"GOOD-CODE"}`},
		{"DELETE", "/api/hosts/desk/signin/" + id, ""},
	}
	tagged := func(method, path, body string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "100.64.0.9:1"
		r = r.WithContext(context.WithValue(r.Context(), taggedKey{}, tagged{peer: "worker-3", node: "worker-3", tag: "tag:worker"}))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w.Code
	}
	fwd := map[string]string{HeaderAgentToken: testToken, HeaderForwarded: "1", HeaderAgent: "w1:p1", HeaderAgentHost: "kube"}
	for _, rt := range routes {
		if w := do(s, rt.method, rt.path, agentHdr("w1:p1"), rt.body); w.Code != 403 {
			t.Errorf("agent %s %s = %d", rt.method, rt.path, w.Code)
		}
		if w := do(s, rt.method, rt.path, fwd, rt.body); w.Code != 403 {
			t.Errorf("forwarded %s %s = %d", rt.method, rt.path, w.Code)
		}
		if c := tagged(rt.method, rt.path, rt.body); c != 403 {
			t.Errorf("tagged %s %s = %d", rt.method, rt.path, c)
		}
	}
	// Nothing the refused calls did touched the session.
	if m := jsonOf(t, do(s, "GET", "/api/hosts/desk/signin/"+id, userHdr, "")); m["state"] != "waiting_code" {
		t.Errorf("state = %v", m["state"])
	}
}

// The host's settings answer carries each installed agent's sign-in state and method (no account), probed with
// `claude auth status`, and is fresh right after a sign-in.
func TestSettingsAgentAuth(t *testing.T) {
	s, _, _ := signinHost(t, "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	v := s.settingsView()
	a, ok := v.AgentAuth["claude"]
	if !ok || a.State != signin.AuthSignedOut || !a.CanSignIn {
		t.Fatalf("before: %+v (installed %v)", v.AgentAuth, v.InstalledKinds)
	}
	snap, err := s.signin.Start(context.Background(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.signin.SendCode(snap.ID, "GOOD-CODE"); err != nil {
		t.Fatal(err)
	}
	signinWait(t, s, snap.ID, "signed_in")
	after := s.settingsView().AgentAuth["claude"]
	if after.State != signin.AuthSignedIn || after.Method != "claude.ai" {
		t.Fatalf("after: %+v", after)
	}
	b, _ := json.Marshal(s.settingsView().AgentAuth)
	if strings.Contains(string(b), "@") {
		t.Fatalf("an account in the answer: %s", b)
	}
}
