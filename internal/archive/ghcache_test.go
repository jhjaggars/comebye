package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gh that runs the given shell body after logging its arguments.
func fakeGHBody(t *testing.T, body string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$*\" in\n*\"pr list\"*) echo '[]'; exit 0;; esac\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const mergedPR = `[{"number":12,"state":"closed","merged_at":"2026-11-10T10:00:00Z","merge_commit_sha":"abc","base":{"ref":"main","repo":{"full_name":"acme/widgets"}}}]`
const openPR = `[{"number":40,"state":"open","merged_at":"","merge_commit_sha":"","base":{"ref":"main","repo":{"full_name":"acme/widgets"}}}]`

// next is a later pass: a new context holding what the earlier ones kept.
func next(prev *gitCtx) *gitCtx {
	g := newGit(true)
	for k, e := range prev.prCache {
		g.prCache[k] = e
	}
	return g
}

func agePRs(g *gitCtx, d time.Duration) {
	for k, e := range g.prCache {
		e.at = e.at.Add(-d)
		g.prCache[k] = e
	}
}

func prRepo(t *testing.T) (clone, sha string) {
	t.Helper()
	hasGit(t)
	_, clone = gitWorld(t)
	gitDo(t, clone, "remote", "add", "upstream", "https://github.com/acme/widgets.git")
	gitDo(t, clone, "checkout", "-q", "-b", "feat")
	return clone, commitFile(t, clone, "k.txt", "k\n", "a change")
}

// A merged PR does not change: it is kept for good, however old.
func TestPRCacheKeepsMergedForever(t *testing.T) {
	clone, sha := prRepo(t)
	log := fakeGHBody(t, "echo '"+mergedPR+"'")
	g1 := newGit(true)
	if r := g1.resolve("h", clone, "widgets", nil, sha); r.state != "landed" || r.pr != "acme/widgets#12" {
		t.Fatalf("%+v", r)
	}
	agePRs(g1, 400*24*time.Hour)
	g2 := next(g1)
	if r := g2.resolve("h", clone, "widgets", nil, sha); r.state != "landed" || r.pr != "acme/widgets#12" {
		t.Fatalf("from the cache: %+v", r)
	}
	if n := ghCalls(t, log); n != 1 || g2.ghCalls != 0 {
		t.Errorf("asked %d times, and the second pass counted %d against its limit", n, g2.ghCalls)
	}
}

// "No PR" is believed for a day, then asked again.
func TestPRCacheRetriesNoPRAfterADay(t *testing.T) {
	clone, sha := prRepo(t)
	log := fakeGHBody(t, "echo '[]'")
	g1 := newGit(true)
	g1.resolve("h", clone, "widgets", nil, sha)
	agePRs(g1, 23*time.Hour)
	g2 := next(g1)
	if r := g2.resolve("h", clone, "widgets", nil, sha); r.state != "unknown" || !strings.Contains(r.how, "GitHub names no PR") {
		t.Fatalf("%+v", r)
	}
	if ghCalls(t, log) != 1 {
		t.Error("asked again within a day")
	}
	agePRs(g2, 2*time.Hour)
	g3 := next(g2)
	g3.resolve("h", clone, "widgets", nil, sha)
	if ghCalls(t, log) != 2 {
		t.Errorf("not asked again after a day: %d", ghCalls(t, log))
	}
}

// An open PR is believed for a day and then asked again, because it may be squash-merged
// meanwhile, which only GitHub can say.
func TestPRCacheRetriesAnOpenPRAfterADay(t *testing.T) {
	clone, sha := prRepo(t)
	log := fakeGHBody(t, "echo '"+openPR+"'")
	g1 := newGit(true)
	if r := g1.resolve("h", clone, "widgets", nil, sha); r.state != "notinmain" {
		t.Fatalf("%+v", r)
	}
	g2 := next(g1)
	g2.resolve("h", clone, "widgets", nil, sha)
	if ghCalls(t, log) != 1 {
		t.Error("asked again at once")
	}
	agePRs(g2, 25*time.Hour)
	if r := next(g2).resolve("h", clone, "widgets", nil, sha); r.state != "notinmain" || ghCalls(t, log) != 2 {
		t.Errorf("%+v, %d calls", r, ghCalls(t, log))
	}
}

// A commit GitHub never had is believed until a remote-tracking ref contains it.
func TestPRCacheKeepsNeverPushedUntilItIsOnARemote(t *testing.T) {
	clone, sha := prRepo(t)
	log := fakeGHBody(t, `echo '{"message":"No commit found for SHA: x","status":"422"}'; exit 1`)
	g1 := newGit(true)
	if r := g1.resolve("h", clone, "widgets", nil, sha); r.state != "unknown" || !strings.Contains(r.how, "GitHub has no such commit") {
		t.Fatalf("%+v", r)
	}
	agePRs(g1, 60*24*time.Hour) // however long ago
	g2 := next(g1)
	if r := g2.resolve("h", clone, "widgets", nil, sha); !strings.Contains(r.how, "GitHub has no such commit") {
		t.Fatalf("%+v", r)
	}
	if ghCalls(t, log) != 1 {
		t.Errorf("asked again with the commit still local: %d", ghCalls(t, log))
	}
	gitDo(t, clone, "push", "-q", "origin", "feat") // now a remote-tracking ref holds it
	next(g2).resolve("h", clone, "widgets", nil, sha)
	if ghCalls(t, log) != 2 {
		t.Errorf("not asked again once the commit was on a remote: %d", ghCalls(t, log))
	}
}

// An error that is not an answer (the network, a rate limit) is not kept.
func TestPRCacheDoesNotKeepErrors(t *testing.T) {
	clone, sha := prRepo(t)
	log := fakeGHBody(t, "echo 'rate limited' >&2; exit 1")
	g := newGit(true)
	g.resolve("h", clone, "widgets", nil, sha)
	if len(g.prNew) != 0 {
		t.Errorf("kept %v", g.prNew)
	}
	next(g).resolve("h", clone, "widgets", nil, sha)
	if ghCalls(t, log) != 2 {
		t.Errorf("an error was believed: %d", ghCalls(t, log))
	}
}

// Through the index: the first pass asks GitHub and writes the answer, a second pass
// reads it and asks nothing, and the verdict is the same.
func TestIndexKeepsGitHubAnswers(t *testing.T) {
	hasSqlite(t)
	_, clone := gitWorld(t)
	gitDo(t, clone, "remote", "add", "upstream", "https://github.com/acme/widgets.git")
	gitDo(t, clone, "checkout", "-q", "-b", "feat/x")
	a := commitFile(t, clone, "c.txt", "one\n", "first step")
	commitFile(t, clone, "c.txt", "one\ntwo\n", "second step")
	gitDo(t, clone, "checkout", "-q", "main")
	gitDo(t, clone, "merge", "-q", "--squash", "feat/x")
	gitDo(t, clone, "commit", "-q", "-m", "Add c (#12)")
	gitDo(t, clone, "push", "-q", "origin", "main")
	log := fakeGHBody(t, "echo '"+mergedPR+"'")

	r := newRig(t)
	ix := &Index{Path: filepath.Join(r.dir, "index.db")}
	cwd := filepath.Join(filepath.Dir(clone), "..", ".worktrees", "widgets", "feat-x")
	r.hostSession("hub", sidHub1, cwd, "feat/x", "2026-11-11T08:00:00Z", "add c", "[feat/x "+a[:10]+"] first step\n")
	for pass := 1; pass <= 2; pass++ {
		if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true, Git: true, GH: true}); err != nil {
			t.Fatal(err)
		}
		rows, err := ix.Query("SELECT kind, source FROM outcomes WHERE ref=" + q(a[:10]) + ";")
		if err != nil || len(rows) != 1 || sget(rows[0], "kind") != "commit-landed" || !strings.Contains(sget(rows[0], "source"), "PR acme/widgets#12") {
			t.Fatalf("pass %d: %v %v", pass, rows, err)
		}
		if n := ghCalls(t, log); n != 1 {
			t.Fatalf("after pass %d GitHub had been asked %d times, want 1", pass, n)
		}
	}
	if rows, _ := ix.Query("SELECT count(*) AS n FROM gh_commit_prs;"); num(rows[0], "n") != 1 {
		t.Errorf("%v", rows)
	}
}
