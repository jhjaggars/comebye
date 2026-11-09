package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// docPin is the document an agent declared with `comebye show`: the Doc
// panel opens it, at Section when that is set.
type docPin struct {
	Path    string    `json:"path"`
	Section string    `json:"section,omitempty"`
	At      time.Time `json:"at"`
	By      string    `json:"by,omitempty"`
}

// pinStore keeps docpins.json: agent id (host/pane) -> its declared
// document. Only the hub's is read (an agent's declaration goes to the hub).
type pinStore struct {
	path string
	mu   sync.Mutex
	data map[string]docPin
}

func openPins(path string) *pinStore {
	s := &pinStore{path: path, data: map[string]docPin{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("docpins: %v (starting with none)", err)
		}
		return s
	}
	if err := json.Unmarshal(b, &s.data); err != nil || s.data == nil {
		log.Printf("docpins: unreadable (starting with none)")
		s.data = map[string]docPin{}
	}
	return s
}

func (s *pinStore) get(agent string) (docPin, bool) {
	if s == nil {
		return docPin{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data[agent]
	return p, ok
}

// set records agent's document; an empty path clears it.
func (s *pinStore) set(agent string, p docPin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Path == "" {
		delete(s.data, agent)
	} else {
		s.data[agent] = p
	}
	// Forget the oldest entries of long-gone agents.
	for len(s.data) > 500 {
		oldest, at := "", time.Now().Add(time.Hour)
		for k, v := range s.data {
			if v.At.Before(at) {
				oldest, at = k, v.At
			}
		}
		delete(s.data, oldest)
	}
	if s.path == "" {
		return nil
	}
	return writeFileAtomic(s.path, s.data)
}

func pinsPath(stateDir string) string { return filepath.Join(stateDir, "docpins.json") }
