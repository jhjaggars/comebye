package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The event store is the hub's log of what it handled itself: a worker
// landed, an agent was parked or removed, a handoff was cleaned up, an
// agent was blocked at a dialog and cleared, a queued prompt was held for
// typing. These notices need no judgement, so they are no longer typed into
// the coordinator's pane; they are events, read with `comebye log`, shown
// on the board in the "Hub did" lane and in the coordinator's fresh-start
// digest. Events survive a hub restart and are kept for a fortnight.

const (
	eventKeep = 14 * 24 * time.Hour
	eventMax  = 2000
	// hubDidWindow is how far back the board lane and the digest look.
	hubDidWindow = 24 * time.Hour
	// hubDidLines is how many of the newest events the board lane lists.
	hubDidLines = 5
	// eventTextMax bounds one event's text.
	eventTextMax = 400
)

// Event kinds. A kind is part of the CLI's --kind filter.
const (
	evLanded    = "landed"     // work landed (CI not failed)
	evPark      = "park"       // an idle agent parked automatically
	evAutoRm    = "autoremove" // an agent removed automatically
	evCleanup   = "cleanup"    // a landed handoff marked ready to remove
	evBlocked   = "blocked"    // an agent is waiting on the user at a dialog
	evUnblocked = "unblocked"  // that dialog cleared
	evHeld      = "held"       // a queued prompt still held for typing
)

// Event is one thing the hub handled.
type Event struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Key makes an event once-only: a second event with the same key is
	// dropped.
	Key  string `json:"key,omitempty"`
	Kind string `json:"kind"`
	// Subject is the handoff, agent or prompt the event is about.
	Subject string `json:"subject,omitempty"`
	// Who is the agent's name, when there is one.
	Who string `json:"who,omitempty"`
	// Text is one line the hub wrote from safe fields only.
	Text string `json:"text"`
}

type eventStore struct {
	mu   sync.Mutex
	path string
	list []Event // oldest first
	seq  int
	now  func() time.Time
}

func eventsPath(dir string) string { return filepath.Join(dir, "events.json") }

func openEvents(path string) *eventStore {
	e := &eventStore{path: path, now: time.Now}
	if path == "" {
		return e
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("events: %v (starting empty)", err)
		}
		return e
	}
	var f struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("events: %v (starting empty)", err)
		return e
	}
	e.list = f.Events
	e.seq = len(e.list)
	e.pruneLocked()
	return e
}

func (e *eventStore) pruneLocked() {
	cut := e.now().Add(-eventKeep)
	i := 0
	for i < len(e.list) && e.list[i].At.Before(cut) {
		i++
	}
	e.list = e.list[i:]
	if len(e.list) > eventMax {
		e.list = e.list[len(e.list)-eventMax:]
	}
}

func (e *eventStore) saveLocked() {
	if e.path == "" {
		return
	}
	b, _ := json.Marshal(struct {
		Events []Event `json:"events"`
	}{e.list})
	tmp := e.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("events: %v", err)
		return
	}
	if err := os.Rename(tmp, e.path); err != nil {
		log.Printf("events: %v", err)
	}
}

// add appends an event. It reports false when the event's key was seen.
// A nil store drops everything.
func (e *eventStore) add(ev Event) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ev.Key != "" {
		for _, x := range e.list {
			if x.Key == ev.Key {
				return false
			}
		}
	}
	if ev.At.IsZero() {
		ev.At = e.now().UTC()
	}
	if len(ev.Text) > eventTextMax {
		ev.Text = oneLine(ev.Text, eventTextMax)
	}
	e.seq++
	ev.ID = "ev" + strconv.Itoa(e.seq) + strconv.FormatInt(ev.At.UnixNano()%100000, 36)
	e.list = append(e.list, ev)
	e.pruneLocked()
	e.saveLocked()
	return true
}

// query returns the events at or after since whose kind is in kinds (all
// kinds when empty), newest first, at most limit (0: all).
func (e *eventStore) query(since time.Time, kinds []string, limit int) []Event {
	if e == nil {
		return []Event{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	out := []Event{}
	for i := len(e.list) - 1; i >= 0; i-- {
		ev := e.list[i]
		if ev.At.Before(since) {
			continue
		}
		if len(want) > 0 && !want[ev.Kind] {
			continue
		}
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// logEvent records something the hub handled instead of telling the
// coordinator. key makes it once-only ("" for always).
func (s *Server) logEvent(kind, subject, who, key, text string) {
	if s.events == nil {
		return
	}
	if key != "" {
		key = kind + "/" + key
	}
	s.events.add(Event{Kind: kind, Subject: subject, Who: who, Key: key, Text: text})
}

// queueLine records a routine line. It used to queue a digest message for
// the coordinator; it is an event now (kind, the entry it is about, a
// once-only key, one safe-text line).
func (s *Server) queueLine(kind, entry, key, text string) {
	s.logEvent(kind, entry, "", key, text)
}

// migrateOutbox turns the lines an older hub had queued for a digest into
// events, then removes the outbox file: the digest message is retired.
func (s *Server) migrateOutbox(dir string) {
	path := outboxPath(dir)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f struct {
		Lines []struct {
			Key   string    `json:"key"`
			Kind  string    `json:"kind"`
			Entry string    `json:"entry"`
			Text  string    `json:"text"`
			At    time.Time `json:"at"`
		} `json:"lines"`
	}
	if json.Unmarshal(b, &f) == nil {
		for _, l := range f.Lines {
			s.events.add(Event{At: l.At, Kind: l.Kind, Subject: l.Entry, Key: l.Key, Text: l.Text})
		}
	}
	_ = os.Remove(path)
}

func outboxPath(dir string) string { return filepath.Join(dir, "notice-outbox.json") }

// parseSince reads a CLI duration: Go's ("90m", "24h") or days ("2d").
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return now.Add(-hubDidWindow), nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || n < 0 {
			return time.Time{}, errors.New("bad since")
		}
		return now.Add(-time.Duration(n * float64(24*time.Hour))), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("bad since")
}

// handleLog answers GET /api/log?since=24h&kind=park,landed&limit=N with
// {events: [...]} newest first.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	q := r.URL.Query()
	since, err := parseSince(q.Get("since"), time.Now())
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_since", "message": "since is a duration such as 90m, 24h or 2d, or an RFC 3339 time"})
		return
	}
	var kinds []string
	for _, k := range strings.Split(q.Get("kind"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			kinds = append(kinds, k)
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, map[string]any{"events": s.events.query(since, kinds, limit)})
}
