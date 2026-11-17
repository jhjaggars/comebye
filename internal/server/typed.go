package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// The typed record is the hub's own memory of text it typed into an agent's
// pane that the user did not write: a hub notice, a relayed answer, a nudge,
// or another agent's prompt. A user message in a transcript looks the same
// whoever typed it, so the app's origin labels come from this record (the
// message is matched to it by a hash of its text), never from what the
// text starts with. A message with no record is the user's.

// Origins of a typed message. "user" is also what an unmatched message gets.
const (
	originUser  = "user"
	originHub   = "hub"
	originRelay = "relay" // the user's answer to a to-do, typed by the hub
	originAgent = "agent"
	originApp   = "app" // the user's own text, typed by the hub for them (a queued prompt)
)

const (
	typedKeep = 14 * 24 * time.Hour
	typedMax  = 5000
	// typedSlack is how far before the record's time a transcript message
	// may be stamped (clock skew between the hub and the harness).
	typedSlack = 2 * time.Minute
)

type typedRec struct {
	Origin string    `json:"origin"`
	Name   string    `json:"name,omitempty"` // an agent's name as given
	To     string    `json:"to"`             // the agent the text went to
	Hash   string    `json:"hash"`
	At     time.Time `json:"at"`
}

type typedStore struct {
	mu   sync.Mutex
	path string
	recs []typedRec
	now  func() time.Time
}

func typedPath(dir string) string { return filepath.Join(dir, "typed.json") }

func openTyped(path string) *typedStore {
	s := &typedStore{path: path, now: time.Now}
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("typed record: %v (starting empty)", err)
		}
		return s
	}
	var f struct {
		Typed []typedRec `json:"typed"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("typed record: %v (starting empty)", err)
		return s
	}
	s.recs = f.Typed
	s.pruneLocked()
	return s
}

var typedTagRE = regexp.MustCompile(` \[notice [0-9a-f]{6}\]\s*$`)

// typedHash is the hash a text is matched by: the hub's id tag dropped, the
// text cleaned the way herdr cleans it, every run of white space one space.
func typedHash(text string) string {
	t := typedTagRE.ReplaceAllString(herdr.CleanPromptText(text), "")
	t = strings.Join(strings.Fields(t), " ")
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:12])
}

func (s *typedStore) pruneLocked() {
	cut := s.now().Add(-typedKeep)
	keep := s.recs[:0]
	for _, r := range s.recs {
		if !r.At.Before(cut) {
			keep = append(keep, r)
		}
	}
	s.recs = keep
	if len(s.recs) > typedMax {
		s.recs = s.recs[len(s.recs)-typedMax:]
	}
}

func (s *typedStore) saveLocked() {
	if s.path == "" {
		return
	}
	b, _ := json.Marshal(struct {
		Typed []typedRec `json:"typed"`
	}{s.recs})
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("typed record: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("typed record: %v", err)
	}
}

// record notes that the hub typed text into agent to, as origin (name is the
// sending agent's, for origin agent). A nil store records nothing.
func (s *typedStore) record(origin, name, to, text string) {
	if s == nil || to == "" || strings.TrimSpace(text) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, typedRec{Origin: origin, Name: name, To: to, Hash: typedHash(text), At: s.now().UTC()})
	s.pruneLocked()
	s.saveLocked()
}

// match finds the record of a transcript message typed into agent to: the
// latest one with the same text hash that was not sent after the message.
func (s *typedStore) match(to, text, msgTime string) (typedRec, bool) {
	if s == nil || to == "" {
		return typedRec{}, false
	}
	h := typedHash(text)
	var mt time.Time
	if msgTime != "" {
		mt, _ = time.Parse(time.RFC3339Nano, msgTime)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.recs) - 1; i >= 0; i-- {
		r := s.recs[i]
		if r.To != to || r.Hash != h {
			continue
		}
		if !mt.IsZero() && mt.Add(typedSlack).Before(r.At) {
			continue // the message is older than this record
		}
		return r, true
	}
	return typedRec{}, false
}

// markOrigins sets Origin (and OriginName) on each user message of t.
func (s *typedStore) markOrigins(to string, t *transcript.Transcript) {
	for i := range t.Messages {
		m := &t.Messages[i]
		if m.Kind != "user" {
			continue
		}
		m.Origin = originUser
		if r, ok := s.match(to, m.Text, m.Time); ok {
			m.Origin = r.Origin
			m.OriginName = r.Name
		}
	}
}

type originKey struct{}

type originVal struct{ origin, name string }

// withOrigin makes promptAgent record its text as origin (for a sender that
// is not the hub's own words: a queued agent or user prompt).
func withOrigin(ctx context.Context, origin, name string) context.Context {
	return context.WithValue(ctx, originKey{}, originVal{origin, name})
}

// recordTyped notes text about to be typed into a on this host. The hub's
// own text is origin hub (relay when it is a relayed answer) unless the
// context says another.
func (s *Server) recordTyped(ctx context.Context, a herdr.AgentInfo, text string) {
	if s.typed == nil {
		return
	}
	ov, _ := ctx.Value(originKey{}).(originVal)
	if ov.origin == "" {
		ov.origin = originHub
		if strings.HasPrefix(strings.TrimSpace(text), "Comebye (hub relay)") {
			ov.origin = originRelay
		}
	}
	s.typed.record(ov.origin, ov.name, a.ID, text)
}
