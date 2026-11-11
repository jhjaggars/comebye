package server

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// landingFix is a hub whose worker desk/w2:p1 committed on branch w of a
// real repository and reported.
func landingFix(t *testing.T) (*Server, landedRepo, ledger.Entry, string) {
	t.Helper()
	r := newLandedRepo(t)
	own := r.edit(t, r.wt, 3, "the worker's change")
	s, _, _ := coordServer(t)
	s.cfg.LaunchRoots = []string{filepath.Dir(r.repo)}
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: r.repo, RepoName: "proj", Branch: "w", Base: "main", AgentID: "desk/w2:p1", PaneID: "w2:p1", AgentName: "home", Worktree: r.wt, Status: ledger.Running, ReplyTo: &ledger.Ref{AgentID: "desk/w1:p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ledger.Report("desk/w2:p1", "## Summary\ndone as "+own, time.Now().Add(-time.Hour)); !ok {
		t.Fatal("report")
	}
	return s, r, e, own
}

func land(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	w := do(s, "POST", "/api/landings", agentHdr("w1:p1"), body)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// settle sends the landing report (filed 10 minutes ago, so the hub's grace
// for a worker that just reported is over) and waits for the hub's check.
func settle(t *testing.T, s *Server, id string) ledger.Entry {
	t.Helper()
	if _, ok, _ := s.ledger.Report("desk/w2:p1", "landed", time.Now().Add(-10*time.Minute)); !ok {
		t.Fatal("landing report refused")
	}
	var e ledger.Entry
	waitFor(t, func() bool {
		s.checkLandings()
		e, _ = s.ledger.Get(id)
		return e.Status != ledger.Landing
	})
	return e
}

func TestLandingRecordsApprovalAndLands(t *testing.T) {
	s, r, e, own := landingFix(t)
	if code, out := land(t, s, `{"agent_id":"desk/w2:p1"}`); code != 400 || out["code"] != "note_required" {
		t.Fatalf("no note = %d %v", code, out)
	}
	if code, out := land(t, s, `{"agent_id":"desk/w1:p1","note":"x"}`); code != 409 || out["code"] != "self" {
		t.Fatalf("self = %d %v", code, out)
	}
	code, _ := land(t, s, `{"agent_id":"desk/w2:p1","note":"user: \"land it\"","base":"main"}`)
	g, _ := s.ledger.Get(e.ID)
	if code != 200 || g.Status != ledger.Landing || g.Approval == nil || g.Approval.Tip != own || g.Approval.Note != `user: "land it"` || !strings.Contains(g.Approval.By, "desk/w1:p1") {
		t.Fatalf("landing = %d %+v %+v", code, g, g.Approval)
	}
	// The worker lands: cherry-picked onto main under a new SHA, pushed,
	// its worktree detached and its branch deleted.
	landedSHA := r.landCherryPick(t)
	git(t, r.wt, "fetch", "-q", "origin")
	git(t, r.wt, "switch", "-q", "--detach", "origin/main")
	git(t, r.wt, "branch", "-q", "-D", "w")

	g = settle(t, s, e.ID)
	if g.Status != ledger.Landed || g.LandCheck == nil || !g.LandCheck.OK || !strings.HasSuffix(g.LandCheck.Base, "main") {
		t.Fatalf("settled = %s %+v", g.Status, g.LandCheck)
	}
	if len(g.LandCheck.Landed) != 1 || g.LandCheck.Landed[0] != landedSHA {
		t.Fatalf("landed = %v, want %s", g.LandCheck.Landed, landedSHA)
	}
	if g.Callback == nil || g.Callback.State != ledger.CallbackPending {
		t.Fatalf("callback = %+v", g.Callback)
	}
	if txt := callbackText(g, false); !strings.Contains(txt, "it is landed") || !strings.Contains(txt, "main") {
		t.Fatalf("notice = %s", txt)
	}
	// Landing again is refused: it has landed.
	if code, out := land(t, s, `{"agent_id":"desk/w2:p1","note":"again"}`); code != 409 || out["code"] != "not_ready" {
		t.Fatalf("re-land = %d %v", code, out)
	}
}

// A worker that detached and deleted its branch without pushing must not
// look landed: the approved tip is checked as well as the worktree.
func TestLandingNotPushedIsNotLanded(t *testing.T) {
	s, r, e, _ := landingFix(t)
	if code, out := land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`); code != 200 {
		t.Fatalf("land = %d %v", code, out)
	}
	git(t, r.wt, "switch", "-q", "--detach", "origin/main")
	git(t, r.wt, "branch", "-q", "-D", "w")
	g := settle(t, s, e.ID)
	if g.Status != ledger.Finished || g.LandCheck == nil || g.LandCheck.OK || !strings.Contains(g.LandCheck.Verdict, "1 commits not in") {
		t.Fatalf("settled = %s %+v", g.Status, g.LandCheck)
	}
	if txt := callbackText(g, false); !strings.Contains(txt, "back to finished") || !strings.Contains(txt, "Do not tell the worker to force") {
		t.Fatalf("notice = %s", txt)
	}
	// It may be landed again.
	if code, _ := land(t, s, `{"agent_id":"desk/w2:p1","note":"retry"}`); code != 200 {
		t.Fatalf("retry = %d", code)
	}
}

func TestLandingRefusedAndCancelled(t *testing.T) {
	s, _, e, _ := landingFix(t)
	s.ledger.Observe([]herdr.AgentInfo{{ID: "desk/w2:p1", Status: herdr.StatusWorking}}, time.Now())
	if code, out := land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`); code != 409 || !strings.Contains(out["message"].(string), "running") {
		t.Fatalf("running = %d %v", code, out)
	}
	s.ledger.Report("desk/w2:p1", "again", time.Now().Add(-time.Hour))
	if code, _ := land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`); code != 200 {
		t.Fatal("land")
	}
	if code, out := land(t, s, `{"agent_id":"desk/w2:p1","cancel":true}`); code != 200 || out["cancelled"] != true {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if g, _ := s.ledger.Get(e.ID); g.Status != ledger.Finished || g.Approval != nil {
		t.Fatalf("cancelled = %+v", g)
	}
}

// A worker that lands a follow-on task from a new branch is checked on that
// branch (named by `comebye land --branch`), not on the handoff's own.
func TestLandingFollowsTheLandedBranch(t *testing.T) {
	for _, named := range []bool{false, true} {
		s, r, e, _ := landingFix(t)
		// The first task's branch w stays as it is, unlanded; the follow-on
		// is done on a new branch from main.
		git(t, r.wt, "switch", "-q", "-c", "follow", "main")
		follow := r.edit(t, r.wt, 5, "the follow-on change")
		body := `{"agent_id":"desk/w2:p1","note":"ok","branch":"follow"}`
		if !named {
			body = `{"agent_id":"desk/w2:p1","note":"ok"}`
		}
		code, out := land(t, s, body)
		if code != 200 {
			t.Fatalf("land = %d %v", code, out)
		}
		g, _ := s.ledger.Get(e.ID)
		if named && (g.Approval.Branch != "follow" || g.Approval.Tip != follow) {
			t.Fatalf("approval = %+v, want branch follow tip %s", g.Approval, follow)
		}
		r.edit(t, r.repo, 38, "someone else's work")
		git(t, r.repo, "cherry-pick", "follow")
		git(t, r.repo, "push", "-q", "origin", "main")
		git(t, r.wt, "fetch", "-q", "origin")
		git(t, r.wt, "switch", "-q", "--detach", "origin/main")
		git(t, r.wt, "branch", "-q", "-D", "follow")
		g = settle(t, s, e.ID)
		if named && (g.Status != ledger.Landed || g.LandCheck == nil || !g.LandCheck.OK) {
			t.Fatalf("named branch: settled = %s %+v", g.Status, g.LandCheck)
		}
		if !named && g.Status == ledger.Landed {
			t.Fatalf("without --branch the entry's own branch is still what is checked, got %s", g.Status)
		}
		if code, out := land(t, s, `{"agent_id":"desk/w2:p1","note":"x","branch":"bad branch"}`); code != 400 && g.Status == ledger.Finished {
			t.Fatalf("bad branch = %d %v", code, out)
		}
	}
}
