package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/stt"
)

// installSTT writes stand-in files of the pinned sizes (sparse), as
// `comebye stt fetch` would have left them.
func installSTT(t *testing.T, state string) {
	t.Helper()
	for _, f := range stt.Files() {
		p := filepath.Join(stt.Dir(state), filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		fh, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.WriteString("x"); err != nil {
			t.Fatal(err)
		}
		if err := fh.Truncate(f.Size); err != nil {
			t.Fatal(err)
		}
		fh.Close()
	}
}

func sttServer(state string) *Server {
	return New(Config{StateDir: state, ReadLines: 100}, nil, nil)
}

func TestSTTStatusNotInstalledThenInstalled(t *testing.T) {
	state := t.TempDir()
	s := sttServer(state)
	w := do(s, "GET", "/api/stt", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"installed":false`) || !strings.Contains(w.Body.String(), `"base":"/stt/`+stt.Version+`/"`) {
		t.Fatalf("not installed: %d %s", w.Code, w.Body)
	}
	installSTT(t, state)
	w = do(s, "GET", "/api/stt", nil, "")
	if !strings.Contains(w.Body.String(), `"installed":true`) || !strings.Contains(w.Body.String(), `"webgpu":`) {
		t.Fatalf("installed: %s", w.Body)
	}
	// The user's alone: with a login allowlist an unknown caller is refused.
	if w := do(New(Config{StateDir: state, AllowLogins: []string{"me@example.com"}, ReadLines: 100}, nil, nil), "GET", "/api/stt", nil, ""); w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/stt: %d", w.Code)
	}
}

func TestSTTRouteHeaders(t *testing.T) {
	state := t.TempDir()
	installSTT(t, state)
	s := sttServer(state)
	base := "/stt/" + stt.Version + "/"
	wasm := stt.FileByPath("ort/ort-wasm-simd-threaded.asyncify.wasm")
	w := do(s, "GET", base+wasm.Path, nil, "")
	h := w.Header()
	if w.Code != 200 || int64(w.Body.Len()) != wasm.Size {
		t.Fatalf("wasm: %d, %d bytes", w.Code, w.Body.Len())
	}
	if h.Get("Content-Type") != "application/wasm" {
		t.Errorf("Content-Type %q, want application/wasm", h.Get("Content-Type"))
	}
	if h.Get("ETag") != `"`+wasm.SHA256+`"` {
		t.Errorf("ETag %q", h.Get("ETag"))
	}
	if cc := h.Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("Cache-Control %q", cc)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff")
	}
	// Revalidation by ETag is a 304 with no body.
	if w = do(s, "GET", base+wasm.Path, map[string]string{"If-None-Match": `"` + wasm.SHA256 + `"`}, ""); w.Code != 304 || w.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d, %d bytes", w.Code, w.Body.Len())
	}
	// Range works, so an interrupted download can resume.
	if w = do(s, "GET", base+wasm.Path, map[string]string{"Range": "bytes=0-9"}, ""); w.Code != 206 || w.Body.Len() != 10 {
		t.Errorf("Range: %d, %d bytes", w.Code, w.Body.Len())
	}
	// HEAD has the headers and no body.
	if w = do(s, "HEAD", base+wasm.Path, nil, ""); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d %v", w.Code, w.Header())
	}
	for path, want := range map[string]string{
		"ort/ort-wasm-simd-threaded.asyncify.mjs":                  "text/javascript; charset=utf-8",
		"models/whisper-base.en/config.json":                       "application/json",
		"models/whisper-base.en/onnx/decoder_model_merged_q4.onnx": "application/octet-stream",
	} {
		if w = do(s, "GET", base+path, nil, ""); w.Code != 200 || w.Header().Get("Content-Type") != want {
			t.Errorf("%s: %d %q, want %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
}

// Only the versioned path and the pinned names are served: no listing, no
// other version, no walking out, nothing else that happens to be on disk.
func TestSTTRouteServesOnlyPinnedFiles(t *testing.T) {
	state := t.TempDir()
	installSTT(t, state)
	if err := os.WriteFile(filepath.Join(stt.Dir(state), "secret.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "ledger.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := sttServer(state)
	base := "/stt/" + stt.Version + "/"
	for _, p := range []string{
		"/stt/", "/stt", base, base + "models/", base + "models", base + "models/whisper-base.en/", base + "ort/", // no listing
		"/stt/base.en-v0/" + "models/whisper-base.en/config.json", // another version
		"/stt/latest/ort/ort-wasm-simd-threaded.asyncify.wasm",
		base + "secret.txt",                                                                            // on disk, not pinned
		base + "../../ledger.json", base + "..%2F..%2Fledger.json", base + "%2e%2e/%2e%2e/ledger.json", // walking out
		base + "models/whisper-base.en/../../secret.txt",
		base + "models/whisper-base.en/config.json/",
	} {
		w := do(s, "GET", p, nil, "")
		if w.Code == 200 || strings.Contains(w.Body.String(), "ledger") && w.Code == 200 {
			t.Errorf("%s: served (%d)", p, w.Code)
		}
		if w.Code != 404 && w.Code != 301 && w.Code != 307 && w.Code != 308 && w.Code != 400 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := do(s, "POST", base+"ort/ort-wasm-simd-threaded.asyncify.wasm", nil, ""); w.Code == 200 {
		t.Errorf("POST served")
	}
	// A truncated file (a half-finished fetch by hand) is not served.
	f := stt.Files()[0]
	if err := os.Truncate(filepath.Join(stt.Dir(state), filepath.FromSlash(f.Path)), 5); err != nil {
		t.Fatal(err)
	}
	if w := do(s, "GET", base+f.Path, nil, ""); w.Code != 404 {
		t.Errorf("truncated file: %d", w.Code)
	}
}

func TestSTTRouteWithoutStateDirOrFiles(t *testing.T) {
	base := "/stt/" + stt.Version + "/ort/ort-wasm-simd-threaded.asyncify.wasm"
	if w := do(newTest(), "GET", base, nil, ""); w.Code != 404 {
		t.Errorf("no state dir: %d", w.Code)
	}
	if w := do(sttServer(t.TempDir()), "GET", base, nil, ""); w.Code != 404 {
		t.Errorf("not installed: %d", w.Code)
	}
}

// The page may compile WebAssembly (the speech runtime) and nothing else
// new: no 'unsafe-eval', no blob:, no remote hosts. And nothing sent blocks
// the microphone.
func TestCSPAllowsWasmOnly(t *testing.T) {
	w := do(newTest(), "GET", "/", nil, "")
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'wasm-unsafe-eval';") {
		t.Fatalf("script-src: %s", csp)
	}
	for _, bad := range []string{"'unsafe-eval'", "blob:", "http:", "https:", "*"} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP contains %s: %s", bad, csp)
		}
	}
	for _, p := range []string{"/", "/app.js", "/stt-worker.js", "/api/version"} {
		w := do(newTest(), "GET", p, nil, "")
		if pp := w.Header().Get("Permissions-Policy"); pp != "" && !strings.Contains(pp, "microphone=(self)") && !strings.Contains(pp, "microphone=*") {
			t.Errorf("%s: Permissions-Policy %q blocks the microphone", p, pp)
		}
		if w.Header().Get("Feature-Policy") != "" {
			t.Errorf("%s: Feature-Policy set", p)
		}
	}
}
