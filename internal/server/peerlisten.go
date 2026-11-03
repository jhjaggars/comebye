package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/tsauth"
)

// The peer listener: a second HTTP listener on the hub's tailnet address
// for tagged nodes (tag:worker), which have no Tailscale login and so
// cannot pass Serve's identity check. The hub asks tailscaled's WhoIs who
// the connection's source address is (the address WireGuard authenticated,
// which no local process can fake from loopback), admits only nodes that
// carry an allowed tag, and gives them the forwarded-agent role and
// nothing else: user-only routes answer 403 as they do for any agent, the
// UI is not served, and the Tailscale-*, agent-token and forwarding
// headers are ignored (a tagged node cannot claim a login, the local
// agent token or another host's name).
//
// Beyond the identity, a node must be a registered peer (one of whose
// addresses is the node itself), or announce itself as one when the hub
// allows self-registration.

type taggedKey struct{}

// tagged is the verified caller of a peer-listener request.
type tagged struct {
	node string // the node's MagicDNS name
	tag  string // the allowed tag it carries
	peer string // its registered peer name, "" before it is registered
}

func taggedOf(r *http.Request) (tagged, bool) {
	t, ok := r.Context().Value(taggedKey{}).(tagged)
	return t, ok
}

// allowedTag returns the first of the node's tags that the hub admits.
func (s *Server) allowedTag(id tsauth.Identity) string {
	for _, t := range id.Tags {
		for _, a := range s.cfg.AllowTags {
			if strings.EqualFold(t, a) {
				return strings.ToLower(t)
			}
		}
	}
	return ""
}

func peerDeny(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
}

// PeerHandler is the handler for the peer listener.
func (s *Server) PeerHandler() http.Handler {
	inner := s.base()
	return s.checkHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// Nothing the client says about who it is counts here.
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "tailscale-") {
				delete(r.Header, k)
			}
		}
		for _, k := range []string{HeaderAgentToken, HeaderForwarded, HeaderAgentHost} {
			r.Header.Del(k)
		}
		if len(s.cfg.AllowTags) == 0 || s.cfg.WhoIs == nil {
			peerDeny(w, http.StatusForbidden, "tags_off", "this hub admits no tagged nodes (-ts-allow-tag)")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		id, err := s.cfg.WhoIs.WhoIs(ctx, r.RemoteAddr)
		cancel()
		if err != nil {
			if !errors.Is(err, tsauth.ErrUnknown) {
				s.logf("peer listener: whois %s: %v", r.RemoteAddr, err)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tag := s.allowedTag(id)
		if tag == "" {
			peerDeny(w, http.StatusForbidden, "tag_not_allowed", "node "+id.Node+" carries none of this hub's allowed tags")
			return
		}
		t := tagged{node: id.Node, tag: tag}
		if name, ptag, ok := s.fleet.FindByHost(id.Node); ok {
			if ptag != "" && !id.HasTag(ptag) {
				peerDeny(w, http.StatusForbidden, "tag_mismatch", "node "+id.Node+" no longer carries the tag "+ptag+" its peer was registered with")
				return
			}
			t.peer = name
		} else if r.URL.Path != "/api/peers/announce" {
			peerDeny(w, http.StatusForbidden, "not_a_peer", "node "+id.Node+" is not a registered peer of this hub"+announceHint(s.cfg.SelfRegister))
			return
		}
		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), taggedKey{}, t)))
	}))
}

func announceHint(on bool) string {
	if on {
		return " (announce it first)"
	}
	return " (a user adds it with `comebye peer add`)"
}

func (s *Server) logf(format string, args ...any) {
	if s.fleet != nil {
		s.fleet.Logf(format, args...)
	}
}

// ServePeerListener serves PeerHandler on ln until it fails. ln must be
// bound to a tailnet address: a wildcard or loopback listener would be
// reachable by (or confusable with) things WhoIs cannot vouch for.
func (s *Server) ServePeerListener(ln net.Listener) error {
	ta, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return errors.New("peer listener: not a TCP listener")
	}
	ip, _ := netip.AddrFromSlice(ta.IP)
	if !tsauth.Tailnet(ip.Unmap()) {
		return errors.New("peer listener: " + ln.Addr().String() + " is not a tailnet address; it must be bound to the hub's tailnet IP")
	}
	hs := &http.Server{
		Handler:           s.PeerHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return hs.Serve(ln)
}

// handleAnnounce is POST /api/peers/announce {name?, url?}: a tagged node
// registers itself as a peer. It is answered only on the peer listener,
// only when the hub allows self-registration, and only for the caller's
// own node: the announced URL must be on the node WhoIs identified.
func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	t, ok := taggedOf(r)
	if !ok {
		peerDeny(w, http.StatusForbidden, "announce_tagged_only", "peers announce themselves over the hub's peer listener, from a tagged node")
		return
	}
	if s.notHub(w) {
		return
	}
	if !s.cfg.SelfRegister {
		peerDeny(w, http.StatusForbidden, "self_register_off", "this hub does not accept self-registration (-peer-self-register); a user adds peers with `comebye peer add`")
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	info, created, err := s.fleet.Announce(req.Name, req.URL, t.node, t.tag)
	if err != nil {
		switch {
		case errors.Is(err, fleet.ErrNameTaken), errors.Is(err, fleet.ErrNodeRegistered):
			peerDeny(w, http.StatusConflict, "peer_exists", err.Error())
		default:
			s.peerError(w, err)
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSONStatus(w, status, info)
}
