package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/scratch"
)

// The path, branch, issue and followup items (askitems.go, askcheck.go).

func TestParseTheNewItemTypes(t *testing.T) {
	_, _, items := parseReply("LEFT\n" +
		"- path /tmp/fetch-cache on desk the download cache\n" +
		"- path `/home/u/.cache/thing`\n" +
		"- branch origin/feat/x-polish the pre-rebase backup\n" +
		"- branch feat/y\n" +
		"- branch upstream/feat/z mine on the fork\n" +
		"- issue octo/widgets#78 tick the box once it lands\n" +
		"- issue https://github.com/octo/widgets/issues/9\n" +
		"- followup post the summary to the ticket\n" +
		"- path relative/dir\n")
	want := []ledger.AskItem{
		{Type: ledger.ItemPath, Name: "/tmp/fetch-cache", Host: "desk", Text: "the download cache"},
		{Type: ledger.ItemPath, Name: "/home/u/.cache/thing"},
		{Type: ledger.ItemBranch, Remote: "origin", Branch: "feat/x-polish", Text: "the pre-rebase backup"},
		{Type: ledger.ItemBranch, Remote: "origin", Branch: "feat/y"},
		{Type: ledger.ItemBranch, Remote: "upstream", Branch: "feat/z", Text: "mine on the fork"},
		{Type: ledger.ItemIssue, Repo: "octo/widgets", Number: 78, Text: "tick the box once it lands"},
		{Type: ledger.ItemIssue, Repo: "octo/widgets", Number: 9},
		{Type: ledger.ItemFollowup, Text: "post the summary to the ticket"},
		{Type: ledger.ItemOther, Text: "path relative/dir"},
	}
	if len(items) != len(want) {
		t.Fatalf("items = %+v", items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("item %d = %+v; want %+v", i, items[i], want[i])
		}
	}
	for _, typ := range []string{"- path ", "- branch ", "- issue ", "- followup "} {
		if !strings.Contains(askFormat, typ) {
			t.Errorf("the ask's format does not list %q", typ)
		}
	}
}

// Leftover lines like the ones marked sessions gave on 2026-11-21, reworded so
// they name no project: what each is re-typed as.
func TestRetypeOldAnswers(t *testing.T) {
	type want struct {
		typ, name, branch, repo string
		number                  int
	}
	for _, c := range []struct {
		was, text string
		want      want
	}{
		// paths
		{"cleanup", "Delete /tmp/jfetch2952 (the fetch scratch copy)", want{typ: "path", name: "/tmp/jfetch2952"}},
		{"cleanup", "rm -rf /tmp/claude-1000/proj-x/scratchpad/build", want{typ: "path", name: "/tmp/claude-1000/proj-x/scratchpad/build"}},
		{"other", "the throwaway herdr socket at `/tmp/herdr-test.sock` is still there", want{typ: "path", name: "/tmp/herdr-test.sock"}},
		{"cleanup", "remove the cloned repo in /home/u/scratch/widgets-clone.", want{typ: "path", name: "/home/u/scratch/widgets-clone"}},
		{"cleanup", "Delete the screenshots dir /tmp/shots-ab12/", want{typ: "path", name: "/tmp/shots-ab12"}},
		// remote branches
		{"cleanup", "Delete the backup branch `origin/feat/wake-4-polish`; it is in main already", want{typ: "cleanup"}}, // two steps
		{"cleanup", "Delete the backup branch `origin/feat/wake-4-polish`. It is already in main.", want{typ: "branch", branch: "feat/wake-4-polish"}},
		{"cleanup", "delete origin/fix/header-pre-rebase (already in origin/main)", want{typ: "branch", branch: "fix/header-pre-rebase"}},
		{"other", "remove the remote branch upstream/spike/cache", want{typ: "branch", branch: "spike/cache"}},
		{"cleanup", "origin/feat/x is pushed", want{typ: "cleanup"}}, // no deletion
		// issues
		{"cleanup", "tick the box on issue octo/widgets#78 once it lands", want{typ: "issue", repo: "octo/widgets", number: 78}},
		{"other", "close issue https://github.com/octo/widgets/issues/12", want{typ: "issue", repo: "octo/widgets", number: 12}},
		// pids
		{"cleanup", "stop the dev server I started (pid 4242)", want{typ: "resource", name: "4242"}},
		{"cleanup", "Stop the dev server I started (pid 4242), then close my PR.", want{typ: "cleanup"}}, // two steps
		{"cleanup", "kill pid 100 and pid 200", want{typ: "cleanup"}},                                    // two things
		// commits and PRs
		{"other", "Local branch 2 commits ahead of origin", want{typ: "commits"}},
		{"other", "watch PR #4464 until it merges", want{typ: "other"}}, // no repository: nothing to check
		{"cleanup", "Delete the remote branch `feat/own` on origin. PR 38 is closed, but it holds the pre-rebase commits.", want{typ: "branch", branch: "feat/own"}},
		{"cleanup", "git branch -D feat/own", want{typ: "cleanup"}}, // the local branch: settled by the removal
		{"other", "waiting for review on octo/widgets/pull/31", want{typ: "pr", repo: "octo/widgets", number: 31}},
		// the user's to do
		{"other", "Post the Jira comment; waiting on the user", want{typ: "followup"}},
		{"other", "the user has to run tf-apply in the prod account", want{typ: "followup"}},
		{"cleanup", "try the browser login: only the user can, it needs their key", want{typ: "followup"}},
		{"other", "the user needs to delete /tmp/keys-copy by hand", want{typ: "followup"}},
		// still free text
		{"other", "a git stash entry with the old experiment", want{typ: "other"}},
		{"other", "an open Chrome tab on the dashboard", want{typ: "other"}},
		{"decision", "delete /tmp/x if the user says so", want{typ: "decision"}}, // only cleanup and other are re-typed
	} {
		got := retype(ledger.AskItem{Type: c.was, Text: c.text}, "feat/own")
		w := c.want
		if got.Type != w.typ || (w.name != "" && got.Name != w.name) || (w.branch != "" && got.Branch != w.branch) || got.Repo != w.repo || got.Number != w.number {
			t.Errorf("%q = %+v; want %+v", c.text, got, w)
			continue
		}
		if got.Type != c.was && (got.Was != c.was || got.Text != c.text) {
			t.Errorf("%q lost its words or old type: %+v", c.text, got)
		}
		if got.Type == "resource" && got.Kind != "pid" {
			t.Errorf("%q = kind %q", c.text, got.Kind)
		}
	}
}

func stubIssues(t *testing.T, states map[int]*probeResult) {
	t.Helper()
	old := probeGHIssue
	t.Cleanup(func() { probeGHIssue = old })
	probeGHIssue = func(_ context.Context, _ string, n int) (*probeResult, error) {
		if r, ok := states[n]; ok {
			return r, nil
		}
		return nil, context.DeadlineExceeded
	}
}

// probeRoots makes home and tmp the only directories a path probe looks in.
func probeRoots(t *testing.T) (home, tmp string) {
	t.Helper()
	home, tmp = t.TempDir(), t.TempDir()
	old := pathProbeRoots
	pathProbeRoots = func() []string { return []string{home, tmp} }
	t.Cleanup(func() { pathProbeRoots = old })
	return home, tmp
}

func TestRecheckPathIssueAndFollowup(t *testing.T) {
	s, _, _ := coordServer(t)
	s.probeTTL = 0
	home, tmp := probeRoots(t)
	s.cfg.ScratchDir = tmp
	stubIssues(t, map[int]*probeResult{78: {State: "closed", At: agoT(time.Hour)}, 79: {State: "open"}})
	there := filepath.Join(home, "clone")
	if err := os.Mkdir(there, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(home, "worktrees", "proj", "x")
	it := &removal{ID: "desk/w2:p1", Host: "desk", Worktree: wt, Branch: "feat/x", Base: "main"}
	pad := filepath.Join(s.scratchOpts(false).ClaudeRoot(), scratch.ProjectDir(wt), "sess", "scratchpad")
	for _, c := range []struct {
		item       ledger.AskItem
		state, now string
	}{
		{ledger.AskItem{Type: "path", Name: there}, itemOpen, "is still there (a directory)"},
		{ledger.AskItem{Type: "path", Name: filepath.Join(home, "gone")}, itemResolved, "is gone"},
		{ledger.AskItem{Type: "path", Name: filepath.Join(pad, "build")}, itemResolved, "scratchpad: removing the session deletes it"},
		{ledger.AskItem{Type: "path", Name: filepath.Join(wt, "out")}, itemResolved, "worktree: removing the session deletes it"},
		{ledger.AskItem{Type: "path", Name: "/etc/passwd"}, itemUnchecked, "home and temporary"},
		{ledger.AskItem{Type: "path", Name: there, Host: "elsewhere"}, itemUnchecked, "not in this hub's fleet"},
		{ledger.AskItem{Type: "issue", Repo: "o/r", Number: 78}, itemResolved, "was closed 1 h ago"},
		{ledger.AskItem{Type: "issue", Repo: "o/r", Number: 79}, itemOpen, "is still open"},
		{ledger.AskItem{Type: "issue", Number: 79}, itemUnchecked, "names no repository"},
		{ledger.AskItem{Type: "followup", Text: "post the comment"}, itemResolved, "yours to do, not the session's: it is kept on the removal"},
		{ledger.AskItem{Type: "branch", Remote: "upstream", Branch: "feat/x"}, itemUnchecked, "only at origin"},
	} {
		st, now, _ := s.recheck(context.Background(), it, c.item)
		if st != c.state || !strings.Contains(now, c.now) {
			t.Errorf("%+v = %s %q; want %s ~%q", c.item, st, now, c.state, c.now)
		}
	}
}

func TestPathProbeOnlyReads(t *testing.T) {
	s, _, _ := coordServer(t)
	home, tmp := probeRoots(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink in home that leads out of it.
	if err := os.Symlink(outside, filepath.Join(home, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, code, body string }{
		{filepath.Join(tmp, "f"), "200", `"present"`},
		{filepath.Join(tmp, "nope", "deeper"), "200", `"absent"`},
		{filepath.Join(home, "out"), "200", "a symlink"}, // looked at, not followed
		{filepath.Join(home, "out", "secret"), "400", "symlink"},
		{"/etc/../" + strings.TrimPrefix(tmp, "/") + "/f", "400", ".."},
		{tmp + "/../f", "400", ".."},
		{"/etc/passwd", "400", "home and temporary"},
		{"relative/f", "400", "absolute"},
		{home, "400", "home and temporary"}, // a root itself is not a leftover
	} {
		w := do(s, "GET", "/api/probe?kind=path&path="+c.path, userHdr, "")
		if got := w.Code; (c.code == "200") != (got == 200) || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s = %d %s; want %s ~%s", c.path, got, w.Body, c.code, c.body)
		}
	}
}

// A stored answer of old "cleanup" lines is re-typed when the review is built:
// a path in the session's scratchpad settles, and a branch item naming the
// session's own backup, provably in main, is one the sweep may delete. The loop
// deletes it, settles the answer, and the removal can go ahead.
func TestSweepSettlesARetypedPathAndBranch(t *testing.T) {
	f := newGoneFix(t, true, "")
	s := f.s
	s.cfg.ScratchDir = t.TempDir()
	pad := filepath.Join(s.scratchOpts(false).ClaudeRoot(), scratch.ProjectDir(f.r.wt), "s1", "scratchpad", "out")
	items := []ledger.AskItem{
		{Type: ledger.ItemCleanup, Text: "delete " + pad},
		{Type: ledger.ItemBranch, Remote: "origin", Branch: "w", Text: "the pre-rebase backup"},
	}
	if _, err := s.ledger.UpdateAsk(f.m.AgentID, f.m.At, func(k *ledger.Ask) bool { k.Items = items; return true }); err != nil {
		t.Fatal(err)
	}
	it := reviewOf(t, s, goneID)
	ar := it.AskReview
	if ar == nil || len(ar.Items) != 2 || ar.Items[0].Type != ledger.ItemPath || ar.Items[0].Was != ledger.ItemCleanup || ar.Items[0].State != itemResolved ||
		!ar.Items[1].Stale || !ar.StaleOnly {
		t.Fatalf("review = %+v", ar)
	}
	n, deleted := s.leftovers(ctx(), time.Now(), true)
	if n != 1 || len(deleted) != 1 || deleted[0].Branch != "w" || remoteHas(t, f.r, "w") {
		t.Fatalf("settled %d deleted %+v", n, deleted)
	}
	a := f.ask(t)
	if a.State != ledger.AskSafe {
		t.Fatalf("ask = %+v", a)
	}
	// The stored items were never re-typed: the review's copy was.
	if e, _ := s.ledger.Get(f.e.ID); e.Mark != nil && len(e.Mark.Ask.Items) > 0 && e.Mark.Ask.Items[0].Type != ledger.ItemCleanup {
		t.Fatalf("stored items changed: %+v", e.Mark.Ask.Items)
	}
}

// A branch the session listed as its own beside its own branch (the copy it
// pushed to land from) is held to the own branch's proof: fully in main with no
// open PR, the sweep deletes it, keeping a backup ref; a listed branch that was
// never pushed is resolved; and the session's answer settles.
func TestListedExtraBranchFullyInMainIsDeleted(t *testing.T) {
	f := newGoneFix(t, true, "")
	s := f.s
	git(t, f.r.repo, "push", "-q", "origin", "main:refs/heads/w-land")
	tip := git(t, f.r.repo, "rev-parse", "main")
	items := []ledger.AskItem{
		{Type: ledger.ItemBranch, Remote: "origin", Branch: "w-land", Text: "the rebased series I landed from; all in main"},
		{Type: ledger.ItemBranch, Remote: "origin", Branch: "w-never-pushed"},
	}
	if _, err := s.ledger.UpdateAsk(f.m.AgentID, f.m.At, func(k *ledger.Ask) bool { k.Items = items; return true }); err != nil {
		t.Fatal(err)
	}
	ar := reviewOf(t, s, goneID).AskReview
	if ar == nil || len(ar.Items) != 2 || !ar.StaleOnly {
		t.Fatalf("review = %+v", ar)
	}
	if v := ar.Items[0]; v.State != itemOpen || !v.Stale || !strings.Contains(v.Now, "safe to delete") {
		t.Errorf("listed = %+v", v)
	}
	if v := ar.Items[1]; v.State != itemResolved || !strings.Contains(v.Now, "no longer has") {
		t.Errorf("never pushed = %+v", v)
	}
	n, deleted := s.leftovers(ctx(), time.Now(), true)
	if n != 1 || len(deleted) != 1 || deleted[0].Branch != "w-land" || deleted[0].Tip != tip || deleted[0].Backup != "refs/comebye/backup/w-land" {
		t.Fatalf("settled %d deleted %+v", n, deleted)
	}
	if remoteHas(t, f.r, "w-land") {
		t.Fatal("the proven listed branch is still on origin")
	}
	if got := git(t, f.r.repo, "rev-parse", "refs/comebye/backup/w-land"); got != tip {
		t.Fatalf("backup = %s, want %s", got, tip)
	}
	if a := f.ask(t); a.State != ledger.AskSafe || a.Loop == nil || !strings.Contains(a.Loop.Text, "deleted origin/w-land") {
		t.Fatalf("ask = %+v", a)
	}
	var audited bool
	for _, a := range s.ledger.AuditLog("") {
		audited = audited || (a.Action == "worker.stale_branch_delete" && strings.Contains(a.Detail, "origin/w-land"))
	}
	if !audited {
		t.Fatal("the deletion was not audited")
	}
}

// A listed branch is never deleted when it has a commit main lacks, when another
// session on the ledger works on a branch of that name, or when the hub re-typed
// the item from the session's words (it did not list it as a branch itself).
func TestListedBranchTheHubCannotProveIsKept(t *testing.T) {
	f := newGoneFix(t, true, "")
	s := f.s
	// feat/other: somebody else's (a running session works on it), all in main.
	git(t, f.r.repo, "push", "-q", "origin", "main:refs/heads/feat/other")
	if _, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: f.r.repo, RepoName: "proj", Branch: "feat/other", AgentID: "desk/w7:p1", PaneID: "w7:p1", AgentName: "other", Status: ledger.Running}); err != nil {
		t.Fatal(err)
	}
	// w-extra: listed, with a commit main lacks.
	git(t, f.r.repo, "branch", "w-extra", "main")
	git(t, f.r.repo, "checkout", "-q", "w-extra")
	f.r.edit(t, f.r.repo, 20, "work only here")
	git(t, f.r.repo, "push", "-q", "origin", "w-extra")
	git(t, f.r.repo, "checkout", "-q", "main")
	items := []ledger.AskItem{
		{Type: ledger.ItemBranch, Remote: "origin", Branch: "feat/other"},
		{Type: ledger.ItemBranch, Remote: "origin", Branch: "w-extra"},
	}
	if _, err := s.ledger.UpdateAsk(f.m.AgentID, f.m.At, func(k *ledger.Ask) bool { k.Items = items; return true }); err != nil {
		t.Fatal(err)
	}
	ar := reviewOf(t, s, goneID).AskReview
	if ar == nil || len(ar.Items) != 2 || ar.StaleOnly {
		t.Fatalf("review = %+v", ar)
	}
	if v := ar.Items[0]; v.State != itemOpen || v.Stale || !strings.Contains(v.Now, "still there") {
		t.Errorf("other = %+v", v)
	}
	if v := ar.Items[1]; v.State != itemOpen || v.Stale || !strings.Contains(v.Now, "is kept") {
		t.Errorf("unmerged = %+v", v)
	}
	if _, deleted := s.leftovers(ctx(), time.Now(), true); len(deleted) != 0 || !remoteHas(t, f.r, "feat/other") || !remoteHas(t, f.r, "w-extra") {
		t.Fatalf("deleted %+v", deleted)
	}
	// Re-typed from words: not listed as a branch by the session.
	it := &removal{EntryID: f.e.ID, Branch: "w"}
	if s.listedOwn(it, ledger.AskItem{Type: ledger.ItemBranch, Branch: "w-x", Was: ledger.ItemCleanup}, "w-x") {
		t.Error("a re-typed item counts as listed")
	}
	if s.listedOwn(it, ledger.AskItem{Type: ledger.ItemBranch, Branch: "main"}, "main") {
		t.Error("the default branch counts as listed")
	}
}

// "Let it clean up" still covers a cleanup line the hub re-typed.
func TestRetypedCleanupIsStillCleanup(t *testing.T) {
	ar := &askReview{State: ledger.AskUnsafe, OnlyCleanup: true, Items: []itemView{
		{AskItem: retype(ledger.AskItem{Type: ledger.ItemCleanup, Text: "stop the dev server (pid 4242)"}, "w"), State: itemOpen},
	}}
	got, why := cleanupItems(ar)
	if why != "" || len(got) != 1 || got[0] != "stop the dev server (pid 4242)" {
		t.Fatalf("cleanup items = %v %q", got, why)
	}
}
