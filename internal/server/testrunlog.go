package server

import (
	"errors"
	"net/http"

	"github.com/jhjaggars/comebye/internal/testruns"
)

// handleTestRunLog is GET /api/test-runs/{id}/log (and, on the hub,
// /api/hosts/{host}/test-runs/{id}/log, proxied to that host): the tail of a
// finished test run's output on this host, a test VM, as tools/ci/remote.sh
// kept it beside the run's done record (at most testruns.MaxLog bytes), as
// plain text. The user's alone, like the host's other reads the hub proxies.
// Only what the suite printed is there; nothing is added or taken out here.
func (s *Server) handleTestRunLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.cfg.StateDir == "" || !testruns.ValidID(id) {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_log", "message": "no log for that run here"})
		return
	}
	b, err := testruns.Log(testruns.DoneDir(s.cfg.StateDir), id)
	if errors.Is(err, testruns.ErrNoLog) {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_log", "message": "no log for that run here (the VM keeps its last 50 runs)"})
		return
	}
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "read_failed", "message": "the log could not be read"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
