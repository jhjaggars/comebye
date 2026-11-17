package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The sent record is the hub's memory of what it has told whom, so the
// same notice is never delivered twice: a finish announced to the
// requester and again to the coordinator as a stand-in, or sent again by a
// retry or after a restart. Every sender claims the notice's subject
// before it sends; a claim that is already held means the notice is out.
// A failed send releases its claim, so a retry still goes out. The record
// keeps the target and the send time of each message (the app's origin
// labels read them later) and is pruned by age. It survives a restart.

const (
	// sentKeep is how long a sent message is remembered.
	sentKeep = 14 * 24 * time.Hour
	// sentMax bounds the record whatever the age.
	sentMax = 5000
)

// sentRec is one claimed (and, once At is set from a send, sent) subject.
type sentRec struct {
	Subject string    `json:"subject"`
	To      string    `json:"to"`
	At      time.Time `json:"at"`
}

type sentStore struct {
	mu   sync.Mutex
	path string
	recs map[string]sentRec
	now  func() time.Time
}

func sentPath(dir string) string { return filepath.Join(dir, "sent.json") }

func openSent(path string) *sentStore {
	s := &sentStore{path: path, recs: map[string]sentRec{}, now: time.Now}
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("sent record: %v (starting empty)", err)
		}
		return s
	}
	var f struct {
		Sent []sentRec `json:"sent"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("sent record: %v (starting empty)", err)
		return s
	}
	for _, r := range f.Sent {
		s.recs[r.Subject] = r
	}
	s.pruneLocked()
	return s
}

func (s *sentStore) pruneLocked() {
	cut := s.now().Add(-sentKeep)
	for k, r := range s.recs {
		if r.At.Before(cut) {
			delete(s.recs, k)
		}
	}
	if len(s.recs) > sentMax {
		all := make([]sentRec, 0, len(s.recs))
		for _, r := range s.recs {
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
		for _, r := range all[:len(all)-sentMax] {
			delete(s.recs, r.Subject)
		}
	}
}

func (s *sentStore) saveLocked() {
	if s.path == "" {
		return
	}
	all := make([]sentRec, 0, len(s.recs))
	for _, r := range s.recs {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
	b, _ := json.Marshal(struct {
		Sent []sentRec `json:"sent"`
	}{all})
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("sent record: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("sent record: %v", err)
	}
}

// claim takes subject for a send to target to. It reports false when the
// subject was claimed (or sent) already: the caller must not send. A nil
// store claims everything. The caller releases the claim if the send fails.
func (s *sentStore) claim(subject, to string) bool {
	if s == nil || subject == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[subject]; ok {
		return false
	}
	s.recs[subject] = sentRec{Subject: subject, To: to, At: s.now().UTC()}
	s.pruneLocked()
	s.saveLocked()
	return true
}

// release gives a claim back after a failed send.
func (s *sentStore) release(subject string) {
	if s == nil || subject == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[subject]; ok {
		delete(s.recs, subject)
		s.saveLocked()
	}
}

// confirm stamps the real send time on a claim once the send went through.
func (s *sentStore) confirm(subject string) {
	if s == nil || subject == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[subject]; ok {
		r.At = s.now().UTC()
		s.recs[subject] = r
		s.saveLocked()
	}
}

// get returns the record of subject.
func (s *sentStore) get(subject string) (sentRec, bool) {
	if s == nil {
		return sentRec{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[subject]
	return r, ok
}

// finishSubject names what a handoff notice is about: the entry, the kind
// of news (finished, landed, landing failed, CI state) and the report
// round, but not who is told, so a stand-in and the requester share it.
func finishSubject(e ledger.Entry) string {
	kind := "finished"
	if l := e.LandCheck; l != nil {
		switch {
		case e.Status == ledger.Landed:
			kind = "landed"
			if l.CI != nil {
				kind += "-" + string(l.CI.State)
			}
			if e.AwaitLandReport {
				kind += "-early"
			}
		case l.StateOf() == ledger.LandFailed && e.FinishedAt != nil && !l.At.Before(*e.FinishedAt):
			kind = "landfailed"
		}
	}
	return "handoff/" + e.ID + "/" + kind + "/" + itoa(e.ReportSeq)
}
