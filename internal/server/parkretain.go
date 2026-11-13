package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// Retention. A parked agent keeps a worktree, a branch and a session, which is
// cheap but not free. After -park-retention (14 days) without a resume the hub
// proposes removing it: it marks the agent "ready to remove", and the user
// confirms in the normal review (nothing is deleted by retention alone). A
// pinned record is never proposed, and a record is proposed once: if the user
// unmarks it in the review, it stays.

const (
	// DefaultParkRetention is how long a parked agent is kept before removal is proposed.
	DefaultParkRetention = 14 * 24 * time.Hour
	parkRetentionTick    = time.Hour
)

// parkProposed remembers the records already proposed for removal
// (parked-proposed.json on the hub).
type parkProposed struct {
	mu   sync.Mutex
	path string
	ids  map[string]time.Time
}

func (s *Server) proposed() *parkProposed {
	if s.cfg.StateDir == "" {
		return nil
	}
	s.propOnce.Do(func() {
		p := &parkProposed{path: filepath.Join(s.cfg.StateDir, "parked-proposed.json"), ids: map[string]time.Time{}}
		if b, err := os.ReadFile(p.path); err == nil {
			_ = json.Unmarshal(b, &p.ids)
		}
		s.prop = p
	})
	return s.prop
}

func (p *parkProposed) has(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.ids[id]
	return ok
}

func (p *parkProposed) add(id string, at time.Time, live map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids[id] = at
	for k, t := range p.ids { // forget what is long gone
		if !live[k] && at.Sub(t) > 90*24*time.Hour {
			delete(p.ids, k)
		}
	}
	if b, err := json.Marshal(p.ids); err == nil {
		tmp := p.path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, p.path)
		}
	}
}

// proposeParkRemoval marks a parked agent ready to remove, so the removal review
// lists it (the worktree and branch are checked there, and the user confirms).
// A ledger worker's own entry is marked when there is one.
func (s *Server) proposeParkRemoval(pr parkedRecord, reason, by string) error {
	if s.ledger == nil {
		return errors.New("this hub keeps no ledger, so there is no removal review to propose it in")
	}
	r := pr.Rec
	m := ledger.Mark{AgentID: pr.Host + "/" + r.ID, Host: pr.Host, PaneID: r.ID, Name: r.Name, Worktree: r.Worktree, CWD: r.CWD, By: by, Reason: clip(reason, 500)}
	for _, e := range s.ledger.Entries() {
		if e.AgentID == pr.Host+"/"+r.PaneID && e.Worktree == r.Worktree && e.Mark == nil {
			m.AgentID, m.PaneID, m.WorkspaceID = e.AgentID, e.PaneID, e.WorkspaceID
			break
		}
	}
	_, _, err := s.applyMark(m, nil, false)
	return err
}

// RunParkRetention follows a clock until ctx is done.
func (s *Server) RunParkRetention(ctx context.Context) {
	if s.cfg.ParkRetention <= 0 || s.ledger == nil {
		return
	}
	tick := time.NewTicker(parkRetentionTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.parkRetentionStep(time.Now())
		}
	}
}

// parkRetentionStep proposes the removal of every parked agent past retention
// that is not pinned and was not proposed before. It returns how many.
func (s *Server) parkRetentionStep(now time.Time) int {
	p := s.proposed()
	if s.cfg.ParkRetention <= 0 || s.ledger == nil || p == nil {
		return 0
	}
	recs := s.parkSnapshot()
	live := map[string]bool{}
	for _, pr := range recs {
		live[pr.Rec.ID] = true
	}
	n := 0
	for _, pr := range recs {
		r := pr.Rec
		if !r.Parked() || r.Pinned || now.Sub(r.ParkedAt) < s.cfg.ParkRetention || p.has(r.ID) {
			continue
		}
		days := int(now.Sub(r.ParkedAt).Hours() / 24)
		why := "Parked " + itoa(days) + " days ago and not resumed (retention)"
		if err := s.proposeParkRemoval(pr, why, "hub (park retention)"); err != nil {
			log.Printf("park retention: %s: %v", r.Name, err)
			continue
		}
		p.add(r.ID, now, live)
		n++
	}
	return n
}

// parkMarkBody is the body of POST /api/park/mark.
type parkMarkBody struct {
	Host string `json:"host"`
	ID   string `json:"id"`
}

// handleParkMark is POST /api/park/mark: the user sends a parked agent to the
// removal review now.
func (s *Server) handleParkMark(w http.ResponseWriter, r *http.Request) {
	var b parkMarkBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&b); err != nil || b.ID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	var hit *parkedRecord
	for _, pr := range s.parkSnapshot() {
		if pr.Rec.ID == b.ID && pr.Rec.Parked() && (b.Host == "" || pr.Host == b.Host) {
			pr := pr
			hit = &pr
		}
	}
	if hit == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_such_record", "message": "no parked agent has that id"})
		return
	}
	if err := s.proposeParkRemoval(*hit, "You asked to remove this parked agent", s.by(r, "")); err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "cannot_propose", "message": err.Error()})
		return
	}
	if p := s.proposed(); p != nil {
		p.add(hit.Rec.ID, time.Now(), map[string]bool{})
	}
	writeJSON(w, map[string]any{"marked": true})
}
