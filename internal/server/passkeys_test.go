package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/passkey"
	"github.com/jhjaggars/comebye/internal/passkey/passkeytest"
)

const testRPID = "example.com" // httptest's default Host

func csrfFor(t *testing.T, s *Server) map[string]string {
	t.Helper()
	w := do(s, "GET", "/api/passkeys", userHdr, "")
	var l struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &l)
	if w.Code != 200 || l.CSRF == "" {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	return map[string]string{"Tailscale-User-Login": "me@example.com", HeaderCSRF: l.CSRF}
}

func jb(v any) string { b, _ := json.Marshal(v); return string(b) }

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
}

// register runs begin and finish with a fake authenticator.
func register(t *testing.T, s *Server, h map[string]string, a *passkeytest.Authenticator, name, approval string, prf bool) *httptest.ResponseRecorder {
	t.Helper()
	w := do(s, "POST", "/api/passkeys/register/begin", h, jb(map[string]string{"approval": approval}))
	if w.Code != 200 {
		return w
	}
	var begin struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
			Extensions map[string]any `json:"extensions"`
		} `json:"publicKey"`
	}
	decodeJSON(t, w, &begin)
	if begin.PublicKey.RP.ID != testRPID || begin.PublicKey.Extensions["prf"] == nil {
		t.Fatalf("begin = %s", w.Body)
	}
	ch, _ := passkey.UnB64(begin.PublicKey.Challenge)
	cd, att := a.Create(ch)
	return do(s, "POST", "/api/passkeys/register/finish", h, jb(map[string]any{
		"challenge": begin.PublicKey.Challenge, "id": a.ID(), "client_data_json": passkey.B64(cd), "attestation_object": passkey.B64(att),
		"prf": map[string]bool{"enabled": prf, "result": prf}, "name": name,
	}))
}

// assertFor answers an approval's challenge with a.
func assertFor(t *testing.T, s *Server, h map[string]string, id string, a *passkeytest.Authenticator) (code int, body string) {
	t.Helper()
	g := do(s, "GET", "/api/approvals/"+id, userHdr, "")
	var d struct {
		Challenge string `json:"challenge"`
	}
	decodeJSON(t, g, &d)
	ch, _ := passkey.UnB64(d.Challenge)
	cd, ad, sig := a.Get(ch)
	w := do(s, "POST", "/api/approvals/"+id+"/respond", h, jb(map[string]string{
		"credential_id": a.ID(), "client_data_json": passkey.B64(cd), "authenticator_data": passkey.B64(ad), "signature": passkey.B64(sig), "sealed": "c2VhbGVk",
	}))
	return w.Code, w.Body.String()
}

func passkeyServer(t *testing.T) (*Server, map[string]string, *passkeytest.Authenticator) {
	t.Helper()
	s, _, _ := todoServer(t)
	s.todos.rateMax = -1
	h := csrfFor(t, s)
	a := passkeytest.New(testRPID)
	if w := register(t, s, h, a, "iPhone", "", true); w.Code != 201 {
		t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	return s, h, a
}

func TestPasskeyRegistration(t *testing.T) {
	s, _, _ := todoServer(t)
	h := csrfFor(t, s)

	// Without the CSRF token nothing is made.
	if w := do(s, "POST", "/api/passkeys/register/begin", userHdr, `{}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"csrf"`) {
		t.Fatalf("no csrf = %d %s", w.Code, w.Body)
	}
	// No PRF: refused with the message about the iOS setting.
	a := passkeytest.New(testRPID)
	w := register(t, s, h, a, "iPhone", "", false)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "turn on Passwords under Settings, General, AutoFill \\u0026 Passwords, AutoFill From") {
		t.Fatalf("no prf = %d %s", w.Code, w.Body)
	}
	if n := len(s.passkeys.List()); n != 0 {
		t.Fatalf("a passkey without PRF was stored (%d)", n)
	}
	// With PRF it is stored, with a fingerprint and the RP ID recorded.
	w = register(t, s, h, a, "  My   iPhone ", "", true)
	if w.Code != 201 {
		t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	var out struct {
		Passkey passkeyView `json:"passkey"`
	}
	decodeJSON(t, w, &out)
	if out.Passkey.Name != "My iPhone" || out.Passkey.ID != a.ID() || len(out.Passkey.Fingerprint) != 24 || !out.Passkey.BackedUp || s.passkeys.RPID() != testRPID {
		t.Fatalf("passkey = %+v", out.Passkey)
	}
	// The same credential again is refused.
	if w := register(t, s, h, a, "again", "", true); w.Code != 403 && w.Code != 409 {
		t.Fatalf("duplicate = %d %s", w.Code, w.Body)
	}
	// Reload: persisted.
	st, err := passkey.Open(passkey.Path(s.cfg.StateDir))
	if err != nil || len(st.List()) != 1 || st.RPID() != testRPID {
		t.Fatalf("reopened = %v %v", st, err)
	}
}

func TestPasskeyWrongHostAndAttestation(t *testing.T) {
	s, _, _ := todoServer(t)
	h := csrfFor(t, s)
	// A passkey made for another RP ID is refused.
	other := passkeytest.New("other.ts.net")
	if w := register(t, s, h, other, "x", "", true); w.Code != 400 {
		t.Fatalf("other rp = %d %s", w.Code, w.Body)
	}
	// An address or localhost is not a name a passkey can use (not test mode).
	for _, host := range []string{"localhost:8080", "100.64.0.1:8080", "hub"} {
		r := httptest.NewRequest("POST", "/api/passkeys/register/begin", strings.NewReader(`{}`))
		r.Host, r.RemoteAddr = host, "100.64.0.1:1"
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("%s: begin = %d %s", host, w.Code, w.Body)
		}
	}
	// A first passkey fixes the RP ID: another host name is refused after.
	a := passkeytest.New(testRPID)
	if w := register(t, s, h, a, "iPhone", "", true); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	r := httptest.NewRequest("POST", "/api/passkeys/register/begin", strings.NewReader(`{}`))
	r.Host, r.RemoteAddr = "elsewhere.ts.net", "100.64.0.1:1"
	for k, v := range h {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "wrong_host") {
		t.Fatalf("moved = %d %s", w.Code, w.Body)
	}
	// A bad flag set (no user verification) is refused.
	b := passkeytest.New(testRPID)
	b.Flags = passkey.FlagUP
	s.passkeys.Remove(a.ID())
	if w := register(t, s, h, b, "x", "", true); w.Code != 400 || !strings.Contains(w.Body.String(), "Face ID") {
		t.Fatalf("no uv = %d %s", w.Code, w.Body)
	}
}

func TestPasskeyChallengeIsSingleUse(t *testing.T) {
	s, _, _ := todoServer(t)
	h := csrfFor(t, s)
	w := do(s, "POST", "/api/passkeys/register/begin", h, `{}`)
	var begin struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	decodeJSON(t, w, &begin)
	a := passkeytest.New(testRPID)
	ch, _ := passkey.UnB64(begin.PublicKey.Challenge)
	cd, att := a.Create(ch)
	body := jb(map[string]any{"challenge": begin.PublicKey.Challenge, "client_data_json": passkey.B64(cd), "attestation_object": passkey.B64(att), "prf": map[string]bool{"enabled": true, "result": true}})
	if w := do(s, "POST", "/api/passkeys/register/finish", h, body); w.Code != 201 {
		t.Fatalf("first = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/passkeys/register/finish", h, body); w.Code != 409 {
		t.Fatalf("replay = %d %s", w.Code, w.Body)
	}
}

func TestSecondPasskeyNeedsAnExistingOnesApproval(t *testing.T) {
	s, h, a := passkeyServer(t)
	b := passkeytest.New(testRPID)
	// No approval: refused at begin.
	if w := register(t, s, h, b, "iPad", "", true); w.Code != 403 || !strings.Contains(w.Body.String(), "approval_needed") {
		t.Fatalf("no approval = %d %s", w.Code, w.Body)
	}
	// The user's page asks for the approval, answers it with the first passkey.
	w := do(s, "POST", "/api/approvals", userHdr, `{"action":"passkey-add"}`)
	var ap struct {
		Approval approvalView `json:"approval"`
		Todo     *Todo        `json:"todo"`
	}
	decodeJSON(t, w, &ap)
	if w.Code != 201 || ap.Todo != nil {
		t.Fatalf("approval = %d %s", w.Code, w.Body)
	}
	// An unapproved id does not open the gate.
	if w := register(t, s, h, b, "iPad", ap.Approval.ID, true); w.Code != 403 {
		t.Fatalf("pending approval = %d %s", w.Code, w.Body)
	}
	if code, body := assertFor(t, s, h, ap.Approval.ID, a); code != 200 {
		t.Fatalf("assert = %d %s", code, body)
	}
	if w := register(t, s, h, b, "iPad", ap.Approval.ID, true); w.Code != 201 {
		t.Fatalf("with approval = %d %s", w.Code, w.Body)
	}
	// One approval, one passkey.
	c := passkeytest.New(testRPID)
	if w := register(t, s, h, c, "Mac", ap.Approval.ID, true); w.Code != 403 {
		t.Fatalf("spent approval = %d %s", w.Code, w.Body)
	}
	// An approval for another action does not count.
	w = do(s, "POST", "/api/approvals", agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"x"}`)
	var gp struct {
		Approval approvalView `json:"approval"`
	}
	decodeJSON(t, w, &gp)
	assertFor(t, s, h, gp.Approval.ID, a)
	if w := register(t, s, h, c, "Mac", gp.Approval.ID, true); w.Code != 403 {
		t.Fatalf("other action = %d %s", w.Code, w.Body)
	}
	// Rename and remove.
	if w := do(s, "PUT", "/api/passkeys/"+b.ID(), h, `{"name":"  iPad Pro "}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"iPad Pro"`) {
		t.Fatalf("rename = %d %s", w.Code, w.Body)
	}
	if w := do(s, "PUT", "/api/passkeys/"+b.ID(), h, `{"name":"  "}`); w.Code != 400 {
		t.Fatalf("empty rename = %d", w.Code)
	}
	if w := do(s, "DELETE", "/api/passkeys/"+b.ID(), userHdr, ""); w.Code != 403 {
		t.Fatalf("remove without csrf = %d", w.Code)
	}
	if w := do(s, "DELETE", "/api/passkeys/"+b.ID(), h, ""); w.Code != 200 {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	if w := do(s, "DELETE", "/api/passkeys/"+b.ID(), h, ""); w.Code != 404 {
		t.Fatalf("remove again = %d", w.Code)
	}
}

func TestPasskeyRoutesAndRoles(t *testing.T) {
	s, h, a := passkeyServer(t)
	// Agents read the public keys (no CSRF token for them) but change nothing.
	w := do(s, "GET", "/api/passkeys", agentHdr("w2:p1"), "")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"csrf"`) || !strings.Contains(w.Body.String(), `"public_key"`) || !strings.Contains(w.Body.String(), a.ID()) {
		t.Fatalf("agent list = %d %s", w.Code, w.Body)
	}
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/passkeys/register/begin", `{}`}, {"POST", "/api/passkeys/register/finish", `{}`},
		{"PUT", "/api/passkeys/" + a.ID(), `{"name":"x"}`}, {"DELETE", "/api/passkeys/" + a.ID(), ""},
		{"GET", "/api/approvals/apnone", ""}, {"POST", "/api/approvals/apnone/respond", `{"deny":true}`},
	} {
		if w := do(s, c.m, c.p, agentHdr("w2:p1"), c.b); w.Code != 403 || !strings.Contains(w.Body.String(), "agent_forbidden") {
			t.Errorf("agent %s %s = %d %s", c.m, c.p, w.Code, w.Body)
		}
		// A forged login header with the agent's CSRF-less call is not enough either.
		if c.m != "GET" {
			if w := do(s, c.m, c.p, userHdr, c.b); w.Code != 403 {
				t.Errorf("no csrf %s %s = %d %s", c.m, c.p, w.Code, w.Body)
			}
		}
	}
	// A stale token is refused.
	if w := do(s, "DELETE", "/api/passkeys/"+a.ID(), map[string]string{"Tailscale-User-Login": "me@example.com", HeaderCSRF: "stale"}, ""); w.Code != 403 {
		t.Fatalf("stale = %d", w.Code)
	}
	if len(s.passkeys.List()) != 1 {
		t.Fatal("a refused call changed the list")
	}
	_ = h
}

// ---- approvals ----

func fileApproval(t *testing.T, s *Server, hdr map[string]string, body string) (int, struct {
	Approval approvalView `json:"approval"`
	Todo     *Todo        `json:"todo"`
	Dup      bool         `json:"duplicate"`
}) {
	t.Helper()
	w := do(s, "POST", "/api/approvals", hdr, body)
	var out struct {
		Approval approvalView `json:"approval"`
		Todo     *Todo        `json:"todo"`
		Dup      bool         `json:"duplicate"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestApprovalFiledByAnAgentAndApproved(t *testing.T) {
	s, h, a := passkeyServer(t)
	code, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"signing key 62B4"}`)
	if code != 201 || out.Todo == nil || out.Todo.Kind != "approval" || out.Todo.Approval == nil {
		t.Fatalf("filed = %d %+v", code, out)
	}
	it := out.Todo
	if it.Question != "GPG is locked on desk" || it.Approval.State != apPending || it.Approval.Host != "desk" || it.Approval.Action != "gpg-unlock" ||
		it.Approval.ID != out.Approval.ID || it.Source != SourceAgent || it.Waiting != nil {
		t.Fatalf("item = %+v / %+v", it, it.Approval)
	}
	if !out.Approval.ExpiresAt.After(time.Now()) || !strings.Contains(string(out.Approval.Request), `"rp":"`+testRPID+`"`) || !strings.Contains(string(out.Approval.Request), `"nonce"`) {
		t.Fatalf("approval = %+v %s", out.Approval, out.Approval.Request)
	}
	// The same request again is the same approval.
	if code, again := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"signing key 62B4"}`); code != 200 || !again.Dup || again.Approval.ID != out.Approval.ID {
		t.Fatalf("dup = %d %+v", code, again)
	}
	// The user reads it (with the challenge) and answers with Face ID.
	g := do(s, "GET", "/api/approvals/"+out.Approval.ID, userHdr, "")
	if g.Code != 200 || !strings.Contains(g.Body.String(), `"challenge"`) || !strings.Contains(g.Body.String(), `"credentials"`) || strings.Contains(g.Body.String(), "sealed") {
		t.Fatalf("get = %d %s", g.Code, g.Body)
	}
	if c, body := assertFor(t, s, h, out.Approval.ID, a); c != 200 || !strings.Contains(body, `"approved"`) {
		t.Fatalf("respond = %d %s", c, body)
	}
	got, _ := s.todos.get(it.ID)
	if got.State != TodoDone || got.Approval.State != apApproved || got.ClosedBy != "passkey" {
		t.Fatalf("item after = %+v", got)
	}
	// Single use: a second answer to the same request fails, even with a valid assertion.
	if c, body := assertFor(t, s, h, out.Approval.ID, a); c != 409 || !strings.Contains(body, "not_pending") {
		t.Fatalf("reuse = %d %s", c, body)
	}
	if pk, _ := s.passkeys.Get(a.ID()); pk.LastUsedAt == nil {
		t.Fatal("the passkey's use was not recorded")
	}
	// The stored approval carries the assertion and the sealed key for the host.
	s.approvals.mu.Lock()
	ap := s.approvals.find(out.Approval.ID)
	okAssert := ap.Assertion != nil && ap.Sealed == "c2VhbGVk" && ap.Credential == a.ID()
	s.approvals.mu.Unlock()
	if !okAssert {
		t.Fatal("the assertion and sealed key were not kept")
	}
}

func TestApprovalWhoMayFile(t *testing.T) {
	s, _, _ := passkeyServer(t)
	cases := []struct {
		name string
		hdr  map[string]string
		body string
		want int
	}{
		{"agent, own host by default", agentHdr("w2:p1"), `{"action":"gpg-unlock"}`, 201},
		{"agent, own host named", agentHdr("w2:p1"), `{"action":"gpg-unlock","host":"desk","subject":"b"}`, 201},
		{"agent, another host", agentHdr("w2:p1"), `{"action":"gpg-unlock","host":"worker-3","subject":"c"}`, 403},
		{"agent, the user's action", agentHdr("w2:p1"), `{"action":"passkey-add"}`, 403},
		{"agent, unknown action", agentHdr("w2:p1"), `{"action":"rm-rf"}`, 400},
		{"user, a host's action", userHdr, `{"action":"gpg-unlock","host":"desk"}`, 403},
		{"user, passkey-add", userHdr, `{"action":"passkey-add"}`, 201},
		{"no login", nil, `{"action":"passkey-add"}`, 401},
		{"bad request", agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"d","request":[1]}`, 400},
		{"request too large", agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"e","request":{"x":"` + strings.Repeat("a", 5000) + `"}}`, 400},
	}
	for _, c := range cases {
		if w := do(s, "POST", "/api/approvals", c.hdr, c.body); w.Code != c.want {
			t.Errorf("%s = %d %s", c.name, w.Code, w.Body)
		}
	}
}

func TestApprovalTaggedWorkerOwnHostOnly(t *testing.T) {
	s, _, _ := passkeyServer(t)
	post := func(host, body string) int {
		r := httptest.NewRequest("POST", "/api/approvals", strings.NewReader(body))
		r.RemoteAddr = "100.64.0.9:1"
		r = r.WithContext(context.WithValue(r.Context(), taggedKey{}, tagged{peer: host, node: host, tag: "tag:worker"}))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w.Code
	}
	if c := post("worker-3", `{"action":"gpg-unlock","subject":"a"}`); c != 201 {
		t.Errorf("own host = %d", c)
	}
	if c := post("worker-3", `{"action":"gpg-unlock","host":"worker-3","subject":"b"}`); c != 201 {
		t.Errorf("own host named = %d", c)
	}
	if c := post("worker-3", `{"action":"gpg-unlock","host":"desk","subject":"c"}`); c != 403 {
		t.Errorf("another host = %d", c)
	}
	if c := post("worker-3", `{"action":"passkey-add"}`); c != 403 {
		t.Errorf("passkey-add = %d", c)
	}
	r := httptest.NewRequest("GET", "/api/passkeys", nil)
	r.RemoteAddr = "100.64.0.9:1"
	r = r.WithContext(context.WithValue(r.Context(), taggedKey{}, tagged{peer: "worker-3", node: "worker-3", tag: "tag:worker"}))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "csrf") {
		t.Errorf("tagged list = %d %s", w.Code, w.Body)
	}
}

func TestApprovalNeedsAPasskeyAndIsRateLimited(t *testing.T) {
	s, _, _ := todoServer(t)
	if code, _ := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock"}`); code != 409 {
		t.Fatalf("no passkeys = %d", code)
	}
	s2, _, _ := passkeyServer(t)
	for i := 0; i < approvalPerMinute; i++ {
		if code, _ := fileApproval(t, s2, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"s`+string(rune('a'+i))+`"}`); code != 201 {
			t.Fatalf("approval %d = %d", i, code)
		}
	}
	if code, _ := fileApproval(t, s2, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"one more"}`); code != 429 {
		t.Fatalf("over the cap = %d", code)
	}
	// One item for the host, showing the latest request.
	n := 0
	for _, it := range s2.todos.list("") {
		if it.Kind == "approval" {
			n++
			if it.Approval.State != apPending {
				t.Errorf("item = %+v", it.Approval)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d approval items for one host, want 1", n)
	}
}

func TestApprovalRefusals(t *testing.T) {
	defer func(n int) { approvalPerMinute = n }(approvalPerMinute)
	approvalPerMinute = 50
	s, h, a := passkeyServer(t)
	other := passkeytest.New(testRPID)
	mk := func(subject string) (string, string) {
		code, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"`+subject+`"}`)
		if code != 201 || out.Todo == nil {
			t.Fatalf("file %q = %d", subject, code)
		}
		return out.Approval.ID, out.Todo.ID
	}
	type tc struct {
		name string
		mut  func(au *passkeytest.Authenticator)
		who  *passkeytest.Authenticator
		code string
	}
	for _, c := range []tc{
		{"unknown passkey", nil, other, "unknown_passkey"},
		{"no user verification", func(au *passkeytest.Authenticator) { au.Flags = passkey.FlagUP }, a, "assertion_failed"},
		{"wrong origin", func(au *passkeytest.Authenticator) { au.Origin = "https://evil.example" }, a, "assertion_failed"},
	} {
		id, tid := mk(c.name)
		au := c.who
		if c.mut != nil {
			cp := *au
			c.mut(&cp)
			au = &cp
		}
		code, body := assertFor(t, s, h, id, au)
		if code != 400 || !strings.Contains(body, c.code) || !strings.Contains(body, `"tries_left":2`) {
			t.Errorf("%s = %d %s", c.name, code, body)
		}
		if it, _ := s.todos.get(tid); it.State != TodoOpen {
			t.Errorf("%s: a refused try closed the item", c.name)
		}
	}
	// An assertion made for another request's challenge is not accepted.
	id1, _ := mk("first")
	id2, _ := mk("second")
	g := do(s, "GET", "/api/approvals/"+id1, userHdr, "")
	var d struct {
		Challenge string `json:"challenge"`
	}
	decodeJSON(t, g, &d)
	ch, _ := passkey.UnB64(d.Challenge)
	cd, ad, sig := a.Get(ch)
	w := do(s, "POST", "/api/approvals/"+id2+"/respond", h, jb(map[string]string{"credential_id": a.ID(), "client_data_json": passkey.B64(cd), "authenticator_data": passkey.B64(ad), "signature": passkey.B64(sig)}))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "challenge") {
		t.Errorf("cross-use = %d %s", w.Code, w.Body)
	}
	// Three failed tries kill a request: a valid assertion then fails too.
	id3, tid3 := mk("three")
	for i := 0; i < approvalMaxTries; i++ {
		assertFor(t, s, h, id3, other)
	}
	if code, body := assertFor(t, s, h, id3, a); code != 409 {
		t.Errorf("after three tries = %d %s", code, body)
	}
	if it, _ := s.todos.get(tid3); it.State != TodoDropped || it.Approval.State != apDenied {
		t.Errorf("killed item = %+v", it)
	}
	// An action whose request expired is refused, and its item closes.
	id4, tid4 := mk("late")
	now := time.Now().Add(approvalTTL + time.Second)
	s.approvals.mu.Lock()
	s.approvals.now = func() time.Time { return now }
	s.approvals.mu.Unlock()
	if code, body := assertFor(t, s, h, id4, a); code != 409 || !strings.Contains(body, "expired") {
		t.Errorf("expired = %d %s", code, body)
	}
	if it, _ := s.todos.get(tid4); it.State != TodoDropped || it.DropReason != "expired" {
		t.Errorf("expired item = %+v", it)
	}
}

func TestApprovalCounter(t *testing.T) {
	s, h, a := passkeyServer(t)
	file := func(subject string) string {
		_, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"`+subject+`"}`)
		return out.Approval.ID
	}
	a.Counter = 0 // a synced passkey: always 0, always accepted
	for i := 0; i < 2; i++ {
		if c, b := assertFor(t, s, h, file("zero"+string(rune('a'+i))), a); c != 200 {
			t.Fatalf("counter 0 = %d %s", c, b)
		}
	}
	a.Counter = 9
	if c, b := assertFor(t, s, h, file("nine"), a); c != 200 {
		t.Fatalf("counter 9 = %d %s", c, b)
	}
	a.Counter = 4 // going back
	if c, b := assertFor(t, s, h, file("four"), a); c != 400 || !strings.Contains(b, "counter") {
		t.Fatalf("counter back = %d %s", c, b)
	}
}

func TestApprovalDenyAndNotNow(t *testing.T) {
	s, h, _ := passkeyServer(t)
	_, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"one"}`)
	if w := do(s, "POST", "/api/approvals/"+out.Approval.ID+"/respond", userHdr, `{"deny":true}`); w.Code != 403 {
		t.Fatalf("deny without csrf = %d", w.Code)
	}
	if w := do(s, "POST", "/api/approvals/"+out.Approval.ID+"/respond", h, `{"deny":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"denied"`) {
		t.Fatalf("deny = %d %s", w.Code, w.Body)
	}
	if it, _ := s.todos.get(out.Todo.ID); it.State != TodoDropped || it.Approval.State != apDenied {
		t.Fatalf("item = %+v", it)
	}
	// "Not now" (dropping the item) denies the request too.
	_, out = fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"two"}`)
	if w := do(s, "POST", "/api/todos/"+out.Todo.ID+"/drop", userHdr, ``); w.Code != 200 {
		t.Fatalf("drop = %d %s", w.Code, w.Body)
	}
	s.approvals.mu.Lock()
	state := s.approvals.find(out.Approval.ID).State
	s.approvals.mu.Unlock()
	if state != apDenied {
		t.Fatalf("after Not now = %s", state)
	}
}

func TestApprovalItemsAreNotMadeOrAnsweredAsText(t *testing.T) {
	s, _, _ := passkeyServer(t)
	for _, hdr := range []map[string]string{userHdr, agentHdr("w2:p1")} {
		if w := do(s, "POST", "/api/todos", hdr, `{"question":"GPG is locked on desk","kind":"approval"}`); w.Code != 400 {
			t.Errorf("add kind approval = %d %s", w.Code, w.Body)
		}
	}
	_, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock"}`)
	if w := do(s, "POST", "/api/todos/"+out.Todo.ID+"/answer", userHdr, `{"text":"yes"}`); w.Code != 409 {
		t.Errorf("answer = %d %s", w.Code, w.Body)
	}
	// A request with its own bytes (a host's signed request) is hashed as given.
	_, out2 := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"signed","request":{ "v":1, "host":"desk", "nonce":"abc" }}`)
	if string(out2.Approval.Request) != `{"v":1,"host":"desk","nonce":"abc"}` {
		t.Errorf("request = %s", out2.Approval.Request)
	}
	g := do(s, "GET", "/api/approvals/"+out2.Approval.ID, userHdr, "")
	var d struct {
		Challenge string `json:"challenge"`
	}
	decodeJSON(t, g, &d)
	if want := passkey.B64(passkey.ApprovalChallenge([]byte(`{"v":1,"host":"desk","nonce":"abc"}`))); d.Challenge != want {
		t.Errorf("challenge = %s want %s", d.Challenge, want)
	}
}

func TestApprovalsSurviveARestart(t *testing.T) {
	s, _, _ := passkeyServer(t)
	_, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock"}`)
	st, err := openApprovals(approvalsPath(s.cfg.StateDir))
	if err != nil || st.find(out.Approval.ID) == nil || st.find(out.Approval.ID).Challenge == "" {
		t.Fatalf("reopened = %v %v", st, err)
	}
}

func TestPasskeysOffOnAPeer(t *testing.T) {
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk"}, nil, nil)
	if w := do(s, "GET", "/api/passkeys", userHdr, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
}
