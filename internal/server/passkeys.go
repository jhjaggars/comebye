package server

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/passkey"
)

// Passkeys: the user's passkeys for approving actions with Face ID.
//
//	GET    /api/passkeys                     the public keys (agents and hosts too: a host pins them)
//	POST   /api/passkeys/register/begin      the user, CSRF: options for navigator.credentials.create
//	POST   /api/passkeys/register/finish     the user, CSRF: the attestation, with the PRF report
//	PUT    /api/passkeys/{id}                the user, CSRF: rename {name}
//	DELETE /api/passkeys/{id}                the user, CSRF
//
// The RP ID is the host name the app was opened on (the hub's *.ts.net name):
// the first passkey records it and the hub refuses to register on another
// name. localhost is allowed only under -test-mode. PRF is required: a
// passkey that does not report it is refused, with the message that sends the
// user to the iOS setting. The hub cannot see the PRF output, so that check
// is on an honest client only; the real enforcement is that a host's setup
// (a later slice) cannot make its blob without PRF.
//
// A second and later passkey needs an existing passkey's approval first (an
// approved "passkey-add" approval, spent by the registration).

const (
	regChalTTL = 5 * time.Minute
	regChalMax = 8

	// NoPRFMessage is what the user sees when a passkey cannot do PRF.
	NoPRFMessage = "This passkey cannot do PRF. In iOS, turn on Passwords under Settings, General, AutoFill & Passwords, AutoFill From."
)

type regChal struct {
	challenge string
	rp        string
	approval  string
	expires   time.Time
}

type regState struct {
	mu    sync.Mutex
	chals map[string]regChal
}

func (s *Server) passkeyOpts(rp string) passkey.Options {
	return passkey.Options{RPID: rp, AllowLocalhost: s.cfg.TestMode}
}

// rpFor is the RP ID for a request: the host name it was made to.
func (s *Server) rpFor(r *http.Request) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(r.Host, "."))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	switch {
	case host == "":
		return "", errors.New("the request has no host name")
	case host == "localhost":
		if !s.cfg.TestMode {
			return "", errors.New("passkeys need the hub's own host name (its *.ts.net name), not localhost: open the app at the name Tailscale Serve gives it")
		}
		return host, nil
	case net.ParseIP(host) != nil || !strings.Contains(host, "."):
		return "", errors.New("passkeys need the hub's host name (its *.ts.net name): open the app at that name, not an address")
	}
	return host, nil
}

type passkeyView struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Fingerprint    string     `json:"fingerprint"`
	PublicKey      string     `json:"public_key"`
	Alg            int        `json:"alg"`
	RPID           string     `json:"rp_id"`
	RPIDHash       string     `json:"rp_id_hash"`
	Counter        uint32     `json:"counter"`
	BackupEligible bool       `json:"backup_eligible"`
	BackedUp       bool       `json:"backed_up"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
}

func passkeyViewOf(p passkey.Passkey, rp string) passkeyView {
	return passkeyView{ID: p.ID, Name: p.Name, Fingerprint: passkey.Fingerprint(p.PublicKey), PublicKey: p.PublicKey, Alg: -7, RPID: rp,
		RPIDHash: p.RPIDHash, Counter: p.Counter, BackupEligible: p.BackupEligible, BackedUp: p.BackedUp, CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt}
}

func (s *Server) passkeysOff(w http.ResponseWriter, r *http.Request) bool {
	if s.passkeys != nil && s.approvals != nil {
		return false
	}
	return s.approvalsOff(w, r)
}

// handlePasskeyList: GET /api/passkeys. The CSRF token goes to the user only.
func (s *Server) handlePasskeyList(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w, r) {
		return
	}
	rp := s.passkeys.RPID()
	list := []passkeyView{}
	for _, p := range s.passkeys.List() {
		list = append(list, passkeyViewOf(p, rp))
	}
	out := map[string]any{"rp_id": rp, "passkeys": list}
	if !callerOf(r).agent() {
		out["csrf"] = s.csrfToken(r)
		out["max"] = passkey.MaxPasskeys
	}
	writeJSON(w, out)
}

func (s *Server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w, r) || !s.relGuard(w, r, true) {
		return
	}
	var b struct {
		Approval string `json:"approval"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b)
	rp, err := s.rpFor(r)
	if err != nil {
		apErr(w, http.StatusBadRequest, "bad_host", err.Error())
		return
	}
	if cur := s.passkeys.RPID(); cur != "" && cur != rp {
		apErr(w, http.StatusConflict, "wrong_host", "this hub's passkeys belong to "+cur+": open the app at https://"+cur+" to add another")
		return
	}
	list := s.passkeys.List()
	if len(list) >= passkey.MaxPasskeys {
		apErr(w, http.StatusConflict, "too_many", "the passkey list is full: remove one first")
		return
	}
	if len(list) > 0 {
		if err := s.spendAddApproval(b.Approval, false); err != nil {
			apErr(w, http.StatusForbidden, "approval_needed", err.Error())
			return
		}
	}
	ch := make([]byte, 32)
	_, _ = rand.Read(ch)
	c := passkey.B64(ch)
	s.reg.mu.Lock()
	now := time.Now()
	for k, v := range s.reg.chals {
		if now.After(v.expires) {
			delete(s.reg.chals, k)
		}
	}
	if len(s.reg.chals) >= regChalMax {
		s.reg.mu.Unlock()
		apErr(w, http.StatusTooManyRequests, "busy", "too many registrations are open: try again in a minute")
		return
	}
	s.reg.chals[c] = regChal{challenge: c, rp: rp, approval: b.Approval, expires: now.Add(regChalTTL)}
	s.reg.mu.Unlock()
	// A fresh user handle each time: with a fixed one, a second passkey made
	// in the same Passwords account would replace the first.
	uid := make([]byte, 16)
	_, _ = rand.Read(uid)
	writeJSON(w, map[string]any{"publicKey": map[string]any{
		"challenge":              c,
		"rp":                     map[string]string{"id": rp, "name": "Comebye"},
		"user":                   map[string]string{"id": passkey.B64(uid), "name": "comebye@" + rp, "displayName": "Comebye"},
		"pubKeyCredParams":       []map[string]any{{"type": "public-key", "alg": -7}},
		"authenticatorSelection": map[string]string{"residentKey": "required", "userVerification": "required"},
		"attestation":            "none",
		"timeout":                60000,
		"extensions":             map[string]any{"prf": map[string]any{}},
	}})
}

func (s *Server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w, r) || !s.relGuard(w, r, true) {
		return
	}
	var b struct {
		Challenge         string `json:"challenge"`
		ID                string `json:"id"`
		ClientDataJSON    string `json:"client_data_json"`
		AttestationObject string `json:"attestation_object"`
		PRF               struct {
			Enabled bool `json:"enabled"`
			Result  bool `json:"result"`
		} `json:"prf"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	s.reg.mu.Lock()
	rc, ok := s.reg.chals[b.Challenge]
	delete(s.reg.chals, b.Challenge) // one try per challenge
	s.reg.mu.Unlock()
	if !ok || time.Now().After(rc.expires) {
		apErr(w, http.StatusConflict, "stale_challenge", "this registration timed out: start again")
		return
	}
	if !b.PRF.Enabled || !b.PRF.Result {
		apErr(w, http.StatusBadRequest, "no_prf", NoPRFMessage)
		return
	}
	rp, err := s.rpFor(r)
	if err != nil || rp != rc.rp {
		apErr(w, http.StatusConflict, "wrong_host", "the registration was started on another host name: start again")
		return
	}
	ch, e1 := passkey.UnB64(rc.challenge)
	cd, e2 := passkey.UnB64(b.ClientDataJSON)
	att, e3 := passkey.UnB64(b.AttestationObject)
	if e1 != nil || e2 != nil || e3 != nil {
		apErr(w, http.StatusBadRequest, "bad_registration", "the passkey's answer was not readable")
		return
	}
	reg, err := passkey.VerifyRegistration(s.passkeyOpts(rp), ch, cd, att)
	if err != nil {
		apErr(w, http.StatusBadRequest, "registration_failed", "The passkey was not accepted: "+err.Error())
		return
	}
	if b.ID != "" && b.ID != reg.ID {
		apErr(w, http.StatusBadRequest, "registration_failed", "The passkey's id does not match its registration")
		return
	}
	gated := len(s.passkeys.List()) > 0
	if gated {
		if err := s.spendAddApproval(rc.approval, true); err != nil {
			apErr(w, http.StatusForbidden, "approval_needed", err.Error())
			return
		}
	}
	p, err := s.passkeys.Add(rp, reg, b.Name)
	if err != nil {
		if gated {
			s.unspendAddApproval(rc.approval)
		}
		code, status := "registration_failed", http.StatusBadRequest
		switch {
		case errors.Is(err, passkey.ErrDuplicate):
			code, status = "duplicate", http.StatusConflict
		case errors.Is(err, passkey.ErrRPMoved):
			code, status = "wrong_host", http.StatusConflict
		}
		apErr(w, status, code, capitalize(err.Error()))
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "passkey.add", OK: true, Detail: p.Name + " " + passkey.Fingerprint(p.PublicKey)})
	writeJSONStatus(w, http.StatusCreated, map[string]any{"passkey": passkeyViewOf(p, rp)})
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) handlePasskeyRename(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w, r) || !s.relGuard(w, r, true) {
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	p, err := s.passkeys.Rename(r.PathValue("id"), b.Name)
	switch {
	case errors.Is(err, passkey.ErrNotFound):
		apErr(w, http.StatusNotFound, "not_found", "no such passkey")
	case err != nil:
		apErr(w, http.StatusBadRequest, "bad_name", capitalize(err.Error()))
	default:
		s.audit(r, ledger.AuditEvent{Action: "passkey.rename", OK: true, Detail: p.Name})
		writeJSON(w, map[string]any{"passkey": passkeyViewOf(p, s.passkeys.RPID())})
	}
}

func (s *Server) handlePasskeyRemove(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w, r) || !s.relGuard(w, r, true) {
		return
	}
	id := r.PathValue("id")
	p, _ := s.passkeys.Get(id)
	if err := s.passkeys.Remove(id); err != nil {
		if errors.Is(err, passkey.ErrNotFound) {
			apErr(w, http.StatusNotFound, "not_found", "no such passkey")
			return
		}
		apErr(w, http.StatusInternalServerError, "save_failed", capitalize(err.Error()))
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "passkey.remove", OK: true, Detail: p.Name + " " + passkey.Fingerprint(p.PublicKey)})
	writeJSON(w, map[string]any{"removed": id})
}
