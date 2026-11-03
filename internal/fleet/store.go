package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// StoreFile is the dynamic peers' file in the state directory.
const StoreFile = "peers.json"

// Record is one dynamic peer as stored on disk and listed by the API. The
// provenance fields (AddedBy, AddedAt, Tag) exist so tag-based auth and
// self-registration can fill them in later without a format change.
type Record struct {
	Name string   `json:"name"`
	URLs []string `json:"urls"`
	// AddedBy is who added it: a tailnet login, or a node for a later
	// self-registration.
	AddedBy string    `json:"added_by,omitempty"`
	AddedAt time.Time `json:"added_at"`
	// Tag is an optional tailnet tag (tag:worker) the peer is expected to
	// carry. Recorded only for now; nothing enforces it yet.
	Tag string `json:"tag,omitempty"`
}

// Peer converts a stored record to a Peer (the URLs were validated when
// it was added; a hand-edited file is re-validated by Load).
func (r Record) Peer(opt AddrOptions) (Peer, error) {
	addrs, err := ParseAddrs(r.URLs, opt)
	if err != nil {
		return Peer{}, err
	}
	return Peer{Name: r.Name, URLs: addrs}, nil
}

type storeFile struct {
	Peers []Record `json:"peers"`
}

// Store persists the dynamic peers. It is safe for concurrent use.
type Store struct {
	path string
	mu   sync.Mutex
	recs map[string]Record
}

// OpenStore reads dir's peers.json (absent is empty). Malformed content
// is an error: starting with silently dropped peers would hide them.
func OpenStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, StoreFile), recs: map[string]Record{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	for _, r := range f.Peers {
		if !ValidName(r.Name) {
			return nil, fmt.Errorf("%s: bad peer name %q", s.path, r.Name)
		}
		s.recs[r.Name] = r
	}
	return s, nil
}

// List returns the records sorted by name.
func (s *Store) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sorted()
}

func (s *Store) sorted() []Record {
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the named record.
func (s *Store) Get(name string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[name]
	return r, ok
}

// ErrExists and ErrNotFound are Add's and Remove's refusals.
var (
	ErrExists   = errors.New("a dynamic peer with this name already exists")
	ErrNotFound = errors.New("no such dynamic peer")
)

// Add stores r (rejecting an existing name) and writes the file; on a
// write error nothing changes.
func (s *Store) Add(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[r.Name]; ok {
		return ErrExists
	}
	s.recs[r.Name] = r
	if err := s.save(); err != nil {
		delete(s.recs, r.Name)
		return err
	}
	return nil
}

// Remove deletes the named record and writes the file.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.recs[name]
	if !ok {
		return ErrNotFound
	}
	delete(s.recs, name)
	if err := s.save(); err != nil {
		s.recs[name] = old
		return err
	}
	return nil
}

// save writes the file atomically (temp file, fsync, rename) with mode 0600.
func (s *Store) save() error {
	b, err := json.MarshalIndent(storeFile{Peers: s.sorted()}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".peers-*.tmp")
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
