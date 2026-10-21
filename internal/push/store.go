package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// subsFile holds the subscriptions in the state dir.
const subsFile = "push-subscriptions.json"

// MaxSubscriptions bounds the store; each phone/browser profile is one.
const MaxSubscriptions = 64

// Keys are a subscription's client keys, base64url as browsers send them.
type Keys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// Subscription is one browser's PushSubscription plus who registered it.
type Subscription struct {
	Endpoint  string    `json:"endpoint"`
	Keys      Keys      `json:"keys"`
	Login     string    `json:"login,omitempty"` // Tailscale-User-Login of the subscriber
	UserAgent string    `json:"user_agent,omitempty"`
	Created   time.Time `json:"created"`
}

// Validate checks the client keys decode to a P-256 point and a 16-byte
// auth secret (the endpoint is checked separately by ValidateEndpoint).
func (s *Subscription) Validate() error {
	p, err := decodeB64(s.Keys.P256DH)
	if err != nil || len(p) != 65 || p[0] != 4 {
		return errors.New("keys.p256dh must be an uncompressed P-256 point")
	}
	a, err := decodeB64(s.Keys.Auth)
	if err != nil || len(a) != 16 {
		return errors.New("keys.auth must be 16 bytes")
	}
	return nil
}

// ErrStoreFull is returned when MaxSubscriptions is reached.
var ErrStoreFull = errors.New("too many push subscriptions")

// Store persists subscriptions, keyed by endpoint, as a JSON file.
type Store struct {
	path string

	mu   sync.Mutex
	subs map[string]Subscription
}

// OpenStore loads the store at dir (a missing file is an empty store).
func OpenStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, subsFile), subs: map[string]Subscription{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Subscription
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	for _, sub := range list {
		if sub.Endpoint != "" {
			s.subs[sub.Endpoint] = sub
		}
	}
	return s, nil
}

// Put adds or replaces the subscription for sub.Endpoint.
func (s *Store) Put(sub Subscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub.Endpoint]; !ok && len(s.subs) >= MaxSubscriptions {
		return ErrStoreFull
	}
	prev, had := s.subs[sub.Endpoint]
	s.subs[sub.Endpoint] = sub
	if err := s.saveLocked(); err != nil {
		if had {
			s.subs[sub.Endpoint] = prev
		} else {
			delete(s.subs, sub.Endpoint)
		}
		return err
	}
	return nil
}

// Get returns the subscription for endpoint.
func (s *Store) Get(endpoint string) (Subscription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[endpoint]
	return sub, ok
}

// Delete removes endpoint, reporting whether it was present.
func (s *Store) Delete(endpoint string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[endpoint]
	if !ok {
		return false, nil
	}
	delete(s.subs, endpoint)
	if err := s.saveLocked(); err != nil {
		s.subs[endpoint] = sub
		return false, err
	}
	return true, nil
}

// List returns all subscriptions, oldest first.
func (s *Store) List() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// Len is the number of subscriptions.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

func (s *Store) listLocked() []Subscription {
	out := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	return out
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.listLocked(), "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(b, '\n'))
}

// writeFileAtomic writes data to path with mode 0600 via a temp file in
// the same directory, fsync, and rename, so a crash never leaves a
// truncated file and the secret is never briefly world-readable.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
