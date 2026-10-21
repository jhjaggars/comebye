package server

import (
	"net/http"
	"strings"
)

// tailscaleAllowed reports whether Tailscale Serve identified the caller as
// an allowlisted login. Serve sets Tailscale-User-Login on every request it
// proxies from a tailnet device and strips any client-supplied copy, so the
// header is trustworthy as long as Serve is the only thing that can reach the
// loopback listener (ListenAndServe enforces a loopback bind when the
// allowlist is set).
func (s *Server) tailscaleAllowed(r *http.Request) bool {
	login := strings.ToLower(strings.TrimSpace(r.Header.Get("Tailscale-User-Login")))
	if login == "" {
		return false
	}
	for _, a := range s.cfg.AllowLogins {
		if login == a {
			return true
		}
	}
	return false
}

// authorized reports whether the request is allowed. With an allowlist
// configured only an allowlisted Tailscale identity is admitted; without one
// the API is open (ListenAndServe logs a warning).
func (s *Server) authorized(r *http.Request) bool {
	if len(s.cfg.AllowLogins) == 0 {
		return true
	}
	return s.tailscaleAllowed(r)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Every API response (proxied peer calls included) carries this
		// instance's app version, so a stale page notices a deploy.
		w.Header().Set("X-App-Version", s.version)
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
