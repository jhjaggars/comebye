package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// landedGreen takes the landing fixture to a landed entry with a green CI
// and the worker's branch deleted (never pushed, so gone from origin too).
func landedGreen(t *testing.T) (*Server, landedRepo, ledger.Entry) {
	t.Helper()
	s, r, e, _ := landingFix(t)
	s.outbox = openOutbox("")
	s.askPoll, s.askWait = 10*time.Millisecond, 100*time.Millisecond
	t.Cleanup(s.asking.Wait)
	land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`)
	r.landCherryPick(t)
	git(t, r.wt, "fetch", "-q", "origin")
	git(t, r.wt, "switch", "-q", "--detach", "origin/main")
	git(t, r.wt, "branch", "-q", "-D", "w")
	old := ghRuns
	ghRuns = func(_ context.Context, _, sha string) ([]byte, error) {
		return []byte(`[{"status":"completed","conclusion":"success","url":"https://ci/1","name":"test"}]`), nil
	}
	t.Cleanup(func() { ghRuns = old })
	g := settle(t, s, e.ID)
	if g.Status != ledger.Landed {
		t.Fatalf("landed: %s %+v", g.Status, g.LandCheck)
	}
	if _, _, err := s.ledger.SetLandCI(e.ID, ledger.CI{State: ledger.CISuccess, SHA: g.LandCheck.Landed[0]}); err != nil {
		t.Fatal(err)
	}
	g, _ = s.ledger.Get(e.ID)
	return s, r, g
}

func TestAutoCleanupMarksLandedGreenDeletedClean(t *testing.T) {
	s, _, e := landedGreen(t)
	s.autoCleanOne(e)
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.ReadyToRemove || g.Mark == nil || g.Mark.By != AutoCreator || g.Mark.Reason == "" || g.Mark.Ask == nil {
		t.Fatalf("not marked: %s %+v", g.Status, g.Mark)
	}
	ls := s.outbox.pending()
	if len(ls) != 1 || ls[0].Kind != "cleanup" || !strings.Contains(ls[0].Text, "marked it ready to remove") || !strings.Contains(ls[0].Text, e.ID) {
		t.Fatalf("outbox = %+v", ls)
	}
	// Marked once: it is no longer a candidate.
	if autoCleanCandidate(g, time.Now()) {
		t.Fatal("a marked entry is still a candidate")
	}
}

func TestAutoCleanupLeavesUncertainEntries(t *testing.T) {
	// The branch is still on origin.
	s, r, e := landedGreen(t)
	git(t, r.repo, "branch", "-q", "w", "main")
	git(t, r.repo, "push", "-q", "origin", "w")
	s.autoCleanOne(e)
	if g, _ := s.ledger.Get(e.ID); g.Status != ledger.Landed || len(s.outbox.pending()) != 0 {
		t.Fatalf("marked with the remote branch still there: %s", g.Status)
	}
	// A dirty worktree.
	s, r, e = landedGreen(t)
	os.WriteFile(filepath.Join(r.wt, "scratch.txt"), []byte("x"), 0o644)
	s.autoCleanOne(e)
	if g, _ := s.ledger.Get(e.ID); g.Status != ledger.Landed {
		t.Fatalf("marked with a dirty worktree: %s", g.Status)
	}
	// CI not green.
	s, _, e = landedGreen(t)
	s.ledger.SetLandCI(e.ID, ledger.CI{State: ledger.CIRunning, SHA: e.LandCheck.Landed[0]})
	g, _ := s.ledger.Get(e.ID)
	if autoCleanCandidate(g, time.Now()) {
		t.Fatal("CI running is a candidate")
	}
}
