package gpgunlock

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/passkey"
)

// DefaultLifetime is how long an unlock lasts. It is fixed at 8 hours; the
// field exists so tests can shorten it.
const DefaultLifetime = 8 * time.Hour

// RequestTTL is how long a request waits for the passkey.
const RequestTTL = 120 * time.Second

// Config is what setup decided: which key, which host, which hub name.
type Config struct {
	Host     string   `json:"host"`
	FPR      string   `json:"fpr"`
	Keygrips []string `json:"keygrips"`
}

// Pinned is the passkey set the host trusts, compared by the user once at
// setup. The hub's list is never trusted afterwards.
type Pinned struct {
	RPID     string      `json:"rp_id"`
	Passkeys []PinnedKey `json:"passkeys"`
}

// PinnedKey is one trusted passkey.
type PinnedKey struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	PublicKey string `json:"public_key"`
	Counter   uint32 `json:"counter,omitempty"`
}

// State is the unlock in force, so a restart cannot extend it.
type State struct {
	ExpiresAt time.Time `json:"expires_at"`
	Keygrips  []string  `json:"keygrips"`
}

// Host is the GPG unlock state of one host, kept in Dir.
type Host struct {
	Dir      string
	GPG      *GPG
	Lifetime time.Duration // DefaultLifetime when zero
	Now      func() time.Time

	mu sync.Mutex
}

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Host) lifetime() time.Duration {
	if h.Lifetime > 0 {
		return h.Lifetime
	}
	return DefaultLifetime
}

func (h *Host) path(n string) string { return filepath.Join(h.Dir, n) }

func (h *Host) writeJSON(name string, v any) error {
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.path(name + ".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path(name))
}

func (h *Host) readJSON(name string, v any) error {
	b, err := os.ReadFile(h.path(name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Files in Dir.
const (
	fileConfig  = "gpg-config.json"
	filePinned  = "gpg-pinned.json"
	fileBlob    = "gpg-blob.json"
	fileHostKey = "gpg-host-key.json"
	fileState   = "gpg-unlock.json"
	dirPending  = "gpg-pending"
)

// ErrNotSetUp is returned where setup has not been done.
var ErrNotSetUp = errors.New("GPG unlock is not set up on this host: run `comebye gpg setup`")

// Config returns the setup config.
func (h *Host) Config() (Config, error) {
	var c Config
	if err := h.readJSON(fileConfig, &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, ErrNotSetUp
		}
		return c, err
	}
	return c, nil
}

// Pinned returns the pinned passkeys (zero value when none).
func (h *Host) Pinned() Pinned {
	var p Pinned
	_ = h.readJSON(filePinned, &p)
	return p
}

// HasBlob reports whether a passphrase blob is stored.
func (h *Host) HasBlob() bool {
	_, err := os.Stat(h.path(fileBlob))
	return err == nil
}

// Setup writes the config and the pinned set and makes the host key. The
// caller has had the user compare the passkeys' fingerprints. It does not
// store a blob: that comes with the first passkey-approved request.
func (h *Host) Setup(c Config, p Pinned) error {
	if c.Host == "" || c.FPR == "" || len(c.Keygrips) == 0 {
		return errors.New("setup needs a host name, a key and its signing keygrips")
	}
	if p.RPID == "" || len(p.Passkeys) == 0 {
		return errors.New("setup needs at least one passkey to trust")
	}
	for i := range c.Keygrips {
		c.Keygrips[i] = strings.ToUpper(c.Keygrips[i])
	}
	if _, err := h.hostKey(); err != nil {
		return err
	}
	if err := h.writeJSON(fileConfig, c); err != nil {
		return err
	}
	return h.writeJSON(filePinned, p)
}

type hostKeyFile struct {
	Seed string `json:"seed"`
}

func (h *Host) hostKey() (ed25519.PrivateKey, error) {
	var f hostKeyFile
	if err := h.readJSON(fileHostKey, &f); err == nil {
		if seed, err := passkey.UnB64(f.Seed); err == nil && len(seed) == ed25519.SeedSize {
			return ed25519.NewKeyFromSeed(seed), nil
		}
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := h.writeJSON(fileHostKey, hostKeyFile{Seed: passkey.B64(k.Seed())}); err != nil {
		return nil, err
	}
	return k, nil
}

// HostKeyPublic is the host key's public half (PKIX DER, base64url), and
// HostFingerprint its short name for the user to compare in the app.
func (h *Host) HostKeyPublic() (string, error) {
	k, err := h.hostKey()
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		return "", err
	}
	return passkey.B64(der), nil
}

func (h *Host) HostFingerprint() (string, error) {
	p, err := h.HostKeyPublic()
	if err != nil {
		return "", err
	}
	return passkey.Fingerprint(p), nil
}

// pending is a request waiting for its answer. Its private key is the
// one-use half of the ECDH pair; the file is removed at first use.
type pending struct {
	Request []byte    `json:"request"`
	Curve   string    `json:"curve"`
	Priv    string    `json:"priv"`
	Exp     time.Time `json:"exp"`
	Setup   bool      `json:"setup,omitempty"`
}

// NewRequest makes a request for the passphrase (or, with setup, for a first
// blob) and remembers it. It returns the request object's bytes, ready to
// post to the hub (POST /api/approvals {action:"gpg-unlock", request}). The
// browser's challenge is passkey.ApprovalChallenge of these bytes. For the
// browser's sake the pub is the one-use public key and curve names it.
func (h *Host) NewRequest(rpID string, setup bool) (id string, request []byte, err error) {
	c, err := h.Config()
	if err != nil {
		return "", nil, err
	}
	// The browser may not support X25519 on every platform; the host accepts
	// either, and issues P-256 (WebCrypto's baseline) unless told otherwise.
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	idb := make([]byte, 8)
	nonce := make([]byte, 16)
	_, _ = rand.Read(idb)
	_, _ = rand.Read(nonce)
	id = "gu_" + fmt.Sprintf("%x", idb)
	exp := h.now().Add(RequestTTL)
	obj := map[string]any{
		"v": 1, "action": "gpg-unlock", "id": id, "host": c.Host, "rp": rpID, "keygrips": c.Keygrips,
		"nonce": passkey.B64(nonce), "exp": exp.Unix(), "curve": CurveP256, "pub": passkey.B64(priv.PublicKey().Bytes()),
	}
	if setup {
		obj["setup"] = true
	}
	hk, err := h.hostKey()
	if err != nil {
		return "", nil, err
	}
	hkPub, _ := h.HostKeyPublic()
	obj["host_key"] = hkPub
	unsigned, _ := json.Marshal(obj) // json.Marshal sorts map keys: canonical
	obj["sig"] = passkey.B64(ed25519.Sign(hk, unsigned))
	request, _ = json.Marshal(obj)
	if err := os.MkdirAll(h.path(dirPending), 0o700); err != nil {
		return "", nil, err
	}
	h.prunePending()
	b, _ := json.Marshal(pending{Request: request, Curve: CurveP256, Priv: passkey.B64(priv.Bytes()), Exp: exp, Setup: setup})
	if err := os.WriteFile(h.path(filepath.Join(dirPending, id+".json")), b, 0o600); err != nil {
		return "", nil, err
	}
	return id, request, nil
}

func (h *Host) prunePending() {
	ents, _ := os.ReadDir(h.path(dirPending))
	for _, e := range ents {
		var p pending
		b, err := os.ReadFile(h.path(filepath.Join(dirPending, e.Name())))
		if err != nil || json.Unmarshal(b, &p) != nil || !h.now().Before(p.Exp) {
			_ = os.Remove(h.path(filepath.Join(dirPending, e.Name())))
		}
	}
}

// takePending loads and deletes a request: one request, one try.
func (h *Host) takePending(id string) (pending, error) {
	var p pending
	if !strings.HasPrefix(id, "gu_") || strings.ContainsAny(id, "/\\.") {
		return p, errors.New("no such request")
	}
	path := h.path(filepath.Join(dirPending, id+".json"))
	b, err := os.ReadFile(path)
	if err != nil {
		return p, errors.New("the request is unknown, already used, or gone")
	}
	_ = os.Remove(path)
	if json.Unmarshal(b, &p) != nil {
		return p, errors.New("the request is unreadable")
	}
	if !h.now().Before(p.Exp) {
		return p, errors.New("the request expired")
	}
	return p, nil
}

func verifyEd(pub any, msg, sig []byte) bool {
	k, ok := pub.(ed25519.PublicKey)
	return ok && ed25519.Verify(k, msg, sig)
}
