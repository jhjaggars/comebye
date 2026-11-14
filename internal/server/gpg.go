package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/gpgunlock"
)

// GPG unlock, the hub's part (docs/security.md): the app answers a host's
// request (approvals.go) and the hub hands the answer to that host, which
// checks it all again itself. The hub never sees the passphrase or K: the
// answer holds an assertion, K sealed to the request's one-use key, and at
// setup the passphrase blob (ciphertext).

// gpgHost is this host's GPG unlock state: the state directory's files. The
// lifetime is fixed at 8 hours (Config.GPGLifetime shortens it for tests).
func (s *Server) gpgHost() *gpgunlock.Host {
	if s.cfg.GPGHost != nil {
		return s.cfg.GPGHost
	}
	s.gpgMu.Lock()
	defer s.gpgMu.Unlock()
	if s.gpg == nil { // one value, so its mutex serialises the answers
		s.gpg = &gpgunlock.Host{Dir: s.cfg.StateDir, GPG: &gpgunlock.GPG{}, AllowLocalhost: s.cfg.TestMode}
	}
	return s.gpg
}

// gpgRequestID is the id of the host's request inside an approval's request.
func gpgRequestID(request []byte) string {
	var r struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(request, &r)
	return r.ID
}

type gpgCompleteBody struct {
	RequestID string           `json:"request_id"`
	Answer    gpgunlock.Answer `json:"answer"`
}

type gpgCompleteReply struct {
	UnlockedUntil time.Time `json:"unlocked_until"`
	Code          string    `json:"code,omitempty"`
	Message       string    `json:"message,omitempty"`
}

// relayGPG gives the host the app's answer and returns until when the key is
// unlocked. The host is this one (in process) or a peer (the hub's own call).
func (s *Server) relayGPG(ctx context.Context, a *approval, ans gpgunlock.Answer) (time.Time, error) {
	id := gpgRequestID(a.Request)
	if id == "" {
		return time.Time{}, errors.New("the request carries no id")
	}
	if a.Host == "" || sameHost(a.Host, s.fleet.LocalName()) {
		st, err := s.gpgHost().Complete(id, ans)
		return st.ExpiresAt, err
	}
	if !s.fleet.HasPeer(a.Host) {
		return time.Time{}, errors.New("the host " + a.Host + " is not known to this hub")
	}
	body, _ := json.Marshal(gpgCompleteBody{RequestID: id, Answer: ans})
	code, out, err := s.fleet.CallHost(ctx, a.Host, "gpg-complete", nil, body)
	if err != nil {
		return time.Time{}, err
	}
	var rep gpgCompleteReply
	if json.Unmarshal(out, &rep) != nil {
		return time.Time{}, errors.New("the host's answer was not readable")
	}
	if code != http.StatusOK {
		if rep.Message == "" {
			rep.Message = "the host refused (" + http.StatusText(code) + ")"
		}
		return time.Time{}, errors.New(rep.Message)
	}
	return rep.UnlockedUntil, nil
}

// handleGPGComplete: POST /api/gpg/complete {request_id, answer}. The peer
// side of the relay, for the hub only (the hub-call marker, user role, and
// refused on a host that keeps the ledger: the hub's own host is relayed to
// in process). Whatever a caller sends, this host checks the request, the
// passkey it pinned, the assertion and the sealed key itself.
func (s *Server) handleGPGComplete(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	if s.ledger != nil || r.Header.Get(fleet.HeaderHubCall) != "1" {
		writeJSONStatus(w, http.StatusForbidden, gpgCompleteReply{Code: "hub_only", Message: "this route is for a hub relaying an answer to its peer"})
		return
	}
	var b gpgCompleteBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	st, err := s.gpgHost().Complete(strings.TrimSpace(b.RequestID), b.Answer)
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, gpgCompleteReply{Code: "host_refused", Message: err.Error()})
		return
	}
	writeJSON(w, gpgCompleteReply{UnlockedUntil: st.ExpiresAt})
}
