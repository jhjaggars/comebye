package archive

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Encryption is public-key, so the archiving host holds only a public
// recipient key and cannot read what it wrote; the identity (private key)
// is needed to restore, search and rebuild the index. Each segment gets a
// fresh X25519 ephemeral key, HKDF-SHA256 derives a one-use AES-256-GCM key
// from the shared secret, and the segment's identity (stream, offset,
// length) is bound as additional data, so a segment cannot be swapped or
// moved without detection.
//
// This is ECIES built from the standard library only (the repo takes no
// module dependencies). It is NOT the age file format; swapping in
// filippo.io/age behind Sealer/Opener is a one-file change if module
// dependencies are approved.

const (
	pubPrefix = "cbapub1:"
	secPrefix = "cbasec1:"
	hkdfInfo  = "comebye-archive-v1"
)

// Sealer encrypts a segment.
type Sealer interface {
	Seal(plain, aad []byte) ([]byte, error)
}

// Opener decrypts a segment.
type Opener interface {
	Open(sealed, aad []byte) ([]byte, error)
}

// GenerateKey returns a new recipient (public) and identity (private) key
// in their text forms.
func GenerateKey() (recipient, identity string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return pubPrefix + base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		secPrefix + base64.RawURLEncoding.EncodeToString(k.Bytes()), nil
}

type x25519Sealer struct{ pub *ecdh.PublicKey }

// NewSealer parses a recipient key ("cbapub1:...").
func NewSealer(recipient string) (Sealer, error) {
	raw, err := decodeKey(strings.TrimSpace(recipient), pubPrefix)
	if err != nil {
		return nil, err
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("archive: bad recipient key: %w", err)
	}
	return &x25519Sealer{pub}, nil
}

func decodeKey(s, prefix string) ([]byte, error) {
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("archive: key must start with %q", prefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("archive: malformed key")
	}
	return raw, nil
}

func segKey(shared, ephPub, rcptPub []byte) ([]byte, error) {
	salt := append(append([]byte{}, ephPub...), rcptPub...)
	return hkdf.Key(sha256.New, shared, salt, hkdfInfo, 32)
}

func (s *x25519Sealer) Seal(plain, aad []byte) ([]byte, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(s.pub)
	if err != nil {
		return nil, err
	}
	key, err := segKey(shared, eph.PublicKey().Bytes(), s.pub.Bytes())
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{}, eph.PublicKey().Bytes()...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plain, aad), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

type x25519Opener struct{ priv *ecdh.PrivateKey }

// NewOpener parses an identity ("cbasec1:...").
func NewOpener(identity string) (Opener, error) {
	raw, err := decodeKey(strings.TrimSpace(identity), secPrefix)
	if err != nil {
		return nil, err
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("archive: bad identity: %w", err)
	}
	return &x25519Opener{priv}, nil
}

// RecipientOf returns the recipient key for an identity.
func RecipientOf(identity string) (string, error) {
	raw, err := decodeKey(strings.TrimSpace(identity), secPrefix)
	if err != nil {
		return "", err
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", err
	}
	return pubPrefix + base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

func (o *x25519Opener) Open(sealed, aad []byte) ([]byte, error) {
	if len(sealed) < 32+12+16 {
		return nil, errors.New("archive: sealed segment too short")
	}
	ephPub, err := ecdh.X25519().NewPublicKey(sealed[:32])
	if err != nil {
		return nil, err
	}
	shared, err := o.priv.ECDH(ephPub)
	if err != nil {
		return nil, err
	}
	key, err := segKey(shared, sealed[:32], o.priv.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce, ct := sealed[32:44], sealed[44:]
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, errors.New("archive: segment failed authentication (wrong key, or the object was altered)")
	}
	return plain, nil
}
