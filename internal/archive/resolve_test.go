package archive

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests use real git repositories (a bare origin and a clone) and, for GitHub, a
// fake `gh` on PATH that answers from a file and logs how it was asked.

func newGit(gh bool) *gitCtx {
	return &gitCtx{host: "h", enabled: true, gh: gh, noFetch: true, cache: map[string]*repoInfo{}, fetched: map[string]bool{}, errs: &[]string{}, res: map[string]resolution{}}
}

// commitFile writes a file in dir, commits it and returns the SHA.
func commitFile(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "add", name)
	gitDo(t, dir, "commit", "-q", "-m", msg)
	return gitDo(t, dir, "rev-parse", "HEAD")
}

// fakePRs puts a `gh` first on PATH. It prints the file named by FAKE_GH_OUT for the
// commit-to-PR lookup, "[]" for a PR list, and appends its arguments to FAKE_GH_LOG.
func fakePRs(t *testing.T, answer string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	out := filepath.Join(bin, "answer.json")
	if err := os.WriteFile(out, []byte(answer), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$@\" >> \"$FAKE_GH_LOG\"\ncase \"$*\" in\n*\"pr list\"*) echo '[]';;\n*) cat \"$FAKE_GH_OUT\";;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_GH_LOG", log)
	t.Setenv("FAKE_GH_OUT", out)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func ghCalls(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "/pulls") {
			n++
		}
	}
	return n
}

func hasGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
}

// A feature branch lands by being rebased onto a main that moved, and by a
// fast-forward: the session's SHA is not an ancestor, and the subject is
// unchanged here only by luck. The patch-id finds it.
func TestResolveRebase(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	commitFile(t, clone, "base.txt", "one\ntwo\nthree\n", "add the base file")
	gitDo(t, clone, "push", "-q", "origin", "main")
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	f := commitFile(t, clone, "a.txt", "alpha\nbeta\n", "add the alpha file")
	gitDo(t, clone, "checkout", "-q", "main")
	// Main moves on, in the same file the branch's change sits next to.
	commitFile(t, clone, "base.txt", "one\nTWO\nthree\nfour\n", "other work lands first")
	gitDo(t, clone, "push", "-q", "origin", "main")
	gitDo(t, clone, "checkout", "-q", "feat")
	gitDo(t, clone, "rebase", "-q", "main")
	rebased := gitDo(t, clone, "rev-parse", "HEAD")
	gitDo(t, clone, "checkout", "-q", "main")
	gitDo(t, clone, "merge", "-q", "--ff-only", "feat")
	gitDo(t, clone, "push", "-q", "origin", "main")
	if f == rebased {
		t.Fatal("the rebase changed nothing")
	}
	log := fakePRs(t, "[]")
	g := newGit(true)
	r := g.resolve("h", clone, "widgets", nil, f)
	if r.state != "landed" || r.landedAs != rebased || !strings.Contains(r.how, "patch-id") || r.patchID == "" {
		t.Fatalf("%+v", r)
	}
	if ghCalls(t, log) != 0 {
		t.Error("GitHub was asked about a commit git had already placed")
	}
	// The rebased commit itself is an ancestor.
	if r := newGit(false).resolve("h", clone, "widgets", nil, rebased); r.state != "landed" || !strings.Contains(r.how, "ancestor") {
		t.Errorf("ancestry: %+v", r)
	}
}

// A reworded commit: same change, new message, which the old subject match missed.
func TestResolveReword(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	f := commitFile(t, clone, "w.txt", "widget\n", "wip widget")
	gitDo(t, clone, "checkout", "-q", "main")
	gitDo(t, clone, "cherry-pick", f)
	gitDo(t, clone, "commit", "-q", "--amend", "-m", "Add the widget, with a subject nothing like the first")
	gitDo(t, clone, "push", "-q", "origin", "main")
	g := newGit(false)
	r := g.resolve("h", clone, "widgets", nil, f)
	if r.state != "landed" || !strings.Contains(r.how, "patch-id") {
		t.Fatalf("%+v", r)
	}
}

// A squash merge: two commits become one with a different diff, so only the PR
// can say they landed. GitHub's answer is faked.
func TestResolveSquashWithAPR(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "remote", "add", "upstream", "https://github.com/acme/widgets.git")
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	a := commitFile(t, clone, "c.txt", "one\n", "first step")
	b := commitFile(t, clone, "c.txt", "one\ntwo\n", "second step")
	gitDo(t, clone, "checkout", "-q", "main")
	gitDo(t, clone, "merge", "-q", "--squash", "feat")
	gitDo(t, clone, "commit", "-q", "-m", "Add c (#12)")
	squash := gitDo(t, clone, "rev-parse", "HEAD")
	gitDo(t, clone, "push", "-q", "origin", "main")
	log := fakePRs(t, `[{"number":12,"state":"closed","merged_at":"2026-11-10T10:00:00Z","merge_commit_sha":"`+squash+`","base":{"ref":"main","repo":{"full_name":"acme/widgets"}}}]`)

	// Without --gh git cannot tell: unknown, and it says why.
	if r := newGit(false).resolve("h", clone, "widgets", nil, a); r.state != "unknown" || !strings.Contains(r.how, "--gh") {
		t.Fatalf("no gh: %+v", r)
	}
	g := newGit(true)
	for _, sha := range []string{a, b} {
		r := g.resolve("h", clone, "widgets", nil, sha)
		if r.state != "landed" || r.pr != "acme/widgets#12" || r.landedAs != squash || !strings.Contains(r.how, "PR acme/widgets#12 merged into main") {
			t.Fatalf("%s: %+v", sha[:8], r)
		}
	}
	if n := ghCalls(t, log); n != 2 {
		t.Errorf("one lookup per commit, got %d", n)
	}
	// The question went to the remote's repository, by the commit's full SHA.
	b2, _ := os.ReadFile(log)
	if !strings.Contains(string(b2), "repos/acme/widgets/commits/"+a+"/pulls") {
		t.Errorf("asked: %s", b2)
	}
}

// A squash with no PR is the case git cannot see: it must read unknown, not landed
// and not unmerged.
func TestResolveSquashWithNoPR(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "remote", "add", "upstream", "https://github.com/acme/widgets.git")
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	a := commitFile(t, clone, "c.txt", "one\n", "first step")
	commitFile(t, clone, "c.txt", "one\ntwo\n", "second step")
	gitDo(t, clone, "checkout", "-q", "main")
	gitDo(t, clone, "merge", "-q", "--squash", "feat")
	gitDo(t, clone, "commit", "-q", "-m", "Add c, pushed straight to main")
	gitDo(t, clone, "push", "-q", "origin", "main")
	fakePRs(t, "[]")
	for _, gh := range []bool{false, true} {
		r := newGit(gh).resolve("h", clone, "widgets", nil, a)
		if r.state != "unknown" || r.landedAs != "" || r.pr != "" {
			t.Fatalf("gh=%v: %+v", gh, r)
		}
		if gh && !strings.Contains(r.how, "GitHub names no PR") {
			t.Errorf("%s", r.how)
		}
	}
}

// A commit in an open PR is not on main: that is known, not a guess.
func TestResolveOpenPR(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "remote", "add", "upstream", "https://github.com/acme/widgets.git")
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	f := commitFile(t, clone, "d.txt", "d\n", "an open change")
	fakePRs(t, `[{"number":40,"state":"open","merged_at":"","merge_commit_sha":"","base":{"ref":"main"}}]`)
	r := newGit(true).resolve("h", clone, "widgets", nil, f)
	if r.state != "notinmain" || r.pr != "acme/widgets#40" || !strings.Contains(r.how, "PR acme/widgets#40 is open") {
		t.Fatalf("%+v", r)
	}
	// Merged into some other branch is not landing on main.
	fakePRs(t, `[{"number":41,"state":"closed","merged_at":"2026-11-10T10:00:00Z","merge_commit_sha":"abc","base":{"ref":"release-1"}}]`)
	r = newGit(true).resolve("h", clone, "widgets", nil, f)
	if r.state != "unknown" || r.pr != "acme/widgets#41" || !strings.Contains(r.how, "PR acme/widgets#41 merged into release-1, not main") {
		t.Fatalf("%+v", r)
	}
}

// A commit this checkout never had, and with no PR lookup, is unknown too.
func TestResolveMissingCommit(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	r := newGit(false).resolve("h", clone, "widgets", nil, "deadbeefdeadbeef")
	if r.state != "unknown" || !strings.Contains(r.how, "not in") {
		t.Fatalf("%+v", r)
	}
	if r := newGit(false).resolve("h", "/nowhere", "nothere", nil, "deadbeef"); r.state != "unchecked" {
		t.Errorf("no checkout: %+v", r)
	}
}

// prime reaches the patch-id scan back only as far as the oldest commit asked about.
func TestPrimeFindsTheOldestCommit(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	f := commitFile(t, clone, "p.txt", "p\n", "something")
	g := newGit(false)
	g.prime([]map[string]any{{"host": "h", "cwd": clone, "repo": "widgets", "sha": f[:10]}, {"host": "h", "cwd": clone, "repo": "widgets", "sha": "ffffffffff"}}, nil)
	if g.since[clone] <= 0 {
		t.Fatalf("%v", g.since)
	}
}

// End to end: a session recorded a commit, main got it rebased and the index says
// so, with the patch-id and the commit on main recorded and the change findable.
func TestIndexResolvesARebasedCommit(t *testing.T) {
	hasSqlite(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "checkout", "-q", "-b", "feat/x")
	f := commitFile(t, clone, "a.txt", "alpha\n", "add alpha")
	gitDo(t, clone, "checkout", "-q", "main")
	commitFile(t, clone, "b.txt", "bravo\n", "other work")
	gitDo(t, clone, "push", "-q", "origin", "main")
	gitDo(t, clone, "checkout", "-q", "feat/x")
	gitDo(t, clone, "rebase", "-q", "main")
	rebased := gitDo(t, clone, "rev-parse", "HEAD")
	gitDo(t, clone, "checkout", "-q", "main")
	gitDo(t, clone, "merge", "-q", "--ff-only", "feat/x")
	gitDo(t, clone, "push", "-q", "origin", "main")

	r := newRig(t)
	ix := &Index{Path: filepath.Join(r.dir, "index.db")}
	cwd := filepath.Join(filepath.Dir(clone), "..", ".worktrees", "widgets", "feat-x")
	r.hostSession("hub", sidHub1, cwd, "feat/x", "2026-11-11T08:00:00Z", "add alpha", "[feat/x "+f[:10]+"] add alpha\n")
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true, Git: true}); err != nil {
		t.Fatal(err)
	}
	rows, err := ix.Query("SELECT kind, text, source FROM outcomes WHERE ref=" + q(f[:10]) + ";")
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", err, rows)
	}
	if sget(rows[0], "kind") != "commit-landed" || !strings.Contains(sget(rows[0], "text"), "on main as "+rebased[:8]) || !strings.Contains(sget(rows[0], "source"), "patch-id") {
		t.Errorf("%v", rows[0])
	}
	pr, err := ix.Query("SELECT patch_id, pr FROM produced WHERE type='commit' AND ref=" + q(f[:10]) + ";")
	if err != nil || len(pr) != 1 || sget(pr[0], "patch_id") == "" {
		t.Errorf("the patch-id is kept with the produced row: %v %v", pr, err)
	}
}

// GitHub's lookup on a fork answers with the parent's PRs: the PR belongs to the
// repository of its base, not the remote that was asked. (Found by a spot-check on the
// real archive: openshift/hypershift#8337 had been recorded as jhjaggars/hypershift#8337.)
func TestResolvePRBelongsToItsBaseRepository(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "remote", "add", "upstream", "https://github.com/me/widgets.git")
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	f := commitFile(t, clone, "e.txt", "e\n", "a change in a parent's PR")
	fakePRs(t, `[{"number":8337,"state":"closed","merged_at":"2026-11-10T10:00:00Z","merge_commit_sha":"abc","base":{"ref":"main","repo":{"full_name":"parent/widgets"}}}]`)
	r := newGit(true).resolve("h", clone, "widgets", nil, f)
	if r.state != "landed" || r.pr != "parent/widgets#8337" || !strings.Contains(r.how, "PR parent/widgets#8337 merged into main") {
		t.Fatalf("%+v", r)
	}
}

// A commit that was never pushed gets GitHub's 422 "No commit found": say that, not
// that GitHub could not be asked.
func TestResolveACommitGitHubNeverHad(t *testing.T) {
	hasGit(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "remote", "add", "upstream", "https://github.com/me/widgets.git")
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	f := commitFile(t, clone, "g.txt", "g\n", "never pushed")
	bin := t.TempDir()
	script := "#!/bin/sh\necho '{\"message\":\"No commit found for SHA: x\",\"status\":\"422\"}'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newGit(true).resolve("h", clone, "widgets", nil, f)
	if r.state != "unknown" || !strings.Contains(r.how, "GitHub has no such commit") {
		t.Fatalf("%+v", r)
	}
}
