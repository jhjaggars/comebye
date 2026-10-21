// Package push implements Web Push (RFC 8030) for herdr-phone: VAPID
// application server identification (RFC 8292), aes128gcm payload
// encryption (RFC 8291 / RFC 8188), a persistent subscription store, and
// a bounded asynchronous sender. It uses only the Go standard library.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

// recordSize is the aes128gcm record size advertised in the header. A
// push message is a single record, so this only has to exceed the
// ciphertext; 4096 is what push services are required to accept.
const recordSize = 4096

// headerLen is salt(16) + rs(4) + idlen(1) + keyid(65).
const headerLen = 16 + 4 + 1 + 65

// MaxPlaintext is the largest payload that fits in one 4096-octet record:
// 4096 - header - GCM tag (16) - padding delimiter (1).
const MaxPlaintext = recordSize - headerLen - 16 - 1

var b64 = base64.RawURLEncoding

// decodeB64 accepts base64url with or without padding (browsers emit the
// unpadded form, but some libraries pad).
func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// encrypt encrypts plaintext for a subscription per RFC 8291: an ECDH
// agreement between asPriv (a fresh ephemeral key per message) and the
// browser's p256dh key, combined with the browser's auth secret, keys a
// single aes128gcm record. salt must be 16 random octets.
func encrypt(plaintext []byte, uaPublic, authSecret []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, fmt.Errorf("payload too large: %d > %d bytes", len(plaintext), MaxPlaintext)
	}
	if len(salt) != 16 {
		return nil, errors.New("salt must be 16 bytes")
	}
	if len(authSecret) != 16 {
		return nil, fmt.Errorf("auth secret must be 16 bytes, got %d", len(authSecret))
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	ecdhSecret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPublic := asPriv.PublicKey().Bytes()

	// key_info = "WebPush: info" || 0x00 || ua_public || as_public
	keyInfo := make([]byte, 0, 14+65+65)
	keyInfo = append(keyInfo, "WebPush: info\x00"...)
	keyInfo = append(keyInfo, uaPublic...)
	keyInfo = append(keyInfo, asPublic...)
	ikm, err := hkdf.Key(sha256.New, ecdhSecret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, headerLen+len(plaintext)+1+gcm.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPublic)))
	out = append(out, asPublic...)

	// One record, so the sequence number is 0 and the nonce is used as is.
	// 0x02 marks the last (only) record; no further padding.
	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, 0x02)
	return gcm.Seal(out, nonce, record, nil), nil
}

// Encrypt encrypts plaintext for a browser subscription with a fresh
// ephemeral key and random salt.
func Encrypt(plaintext []byte, keys Keys) ([]byte, error) {
	uaPublic, err := decodeB64(keys.P256DH)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	auth, err := decodeB64(keys.Auth)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return encrypt(plaintext, uaPublic, auth, asPriv, salt)
}

// vapidJWT builds the ES256 JWT for RFC 8292: aud is the push service
// origin, exp at most 24h ahead (we use 12h), sub a contact URI.
func vapidJWT(key *ecdsa.PrivateKey, audience, subject string, now time.Time) (string, error) {
	header := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{audience, now.Add(12 * time.Hour).Unix(), subject})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 signatures are the raw 32-byte big-endian R || S, not DER.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// verifyES256 checks a raw R||S signature; used by tests.
func verifyES256(pub *ecdsa.PublicKey, signingInput string, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(signingInput))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, digest[:], r, s)
}
