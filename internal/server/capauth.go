package server

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/tsauth"
)

// capPeers admits tagged nodes through Tailscale Serve, by a grant's app
// capability (AllowCap): the hub's normal URL, with no extra port. A tagged
// node has no login, so Serve sends no Tailscale-User-Login for it, but with
// --accept-app-caps it does send the capability (tsauth.HeaderAppCaps).
//
// That header does not say who the caller is, so the hub asks WhoIs about
// X-Forwarded-For, which Serve overwrites with the real source address. The
// answer must be a tagged node that holds the capability itself. From there
// the rules are the peer listener's: the forwarded-agent role and nothing
// else, a registered peer (or one announcing itself), /api/* only, and no
// login, token or host name the client claims counts.
//
// Requests with a login, or without the capability, pass through untouched:
// users are authorized as ever, and anyone else meets the usual checks.
//
// Serve removes a client's copy of these headers, so a tailnet node cannot
// forge them. A local process on the hub can, on the loopback port, as it
// can Tailscale-User-Login; that gets it less than the login does (the agent
// role, and only as a node that really holds the grant). See docs/security.md.
func (s *Server) capPeers(next http.Handler) http.Handler {
	if s.cfg.AllowCap == "" || s.cfg.WhoIs == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("Tailscale-User-Login")) != "" ||
			!tsauth.HeaderHasCap(r.Header.Get(tsauth.HeaderAppCaps), s.cfg.AllowCap) {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// Serve sets exactly one plain address. Anything else did not come
		// from it.
		src := r.Header.Values("X-Forwarded-For")
		if len(src) != 1 || !validAddr(src[0]) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		dropClaims(r)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		id, err := s.cfg.WhoIs.WhoIs(ctx, src[0])
		cancel()
		if err != nil {
			if !errors.Is(err, tsauth.ErrUnknown) {
				s.logf("capability peer: whois: %v", err)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if len(id.Tags) == 0 || !id.HasCap(s.cfg.AllowCap) {
			peerDeny(w, http.StatusForbidden, "cap_not_held", "node "+id.Node+" is not a tagged node holding this hub's capability")
			return
		}
		t, ok := s.admitPeer(w, r, id, strings.ToLower(id.Tags[0]))
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), taggedKey{}, t)))
	})
}

func validAddr(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}
