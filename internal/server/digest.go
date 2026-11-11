package server

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

// Digest history: what each digest sent to the coordinator contained, kept
// for the app's Briefings screen.

const maxDigestHistory = 100

// Digest is one sent batch of routine lines.
type Digest struct {
	At    time.Time `json:"at"`
	To    string    `json:"to,omitempty"`
	Lines []outLine `json:"lines"`
}

type digestStore struct {
	mu   sync.Mutex
	path string
	list []Digest // newest first
	read bool
}

func (d *digestStore) load() {
	if d.read {
		return
	}
	d.read = true
	if b, err := os.ReadFile(d.path); err == nil {
		_ = json.Unmarshal(b, &d.list)
	}
}

func (d *digestStore) add(x Digest) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.load()
	d.list = append([]Digest{x}, d.list...)
	if len(d.list) > maxDigestHistory {
		d.list = d.list[:maxDigestHistory]
	}
	if d.path != "" {
		b, _ := json.Marshal(d.list)
		_ = os.WriteFile(d.path, b, 0o600)
	}
}

func (d *digestStore) all() []Digest {
	if d == nil {
		return []Digest{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.load()
	return append([]Digest{}, d.list...)
}

// recordDigest keeps what was sent.
func (s *Server) recordDigest(to string, lines []outLine) {
	s.digests.add(Digest{At: time.Now().UTC(), To: to, Lines: lines})
}

// handleGetDigests answers {digests: [...]}, newest first.
func (s *Server) handleGetDigests(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	writeJSON(w, map[string]any{"digests": s.digests.all()})
}
