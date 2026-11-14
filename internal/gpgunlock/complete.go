package gpgunlock

import (
	"errors"
	"os"
	"time"

	"github.com/jhjaggars/comebye/internal/passkey"
)

// Answer is what the app returns for a request: the passkey's assertion, K
// sealed to the request's key, and (setup only) the blob it made.
type Answer struct {
	CredentialID      string `json:"credential_id"`
	ClientDataJSON    string `json:"client_data_json"` // base64url
	AuthenticatorData string `json:"authenticator_data"`
	Signature         string `json:"signature"`
	Sealed            string `json:"sealed"`
	Blob              *Blob  `json:"blob,omitempty"`
}

// Complete checks an answer and unlocks. The checks, in order: the request
// exists, is unused and unexpired (and is deleted now, so one request gives
// one try); the passkey is in the pinned set and the assertion verifies for
// the host's own recomputed challenge; K is opened; the passphrase is
// decrypted and preset; a test signature proves it. A wrong passphrase
// stores nothing and leaves the key locked.
func (h *Host) Complete(id string, a Answer) (State, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cfg, err := h.Config()
	if err != nil {
		return State{}, err
	}
	p, err := h.takePending(id)
	if err != nil {
		return State{}, err
	}
	pin := h.Pinned()
	var key *PinnedKey
	for i := range pin.Passkeys {
		if pin.Passkeys[i].ID == a.CredentialID {
			key = &pin.Passkeys[i]
		}
	}
	if key == nil {
		return State{}, errors.New("that passkey is not one this host trusts: run `comebye gpg setup` to pin it")
	}
	cd, e1 := passkey.UnB64(a.ClientDataJSON)
	ad, e2 := passkey.UnB64(a.AuthenticatorData)
	sig, e3 := passkey.UnB64(a.Signature)
	if e1 != nil || e2 != nil || e3 != nil {
		return State{}, errors.New("the answer is malformed")
	}
	challenge := passkey.ApprovalChallenge(p.Request)
	counter, err := passkey.VerifyAssertion(passkey.Options{RPID: pin.RPID}, key.PublicKey, key.Counter, challenge, cd, ad, sig)
	if err != nil {
		return State{}, err
	}
	if counter != key.Counter {
		key.Counter = counter
		_ = h.writeJSON(filePinned, pin)
	}
	priv, _ := passkey.UnB64(p.Priv)
	k, err := openSealed(p.Curve, priv, challenge, a.Sealed)
	wipe(priv)
	if err != nil {
		return State{}, err
	}
	defer wipe(k)

	blob := a.Blob
	if p.Setup {
		if blob == nil {
			return State{}, errors.New("setup needs the passphrase blob")
		}
		if blob.Host != cfg.Host || !sameGrips(blob.Keygrips, cfg.Keygrips) {
			return State{}, errors.New("the blob was made for another host or key")
		}
	} else {
		var stored Blob
		if err := h.readJSON(fileBlob, &stored); err != nil {
			return State{}, errors.New("no passphrase is stored here: set up again")
		}
		blob = &stored
	}
	pass, err := blob.Decrypt(k)
	if err != nil {
		return State{}, err
	}
	defer wipe(pass)

	for _, g := range cfg.Keygrips {
		if err := h.GPG.Preset(g, pass); err != nil {
			_ = h.GPG.ForgetAll(cfg.Keygrips)
			return State{}, err
		}
	}
	if err := h.GPG.TestSign(cfg.FPR); err != nil {
		_ = h.GPG.ForgetAll(cfg.Keygrips)
		return State{}, ErrBadPassphrase
	}
	if p.Setup {
		if err := h.writeJSON(fileBlob, blob); err != nil {
			_ = h.GPG.ForgetAll(cfg.Keygrips)
			return State{}, err
		}
	}
	return h.armLocked(cfg.Keygrips)
}

func sameGrips(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (h *Host) armLocked(grips []string) (State, error) {
	st := State{ExpiresAt: h.now().Add(h.lifetime()), Keygrips: grips}
	return st, h.writeJSON(fileState, st)
}

// Unlocked returns the unlock in force, if any (the state file's, not the
// agent's: use Status for both).
func (h *Host) Unlocked() (State, bool) {
	var st State
	if h.readJSON(fileState, &st) != nil || !h.now().Before(st.ExpiresAt) {
		return st, false
	}
	return st, true
}

// Sweep ends an unlock whose time has come: it forgets the keys and removes
// the state file. Call it at start (so a restart cannot extend an unlock),
// at the expiry time and on demand. It reports whether it ended one.
func (h *Host) Sweep() (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var st State
	if err := h.readJSON(fileState, &st); err != nil {
		return false, nil
	}
	if h.now().Before(st.ExpiresAt) {
		return false, nil
	}
	err := h.GPG.ForgetAll(st.Keygrips)
	_ = os.Remove(h.path(fileState))
	return true, err
}

// NextExpiry is when the unlock in force ends.
func (h *Host) NextExpiry() (time.Time, bool) {
	var st State
	if h.readJSON(fileState, &st) != nil {
		return time.Time{}, false
	}
	return st.ExpiresAt, true
}

// Lock forgets the keys now.
func (h *Host) Lock() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	cfg, err := h.Config()
	if err != nil {
		return err
	}
	err = h.GPG.ForgetAll(cfg.Keygrips)
	_ = os.Remove(h.path(fileState))
	return err
}

// Reset forgets the keys and deletes the blob, the pinned set, the host key
// and the config: the lost-passkey recovery. The passphrase still works in
// pinentry; set up again to use a passkey.
func (h *Host) Reset() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var err error
	if cfg, e := h.Config(); e == nil {
		err = h.GPG.ForgetAll(cfg.Keygrips)
	}
	for _, n := range []string{fileConfig, filePinned, fileBlob, fileHostKey, fileState} {
		_ = os.Remove(h.path(n))
	}
	_ = os.RemoveAll(h.path(dirPending))
	return err
}

// Status is the host's state for `comebye gpg status`.
type Status struct {
	SetUp      bool       `json:"set_up"`
	Host       string     `json:"host,omitempty"`
	Key        string     `json:"key,omitempty"`
	Passkeys   int        `json:"passkeys"`
	HasBlob    bool       `json:"has_blob"`
	Locked     bool       `json:"locked"`
	UnlockedTo *time.Time `json:"unlocked_until,omitempty"`
	Keys       []KeyInfo  `json:"keys,omitempty"`
}

func (h *Host) Status() (Status, error) {
	var s Status
	cfg, err := h.Config()
	if err != nil {
		return s, err
	}
	s.SetUp, s.Host, s.Key = true, cfg.Host, cfg.FPR
	s.Passkeys, s.HasBlob = len(h.Pinned().Passkeys), h.HasBlob()
	if s.Locked, err = h.GPG.Locked(cfg.Keygrips); err != nil {
		return s, err
	}
	if st, ok := h.Unlocked(); ok && !s.Locked {
		s.UnlockedTo = &st.ExpiresAt
	}
	info, _ := h.GPG.KeyInfo()
	for _, k := range info {
		for _, g := range cfg.Keygrips {
			if k.Grip == g {
				s.Keys = append(s.Keys, k)
			}
		}
	}
	return s, nil
}
