package stt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The pinned list is what the whole feature trusts: every entry must be a
// pinned https URL (Hugging Face by commit, never "main"), a SHA-256, a
// size and a path that stays inside the data directory.
func TestPinnedFiles(t *testing.T) {
	hexRE := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, f := range Files() {
		if !strings.HasPrefix(f.URL, "https://") {
			t.Errorf("%s: URL is not https: %s", f.Path, f.URL)
		}
		if strings.Contains(f.URL, "/resolve/main/") || strings.Contains(f.URL, "@latest") {
			t.Errorf("%s: URL is not pinned: %s", f.Path, f.URL)
		}
		if strings.Contains(f.URL, "huggingface.co") && !regexp.MustCompile(`/resolve/[0-9a-f]{40}/`).MatchString(f.URL) {
			t.Errorf("%s: Hugging Face URL must name a commit: %s", f.Path, f.URL)
		}
		if !hexRE.MatchString(f.SHA256) || f.Size <= 0 || !safeRel(f.Path) || seen[f.Path] {
			t.Errorf("bad entry %+v", f)
		}
		if path.Base(f.URL) != path.Base(f.Path) {
			t.Errorf("%s: URL file name differs: %s", f.Path, f.URL)
		}
		seen[f.Path] = true
	}
	// The sizes the app shows: WebGPU needs fp32 + q4 weights, WASM the int8 ones.
	if Bytes(WebGPU) <= Bytes(WASM) || Bytes(WASM) < 50e6 || Bytes(WebGPU) > Total() {
		t.Fatalf("sizes: webgpu %d wasm %d total %d", Bytes(WebGPU), Bytes(WASM), Total())
	}
	if FileByPath("../etc/passwd") != nil || FileByPath("models") != nil {
		t.Fatal("FileByPath must match exact pinned names only")
	}
}

// serve returns a server with the given body per URL path, and the files
// that fetch them.
func serve(t *testing.T, bodies map[string]string) (*httptest.Server, []File) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(b))
	}))
	t.Cleanup(srv.Close)
	var list []File
	for p, b := range bodies {
		sum := sha256.Sum256([]byte(b))
		list = append(list, File{Path: strings.TrimPrefix(p, "/"), URL: srv.URL + p, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))})
	}
	return srv, list
}

func TestFetchInstallsAndSkips(t *testing.T) {
	_, list := serve(t, map[string]string{"/a/one.bin": "first", "/two.json": "second"})
	dir := t.TempDir()
	var log []string
	if err := Fetch(context.Background(), nil, dir, list, func(m string) { log = append(log, m) }); err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path))); err != nil || int64(len(b)) != f.Size {
			t.Fatalf("%s not installed: %v", f.Path, err)
		}
		if !fileOK(filepath.Join(dir, filepath.FromSlash(f.Path)), f) {
			t.Fatalf("%s fails its own check", f.Path)
		}
	}
	// A second run downloads nothing: the server is gone and it still succeeds.
	for i := range list {
		list[i].URL = "http://127.0.0.1:1/never"
	}
	log = nil
	if err := Fetch(context.Background(), nil, dir, list, func(m string) { log = append(log, m) }); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(log) != 2 || !strings.Contains(log[0], "already installed") {
		t.Fatalf("log: %v", log)
	}
}

// A download that is not the pinned file is refused: nothing is installed,
// no partial file is left, and the error says which file and why.
func TestFetchRefusesChecksumMismatch(t *testing.T) {
	srv, list := serve(t, map[string]string{"/model.onnx": "the real bytes"})
	list[0].SHA256 = strings.Repeat("0", 64) // pinned hash says something else
	dir := t.TempDir()
	err := Fetch(context.Background(), srv.Client(), dir, list, nil)
	var ce *ChecksumError
	if !errors.As(err, &ce) || ce.Path != "model.onnx" || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("want a ChecksumError, got %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

func TestFetchRefusesWrongSize(t *testing.T) {
	srv, list := serve(t, map[string]string{"/m.bin": "0123456789"})
	list[0].Size = 4 // longer than pinned: stops reading and refuses
	if err := Fetch(context.Background(), srv.Client(), t.TempDir(), list, nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("too long: %v", err)
	}
	list[0].Size = 40 // shorter than pinned
	dir := t.TempDir()
	if err := Fetch(context.Background(), srv.Client(), dir, list, nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("too short: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.bin")); !os.IsNotExist(err) {
		t.Fatalf("installed despite the mismatch: %v", err)
	}
}

// A corrupt file already on disk is replaced, not trusted because it exists.
func TestFetchReplacesCorruptFile(t *testing.T) {
	srv, list := serve(t, map[string]string{"/m.bin": "good bytes"})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.bin"), []byte("evil bytes"), 0o644); err != nil { // same size
		t.Fatal(err)
	}
	if err := Fetch(context.Background(), srv.Client(), dir, list, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "m.bin")); string(b) != "good bytes" {
		t.Fatalf("got %q", b)
	}
}

func TestFetchHTTPErrorAndUnsafePath(t *testing.T) {
	srv, list := serve(t, map[string]string{"/m.bin": "x"})
	list[0].URL = srv.URL + "/missing"
	if err := Fetch(context.Background(), srv.Client(), t.TempDir(), list, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: %v", err)
	}
	for _, p := range []string{"../escape", "/abs", "a/../../b", ""} {
		if err := Fetch(context.Background(), srv.Client(), t.TempDir(), []File{{Path: p, URL: srv.URL + "/m.bin"}}, nil); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
}

func TestStatusAndVerify(t *testing.T) {
	dir := t.TempDir()
	if ok, missing := Status(dir); ok || len(missing) != len(Files()) {
		t.Fatalf("empty dir: %v %d", ok, len(missing))
	}
	// Right sizes, wrong content: Status (sizes) says installed, Verify (hashes) does not.
	for _, f := range Files() {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		fh, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := fh.Truncate(f.Size); err != nil {
			t.Fatal(err)
		}
		fh.Close()
	}
	if ok, missing := Status(dir); !ok || len(missing) != 0 {
		t.Fatalf("sized files: %v %v", ok, missing)
	}
	if bad := Verify(dir); len(bad) != len(Files()) {
		t.Fatalf("Verify found %d bad, want %d", len(bad), len(Files()))
	}
}
