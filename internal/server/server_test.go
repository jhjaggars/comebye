package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testHosts admits httptest.NewRequest's default Host (example.com) for
// tests that go through Handler's Host check.
var testHosts = []string{"example.com"}

func newTest(allow ...string) *Server {
	return New(Config{AllowLogins: allow, ReadLines: 100}, nil, nil)
}

func do(s *Server, method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "100.64.0.1:1234"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

func TestStaticETag(t *testing.T) {
	s := newTest("me@example.com") // static assets must be reachable without auth
	w := do(s, "GET", "/", nil, "")
	if w.Code != 200 || w.Header().Get("ETag") == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("first: %d %v", w.Code, w.Header())
	}
	if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing security headers")
	}
	etag := w.Header().Get("ETag")
	w = do(s, "GET", "/", map[string]string{"If-None-Match": etag}, "")
	if w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("revalidate: %d", w.Code)
	}
	if w = do(s, "GET", "/", map[string]string{"If-None-Match": `"other"`}, ""); w.Code != 200 {
		t.Fatalf("stale etag: %d", w.Code)
	}
	if w = do(s, "GET", "/icon-192.png", nil, ""); !strings.Contains(w.Header().Get("Cache-Control"), "max-age") {
		t.Fatal("icon should have max-age")
	}
	if w = do(s, "GET", "/nope", nil, ""); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
	// Vendored libraries live in a subdirectory and are served like app.js.
	for _, p := range []string{"/vendor/marked.umd.js", "/vendor/purify.min.js"} {
		w = do(s, "GET", p, nil, "")
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") ||
			w.Header().Get("ETag") == "" || w.Header().Get("Cache-Control") != "no-cache" || w.Body.Len() < 10000 {
			t.Fatalf("%s: %d %v (%d bytes)", p, w.Code, w.Header(), w.Body.Len())
		}
	}
	if w = do(s, "GET", "/vendor/README.md", nil, ""); w.Code != 404 {
		t.Fatalf("vendor README should not be served: %d", w.Code)
	}
}

func TestOutputRejectsUnknownSource(t *testing.T) {
	s := newTest()
	// Validated before the agent lookup, so no herdr client is needed.
	if w := do(s, "GET", "/api/agents/wS:p1/output?source=detection", nil, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("source=detection: got %d want 400", w.Code)
	}
}

func TestKeysRejectsNonWhitelisted(t *testing.T) {
	s := newTest()
	for _, body := range []string{`{"keys":["a"]}`, `{"keys":["1","ctrl+x"]}`, `{"keys":["yes"]}`} {
		w := do(s, "POST", "/api/agents/wS:p1/keys", nil, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_key") {
			t.Fatalf("%s: got %d %q", body, w.Code, w.Body.String())
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	s := newTest("me@example.com")
	h := s.auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	check := func(login string, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/x", nil)
		if login != "" {
			r.Header.Set("Tailscale-User-Login", login)
		}
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != want {
			t.Fatalf("login %q: got %d want %d", login, w.Code, want)
		}
	}
	check("", 401)
	check("me@example.com", 204)
	if !newTest().authorized(httptest.NewRequest("GET", "/x", nil)) {
		t.Fatal("with no allowlist the API should be open")
	}
}

func TestTailscaleLoginAllowlist(t *testing.T) {
	s := newTest("me@example.com")
	cases := []struct {
		name, login string
		want        bool
	}{
		{"allowed", "me@example.com", true},
		{"case-insensitive", "Me@Example.com", true},
		{"other user", "other@example.com", false},
		{"no header", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/agents", nil)
		if c.login != "" {
			r.Header.Set("Tailscale-User-Login", c.login)
		}
		if got := s.authorized(r); got != c.want {
			t.Errorf("%s: authorized = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLegacyTokenCredentialsRejected(t *testing.T) {
	s := newTest("me@example.com")
	bearer := httptest.NewRequest("GET", "/api/agents", nil)
	bearer.Header.Set("Authorization", "Bearer secret")
	cookie := httptest.NewRequest("GET", "/api/agents", nil)
	cookie.AddCookie(&http.Cookie{Name: "herdrphone_token", Value: "secret"})
	query := httptest.NewRequest("GET", "/api/agents?token=secret", nil)
	for name, r := range map[string]*http.Request{"bearer": bearer, "cookie": cookie, "query": query} {
		if s.authorized(r) {
			t.Errorf("%s credential should not grant access without a Tailscale identity", name)
		}
	}
	for _, path := range []string{"/api/login", "/api/logout"} {
		if w := do(s, "POST", path, nil, `{"token":"secret"}`); w.Code == 204 || len(w.Result().Cookies()) != 0 {
			t.Errorf("%s should no longer exist: %d", path, w.Code)
		}
	}
}

func TestAllowlistRequiresLoopback(t *testing.T) {
	s := New(Config{Addr: "0.0.0.0:0", AllowLogins: []string{"me@example.com"}}, nil, nil)
	if err := s.ListenAndServe(); err == nil {
		t.Fatal("expected refusal to serve identity auth on a non-loopback address")
	}
	for addr, want := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true, "0.0.0.0:1": false, ":1": false} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestCSRFProtection(t *testing.T) {
	s := New(Config{AllowLogins: []string{"me@example.com"}, AllowHosts: testHosts, ReadLines: 100}, nil, nil)
	h := s.Handler()
	post := func(hdr map[string]string) int {
		r := httptest.NewRequest("POST", "/api/agents/x/keys", strings.NewReader(`{"keys":[]}`))
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := post(map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); c != http.StatusForbidden {
		t.Errorf("cross-site POST: got %d, want 403", c)
	}
	if c := post(map[string]string{"Content-Type": "text/plain"}); c != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST (no-cors form): got %d, want 415", c)
	}
	// Same-origin JSON passes the CSRF layer and reaches the handler, which
	// rejects the empty key list as a bad request.
	if c := post(map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}); c == http.StatusForbidden || c == http.StatusUnsupportedMediaType {
		t.Errorf("same-origin JSON POST was blocked: %d", c)
	}
}
