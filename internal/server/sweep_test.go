package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

func TestSweepClassify(t *testing.T) {
	idle := &herdr.AgentInfo{Status: herdr.StatusIdle}
	clean := func(mod func(*worktreeChecks)) *worktreeChecks {
		c := &worktreeChecks{Exists: true, Branch: "feat/x", BaseUsed: "origin/main", Detail: &worktreeDetail{Unpushed: 0}}
		if mod != nil {
			mod(c)
		}
		return c
	}
	fin := ledger.Entry{Status: ledger.Finished, Branch: "feat/x"}
	tests := []struct {
		name  string
		live  *herdr.AgentInfo
		c     *worktreeChecks
		err   string
		class string
		want  string
	}{
		{"finished and merged", idle, clean(nil), "", SweepReady, "every commit"},
		{"agent gone, merged", nil, clean(nil), "", SweepReady, ""},
		{"pushed but unmerged", idle, clean(func(c *worktreeChecks) { c.Unmerged = 2 }), "", SweepNeedsLook, "pushed but not merged"},
		{"unmerged and unpushed", idle, clean(func(c *worktreeChecks) { c.Unmerged = 2; c.Detail.Unpushed = 1 }), "", SweepNeedsLook, "on no remote"},
		{"merged yet unpushed", idle, clean(func(c *worktreeChecks) { c.Detail.Unpushed = 3 }), "", SweepNeedsLook, "on no remote"},
		{"dirty", idle, clean(func(c *worktreeChecks) { c.Dirty = true; c.DirtyFiles = 2 }), "", SweepNeedsLook, "uncommitted"},
		{"rebase", idle, clean(func(c *worktreeChecks) { c.InProgress = "rebase" }), "", SweepNeedsLook, "rebase"},
		{"unknown merge state", idle, clean(func(c *worktreeChecks) { c.Unmerged = -1 }), "", SweepNeedsLook, "merged"},
		{"working", &herdr.AgentInfo{Status: herdr.StatusWorking}, clean(nil), "", SweepNeedsLook, "working"},
		{"checks failed", idle, nil, "host down", SweepNeedsLook, "host down"},
		{"worktree gone", idle, clean(func(c *worktreeChecks) { c.Exists = false }), "", SweepNeedsLook, "gone"},
		{"primary checkout", idle, clean(func(c *worktreeChecks) { c.Primary = true }), "", SweepSkip, "primary"},
	}
	for _, tc := range tests {
		class, reason := sweepClassify(fin, tc.live, tc.c, tc.err)
		if class != tc.class || !strings.Contains(reason, tc.want) {
			t.Errorf("%s: got %s %q, want %s containing %q", tc.name, class, reason, tc.class, tc.want)
		}
	}
}

// sweepFixture is a hub with real worktrees for several ledger workers.
type sweepFix struct {
	s   *Server
	ids map[string]string // name -> agent id
}

func newSweepFix(t *testing.T) *sweepFix {
	t.Helper()
	root, repo, wtRoot := workerTree(t)
	names := []string{"merged", "pushed", "dirty", "marked", "coord", "home"}
	wts := map[string]string{}
	agents := []herdr.AgentInfo{}
	for i, n := range names {
		wt := filepath.Join(wtRoot, "proj", n)
		git(t, repo, "worktree", "add", "-q", "-b", n, wt, "main")
		wts[n] = wt
		agents = append(agents, herdr.AgentInfo{Name: n, PaneID: "w" + itoa(i+1) + ":p1", WorkspaceID: "w" + itoa(i+1), Agent: "pi", Status: herdr.StatusIdle, CWD: wt})
	}
	// "pushed": a commit on a remote-tracking ref but not in main.
	os.WriteFile(wts["pushed"]+"/p.txt", []byte("p\n"), 0o644)
	git(t, wts["pushed"], "add", ".")
	git(t, wts["pushed"], "commit", "-q", "-m", "p")
	git(t, repo, "update-ref", "refs/remotes/origin/pushed", "refs/heads/pushed")
	os.WriteFile(wts["dirty"]+"/d.txt", []byte("d\n"), 0o644)

	fh, client := startFakeLaunchHerdr(t)
	fh.set(func(f *fakeLaunchHerdr) { f.agents = agents })
	m := managerWith(t, client, "desk", len(agents))
	s := New(Config{ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), LaunchRoots: []string{root}, WorktreeRoot: wtRoot, AllowLogins: []string{"me@example.com"}, AgentToken: testToken}, client, m)
	s.askPoll, s.askWait = 10_000_000, 50_000_000
	f := &sweepFix{s: s, ids: map[string]string{}}
	for i, n := range names {
		id := "desk/w" + itoa(i+1) + ":p1"
		f.ids[n] = id
		st := ledger.Finished
		if n == "marked" {
			st = ledger.ReadyToRemove
		}
		e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: repo, RepoName: "proj", Branch: n, Base: "main", AgentID: id, PaneID: agents[i].PaneID, AgentName: n, WorkspaceID: agents[i].WorkspaceID, Worktree: wts[n], Status: st})
		if err != nil {
			t.Fatal(err)
		}
		if n == "marked" {
			s.ledger.SetMark(ledger.Mark{AgentID: id, Reason: "by hand"})
		}
		_ = e
	}
	if err := s.recordRole(f.ids["coord"], "coord", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	return f
}

func sweepOf(t *testing.T, s *Server, body string) sweepResult {
	t.Helper()
	w := do(s, "POST", "/api/sweep", userHdr, body)
	var r sweepResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("sweep = %d %s", w.Code, w.Body)
	}
	return r
}

func classOf(r sweepResult, id string) (string, string) {
	for _, l := range [][]sweepItem{r.Ready, r.NeedsLook, r.Skipped} {
		for _, it := range l {
			if it.ID == id {
				return it.Class, it.Reason
			}
		}
	}
	return "", ""
}

func statusOf(s *Server, id string) ledger.Status {
	e, _ := s.ledger.ByAgent(id)
	return e.Status
}

func TestSweepDryRunMarksNothing(t *testing.T) {
	f := newSweepFix(t)
	r := sweepOf(t, f.s, `{"dry_run":true}`)
	for _, l := range [][]sweepItem{r.Ready, r.NeedsLook, r.Skipped} {
		for _, it := range l {
			t.Logf("%-10s %-22s %s", it.Class, it.Name+"/"+it.Branch, it.Reason)
		}
	}
	if !r.DryRun {
		t.Fatal("not a dry run")
	}
	if c, why := classOf(r, f.ids["merged"]); c != SweepReady || !strings.Contains(why, "Sweep:") {
		t.Fatalf("merged = %s %q", c, why)
	}
	for n, want := range map[string]string{"pushed": SweepNeedsLook, "dirty": SweepNeedsLook, "marked": SweepSkip, "coord": SweepSkip} {
		if c, why := classOf(r, f.ids[n]); c != want {
			t.Errorf("%s = %s %q, want %s", n, c, why, want)
		}
	}
	if c, _ := classOf(r, f.ids["home"]); c == "" {
		t.Error("home worker missing")
	}
	for _, it := range r.Ready {
		if it.Marked {
			t.Error("dry run marked an item")
		}
	}
	if got := statusOf(f.s, f.ids["merged"]); got != ledger.Finished {
		t.Fatalf("merged status = %s after a dry run", got)
	}
	if len(f.s.ledger.Marks()) != 0 {
		t.Fatalf("marks = %v", f.s.ledger.Marks())
	}
}

func TestSweepMarksOnlyReady(t *testing.T) {
	f := newSweepFix(t)
	r := sweepOf(t, f.s, `{}`)
	marked := map[string]bool{}
	for _, it := range r.Ready {
		if !it.Marked {
			t.Errorf("%s ready but not marked", it.ID)
		}
		marked[it.ID] = true
	}
	for n, id := range f.ids {
		st := statusOf(f.s, id)
		switch {
		case n == "marked":
			e, _ := f.s.ledger.ByAgent(id)
			if st != ledger.ReadyToRemove || e.Mark.Reason != "by hand" {
				t.Errorf("already-marked changed: %s %+v", st, e.Mark)
			}
		case marked[id]:
			e, _ := f.s.ledger.ByAgent(id)
			if st != ledger.ReadyToRemove || e.Mark == nil || !strings.HasPrefix(e.Mark.Reason, "Sweep:") || e.Mark.Ask == nil {
				t.Errorf("%s not marked properly: %s %+v", n, st, e.Mark)
			}
		default:
			if st != ledger.Finished {
				t.Errorf("%s (%s) changed to %s", n, id, st)
			}
		}
	}
	if !marked[f.ids["merged"]] || marked[f.ids["pushed"]] || marked[f.ids["dirty"]] || marked[f.ids["coord"]] {
		t.Fatalf("marked = %v", marked)
	}
	// A second sweep finds it already marked.
	r = sweepOf(t, f.s, `{}`)
	if c, why := classOf(r, f.ids["merged"]); c != SweepSkip || !strings.Contains(why, "already marked") {
		t.Fatalf("second sweep: %s %q", c, why)
	}
}

func TestSweepNeverSweepsACoordinatorOrItself(t *testing.T) {
	f := newSweepFix(t)
	cands, skipped := f.s.sweepCandidates(f.s.fleet.Agents(), 0, f.ids["merged"], time.Now())
	for _, c := range cands {
		if c.e.AgentID == f.ids["coord"] || c.e.AgentID == f.ids["merged"] {
			t.Fatalf("candidate %s", c.e.AgentID)
		}
	}
	got := map[string]string{}
	for _, it := range skipped {
		got[it.ID] = it.Reason
	}
	if !strings.Contains(got[f.ids["coord"]], "coordinator") || !strings.Contains(got[f.ids["merged"]], "caller") {
		t.Fatalf("skipped = %v", got)
	}
}

func TestSweepIdleRunningWorker(t *testing.T) {
	f := newSweepFix(t)
	e, _ := f.s.ledger.ByAgent(f.ids["merged"])
	f.s.ledger.Update(e.ID, func(e *ledger.Entry) { e.Status = ledger.Running })
	agents := f.s.fleet.Agents()
	// Just seen idle: not yet.
	if cands, _ := f.s.sweepCandidates(agents, defaultSweepIdle, "", time.Now()); hasCand(cands, f.ids["merged"]) {
		t.Fatal("a running worker idle for moments was a candidate")
	}
	// Idle for long enough.
	if cands, _ := f.s.sweepCandidates(agents, defaultSweepIdle, "", time.Now().Add(2*defaultSweepIdle)); !hasCand(cands, f.ids["merged"]) {
		t.Fatal("a long-idle running worker was not a candidate")
	}
}

func TestSweepRoutePermissions(t *testing.T) {
	f := newSweepFix(t)
	if w := do(f.s, "POST", "/api/sweep", nil, `{}`); w.Code == 200 {
		t.Fatal("unauthenticated sweep allowed")
	}
	w := do(f.s, "POST", "/api/sweep", map[string]string{HeaderAgentToken: testToken, HeaderAgent: "w1:p1"}, `{"dry_run":true}`)
	if w.Code != 200 {
		t.Fatalf("agent sweep = %d %s", w.Code, w.Body)
	}
	var r sweepResult
	json.Unmarshal(w.Body.Bytes(), &r)
	if c, why := classOf(r, f.ids["merged"]); c != SweepSkip || !strings.Contains(why, "caller") {
		t.Fatalf("an agent swept itself: %s %q", c, why)
	}
}

func hasCand(cs []sweepCandidate, id string) bool {
	for _, c := range cs {
		if c.e.AgentID == id {
			return true
		}
	}
	return false
}
