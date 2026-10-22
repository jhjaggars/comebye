package server

import (
	"net"
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
// (only served with InsecureNoAuth, see ListenAndServe) the API is open.
func (s *Server) authorized(r *http.Request) bool {
	if len(s.cfg.AllowLogins) == 0 {
		return true
	}
	return s.tailscaleAllowed(r)
}

// normalizeHost lowercases a host name and drops any port, IPv6 brackets
// and trailing dot.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

// hostAllowed reports whether a request was addressed to a name this server
// answers to. Tailscale Serve forwards the browser's Host (the machine's
// <name>.<tailnet>.ts.net); local tools use localhost or a loopback IP.
// Anything else is a DNS rebinding attempt: a page on an attacker's domain
// that re-resolves it to 127.0.0.1 reaches the loopback port as same-origin,
// and since any local client may set Tailscale-User-Login it could drive
// the agents. Nobody but Tailscale can point a ts.net name at this host.
func (s *Server) hostAllowed(r *http.Request) bool {
	host := normalizeHost(r.Host)
	switch {
	case host == "":
		return false
	case host == "localhost" || host == "127.0.0.1" || host == "::1":
		return true
	case strings.HasSuffix(host, ".ts.net"):
		return true
	}
	for _, h := range s.cfg.AllowHosts {
		if host == h {
			return true
		}
	}
	return false
}

// checkHost answers 421 Misdirected Request when the Host is not one of
// ours (see hostAllowed), for the UI and the API alike.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r) {
			http.Error(w, "misdirected request: unknown host (see -allow-host)", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SplitHosts parses -allow-host values: each may hold several
// comma-separated names; ports are dropped and names lowercased.
func SplitHosts(specs ...string) []string {
	var out []string
	for _, spec := range specs {
		for _, h := range strings.Split(spec, ",") {
			if h = normalizeHost(h); h != "" {
				out = append(out, h)
			}
		}
	}
	return out
}
