package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/testruns"
)

// A finished test run's log: served by the test VM as plain text, at most
// testruns.MaxLog bytes, to the user alone (an agent is refused, as for the
// host's other reads), and proxied by the hub to the VM under the same rule.
func TestTestRunLog(t *testing.T) {
	vmDir := t.TempDir()
	dd := testruns.DoneDir(vmDir)
	if err := os.MkdirAll(dd, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dd, "desk-20261124-101500-1.log"), []byte("ok  ./internal/x\nFAIL ./internal/y\n"), 0o644)
	os.WriteFile(filepath.Join(dd, "desk-20261124-101500-2.log"), []byte(strings.Repeat("y\n", testruns.MaxLog)+"last line\n"), 0o644)
	os.WriteFile(filepath.Join(vmDir, "secret.log"), []byte("not a run's"), 0o644)

	// The VM itself: the user's route.
	vm := newServer(t, Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "kv-a", StateDir: vmDir, AgentToken: testToken}, nil, nil)
	w := do(vm, "GET", "/api/test-runs/desk-20261124-101500-1/log", userHdr, "")
	if w.Code != 200 || w.Body.String() != "ok  ./internal/x\nFAIL ./internal/y\n" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("log: %d %q %v", w.Code, w.Body, w.Header())
	}
	w = do(vm, "GET", "/api/test-runs/desk-20261124-101500-2/log", userHdr, "")
	if w.Code != 200 || w.Body.Len() > testruns.MaxLog || !strings.HasSuffix(w.Body.String(), "last line\n") {
		t.Fatalf("big log: %d, %d bytes", w.Code, w.Body.Len())
	}
	for _, id := range []string{"nope", "..%2Fsecret", "%2E%2E", "UPPER"} {
		if w := do(vm, "GET", "/api/test-runs/"+id+"/log", userHdr, ""); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "not a run") {
			t.Errorf("%s: %d %s", id, w.Code, w.Body)
		}
	}
	if w := do(vm, "GET", "/api/test-runs/desk-20261124-101500-1/log", agentHdr("w1:p1"), ""); w.Code != http.StatusForbidden {
		t.Errorf("agent: %d %s", w.Code, w.Body)
	}
	if w := do(vm, "GET", "/api/test-runs/desk-20261124-101500-1/log", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", w.Code)
	}

	// Through the hub: the VM is a peer (one that lets the hub in), and the hub's own rule applies first.
	open := newServer(t, Config{ReadLines: 100, HostName: "kv-a", StateDir: vmDir}, nil, nil)
	var asked []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		open.Handler().ServeHTTP(w, r)
	}))
	defer peer.Close()
	pu, _ := url.Parse(peer.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kv-a", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	hub := newServer(t, Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), AgentToken: testToken, Fleet: fl}, nil, nil)
	w = do(hub, "GET", "/api/hosts/kv-a/test-runs/desk-20261124-101500-1/log", userHdr, "")
	if w.Code != 200 || w.Body.String() != "ok  ./internal/x\nFAIL ./internal/y\n" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("through the hub: %d %q %v", w.Code, w.Body, w.Header())
	}
	if len(asked) != 1 || asked[0] != "/api/test-runs/desk-20261124-101500-1/log" {
		t.Errorf("the peer was asked %v", asked)
	}
	w = do(hub, "GET", "/api/hosts/kv-a/test-runs/nope/log", userHdr, "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no_log") {
		t.Errorf("no such run, through the hub: %d %s", w.Code, w.Body)
	}
	n := len(asked)
	if w := do(hub, "GET", "/api/hosts/kv-a/test-runs/desk-20261124-101500-1/log", agentHdr("w1:p1"), ""); w.Code != http.StatusForbidden {
		t.Errorf("agent, through the hub: %d %s", w.Code, w.Body)
	}
	if w := do(hub, "GET", "/api/hosts/kv-a/test-runs/desk-20261124-101500-1/log", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous, through the hub: %d", w.Code)
	}
	if w := do(hub, "GET", "/api/hosts/kv-a/test-runs/a%2F..%2F..%2Fx/log", userHdr, ""); w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Errorf("a path in the id: %d", w.Code)
	}
	if len(asked) != n {
		t.Errorf("refused calls reached the peer: %v", asked[n:])
	}
	if w := do(hub, "GET", "/api/hosts/kv-zz/test-runs/desk-20261124-101500-1/log", userHdr, ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown host: %d", w.Code)
	}
}
