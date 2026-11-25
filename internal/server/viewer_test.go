package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func viewerFor(t *testing.T, s *Server, login string) string {
	t.Helper()
	hdr := map[string]string{}
	if login != "" {
		hdr["Tailscale-User-Login"] = login
	}
	w := do(s, "GET", "/api/version", hdr, "")
	var body struct{ Version, Viewer string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
		t.Fatalf("/api/version as %q: %d %s %v", login, w.Code, w.Body, err)
	}
	if len(body.Viewer) != 16 || strings.Trim(body.Viewer, "0123456789abcdef") != "" {
		t.Fatalf("viewer %q: want 16 hex digits", body.Viewer)
	}
	if body.Version != s.version {
		t.Fatalf("version %q, want %q", body.Version, s.version)
	}
	return body.Viewer
}

// The viewer is stable for a login (across requests and a restart on the same state directory), different between
// logins, and never the login or any part of it.
func TestVersionViewer(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AllowLogins: []string{"me@example.com", "you@example.com"}, StateDir: dir, NoLedger: true}
	s := newServer(t, cfg, nil, nil)
	me, you := viewerFor(t, s, "me@example.com"), viewerFor(t, s, "you@example.com")
	if me == you {
		t.Fatalf("two logins share the viewer %q", me)
	}
	if again := viewerFor(t, s, " Me@Example.com "); again != me {
		t.Fatalf("same login, other spelling: %q vs %q", again, me)
	}
	for _, v := range []string{me, you} {
		for _, part := range []string{"me", "you", "example", "com", "@"} {
			if strings.Contains(v, part) {
				t.Fatalf("viewer %q contains %q", v, part)
			}
		}
	}
	// The key lives in the state directory, private, so a restart keeps every viewer.
	st, err := os.Stat(filepath.Join(dir, viewerKeyFile))
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("viewer key file: %v %v", st, err)
	}
	if again := viewerFor(t, newServer(t, cfg, nil, nil), "me@example.com"); again != me {
		t.Fatalf("after a restart: %q vs %q", again, me)
	}
	// Another hub (another key) gives the same login another viewer.
	other := newServer(t, Config{AllowLogins: cfg.AllowLogins, StateDir: t.TempDir(), NoLedger: true}, nil, nil)
	if v := viewerFor(t, other, "me@example.com"); v == me {
		t.Fatalf("another hub's key gave the same viewer %q", v)
	}
	// A 401 says nothing about anyone.
	if w := do(s, "GET", "/api/version", map[string]string{"Tailscale-User-Login": "stranger@example.com"}, ""); w.Code != 401 || strings.Contains(w.Body.String(), "viewer") {
		t.Fatalf("not allowed: %d %s", w.Code, w.Body)
	}
}
