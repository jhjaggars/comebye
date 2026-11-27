package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/jhjaggars/comebye/internal/archive"
)

// POST /api/recall/query: `comebye recall` without a local index (#143,
// slice 5). The hub builds the one all-hosts recall index (archiveship.go),
// and the CLI on any host asks its own server, which answers from its index
// when it has no hub, and otherwise sends the question to the hub: an
// agent's question is forwarded like any agent call, the user's goes through
// userViaHub. A tagged worker is refused, as on every recall route: the
// plaintext index never reaches a worker. See docs/security.md.
//
// The question is not logged, and the answer is not stored (no-store).

const (
	recallQueryBody    = 16 << 10
	recallQueryArgs    = 64
	recallQueryLimit   = 200
	recallQueryTimeout = 20 * time.Second
)

// recallQueryLimiter lets two questions run at once.
var recallQueryLimiter = make(chan struct{}, 2)

func (s *Server) recallQueryRoutes() {
	s.mux.HandleFunc("POST /api/recall/query", s.authAgent(s.recallOnly(s.handleRecallQuery)))
}

func (s *Server) handleRecallQuery(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Upstream != nil && !callerOf(r).forwarded {
		s.userViaHub(w, r) // the user on a peer (an agent's call was forwarded already)
		return
	}
	var q archive.RecallQuery
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, recallQueryBody)).Decode(&q); err != nil {
		recallBad(w, "invalid body")
		return
	}
	if msg := recallQueryCheck(&q); msg != "" {
		recallBad(w, msg)
		return
	}
	path := s.cfg.RecallIndex
	if fi, err := os.Stat(path); path == "" || err != nil || !fi.Mode().IsRegular() {
		recallErr(w, "query", archive.ErrNoIndex)
		return
	}
	ix := &archive.Index{Path: path, ReadOnly: true, Timeout: recallQueryTimeout}
	select {
	case recallQueryLimiter <- struct{}{}:
		defer func() { <-recallQueryLimiter }()
	case <-r.Context().Done():
		return
	}
	if ix.Outdated() {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"code": "index_outdated", "message": "the transcript index is in an older format; the next index run rebuilds it"})
		return
	}
	a, err := ix.Recall(q)
	switch {
	case errors.Is(err, archive.ErrRecallUsage):
		recallBad(w, "that question needs other arguments")
		return
	case errors.Is(err, archive.ErrAmbiguous):
		recallBad(w, "that id matches several; give more of it")
		return
	case err != nil:
		recallErr(w, "query", err)
		return
	}
	writeJSON(w, a)
}

// recallQueryCheck bounds a question ("" when it is fine).
func recallQueryCheck(q *archive.RecallQuery) string {
	if !archive.RecallOps[q.Op] {
		return "unknown question"
	}
	if len(q.Args) > recallQueryArgs {
		return "too many words"
	}
	n := 0
	for _, a := range q.Args {
		n += len(a)
	}
	if n > recallMaxQ {
		return "the question is too long"
	}
	f := q.Filter
	for _, v := range []string{f.Since, f.Until, f.Host, f.Kind, f.Project, f.Branch, f.Ledger, f.Agent, f.Role} {
		if len(v) > recallMaxValue {
			return "a filter value is too long"
		}
	}
	if f.Limit < 0 || q.Text < 0 {
		return "a negative count"
	}
	q.Filter.Limit = min(f.Limit, recallQueryLimit)
	q.Text = min(q.Text, recallMaxMessages)
	return ""
}
