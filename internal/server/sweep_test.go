package server

import (
	"context"
	"encoding/json"
	"errors"
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
		{"squash-merged", idle, clean(func(c *worktreeChecks) {
			c.Unmerged = 3
			c.Equivalent = "its changes to 2 files are identical in origin/main"
		}), "", SweepReady, "no equal in origin/main"},
		{"dirty names its files", idle, clean(func(c *worktreeChecks) {
			c.Dirty, c.DirtyFiles = true, 1
			c.Detail.Status = []string{"?? NOTES.md"}
		}), "", SweepNeedsLook, "1 file: NOTES.md"},
		{"worktree and repo gone", idle, clean(func(c *worktreeChecks) {
			c.Exists = false
			c.RepoMissing = "the recorded repository /x no longer exists"
		}), "", SweepGone, "no longer exists"},
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
	fh  *fakeLaunchHerdr
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
	t.Cleanup(s.asking.Wait) // before the temp dirs go: asks run git in the worktrees
	f := &sweepFix{fh: fh, s: s, ids: map[string]string{}}
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
	for _, l := range [][]sweepItem{r.Ready, r.NeedsLook, r.Delivered, r.Gone, r.Skipped} {
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

func TestSweepDelivered(t *testing.T) {
	idle := &herdr.AgentInfo{Status: herdr.StatusIdle}
	c := func(unpushed int, dirty bool) *worktreeChecks {
		return &worktreeChecks{Exists: true, Branch: "x", BaseUsed: "main", Unmerged: 2, Dirty: dirty, DirtyFiles: 1, Detail: &worktreeDetail{Unpushed: unpushed, Status: []string{" M scratch.md"}}}
	}
	rep := ledger.Entry{Status: ledger.Finished, Branch: "research/x", FinalReport: "findings"}
	cases := []struct {
		name  string
		e     ledger.Entry
		c     *worktreeChecks
		class string
		want  string
	}{
		{"research branch with a report", rep, c(2, false), SweepDelivered, "branch has 2 local commits"},
		{"pushed only", rep, c(0, false), SweepDelivered, "branch is pushed, 2 commits not in main"},
		{"dirty too", rep, c(2, true), SweepDelivered, "uncommitted: 1 file: scratch.md"},
		{"declared by purpose", ledger.Entry{Status: ledger.Finished, Branch: "feat/openshell", Purpose: "research", FinalReport: "r"}, c(3, false), SweepDelivered, "work delivered in the report"},
		{"no report yet", ledger.Entry{Status: ledger.Finished, Branch: "research/x"}, c(2, false), SweepNeedsLook, "on no remote"},
		{"ordinary work is never delivered", ledger.Entry{Status: ledger.Finished, Branch: "feat/x", FinalReport: "done"}, c(2, false), SweepNeedsLook, "on no remote"},
		{"a prefix does not override a declared purpose", ledger.Entry{Status: ledger.Finished, Branch: "research/x", Purpose: "work", FinalReport: "r"}, c(2, false), SweepNeedsLook, "on no remote"},
	}
	for _, tc := range cases {
		class, reason := sweepClassify(tc.e, idle, tc.c, "")
		if class != tc.class || !strings.Contains(reason, tc.want) {
			t.Errorf("%s: got %s %q, want %s containing %q", tc.name, class, reason, tc.class, tc.want)
		}
	}
}

// squashFixture: a branch of two commits whose changes main received as one
// squash commit.
func squashFixture(t *testing.T) (wt string) {
	t.Helper()
	_, repo, wtRoot := workerTree(t)
	wt = filepath.Join(wtRoot, "proj", "sq")
	git(t, repo, "worktree", "add", "-q", "-b", "sq", wt, "main")
	os.WriteFile(wt+"/b.txt", []byte("one\n"), 0o644)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "one")
	os.WriteFile(wt+"/b.txt", []byte("one\ntwo\n"), 0o644)
	os.WriteFile(wt+"/c.txt", []byte("c\n"), 0o644)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "two")
	// main gets the same final content as a single commit.
	os.WriteFile(repo+"/b.txt", []byte("one\ntwo\n"), 0o644)
	os.WriteFile(repo+"/c.txt", []byte("c\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "squash (#1)")
	return wt
}

func TestSquashMergedBranchIsEquivalent(t *testing.T) {
	wt := squashFixture(t)
	c := checkWorktree(context.Background(), wt, "sq", "main")
	if c.Unmerged != 2 || !strings.Contains(c.Equivalent, "2 files are identical in main") {
		t.Fatalf("unmerged=%d equivalent=%q", c.Unmerged, c.Equivalent)
	}
	if ok, why := c.Removable(); !ok {
		t.Fatalf("not removable: %s", why)
	}
	class, reason := sweepClassify(ledger.Entry{Status: ledger.Finished, Branch: "sq"}, nil, &c, "")
	if class != SweepReady || !strings.Contains(reason, "no equal in main") {
		t.Fatalf("%s %q", class, reason)
	}
}

func TestSquashEquivalenceBreaksWhenMainDiffers(t *testing.T) {
	wt := squashFixture(t)
	// main changed one of the files since: the content check fails and, with
	// no merged PR, the branch stays unmerged.
	repo := filepath.Dir(git(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	os.WriteFile(repo+"/c.txt", []byte("c changed\n"), 0o644)
	git(t, repo, "commit", "-q", "-am", "later")
	old := ghMergedPRs
	t.Cleanup(func() { ghMergedPRs = old })
	ghMergedPRs = func(context.Context, string, string) ([]ghPR, error) { return nil, errors.New("no gh") }
	c := checkWorktree(context.Background(), wt, "sq", "main")
	if c.Equivalent != "" || c.Unmerged != 2 {
		t.Fatalf("unmerged=%d equivalent=%q", c.Unmerged, c.Equivalent)
	}
	if ok, _ := c.Removable(); ok {
		t.Fatal("removable")
	}
	// A merged PR whose head is the branch tip makes it equivalent anyway;
	// one whose head is another commit does not.
	ghMergedPRs = func(context.Context, string, string) ([]ghPR, error) {
		return []ghPR{{Number: 7, HeadRefOid: "0000000000000000000000000000000000000000"}}, nil
	}
	if c = checkWorktree(context.Background(), wt, "sq", "main"); c.Equivalent != "" {
		t.Fatalf("stale PR head accepted: %q", c.Equivalent)
	}
	tip := git(t, wt, "rev-parse", "sq")
	ghMergedPRs = func(context.Context, string, string) ([]ghPR, error) {
		return []ghPR{{Number: 7, HeadRefOid: tip}}, nil
	}
	if c = checkWorktree(context.Background(), wt, "sq", "main"); !strings.Contains(c.Equivalent, "PR #7") {
		t.Fatalf("equivalent = %q", c.Equivalent)
	}
}

func TestSweepAdHocGoneAndMarkingGroups(t *testing.T) {
	f := newSweepFix(t)
	repoDir := f.s.cfg.LaunchRoots[0] + "/proj"
	// An agent with no ledger entry at all, in a worktree of its own.
	wt := filepath.Join(f.s.cfg.WorktreeRoot, "proj", "byhand")
	git(t, repoDir, "worktree", "add", "-q", "-b", "byhand", wt, "main")
	// A research worker that reported, with a local commit.
	rw := f.ids["pushed"]
	e, _ := f.s.ledger.ByAgent(rw)
	f.s.ledger.Update(e.ID, func(e *ledger.Entry) { e.Branch = "pushed"; e.Purpose = "research"; e.FinalReport = "done" })
	// A worker on a host the hub does not know.
	if _, err := f.s.ledger.Add(ledger.Entry{Host: "arch1", Repo: "/r", RepoName: "r", Branch: "smoke/x", AgentID: "arch1/w1:p1", AgentName: "smoke", Worktree: "/w/smoke", Status: ledger.Finished}); err != nil {
		t.Fatal(err)
	}
	agents := f.s.fleet.Agents()
	agents = append(agents, herdr.AgentInfo{ID: "desk/w9:p1", Host: "desk", Name: "byhand", PaneID: "w9:p1", WorkspaceID: "w9", Status: herdr.StatusIdle,
		CWD: wt, Worktree: &herdr.WorktreeInfo{RepoName: "proj", CheckoutPath: wt, IsLinked: true, Branch: "byhand"}})
	cands, skipped := f.s.sweepCandidates(agents, 0, "", time.Now())
	if hasCand(cands, "desk/w9:p1") {
		t.Fatal("ad hoc agent considered with idle-after 0")
	}
	long := time.Now().Add(-2 * time.Hour)
	for i := range agents {
		agents[i].StatusSince = &long
	}
	cands, skipped = f.s.sweepCandidates(agents, defaultSweepIdle, "", time.Now())
	if !hasCand(cands, "desk/w9:p1") {
		t.Fatalf("an idle agent with no handoff was not a candidate: %v", skipped)
	}
	for _, c := range cands {
		if c.e.AgentID == "desk/w9:p1" && (!c.adHoc || c.e.Branch != "byhand" || c.e.Worktree != wt) {
			t.Fatalf("ad hoc candidate = %+v", c)
		}
	}
	gone := false
	for _, it := range skipped {
		if it.ID == "arch1/w1:p1" {
			gone = it.Class == SweepGone
		}
	}
	if !gone {
		t.Fatalf("unknown host not in the gone group: %v", skipped)
	}
}

func TestSweepMarksDeliveredOnlyWhenAsked(t *testing.T) {
	f := newSweepFix(t)
	id := f.ids["pushed"]
	e, _ := f.s.ledger.ByAgent(id)
	f.s.ledger.Update(e.ID, func(e *ledger.Entry) { e.Purpose = "research"; e.FinalReport = "findings" })
	r := sweepOf(t, f.s, `{"dry_run":true,"mark_delivered":true}`)
	if len(r.Delivered) != 1 || r.Delivered[0].ID != id || r.Delivered[0].Marked || !strings.Contains(r.Delivered[0].Reason, "work delivered in the report") {
		t.Fatalf("delivered = %+v", r.Delivered)
	}
	r = sweepOf(t, f.s, `{}`)
	if len(r.Delivered) != 1 || r.Delivered[0].Marked || statusOf(f.s, id) != ledger.Finished {
		t.Fatalf("a plain sweep marked the delivered group: %+v %s", r.Delivered, statusOf(f.s, id))
	}
	if !strings.Contains(sweepSummary(r), "Work delivered in the report") {
		t.Fatal("the coordinator summary omits the delivered group")
	}
	r = sweepOf(t, f.s, `{"mark_delivered":true}`)
	got, _ := f.s.ledger.ByAgent(id)
	if len(r.Delivered) != 1 || !r.Delivered[0].Marked || got.Status != ledger.ReadyToRemove || got.Mark == nil || !got.Mark.Discard || !strings.HasPrefix(got.Mark.Reason, "Sweep:") {
		t.Fatalf("delivered not marked for discard: %+v %+v", r.Delivered, got.Mark)
	}
}

func TestSweepMarksGoneRowsItself(t *testing.T) {
	f := newSweepFix(t)
	if _, err := f.s.ledger.Add(ledger.Entry{Host: "arch1", Repo: "/r", RepoName: "r", Branch: "smoke/x", AgentID: "arch1/w1:p1", AgentName: "smoke", Worktree: "/w/smoke", Status: ledger.Finished}); err != nil {
		t.Fatal(err)
	}
	r := sweepOf(t, f.s, `{"dry_run":true}`)
	if len(r.Gone) != 1 || r.Gone[0].Marked || statusOf(f.s, "arch1/w1:p1") != ledger.Finished {
		t.Fatalf("dry run: %+v", r.Gone)
	}
	r = sweepOf(t, f.s, `{}`)
	got, _ := f.s.ledger.ByAgent("arch1/w1:p1")
	if len(r.Gone) != 1 || !r.Gone[0].Marked || got.Status != ledger.ReadyToRemove || got.Mark == nil || got.Mark.Discard || !strings.HasPrefix(got.Mark.Reason, "Sweep:") {
		t.Fatalf("gone not marked by a plain sweep: %+v %+v", r.Gone, got.Mark)
	}
}

// A gone row (host not in the fleet) is reviewed without a failed-checks
// warning, and confirming it clears the record without calling any host.
func TestConfirmingAGoneRowClearsTheRecord(t *testing.T) {
	f := newSweepFix(t)
	if _, err := f.s.ledger.Add(ledger.Entry{Host: "arch1", Repo: "/r", RepoName: "r", Branch: "smoke/x", AgentID: "arch1/w1:p1", AgentName: "smoke", Worktree: "/w/smoke", Status: ledger.Finished}); err != nil {
		t.Fatal(err)
	}
	sweepOf(t, f.s, `{}`)
	w := do(f.s, "GET", "/api/removals", userHdr, "")
	var rv struct {
		Removals []removal `json:"removals"`
	}
	json.Unmarshal(w.Body.Bytes(), &rv)
	var it *removal
	for i := range rv.Removals {
		if rv.Removals[i].ID == "arch1/w1:p1" {
			it = &rv.Removals[i]
		}
	}
	if it == nil || !it.Safe || it.ChecksError != "" || len(it.Warnings) != 0 || !strings.Contains(strings.Join(it.Notes, " "), "only clears the record") {
		t.Fatalf("review of a gone row: %+v", it)
	}
	w = do(f.s, "POST", "/api/removals/confirm", userHdr, `{"ids":["arch1/w1:p1"]}`)
	var cr struct {
		Results []removalOutcome `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &cr)
	if w.Code != 200 || len(cr.Results) != 1 || !cr.Results[0].OK {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	e, _ := f.s.ledger.Get(it.EntryID)
	if e.Status != ledger.Removed || len(f.s.ledger.Marks()) != 0 {
		t.Fatalf("record not cleared: %s %v", e.Status, f.s.ledger.Marks())
	}
}

// A worker whose worktree and repository are both gone (on a host the hub
// knows): the sweep calls it gone and marks it, the review says nothing is
// left to check or delete, and confirming drops the record.
func TestConfirmingARowWhoseRepositoryIsGone(t *testing.T) {
	f := newSweepFix(t)
	wt := filepath.Join(f.s.cfg.WorktreeRoot, "orphan", "vanished")
	if _, err := f.s.ledger.Add(ledger.Entry{Host: "desk", Repo: filepath.Join(f.s.cfg.LaunchRoots[0], "no-such-repo"), RepoName: "no-such-repo", Branch: "scratch/x", AgentID: "desk/w42:p1", AgentName: "vanished", Worktree: wt, Status: ledger.Finished}); err != nil {
		t.Fatal(err)
	}
	// herdr has no workspace for the vanished worker; the removal asks.
	f.fh.set(func(h *fakeLaunchHerdr) {
		h.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
			switch method {
			case "workspace.list":
				return map[string]any{"type": "workspace_list", "workspaces": []any{}}, nil, true
			case "worktree.list":
				return map[string]any{"type": "worktree_list", "worktrees": []any{}}, nil, true
			}
			return nil, nil, false
		}
	})
	r := sweepOf(t, f.s, `{}`)
	if c, why := classOf(r, "desk/w42:p1"); c != SweepGone || !strings.Contains(why, "no longer exists") {
		t.Fatalf("class = %s %q", c, why)
	}
	w := do(f.s, "GET", "/api/removals", userHdr, "")
	var rv struct {
		Removals []removal `json:"removals"`
	}
	json.Unmarshal(w.Body.Bytes(), &rv)
	var it *removal
	for i := range rv.Removals {
		if rv.Removals[i].ID == "desk/w42:p1" {
			it = &rv.Removals[i]
		}
	}
	if it == nil || !it.Safe || len(it.Warnings) != 0 || !strings.Contains(strings.Join(it.Notes, " "), "confirming only drops the record") {
		t.Fatalf("review: %+v", it)
	}
	w = do(f.s, "POST", "/api/removals/confirm", userHdr, `{"ids":["desk/w42:p1"]}`)
	var cr struct {
		Results []removalOutcome `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &cr)
	if w.Code != 200 || len(cr.Results) != 1 || !cr.Results[0].OK {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	if e, _ := f.s.ledger.Get(it.EntryID); e.Status != ledger.Removed {
		t.Fatalf("status = %s", e.Status)
	}
}
