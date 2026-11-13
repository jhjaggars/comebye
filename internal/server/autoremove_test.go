package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

const arAgent = "desk/w2:p1"

// autoFix is a worker whose branch w holds one commit, landed on origin/main
// (a cherry-pick, so git cherry matches it under another SHA), marked ready
// to remove with a SAFE answer, in a hub with automatic removal on.
func autoFix(t *testing.T) (*Server, landedRepo, ledger.Entry) {
	t.Helper()
	s, r, e := workerFix(t)
	r.landCherryPick(t)
	markSafe(t, s, r)
	return s, r, e
}

// workerFix is landingFix with a herdr that knows the worker's workspace and
// worktrees (so a removal can close it and an undo can start it again), and
// automatic removal on.
func workerFix(t *testing.T) (*Server, landedRepo, ledger.Entry) {
	t.Helper()
	r := newLandedRepo(t)
	own := r.edit(t, r.wt, 3, "the worker's change")
	s, fh, _ := coordServer(t)
	s.cfg.LaunchRoots = []string{filepath.Dir(r.repo)}
	s.cfg.WorktreeRoot = filepath.Join(filepath.Dir(r.repo), "wts")
	var wts []map[string]any
	wh := worktreeHook(t, &wts, nil)
	fh.set(func(f *fakeLaunchHerdr) {
		f.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
			switch method {
			case "workspace.list":
				return map[string]any{"type": "workspace_list", "workspaces": []any{
					map[string]any{"workspace_id": "w2", "label": "proj/w", "pane_count": 1, "worktree": map[string]any{"repo_name": "proj", "repo_root": r.repo, "checkout_path": r.wt, "is_linked_worktree": true, "repo_key": "k"}},
				}}, nil, true
			case "workspace.close", "pane.close":
				kept := f.agents[:0]
				for _, a := range f.agents {
					if a.WorkspaceID != p["workspace_id"] && a.PaneID != p["pane_id"] {
						kept = append(kept, a)
					}
				}
				f.agents = kept
				return map[string]any{"type": "ok"}, nil, true
			}
			return wh(method, p)
		}
		for i := range f.agents {
			f.agents[i].WorkspaceID = "w" + string(f.agents[i].PaneID[1])
		}
	})
	s.autoRemove = true
	s.outbox = openOutbox("")
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: r.repo, RepoName: "proj", Branch: "w", Base: "main", Kind: "pi", AgentID: arAgent, PaneID: "w2:p1", AgentName: "home", WorkspaceID: "w2", Worktree: r.wt, Status: ledger.Running, ReplyTo: &ledger.Ref{AgentID: "desk/w1:p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ledger.Report(arAgent, "## Summary\ndone as "+own, time.Now().Add(-time.Hour)); !ok {
		t.Fatal("report")
	}
	return s, r, e
}

func markSafe(t *testing.T, s *Server, r landedRepo) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := s.ledger.SetMark(ledger.Mark{AgentID: arAgent, Host: "desk", PaneID: "w2:p1", Name: "home", Worktree: r.wt,
		By: "desk/w1:p1", Reason: "landed", Ask: &ledger.Ask{State: ledger.AskSafe, At: now}}); err != nil {
		t.Fatal(err)
	}
}

func setAsk(t *testing.T, s *Server, a *ledger.Ask) {
	t.Helper()
	if _, _, err := s.ledger.Update(entryOf(t, s).ID, func(e *ledger.Entry) { e.Mark.Ask = a }); err != nil {
		t.Fatal(err)
	}
}

func entryOf(t *testing.T, s *Server) ledger.Entry {
	t.Helper()
	e, ok := s.ledger.ByAgent(arAgent)
	if !ok {
		for _, x := range s.ledger.Entries() {
			if x.AgentID == arAgent {
				return x
			}
		}
		t.Fatal("no entry")
	}
	return e
}

// stays asserts that a step removes nothing and the worker is still in the
// review.
func stays(t *testing.T, s *Server, r landedRepo, what string) {
	t.Helper()
	if got := s.autoRemoveStep(context.Background(), time.Now()); len(got) != 0 {
		t.Fatalf("%s: removed %v", what, got)
	}
	e := entryOf(t, s)
	if e.Status != ledger.ReadyToRemove || e.AutoRemoved != nil {
		t.Fatalf("%s: status %s, auto %+v", what, e.Status, e.AutoRemoved)
	}
	if _, err := os.Stat(r.wt); err != nil {
		t.Fatalf("%s: worktree gone: %v", what, err)
	}
	if n := len(s.outbox.pending()); n != 0 {
		t.Fatalf("%s: %d digest lines", what, n)
	}
}

func TestAutoRemoveHappyPath(t *testing.T) {
	s, r, e := autoFix(t)
	// Local main is stale: only origin/main has the work. The worker's own
	// main would not show it.
	git(t, r.repo, "reset", "-q", "--hard", r.base)
	got := s.autoRemoveStep(context.Background(), time.Now())
	if len(got) != 1 || got[0] != arAgent {
		t.Fatalf("removed = %v", got)
	}
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Removed || g.AutoRemoved == nil || !g.AutoRemoved.WorktreeRemoved || g.AutoRemoved.Reason != "landed, clean, SAFE" ||
		!strings.Contains(g.AutoRemoved.Landed, "origin/main") {
		t.Fatalf("entry = %s %+v", g.Status, g.AutoRemoved)
	}
	if _, err := os.Stat(r.wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if git(t, r.repo, "rev-parse", "--verify", "refs/heads/w") == "" {
		t.Fatal("the branch must be kept")
	}
	var audited bool
	for _, a := range s.ledger.AuditLog("") {
		audited = audited || (a.Action == "worker.auto_remove" && a.OK && a.By == AutoCreator && strings.Contains(a.Detail, "landed, clean, SAFE"))
	}
	if !audited {
		t.Fatalf("no audit entry: %+v", s.ledger.AuditLog(""))
	}
	ls := s.outbox.pending()
	if len(ls) != 1 || ls[0].Text != "removed home automatically: landed, clean, SAFE." {
		t.Fatalf("digest = %+v", ls)
	}
	// The review lists it, with an Undo.
	w := do(s, "GET", "/api/removals", userHdr, "")
	var rv struct {
		Removals    []removal
		AutoRemoved []autoRemovedView `json:"auto_removed"`
	}
	json.Unmarshal(w.Body.Bytes(), &rv)
	if len(rv.Removals) != 0 || len(rv.AutoRemoved) != 1 || !rv.AutoRemoved[0].CanUndo || rv.AutoRemoved[0].Branch != "w" {
		t.Fatalf("review = %s", w.Body)
	}
	// ... for a day.
	if got := s.recentAutoRemovals(time.Now().Add(25 * time.Hour)); len(got) != 0 {
		t.Fatalf("still listed after a day: %+v", got)
	}
}

func TestAutoRemoveNoCommitsAndMergedPR(t *testing.T) {
	// A research / Page-only handoff: the branch has no commits of its own.
	s, r, _ := autoFix(t)
	git(t, r.wt, "reset", "-q", "--hard", r.base) // the branch is at the base: no commits of its own
	if got := s.autoRemoveStep(context.Background(), time.Now()); len(got) != 1 {
		t.Fatalf("no-commits worker not removed: %v", got)
	}
	// A squash-merged PR whose head is the branch tip; the branch was never
	// pushed here, so its commit is "on no remote", but the PR is the proof.
	s, r, _ = workerFix(t)
	markSafe(t, s, r)
	old := ghMergedPRs
	t.Cleanup(func() { ghMergedPRs = old })
	tip := git(t, r.wt, "rev-parse", "HEAD")
	ghMergedPRs = func(context.Context, string, string) ([]ghPR, error) {
		return []ghPR{{Number: 7, HeadRefOid: tip}}, nil
	}
	if got := s.autoRemoveStep(context.Background(), time.Now()); len(got) != 1 {
		t.Fatalf("merged-PR worker not removed: %v", got)
	}
	if g := entryOf(t, s); g.AutoRemoved == nil || g.AutoRemoved.Landed != "merged as PR #7" {
		t.Fatalf("landed = %+v", g.AutoRemoved)
	}
}

func TestAutoRemoveGuardsKeepTheWorkerInTheReview(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		s, r, _ := autoFix(t)
		s.autoRemove = false
		stays(t, s, r, "disabled")
	})
	t.Run("not landed", func(t *testing.T) {
		s, r, _ := workerFix(t)
		markSafe(t, s, r)
		stays(t, s, r, "work not in origin/main")
	})
	t.Run("stale local main is not trusted", func(t *testing.T) {
		// The work is in the checkout's own main, which was never pushed.
		s, r, _ := workerFix(t)
		git(t, r.repo, "cherry-pick", "w")
		markSafe(t, s, r)
		stays(t, s, r, "landed on local main only")
	})
	t.Run("a new commit after the landing", func(t *testing.T) {
		s, r, _ := autoFix(t)
		r.edit(t, r.wt, 5, "more work")
		stays(t, s, r, "unmerged commit")
	})
	t.Run("uncommitted file", func(t *testing.T) {
		s, r, _ := autoFix(t)
		os.WriteFile(filepath.Join(r.wt, "f.txt"), []byte("changed\n"), 0o644)
		stays(t, s, r, "modified file")
	})
	t.Run("untracked file", func(t *testing.T) {
		s, r, _ := autoFix(t)
		os.WriteFile(filepath.Join(r.wt, "notes.txt"), []byte("x"), 0o644)
		stays(t, s, r, "untracked file")
	})
	t.Run("rebase in progress", func(t *testing.T) {
		s, r, _ := autoFix(t)
		_, gd, _, _ := gitDirs(context.Background(), r.wt)
		os.MkdirAll(filepath.Join(gd, "rebase-merge"), 0o755)
		stays(t, s, r, "rebase")
	})
	for name, ask := range map[string]*ledger.Ask{
		"did not answer": {State: ledger.AskNoAnswer},
		"unsafe":         {State: ledger.AskUnsafe, Reason: "a PR is open"},
		"unclear":        {State: ledger.AskUnclear},
		"asking":         {State: ledger.AskAsking},
		"not asked":      {State: ledger.AskNotAsked},
		"unchecked":      nil,
	} {
		t.Run("ask "+name, func(t *testing.T) {
			s, r, _ := autoFix(t)
			setAsk(t, s, ask)
			stays(t, s, r, name)
		})
	}
	t.Run("discard proposal", func(t *testing.T) {
		s, r, e := autoFix(t)
		s.ledger.Update(e.ID, func(x *ledger.Entry) { x.Mark.Discard = true })
		stays(t, s, r, "discard")
	})
	t.Run("open to-do waiting on it", func(t *testing.T) {
		s, r, _ := autoFix(t)
		if _, _, err := s.todos.add(Todo{Question: "ship it?", CreatedBy: "x", Waiting: &TodoAgent{ID: arAgent, Name: "home"}}); err != nil {
			t.Fatal(err)
		}
		stays(t, s, r, "to-do")
	})
	t.Run("queued prompt", func(t *testing.T) {
		s, r, _ := autoFix(t)
		if _, _, err := s.pq.add(QueuedPrompt{Agent: arAgent, Name: "home", Text: "next", From: "user"}); err != nil {
			t.Fatal(err)
		}
		stays(t, s, r, "queued prompt")
	})
	t.Run("coordinator", func(t *testing.T) {
		s, r, _ := autoFix(t)
		if err := s.recordRole(arAgent, "home", RoleCoordinator, "test"); err != nil {
			t.Fatal(err)
		}
		stays(t, s, r, "coordinator")
	})
	t.Run("no worktree of its own", func(t *testing.T) {
		s, r, e := autoFix(t)
		s.ledger.Update(e.ID, func(x *ledger.Entry) { x.Worktree = "" })
		s.ledger.Update(e.ID, func(x *ledger.Entry) { x.Mark.Worktree = "" })
		stays(t, s, r, "no worktree")
	})
	t.Run("a mark withdrawn during the checks", func(t *testing.T) {
		s, r, _ := autoFix(t)
		it := s.marked()[0]
		s.ledger.ClearMark(arAgent)
		if s.autoRemoveOne(context.Background(), it) {
			t.Fatal("removed an unmarked worker")
		}
		if _, err := os.Stat(r.wt); err != nil {
			t.Fatal(err)
		}
	})
}

// The verdict's rules that depend on live state, on the removal as the
// review fills it in.
func TestAutoRemoveVerdict(t *testing.T) {
	clean := func() *removal {
		return &removal{
			ID: arAgent, EntryID: "e1", Worktree: "/wt",
			Mark: ledger.Mark{Ask: &ledger.Ask{State: ledger.AskSafe}},
			Checks: &worktreeChecks{Exists: true, Unmerged: 0, Detail: &worktreeDetail{},
				Origin: &originCheck{Ref: "origin/main", Fetched: true}},
		}
	}
	if ok, why := autoRemoveVerdict(clean(), autoFacts{}); !ok {
		t.Fatalf("clean = %q", why)
	}
	cases := map[string]func(it *removal, f *autoFacts){
		"working":                   func(it *removal, _ *autoFacts) { it.Live = liveInfo{Present: true, Status: herdr.StatusWorking} },
		"blocked":                   func(it *removal, _ *autoFacts) { it.Live = liveInfo{Present: true, Status: herdr.StatusBlocked} },
		"background task":           func(_ *removal, f *autoFacts) { f.Background = 1 },
		"to-do":                     func(_ *removal, f *autoFacts) { f.TodoWaiting = true },
		"queued":                    func(_ *removal, f *autoFacts) { f.Queued = 2 },
		"coordinator":               func(it *removal, _ *autoFacts) { it.Coordinator = true },
		"no entry":                  func(it *removal, _ *autoFacts) { it.EntryID = "" },
		"re-asking":                 func(it *removal, _ *autoFacts) { n := time.Now(); it.Mark.Ask.ReaskedAt = &n },
		"warning":                   func(it *removal, _ *autoFacts) { it.Warnings = []string{"pages are recorded"} },
		"checks failed":             func(it *removal, _ *autoFacts) { it.ChecksError = "boom" },
		"primary":                   func(it *removal, _ *autoFacts) { it.Checks.Primary = true },
		"dirty":                     func(it *removal, _ *autoFacts) { it.Checks.Dirty, it.Checks.DirtyFiles = true, 1 },
		"peer without origin check": func(it *removal, _ *autoFacts) { it.Checks.Origin = nil },
		"fetch failed":              func(it *removal, _ *autoFacts) { it.Checks.Origin = &originCheck{Why: "offline", Unmerged: -1} },
		"unknown merge":             func(it *removal, _ *autoFacts) { it.Checks.Origin.Unmerged = -1 },
		"unmerged":                  func(it *removal, _ *autoFacts) { it.Checks.Origin.Unmerged = 2 },
		"unpushed":                  func(it *removal, _ *autoFacts) { it.Checks.Detail.Unpushed = 1 },
		"unpushed unknown":          func(it *removal, _ *autoFacts) { it.Checks.Detail.Unpushed = -1 },
		"no detail":                 func(it *removal, _ *autoFacts) { it.Checks.Detail = nil },
	}
	for name, mutate := range cases {
		it, f := clean(), autoFacts{}
		mutate(it, &f)
		if ok, _ := autoRemoveVerdict(it, f); ok {
			t.Errorf("%s: judged clean", name)
		}
	}
	// A merged PR vouches for commits that look unpushed.
	it := clean()
	it.Checks.Origin.Unmerged, it.Checks.Origin.MergedPR, it.Checks.Detail.Unpushed = 2, 9, 2
	if ok, why := autoRemoveVerdict(it, autoFacts{}); !ok {
		t.Fatalf("merged PR = %q", why)
	}
}

func TestUndoAutoRemove(t *testing.T) {
	s, _, e := autoFix(t)
	if got := s.autoRemoveStep(context.Background(), time.Now()); len(got) != 1 {
		t.Fatalf("not removed: %v", got)
	}
	if w := do(s, "POST", "/api/removals/undo", userHdr, `{"entry_id":"nope"}`); w.Code != 404 {
		t.Fatalf("unknown = %d", w.Code)
	}
	w := do(s, "POST", "/api/removals/undo", userHdr, `{"entry_id":"`+e.ID+`"}`)
	if w.Code != 200 {
		t.Fatalf("undo = %d %s", w.Code, w.Body)
	}
	var out struct{ Worktree string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if git(t, out.Worktree, "symbolic-ref", "--short", "HEAD") != "w" {
		t.Fatalf("worktree not recreated on its branch: %s", w.Body)
	}
	g, _ := s.ledger.Get(e.ID)
	if g.AutoRemoved.UndoneAt == nil || !strings.Contains(g.AutoRemoved.UndoNote, "new worker started") {
		t.Fatalf("not recorded: %+v", g.AutoRemoved)
	}
	if w := do(s, "POST", "/api/removals/undo", userHdr, `{"entry_id":"`+e.ID+`"}`); w.Code != 409 {
		t.Fatalf("twice = %d", w.Code)
	}
	if w := do(s, "POST", "/api/removals/undo", agentHdr("w1:p1"), `{"entry_id":"`+e.ID+`"}`); w.Code != 403 {
		t.Fatalf("agent undo = %d", w.Code)
	}
}
