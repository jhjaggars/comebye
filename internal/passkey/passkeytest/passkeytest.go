// Package passkeytest is a software authenticator for tests: it makes
// registration and assertion responses the way a browser's WebAuthn calls
// return them (ES256, attestation none), with flags a test can change.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"

	"github.com/jhjaggars/comebye/internal/passkey"
)

// Authenticator holds one generated credential.
type Authenticator struct {
	Key     *ecdsa.PrivateKey
	CredID  []byte
	RPID    string
	Origin  string // default https://RPID
	Flags   byte   // authenticator flags; default UP|UV (plus BE|BS on a registration)
	Counter uint32
}

// New makes an authenticator for rpID.
func New(rpID string) *Authenticator {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	return &Authenticator{Key: k, CredID: id, RPID: rpID, Origin: "https://" + rpID, Flags: passkey.FlagUP | passkey.FlagUV}
}

// ID is the credential id as WebAuthn encodes it.
func (a *Authenticator) ID() string { return passkey.B64(a.CredID) }

func head(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	}
	return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
}
func cint(n int64) []byte {
	if n >= 0 {
		return head(0, uint64(n))
	}
	return head(1, uint64(-1-n))
}
func cbytes(b []byte) []byte { return append(head(2, uint64(len(b))), b...) }
func ctext(s string) []byte  { return append(head(3, uint64(len(s))), s...) }
func cmap(kv ...[]byte) []byte {
	out := head(5, uint64(len(kv)/2))
	for _, x := range kv {
		out = append(out, x...)
	}
	return out
}

func (a *Authenticator) authData(flags byte, attested bool) []byte {
	h := sha256.Sum256([]byte(a.RPID))
	b := append([]byte(nil), h[:]...)
	b = append(b, flags)
	b = binary.BigEndian.AppendUint32(b, a.Counter)
	if attested {
		b = append(b, make([]byte, 16)...)
		b = binary.BigEndian.AppendUint16(b, uint16(len(a.CredID)))
		b = append(b, a.CredID...)
		raw, _ := a.Key.PublicKey.Bytes() // 0x04 || X || Y
		b = append(b, cmap(cint(1), cint(2), cint(3), cint(-7), cint(-1), cint(1),
			cint(-2), cbytes(raw[1:33]), cint(-3), cbytes(raw[33:65]))...)
	}
	return b
}

func (a *Authenticator) clientData(typ string, challenge []byte) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": passkey.B64(challenge), "origin": a.Origin, "crossOrigin": false})
	return b
}

// Create answers a registration: client data JSON and the attestation object.
func (a *Authenticator) Create(challenge []byte) (clientDataJSON, attestationObject []byte) {
	ad := a.authData(a.Flags|passkey.FlagAT|passkey.FlagBE|passkey.FlagBS, true)
	att := cmap(ctext("fmt"), ctext("none"), ctext("attStmt"), cmap(), ctext("authData"), cbytes(ad))
	return a.clientData("webauthn.create", challenge), att
}

// Get answers an assertion for challenge.
func (a *Authenticator) Get(challenge []byte) (clientDataJSON, authenticatorData, signature []byte) {
	ad := a.authData(a.Flags, false)
	cd := a.clientData("webauthn.get", challenge)
	cdh := sha256.Sum256(cd)
	m := sha256.Sum256(append(append([]byte(nil), ad...), cdh[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, a.Key, m[:])
	return cd, ad, sig
}
