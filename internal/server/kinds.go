package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// kindMemory is how long a kind that worked stays available when it can no
// longer be found (a PATH that is wrong right now, an install in a place
// nobody searches).
const kindMemory = 90 * 24 * time.Hour

// kindRefresh bounds how often a kind's entry is rewritten while it keeps
// being seen.
const kindRefresh = time.Hour

// kindStore keeps kinds.json: agent kind -> when it last worked on this
// host (found installed, seen running or launched).
type kindStore struct {
	path string
	mu   sync.Mutex
	data map[string]time.Time
}

func openKinds(path string) *kindStore {
	s := &kindStore{path: path, data: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("kinds: %v (starting with none)", err)
		}
		return s
	}
	var m map[string]struct {
		LastOK time.Time `json:"last_ok"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("kinds: %v (starting with none)", err)
		return s
	}
	for k, v := range m {
		s.data[k] = v.LastOK
	}
	return s
}

// remember records that kind worked now.
func (s *kindStore) remember(kind string, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.data[kind]; ok && now.Sub(last) < kindRefresh {
		return
	}
	s.data[kind] = now
	s.save()
}

// forget drops kind (it was remembered but could not be started).
func (s *kindStore) forget(kind string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[kind]; ok {
		delete(s.data, kind)
		s.save()
	}
}

// recalled reports whether kind worked within kindMemory.
func (s *kindStore) recalled(kind string, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.data[kind]
	return ok && now.Sub(last) < kindMemory
}

// save writes the file; s.mu is held.
func (s *kindStore) save() {
	out := map[string]map[string]time.Time{}
	for k, t := range s.data {
		out[k] = map[string]time.Time{"last_ok": t}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err == nil {
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil {
		log.Printf("kinds: %v", err)
	}
}

func kindsPath(stateDir string) string { return filepath.Join(stateDir, "kinds.json") }
