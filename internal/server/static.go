package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// contentSecurityPolicy is conservative: same-origin scripts only, inline
// styles allowed (index.html has an inline <style> and style attributes).
// 'wasm-unsafe-eval' is for the speech-to-text runtime (WebAssembly, in a
// worker); it permits compiling WebAssembly and nothing else: JavaScript
// eval and new Function stay blocked.
const contentSecurityPolicy = "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; manifest-src 'self'; worker-src 'self'; " +
	"frame-src 'self'%s; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// appCSP is the app's policy; pagesOrigin, when pages are served on their
// own origin, is the one other origin the app may frame.
func appCSP(pagesOrigin string) string {
	if pagesOrigin != "" {
		pagesOrigin = " " + pagesOrigin
	}
	return fmt.Sprintf(contentSecurityPolicy, pagesOrigin)
}

type staticFile struct {
	data  []byte
	etag  string
	ctype string
	cache string
}

// staticHandler serves embedded files with content-hash ETags, since
// embed.FS has zero modtimes and http.FileServer would emit no validators.
type staticHandler struct {
	csp   string // the Content-Security-Policy to send
	files map[string]*staticFile
	// version identifies this build of the app (see assetVersion).
	version string
}

// versionFiles are the assets whose content defines the app version: what
// a long-lived page (an iOS Home Screen app survives deploys in memory)
// must reload to pick up. Icons are left out; the vendored libraries are
// in, so a library bump reloads open pages too.
var versionFiles = []string{"index.html", "app.js", "keys.js", "sidepanel.js", "themes.js", "sw.js", "manifest.webmanifest",
	"vendor/marked.umd.js", "vendor/purify.min.js", "stt.js", "stt-worker.js", "vendor/transformers.bundle.js"}

// assetVersion combines the per-file ETags of versionFiles into a short
// stable hash: identical assets always give the same version.
func assetVersion(files map[string]*staticFile) string {
	h := sha256.New()
	for _, name := range versionFiles {
		etag := ""
		if f := files[name]; f != nil {
			etag = f.etag
		}
		fmt.Fprintf(h, "%s=%s\n", name, etag)
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}

func newStaticHandler(fsys fs.FS) (*staticHandler, error) {
	h := &staticHandler{files: map[string]*staticFile{}, csp: appCSP("")}
	// Walk subdirectories too (vendor/); names are slash paths like
	// "vendor/marked.umd.js", matching the request path.
	err := fs.WalkDir(fsys, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		f := &staticFile{data: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, cache: "no-cache"}
		switch path.Ext(name) {
		case ".html":
			f.ctype = "text/html; charset=utf-8"
		case ".js":
			f.ctype = "text/javascript; charset=utf-8"
		case ".css":
			f.ctype = "text/css; charset=utf-8"
		case ".webmanifest":
			f.ctype = "application/manifest+json"
		case ".svg":
			f.ctype = "image/svg+xml"
			f.cache = "public, max-age=86400"
		case ".png":
			f.ctype = "image/png"
			f.cache = "public, max-age=86400"
		case ".txt":
			f.ctype = "text/plain; charset=utf-8"
		default:
			f.ctype = http.DetectContentType(b)
		}
		h.files[name] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.version = assetVersion(h.files)
	return h, nil
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	f, ok := h.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Content-Security-Policy", h.csp)
	hd.Set("Content-Type", f.ctype)
	hd.Set("Cache-Control", f.cache)
	hd.Set("ETag", f.etag)
	if etagMatches(r.Header.Get("If-None-Match"), f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(f.data)
}

func etagMatches(header, etag string) bool {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimPrefix(strings.TrimSpace(p), "W/")
		if p == "*" || p == etag {
			return true
		}
	}
	return false
}

// diskStatic serves the app from a directory, re-reading it on every
// request, so an edit to web/ shows on reload without a rebuild. It is the
// dev harness's (tools/uitest/dev.sh, COMEBYE_DEV_WEB_DIR); a release serves
// the embedded copy.
type diskStatic struct{ dir, csp string }

func (d diskStatic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, err := newStaticHandler(os.DirFS(d.dir))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.csp = d.csp
	h.ServeHTTP(w, r)
}
