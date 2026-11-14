package passkey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testRP = "hub.tailnet.ts.net"

var testOpts = Options{RPID: testRP}

// ---- helpers: hand-built CBOR, authenticator data and responses ----

func cbUint(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	case n < 65536:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
	b := []byte{major<<5 | 26, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], uint32(n))
	return b
}
func cbInt(n int64) []byte {
	if n >= 0 {
		return cbUint(0, uint64(n))
	}
	return cbUint(1, uint64(-1-n))
}
func cbBytes(b []byte) []byte { return append(cbUint(2, uint64(len(b))), b...) }
func cbText(s string) []byte  { return append(cbUint(3, uint64(len(s))), s...) }
func cbMap(kv ...[]byte) []byte {
	out := cbUint(5, uint64(len(kv)/2))
	for _, x := range kv {
		out = append(out, x...)
	}
	return out
}

func cose(k *ecdsa.PublicKey, alg int64) []byte {
	raw, _ := k.Bytes() // 0x04 || X || Y
	return cbMap(cbInt(1), cbInt(2), cbInt(3), cbInt(alg), cbInt(-1), cbInt(1),
		cbInt(-2), cbBytes(raw[1:33]), cbInt(-3), cbBytes(raw[33:65]))
}

func authData(rp string, flags byte, counter uint32, credID []byte, coseKey []byte) []byte {
	h := sha256.Sum256([]byte(rp))
	b := append([]byte(nil), h[:]...)
	b = append(b, flags)
	b = binary.BigEndian.AppendUint32(b, counter)
	if flags&FlagAT != 0 {
		b = append(b, make([]byte, 16)...)
		b = binary.BigEndian.AppendUint16(b, uint16(len(credID)))
		b = append(b, credID...)
		b = append(b, coseKey...)
	}
	return b
}

func clientDataJSON(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": B64(challenge), "origin": origin, "crossOrigin": false})
	return b
}

func attestation(fmtName string, ad []byte, stmt []byte) []byte {
	if stmt == nil {
		stmt = cbMap()
	}
	return cbMap(cbText("fmt"), cbText(fmtName), cbText("attStmt"), stmt, cbText("authData"), cbBytes(ad))
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, k *ecdsa.PrivateKey, ad, cd []byte) []byte {
	t.Helper()
	cdh := sha256.Sum256(cd)
	m := sha256.Sum256(append(append([]byte(nil), ad...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k, m[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// ---- registration ----

func TestVerifyRegistration(t *testing.T) {
	k := newKey(t)
	cid := []byte("credential-id-0123456789")
	ch := []byte("0123456789abcdef0123456789abcdef")
	good := byte(FlagUP | FlagUV | FlagAT | FlagBE | FlagBS)
	origin := "https://" + testRP
	mk := func(rp string, flags byte, alg int64, fm string, stmt []byte) []byte {
		return attestation(fm, authData(rp, flags, 0, cid, cose(&k.PublicKey, alg)), stmt)
	}
	cases := []struct {
		name   string
		att    []byte
		cd     []byte
		opts   Options
		wantEr error
	}{
		{"ok", mk(testRP, good, -7, "none", nil), clientDataJSON("webauthn.create", ch, origin), testOpts, nil},
		{"ok with a port", mk(testRP, good, -7, "none", nil), clientDataJSON("webauthn.create", ch, origin+":8443"), testOpts, nil},
		{"wrong rp hash", mk("other.ts.net", good, -7, "none", nil), clientDataJSON("webauthn.create", ch, origin), testOpts, ErrRPID},
		{"no user verification", mk(testRP, FlagUP|FlagAT, -7, "none", nil), clientDataJSON("webauthn.create", ch, origin), testOpts, ErrFlags},
		{"no user presence", mk(testRP, FlagUV|FlagAT, -7, "none", nil), clientDataJSON("webauthn.create", ch, origin), testOpts, ErrFlags},
		{"packed attestation", mk(testRP, good, -7, "packed", nil), clientDataJSON("webauthn.create", ch, origin), testOpts, ErrAttest},
		{"none with a statement", mk(testRP, good, -7, "none", cbMap(cbText("x"), cbInt(1))), clientDataJSON("webauthn.create", ch, origin), testOpts, ErrAttest},
		{"rs256", mk(testRP, good, -257, "none", nil), clientDataJSON("webauthn.create", ch, origin), testOpts, ErrAlg},
		{"wrong challenge", mk(testRP, good, -7, "none", nil), clientDataJSON("webauthn.create", []byte("another challenge...........xxxx"), origin), testOpts, ErrChallenge},
		{"a get, not a create", mk(testRP, good, -7, "none", nil), clientDataJSON("webauthn.get", ch, origin), testOpts, ErrType},
		{"wrong origin", mk(testRP, good, -7, "none", nil), clientDataJSON("webauthn.create", ch, "https://evil.example"), testOpts, ErrOrigin},
		{"http origin", mk(testRP, good, -7, "none", nil), clientDataJSON("webauthn.create", ch, "http://"+testRP), testOpts, ErrOrigin},
		{"lookalike origin", mk(testRP, good, -7, "none", nil), clientDataJSON("webauthn.create", ch, "https://"+testRP+".evil.example"), testOpts, ErrOrigin},
		{"no key in the data", attestation("none", authData(testRP, FlagUP|FlagUV, 0, nil, nil), nil), clientDataJSON("webauthn.create", ch, origin), testOpts, errAny},
		{"not CBOR", []byte{0xff, 0x00}, clientDataJSON("webauthn.create", ch, origin), testOpts, errAny},
		{"localhost without the switch", mk("localhost", good, -7, "none", nil), clientDataJSON("webauthn.create", ch, "http://localhost:8080"), Options{RPID: "localhost"}, ErrOrigin},
		{"localhost under test mode", mk("localhost", good, -7, "none", nil), clientDataJSON("webauthn.create", ch, "http://localhost:8080"), Options{RPID: "localhost", AllowLocalhost: true}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg, err := VerifyRegistration(c.opts, ch, c.cd, c.att)
			checkErr(t, err, c.wantEr)
			if err != nil {
				return
			}
			if reg.ID != B64(cid) || !reg.BackupEligible || !reg.BackedUp {
				t.Errorf("registration = %+v", reg)
			}
			pk, err := ParsePublicKey(reg.PublicKey)
			if err != nil || !pk.Equal(&k.PublicKey) {
				t.Errorf("stored key does not round-trip: %v", err)
			}
		})
	}
}

var errAny = errors.New("any error")

func checkErr(t *testing.T, got, want error) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("unexpected error: %v", got)
	case want == errAny && got == nil:
		t.Fatal("expected an error")
	case want != nil && want != errAny && !errors.Is(got, want):
		t.Fatalf("error = %v, want %v", got, want)
	}
}

// ---- assertion ----

func TestVerifyAssertion(t *testing.T) {
	k := newKey(t)
	other := newKey(t)
	spki, _ := MarshalPublicKey(&k.PublicKey)
	ch := ApprovalChallenge([]byte(`{"action":"gpg-unlock","host":"w3"}`))
	otherAction := ApprovalChallenge([]byte(`{"action":"peer-add","host":"w3"}`))
	origin := "https://" + testRP
	uv := byte(FlagUP | FlagUV)
	cases := []struct {
		name     string
		rp       string
		flags    byte
		counter  uint32
		stored   uint32
		signer   *ecdsa.PrivateKey
		signedCh []byte // the challenge in the signed client data
		origin   string
		typ      string
		want     error
		wantCtr  uint32
	}{
		{"ok, counter 0 and 0", testRP, uv, 0, 0, k, ch, origin, "webauthn.get", nil, 0},
		{"counter 0 after a stored 5 is ignored", testRP, uv, 0, 5, k, ch, origin, "webauthn.get", nil, 5},
		{"counter goes up", testRP, uv, 7, 5, k, ch, origin, "webauthn.get", nil, 7},
		{"counter first seen", testRP, uv, 3, 0, k, ch, origin, "webauthn.get", nil, 3},
		{"counter going back", testRP, uv, 4, 5, k, ch, origin, "webauthn.get", ErrCounter, 0},
		{"counter repeated", testRP, uv, 5, 5, k, ch, origin, "webauthn.get", ErrCounter, 0},
		{"wrong rp hash", "other.ts.net", uv, 0, 0, k, ch, origin, "webauthn.get", ErrRPID, 0},
		{"no user verification", testRP, FlagUP, 0, 0, k, ch, origin, "webauthn.get", ErrFlags, 0},
		{"no user presence", testRP, FlagUV, 0, 0, k, ch, origin, "webauthn.get", ErrFlags, 0},
		{"signed by another key", testRP, uv, 0, 0, other, ch, origin, "webauthn.get", ErrSignature, 0},
		{"another action's challenge", testRP, uv, 0, 0, k, otherAction, origin, "webauthn.get", ErrChallenge, 0},
		{"wrong origin", testRP, uv, 0, 0, k, ch, "https://evil.example", "webauthn.get", ErrOrigin, 0},
		{"a create, not a get", testRP, uv, 0, 0, k, ch, origin, "webauthn.create", ErrType, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ad := authData(c.rp, c.flags, c.counter, nil, nil)
			cd := clientDataJSON(c.typ, c.signedCh, c.origin)
			sig := sign(t, c.signer, ad, cd)
			ctr, err := VerifyAssertion(testOpts, spki, c.stored, ch, cd, ad, sig)
			checkErr(t, err, c.want)
			if err == nil && ctr != c.wantCtr {
				t.Errorf("counter = %d, want %d", ctr, c.wantCtr)
			}
		})
	}
	t.Run("signature over other data", func(t *testing.T) {
		ad := authData(testRP, uv, 0, nil, nil)
		cd := clientDataJSON("webauthn.get", ch, origin)
		sig := sign(t, k, ad, cd)
		ad2 := authData(testRP, uv, 1, nil, nil) // tampered after signing
		_, err := VerifyAssertion(testOpts, spki, 0, ch, cd, ad2, sig)
		checkErr(t, err, ErrSignature)
	})
	t.Run("garbage signature", func(t *testing.T) {
		ad := authData(testRP, uv, 0, nil, nil)
		cd := clientDataJSON("webauthn.get", ch, origin)
		_, err := VerifyAssertion(testOpts, spki, 0, ch, cd, ad, []byte("nope"))
		checkErr(t, err, ErrSignature)
	})
	t.Run("short authenticator data", func(t *testing.T) {
		cd := clientDataJSON("webauthn.get", ch, origin)
		_, err := VerifyAssertion(testOpts, spki, 0, ch, cd, make([]byte, 10), nil)
		checkErr(t, err, errAny)
	})
	t.Run("empty challenge never matches", func(t *testing.T) {
		ad := authData(testRP, uv, 0, nil, nil)
		cd := clientDataJSON("webauthn.get", nil, origin)
		_, err := VerifyAssertion(testOpts, spki, 0, nil, cd, ad, sign(t, k, ad, cd))
		checkErr(t, err, ErrChallenge)
	})
}

func TestApprovalChallengeBindsTheRequest(t *testing.T) {
	a := ApprovalChallenge([]byte(`{"a":1}`))
	if string(a) == string(ApprovalChallenge([]byte(`{"a":2}`))) || len(a) != 32 {
		t.Fatal("challenge must differ per request and be 32 bytes")
	}
}

func TestFingerprint(t *testing.T) {
	k := newKey(t)
	spki, _ := MarshalPublicKey(&k.PublicKey)
	f := Fingerprint(spki)
	if len(f) != 24 || strings.Count(f, "-") != 4 || f != strings.ToUpper(f) {
		t.Errorf("fingerprint = %q", f)
	}
	if Fingerprint("!!") != "" {
		t.Error("a bad key has no fingerprint")
	}
}

func TestCBORRefusals(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":            nil,
		"truncated string": {0x63, 'a'},
		"indefinite array": {0x9f, 0x01, 0xff},
		"float":            {0xf9, 0x3c, 0x00},
		"tag":              {0xc1, 0x01},
		"huge array":       {0x9b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"byte key":         {0xa1, 0x41, 0x00, 0x01},
		"duplicate key":    {0xa2, 0x01, 0x01, 0x01, 0x02},
		"too deep":         nil,
	} {
		if name == "too deep" {
			in = append([]byte(strings.Repeat("\x81", 20)), 0x00)
		}
		if _, _, err := cborDecode(in); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	v, rest, err := cborDecode(append(cbMap(cbText("a"), cbInt(-5)), 0x99))
	if err != nil || len(rest) != 1 || v.(map[any]any)["a"].(int64) != -5 {
		t.Errorf("decode = %v %v %v", v, rest, err)
	}
}
