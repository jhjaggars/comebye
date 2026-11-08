package server

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/jhjaggars/comebye/internal/stt"
)

// Speech to text runs in the browser (web/stt-worker.js). This file is the
// host's part: it says whether the engine's files are installed and serves
// them. The files are 100 to 240 MB, so they are not in the binary: they
// are in <state dir>/stt/<version>/, put there by `comebye stt fetch`.

// sttDir is where the pinned files live, or "" without a state directory.
func (s *Server) sttDir() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	return stt.Dir(s.cfg.StateDir)
}

// handleSTT is GET /api/stt: whether voice input is installed here, and what
// the browser must download for each backend. The page asks on the first tap
// of a mic button, never at load.
func (s *Server) handleSTT(w http.ResponseWriter, r *http.Request) {
	installed, missing := false, []string(nil)
	if dir := s.sttDir(); dir != "" {
		installed, missing = stt.Status(dir)
	}
	out := map[string]any{
		"version":   stt.Version,
		"installed": installed,
		"base":      "/stt/" + stt.Version + "/",
		"bytes":     map[string]int64{"webgpu": stt.Bytes(stt.WebGPU), "wasm": stt.Bytes(stt.WASM)},
	}
	if !installed {
		out["missing"] = len(missing)
	}
	writeJSON(w, out)
}

// serveSTT serves GET /stt/<version>/<path>. Only the pinned files are ever
// served, by exact name: there is no directory listing, no path walking, and
// a name that is not in the list is 404 whatever is on disk. Like the
// other static files it needs no login (they are public upstream files, the
// same bytes for everyone); the Host check still applies.
//
// They never change under a version, so the browser may keep them for good:
// the ETag is the pinned SHA-256, the cache is immutable, and Range requests
// work (http.ServeContent). The runtime is application/wasm so the browser
// compiles it as it streams.
func (s *Server) serveSTT(w http.ResponseWriter, r *http.Request) {
	dir := s.sttDir()
	f := stt.FileByPath(r.PathValue("path"))
	if dir == "" || r.PathValue("version") != stt.Version || f == nil {
		http.NotFound(w, r)
		return
	}
	fh, err := os.Open(filepath.Join(dir, filepath.FromSlash(f.Path)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer fh.Close()
	if st, err := fh.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", sttContentType(f.Path))
	hd.Set("ETag", `"`+f.SHA256+`"`)
	hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	http.ServeContent(w, r, f.Path, time.Time{}, fh)
}

func sttContentType(name string) string {
	switch path.Ext(name) {
	case ".wasm":
		return "application/wasm"
	case ".mjs", ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json"
	}
	return "application/octet-stream"
}
