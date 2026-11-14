package passkey

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Passkey is one registered passkey. Only public material is kept: the
// credential id, the public key and what the registration reported.
type Passkey struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	PublicKey      string     `json:"public_key"` // PKIX DER, base64url
	RPIDHash       string     `json:"rp_id_hash"` // hex
	Counter        uint32     `json:"counter,omitempty"`
	BackupEligible bool       `json:"backup_eligible,omitempty"`
	BackedUp       bool       `json:"backed_up,omitempty"`
	AAGUID         string     `json:"aaguid,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
}

type fileData struct {
	Version int `json:"version"`
	// RPID is recorded with the first passkey; the hub refuses to register
	// on any other host name. It is kept after the last passkey is removed
	// only until the next one is made: with none left, any name may start.
	RPID    string    `json:"rp_id,omitempty"`
	Passkey []Passkey `json:"passkeys"`
}

// Store is passkeys.json in the hub's state directory.
type Store struct {
	mu   sync.Mutex
	path string
	data fileData
	now  func() time.Time
}

var (
	ErrNotFound  = errors.New("no such passkey")
	ErrDuplicate = errors.New("that passkey is already registered")
	ErrRPMoved   = errors.New("passkeys are registered on another host name")
)

// MaxPasskeys bounds the list.
const MaxPasskeys = 16

// Open reads path ("" keeps the store in memory only).
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now, data: fileData{Version: 1}}
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, err
		default:
			if err := json.Unmarshal(b, &s.data); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		}
	}
	return s, nil
}

// Path is the file name inside a state directory.
func Path(dir string) string { return filepath.Join(dir, "passkeys.json") }

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".passkeys-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// RPID is the recorded RP ID ("" before the first passkey).
func (s *Store) RPID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rp()
}

func (s *Store) rp() string {
	if len(s.data.Passkey) == 0 {
		return ""
	}
	return s.data.RPID
}

// List returns the passkeys, oldest first.
func (s *Store) List() []Passkey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Passkey{}, s.data.Passkey...)
}

// Get returns one passkey by credential id.
func (s *Store) Get(id string) (Passkey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.data.Passkey {
		if p.ID == id {
			return p, true
		}
	}
	return Passkey{}, false
}

// CleanName trims a user-given name to one short line.
func CleanName(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	if r := []rune(n); len(r) > 60 {
		n = string(r[:60])
	}
	return n
}

// Add stores a verified registration under rpID. The first passkey fixes the
// RP ID; later ones must use the same.
func (s *Store) Add(rpID string, reg Registration, name string) (Passkey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.rp(); cur != "" && !strings.EqualFold(cur, rpID) {
		return Passkey{}, ErrRPMoved
	}
	for _, p := range s.data.Passkey {
		if p.ID == reg.ID {
			return Passkey{}, ErrDuplicate
		}
	}
	if len(s.data.Passkey) >= MaxPasskeys {
		return Passkey{}, fmt.Errorf("at most %d passkeys: remove one first", MaxPasskeys)
	}
	name = CleanName(name)
	if name == "" {
		name = fmt.Sprintf("Passkey %d", len(s.data.Passkey)+1)
	}
	p := Passkey{ID: reg.ID, Name: name, PublicKey: reg.PublicKey, RPIDHash: reg.RPIDHash, Counter: reg.Counter,
		BackupEligible: reg.BackupEligible, BackedUp: reg.BackedUp, AAGUID: reg.AAGUID, CreatedAt: s.now().UTC()}
	prev, prevRP := s.data.Passkey, s.data.RPID
	s.data.Passkey = append(append([]Passkey{}, prev...), p)
	s.data.RPID = strings.ToLower(rpID)
	if err := s.save(); err != nil {
		s.data.Passkey, s.data.RPID = prev, prevRP
		return Passkey{}, err
	}
	return p, nil
}

// Rename changes a passkey's name.
func (s *Store) Rename(id, name string) (Passkey, error) {
	name = CleanName(name)
	if name == "" {
		return Passkey{}, errors.New("a name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Passkey {
		if s.data.Passkey[i].ID == id {
			old := s.data.Passkey[i].Name
			s.data.Passkey[i].Name = name
			if err := s.save(); err != nil {
				s.data.Passkey[i].Name = old
				return Passkey{}, err
			}
			return s.data.Passkey[i], nil
		}
	}
	return Passkey{}, ErrNotFound
}

// Remove deletes a passkey.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.data.Passkey {
		if p.ID == id {
			prev := s.data.Passkey
			s.data.Passkey = append(append([]Passkey{}, prev[:i]...), prev[i+1:]...)
			if err := s.save(); err != nil {
				s.data.Passkey = prev
				return err
			}
			return nil
		}
	}
	return ErrNotFound
}

// Used records a successful assertion: the new counter and the time.
func (s *Store) Used(id string, counter uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Passkey {
		if s.data.Passkey[i].ID == id {
			now := s.now().UTC()
			s.data.Passkey[i].Counter, s.data.Passkey[i].LastUsedAt = counter, &now
			_ = s.save()
			return
		}
	}
}
