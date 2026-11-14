package gpgunlock

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jhjaggars/comebye/internal/passkey"
)

// Curves a request's one-use key may use. The browser picks the one its
// WebCrypto supports (X25519 where it can, else P-256).
const (
	CurveP256   = "P-256"
	CurveX25519 = "X25519"
)

func curveOf(name string) (ecdh.Curve, error) {
	switch name {
	case CurveP256:
		return ecdh.P256(), nil
	case CurveX25519:
		return ecdh.X25519(), nil
	}
	return nil, errors.New("unknown curve " + name)
}

// PRFSalt is the salt the browser gives the passkey's PRF: it names the host
// and the keys, so each host gets a different K.
func PRFSalt(host string, keygrips []string) []byte {
	h := sha256.New()
	h.Write([]byte("comebye-prf-v1\x00" + host + "\x00" + strings.Join(keygrips, ",")))
	return h.Sum(nil)
}

// DeriveK is K from the PRF output (the browser does this; here for tests).
func DeriveK(prf []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, prf, nil, "comebye-gpg-k-v1", 32)
}

// sealed is K sealed to a request's one-use key: ECDH with an ephemeral key,
// HKDF salted with the request's challenge, AES-256-GCM with the challenge as
// authenticated data. It binds K to that one request.
type sealed struct {
	V     int    `json:"v"`
	Curve string `json:"curve"`
	EPK   string `json:"epk"`
	IV    string `json:"iv"`
	CT    string `json:"ct"`
}

func sealKey(shared, challenge []byte) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, shared, challenge, "comebye-seal-v1", 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Seal seals k to the request's public key pub (raw: uncompressed point for
// P-256, 32 bytes for X25519). The browser does this; here for tests.
func Seal(curve string, pub, challenge, k []byte) (string, error) {
	c, err := curveOf(curve)
	if err != nil {
		return "", err
	}
	rp, err := c.NewPublicKey(pub)
	if err != nil {
		return "", errors.New("the request's key is not valid")
	}
	eph, err := c.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := eph.ECDH(rp)
	if err != nil {
		return "", err
	}
	aead, err := sealKey(shared, challenge)
	if err != nil {
		return "", err
	}
	iv := make([]byte, aead.NonceSize())
	_, _ = rand.Read(iv)
	b, _ := json.Marshal(sealed{V: 1, Curve: curve, EPK: passkey.B64(eph.PublicKey().Bytes()), IV: passkey.B64(iv),
		CT: passkey.B64(aead.Seal(nil, iv, k, challenge))})
	return passkey.B64(b), nil
}

// openSealed opens K with the request's private key.
func openSealed(curve string, priv, challenge []byte, s string) ([]byte, error) {
	raw, err := passkey.UnB64(s)
	var sl sealed
	if err != nil || json.Unmarshal(raw, &sl) != nil || sl.V != 1 || sl.Curve != curve {
		return nil, errors.New("the sealed key is malformed")
	}
	c, err := curveOf(curve)
	if err != nil {
		return nil, err
	}
	pk, err := c.NewPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	epk, _ := passkey.UnB64(sl.EPK)
	iv, _ := passkey.UnB64(sl.IV)
	ct, _ := passkey.UnB64(sl.CT)
	ep, err := c.NewPublicKey(epk)
	if err != nil {
		return nil, errors.New("the sealed key is malformed")
	}
	shared, err := pk.ECDH(ep)
	if err != nil {
		return nil, errors.New("the sealed key is malformed")
	}
	aead, err := sealKey(shared, challenge)
	if err != nil || len(iv) != aead.NonceSize() {
		return nil, errors.New("the sealed key is malformed")
	}
	k, err := aead.Open(nil, iv, ct, challenge)
	if err != nil || len(k) != 32 {
		return nil, errors.New("the sealed key does not open")
	}
	return k, nil
}

// Blob is the passphrase at rest: AES-256-GCM under K, with the host and the
// keygrips as authenticated data, so a blob works only for the host and keys
// it was made for.
type Blob struct {
	V        int      `json:"v"`
	Host     string   `json:"host"`
	Keygrips []string `json:"keygrips"`
	IV       string   `json:"iv"`
	CT       string   `json:"ct"`
}

func blobAAD(host string, keygrips []string) []byte {
	return []byte("comebye-gpg-blob-v1\x00" + host + "\x00" + strings.Join(keygrips, ","))
}

func gcm(k []byte) (cipher.AEAD, error) {
	if len(k) != 32 {
		return nil, errors.New("K must be 32 bytes")
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// EncryptBlob makes the blob (the browser does this at setup; for tests).
func EncryptBlob(k []byte, host string, keygrips []string, pass []byte) (Blob, error) {
	a, err := gcm(k)
	if err != nil {
		return Blob{}, err
	}
	iv := make([]byte, a.NonceSize())
	_, _ = rand.Read(iv)
	return Blob{V: 1, Host: host, Keygrips: keygrips, IV: passkey.B64(iv),
		CT: passkey.B64(a.Seal(nil, iv, pass, blobAAD(host, keygrips)))}, nil
}

// Decrypt returns the passphrase. Callers wipe it.
func (b Blob) Decrypt(k []byte) ([]byte, error) {
	a, err := gcm(k)
	if err != nil || b.V != 1 {
		return nil, errors.New("the stored passphrase cannot be read")
	}
	iv, _ := passkey.UnB64(b.IV)
	ct, _ := passkey.UnB64(b.CT)
	if len(iv) != a.NonceSize() {
		return nil, errors.New("the stored passphrase cannot be read")
	}
	p, err := a.Open(nil, iv, ct, blobAAD(b.Host, b.Keygrips))
	if err != nil {
		return nil, errors.New("the passkey's key does not open the stored passphrase")
	}
	return p, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
