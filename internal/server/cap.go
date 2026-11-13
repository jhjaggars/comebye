package server

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The per-repository worker cap: a host setting (settings.go) for the most
// workers running at once in one repository on that host. A handoff over
// it is queued as a `waiting` entry with WaitKind "cap" and starts, oldest
// first, when a worker in that repository stops running. `comebye release`
// starts one anyway. 0 is no cap.

const (
	waitCap     = "cap"
	capCacheTTL = 30 * time.Second
)

type capCacheEntry struct {
	n  int
	at time.Time
}

// capFor is the cap in force on host: this host's settings, or a peer's
// (read through the hub and kept for capCacheTTL). A peer that does not
// answer keeps its last known cap; one too old to say has none.
func (s *Server) capFor(host string) int {
	if host == "" || host == s.fleet.LocalName() {
		n, _ := s.maxWorkersPerRepo()
		return n
	}
	s.capMu.Lock()
	e, ok := s.capSeen[host]
	s.capMu.Unlock()
	if ok && time.Since(e.at) < capCacheTTL {
		return e.n
	}
	var v settingsView
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := 0
	if err := s.callHost(ctx, host, "settings", nil, nil, &v); err == nil {
		n = v.Effective.MaxWorkersPerRepo
	} else if ok {
		n = e.n
	}
	s.capMu.Lock()
	if s.capSeen == nil {
		s.capSeen = map[string]capCacheEntry{}
	}
	s.capSeen[host] = capCacheEntry{n, time.Now()}
	s.capMu.Unlock()
	return n
}

// runningInLocked counts the workers running in repoName on host, plus queued
// handoffs of the repository being started right now (skip is not one).
// The caller holds coord.mu.
func (s *Server) runningInLocked(host, repoName, skip string) int {
	n := 0
	for _, e := range s.ledger.Entries() {
		switch {
		case e.Host != host || e.RepoName != repoName || e.ID == skip:
		case e.Status == ledger.Running:
			n++
		case e.Status == ledger.Waiting && s.coord.starting["queued/"+e.ID]:
			n++
		}
	}
	return n
}

func capNote(running, limit int, repoName, host string) string {
	return fmt.Sprintf("waiting for a free slot: %d of %d workers are running in %s on %s (the host's limit per repository). It starts when one finishes.", running, limit, repoName, host)
}

// capHoldLocked says whether a waiting handoff whose steps are done must
// keep waiting because its repository is at the host's cap, and records
// the reason on the entry. coord.mu is held.
func (s *Server) capHoldLocked(e ledger.Entry, limit int) bool {
	if limit <= 0 {
		s.setWait(e, "", "")
		return false
	}
	running := s.runningInLocked(e.Host, e.RepoName, e.ID)
	if running < limit {
		s.setWait(e, "", "")
		return false
	}
	s.setWait(e, waitCap, capNote(running, limit, e.RepoName, e.Host))
	return true
}

// setWait records why e is waiting (empty: no cap reason), when that changed.
func (s *Server) setWait(e ledger.Entry, kind, note string) {
	if e.WaitKind == kind && e.WaitNote == note {
		return
	}
	if _, _, err := s.ledger.Update(e.ID, func(x *ledger.Entry) {
		if x.Status == ledger.Waiting {
			x.WaitKind, x.WaitNote = kind, note
		}
	}); err != nil {
		s.logLedger(err)
	}
}

// overCap says whether a new handoff for repo on host has to wait: the
// repository already has the host's limit of running workers, and no
// worker or queued handoff exists for the branch (those answer instead).
func (s *Server) overCap(b handoffBody) (bool, string) {
	limit := s.capFor(b.Host)
	if limit <= 0 {
		return false, ""
	}
	repoName := filepath.Base(filepath.Clean(b.Repo))
	branch := strings.TrimSpace(b.Branch)
	if _, ok := s.ledger.Find(func(e *ledger.Entry) bool {
		return e.Host == b.Host && e.RepoName == repoName && (e.Branch == branch || slices.Contains(e.PreviousBranches, branch)) &&
			(e.Active() || e.Status == ledger.Waiting)
	}); ok {
		return false, ""
	}
	s.coord.mu.Lock()
	running := s.runningInLocked(b.Host, repoName, "")
	s.coord.mu.Unlock()
	if running < limit && !s.capQueue(b.Host, repoName) {
		return false, ""
	}
	return true, capNote(running, limit, repoName, b.Host)
}

// capQueue says whether handoffs already wait on the cap in repoName:
// a new one goes behind them rather than taking a slot that frees.
func (s *Server) capQueue(host, repoName string) bool {
	_, ok := s.ledger.Find(func(e *ledger.Entry) bool {
		return e.Status == ledger.Waiting && e.WaitKind == waitCap && e.Host == host && e.RepoName == repoName
	})
	return ok
}
