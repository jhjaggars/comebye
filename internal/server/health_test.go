package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

func healthServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg.StateDir = dir
	cfg.AllowLogins = []string{"me@example.com"}
	cfg.Version = "v-test"
	return New(cfg, herdr.NewClient(""), state.New(herdr.NewClient(""), &state.Notifier{})), dir
}

func getHealth(s *Server, path, host string) (int, map[string]any) {
	req := httptest.NewRequest("GET", path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestHealthzIsUnauthenticatedAnyHostAndHasNoSecrets(t *testing.T) {
	s, dir := healthServer(t, Config{HubOnly: true})
	// No Tailscale header, and a Host that checkHost would refuse.
	for _, path := range []string{"/healthz", "/readyz"} {
		code, body := getHealth(s, path, "10.42.0.7:8741")
		if code != 200 || body["status"] != "ok" || body["version"] != "v-test" || body["hub_only"] != true {
			t.Fatalf("%s: %d %v", path, code, body)
		}
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), dir) {
			t.Errorf("%s leaks the state dir: %s", path, raw)
		}
	}
	// ... while the API still refuses the same request.
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Host = "10.42.0.7:8741"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Errorf("/api/agents answered an unauthenticated request with a foreign Host")
	}
	// The probe leaves no file behind.
	if _, err := os.Stat(filepath.Join(dir, ".healthz")); err == nil {
		t.Error(".healthz left behind")
	}
}

func TestHealthzFailsWhenTheStateDirIsNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	s, dir := healthServer(t, Config{})
	now := time.Now()
	s.health.now = func() time.Time { return now }
	if code, _ := getHealth(s, "/healthz", "localhost"); code != 200 {
		t.Fatalf("writable dir: %d", code)
	}
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	// Cached: still ok within the interval.
	if code, _ := getHealth(s, "/healthz", "localhost"); code != 200 {
		t.Fatalf("within the cache interval: %d", code)
	}
	now = now.Add(2 * healthProbeEvery)
	code, body := getHealth(s, "/healthz", "localhost")
	if code != 503 || body["status"] != "fail" || body["checks"].(map[string]any)["state_dir"] != "fail" {
		t.Fatalf("read-only dir: %d %v", code, body)
	}
	os.Chmod(dir, 0o700)
	now = now.Add(2 * healthProbeEvery)
	if code, _ := getHealth(s, "/healthz", "localhost"); code != 200 {
		t.Fatalf("recovered: %d", code)
	}
}

func TestReadyzFollowsTheLockAndShutdown(t *testing.T) {
	var lockErr error
	s, _ := healthServer(t, Config{LockErr: func() error { return lockErr }})
	if code, _ := getHealth(s, "/readyz", "localhost"); code != 200 {
		t.Fatalf("ready: %d", code)
	}
	lockErr = errors.New("read-only file system")
	code, body := getHealth(s, "/readyz", "localhost")
	if code != 503 || body["checks"].(map[string]any)["lock"] != "fail" {
		t.Fatalf("lost lock: %d %v", code, body)
	}
	if code, _ := getHealth(s, "/healthz", "localhost"); code != 200 {
		t.Errorf("liveness must not depend on the lock: %d", code)
	}
	lockErr = nil
	s.ShuttingDown()
	if code, _ := getHealth(s, "/readyz", "localhost"); code != 503 {
		t.Errorf("shutting down: %d", code)
	}
	if code, _ := getHealth(s, "/healthz", "localhost"); code != 200 {
		t.Errorf("liveness while shutting down: %d", code)
	}
}

func TestHealthRejectsWrites(t *testing.T) {
	s, _ := healthServer(t, Config{})
	req := httptest.NewRequest("POST", "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestHealthHandlerServesOnlyProbes(t *testing.T) {
	s, _ := healthServer(t, Config{})
	h := s.HealthHandler()
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/api/agents": 404, "/": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}

// A hub without herdr: its own host is reported as hosting no agents, and
// anything that needs herdr says so instead of failing obscurely.
func TestHubOnlyHostAndHerdrCalls(t *testing.T) {
	s, _ := healthServer(t, Config{HubOnly: true})
	if _, err := s.client.ListAgents(t.Context()); !errors.Is(err, herdr.ErrNoHerdr) {
		t.Fatalf("ListAgents = %v", err)
	}
	rec := httptest.NewRecorder()
	writeError(rec, herdr.ErrNoHerdr)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_herdr") {
		t.Errorf("writeError: %d %s", rec.Code, rec.Body)
	}
}

// The browser's slim host view keeps no_agents, so the app can say "no agents here".
func TestHubOnlyHostReachesTheBrowser(t *testing.T) {
	hs := slimHosts([]state.HostStatus{{Name: "hub", Local: true, Status: "ok", NoAgents: true}})
	b, _ := json.Marshal(hs)
	if !strings.Contains(string(b), `"no_agents":true`) {
		t.Fatalf("slim host lacks no_agents: %s", b)
	}
}

// A normal instance (with a herdr client) is unchanged by the hub-only work:
// its host is not "no agents", the probes do not claim hub-only, and the
// herdr-backed routes still reach herdr.
func TestNormalHostIsUnchanged(t *testing.T) {
	s, _, _ := coordServer(t)
	w := do(s, "GET", "/api/hosts", userHdr, "")
	if strings.Contains(w.Body.String(), "no_agents") {
		t.Errorf("a normal host reports no_agents: %s", w.Body)
	}
	code, body := getHealth(s, "/healthz", "localhost")
	if code != 200 || body["hub_only"] != nil {
		t.Errorf("healthz = %d %v", code, body)
	}
	if w := do(s, "GET", "/api/launch/options", userHdr, ""); !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Errorf("launching is off on a normal host: %s", w.Body)
	}
}
