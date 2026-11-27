package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/gpgunlock"
	"github.com/jhjaggars/comebye/internal/passkey"
	"github.com/jhjaggars/comebye/internal/passkey/passkeytest"
)

// The whole `comebye gpg` flow against a fake hub and real gpg in a temp
// GNUPGHOME (a throwaway key; the user's own is never touched).
func TestGPGCommands(t *testing.T) {
	for _, b := range []string{"gpg", "gpg-connect-agent", "gpgconf"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " is not installed")
		}
	}
	home, err := os.MkdirTemp("", "gu")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--kill", "gpg-agent").Run()
		os.RemoveAll(home)
	})
	const pass = "correct horse"
	gen := exec.Command("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", pass, "--quick-gen-key", "T <t@example.invalid>", "ed25519", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	colon, _ := exec.Command("gpg", "--batch", "--list-secret-keys", "--with-colons").Output()
	fpr := ""
	for _, ln := range strings.Split(string(colon), "\n") {
		if f := strings.Split(ln, ":"); f[0] == "fpr" {
			fpr = f[9]
			break
		}
	}

	const rp = "hub.example.ts.net"
	auth := passkeytest.New(rp)
	ccd, att := auth.Create([]byte("c"))
	reg, err := passkey.VerifyRegistration(passkey.Options{RPID: rp}, []byte("c"), ccd, att)
	if err != nil {
		t.Fatal(err)
	}
	f, e, out, errb := newFake(t)
	f.answers["GET /api/passkeys"] = answer{body: `{"rp_id":"` + rp + `","passkeys":[{"id":"` + reg.ID + `","name":"iPhone","public_key":"` + reg.PublicKey + `","fingerprint":"LIES"}]}`}
	f.answers["POST /api/approvals"] = answer{body: `{"approval":{"id":"ap_1"}}`}

	if code := run(e, "gpg", "status"); code != 0 || !strings.Contains(out.String(), "not set up") {
		t.Fatalf("status before: %d %q %q", code, out, errb)
	}
	out.Reset()
	// A refusal at the prompt changes nothing.
	e.Stdin = strings.NewReader("n\n")
	if code := run(e, "gpg", "setup", "--key", fpr, "--host", "w1"); code == 0 {
		t.Fatalf("setup without confirmation succeeded: %q", out)
	}
	if _, err := gpgHost(e).Config(); err != gpgunlock.ErrNotSetUp {
		t.Fatalf("declined setup left config: %v", err)
	}
	out.Reset()
	e.Stdin = strings.NewReader("y\n")
	if code := run(e, "gpg", "setup", "--key", fpr, "--host", "w1"); code != 0 {
		t.Fatalf("setup: %q %q", out, errb)
	}
	if strings.Contains(out.String(), "LIES") || !strings.Contains(out.String(), passkey.Fingerprint(reg.PublicKey)) || !strings.Contains(out.String(), "Host key fingerprint") {
		t.Fatalf("setup output: %q", out)
	}
	// Setup files the "Set up GPG unlock" item: a setup request, with the longer wait.
	if !strings.Contains(out.String(), `Filed "Set up GPG unlock on w1"`) {
		t.Fatalf("setup did not file its item: %q", out)
	}
	var first struct {
		Action, Host, Subject string
		Request               json.RawMessage
	}
	_ = json.Unmarshal([]byte(f.find("POST", "/api/approvals").body), &first)
	var firstReq struct {
		Setup   bool   `json:"setup"`
		HostKey string `json:"host_key"`
	}
	_ = json.Unmarshal(first.Request, &firstReq)
	hfp, _ := gpgHost(e).HostFingerprint()
	if first.Action != "gpg-setup" || first.Host != "w1" || !firstReq.Setup || passkey.Fingerprint(firstReq.HostKey) != hfp || !strings.Contains(out.String(), hfp) {
		t.Fatalf("setup filed %+v (request %s), want a gpg-setup request signed by host key %s", first, first.Request, hfp)
	}
	if b, _ := os.ReadFile(home + "/gpg-agent.conf"); !strings.Contains(string(b), "allow-preset-passphrase") || !strings.Contains(string(b), "max-cache-ttl 28800") {
		t.Fatalf("agent conf: %q", b)
	}
	out.Reset()
	if code := run(e, "gpg", "status"); code != 0 || !strings.Contains(out.String(), "locked") || !strings.Contains(out.String(), "not stored yet") {
		t.Fatalf("status: %q", out)
	}

	out.Reset()
	if code := run(e, "gpg", "unlock", "--json"); code != 0 {
		t.Fatalf("unlock: %q %q", out, errb)
	}
	var u struct{ Request, Approval string }
	_ = json.Unmarshal(out.Bytes(), &u)
	var posted struct {
		Action  string          `json:"action"`
		Request json.RawMessage `json:"request"`
	}
	_ = json.Unmarshal([]byte(f.find("POST", "/api/approvals").body), &posted)
	if posted.Action != "gpg-setup" || u.Approval != "ap_1" || !strings.Contains(string(posted.Request), u.Request) {
		t.Fatalf("filed %+v, printed %+v", posted, u)
	}

	// The browser's part: assertion over the request, K sealed, blob.
	var ro struct {
		Pub      string   `json:"pub"`
		Keygrips []string `json:"keygrips"`
	}
	_ = json.Unmarshal(posted.Request, &ro)
	ch := passkey.ApprovalChallenge(posted.Request)
	cd, ad, sig := auth.Get(ch)
	prf := make([]byte, 32)
	_, _ = rand.Read(prf)
	k, _ := gpgunlock.DeriveK(prf)
	pub, _ := passkey.UnB64(ro.Pub)
	sealed, _ := gpgunlock.Seal(gpgunlock.CurveP256, pub, ch, k)
	blob, _ := gpgunlock.EncryptBlob(k, "w1", ro.Keygrips, []byte(pass))
	ans, _ := json.Marshal(gpgunlock.Answer{CredentialID: auth.ID(), ClientDataJSON: passkey.B64(cd), AuthenticatorData: passkey.B64(ad), Signature: passkey.B64(sig), Sealed: sealed, Blob: &blob})

	out.Reset()
	e.Stdin = bytes.NewReader(ans)
	if code := run(e, "gpg", "complete", u.Request); code != 0 || !strings.Contains(out.String(), "Unlocked until") {
		t.Fatalf("complete: %d %q %q", code, out, errb)
	}
	out.Reset()
	if code := run(e, "gpg", "status", "--json"); code != 0 {
		t.Fatal(out.String())
	}
	var st gpgunlock.Status
	_ = json.Unmarshal(out.Bytes(), &st)
	if st.Locked || !st.HasBlob || st.Passkeys != 1 || st.UnlockedTo == nil {
		t.Fatalf("status after: %+v", st)
	}
	// Already unlocked: unlock does nothing.
	out.Reset()
	if code := run(e, "gpg", "unlock"); code != 0 || !strings.Contains(out.String(), "already unlocked") {
		t.Fatalf("unlock again: %q", out)
	}
	// Replaying the answer fails: the request was used.
	e.Stdin = bytes.NewReader(ans)
	if code := run(e, "gpg", "complete", u.Request); code == 0 {
		t.Fatal("a used request completed again")
	}

	out.Reset()
	if code := run(e, "gpg", "lock"); code != 0 {
		t.Fatalf("lock: %q", errb)
	}
	// With a passphrase stored, an unlock request is an unlock, not a setup.
	out.Reset()
	if code := run(e, "gpg", "unlock", "--json"); code != 0 {
		t.Fatalf("unlock after setup: %q %q", out, errb)
	}
	_ = json.Unmarshal([]byte(f.find("POST", "/api/approvals").body), &posted)
	var again struct {
		Setup bool `json:"setup"`
	}
	_ = json.Unmarshal(posted.Request, &again)
	if posted.Action != "gpg-unlock" || again.Setup {
		t.Fatalf("filed %+v after setup, want gpg-unlock", posted)
	}
	if code := run(e, "gpg", "status", "--json"); code != 0 {
		t.Fatal()
	}
	posts := func(path string) int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, r := range f.reqs {
			if r.method == "POST" && r.path == path {
				n++
			}
		}
		return n
	}
	// One item per host: a second unlock while the request waits files nothing
	// new, --force does.
	before := posts("/api/approvals")
	out.Reset()
	if code := run(e, "gpg", "unlock"); code != 0 || !strings.Contains(out.String(), "Already asked") || posts("/api/approvals") != before {
		t.Fatalf("second unlock: %d %q", code, out)
	}
	out.Reset()
	if code := run(e, "gpg", "unlock", "--force"); code != 0 || posts("/api/approvals") != before+1 {
		t.Fatalf("forced unlock: %d %q", code, out)
	}
	// The old daemon is retired: `gpg watch` (with its old flags, as a unit
	// still runs it) says so, exits 0, and asks the hub nothing. The
	// watcher's rounds are the server's now (server/gpgwatch_test.go).
	nreqs := func() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }
	beforeGet := nreqs()
	out.Reset()
	if code := run(e, "gpg", "watch", "--once", "--every", "30s"); code != 0 || !strings.Contains(out.String(), "no longer needed") || !strings.Contains(out.String(), "disable --now comebye-gpg-watch.service") {
		t.Fatalf("gpg watch: %d %q", code, out)
	}
	if posts("/api/approvals") != before+1 || nreqs() != beforeGet {
		t.Fatal("the retired gpg watch called the hub")
	}
	if code := run(e, "gpg", "reset", "--yes"); code != 0 {
		t.Fatalf("reset: %q", errb)
	}
	out.Reset()
	if code := run(e, "gpg", "status"); !strings.Contains(out.String(), "not set up") {
		t.Fatalf("after reset: %d %q", code, out)
	}
	for _, args := range [][]string{{"gpg"}, {"gpg", "frob"}, {"gpg", "setup"}, {"gpg", "complete"}} {
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
}
