package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jhjaggars/comebye/web"
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

// Every API response, a rejected one included, names the binary version.
func TestComebyeVersionHeader(t *testing.T) {
	s := newServer(t, Config{AllowLogins: []string{"me@example.com"}, HostName: "desk", Version: "v9.9.9"}, nil, nil)
	for _, p := range []string{"/api/version", "/api/agents", "/api/hosts"} {
		if got := do(s, "GET", p, nil, "").Header().Get("X-Comebye-Version"); got != "v9.9.9" {
			t.Fatalf("%s (no login, 401) X-Comebye-Version = %q", p, got)
		}
	}
	w := do(s, "GET", "/api/version", map[string]string{"Tailscale-User-Login": "me@example.com"}, "")
	if w.Code != 200 || w.Header().Get("X-Comebye-Version") != "v9.9.9" || w.Header().Get("X-App-Version") != s.version {
		t.Fatalf("200: %d %v", w.Code, w.Header())
	}
}

// The served sw.js carries the build version, so each release is a new
// worker with a cache of its own; its ETag is of the stamped bytes.
func TestServiceWorkerStamped(t *testing.T) {
	stamped := func(app string) (*staticHandler, string) {
		fsys := assets(app)
		fsys["sw.js"] = &fstest.MapFile{Data: []byte(`const VERSION = "__COMEBYE_VERSION__";`)}
		h, err := newStaticHandler(fsys)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/sw.js", nil))
		if w.Code != 200 || w.Body.String() != `const VERSION = "`+h.version+`";` {
			t.Fatalf("sw.js = %d %q, version %q", w.Code, w.Body, h.version)
		}
		return h, w.Header().Get("ETag")
	}
	h1, e1 := stamped("v1")
	h2, e2 := stamped("v2")
	if h1.version == h2.version || e1 == e2 {
		t.Fatalf("a new release kept sw.js: %q %q / %q %q", h1.version, h2.version, e1, e2)
	}
	r := httptest.NewRequest("GET", "/sw.js", nil)
	r.Header.Set("If-None-Match", e1)
	w := httptest.NewRecorder()
	h1.ServeHTTP(w, r)
	if w.Code != 304 {
		t.Fatalf("revalidating sw.js: %d", w.Code)
	}
	// The real embedded worker has the mark, and is stamped.
	real, err := newStaticHandler(web.FS)
	if err != nil {
		t.Fatal(err)
	}
	if b := string(real.files["sw.js"].data); strings.Contains(b, swVersionMark) || !strings.Contains(b, `"`+real.version+`"`) {
		t.Fatal("embedded sw.js is not stamped with the version")
	}
}
