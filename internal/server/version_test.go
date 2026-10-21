package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jhjaggars/herdr-phone/web"
)

func assets(app string) fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte("<html>")},
		"app.js":               {Data: []byte(app)},
		"sw.js":                {Data: []byte("// sw")},
		"manifest.webmanifest": {Data: []byte("{}")},
		"icon-192.png":         {Data: []byte("png")},
		"vendor/marked.umd.js": {Data: []byte("// marked")},
		"vendor/purify.min.js": {Data: []byte("// purify")},
	}
}

func staticVersion(t *testing.T, fsys fstest.MapFS) string {
	t.Helper()
	h, err := newStaticHandler(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return h.version
}

func TestAssetVersionStable(t *testing.T) {
	a, b := staticVersion(t, assets("v1")), staticVersion(t, assets("v1"))
	if a == "" || a != b {
		t.Fatalf("identical assets: %q vs %q", a, b)
	}
	if c := staticVersion(t, assets("v2")); c == a {
		t.Fatalf("changed app.js kept version %q", c)
	}
	icon := assets("v1")
	icon["icon-192.png"] = &fstest.MapFile{Data: []byte("other png")}
	if c := staticVersion(t, icon); c != a {
		t.Fatalf("icon change bumped the version: %q vs %q", c, a)
	}
	// A vendored library bump must reload open pages like an app.js change.
	lib := assets("v1")
	lib["vendor/purify.min.js"] = &fstest.MapFile{Data: []byte("// purify 2")}
	if c := staticVersion(t, lib); c == a {
		t.Fatalf("vendor change kept version %q", c)
	}
	// The real embedded assets give the server's version, every time.
	s1, s2 := newTest(), newTest()
	real, err := newStaticHandler(web.FS)
	if err != nil {
		t.Fatal(err)
	}
	if s1.version == "" || s1.version != s2.version || s1.version != real.version {
		t.Fatalf("server versions %q %q %q", s1.version, s2.version, real.version)
	}
}

func TestVersionExposed(t *testing.T) {
	s := newTest()
	w := do(s, "GET", "/api/version", nil, "")
	var body struct{ Version string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.Version != s.version {
		t.Fatalf("/api/version: %d %s %v", w.Code, w.Body, err)
	}
	for _, p := range []string{"/api/version", "/api/agents", "/api/hosts"} {
		if got := do(s, "GET", p, nil, "").Header().Get("X-App-Version"); got != s.version {
			t.Fatalf("%s X-App-Version = %q, want %q", p, got, s.version)
		}
	}
	// A rejected request still says which version is serving.
	if got := do(newTest("me@example.com"), "GET", "/api/agents", nil, "").Header().Get("X-App-Version"); got != s.version {
		t.Fatalf("401 X-App-Version = %q", got)
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if first := firstSSEData(t, srv.URL); !strings.Contains(first, `"version":"`+s.version+`"`) {
		t.Fatalf("snapshot = %s", first)
	}
}

// firstSSEData returns the first data line of the /api/events stream.
func firstSSEData(t *testing.T, base string) string {
	t.Helper()
	r, _ := http.NewRequest("GET", base+"/api/events", nil)
	r.Header.Set("Tailscale-User-Login", "me@example.com")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			return d
		}
	}
	return ""
}
