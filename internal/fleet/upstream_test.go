package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamHardening(t *testing.T) {
	hits := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
		case "/big":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`"` + strings.Repeat("x", 9<<20) + `"`))
		case "/forbidden-text":
			http.Error(w, "nope", http.StatusForbidden)
		case "/forbidden-json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"code":"agent_forbidden"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"path":"` + r.URL.EscapedPath() + `"}`))
		}
	}))
	defer hub.Close()
	for _, bad := range []string{"http://hub.example.ts.net", "https://hub/x", "https://u:p@hub", "ftp://hub", "hub"} {
		if _, err := NewUpstream(bad, false, nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	u, err := NewUpstream(hub.URL, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	get := func(segs ...string) (int, string, error) {
		code, _, data, err := u.Do(ctx, UpstreamRequest{Method: "GET", Segments: segs})
		return code, string(data), err
	}
	if _, _, err := get("redirect"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect followed: %v", err)
	}
	if _, _, err := get("big"); err == nil {
		t.Error("oversized answer accepted")
	}
	if _, _, err := get("forbidden-text"); err == nil || !strings.Contains(err.Error(), "-ts-allow-login") {
		t.Errorf("rejection: %v", err)
	}
	if code, _, err := get("forbidden-json"); err != nil || code != 403 {
		t.Errorf("JSON 403 should pass through: %d %v", code, err)
	}
	// Segments are escaped: nothing climbs out of the path.
	if _, body, err := get("api", "../../etc", "x?y"); err != nil || body != `{"path":"/api/..%2F..%2Fetc/x%3Fy"}` {
		t.Errorf("escaping: %s %v", body, err)
	}
	n := hits
	if _, _, _, err := u.Do(ctx, UpstreamRequest{Method: "POST", Segments: []string{"x"}, Body: []byte("not json"), MaxBody: 100}); err == nil {
		t.Error("non-JSON body sent")
	}
	if _, _, _, err := u.Do(ctx, UpstreamRequest{Method: "POST", Segments: []string{"x"}, Body: []byte(`"` + strings.Repeat("x", 200) + `"`), MaxBody: 100}); err == nil {
		t.Error("oversized body sent")
	}
	if hits != n {
		t.Error("an invalid body reached the hub")
	}
}
