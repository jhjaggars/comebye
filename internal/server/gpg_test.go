package server

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/gpgunlock"
	"github.com/jhjaggars/comebye/internal/gpgunlock/gpgtest"
	"github.com/jhjaggars/comebye/internal/passkey"
	"github.com/jhjaggars/comebye/internal/passkey/passkeytest"
)

// gpgRig is a hub whose own host has GPG unlock set up for a throwaway key,
// the registered passkey pinned.
type gpgRig struct {
	s    *Server
	h    map[string]string
	a    *passkeytest.Authenticator
	host *gpgunlock.Host
	key  *gpgtest.Key
	prf  []byte
}

func newGPGRig(t *testing.T) *gpgRig {
	t.Helper()
	key := gpgtest.New(t, 8*time.Hour)
	s, h, a := passkeyServer(t)
	host := &gpgunlock.Host{Dir: t.TempDir(), GPG: key.GPG}
	s.cfg.GPGHost = host
	grips, err := key.GPG.Keygrips(key.FPR)
	if err != nil {
		t.Fatal(err)
	}
	pk := s.passkeys.List()[0]
	if err := host.Setup(gpgunlock.Config{Host: "desk", FPR: key.FPR, Keygrips: grips},
		gpgunlock.Pinned{RPID: testRPID, Passkeys: []gpgunlock.PinnedKey{{ID: pk.ID, PublicKey: pk.PublicKey}}}); err != nil {
		t.Fatal(err)
	}
	prf := make([]byte, 32)
	_, _ = rand.Read(prf)
	return &gpgRig{s: s, h: h, a: a, host: host, key: key, prf: prf}
}

// file files the host's request with the hub, as `comebye gpg unlock` does.
func (r *gpgRig) file(t *testing.T, setup bool) (reqID string, out struct {
	Approval approvalView `json:"approval"`
	Todo     *Todo        `json:"todo"`
}, req []byte) {
	t.Helper()
	id, req, err := r.host.NewRequest(testRPID, setup)
	if err != nil {
		t.Fatal(err)
	}
	action := "gpg-unlock"
	if setup {
		action = "gpg-setup"
	}
	w := do(r.s, "POST", "/api/approvals", agentHdr("w2:p1"), jb(map[string]any{"action": action, "subject": "key", "request": json.RawMessage(req)}))
	if w.Code != 201 {
		t.Fatalf("file = %d %s", w.Code, w.Body)
	}
	decodeJSON(t, w, &out)
	return id, out, req
}

// answer plays the app: assertion for the approval's challenge, K sealed to the request's key, the blob at setup.
func (r *gpgRig) answer(t *testing.T, apID string, req []byte, prf []byte, pass string, blob bool) (int, string) {
	t.Helper()
	var o struct {
		Curve string `json:"curve"`
		Pub   string `json:"pub"`
	}
	_ = json.Unmarshal(req, &o)
	k, err := gpgunlock.DeriveK(prf)
	if err != nil {
		t.Fatal(err)
	}
	ch := passkey.ApprovalChallenge(req)
	pub, _ := passkey.UnB64(o.Pub)
	sealed, err := gpgunlock.Seal(o.Curve, pub, ch, k)
	if err != nil {
		t.Fatal(err)
	}
	cd, ad, sig := r.a.Get(ch)
	body := map[string]any{"credential_id": r.a.ID(), "client_data_json": passkey.B64(cd), "authenticator_data": passkey.B64(ad), "signature": passkey.B64(sig), "sealed": sealed}
	if blob {
		cfg, _ := r.host.Config()
		b, _ := gpgunlock.EncryptBlob(k, cfg.Host, cfg.Keygrips, []byte(pass))
		body["blob"] = b
	}
	w := do(r.s, "POST", "/api/approvals/"+apID+"/respond", r.h, jb(body))
	return w.Code, w.Body.String()
}

func TestGPGSetupAndUnlockRelay(t *testing.T) {
	r := newGPGRig(t)
	// Setup: the item says so, the answer carries the blob, the host unlocks.
	_, ap, req := r.file(t, true)
	if ap.Todo == nil || ap.Todo.Question != "Set up GPG unlock on desk" || ap.Approval.Action != "gpg-setup" {
		t.Fatalf("setup item = %+v", ap.Todo)
	}
	if !ap.Approval.ExpiresAt.After(time.Now().Add(5 * time.Minute)) {
		t.Fatalf("a setup request waits too briefly: %v", ap.Approval.ExpiresAt)
	}
	code, body := r.answer(t, ap.Approval.ID, req, r.prf, "wrong passphrase", true)
	if code != 409 || !strings.Contains(body, "host_refused") || !strings.Contains(body, "does not unlock the key") {
		t.Fatalf("wrong passphrase = %d %s", code, body)
	}
	if r.host.HasBlob() {
		t.Fatal("a wrong passphrase stored a blob")
	}
	it, _ := r.s.todos.get(ap.Todo.ID)
	if it.State != TodoOpen || it.Approval.State != apDenied || !strings.Contains(it.Approval.Note, "does not unlock the key") {
		t.Fatalf("a refused item must stay open with the reason: %+v / %+v", it, it.Approval)
	}
	// A second try on the spent request is refused by the hub (settled).
	if code, _ := r.answer(t, ap.Approval.ID, req, r.prf, r.key.Pass, true); code != 409 {
		t.Fatalf("a spent request answered again = %d", code)
	}
	// A new request reuses the open item.
	_, ap2, req2 := r.file(t, true)
	if ap2.Todo == nil || ap2.Todo.ID != ap.Todo.ID || ap2.Todo.Approval.ID != ap2.Approval.ID || ap2.Todo.Approval.Note != "" {
		t.Fatalf("new request did not reuse the item: %+v", ap2.Todo)
	}
	code, body = r.answer(t, ap2.Approval.ID, req2, r.prf, r.key.Pass, true)
	if code != 200 || !strings.Contains(body, "unlocked_until") || !strings.Contains(body, `"approved"`) {
		t.Fatalf("setup = %d %s", code, body)
	}
	if !r.host.HasBlob() {
		t.Fatal("no blob after setup")
	}
	if it, _ := r.s.todos.get(ap.Todo.ID); it.State != TodoDone {
		t.Fatalf("item after setup = %+v", it)
	}
	if st, err := r.host.Status(); err != nil || st.Locked || st.UnlockedTo == nil {
		t.Fatalf("status = %+v %v", st, err)
	}
	// What the hub keeps holds neither the passphrase, K, the PRF output nor the blob.
	k, _ := gpgunlock.DeriveK(r.prf)
	blobJSON, _ := os.ReadFile(r.host.Dir + "/gpg-blob.json")
	var stored gpgunlock.Blob
	_ = json.Unmarshal(blobJSON, &stored)
	for _, f := range []string{r.s.cfg.StateDir + "/approvals.json", r.s.cfg.StateDir + "/todos.json"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for what, secret := range map[string]string{"passphrase": r.key.Pass, "prf": passkey.B64(r.prf), "K": passkey.B64(k), "blob": stored.CT} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s holds the %s", f, what)
			}
		}
	}

	// Unlock later: lock, a new request, the same PRF opens the stored blob.
	if err := r.host.Lock(); err != nil {
		t.Fatal(err)
	}
	_, un, ureq := r.file(t, false)
	if un.Todo == nil || un.Todo.Question != "GPG is locked on desk" {
		t.Fatalf("unlock item = %+v", un.Todo)
	}
	if code, body := r.answer(t, un.Approval.ID, ureq, r.prf, "", false); code != 200 {
		t.Fatalf("unlock = %d %s", code, body)
	}
	if st, _ := r.host.Status(); st.Locked {
		t.Fatal("still locked after the unlock")
	}
	// Another passkey's PRF (K) does not open the stored passphrase.
	_ = r.host.Lock()
	_, bad, breq := r.file(t, false)
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	if code, body := r.answer(t, bad.Approval.ID, breq, other, "", false); code != 409 || !strings.Contains(body, "host_refused") {
		t.Fatalf("another PRF = %d %s", code, body)
	}
	if st, _ := r.host.Status(); !st.Locked {
		t.Fatal("unlocked with the wrong K")
	}
}

// A passkey the hub knows but the host never pinned is refused by the host.
func TestGPGHostDoesNotTrustTheHubsList(t *testing.T) {
	r := newGPGRig(t)
	other := passkeytest.New(testRPID)
	if w := register(t, r.s, r.h, other, "second", approvedAdd(t, r), true); w.Code != 201 {
		t.Fatalf("second passkey = %d %s", w.Code, w.Body)
	}
	_, ap, req := r.file(t, true)
	r.a = other
	code, body := r.answer(t, ap.Approval.ID, req, r.prf, r.key.Pass, true)
	if code != 409 || !strings.Contains(body, "host_refused") || !strings.Contains(body, "not one this host trusts") {
		t.Fatalf("unpinned passkey = %d %s", code, body)
	}
	if r.host.HasBlob() {
		t.Fatal("a blob was stored")
	}
}

func approvedAdd(t *testing.T, r *gpgRig) string {
	t.Helper()
	w := do(r.s, "POST", "/api/approvals", r.h, `{"action":"passkey-add","subject":"Add"}`)
	if w.Code != 201 {
		t.Fatalf("add approval = %d %s", w.Code, w.Body)
	}
	var out struct {
		Approval approvalView `json:"approval"`
	}
	decodeJSON(t, w, &out)
	if c, b := assertFor(t, r.s, r.h, out.Approval.ID, r.a); c != 200 {
		t.Fatalf("approve add = %d %s", c, b)
	}
	return out.Approval.ID
}

// The peer route of the relay is the hub's own call only.
func TestGPGCompleteRouteIsHubOnly(t *testing.T) {
	s, _, _ := todoServer(t)
	body := `{"request_id":"gu_00","answer":{}}`
	if w := do(s, "POST", "/api/gpg/complete", agentHdr("w2:p1"), body); w.Code != 403 {
		t.Fatalf("agent = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/gpg/complete", userHdr, body); w.Code != 403 || !strings.Contains(w.Body.String(), "hub_only") {
		t.Fatalf("user without the hub marker = %d %s", w.Code, w.Body)
	}
}

// A peer host takes the hub's relayed answer on /api/gpg/complete and checks
// it itself: the same refusals, and the same unlock.
func TestGPGPeerRoute(t *testing.T) {
	r := newGPGRig(t)
	id, _, req := r.file(t, true)
	r.s.ledger = nil // a peer keeps no ledger
	var o struct {
		Curve string `json:"curve"`
		Pub   string `json:"pub"`
	}
	_ = json.Unmarshal(req, &o)
	k, _ := gpgunlock.DeriveK(r.prf)
	ch := passkey.ApprovalChallenge(req)
	pub, _ := passkey.UnB64(o.Pub)
	sealed, _ := gpgunlock.Seal(o.Curve, pub, ch, k)
	cd, ad, sig := r.a.Get(ch)
	cfg, _ := r.host.Config()
	blob, _ := gpgunlock.EncryptBlob(k, cfg.Host, cfg.Keygrips, []byte(r.key.Pass))
	ans := gpgunlock.Answer{CredentialID: r.a.ID(), ClientDataJSON: passkey.B64(cd), AuthenticatorData: passkey.B64(ad), Signature: passkey.B64(sig), Sealed: sealed, Blob: &blob}
	hdr := map[string]string{"Tailscale-User-Login": "me@example.com", "X-Comebye-Hub-Call": "1"}
	bad := ans
	bad.Signature = passkey.B64([]byte("nope"))
	if w := do(r.s, "POST", "/api/gpg/complete", hdr, jb(gpgCompleteBody{RequestID: id, Answer: bad})); w.Code != 409 || !strings.Contains(w.Body.String(), "host_refused") {
		t.Fatalf("bad answer = %d %s", w.Code, w.Body)
	}
	// The failed try spent the request.
	if w := do(r.s, "POST", "/api/gpg/complete", hdr, jb(gpgCompleteBody{RequestID: id, Answer: ans})); w.Code != 409 || !strings.Contains(w.Body.String(), "already used") {
		t.Fatalf("spent request = %d %s", w.Code, w.Body)
	}
	id2, _, req2 := r.file(t, true)
	ch = passkey.ApprovalChallenge(req2)
	pub2 := func() []byte { _ = json.Unmarshal(req2, &o); p, _ := passkey.UnB64(o.Pub); return p }()
	ans.Sealed, _ = gpgunlock.Seal(o.Curve, pub2, ch, k)
	cd, ad, sig = r.a.Get(ch)
	ans.ClientDataJSON, ans.AuthenticatorData, ans.Signature = passkey.B64(cd), passkey.B64(ad), passkey.B64(sig)
	w := do(r.s, "POST", "/api/gpg/complete", hdr, jb(gpgCompleteBody{RequestID: id2, Answer: ans}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "unlocked_until") {
		t.Fatalf("good answer = %d %s", w.Code, w.Body)
	}
	if st, _ := r.host.Status(); st.Locked {
		t.Fatal("still locked")
	}
}

// The host takes its request back when the key was unlocked another way
// (pinentry): the item closes, the request is spent, a second call is safe,
// and another host's agent cannot do it.
func TestGPGWithdraw(t *testing.T) {
	r := newGPGRig(t)
	_, ap, req := r.file(t, false)
	id := ap.Approval.ID
	r.s.approvals.mu.Lock()
	r.s.approvals.find(id).Host = "elsewhere"
	r.s.approvals.mu.Unlock()
	if w := do(r.s, "POST", "/api/approvals/"+id+"/withdraw", agentHdr("w2:p1"), `{}`); w.Code != 403 || !strings.Contains(w.Body.String(), "host_forbidden") {
		t.Fatalf("another host's agent: %d %s", w.Code, w.Body)
	}
	r.s.approvals.mu.Lock()
	r.s.approvals.find(id).Host = ap.Approval.Host
	r.s.approvals.mu.Unlock()
	if w := do(r.s, "POST", "/api/approvals/"+id+"/withdraw", agentHdr("w2:p1"), `{}`); w.Code != 200 {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body)
	}
	if it, _ := r.s.todos.get(ap.Todo.ID); it.State != TodoDropped {
		t.Fatalf("the item is %q after the withdrawal", it.State)
	}
	if w := do(r.s, "POST", "/api/approvals/"+id+"/withdraw", agentHdr("w2:p1"), `{}`); w.Code != 200 {
		t.Fatalf("withdraw twice: %d %s", w.Code, w.Body)
	}
	if code, body := r.answer(t, id, req, r.prf, "", false); code != 409 || !strings.Contains(body, "not_pending") {
		t.Fatalf("answering a withdrawn request: %d %s", code, body)
	}
	if w := do(r.s, "POST", "/api/approvals/ap_nope/withdraw", agentHdr("w2:p1"), `{}`); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
}
