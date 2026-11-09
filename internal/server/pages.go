package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jhjaggars/comebye/web"
)

// Pages: reports an agent publishes (HTML or Markdown) for the user to read
// in the app, `comebye publish`. They live on the hub, in its state
// directory: pages/pages.json (the index) and pages/<id>/v<N>.{html,md}
// (one file per version, the last few kept).
//
// A page is agent-written code, so it is never trusted:
//   - /p/... answers with Content-Security-Policy: sandbox allow-scripts and
//     no allow-same-origin, so a page runs in an opaque origin: no cookies,
//     no storage, no reading of /api/* or of other pages (CORS fails, and
//     connect-src is 'none' besides), no navigating the top window, no
//     popups, no form posts. Its own scripts still run.
//   - no response here ever carries Access-Control-Allow-Origin: the
//     Tailscale identity is ambient, so a CORS grant would let any site
//     the user visits read their pages.
//   - the app shows a page in <iframe sandbox="allow-scripts"> with a
//     provenance bar naming who published it; a page opened directly has the
//     header but no bar.
//
// Roles (see agentauth.go): any agent, a tagged node included, publishes and
// lists. Updating (same id, or same key) and deleting is the user's, the
// page's own publisher's or a recorded coordinator's. The publisher is taken
// from the authenticated caller, never from the body. Reading a page is the
// user's alone (GET /p/...); an agent reads a body back through GET
// /api/pages/{id}?body=1 under the same host rule as other agents' output.

const (
	pageMaxBytes    = 5 << 20 // one version's body
	pageMaxRequest  = 7<<20 + 64<<10
	pageMaxTitle    = 120
	pageMaxVersions = 10
	pageQuota       = 500 << 20 // all kept versions together
	pageMaxPages    = 1000
	pageRate        = 20 // publishes per publisher per minute
	pageMaxKey      = 64
)

var (
	pageIDRE   = regexp.MustCompile(`^pg_[0-9a-f]{10}$`)
	pageKeyRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	pageTitleR = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

var pageKinds = map[string]string{"html": "html", "md": "md"}

// PageVersion is one stored body.
type PageVersion struct {
	N    int       `json:"n"`
	Kind string    `json:"kind"`
	Size int64     `json:"size"`
	At   time.Time `json:"at"`
}

// Page is one published page and its kept versions.
type Page struct {
	ID    string `json:"id"`
	Key   string `json:"key,omitempty"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	// By is the publisher as the user reads it; Agent is its fleet id
	// ("host/pane"), empty when the user published it; Host is where it
	// ran. All three come from the authenticated caller.
	By      string        `json:"by"`
	Agent   string        `json:"agent,omitempty"`
	Host    string        `json:"host,omitempty"`
	Tagged  bool          `json:"tagged,omitempty"`
	Handoff string        `json:"handoff,omitempty"`
	Created time.Time     `json:"created_at"`
	Updated time.Time     `json:"updated_at"`
	Version int           `json:"version"` // the newest
	Size    int64         `json:"size"`    // of the newest
	History []PageVersion `json:"versions"`
}

type pagesFile struct {
	Version int    `json:"version"`
	Pages   []Page `json:"pages"`
}

// pageWho is the caller of a page call.
type pageWho struct {
	by     string
	agent  string // fleet id, "" for the user
	host   string
	tagged bool
	user   bool
	coord  bool
}

func (w pageWho) rateKey() string {
	switch {
	case w.user:
		return "user"
	case w.agent != "":
		return w.agent
	}
	return "host:" + w.host
}

// mayChange: the user, a coordinator, the page's publisher, or (for a tagged
// node, whose pane is only claimed) any caller on the page's host.
func (w pageWho) mayChange(p Page) bool {
	switch {
	case w.user || w.coord:
		return true
	case w.tagged:
		return sameHost(p.Host, w.host)
	case w.agent != "":
		return p.Agent == w.agent
	}
	return false
}

// pageStore keeps the index and the version files.
type pageStore struct {
	mu    sync.Mutex
	dir   string // "" keeps nothing on disk (an unreadable index)
	data  pagesFile
	now   func() time.Time
	rate  map[string][]time.Time
	quota int64
}

func newPageStore(dir string) *pageStore {
	return &pageStore{dir: dir, data: pagesFile{Version: 1}, now: time.Now, rate: map[string][]time.Time{}, quota: pageQuota}
}

func openPageStore(dir string) (*pageStore, error) {
	st := newPageStore(dir)
	b, err := os.ReadFile(filepath.Join(dir, "pages.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &st.data); err != nil {
		return nil, fmt.Errorf("parse pages.json: %w", err)
	}
	st.data.Version = 1
	return st, nil
}

func (st *pageStore) saveLocked() error {
	if st.dir == "" {
		return nil
	}
	return writeFileAtomic(filepath.Join(st.dir, "pages.json"), &st.data)
}

func (st *pageStore) bodyPath(id string, n int, kind string) string {
	return filepath.Join(st.dir, id, "v"+strconv.Itoa(n)+"."+kind)
}

func newPageID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "pg_" + hex.EncodeToString(b)
}

var (
	errPageNotFound  = errors.New("not_found")
	errPageForbidden = errors.New("page_forbidden")
	errPageConflict  = errors.New("version_conflict")
	errPageQuota     = errors.New("page_quota")
	errPageFull      = errors.New("page_full")
	errPageRate      = errors.New("page_rate")
	errPageNoBody    = errors.New("body_gone")
)

// pageIn is one publish.
type pageIn struct {
	ID, Key, Title, Kind, Handoff string
	Body                          []byte
	IfVersion                     *int
}

// publish stores a new page, or a new version of the page named by id or
// key. created tells which.
func (st *pageStore) publish(in pageIn, who pageWho) (out Page, created bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()

	idx := -1
	for i, p := range st.data.Pages {
		if (in.ID != "" && p.ID == in.ID) || (in.ID == "" && in.Key != "" && p.Key == in.Key) {
			idx = i
			break
		}
	}
	if in.ID != "" && idx < 0 {
		return Page{}, false, errPageNotFound
	}
	if idx >= 0 {
		p := st.data.Pages[idx]
		if !who.mayChange(p) {
			return Page{}, false, errPageForbidden
		}
		if in.IfVersion != nil && *in.IfVersion != p.Version {
			return p, false, errPageConflict
		}
	} else if len(st.data.Pages) >= pageMaxPages {
		return Page{}, false, errPageFull
	}

	recent := st.rate[who.rateKey()][:0]
	for _, at := range st.rate[who.rateKey()] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= pageRate {
		st.rate[who.rateKey()] = recent
		return Page{}, false, errPageRate
	}

	// The quota counts every kept version, less the ones this publish drops.
	var total int64
	for _, p := range st.data.Pages {
		for _, v := range p.History {
			total += v.Size
		}
	}
	var drop []PageVersion
	if idx >= 0 {
		h := st.data.Pages[idx].History
		if n := len(h) + 1 - pageMaxVersions; n > 0 {
			drop = h[:n]
		}
	}
	for _, v := range drop {
		total -= v.Size
	}
	if total+int64(len(in.Body)) > st.quota {
		return Page{}, false, errPageQuota
	}
	st.rate[who.rateKey()] = append(recent, now)

	var p Page
	if idx >= 0 {
		p = st.data.Pages[idx]
		p.History = append([]PageVersion(nil), p.History...)
	} else {
		p = Page{ID: newPageID(), Key: in.Key, Created: now, By: who.by, Agent: who.agent, Host: who.host, Tagged: who.tagged}
		created = true
	}
	p.Version++
	p.Kind = in.Kind
	p.Updated = now
	p.Size = int64(len(in.Body))
	if in.Title != "" {
		p.Title = in.Title
	} else if p.Title == "" || idx < 0 {
		p.Title = deriveTitle(in.Kind, in.Body)
	}
	if in.Handoff != "" {
		p.Handoff = in.Handoff
	}
	p.History = append(p.History, PageVersion{N: p.Version, Kind: in.Kind, Size: p.Size, At: now})
	if n := len(p.History) - pageMaxVersions; n > 0 {
		p.History = p.History[n:]
	}

	if st.dir != "" {
		if err := writeBodyFile(st.bodyPath(p.ID, p.Version, in.Kind), in.Body); err != nil {
			return Page{}, false, err
		}
	}
	prev := st.data
	if idx >= 0 {
		st.data.Pages = append([]Page(nil), prev.Pages...)
		st.data.Pages[idx] = p
	} else {
		st.data.Pages = append(append([]Page(nil), prev.Pages...), p)
	}
	if err := st.saveLocked(); err != nil {
		st.data = prev
		if st.dir != "" {
			_ = os.Remove(st.bodyPath(p.ID, p.Version, in.Kind))
		}
		return Page{}, false, err
	}
	if st.dir != "" {
		for _, v := range drop {
			_ = os.Remove(st.bodyPath(p.ID, v.N, v.Kind))
		}
	}
	return p, created, nil
}

func writeBodyFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".v-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (st *pageStore) get(id string) (Page, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, p := range st.data.Pages {
		if p.ID == id {
			return p, true
		}
	}
	return Page{}, false
}

// list returns the pages newest first.
func (st *pageStore) list() []Page {
	st.mu.Lock()
	out := append([]Page{}, st.data.Pages...)
	st.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// body reads one version (0 is the newest).
func (st *pageStore) body(id string, n int) (Page, PageVersion, []byte, error) {
	p, ok := st.get(id)
	if !ok {
		return Page{}, PageVersion{}, nil, errPageNotFound
	}
	if n == 0 {
		n = p.Version
	}
	for _, v := range p.History {
		if v.N != n {
			continue
		}
		b, err := os.ReadFile(st.bodyPath(p.ID, v.N, v.Kind))
		if err != nil {
			return p, v, nil, errPageNoBody
		}
		return p, v, b, nil
	}
	return p, PageVersion{}, nil, errPageNotFound
}

func (st *pageStore) remove(id string, who pageWho) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, p := range st.data.Pages {
		if p.ID != id {
			continue
		}
		if !who.mayChange(p) {
			return errPageForbidden
		}
		prev := st.data
		st.data.Pages = append(append([]Page(nil), prev.Pages[:i]...), prev.Pages[i+1:]...)
		if err := st.saveLocked(); err != nil {
			st.data = prev
			return err
		}
		if st.dir != "" {
			_ = os.RemoveAll(filepath.Join(st.dir, p.ID))
		}
		return nil
	}
	return errPageNotFound
}

// deriveTitle: the <title> of an HTML page or the first heading of a
// Markdown one.
func deriveTitle(kind string, body []byte) string {
	var t string
	switch kind {
	case "html":
		head := body
		if len(head) > 64<<10 {
			head = head[:64<<10]
		}
		if m := pageTitleR.FindSubmatch(head); m != nil {
			t = html.UnescapeString(string(m[1]))
		}
	case "md":
		for _, ln := range strings.Split(string(body), "\n") {
			if s := strings.TrimSpace(ln); strings.HasPrefix(s, "#") {
				t = strings.TrimSpace(strings.TrimLeft(s, "#"))
				break
			}
		}
	}
	if t = oneLine(t, pageMaxTitle); t == "" {
		return "Untitled"
	}
	return t
}

// sniffKind guesses when the publisher did not say: markup starts with '<'.
func sniffKind(body []byte) string {
	if s := strings.TrimLeft(string(body[:min(len(body), 512)]), " \t\r\n\ufeff"); strings.HasPrefix(s, "<") {
		return "html"
	}
	return "md"
}

// ---- API ----

func (s *Server) pageRoutes() {
	s.mux.HandleFunc("POST /api/pages", s.authWorker(s.handlePageCreate))
	s.mux.HandleFunc("GET /api/pages", s.authWorker(s.handlePageList))
	s.mux.HandleFunc("GET /api/pages/{id}", s.authWorker(s.handlePageGet))
	s.mux.HandleFunc("DELETE /api/pages/{id}", s.authWorker(s.handlePageDelete))
	// The pages themselves: the user's alone. Every other path under /p/
	// answers 404 with the same headers.
	s.mux.HandleFunc("GET /p/_/{name}", s.handlePageLib)
	s.mux.HandleFunc("GET /p/{id}", s.auth(s.handlePageView))
	s.mux.HandleFunc("GET /p/{id}/{ver}", s.auth(s.handlePageView))
	s.mux.HandleFunc("GET /p/", s.handlePageMissing)
}

func (s *Server) pagesOff(w http.ResponseWriter, r *http.Request) bool {
	if s.ledgerOff(w, r) {
		return true
	}
	if s.pages == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"code": "pages_disabled", "message": "pages are off on this hub"})
		return true
	}
	return false
}

// pageCaller describes the authenticated caller for the page rules.
func (s *Server) pageCaller(r *http.Request) pageWho {
	c := callerOf(r)
	if !c.agent() {
		return pageWho{user: true, by: requester(r, "")}
	}
	host := c.host
	if host == "" {
		host = s.fleet.LocalName()
	}
	return pageWho{by: s.by(r, ""), agent: c.agentRef(), host: host, tagged: c.tagged, coord: !c.tagged && s.callerIsCoordinator(c)}
}

func pageErr(w http.ResponseWriter, err error) {
	var code, msg string
	var status int
	switch {
	case errors.Is(err, errPageNotFound):
		status, code, msg = http.StatusNotFound, "not_found", "no such page"
	case errors.Is(err, errPageForbidden):
		status, code, msg = http.StatusForbidden, "page_forbidden", "only the user, the page's publisher or a coordinator can change or delete a page"
	case errors.Is(err, errPageConflict):
		status, code, msg = http.StatusConflict, "version_conflict", "the page changed since that version: read it again"
	case errors.Is(err, errPageQuota):
		status, code, msg = http.StatusRequestEntityTooLarge, "page_quota", fmt.Sprintf("the pages already use the %d MiB quota: delete some", pageQuota>>20)
	case errors.Is(err, errPageFull):
		status, code, msg = http.StatusTooManyRequests, "page_full", fmt.Sprintf("%d pages already: delete some", pageMaxPages)
	case errors.Is(err, errPageRate):
		status, code, msg = http.StatusTooManyRequests, "page_rate", "publishing too fast: wait a minute"
	case errors.Is(err, errPageNoBody):
		status, code, msg = http.StatusGone, "body_gone", "that version's file is missing on the hub"
	default:
		status, code, msg = http.StatusInternalServerError, "page_error", err.Error()
	}
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
}

type pageBody struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	BodyB64   string `json:"body_b64"`
	Handoff   string `json:"handoff"`
	IfVersion *int   `json:"if_version"`
}

// pageJSON is a page as the API shows it.
func (s *Server) pageJSON(p Page) map[string]any {
	m := map[string]any{"page": p, "path": "/p/" + p.ID}
	if s.cfg.PublicURL != "" {
		m["url"] = s.cfg.PublicURL + "/p/" + p.ID
	}
	return m
}

// handlePageCreate: POST /api/pages.
func (s *Server) handlePageCreate(w http.ResponseWriter, r *http.Request) {
	if s.pagesOff(w, r) {
		return
	}
	var b pageBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, pageMaxRequest)).Decode(&b); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "page_too_large", "message": fmt.Sprintf("a page is at most %d MiB", pageMaxBytes>>20)})
			return
		}
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	bad := func(code, msg string) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": code, "message": msg})
	}
	in := pageIn{ID: strings.TrimSpace(b.ID), Key: strings.TrimSpace(b.Key), Title: oneLine(b.Title, pageMaxTitle), Handoff: strings.TrimSpace(b.Handoff), IfVersion: b.IfVersion}
	if in.ID != "" && !pageIDRE.MatchString(in.ID) {
		bad("bad_id", "not a page id")
		return
	}
	if in.Key != "" && !pageKeyRE.MatchString(in.Key) {
		bad("bad_key", fmt.Sprintf("a key is letters, digits, '.', '_' and '-', up to %d", pageMaxKey))
		return
	}
	if in.Handoff != "" && !todoHandoffRE.MatchString(in.Handoff) {
		bad("bad_handoff", "not a handoff id")
		return
	}
	switch {
	case b.Body != "" && b.BodyB64 != "":
		bad("bad_body", "send body or body_b64, not both")
		return
	case b.BodyB64 != "":
		raw, err := base64.StdEncoding.DecodeString(b.BodyB64)
		if err != nil {
			bad("bad_body", "body_b64 is not base64")
			return
		}
		in.Body = raw
	default:
		in.Body = []byte(b.Body)
	}
	if len(in.Body) == 0 {
		bad("bad_body", "the page is empty")
		return
	}
	if len(in.Body) > pageMaxBytes {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "page_too_large", "message": fmt.Sprintf("a page is at most %d MiB", pageMaxBytes>>20)})
		return
	}
	if !utf8.Valid(in.Body) {
		bad("bad_body", "a page is UTF-8 text (HTML or Markdown); put images in as data: URIs")
		return
	}
	if in.Kind = pageKinds[strings.ToLower(strings.TrimSpace(b.Kind))]; in.Kind == "" {
		if strings.TrimSpace(b.Kind) != "" {
			bad("bad_kind", "kind is html or md")
			return
		}
		in.Kind = sniffKind(in.Body)
	}
	p, created, err := s.pages.publish(in, s.pageCaller(r))
	if err != nil {
		if errors.Is(err, errPageConflict) {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"code": "version_conflict", "message": "the page changed since that version: read it again", "page": p})
			return
		}
		pageErr(w, err)
		return
	}
	m := s.pageJSON(p)
	m["created"] = created
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSONStatus(w, status, m)
}

// visibleTo: the user and ordinary agents see every page's metadata; a
// tagged node only its own host's.
func (w pageWho) sees(p Page) bool { return !w.tagged || sameHost(p.Host, w.host) }

// handlePageList: GET /api/pages[?mine=1][&handoff=ID].
func (s *Server) handlePageList(w http.ResponseWriter, r *http.Request) {
	if s.pagesOff(w, r) {
		return
	}
	who := s.pageCaller(r)
	q := r.URL.Query()
	out := []Page{}
	for _, p := range s.pages.list() {
		if !who.sees(p) || (q.Get("mine") == "1" && !who.mayOwn(p)) || (q.Get("handoff") != "" && p.Handoff != q.Get("handoff")) {
			continue
		}
		out = append(out, p)
	}
	writeJSON(w, map[string]any{"pages": out, "public_url": s.cfg.PublicURL})
}

// mayOwn is "mine" in a listing: published by this caller.
func (w pageWho) mayOwn(p Page) bool {
	switch {
	case w.user:
		return p.Agent == ""
	case w.tagged:
		return sameHost(p.Host, w.host)
	}
	return w.agent != "" && p.Agent == w.agent
}

// handlePageGet: GET /api/pages/{id}[?body=1[&version=N]]. The body of a
// page is other agents' text, so an agent reads it only for pages from its
// own host (a coordinator any), as with agent output.
func (s *Server) handlePageGet(w http.ResponseWriter, r *http.Request) {
	if s.pagesOff(w, r) {
		return
	}
	who := s.pageCaller(r)
	p, ok := s.pages.get(r.PathValue("id"))
	if !ok || !who.sees(p) {
		pageErr(w, errPageNotFound)
		return
	}
	m := s.pageJSON(p)
	if r.URL.Query().Get("body") == "1" {
		if !who.user && !who.coord && !sameHost(p.Host, who.host) {
			readForbidden(w)
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("version"))
		_, v, b, err := s.pages.body(p.ID, n)
		if err != nil {
			pageErr(w, err)
			return
		}
		m["body"], m["body_version"], m["body_kind"] = string(b), v.N, v.Kind
	}
	writeJSON(w, m)
}

// handlePageDelete: DELETE /api/pages/{id}.
func (s *Server) handlePageDelete(w http.ResponseWriter, r *http.Request) {
	if s.pagesOff(w, r) {
		return
	}
	who := s.pageCaller(r)
	id := r.PathValue("id")
	if p, ok := s.pages.get(id); ok && !who.sees(p) {
		pageErr(w, errPageNotFound)
		return
	}
	if err := s.pages.remove(id, who); err != nil {
		pageErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"deleted": id})
}

// ---- serving a page ----

// The policy every /p/ response carries (errors too: see pagesHeaders).
// sandbox without allow-same-origin gives the page an opaque origin;
// allow-scripts lets its own code run. Nothing can be fetched or posted
// (connect-src, form-action), embedded (default-src 'none': no frames, no
// objects) or loaded except images and fonts as data: URIs and scripts
// inline or from cdnjs.
const pageCSP = "sandbox allow-scripts; default-src 'none'; base-uri 'none'; script-src 'unsafe-inline' https://cdnjs.cloudflare.com; " +
	"style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'"

// A Markdown page is shown by a fixed shell that renders it with the
// vendored libraries, which it loads from this server: the one place
// 'self' is allowed.
const pageMDCSP = "sandbox allow-scripts; default-src 'none'; base-uri 'none'; script-src 'unsafe-inline' 'self'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; form-action 'none'"

func setPageHeaders(h http.Header) {
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Del("Access-Control-Allow-Origin")
}

// pagesHeaders puts the page policy on every response under /p/, whatever
// answers it (a wrong Host, a missing route, an auth failure included).
func pagesHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/p" || strings.HasPrefix(r.URL.Path, "/p/") {
			setPageHeaders(w.Header())
		}
		next.ServeHTTP(w, r)
	})
}

func pageText(w http.ResponseWriter, status int, msg string) {
	setPageHeaders(w.Header())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

// handlePageView: GET /p/{id}[/{version}] (the user only: a page may hold
// anything an agent saw).
func (s *Server) handlePageView(w http.ResponseWriter, r *http.Request) {
	setPageHeaders(w.Header())
	if s.pages == nil {
		pageText(w, http.StatusNotFound, "no pages here: they are kept on the hub")
		return
	}
	id := r.PathValue("id")
	n := 0
	if v := strings.TrimPrefix(r.PathValue("ver"), "v"); r.PathValue("ver") != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			pageText(w, http.StatusNotFound, "no such page")
			return
		}
	}
	if !pageIDRE.MatchString(id) {
		pageText(w, http.StatusNotFound, "no such page")
		return
	}
	_, v, body, err := s.pages.body(id, n)
	switch {
	case errors.Is(err, errPageNotFound):
		pageText(w, http.StatusNotFound, "no such page")
		return
	case err != nil:
		pageText(w, http.StatusGone, "that page's file is missing on the hub")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if v.Kind == "md" {
		h.Set("Content-Security-Policy", pageMDCSP)
		body = markdownShell(body)
	}
	_, _ = w.Write(body)
}

// handlePageMissing answers every other /p/ path.
func (s *Server) handlePageMissing(w http.ResponseWriter, r *http.Request) {
	pageText(w, http.StatusNotFound, "no such page")
}

// pageLibs are the vendored libraries a Markdown page loads.
var pageLibs = map[string]string{"marked.js": "vendor/marked.umd.js", "purify.js": "vendor/purify.min.js"}

// handlePageLib: GET /p/_/{name}. Public like the app's own assets.
func (s *Server) handlePageLib(w http.ResponseWriter, r *http.Request) {
	setPageHeaders(w.Header())
	path, ok := pageLibs[r.PathValue("name")]
	if !ok {
		pageText(w, http.StatusNotFound, "no such file")
		return
	}
	b, err := web.FS.ReadFile(path)
	if err != nil {
		log.Printf("pages: %s: %v", path, err)
		pageText(w, http.StatusInternalServerError, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

const mdShellHead = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Page</title>
<style>
:root{color-scheme:light dark;--fg:#1c2220;--bg:#fff;--mut:#5b6560;--line:#d9ddd8;--code:#eef0ec}
@media (prefers-color-scheme:dark){:root{--fg:#e4e9e5;--bg:#121715;--mut:#9aa6a0;--line:#2c3532;--code:#232b28}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.6 system-ui,sans-serif}
main{max-width:76ch;margin:0 auto;padding:20px 16px 56px;overflow-wrap:anywhere}
h1,h2,h3{line-height:1.25}pre{background:var(--code);padding:12px;border-radius:6px;overflow-x:auto}
code{background:var(--code);padding:1px 4px;border-radius:3px;font-size:.92em}pre code{padding:0;background:none}
table{border-collapse:collapse;display:block;overflow-x:auto}th,td{border:1px solid var(--line);padding:5px 9px}
blockquote{margin-left:0;padding-left:12px;border-left:3px solid var(--line);color:var(--mut)}
.u{color:var(--mut);font-size:.9em}
</style></head><body><main id="out"><noscript>This page needs scripts to render.</noscript></main>
`

const mdShellTail = `<script src="/p/_/purify.js"></script><script src="/p/_/marked.js"></script>
<script>
(function(){
var out=document.getElementById("out"),src=JSON.parse(document.getElementById("src").textContent);
if(!window.marked||!window.DOMPurify){out.textContent=src;return}
var m=new marked.Marked({gfm:true});
m.use({renderer:{html:function(t){return (t.text||"").replace(/&/g,"&amp;").replace(/</g,"&lt;")}}});
out.innerHTML=DOMPurify.sanitize(m.parse(src),{ALLOWED_URI_REGEXP:/^(?:https?:|mailto:|#)/i,FORBID_TAGS:["style","form","input","button","textarea","select","img","svg","math"],ALLOW_DATA_ATTR:false});
Array.prototype.forEach.call(out.querySelectorAll("a"),function(a){var h=a.getAttribute("href")||"";
 if(h.charAt(0)==="#")return;var s=document.createElement("span");s.textContent=a.textContent+(h?" ("+h+")":"");a.replaceWith(s)});
var h=out.querySelector("h1");if(h)document.title=h.textContent;
})();
</script></body></html>`

// markdownShell wraps Markdown source in a page that renders it. The source
// travels as a JSON string in a data block (json.Marshal escapes <, > and &,
// so it cannot close the block); the page's script does the rest.
func markdownShell(md []byte) []byte {
	j, _ := json.Marshal(string(md))
	return []byte(mdShellHead + `<script type="application/json" id="src">` + string(j) + "</script>\n" + mdShellTail)
}
