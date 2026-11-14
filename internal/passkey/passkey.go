package passkey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Authenticator data flags.
const (
	FlagUP = 0x01 // user present
	FlagUV = 0x04 // user verified
	FlagBE = 0x08 // backup eligible (a synced passkey)
	FlagBS = 0x10 // backed up
	FlagAT = 0x40 // attested credential data follows
	FlagED = 0x80 // extension data follows
)

// Errors a caller can tell apart. The text of each is written for the user.
var (
	ErrChallenge = errors.New("the challenge does not match")
	ErrOrigin    = errors.New("the passkey was used on another address than this hub's")
	ErrRPID      = errors.New("the passkey belongs to another site name")
	ErrType      = errors.New("the browser answered a different kind of request")
	ErrFlags     = errors.New("the passkey did not report presence and Face ID (user verification)")
	ErrSignature = errors.New("the signature does not verify")
	ErrCounter   = errors.New("the passkey's counter went backwards")
	ErrAttest    = errors.New("only attestation none is accepted")
	ErrAlg       = errors.New("only ES256 (P-256) passkeys are accepted")
)

// RPIDHash is SHA-256 of the RP ID.
func RPIDHash(rpID string) [32]byte { return sha256.Sum256([]byte(rpID)) }

// Options sets what a check expects.
type Options struct {
	// RPID is the hub's host name; Origin must be https on that name (any
	// port) unless AllowLocalhost is set, which also admits http://localhost
	// for the test harness.
	RPID           string
	AllowLocalhost bool
}

// ParsedAuthData is the authenticator data of a registration or an assertion.
type ParsedAuthData struct {
	RPIDHash  [32]byte
	Flags     byte
	Counter   uint32
	AAGUID    []byte
	CredID    []byte
	PublicKey *ecdsa.PublicKey // set when the AT flag is
	Raw       []byte
}

// BackupEligible reports a synced (multi-device) passkey.
func (a ParsedAuthData) BackupEligible() bool { return a.Flags&FlagBE != 0 }

// BackedUp reports that the passkey is currently synced.
func (a ParsedAuthData) BackedUp() bool { return a.Flags&FlagBS != 0 }

// ParseAuthData parses authenticator data. With attested set, the attested
// credential data (the new key) must be present.
func ParseAuthData(b []byte, attested bool) (ParsedAuthData, error) {
	var a ParsedAuthData
	if len(b) < 37 {
		return a, errors.New("the authenticator data is too short")
	}
	copy(a.RPIDHash[:], b[:32])
	a.Flags = b[32]
	a.Counter = binary.BigEndian.Uint32(b[33:37])
	a.Raw = b
	rest := b[37:]
	if a.Flags&FlagAT == 0 {
		if attested {
			return a, errors.New("the registration carries no new key")
		}
		return a, nil
	}
	if len(rest) < 18 {
		return a, errors.New("the attested credential data is too short")
	}
	a.AAGUID = rest[:16]
	n := int(binary.BigEndian.Uint16(rest[16:18]))
	rest = rest[18:]
	if n == 0 || n > 1023 || len(rest) < n {
		return a, errors.New("the credential id is malformed")
	}
	a.CredID = rest[:n]
	rest = rest[n:]
	key, after, err := cborDecode(rest)
	if err != nil {
		return a, fmt.Errorf("the public key: %w", err)
	}
	if a.PublicKey, err = coseES256(key); err != nil {
		return a, err
	}
	if a.Flags&FlagED == 0 && len(after) != 0 {
		return a, errors.New("unexpected bytes after the public key")
	}
	return a, nil
}

// coseES256 reads a COSE_Key and returns its P-256 key. It refuses anything
// but EC2, ES256, P-256, and a point that is not on the curve.
func coseES256(v any) (*ecdsa.PublicKey, error) {
	m, ok := v.(map[any]any)
	if !ok {
		return nil, errors.New("the public key is not a COSE key")
	}
	if kty, _ := m[int64(1)].(int64); kty != 2 {
		return nil, ErrAlg
	}
	if alg, _ := m[int64(3)].(int64); alg != -7 {
		return nil, ErrAlg
	}
	if crv, _ := m[int64(-1)].(int64); crv != 1 {
		return nil, ErrAlg
	}
	x, _ := m[int64(-2)].([]byte)
	y, _ := m[int64(-3)].([]byte)
	if len(x) != 32 || len(y) != 32 {
		return nil, errors.New("the public key coordinates are malformed")
	}
	// ParseUncompressedPublicKey rejects a point that is not on P-256.
	k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append([]byte{4}, append(append([]byte(nil), x...), y...)...))
	if err != nil {
		return nil, errors.New("the public key is not a point on P-256")
	}
	return k, nil
}

// MarshalPublicKey is the key as the hub stores it: PKIX DER, base64url.
func MarshalPublicKey(k *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return "", err
	}
	return B64(der), nil
}

// ParsePublicKey reads what MarshalPublicKey wrote.
func ParsePublicKey(s string) (*ecdsa.PublicKey, error) {
	der, err := UnB64(s)
	if err != nil {
		return nil, err
	}
	pk, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	k, ok := pk.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, ErrAlg
	}
	return k, nil
}

// B64 and UnB64 are unpadded base64url, WebAuthn's own encoding.
func B64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func UnB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Fingerprint is a short, comparable name for a public key: the first 10
// bytes of SHA-256 of its DER, as five groups of four hex digits.
func Fingerprint(spki string) string {
	der, err := UnB64(spki)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	h := fmt.Sprintf("%x", sum[:10])
	return strings.ToUpper(h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20])
}

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

func (o Options) checkClientData(raw []byte, typ string, challenge []byte) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return errors.New("the client data is not JSON")
	}
	if cd.Type != typ {
		return ErrType
	}
	got, err := UnB64(cd.Challenge)
	if err != nil || len(challenge) == 0 || !bytes.Equal(got, challenge) {
		return ErrChallenge
	}
	if cd.CrossOrigin {
		return ErrOrigin
	}
	u, err := url.Parse(cd.Origin)
	if err != nil || u.Host == "" || u.Path != "" || u.User != nil {
		return ErrOrigin
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https" && host == strings.ToLower(o.RPID):
	case o.AllowLocalhost && (u.Scheme == "http" || u.Scheme == "https") && host == "localhost" && strings.EqualFold(o.RPID, "localhost"):
	default:
		return ErrOrigin
	}
	return nil
}

// Registration is what a verified registration yields.
type Registration struct {
	ID             string // base64url credential id
	PublicKey      string // PKIX DER, base64url
	RPIDHash       string // hex
	BackupEligible bool
	BackedUp       bool
	Counter        uint32
	AAGUID         string // hex
}

// VerifyRegistration checks a navigator.credentials.create response against
// the challenge the hub issued: client data, attestation none, RP ID hash,
// flags (UP and UV), ES256.
func VerifyRegistration(o Options, challenge, clientDataJSON, attestationObject []byte) (Registration, error) {
	var out Registration
	if err := o.checkClientData(clientDataJSON, "webauthn.create", challenge); err != nil {
		return out, err
	}
	v, rest, err := cborDecode(attestationObject)
	if err != nil || len(rest) != 0 {
		return out, errors.New("the attestation object is malformed")
	}
	m, ok := v.(map[any]any)
	if !ok {
		return out, errors.New("the attestation object is malformed")
	}
	if f, _ := m["fmt"].(string); f != "none" {
		return out, ErrAttest
	}
	if st, ok := m["attStmt"].(map[any]any); !ok || len(st) != 0 {
		return out, ErrAttest
	}
	raw, _ := m["authData"].([]byte)
	ad, err := ParseAuthData(raw, true)
	if err != nil {
		return out, err
	}
	if ad.RPIDHash != RPIDHash(o.RPID) {
		return out, ErrRPID
	}
	if ad.Flags&FlagUP == 0 || ad.Flags&FlagUV == 0 {
		return out, ErrFlags
	}
	spki, err := MarshalPublicKey(ad.PublicKey)
	if err != nil {
		return out, err
	}
	return Registration{
		ID: B64(ad.CredID), PublicKey: spki, RPIDHash: fmt.Sprintf("%x", ad.RPIDHash[:]),
		BackupEligible: ad.BackupEligible(), BackedUp: ad.BackedUp(), Counter: ad.Counter, AAGUID: fmt.Sprintf("%x", ad.AAGUID),
	}, nil
}

// VerifyAssertion checks a navigator.credentials.get response for the
// challenge, against a stored public key. It returns the new counter.
//
// Synced passkeys report counter 0 every time, so the counter is checked only
// when it can be: both the stored and the new value above 0 and the new one
// not higher is a failure. Replay protection is the caller's: a nonce, an
// expiry and a single use.
func VerifyAssertion(o Options, spki string, storedCounter uint32, challenge, clientDataJSON, authenticatorData, signature []byte) (uint32, error) {
	if err := o.checkClientData(clientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	ad, err := ParseAuthData(authenticatorData, false)
	if err != nil {
		return 0, err
	}
	if ad.RPIDHash != RPIDHash(o.RPID) {
		return 0, ErrRPID
	}
	if ad.Flags&FlagUP == 0 || ad.Flags&FlagUV == 0 {
		return 0, ErrFlags
	}
	key, err := ParsePublicKey(spki)
	if err != nil {
		return 0, err
	}
	cdh := sha256.Sum256(clientDataJSON)
	msg := sha256.Sum256(append(append([]byte(nil), authenticatorData...), cdh[:]...))
	if !ecdsa.VerifyASN1(key, msg[:], signature) {
		return 0, ErrSignature
	}
	if storedCounter > 0 && ad.Counter > 0 && ad.Counter <= storedCounter {
		return 0, ErrCounter
	}
	if ad.Counter > storedCounter {
		return ad.Counter, nil
	}
	return storedCounter, nil
}

// ApprovalChallenge is the challenge an approval signs: SHA-256 of a domain
// prefix and the request's canonical JSON, so an assertion made for one
// request is useless for any other.
func ApprovalChallenge(request []byte) []byte {
	h := sha256.New()
	h.Write([]byte("comebye-approval-v1\x00"))
	h.Write(request)
	return h.Sum(nil)
}
