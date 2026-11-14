package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/passkey"
)

// Approvals: an action that waits for the user's passkey (Face ID).
//
// A request is filed by a host or an agent (POST /api/approvals) or, for
// adding a passkey, by the user's own page. The hub makes the challenge, the
// user answers it in the app with a WebAuthn assertion (POST
// /api/approvals/{id}/respond, CSRF), and the hub checks it against its own
// list of passkeys as a first filter. The host that asked checks again
// against the passkeys it pinned (a later slice): the hub's check is not what
// a host trusts.
//
// A synced passkey reports counter 0, so replay protection is not the counter
// but this: one nonce in the request, an expiry (120 s) and a single use
// (an approval leaves "pending" on its first success, and three failed tries
// kill it).
//
// A request that an agent or a host files also becomes a to-do item of kind
// "approval" (modelled on "release"): the item carries the public fields
// (TodoApproval) and the app draws the Face ID button. The challenge, the
// request and the assertion stay here, in approvals.json.
//
// Who can do what (docs/security.md):
//   - POST /api/approvals: the user, a recorded agent, or a tagged worker, and
//     an agent only for its own host and only for the actions below. The
//     action's wording in the to-do is the server's, never the agent's.
//   - Everything else is s.auth (the user role only), with the CSRF header on
//     the routes that act.

// Approval states.
const (
	apPending  = "pending"
	apApproved = "approved"
	apDenied   = "denied"
	apExpired  = "expired"
)

const (
	// approvalTTL is how long a request waits for Face ID.
	approvalTTL = 120 * time.Second
	// approvalMaxTries is the failed assertions that kill a request.
	approvalMaxTries = 3
	// approvalKeep is how long a settled request is kept (for hosts and the
	// audit trail), and addUse how long an approved passkey-add stays usable.
	approvalKeep = time.Hour
	addUse       = 5 * time.Minute

	maxApprovalRequest = 4 << 10
	maxApprovalSealed  = 2 << 10
	maxApprovalSubject = 200
)

// approvalPerMinute caps the requests one host may file in a minute: a new
// request for the same action replaces the old one, so the number waiting is
// not the limit, the churn is.
var approvalPerMinute = 10

// approvalActions are the actions a request may name. agent: an agent or a
// host may file it; user: the user's own page may; todo: a to-do item is filed.
var approvalActions = map[string]struct {
	agent, user, todo bool
	title             string // %s: the host
}{
	"gpg-unlock":  {agent: true, todo: true, title: "GPG is locked on %s"},
	"passkey-add": {user: true},
}

// TodoApproval is the public part of a kind "approval" item: what is asked,
// of whom, and where it stands. The secrets of the exchange are not here.
type TodoApproval struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	Host        string    `json:"host"`
	Subject     string    `json:"subject,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type apAssertion struct {
	ClientDataJSON    string `json:"client_data_json"`
	AuthenticatorData string `json:"authenticator_data"`
	Signature         string `json:"signature"`
}

type approval struct {
	ID          string          `json:"id"`
	Action      string          `json:"action"`
	Host        string          `json:"host"`
	Subject     string          `json:"subject,omitempty"`
	RequestedBy string          `json:"requested_by,omitempty"`
	State       string          `json:"state"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
	Request     json.RawMessage `json:"request"`
	Challenge   string          `json:"challenge"` // base64url
	Tries       int             `json:"tries,omitempty"`
	TodoID      string          `json:"todo_id,omitempty"`
	Credential  string          `json:"credential,omitempty"`
	Assertion   *apAssertion    `json:"assertion,omitempty"`
	Sealed      string          `json:"sealed,omitempty"`
	Used        bool            `json:"used,omitempty"` // a passkey-add approval was spent
	Note        string          `json:"note,omitempty"`
}

func (a *approval) todoView() *TodoApproval {
	return &TodoApproval{ID: a.ID, Action: a.Action, Host: a.Host, Subject: a.Subject, RequestedBy: a.RequestedBy, State: a.State, ExpiresAt: a.ExpiresAt}
}

type approvalStore struct {
	mu    sync.Mutex
	path  string
	items []*approval
	now   func() time.Time
}

func approvalsPath(dir string) string { return filepath.Join(dir, "approvals.json") }

func openApprovals(path string) (*approvalStore, error) {
	st := &approvalStore{path: path, now: time.Now}
	if path == "" {
		return st, nil
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	var f struct {
		Items []*approval `json:"items"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	st.items = f.Items
	return st, nil
}

func (st *approvalStore) saveLocked() error {
	if st.path == "" {
		return nil
	}
	return writeFileAtomic(st.path, map[string]any{"version": 1, "items": st.items})
}

func newApprovalID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "ap" + hex.EncodeToString(b)
}

// expireLocked settles requests past their time and forgets old settled
// ones; it returns the ones that just expired.
func (st *approvalStore) expireLocked() (expired []*approval) {
	now := st.now()
	keep := st.items[:0]
	for _, a := range st.items {
		if a.State == apPending && !now.Before(a.ExpiresAt) {
			a.State, a.DecidedAt = apExpired, &now
			expired = append(expired, a)
		}
		if a.State != apPending && a.DecidedAt != nil && now.Sub(*a.DecidedAt) > approvalKeep {
			continue
		}
		keep = append(keep, a)
	}
	st.items = keep
	return expired
}

func (st *approvalStore) find(id string) *approval {
	for _, a := range st.items {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// sweepApprovals settles what expired and closes the to-do items of those.
func (s *Server) sweepApprovals() {
	if s.approvals == nil {
		return
	}
	s.approvals.mu.Lock()
	exp := s.approvals.expireLocked()
	if len(exp) > 0 {
		_ = s.approvals.saveLocked()
	}
	copies := make([]approval, len(exp))
	for i, a := range exp {
		copies[i] = *a
	}
	s.approvals.mu.Unlock()
	for i := range copies {
		s.settleApprovalTodo(&copies[i], "expired")
	}
}

// settleApprovalTodo brings the item in line with its approval: approved
// closes it done, denied and expired drop it. Safe to call twice.
func (s *Server) settleApprovalTodo(a *approval, reason string) {
	if a.TodoID == "" || s.todos == nil {
		return
	}
	_, _ = s.todos.mutate(a.TodoID, func(it *Todo) error {
		if it.Approval != nil {
			it.Approval.State = a.State
		}
		return nil
	})
	switch a.State {
	case apApproved:
		_, _ = s.todos.close(a.TodoID, TodoDone, "passkey", "")
	case apDenied, apExpired:
		_, _ = s.todos.close(a.TodoID, TodoDropped, "hub", reason)
	}
}

// approvalClosedByUser: the user closed the item without Face ID ("Not now"):
// the request is denied.
func (s *Server) approvalClosedByUser(it Todo) {
	if it.Approval == nil || s.approvals == nil {
		return
	}
	s.approvals.mu.Lock()
	a := s.approvals.find(it.Approval.ID)
	if a != nil && a.State == apPending {
		now := s.approvals.now()
		a.State, a.DecidedAt, a.Note = apDenied, &now, "not now"
		_ = s.approvals.saveLocked()
	}
	s.approvals.mu.Unlock()
}

type approvalView struct {
	ID          string          `json:"id"`
	Action      string          `json:"action"`
	Host        string          `json:"host"`
	Subject     string          `json:"subject,omitempty"`
	RequestedBy string          `json:"requested_by,omitempty"`
	State       string          `json:"state"`
	ExpiresAt   time.Time       `json:"expires_at"`
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
	Todo        string          `json:"todo,omitempty"`
	Request     json.RawMessage `json:"request"`
	Note        string          `json:"note,omitempty"`
}

func (a *approval) view() approvalView {
	return approvalView{ID: a.ID, Action: a.Action, Host: a.Host, Subject: a.Subject, RequestedBy: a.RequestedBy, State: a.State,
		ExpiresAt: a.ExpiresAt, DecidedAt: a.DecidedAt, Todo: a.TodoID, Request: a.Request, Note: a.Note}
}

func apErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
}

// approvalsOff answers when this instance keeps no passkeys (a peer: they
// live on the hub).
func (s *Server) approvalsOff(w http.ResponseWriter, r *http.Request) bool {
	if s.approvals != nil && s.passkeys != nil && s.todos != nil {
		return false
	}
	if callerOf(r).agent() {
		apErr(w, http.StatusConflict, "no_hub", "passkeys and approvals live on the hub: start this with -hub")
		return true
	}
	apErr(w, http.StatusServiceUnavailable, "passkeys_off", "this instance keeps no passkeys: use the hub")
	return true
}

// handleApprovalCreate: POST /api/approvals {action, host, subject, request}.
func (s *Server) handleApprovalCreate(w http.ResponseWriter, r *http.Request) {
	if s.approvalsOff(w, r) {
		return
	}
	var b struct {
		Action  string          `json:"action"`
		Host    string          `json:"host"`
		Subject string          `json:"subject"`
		Request json.RawMessage `json:"request"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	spec, ok := approvalActions[b.Action]
	if !ok {
		apErr(w, http.StatusBadRequest, "bad_action", "the action is not one that can be approved")
		return
	}
	c := callerOf(r)
	host := strings.ToLower(cleanID(b.Host))
	if c.agent() {
		if !spec.agent {
			apErr(w, http.StatusForbidden, "action_forbidden", "only the user asks for that approval, in the app")
			return
		}
		// An agent or a host asks for its own host only. A tagged node's host
		// is what WhoIs said; a local agent's is this one.
		if host == "" {
			host = strings.ToLower(c.host)
		}
		if c.host == "" || !sameHost(host, c.host) {
			apErr(w, http.StatusForbidden, "host_forbidden", "an approval can be asked for only by, and for, an agent's own host")
			return
		}
	} else {
		if !spec.user {
			apErr(w, http.StatusForbidden, "action_forbidden", "that approval is asked for by a host or an agent, not from the app")
			return
		}
		if host == "" {
			host = strings.ToLower(s.fleet.LocalName())
		}
	}
	if host == "" {
		apErr(w, http.StatusBadRequest, "bad_host", "the host is required")
		return
	}
	rp := s.passkeys.RPID()
	if rp == "" {
		apErr(w, http.StatusConflict, "no_passkeys", "no passkey is registered: add one in Settings, Passkeys")
		return
	}
	subject := oneLine(b.Subject, maxApprovalSubject)

	var reqBytes []byte
	if len(b.Request) > 0 && string(b.Request) != "null" {
		var obj map[string]any
		if len(b.Request) > maxApprovalRequest || json.Unmarshal(b.Request, &obj) != nil || obj == nil {
			apErr(w, http.StatusBadRequest, "bad_request", "the request is a JSON object of at most 4 KiB")
			return
		}
		// The host's own bytes, compacted: the challenge is a hash of them.
		compact, err := compactJSON(b.Request)
		if err != nil {
			apErr(w, http.StatusBadRequest, "bad_request", "the request is not valid JSON")
			return
		}
		reqBytes = compact
	}

	st := s.approvals
	st.mu.Lock()
	expired := st.expireLocked()
	now := st.now()
	recent := 0
	var old *approval
	for _, a := range st.items {
		if !sameHost(a.Host, host) {
			continue
		}
		if now.Sub(a.CreatedAt) < time.Minute {
			recent++
		}
		if a.State == apPending && a.Action == b.Action {
			old = a
		}
	}
	if old != nil && old.Subject == subject && (reqBytes == nil || bytes.Equal(old.Request, reqBytes)) {
		out := old.view()
		st.mu.Unlock()
		writeJSON(w, map[string]any{"approval": out, "duplicate": true})
		return
	}
	if recent >= approvalPerMinute {
		st.mu.Unlock()
		apErr(w, http.StatusTooManyRequests, "approval_rate", "that host is asking too often: wait a minute")
		return
	}
	// A new request for the same action replaces the one waiting: the user
	// sees one item per host and action, always the latest.
	reuse := ""
	if old != nil {
		old.State, old.DecidedAt, old.Note = apExpired, &now, "replaced by a newer request"
		reuse = old.TodoID
	}
	a := &approval{ID: newApprovalID(), Action: b.Action, Host: host, Subject: subject, RequestedBy: s.by(r, ""),
		State: apPending, CreatedAt: now, ExpiresAt: now.Add(approvalTTL)}
	if reqBytes == nil {
		nonce := make([]byte, 16)
		_, _ = rand.Read(nonce)
		reqBytes, _ = json.Marshal(map[string]any{ // json.Marshal sorts map keys: canonical
			"v": 1, "action": a.Action, "id": a.ID, "host": host, "subject": subject, "rp": rp,
			"nonce": passkey.B64(nonce), "exp": a.ExpiresAt.Unix(),
		})
	}
	a.Request = reqBytes
	a.Challenge = passkey.B64(passkey.ApprovalChallenge(reqBytes))
	st.items = append(st.items, a)
	_ = st.saveLocked()
	st.mu.Unlock()
	for _, e := range expired {
		cp := *e
		s.settleApprovalTodo(&cp, "expired")
	}

	var item *Todo
	if spec.todo && reuse != "" {
		if it, err := s.todos.mutate(reuse, func(it *Todo) error {
			if !it.active() || it.Approval == nil {
				return errTodoClosed
			}
			it.Approval = a.todoView()
			it.Context = approvalContext(a, c.agent())
			return nil
		}); err == nil {
			item = &it
			st.mu.Lock()
			a.TodoID = it.ID
			_ = st.saveLocked()
			st.mu.Unlock()
		}
	}
	if spec.todo && item == nil {
		it := Todo{
			Question: fmt.Sprintf(spec.title, host), Kind: "approval", CreatedBy: s.by(r, ""), Approval: a.todoView(),
			Context: approvalContext(a, c.agent()),
		}
		if c.agent() {
			it.Source = SourceAgent
			if c.pane != "" {
				it.CreatedByID = s.replyID(c)
			}
		}
		out, dup, err := s.todos.add(it)
		if err != nil {
			st.mu.Lock()
			st.items = removeApproval(st.items, a.ID)
			_ = st.saveLocked()
			st.mu.Unlock()
			todoErr(w, err)
			return
		}
		if !dup {
			item = &out
			st.mu.Lock()
			a.TodoID = out.ID
			_ = st.saveLocked()
			st.mu.Unlock()
		}
	}
	s.audit(r, ledger.AuditEvent{Action: "approval.request", Todo: a.TodoID, Host: host, OK: true, Detail: a.Action + " " + a.ID})
	st.mu.Lock()
	out := a.view()
	st.mu.Unlock()
	writeJSONStatus(w, http.StatusCreated, map[string]any{"approval": out, "todo": item})
}

func removeApproval(in []*approval, id string) []*approval {
	out := in[:0]
	for _, a := range in {
		if a.ID != id {
			out = append(out, a)
		}
	}
	return out
}

func compactJSON(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// approvalContext is the server's wording for the item.
func approvalContext(a *approval, agent bool) string {
	var sb strings.Builder
	switch a.Action {
	case "gpg-unlock":
		fmt.Fprintf(&sb, "Unlocking the GPG key on %s needs your passkey (Face ID).", a.Host)
	default:
		fmt.Fprintf(&sb, "%s on %s needs your passkey (Face ID).", a.Action, a.Host)
	}
	if a.Subject != "" {
		fmt.Fprintf(&sb, "\n\nAsked for: %s", a.Subject)
	}
	if a.RequestedBy != "" {
		who := "Asked by"
		if !agent {
			who = "Asked from"
		}
		fmt.Fprintf(&sb, "\n\n%s %s. The request lasts %d minutes.", who, a.RequestedBy, int(approvalTTL.Minutes()))
	}
	return sb.String()
}

// approvalDetail is what the app needs to ask for the passkey.
func (s *Server) approvalDetail(a *approval) map[string]any {
	creds := []map[string]string{}
	for _, p := range s.passkeys.List() {
		creds = append(creds, map[string]string{"id": p.ID, "name": p.Name})
	}
	return map[string]any{"approval": a.view(), "challenge": a.Challenge, "rp_id": s.passkeys.RPID(), "credentials": creds}
}

// handleApprovalGet: GET /api/approvals/{id}. The user only.
func (s *Server) handleApprovalGet(w http.ResponseWriter, r *http.Request) {
	if s.approvalsOff(w, r) {
		return
	}
	s.sweepApprovals()
	st := s.approvals
	st.mu.Lock()
	a := st.find(r.PathValue("id"))
	if a == nil {
		st.mu.Unlock()
		apErr(w, http.StatusNotFound, "not_found", "no such approval")
		return
	}
	out := s.approvalDetail(a)
	st.mu.Unlock()
	out["csrf"] = s.csrfToken(r)
	writeJSON(w, out)
}

// handleApprovalRespond: POST /api/approvals/{id}/respond (CSRF). Body
// {"deny":true} or the assertion: {credential_id, client_data_json,
// authenticator_data, signature, sealed?} (base64url).
func (s *Server) handleApprovalRespond(w http.ResponseWriter, r *http.Request) {
	if s.approvalsOff(w, r) || !s.relGuard(w, r, true) {
		return
	}
	var b struct {
		Deny              bool   `json:"deny"`
		CredentialID      string `json:"credential_id"`
		ClientDataJSON    string `json:"client_data_json"`
		AuthenticatorData string `json:"authenticator_data"`
		Signature         string `json:"signature"`
		Sealed            string `json:"sealed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	s.sweepApprovals()
	st := s.approvals
	st.mu.Lock()
	a := st.find(r.PathValue("id"))
	if a == nil {
		st.mu.Unlock()
		apErr(w, http.StatusNotFound, "not_found", "no such approval")
		return
	}
	if a.State != apPending {
		msg := "this request was already settled"
		if a.State == apExpired {
			msg = "this request expired: ask again"
		}
		st.mu.Unlock()
		apErr(w, http.StatusConflict, "not_pending", msg)
		return
	}
	finish := func(state, note string) {
		now := st.now()
		a.State, a.DecidedAt, a.Note = state, &now, note
		_ = st.saveLocked()
	}
	if b.Deny {
		finish(apDenied, "denied")
		cp := *a
		st.mu.Unlock()
		s.settleApprovalTodo(&cp, "denied")
		s.audit(r, ledger.AuditEvent{Action: "approval.deny", Todo: cp.TodoID, Host: cp.Host, OK: true, Detail: cp.Action + " " + cp.ID})
		writeJSON(w, map[string]any{"approval": cp.view()})
		return
	}
	fail := func(code, msg string) {
		a.Tries++
		left := approvalMaxTries - a.Tries
		var cp approval
		killed := left <= 0
		if killed {
			finish(apDenied, "too many failed tries")
			msg += ". That was the last try: ask again."
		} else {
			_ = st.saveLocked()
		}
		cp = *a
		st.mu.Unlock()
		if killed {
			s.settleApprovalTodo(&cp, "too many failed tries")
		}
		s.audit(r, ledger.AuditEvent{Action: "approval.refused", Todo: cp.TodoID, Host: cp.Host, Detail: cp.Action + " " + cp.ID + ": " + code})
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"code": code, "message": msg, "tries_left": max(left, 0), "approval": cp.view()})
	}
	if len(b.Sealed) > maxApprovalSealed {
		st.mu.Unlock()
		apErr(w, http.StatusRequestEntityTooLarge, "too_large", "the sealed key is too large")
		return
	}
	pk, ok := s.passkeys.Get(b.CredentialID)
	if !ok {
		fail("unknown_passkey", "That passkey is not registered with this hub")
		return
	}
	cd, e1 := passkey.UnB64(b.ClientDataJSON)
	ad, e2 := passkey.UnB64(b.AuthenticatorData)
	sig, e3 := passkey.UnB64(b.Signature)
	chal, e4 := passkey.UnB64(a.Challenge)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		fail("bad_assertion", "The passkey's answer was not readable")
		return
	}
	ctr, err := passkey.VerifyAssertion(s.passkeyOpts(s.passkeys.RPID()), pk.PublicKey, pk.Counter, chal, cd, ad, sig)
	if err != nil {
		fail("assertion_failed", "The passkey was not accepted: "+err.Error())
		return
	}
	if !st.now().Before(a.ExpiresAt) {
		finish(apExpired, "")
		cp := *a
		st.mu.Unlock()
		s.settleApprovalTodo(&cp, "expired")
		apErr(w, http.StatusConflict, "not_pending", "this request expired: ask again")
		return
	}
	a.Credential = pk.ID
	a.Assertion = &apAssertion{ClientDataJSON: b.ClientDataJSON, AuthenticatorData: b.AuthenticatorData, Signature: b.Signature}
	a.Sealed = b.Sealed
	finish(apApproved, "")
	cp := *a
	st.mu.Unlock()
	s.passkeys.Used(pk.ID, ctr)
	s.settleApprovalTodo(&cp, "")
	s.audit(r, ledger.AuditEvent{Action: "approval.approve", Todo: cp.TodoID, Host: cp.Host, OK: true, Detail: cp.Action + " " + cp.ID + " with " + pk.Name})
	writeJSON(w, map[string]any{"approval": cp.view()})
}

// spendAddApproval checks that id is an approved, unspent passkey-add
// approval (and spends it when spend is set).
func (s *Server) spendAddApproval(id string, spend bool) error {
	st := s.approvals
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expireLocked()
	a := st.find(id)
	switch {
	case a == nil || a.Action != "passkey-add":
		return errors.New("Adding another passkey needs an existing passkey's approval first")
	case a.State != apApproved || a.Used:
		return errors.New("That approval was already used or not given: approve with an existing passkey again")
	case a.DecidedAt == nil || st.now().Sub(*a.DecidedAt) > addUse:
		return errors.New("That approval is too old: approve with an existing passkey again")
	}
	if spend {
		a.Used = true
		_ = st.saveLocked()
	}
	return nil
}

func (s *Server) unspendAddApproval(id string) {
	st := s.approvals
	st.mu.Lock()
	if a := st.find(id); a != nil {
		a.Used = false
		_ = st.saveLocked()
	}
	st.mu.Unlock()
}
