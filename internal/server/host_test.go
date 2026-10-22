package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jhjaggars/herdr-phone/internal/transcript"
)

// A rebinding page (evil.example re-resolved to 127.0.0.1) is refused before
// auth, even though any local client can set a valid identity header; the
// names Serve and local tools use get through.
func TestHostCheckBlocksRebinding(t *testing.T) {
	s := New(Config{AllowLogins: []string{"me@example.com"}, AllowHosts: SplitHosts("Phone.Lan:8741, other.lan"), ReadLines: 100}, nil, nil)
	h := s.Handler()
	get := func(host, path string) int {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Host = host
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, host := range []string{"evil.example:8741", "evil.example", "127.0.0.1.evil.example", "ts.net.evil.example", "", "example.com", "10.0.0.5:8741"} {
		for _, path := range []string{"/api/version", "/"} {
			if c := get(host, path); c != http.StatusMisdirectedRequest {
				t.Errorf("Host %q %s: got %d, want 421", host, path, c)
			}
		}
	}
	for _, host := range []string{"localhost:8741", "LOCALHOST", "127.0.0.1:8741", "[::1]:8741", "box.tailnet.ts.net", "Box.Tailnet.TS.NET.", "phone.lan", "other.lan:1"} {
		if c := get(host, "/api/version"); c != http.StatusOK {
			t.Errorf("Host %q: got %d, want 200", host, c)
		}
	}
}

func TestListenRequiresAllowlistOrInsecureFlag(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}, nil, nil)
	err := s.ListenAndServe()
	if err == nil || !strings.Contains(err.Error(), "insecure-no-auth") {
		t.Fatalf("expected refusal without an allowlist, got %v", err)
	}
}

// API responses default to nosniff + no-store; the transcript keeps its
// ETag revalidation (no-cache), which the browser relies on.
func TestAPIResponseHeaders(t *testing.T) {
	s := newTest()
	w := do(s, "GET", "/api/version", nil, "")
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("version headers: %v", w.Header())
	}
	path := writeSession(t, piMsg("u1", "user", "hi"))
	h := s.auth(func(w http.ResponseWriter, r *http.Request) { s.serveTranscript(w, r, path, transcript.Read) })
	w = httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/api/agents/x/transcript", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" {
		t.Fatalf("transcript: %d %v", w.Code, w.Header())
	}
}

func TestEventStreamsCapped(t *testing.T) {
	s := newTest()
	s.streams.Store(maxStreams)
	if w := do(s, "GET", "/api/events", nil, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("over the cap: %d", w.Code)
	}
	if n := s.streams.Load(); n != maxStreams {
		t.Fatalf("refused stream leaked a slot: %d", n)
	}
}
