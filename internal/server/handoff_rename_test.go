package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// handoffOnce starts a handoff for feat/a, then ends its agent as a
// restart would find it (gone, and past the start grace).
func handoffOnce(t *testing.T) (s *Server, fh *fakeLaunchHerdr, repo, wt string, wts *[]map[string]any) {
	t.Helper()
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	list := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = worktreeHook(t, &list, nil) })
	s = New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot, StateDir: t.TempDir()}, client, nil)
	s.findExec = func(name string) bool { return name == "pi" }
	if w := do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/a","kind":"pi","brief":"A"}`); w.Code != 200 {
		t.Fatalf("handoff = %d %s", w.Code, w.Body)
	}
	wt = filepath.Join(wtRoot, "proj", "feat-a")
	list = append(list, map[string]any{"path": wt, "branch": "feat/a", "is_linked_worktree": true, "label": "feat-a", "open_workspace_id": "w9"})
	s.coord.mu.Lock()
	s.coord.started = map[string]time.Time{}
	s.coord.mu.Unlock()
	fh.set(func(f *fakeLaunchHerdr) { f.agents = nil; f.calls = nil })
	return s, fh, repo, wt, &list
}

func TestHandoffRestartSameBranch(t *testing.T) {
	s, _, repo, wt, _ := handoffOnce(t)
	w := do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/a","kind":"pi","brief":"A again"}`)
	var r struct{ Entry ledger.Entry }
	json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restarted":true`) || r.Entry.Branch != "feat/a" || r.Entry.Worktree != wt {
		t.Fatalf("restart = %d %s", w.Code, w.Body)
	}
	if len(s.ledger.Entries()) != 1 {
		t.Fatalf("entries = %d", len(s.ledger.Entries()))
	}
}

func TestHandoffRestartRenamedBranch(t *testing.T) {
	s, _, repo, wt, wts := handoffOnce(t)
	git(t, wt, "branch", "-m", "feat/4840-a/me")
	(*wts)[1]["branch"] = "feat/4840-a/me"

	w := do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/a","kind":"pi","brief":"A again"}`)
	var r struct{ Entry ledger.Entry }
	json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restarted":true`) {
		t.Fatalf("restart = %d %s", w.Code, w.Body)
	}
	if r.Entry.Branch != "feat/4840-a/me" || r.Entry.Worktree != wt || len(r.Entry.PreviousBranches) != 1 || r.Entry.PreviousBranches[0] != "feat/a" {
		t.Fatalf("entry = %+v", r.Entry)
	}
	if n := len(s.ledger.Entries()); n != 1 {
		t.Fatalf("entries = %d", n)
	}
	if git(t, wt, "symbolic-ref", "--short", "HEAD") != "feat/4840-a/me" {
		t.Fatal("worktree branch changed")
	}

	// Either name finds the entry afterwards.
	s.coord.mu.Lock()
	s.coord.started = map[string]time.Time{}
	s.coord.mu.Unlock()
	for _, b := range []string{"feat/a", "feat/4840-a/me"} {
		s.coord.mu.Lock()
		s.coord.started = map[string]time.Time{}
		s.coord.mu.Unlock()
		if w := do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"`+b+`","kind":"pi","brief":"again"}`); w.Code != 200 || len(s.ledger.Entries()) != 1 {
			t.Fatalf("%s: %d %s (%d entries)", b, w.Code, w.Body, len(s.ledger.Entries()))
		}
	}
}

// Without the ledger's hint, a worktree on another branch at the path a
// branch maps to is a different task and stays refused.
func TestWorkerPathTakenByOtherBranch(t *testing.T) {
	s, fh, repo, wt, wts := handoffOnce(t)
	git(t, wt, "branch", "-m", "other")
	(*wts)[1]["branch"] = "other"
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	code, r := startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"feat/a","kind":"pi"}`)
	if code != http.StatusConflict || r["code"] != "path_taken" {
		t.Fatalf("other branch = %d %v", code, r)
	}
}

// A different, still-live worker owning the worktree blocks the restart.
func TestHandoffRestartPathOwnedByOtherWorker(t *testing.T) {
	s, _, repo, wt, _ := handoffOnce(t)
	if _, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: repo, RepoName: "proj", Branch: "other", Worktree: wt, Status: ledger.Running}); err != nil {
		t.Fatal(err)
	}
	w := do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/a","kind":"pi","brief":"A again"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "path_taken") {
		t.Fatalf("restart = %d %s", w.Code, w.Body)
	}
}

func TestRememberedKinds(t *testing.T) {
	dir := t.TempDir()
	s := &Server{kinds: openKinds(kindsPath(dir))}
	s.findExec = func(name string) bool { return name == "pi" }
	if got := s.launchKinds(nil); strings.Join(got, ",") != "pi" {
		t.Fatalf("kinds = %v", got)
	}
	// pi is no longer found, but a reopened store still remembers it.
	s = &Server{kinds: openKinds(kindsPath(dir))}
	s.findExec = func(string) bool { return false }
	kinds, rem := s.launchKindsDetail(nil)
	if strings.Join(kinds, ",") != "pi" || !rem["pi"] {
		t.Fatalf("remembered = %v %v", kinds, rem)
	}
	// Stale entries expire, and forgetting removes them.
	s.kinds.data["pi"] = time.Now().Add(-kindMemory - time.Hour)
	if got := s.launchKinds(nil); len(got) != 0 {
		t.Fatalf("expired kinds = %v", got)
	}
	s.kinds.data["pi"] = time.Now()
	s.kinds.forget("pi")
	if got := s.launchKinds(nil); len(got) != 0 {
		t.Fatalf("forgotten kinds = %v", got)
	}
	if b, _ := os.ReadFile(kindsPath(dir)); strings.Contains(string(b), "pi") {
		t.Fatalf("kinds.json = %s", b)
	}
}

// A remembered kind that cannot be started fails with a clear error and is
// forgotten.
func TestRememberedKindStartFails(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh.set(func(f *fakeLaunchHerdr) {
		hook := worktreeHook(t, &wts, nil)
		f.hook = func(m string, p map[string]any) (any, *herdr.Error, bool) {
			if m == "agent.start" {
				return nil, &herdr.Error{Code: "spawn_failed", Message: "no such command"}, true
			}
			return hook(m, p)
		}
	})
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot, StateDir: t.TempDir()}, client, nil)
	s.findExec = func(string) bool { return false }
	s.kinds.remember("pi", time.Now())
	code, r := startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"x","kind":"pi"}`)
	if code == 200 || !strings.Contains(r["message"].(string), "worked on this host before") {
		t.Fatalf("start = %d %v", code, r)
	}
	if s.kinds.recalled("pi", time.Now()) {
		t.Fatal("pi still remembered")
	}
	if code, r = startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"y","kind":"pi"}`); r["code"] != "bad_kind" {
		t.Fatalf("after forget = %d %v", code, r)
	}
}
