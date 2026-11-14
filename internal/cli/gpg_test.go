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
	if posted.Action != "gpg-unlock" || u.Approval != "ap_1" || !strings.Contains(string(posted.Request), u.Request) {
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
	if code := run(e, "gpg", "status", "--json"); code != 0 {
		t.Fatal()
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
