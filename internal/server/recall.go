package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jhjaggars/comebye/internal/archive"
)

// The app's transcript search: read-only, over this host's index file.
//
// These routes hold every session's text, tool output included, from every host
// the index covers. They are the user's alone (wrapped in auth): an agent or a
// tagged worker gets 403, and they are never forwarded to a hub. Agents on a host
// with the index keep `comebye recall`, which reads the file directly; a worker
// has neither the index nor these routes. See docs/security.md.
//
// The queries are not logged: what someone searched for stays out of the journal.

const (
	recallMaxQ        = 500 // bytes of query text
	recallMaxSessions = 50
	recallMaxMessages = 200
	recallMaxSkip     = 100000
	recallMaxValue    = 200 // bytes of a filter value
)

var (
	recallHostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	recallKindRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	recallIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	recallSubRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	recallDateRE = regexp.MustCompile(`^[0-9][0-9TZ:.+-]{3,34}$`)
)

func (s *Server) recallRoutes() {
	s.mux.HandleFunc("GET /api/recall/status", s.auth(s.recallOnly(s.handleRecallStatus)))
	s.mux.HandleFunc("GET /api/recall/facets", s.auth(s.recallOnly(s.handleRecallFacets)))
	s.mux.HandleFunc("GET /api/recall/search", s.auth(s.recallOnly(s.handleRecallSearch)))
	s.mux.HandleFunc("GET /api/recall/recent", s.auth(s.recallOnly(s.handleRecallRecent)))
	s.mux.HandleFunc("GET /api/recall/sessions/{host}/{kind}/{id}", s.auth(s.recallOnly(s.handleRecallSession)))
	s.mux.HandleFunc("GET /api/recall/produced", s.auth(s.recallOnly(s.handleRecallProduced)))
	s.mux.HandleFunc("GET /api/recall/sessions/{host}/{kind}/{id}/produced", s.auth(s.recallOnly(s.handleRecallSessionProduced)))
	s.mux.HandleFunc("GET /api/recall/sessions/{host}/{kind}/{id}/hits", s.auth(s.recallOnly(s.handleRecallHits)))
}

// recallOnly marks the answer as not to be stored anywhere, whatever it is.
func (s *Server) recallOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

func recallBad(w http.ResponseWriter, msg string) {
	writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_request", "message": msg})
}

// recallErr answers a failed query. The error can quote the query, so it goes
// neither to the log nor to the client as is.
func recallErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, archive.ErrNoIndex):
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"code": "no_index", "message": "this host has no transcript index yet"})
	case errors.Is(err, archive.ErrNoSession):
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_session", "message": "that session is not in the index: it was deleted, or has not been indexed yet"})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSONStatus(w, http.StatusGatewayTimeout, map[string]string{"code": "timeout", "message": "the search took too long; try fewer or rarer words"})
	case errors.Is(err, context.Canceled):
		// The client went away.
	default:
		log.Printf("recall: %s failed", what)
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "index_error", "message": "the transcript index could not be read"})
	}
}

func (s *Server) handleRecallStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.recall.Status(r.Context())
	if err != nil {
		recallErr(w, "status", err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleRecallFacets(w http.ResponseWriter, r *http.Request) {
	f, err := s.recall.Facets(r.Context())
	if err != nil {
		recallErr(w, "facets", err)
		return
	}
	writeJSON(w, f)
}

// recallInt reads a whole number within [0, max]; absent is def.
func recallInt(v string, def, max int) (int, bool) {
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}

// recallFilter reads the filters a listing and a search share.
func recallFilter(w http.ResponseWriter, v url.Values) (archive.Filter, bool) {
	f := archive.Filter{Host: v.Get("host"), Kind: v.Get("kind"), Project: v.Get("project"), Branch: v.Get("branch"), Ledger: v.Get("ledger"), Since: v.Get("since"), Until: v.Get("until")}
	for _, x := range []string{f.Host, f.Kind, f.Project, f.Branch, f.Ledger} {
		if len(x) > recallMaxValue {
			recallBad(w, "a filter value is too long")
			return f, false
		}
	}
	for _, d := range []string{f.Since, f.Until} {
		if d != "" && !recallDateRE.MatchString(d) {
			recallBad(w, "since and until are dates or times")
			return f, false
		}
	}
	return f, true
}

func (s *Server) handleRecallRecent(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	f, ok := recallFilter(w, v)
	if !ok {
		return
	}
	limit, ok1 := recallInt(v.Get("limit"), 20, recallMaxSessions)
	skip, ok2 := recallInt(v.Get("skip"), 0, recallMaxSkip)
	if !ok1 || !ok2 {
		recallBad(w, "limit and skip are whole numbers")
		return
	}
	f.Limit = limit
	res, err := s.recall.Recent(r.Context(), f, skip)
	if err != nil {
		recallErr(w, "recent", err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) handleRecallSearch(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	text := strings.TrimSpace(v.Get("q"))
	if text == "" {
		recallBad(w, "q is required")
		return
	}
	if len(text) > recallMaxQ {
		recallBad(w, "q is too long")
		return
	}
	if m := v.Get("mode"); m != "" && m != "messages" {
		recallBad(w, "unknown mode")
		return
	}
	f, ok := recallFilter(w, v)
	if !ok {
		return
	}
	f.Role = v.Get("role")
	switch f.Role {
	case "", "said", "commands", "output":
	default:
		recallBad(w, "role is said, commands or output")
		return
	}
	sortBy := v.Get("sort")
	if sortBy != "" && sortBy != "rank" && sortBy != "new" {
		recallBad(w, "sort is rank or new")
		return
	}
	limit, ok1 := recallInt(v.Get("limit"), 20, recallMaxSessions)
	skip, ok2 := recallInt(v.Get("skip"), 0, recallMaxSkip)
	if !ok1 || !ok2 {
		recallBad(w, "limit and skip are whole numbers")
		return
	}
	f.Limit = limit
	res, err := s.recall.Search(r.Context(), text, f, sortBy, skip)
	if err != nil {
		recallErr(w, "search", err)
		return
	}
	writeJSON(w, res)
}

// recallSessionPath validates a session's path values against an allowlist.
func recallSessionPath(w http.ResponseWriter, r *http.Request) (host, kind, id string, ok bool) {
	host, kind, id = r.PathValue("host"), r.PathValue("kind"), r.PathValue("id")
	if !recallHostRE.MatchString(host) || !recallKindRE.MatchString(kind) || !recallIDRE.MatchString(id) {
		recallBad(w, "not a session")
		return "", "", "", false
	}
	return host, kind, id, true
}

func (s *Server) handleRecallSession(w http.ResponseWriter, r *http.Request) {
	host, kind, id, ok := recallSessionPath(w, r)
	if !ok {
		return
	}
	v := r.URL.Query()
	limit, ok := recallInt(v.Get("limit"), 100, recallMaxMessages)
	if !ok {
		recallBad(w, "limit is a whole number")
		return
	}
	o := archive.PageOpts{Limit: limit, Dir: v.Get("dir"), Sub: v.Get("sub"), Q: strings.TrimSpace(v.Get("q"))}
	if o.Sub != "" && !recallSubRE.MatchString(o.Sub) {
		recallBad(w, "not a subagent")
		return
	}
	if o.Dir != "" && o.Dir != "around" && o.Dir != "before" && o.Dir != "after" {
		recallBad(w, "dir is around, before or after")
		return
	}
	if len(o.Q) > recallMaxQ {
		recallBad(w, "q is too long")
		return
	}
	if sv := v.Get("seq"); sv != "" {
		n, err := strconv.Atoi(sv)
		if err != nil || n < -1 {
			recallBad(w, "seq is a message number")
			return
		}
		o.Seq = &n
	}
	p, err := s.recall.Session(r.Context(), host, kind, id, o)
	if err != nil {
		recallErr(w, "session", err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) handleRecallHits(w http.ResponseWriter, r *http.Request) {
	host, kind, id, ok := recallSessionPath(w, r)
	if !ok {
		return
	}
	text := strings.TrimSpace(r.URL.Query().Get("q"))
	if text == "" || len(text) > recallMaxQ {
		recallBad(w, "q is required, up to 500 bytes")
		return
	}
	hits, err := s.recall.Hits(r.Context(), host, kind, id, text)
	if err != nil {
		recallErr(w, "hits", err)
		return
	}
	writeJSON(w, map[string]any{"hits": hits, "capped": len(hits) >= archive.HitsCap})
}

// handleRecallProduced answers "where did this come from?": the sessions that made
// the artifact that ref names, and the message of each.
func (s *Server) handleRecallProduced(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	ref := strings.TrimSpace(v.Get("ref"))
	if ref == "" || len(ref) > recallMaxQ {
		recallBad(w, "ref is required, up to 500 bytes")
		return
	}
	f, ok := recallFilter(w, v)
	if !ok {
		return
	}
	limit, ok := recallInt(v.Get("limit"), 20, recallMaxSessions)
	if !ok {
		recallBad(w, "limit is a whole number")
		return
	}
	items, known, err := s.recall.ProducedRef(r.Context(), ref, f, limit)
	if err != nil {
		recallErr(w, "produced", err)
		return
	}
	if !known {
		recallBad(w, "ref is a commit SHA (7 to 40 hex), a pg_ id, owner/repo#N, owner/repo@tag, a GitHub PR, issue or release URL, or a sha256: digest")
		return
	}
	writeJSON(w, map[string]any{"produced": items})
}

// handleRecallSessionProduced answers "what did this session make?", in order.
func (s *Server) handleRecallSessionProduced(w http.ResponseWriter, r *http.Request) {
	host, kind, id, ok := recallSessionPath(w, r)
	if !ok {
		return
	}
	items, err := s.recall.ProducedBySession(r.Context(), host, kind, id)
	if err != nil {
		recallErr(w, "session produced", err)
		return
	}
	writeJSON(w, map[string]any{"produced": items})
}
