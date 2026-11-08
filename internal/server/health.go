package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Health probes for an orchestrator (Kubernetes liveness and readiness).
// Both are unauthenticated, answer on any Host header (a kubelet probes the
// pod IP), do no network I/O and carry no secrets: no paths, no names, no
// counts, only the version and a status per check.
//
//	GET /healthz  the process serves requests and the state directory is
//	              writable (a file is written, synced and removed there; the
//	              result is cached for healthProbeEvery so a probe storm costs
//	              one write per interval).
//	GET /readyz   /healthz, plus: the single-hub lock is still held and its
//	              heartbeat is being written (Config.LockErr), and the server
//	              is not shutting down.
//
// A 200 answer is {"status":"ok",...}; a failing check answers 503 with
// "status":"fail" and the check that failed.

const healthProbeEvery = 5 * time.Second

type healthProbe struct {
	dir string
	now func() time.Time

	mu   sync.Mutex
	at   time.Time
	fail bool
}

// writable reports whether dir accepts a write, re-testing at most every
// healthProbeEvery.
func (p *healthProbe) writable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && p.now().Sub(p.at) < healthProbeEvery {
		return !p.fail
	}
	p.fail = p.test() != nil
	p.at = p.now()
	return !p.fail
}

func (p *healthProbe) test() error {
	f, err := os.OpenFile(filepath.Join(p.dir, ".healthz"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(p.now().UTC().Format(time.RFC3339Nano))
	serr := f.Sync()
	cerr := f.Close()
	_ = os.Remove(f.Name())
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			return e
		}
	}
	return nil
}

// ShuttingDown makes /readyz fail from now on, so traffic drains before the
// process exits.
func (s *Server) ShuttingDown() { s.shuttingDown.Store(true) }

// serveHealth answers /healthz and /readyz; it reports whether it did.
func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) bool {
	ready := false
	switch r.URL.Path {
	case "/healthz":
	case "/readyz":
		ready = true
	default:
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	checks := map[string]string{}
	ok := true
	set := func(name string, good bool) {
		if good {
			checks[name] = "ok"
		} else {
			checks[name] = "fail"
			ok = false
		}
	}
	if s.health != nil {
		set("state_dir", s.health.writable())
	}
	if ready {
		if s.cfg.LockErr != nil {
			set("lock", s.cfg.LockErr() == nil)
		}
		set("serving", !s.shuttingDown.Load())
	}
	body := map[string]any{"status": "ok", "version": s.cfg.Version, "checks": checks}
	if s.cfg.HubOnly {
		body["hub_only"] = true
	}
	status := http.StatusOK
	if !ok {
		body["status"] = "fail"
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(body)
	}
	return true
}

// HealthHandler serves only /healthz and /readyz, for a separate listener
// (-health-addr) that an orchestrator can reach when the main one is
// loopback-only.
func (s *Server) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.serveHealth(w, r) {
			http.NotFound(w, r)
		}
	})
}
