package gpgunlock

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/passkey"
	"github.com/jhjaggars/comebye/internal/passkey/passkeytest"
)

const testRP = "hub.example.ts.net"

func TestParseKeyInfo(t *testing.T) {
	out := "S KEYINFO 62B4AA - - - 1 P - - -\nS KEYINFO AB12 D - - - - - - -\nOK\nS KEYINFO bad\n"
	got := ParseKeyInfo(out)
	if len(got) != 2 || got[0] != (KeyInfo{"62B4AA", true}) || got[1] != (KeyInfo{"AB12", false}) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseKeygrips(t *testing.T) {
	colons := "sec:u:255:22:AAAA:1:::u:::scESC:::+:::23::0:\nfpr:::::::::AAAA:\ngrp:::::::::GRIPPRIMARY:\n" +
		"ssb:u:255:18:BBBB:1::::::e:::+:::cv25519::\nfpr:::::::::BBBB:\ngrp:::::::::GRIPENC:\n" +
		"ssb:u:255:22:CCCC:1::::::s:::+:::ed25519::\nfpr:::::::::CCCC:\ngrp:::::::::gripsign:\n"
	got := ParseKeygrips(colons)
	if strings.Join(got, ",") != "GRIPPRIMARY,GRIPSIGN" {
		t.Fatalf("got %v", got)
	}
}

func TestSetAgentConf(t *testing.T) {
	got := SetAgentConf("pinentry-program /usr/bin/pinentry-curses\ndefault-cache-ttl 28800\nmax-cache-ttl 100\n", 8*time.Hour)
	want := "pinentry-program /usr/bin/pinentry-curses\ndefault-cache-ttl 28800\nmax-cache-ttl 28800\nallow-preset-passphrase\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if again := SetAgentConf(got, 8*time.Hour); again != got {
		t.Fatalf("not idempotent: %q", again)
	}
	if e := SetAgentConf("", time.Hour); e != "allow-preset-passphrase\nmax-cache-ttl 3600\n" {
		t.Fatalf("empty: %q", e)
	}
}

func TestSealOpen(t *testing.T) {
	for _, curve := range []string{CurveP256, CurveX25519} {
		c, _ := curveOf(curve)
		priv, _ := c.GenerateKey(rand.Reader)
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		ch := []byte("challenge-1")
		s, err := Seal(curve, priv.PublicKey().Bytes(), ch, k)
		if err != nil {
			t.Fatal(err)
		}
		got, err := openSealed(curve, priv.Bytes(), ch, s)
		if err != nil || string(got) != string(k) {
			t.Fatalf("%s: open: %v", curve, err)
		}
		if _, err := openSealed(curve, priv.Bytes(), []byte("challenge-2"), s); err == nil {
			t.Fatalf("%s: opened for another request", curve)
		}
		other, _ := c.GenerateKey(rand.Reader)
		if _, err := openSealed(curve, other.Bytes(), ch, s); err == nil {
			t.Fatalf("%s: opened with another key", curve)
		}
		if _, err := openSealed(curve, priv.Bytes(), ch, s[:len(s)-4]); err == nil {
			t.Fatalf("%s: opened a damaged blob", curve)
		}
	}
	if _, err := Seal("P-384", nil, nil, nil); err == nil {
		t.Fatal("unknown curve accepted")
	}
}

func TestBlob(t *testing.T) {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	b, err := EncryptBlob(k, "w1", []string{"A", "B"}, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.CT, "hunter2") {
		t.Fatal("plaintext in blob")
	}
	p, err := b.Decrypt(k)
	if err != nil || string(p) != "hunter2" {
		t.Fatalf("decrypt: %q %v", p, err)
	}
	k2 := append([]byte(nil), k...)
	k2[0] ^= 1
	if _, err := b.Decrypt(k2); err == nil {
		t.Fatal("another K opened the blob")
	}
	moved := b
	moved.Host = "w2"
	if _, err := moved.Decrypt(k); err == nil {
		t.Fatal("blob opened for another host")
	}
	moved = b
	moved.Keygrips = []string{"A"}
	if _, err := moved.Decrypt(k); err == nil {
		t.Fatal("blob opened for other keys")
	}
	if string(PRFSalt("w1", []string{"A"})) == string(PRFSalt("w2", []string{"A"})) {
		t.Fatal("PRF salt ignores the host")
	}
}

// rig is a throwaway GNUPGHOME with a key, never the user's own.
type rig struct {
	g    *GPG
	fpr  string
	pass string
	h    *Host
	auth *passkeytest.Authenticator
	now  *fakeClock
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newRig(t *testing.T, lifetime time.Duration) *rig {
	t.Helper()
	g := &GPG{}
	if !g.Available() {
		t.Skip("gpg is not installed")
	}
	if _, err := (&GPG{}).presetTool(); err != nil {
		t.Skip(err.Error())
	}
	home, err := os.MkdirTemp("", "gu")
	if err != nil {
		t.Fatal(err)
	}
	g.Home = home
	t.Cleanup(func() {
		_, _ = g.run(nil, "gpgconf", "--kill", "gpg-agent")
		os.RemoveAll(home)
	})
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := g.ConfigureAgent(lifetime); err != nil {
		t.Fatal(err)
	}
	r := &rig{g: g, pass: "correct horse battery"}
	cmd := exec.Command("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", r.pass, "--quick-gen-key", "Test <t@example.invalid>", "ed25519", "sign", "never")
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make the throwaway key: %v\n%s", err, out)
	}
	out, err := g.run(nil, "gpg", "--batch", "--list-secret-keys", "--with-colons")
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(string(out), "\n") {
		if f := strings.Split(ln, ":"); f[0] == "fpr" {
			r.fpr = f[9]
			break
		}
	}
	r.now = &fakeClock{t: time.Now()}
	r.h = &Host{Dir: t.TempDir(), GPG: g, Lifetime: lifetime, Now: r.now.Now}
	r.auth = passkeytest.New(testRP)
	return r
}

// setup pins the test passkey and makes the config, as `gpg setup` does.
func (r *rig) setup(t *testing.T) {
	t.Helper()
	grips, err := r.g.Keygrips(r.fpr)
	if err != nil {
		t.Fatal(err)
	}
	ccd, att := r.auth.Create([]byte("reg-challenge"))
	reg, err := passkey.VerifyRegistration(passkey.Options{RPID: testRP}, []byte("reg-challenge"), ccd, att)
	if err != nil {
		t.Fatal(err)
	}
	err = r.h.Setup(Config{Host: "w1", FPR: r.fpr, Keygrips: grips}, Pinned{RPID: testRP, Passkeys: []PinnedKey{{ID: reg.ID, PublicKey: reg.PublicKey}}})
	if err != nil {
		t.Fatal(err)
	}
}

func (r *rig) locked(t *testing.T) bool {
	t.Helper()
	c, _ := r.h.Config()
	l, err := r.g.Locked(c.Keygrips)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// answer plays the browser: assertion over the request's challenge, K from a
// PRF output, K sealed to the request's key, and the blob when setting up.
func (r *rig) answer(t *testing.T, id string, req []byte, pass string, k []byte, setup bool) Answer {
	t.Helper()
	var o struct {
		Curve string `json:"curve"`
		Pub   string `json:"pub"`
		Host  string `json:"host"`
	}
	_ = json.Unmarshal(req, &o)
	ch := passkey.ApprovalChallenge(req)
	cd, ad, sig := r.auth.Get(ch)
	pub, _ := passkey.UnB64(o.Pub)
	sealed, err := Seal(o.Curve, pub, ch, k)
	if err != nil {
		t.Fatal(err)
	}
	a := Answer{CredentialID: r.auth.ID(), ClientDataJSON: passkey.B64(cd), AuthenticatorData: passkey.B64(ad), Signature: passkey.B64(sig), Sealed: sealed}
	if setup {
		c, _ := r.h.Config()
		b, _ := EncryptBlob(k, o.Host, c.Keygrips, []byte(pass))
		a.Blob = &b
	}
	return a
}

func newK(t *testing.T) []byte {
	prf := make([]byte, 32)
	_, _ = rand.Read(prf)
	k, err := DeriveK(prf)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (r *rig) doSetup(t *testing.T, k []byte, pass string) (State, error) {
	id, req, err := r.h.NewRequest(testRP, true)
	if err != nil {
		t.Fatal(err)
	}
	return r.h.Complete(id, r.answer(t, id, req, pass, k, true))
}

func TestGPGPresetSignExpiryRestart(t *testing.T) {
	const life = 8 * time.Hour
	r := newRig(t, life)
	r.setup(t)
	if !r.locked(t) {
		t.Fatal("fresh key should be locked")
	}
	if err := r.g.TestSign(r.fpr); err == nil {
		t.Fatal("signed while locked")
	}
	k := newK(t)
	st, err := r.doSetup(t, k, r.pass)
	if err != nil {
		t.Fatal(err)
	}
	if r.locked(t) {
		t.Fatal("still locked after setup")
	}
	if err := r.g.TestSign(r.fpr); err != nil {
		t.Fatalf("cannot sign after unlock: %v", err)
	}
	if want := r.now.Now().Add(life); !st.ExpiresAt.Equal(want) {
		t.Fatalf("expires %v want %v", st.ExpiresAt, want)
	}
	if !r.h.HasBlob() {
		t.Fatal("blob not stored")
	}
	raw, _ := os.ReadFile(filepath.Join(r.h.Dir, fileBlob))
	if strings.Contains(string(raw), r.pass) {
		t.Fatal("passphrase in blob file")
	}
	s, err := r.h.Status()
	if err != nil || s.Locked || s.UnlockedTo == nil || len(s.Keys) == 0 {
		t.Fatalf("status %+v %v", s, err)
	}

	// Lock now, then unlock from the stored blob (no passphrase in the answer).
	if err := r.h.Lock(); err != nil {
		t.Fatal(err)
	}
	if !r.locked(t) || r.g.TestSign(r.fpr) == nil {
		t.Fatal("Lock did not lock")
	}
	id, req, _ := r.h.NewRequest(testRP, false)
	if _, err := r.h.Complete(id, r.answer(t, id, req, "", k, false)); err != nil {
		t.Fatalf("unlock from blob: %v", err)
	}
	if err := r.g.TestSign(r.fpr); err != nil {
		t.Fatal(err)
	}
	// A request is good for one try.
	if _, err := r.h.Complete(id, r.answer(t, id, req, "", k, false)); err == nil {
		t.Fatal("a used request worked again")
	}

	// Not yet expired: sweep leaves it. Past the expiry: sweep forgets.
	if ended, _ := r.h.Sweep(); ended || r.locked(t) {
		t.Fatal("swept too early")
	}
	r.now.Add(life + time.Second)
	if ended, err := r.h.Sweep(); !ended || err != nil {
		t.Fatalf("sweep at expiry: %v %v", ended, err)
	}
	if !r.locked(t) || r.g.TestSign(r.fpr) == nil {
		t.Fatal("still unlocked after expiry")
	}

	// Restart after expiry: unlock, then "restart" with the clock past the
	// expiry; a fresh Host's Run forgets at once and does not extend it.
	id, req, _ = r.h.NewRequest(testRP, false)
	if _, err := r.h.Complete(id, r.answer(t, id, req, "", k, false)); err != nil {
		t.Fatal(err)
	}
	r.now.Add(life + time.Minute)
	h2 := &Host{Dir: r.h.Dir, GPG: r.g, Lifetime: life, Now: r.now.Now}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h2.Run(ctx, func(err error) { t.Error(err) }); close(done) }()
	waitFor(t, func() bool { return r.locked(t) })
	cancel()
	<-done
	if _, ok := h2.Unlocked(); ok {
		t.Fatal("state file still says unlocked")
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// A shortened lifetime really ends at the agent: the timer forgets, in real time.
func TestGPGRealTimeExpiry(t *testing.T) {
	r := newRig(t, 2*time.Second)
	r.h.Now = nil
	r.setup(t)
	if _, err := r.doSetup(t, newK(t), r.pass); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.h.Run(ctx, nil)
	if r.locked(t) {
		t.Fatal("locked right after unlock")
	}
	waitFor(t, func() bool { return r.locked(t) })
	if err := r.g.TestSign(r.fpr); err == nil {
		t.Fatal("signed after the lifetime")
	}
}

func TestGPGWrongPassphraseStoresNothing(t *testing.T) {
	r := newRig(t, time.Hour)
	r.setup(t)
	_, err := r.doSetup(t, newK(t), "not the passphrase")
	if err != ErrBadPassphrase {
		t.Fatalf("err %v", err)
	}
	if r.h.HasBlob() {
		t.Fatal("blob stored for a wrong passphrase")
	}
	if _, ok := r.h.Unlocked(); ok {
		t.Fatal("state file written")
	}
	if !r.locked(t) || r.g.TestSign(r.fpr) == nil {
		t.Fatal("key left unlocked by a wrong passphrase")
	}
}

func TestCompleteRefusals(t *testing.T) {
	r := newRig(t, time.Hour)
	r.setup(t)
	k := newK(t)

	// A passkey the host did not pin.
	other := passkeytest.New(testRP)
	id, req, _ := r.h.NewRequest(testRP, true)
	a := r.answer(t, id, req, r.pass, k, true)
	cd, ad, sig := other.Get(passkey.ApprovalChallenge(req))
	a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature = other.ID(), passkey.B64(cd), passkey.B64(ad), passkey.B64(sig)
	if _, err := r.h.Complete(id, a); err == nil || !strings.Contains(err.Error(), "not one this host trusts") {
		t.Fatalf("unpinned passkey: %v", err)
	}

	// An assertion made for another request's challenge.
	id, req, _ = r.h.NewRequest(testRP, true)
	_, req2, _ := r.h.NewRequest(testRP, true)
	a = r.answer(t, id, req2, r.pass, k, true)
	if _, err := r.h.Complete(id, a); err != passkey.ErrChallenge {
		t.Fatalf("other request's assertion: %v", err)
	}

	// Without the user verification flag.
	id, req, _ = r.h.NewRequest(testRP, true)
	r.auth.Flags = passkey.FlagUP
	a = r.answer(t, id, req, r.pass, k, true)
	r.auth.Flags = passkey.FlagUP | passkey.FlagUV
	if _, err := r.h.Complete(id, a); err != passkey.ErrFlags {
		t.Fatalf("no UV: %v", err)
	}

	// K sealed to another request's key does not open.
	id, req, _ = r.h.NewRequest(testRP, true)
	other2, _ := ecdh.P256().GenerateKey(rand.Reader)
	a = r.answer(t, id, req, r.pass, k, true)
	a.Sealed, _ = Seal(CurveP256, other2.PublicKey().Bytes(), passkey.ApprovalChallenge(req), k)
	if _, err := r.h.Complete(id, a); err == nil {
		t.Fatal("sealed to another key opened")
	}

	// An expired request.
	id, req, _ = r.h.NewRequest(testRP, true)
	a = r.answer(t, id, req, r.pass, k, true)
	r.now.Add(RequestTTL + time.Second)
	if _, err := r.h.Complete(id, a); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}

	// A blob for another host.
	id, req, _ = r.h.NewRequest(testRP, true)
	a = r.answer(t, id, req, r.pass, k, true)
	a.Blob.Host = "w2"
	if _, err := r.h.Complete(id, a); err == nil {
		t.Fatal("blob for another host accepted")
	}

	// Unknown and traversing ids.
	for _, bad := range []string{"gu_none", "../x", "gu_../../etc"} {
		if _, err := r.h.Complete(bad, a); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if r.h.HasBlob() || !r.locked(t) {
		t.Fatal("a refused answer left state behind")
	}
}

func TestRequestShape(t *testing.T) {
	r := newRig(t, time.Hour)
	r.setup(t)
	_, req, err := r.h.NewRequest(testRP, false)
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]any
	if err := json.Unmarshal(req, &o); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"v", "action", "id", "host", "rp", "keygrips", "nonce", "exp", "pub", "curve", "host_key", "sig"} {
		if o[f] == nil {
			t.Errorf("request lacks %s", f)
		}
	}
	if o["action"] != "gpg-unlock" || o["host"] != "w1" {
		t.Errorf("request %v", o)
	}
	// The signature verifies under the host key.
	sig := o["sig"].(string)
	delete(o, "sig")
	unsigned, _ := json.Marshal(o)
	hk, _ := r.h.hostKey()
	raw, _ := passkey.UnB64(sig)
	if !verifyEd(hk.Public(), unsigned, raw) {
		t.Fatal("host signature does not verify")
	}
	if fp, _ := r.h.HostFingerprint(); fp == "" {
		t.Fatal("no host fingerprint")
	}
}

func TestReset(t *testing.T) {
	r := newRig(t, time.Hour)
	r.setup(t)
	if _, err := r.doSetup(t, newK(t), r.pass); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.Config(); err != ErrNotSetUp {
		t.Fatalf("config after reset: %v", err)
	}
	if r.h.HasBlob() || len(r.h.Pinned().Passkeys) != 0 {
		t.Fatal("reset left the blob or pinned keys")
	}
	if r.g.TestSign(r.fpr) == nil {
		t.Fatal("reset left the key unlocked")
	}
}
