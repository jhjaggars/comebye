package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// Auto cleanup. A handoff whose work is landed, whose CI on the landed
// commit is green, whose branch is gone from origin and whose worktree is
// clean has nothing left for a person to judge. The hub marks it ready to
// remove itself, with the usual ask to the session, and tells the
// coordinator in one short line. The user still confirms the removal in
// the app. Anything uncertain (a host that cannot say whether the branch
// is on origin, a dirty or detached-in-progress worktree, a session that
// answers UNSAFE) leaves the entry as it was, to be handled as before.

const (
	autoCleanEvery  = 3 * time.Minute // between looks at one entry
	autoCleanWindow = 6 * time.Hour   // after landing
	autoCleanTries  = 20
	autoCleanReason = "landed, CI green, remote branch deleted, worktree clean (marked by the hub)"
)

// autoCleanCandidate: the entry is landed and its landing report is in,
// CI is green, and nothing marks it yet.
func autoCleanCandidate(e ledger.Entry, now time.Time) bool {
	l := e.LandCheck
	return e.Status == ledger.Landed && e.Mark == nil && !e.AwaitLandReport && e.ReportSource == "explicit" &&
		l != nil && l.OK && l.CI != nil && l.CI.State == ledger.CISuccess && now.Sub(l.At) < autoCleanWindow &&
		e.Worktree != "" && e.AgentID != ""
}

// autoCleanOK reads a worktree's checks: all of them true, none unknown.
func autoCleanOK(c *worktreeChecks) bool {
	return c != nil && c.Exists && !c.Primary && !c.Dirty && c.InProgress == "" && c.Error == "" &&
		c.Unmerged == 0 && c.RemoteGone != nil && *c.RemoteGone
}

// autoCleanup looks at the landed entries that qualify, at most once per
// autoCleanEvery each.
func (s *Server) autoCleanup(agents []herdr.AgentInfo) {
	now := time.Now()
	for _, e := range s.ledger.Entries() {
		if !autoCleanCandidate(e, now) {
			continue
		}
		key := "clean/" + e.ID
		s.coord.mu.Lock()
		last, seen := s.coord.calling[key]
		if (seen && now.Sub(last) < autoCleanEvery) || s.coord.checking[key] {
			s.coord.mu.Unlock()
			continue
		}
		s.coord.calling[key] = now
		s.coord.checking[key] = true
		s.coord.mu.Unlock()
		go func(e ledger.Entry) {
			defer func() {
				s.coord.mu.Lock()
				delete(s.coord.checking, key)
				s.coord.mu.Unlock()
			}()
			s.autoCleanOne(e)
		}(e)
	}
}

func (s *Server) autoCleanOne(e ledger.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := checkOpts{Remote: true}.forEntry(e)
	base := e.Base
	if e.Approval != nil && e.Approval.Base != "" {
		base = e.Approval.Base
	}
	c, lerr := s.checksOn(ctx, e.Host, e.Worktree, landBranch(e), base, opts)
	if lerr != nil || !autoCleanOK(c) {
		return
	}
	// The entry may have changed while the checks ran.
	if cur, ok := s.ledger.Get(e.ID); !ok || !autoCleanCandidate(cur, time.Now()) {
		return
	}
	var agent *herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		if a.ID == e.AgentID {
			a := a
			agent = &a
		}
	}
	m := ledger.Mark{AgentID: e.AgentID, By: AutoCreator, Reason: autoCleanReason}
	if _, _, err := s.applyMark(m, agent, true); err != nil {
		log.Printf("auto cleanup of %s: %v", e.ID, err)
		return
	}
	s.queueLine("cleanup", e.ID, e.ID, autoCleanLine(e))
}

// autoCleanLine is the one line the coordinator gets.
func autoCleanLine(e ledger.Entry) string {
	return fmt.Sprintf("handoff %s (branch %s in %s on %s) is landed with CI green, its remote branch deleted and its worktree clean; the hub marked it ready to remove and the user confirms in the app. Nothing to do unless they want follow-ups from it.",
		safeText(e.ID), safeText(e.Branch), safeText(e.RepoName), safeText(e.Host))
}
