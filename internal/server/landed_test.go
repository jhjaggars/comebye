package server

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// landedRepo is a repository with an origin and a 40-line file f.txt on
// main (pushed), plus a worktree "wt" on branch "w" started from main.
type landedRepo struct {
	repo, wt, origin, base string
}

func newLandedRepo(t *testing.T) landedRepo {
	t.Helper()
	gitEnv(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	r := landedRepo{repo: filepath.Join(dir, "proj"), wt: filepath.Join(dir, "wt"), origin: filepath.Join(dir, "origin.git")}
	git(t, dir, "init", "-q", "-b", "main", "proj")
	git(t, dir, "init", "-q", "-b", "main", "--bare", "origin.git")
	git(t, r.repo, "remote", "add", "origin", r.origin)
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, "line "+itoa(i))
	}
	os.WriteFile(filepath.Join(r.repo, "f.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	git(t, r.repo, "add", ".")
	git(t, r.repo, "commit", "-q", "-m", "base")
	git(t, r.repo, "push", "-q", "origin", "main")
	r.base = git(t, r.repo, "rev-parse", "HEAD")
	git(t, r.repo, "worktree", "add", "-q", "-b", "w", r.wt, "main")
	return r
}

// edit replaces line n of f.txt in dir and commits it.
func (r landedRepo) edit(t *testing.T, dir string, n int, msg string) string {
	t.Helper()
	p := filepath.Join(dir, "f.txt")
	b, _ := os.ReadFile(p)
	ls := strings.Split(string(b), "\n")
	ls[n-1] = msg
	os.WriteFile(p, []byte(strings.Join(ls, "\n")), 0o644)
	git(t, dir, "commit", "-q", "-am", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

// landCherryPick lands the worker's tip on main as a new commit, after
// someone else's commit touching a far-away line (so the blob, and the
// index line of the diff, differ from the branch's) and pushes main.
func (r landedRepo) landCherryPick(t *testing.T) (landed string) {
	t.Helper()
	r.edit(t, r.repo, 38, "someone else's work")
	git(t, r.repo, "cherry-pick", "w")
	git(t, r.repo, "push", "-q", "origin", "main")
	return git(t, r.repo, "rev-parse", "HEAD")
}

func ctx() context.Context { return context.Background() }

func TestLandedCherryPickedWithDifferentIndexLine(t *testing.T) {
	r := newLandedRepo(t)
	own := r.edit(t, r.wt, 3, "the worker's change")
	landed := r.landCherryPick(t)
	// The diffs differ in their index lines (and only there).
	a, b := git(t, r.repo, "show", "--format=", own), git(t, r.repo, "show", "--format=", landed)
	if a == b {
		t.Fatal("fixture: the index lines should differ")
	}
	// Local main is stale; origin/main holds the landing.
	git(t, r.repo, "reset", "-q", "--hard", r.base)

	c := checkWorktree(ctx(), r.wt, "", "")
	if c.Error != "" || c.Unmerged != 0 {
		t.Fatalf("checks = %+v, want 0 unmerged", c)
	}
	if len(c.Landed) != 1 || c.Landed[0].SHA != own || c.Landed[0].As != landed {
		t.Fatalf("landed = %+v, want %s as %s", c.Landed, own, landed)
	}
	if got := landedText(c.BaseUsed, c.Landed); got != "landed on origin/main as "+landed[:7] {
		t.Fatalf("text = %q", got)
	}
	inspectWorktree(ctx(), &c, false)
	if c.Detail.Unpushed != 0 || len(c.Detail.Commits) != 0 {
		t.Fatalf("detail = %+v: landed work is neither unmerged nor unpushed", c.Detail)
	}
	if ok, why := c.Removable(); !ok {
		t.Fatalf("not removable: %s", why)
	}
}

func TestLandedRebasedAndMerged(t *testing.T) {
	r := newLandedRepo(t)
	r.edit(t, r.wt, 3, "first of two")
	r.edit(t, r.wt, 4, "second of two")
	// Upstream moves on, then the PR is rebase-merged: both commits land
	// under new SHAs on top of the new main.
	r.edit(t, r.repo, 30, "unrelated upstream work")
	git(t, r.repo, "cherry-pick", "w~1", "w")
	git(t, r.repo, "push", "-q", "origin", "main")
	want := git(t, r.repo, "rev-parse", "HEAD")
	r.edit(t, r.repo, 31, "even later work")
	git(t, r.repo, "push", "-q", "origin", "main")

	c := checkWorktree(ctx(), r.wt, "", "")
	if c.Unmerged != 0 || len(c.Landed) != 2 {
		t.Fatalf("checks = unmerged %d landed %+v", c.Unmerged, c.Landed)
	}
	found := false
	for _, l := range c.Landed {
		found = found || l.As == want
		if l.As == "" {
			t.Fatalf("no landed SHA for %s", l.SHA)
		}
	}
	if !found {
		t.Fatalf("landed = %+v, want one as %s", c.Landed, want)
	}
}

func TestLandedWorktreeRemovedBranchPresent(t *testing.T) {
	r := newLandedRepo(t)
	own := r.edit(t, r.wt, 3, "the worker's change")
	landed := r.landCherryPick(t)
	git(t, r.repo, "worktree", "remove", r.wt)

	c := checkGone(ctx(), r.wt, r.repo, "w", nil, nil, "main")
	if warn, msg := c.goneVerdict(); warn || !strings.Contains(msg, "landed on") || !strings.Contains(msg, landed[:7]) {
		t.Fatalf("verdict = %v %q", warn, msg)
	}
	if c.Exists || !c.Gone || c.Unmerged != 0 || len(c.Records) != 1 || c.Records[0] != "branch w" || c.Tip != own {
		t.Fatalf("checks = %+v", c)
	}
}

func TestLandedWorktreeAndBranchRemoved(t *testing.T) {
	r := newLandedRepo(t)
	own := r.edit(t, r.wt, 3, "the worker's change")
	landed := r.landCherryPick(t)
	git(t, r.repo, "worktree", "remove", r.wt)
	git(t, r.repo, "branch", "-D", "w")

	// The ledger recorded the commit in a report: patch-equivalence finds it.
	c := checkGone(ctx(), r.wt, r.repo, "w", nil, []string{own}, "main")
	warn, msg := c.goneVerdict()
	if warn || c.Unmerged != 0 || len(c.Landed) != 1 || c.Landed[0].As != landed {
		t.Fatalf("with hint: %v %q %+v", warn, msg, c)
	}
	if !strings.Contains(msg, "recorded commit "+own[:7]) || !strings.Contains(msg, "landed on main as "+landed[:7]) {
		t.Fatalf("msg = %q", msg)
	}

	// Nothing recorded: nothing of it exists outside main, which is not a warning.
	c = checkGone(ctx(), r.wt, r.repo, "w", nil, nil, "main")
	warn, msg = c.goneVerdict()
	if warn || msg != "the worktree and branch were removed; nothing of it exists outside origin/main" && msg != "the worktree and branch were removed; nothing of it exists outside main" {
		t.Fatalf("no record: %v %q", warn, msg)
	}

	// A hint that is not a commit here is ignored.
	c = checkGone(ctx(), r.wt, r.repo, "w", nil, []string{"deadbee"}, "main")
	if warn, _ := c.goneVerdict(); warn || len(c.Records) != 0 {
		t.Fatalf("unresolved hint: %+v", c)
	}
}

func TestLandedGoneKeepsTruePositives(t *testing.T) {
	r := newLandedRepo(t)
	own := r.edit(t, r.wt, 3, "never landed")
	git(t, r.repo, "worktree", "remove", r.wt)

	// Branch still there: its unmerged commit is flagged.
	c := checkGone(ctx(), r.wt, r.repo, "w", nil, nil, "main")
	if warn, msg := c.goneVerdict(); !warn || c.Unmerged != 1 || !strings.Contains(msg, "1 commits not in") {
		t.Fatalf("branch present: %v %q %+v", warn, msg, c)
	}
	// Branch deleted, but the ledger recorded the commit: still flagged.
	git(t, r.repo, "branch", "-D", "w")
	c = checkGone(ctx(), r.wt, r.repo, "w", nil, []string{own}, "main")
	if warn, msg := c.goneVerdict(); !warn || c.Unmerged != 1 {
		t.Fatalf("hint only: %v %q %+v", warn, msg, c)
	}
	// An earlier name of the branch counts as well.
	git(t, r.repo, "branch", "old-name", own)
	c = checkGone(ctx(), r.wt, r.repo, "w", []string{"old-name"}, nil, "main")
	if warn, _ := c.goneVerdict(); !warn || c.Unmerged != 1 {
		t.Fatalf("previous name: %+v", c)
	}
}

func TestLandedGoneWithoutARepoStillWarns(t *testing.T) {
	c := checkGone(ctx(), "/nowhere/wt", "", "w", nil, nil, "main")
	if warn, _ := c.goneVerdict(); !warn {
		t.Fatal("no repository on record must warn")
	}
	c = checkGone(ctx(), "/nowhere/wt", t.TempDir(), "w", nil, nil, "main")
	if warn, _ := c.goneVerdict(); !warn {
		t.Fatal("a directory that is not a repository must warn")
	}
}

// A branch equal to main (a deploy branch), with no upstream of its own,
// and one that is behind main, have no work of their own: not unpushed.
func TestBranchEqualToOrBehindBaseIsNotUnpushed(t *testing.T) {
	r := newLandedRepo(t)
	git(t, r.wt, "switch", "-q", "-c", "deploy/hub", "main")
	for _, local := range []bool{true, false} {
		if !local {
			// main moves on; the branch is now behind it, and local main is stale.
			r.edit(t, r.repo, 30, "more upstream work")
			git(t, r.repo, "push", "-q", "origin", "main")
			git(t, r.repo, "reset", "-q", "--hard", r.base)
		}
		c := checkWorktree(ctx(), r.wt, "", "")
		inspectWorktree(ctx(), &c, false)
		if c.Unmerged != 0 || c.Detail.Unpushed != 0 || c.Detail.Upstream != "" {
			t.Fatalf("local=%v: unmerged %d unpushed %d upstream %q", local, c.Unmerged, c.Detail.Unpushed, c.Detail.Upstream)
		}
		class, reason := sweepClassify(ledger.Entry{Status: ledger.Finished}, nil, &c, "")
		if class != SweepReady {
			t.Fatalf("local=%v: sweep = %s %q", local, class, reason)
		}
	}
}

func TestUnmergedAndUnpushedStillFlagged(t *testing.T) {
	r := newLandedRepo(t)
	r.edit(t, r.wt, 3, "only here")
	c := checkWorktree(ctx(), r.wt, "", "")
	inspectWorktree(ctx(), &c, false)
	if c.Unmerged != 1 || c.Detail.Unpushed != 1 || len(c.Landed) != 0 {
		t.Fatalf("unmerged %d unpushed %d landed %+v", c.Unmerged, c.Detail.Unpushed, c.Landed)
	}
	class, reason := sweepClassify(ledger.Entry{Status: ledger.Finished}, nil, &c, "")
	if class != SweepNeedsLook || !strings.Contains(reason, "on no remote") {
		t.Fatalf("sweep = %s %q", class, reason)
	}
	// Pushed to its own branch, not merged: an open PR.
	git(t, r.wt, "push", "-q", "origin", "w")
	c = checkWorktree(ctx(), r.wt, "", "")
	inspectWorktree(ctx(), &c, false)
	if class, reason = sweepClassify(ledger.Entry{Status: ledger.Finished}, nil, &c, ""); class != SweepNeedsLook || !strings.Contains(reason, "pushed but not merged") {
		t.Fatalf("pushed: %s %q", class, reason)
	}
	// Uncommitted changes stay a true positive too.
	os.WriteFile(filepath.Join(r.wt, "new.txt"), []byte("x"), 0o644)
	c = checkWorktree(ctx(), r.wt, "", "")
	if !c.Dirty {
		t.Fatal("dirty not seen")
	}
}

func TestSweepClassifyGoneWorktree(t *testing.T) {
	fin := ledger.Entry{Status: ledger.Finished}
	gone := &worktreeChecks{Gone: true, BaseUsed: "main"}
	if class, reason := sweepClassify(fin, nil, gone, ""); class != SweepReady || !strings.Contains(reason, "nothing of it exists outside main") {
		t.Fatalf("gone, nothing: %s %q", class, reason)
	}
	gone = &worktreeChecks{Gone: true, BaseUsed: "main", Records: []string{"branch w"}, Unmerged: 2}
	if class, _ := sweepClassify(fin, nil, gone, ""); class != SweepNeedsLook {
		t.Fatalf("gone, unmerged: %s", class)
	}
}

func TestEntryHints(t *testing.T) {
	e := ledger.Entry{
		FinalReport: "Committed d061192 on the branch; see also 64c1774. Words like defaced and 2026110 are not SHAs.",
		Mark:        &ledger.Mark{Reason: "its commit d061192 is landed as e4cc5f3256ac3c1d73b3755126ee1f42197748de"},
	}
	got := entryHints(e)
	want := []string{"d061192", "64c1774", "e4cc5f3256ac3c1d73b3755126ee1f42197748de"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("hints = %v, want %v", got, want)
	}
	if got := hexOnly([]string{"d061192", "ZZZZZZZ", "abc", "-x=y"}); len(got) != 1 {
		t.Fatalf("hexOnly = %v", got)
	}
}

// Through the hub's own entry point: a ledger worker whose worktree and
// branch are gone is checked from the repository the entry names, and the
// repository must lie inside the roots.
func TestLocalChecksGoneWorktreeUsesLedgerRepo(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	git(t, repo, "branch", "w")
	s := newWorkerServer(t, nil, root, wtRoot)
	missing := filepath.Join(wtRoot, "proj", "w")

	c, lerr := s.localChecks(ctx(), missing, "w", "main", checkOpts{Detail: true, Repo: repo})
	if lerr != nil || c.Exists || !c.Gone || c.Unmerged != 0 || len(c.Records) != 1 {
		t.Fatalf("with repo: %+v %v", c, lerr)
	}
	git(t, repo, "branch", "-D", "w")
	c, _ = s.localChecks(ctx(), missing, "w", "main", checkOpts{Repo: repo})
	if warn, msg := c.goneVerdict(); warn || !strings.Contains(msg, "nothing of it exists outside") {
		t.Fatalf("branch gone: %v %q", warn, msg)
	}
	// No repository given, or one outside the roots: nothing to check against.
	for _, rp := range []string{"", t.TempDir()} {
		c, _ = s.localChecks(ctx(), missing, "w", "main", checkOpts{Repo: rp})
		if c.Gone {
			t.Fatalf("repo %q: checked anyway", rp)
		}
		if warn, _ := c.goneVerdict(); !warn {
			t.Fatalf("repo %q: no warning", rp)
		}
	}
	// The options survive the query string a remote host receives.
	o := checkOpts{Repo: repo, Prev: []string{"a", "b"}, Hints: []string{"d061192"}}
	q := o.query()
	if q.Get("repo") != repo || strings.Join(splitList(q.Get("prev")), ",") != "a,b" || q.Get("hints") != "d061192" {
		t.Fatalf("query = %v", q)
	}
}

// The hub sends a peer the ledger's repository, earlier branch names and
// recorded SHAs with every checks call (inspect, removal review, sweep):
// the fleet's allowlist of query parameters once dropped them, so every
// gone worktree on a peer read "no repository on record".
func TestGoneWorktreeOptionsReachThePeer(t *testing.T) {
	peer := &workerPeer{}
	ps := httptest.NewServer(peer)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir()}, nil, nil)
	e, err := s.ledger.Add(ledger.Entry{
		Host: "kube", Repo: "/home/k/code/proj", RepoName: "proj", Branch: "feat/x", Base: "main", PreviousBranches: []string{"feat/old"},
		AgentID: "kube/wS:p9", PaneID: "wS:p9", Worktree: "/home/k/.worktrees/proj/feat-x", Status: ledger.Finished,
		FinalReport: "committed d061192 on the branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(what string) {
		t.Helper()
		got := peer.saw("GET /api/checks")
		if len(got) == 0 {
			t.Fatalf("%s: no checks call", what)
		}
		last := got[len(got)-1]
		for _, want := range []string{"repo=%2Fhome%2Fk%2Fcode%2Fproj", "prev=feat%2Fold", "hints=d061192"} {
			if !strings.Contains(last, want) {
				t.Fatalf("%s: the peer was sent %s, lacking %s", what, last, want)
			}
		}
	}
	// inspect by agent id
	if w := do(s, "GET", "/api/inspect?agent=kube/wS:p9", nil, ""); w.Code != 200 {
		t.Fatalf("inspect = %d %s", w.Code, w.Body)
	}
	check("inspect")
	// the removal review of a marked entry
	if w := do(s, "POST", "/api/marks", nil, `{"agent_id":"kube/wS:p9","reason":"done"}`); w.Code != 200 {
		t.Fatalf("mark: %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/removals", nil, ""); w.Code != 200 {
		t.Fatalf("removals = %d %s", w.Code, w.Body)
	}
	check("removal review")
	_ = e
}

// The recorded repository was renamed (herdr-phone -> comebye): the checks
// find the project's repository from a worktree beside the gone one, and,
// with no such sibling, say the recorded repository no longer exists.
func TestGoneWorktreeRenamedRepo(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	// Another worktree of the same project still exists under the old name.
	sib := filepath.Join(wtRoot, "herdr-phone", "deploy-x")
	git(t, repo, "worktree", "add", "-q", "--detach", sib, "main")
	// The worker's commit landed as a different commit; its branch is gone.
	git(t, repo, "branch", "w", "main")
	git(t, repo, "switch", "-q", "w")
	os.WriteFile(filepath.Join(repo, "w.txt"), []byte("w\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "worker")
	own := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "switch", "-q", "main")
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("o\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "other")
	git(t, repo, "cherry-pick", own)
	landed := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "branch", "-D", "w")

	s := newWorkerServer(t, nil, root, wtRoot)
	gone := filepath.Join(wtRoot, "herdr-phone", "feat-w")
	old := filepath.Join(root, "herdr-phone") // no longer exists
	c, lerr := s.localChecks(ctx(), gone, "feat/w", "main", checkOpts{Repo: old, Hints: []string{own[:7]}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	warn, msg := c.goneVerdict()
	if warn || !c.Gone || c.Unmerged != 0 || len(c.Landed) != 1 || c.Landed[0].As != landed ||
		!strings.Contains(msg, "landed on main as "+landed[:7]) || !strings.Contains(msg, "no longer exists") {
		t.Fatalf("with a sibling: warn=%v %q %+v", warn, msg, c)
	}

	// No sibling worktree: say what is missing.
	git(t, repo, "worktree", "remove", "--force", sib)
	c, _ = s.localChecks(ctx(), gone, "feat/w", "main", checkOpts{Repo: old, Hints: []string{own[:7]}})
	warn, msg = c.goneVerdict()
	if !warn || c.Gone || !strings.Contains(msg, "the recorded repository "+old+" no longer exists") {
		t.Fatalf("without a sibling: warn=%v %q", warn, msg)
	}
	// Nothing recorded at all keeps the old wording.
	c, _ = s.localChecks(ctx(), gone, "feat/w", "main", checkOpts{})
	if _, msg = c.goneVerdict(); !strings.Contains(msg, "no repository on record") {
		t.Fatalf("msg = %q", msg)
	}
}
