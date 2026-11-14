package gpgunlock

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/passkey"
)

type nodeOut struct {
	Sealed          string `json:"sealed"`
	Salt            string `json:"salt"`
	Fingerprint     string `json:"fingerprint"`
	Signed          bool   `json:"signed"`
	Setup           bool   `json:"setup"`
	Challenge       string `json:"challenge"`
	Blob            *Blob  `json:"blob"`
	RefusedTampered string `json:"refusedTampered"`
}

// browser runs web/unlockcrypto.js (the app's own file) in Node on a request.
func browser(t *testing.T, req []byte, challenge, host string, prf []byte, pass *string) nodeOut {
	t.Helper()
	in := map[string]any{"request": string(req), "challenge": challenge, "rp": testRP, "host": host, "prf": passkey.B64(prf)}
	if pass != nil {
		in["pass"] = *pass
	}
	b, _ := json.Marshal(in)
	cmd := exec.Command("node", "../../web/test/unlockinterop.mjs")
	cmd.Stdin = bytes.NewReader(b)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v\n%s", err, errb.String())
	}
	var o nodeOut
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("node output %q: %v", out.String(), err)
	}
	return o
}

// The app's JavaScript and the host agree on every format: the request
// check and its signature, the PRF salt, HKDF, the seal and the blob. Real
// gpg in a temp GNUPGHOME, a software passkey for the assertion.
func TestBrowserCryptoInterop(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	r := newRig(t, 8*60*60*1e9)
	r.setup(t)
	cfg, _ := r.h.Config()
	prf := make([]byte, 32)
	_, _ = rand.Read(prf)
	wantK, err := DeriveK(prf)
	if err != nil {
		t.Fatal(err)
	}
	hfp, _ := r.h.HostFingerprint()

	// Setup: the browser seals K and encrypts the passphrase.
	id, req, err := r.h.NewRequest(testRP, true)
	if err != nil {
		t.Fatal(err)
	}
	ch := passkey.B64(passkey.ApprovalChallenge(req))
	pass := r.pass
	o := browser(t, req, ch, cfg.Host, prf, &pass)
	if o.Fingerprint != hfp || !o.Setup || o.Challenge != ch {
		t.Fatalf("browser read the request differently: %+v (host fingerprint %s)", o, hfp)
	}
	if !o.Signed {
		t.Log("node has no Ed25519: signature not checked in the browser")
	}
	if o.RefusedTampered == "" {
		t.Fatal("a tampered request was not refused")
	}
	if salt, _ := passkey.UnB64(o.Salt); !bytes.Equal(salt, PRFSalt(cfg.Host, cfg.Keygrips)) {
		t.Fatal("PRF salt differs")
	}
	chb := passkey.ApprovalChallenge(req)
	cd, ad, sig := r.auth.Get(chb)
	a := Answer{CredentialID: r.auth.ID(), ClientDataJSON: passkey.B64(cd), AuthenticatorData: passkey.B64(ad), Signature: passkey.B64(sig), Sealed: o.Sealed, Blob: o.Blob}
	if _, err := r.h.Complete(id, a); err != nil {
		t.Fatalf("setup with the browser's output: %v", err)
	}
	if r.locked(t) {
		t.Fatal("still locked after setup")
	}
	stored := new(Blob)
	if err := r.h.readJSON(fileBlob, stored); err != nil {
		t.Fatal(err)
	}
	if p, err := stored.Decrypt(wantK); err != nil || string(p) != r.pass {
		t.Fatalf("the stored blob does not open with Go's K: %v", err)
	}

	// Unlock later: same PRF, no blob, a new request.
	if err := r.h.Lock(); err != nil {
		t.Fatal(err)
	}
	id, req, err = r.h.NewRequest(testRP, false)
	if err != nil {
		t.Fatal(err)
	}
	ch = passkey.B64(passkey.ApprovalChallenge(req))
	o = browser(t, req, ch, cfg.Host, prf, nil)
	if o.Setup || o.Blob != nil {
		t.Fatalf("unlock request read as setup: %+v", o)
	}
	cd, ad, sig = r.auth.Get(passkey.ApprovalChallenge(req))
	a = Answer{CredentialID: r.auth.ID(), ClientDataJSON: passkey.B64(cd), AuthenticatorData: passkey.B64(ad), Signature: passkey.B64(sig), Sealed: o.Sealed}
	if _, err := r.h.Complete(id, a); err != nil {
		t.Fatalf("unlock with the browser's output: %v", err)
	}
	if r.locked(t) {
		t.Fatal("still locked after unlock")
	}
	// A browser holding another passkey's PRF cannot unlock.
	_ = r.h.Lock()
	id, req, _ = r.h.NewRequest(testRP, false)
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	o = browser(t, req, passkey.B64(passkey.ApprovalChallenge(req)), cfg.Host, other, nil)
	cd, ad, sig = r.auth.Get(passkey.ApprovalChallenge(req))
	a = Answer{CredentialID: r.auth.ID(), ClientDataJSON: passkey.B64(cd), AuthenticatorData: passkey.B64(ad), Signature: passkey.B64(sig), Sealed: o.Sealed}
	if _, err := r.h.Complete(id, a); err == nil || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("another PRF unlocked: %v", err)
	}
}
