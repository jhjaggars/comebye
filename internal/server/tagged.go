package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/tsauth"
)

// Tagged nodes (tag:worker) have no Tailscale login and so cannot pass
// Serve's identity check. capPeers (capauth.go) admits them through Tailscale
// Serve by a grant's app capability (AllowCap): the hub asks tailscaled's
// WhoIs who the source address is, and gives a node that holds the capability
// the forwarded-agent role and nothing else: user-only routes answer 403 as
// they do for any agent, the UI is not served, and the Tailscale-*,
// agent-token and forwarding headers are ignored (a tagged node cannot claim
// a login, the local agent token or another host's name).
//
// Beyond the identity, a node must be a registered peer (one of whose
// addresses is the node itself), or announce itself as one when the hub
// allows self-registration. (Until v0.6 there was a second way in, a plain
// HTTP "peer listener" on port 8742; it was removed.)

type taggedKey struct{}

// tagged is the verified caller of a request admitted by capPeers.
type tagged struct {
	node string // the node's MagicDNS name
	tag  string // the allowed tag it carries
	peer string // its registered peer name, "" before it is registered
}

func taggedOf(r *http.Request) (tagged, bool) {
	t, ok := r.Context().Value(taggedKey{}).(tagged)
	return t, ok
}

// admitPeer applies the rules for an admitted node: it
// must be a registered peer that still carries the peer's recorded tag, or
// be announcing itself. It refuses the request itself when not.
func (s *Server) admitPeer(w http.ResponseWriter, r *http.Request, id tsauth.Identity, tag string) (tagged, bool) {
	t := tagged{node: id.Node, tag: tag}
	if name, ptag, ok := s.fleet.FindByHost(id.Node); ok {
		if ptag != "" && !id.HasTag(ptag) {
			peerDeny(w, http.StatusForbidden, "tag_mismatch", "node "+id.Node+" no longer carries the tag "+ptag+" its peer was registered with")
			return t, false
		}
		t.peer = name
	} else if r.URL.Path != "/api/peers/announce" {
		peerDeny(w, http.StatusForbidden, "not_a_peer", "node "+id.Node+" is not a registered peer of this hub"+announceHint(s.cfg.SelfRegister))
		return t, false
	}
	return t, true
}

// dropClaims removes everything a client says about who it is.
func dropClaims(r *http.Request) {
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "tailscale-") {
			delete(r.Header, k)
		}
	}
	for _, k := range []string{HeaderAgentToken, HeaderForwarded, HeaderAgentHost} {
		r.Header.Del(k)
	}
}

func peerDeny(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
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

// handleAnnounce is POST /api/peers/announce {name?, url?}: a tagged node
// registers itself as a peer. It is answered only for a caller admitted as
// a tagged node (admitted by capPeers), only when the hub allows self-registration, and only for the caller's
// own node: the announced URL must be on the node WhoIs identified.
func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	t, ok := taggedOf(r)
	if !ok {
		peerDeny(w, http.StatusForbidden, "announce_tagged_only", "peers announce themselves from a tagged node, through the hub's Serve URL with the capability")
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
