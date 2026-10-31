package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// gitEnv isolates git from the user's configuration (signing, hooks,
// default branch) for the test and for the server's own git calls.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// workerTree builds a launch root holding a repository "proj" (one commit
// on main) and an empty worktree root, both resolved.
func workerTree(t *testing.T) (root, repo, wtRoot string) {
	t.Helper()
	gitEnv(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root, wtRoot = filepath.Join(base, "code"), filepath.Join(base, "worktrees")
	repo = filepath.Join(root, "proj")
	for _, d := range []string{repo + "/sub", wtRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(repo+"/a.txt", []byte("a\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "first")
	return root, repo, wtRoot
}

func newWorkerServer(t *testing.T, client *herdr.Client, root, wtRoot string) *Server {
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot}, client, nil)
	s.findExec = func(name string) bool { return name == "pi" || name == "claude" }
	return s
}

// worktreeHook answers worktree.list from wts and worktree.create/open as
// herdr would (workspace w9 with root pane w9:p1), creating the worktree
// with git on create.
func worktreeHook(t *testing.T, wts *[]map[string]any, panes []map[string]any) func(string, map[string]any) (any, *herdr.Error, bool) {
	return func(method string, p map[string]any) (any, *herdr.Error, bool) {
		ws := map[string]any{"workspace_id": "w9", "number": 9, "label": p["label"], "focused": false, "pane_count": 1, "tab_count": 1, "active_tab_id": "w9:t1", "agent_status": "unknown"}
		root := map[string]any{"pane_id": "w9:p1", "terminal_id": "t1", "workspace_id": "w9", "tab_id": "w9:t1", "focused": true, "agent_status": "unknown", "revision": 0}
		switch method {
		case "worktree.list":
			return map[string]any{"type": "worktree_list", "source": map[string]any{}, "worktrees": *wts}, nil, true
		case "worktree.create":
			args := []string{"-C", p["cwd"].(string), "worktree", "add", "-q"}
			if base, ok := p["base"].(string); ok {
				args = append(args, "-b", p["branch"].(string), p["path"].(string), base)
			} else {
				args = append(args, p["path"].(string), p["branch"].(string))
			}
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				return nil, &herdr.Error{Code: "worktree_failed", Message: string(out)}, true
			}
			wt := map[string]any{"path": p["path"], "branch": p["branch"], "is_linked_worktree": true, "label": p["label"], "is_bare": false, "is_detached": false, "is_prunable": false}
			return map[string]any{"type": "worktree_created", "workspace": ws, "tab": map[string]any{}, "root_pane": root, "worktree": wt}, nil, true
		case "worktree.open":
			wt := map[string]any{"path": p["path"], "is_linked_worktree": true, "label": p["label"], "is_bare": false, "is_detached": false, "is_prunable": false}
			return map[string]any{"type": "worktree_opened", "workspace": ws, "tab": map[string]any{}, "root_pane": root, "worktree": wt, "already_open": true}, nil, true
		case "pane.list":
			if panes != nil {
				return map[string]any{"type": "pane_list", "panes": panes}, nil, true
			}
		}
		return nil, nil, false
	}
}

func startWorkerReq(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	w := do(s, "POST", "/api/worker", nil, body)
	var r map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return w.Code, r
}

func TestValidBranch(t *testing.T) {
	for b, want := range map[string]bool{
		"feat": true, "fix/login-bug": true, "v1.2_x": true, "a": true, "release/2026.10": true,
		"": false, "-x": false, ".x": false, "a..b": false, "a//b": false, "a/": false, "a.": false,
		"a.lock": false, "a/.b": false, "HEAD": false, "a b": false, "a~1": false, "a^": false, "a:b": false,
		"a@{1}": false, "a\\b": false, "a*": false, "a?": false, "a[": false, "/a": false, "a/-b": false,
		strings.Repeat("x", 101): false, "ünicode": false,
	} {
		if got := validBranch(b); got != want {
			t.Errorf("validBranch(%q) = %v, want %v", b, got, want)
		}
	}
	gitEnv(t)
	if !gitRefFormatOK(context.Background(), "fix/login-bug") || gitRefFormatOK(context.Background(), "a/b.lock") {
		t.Error("check-ref-format disagrees")
	}
	if got := branchDirName("fix/login/bug"); got != "fix-login-bug" {
		t.Errorf("branchDirName = %q", got)
	}
}

func TestWorkerCreate(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false, "label": "proj"}}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = worktreeHook(t, &wts, nil) })
	s := newWorkerServer(t, client, root, wtRoot)

	code, r := startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"fix/login","kind":"pi","prompt":"work in {{worktree}} from {{base}}"}`)
	want := filepath.Join(wtRoot, "proj", "fix-login")
	if code != 200 || r["worktree"] != want || r["branch"] != "fix/login" || r["base"] != "main" || r["reused"] != false || r["workspace_id"] != "w9" {
		t.Fatalf("worker = %d %v", code, r)
	}
	agent := r["agent"].(map[string]any)
	if agent["id"] != "desk/w9:p1" || agent["name"] != "fix-login" || agent["agent"] != "pi" {
		t.Fatalf("agent = %v", agent)
	}
	if got := strings.Join(fh.methods(), ","); got != "agent.list,worktree.list,worktree.create,agent.start,agent.get,agent.prompt" {
		t.Fatalf("calls = %s", got)
	}
	c := fh.call("worktree.create").Params
	if c["cwd"] != repo || c["branch"] != "fix/login" || c["base"] != "main" || c["path"] != want || c["label"] != "proj/fix-login" || c["focus"] != false {
		t.Fatalf("worktree.create params = %v", c)
	}
	if p := fh.call("agent.prompt").Params; p["target"] != "fix-login" || p["text"] != "work in "+want+" from main" {
		t.Fatalf("agent.prompt params = %v", p)
	}
	if git(t, want, "symbolic-ref", "--short", "HEAD") != "fix/login" {
		t.Fatal("worktree not on its branch")
	}

	// The same branch again: its worktree is reopened, and since the root
	// pane now runs the worker, another free pane is used...
	wts = append(wts, map[string]any{"path": want, "branch": "fix/login", "is_linked_worktree": true, "label": "fix-login", "open_workspace_id": "w9"})
	panes := []map[string]any{
		{"pane_id": "w9:p1", "workspace_id": "w9", "agent": "pi"},
		{"pane_id": "w9:p2", "workspace_id": "w9"},
	}
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil; f.hook = worktreeHook(t, &wts, panes) })
	code, r = startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"fix/login","kind":"pi","name":"second"}`)
	if code != 200 || r["reused"] != true || r["agent"].(map[string]any)["pane_id"] != "w9:p2" {
		t.Fatalf("reuse = %d %v", code, r)
	}
	if got := strings.Join(fh.methods(), ","); got != "agent.list,worktree.list,worktree.open,pane.list,agent.list,agent.start,agent.get" {
		t.Fatalf("calls = %s", got)
	}
	if o := fh.call("worktree.open").Params; o["path"] != want || o["cwd"] != repo || o["focus"] != false || o["base"] != nil {
		t.Fatalf("worktree.open params = %v", o)
	}
	// ...and with every pane busy, nothing is started.
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	if code, r := startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"fix/login","kind":"pi","name":"third"}`); code != http.StatusConflict || r["code"] != "no_free_pane" {
		t.Fatalf("busy = %d %v", code, r)
	}
	for _, m := range fh.methods() {
		if m == "agent.start" {
			t.Fatal("started an agent over a busy pane")
		}
	}
}

func TestWorkerValidation(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = worktreeHook(t, &wts, nil) })
	s := newWorkerServer(t, client, root, wtRoot)
	os.MkdirAll(filepath.Join(wtRoot, "proj", "squatter"), 0o755)
	for body, want := range map[string]string{
		`{"repo":"` + repo + `","branch":"a..b","kind":"pi"}`:             "bad_branch",
		`{"repo":"` + repo + `","branch":"-x","kind":"pi"}`:               "bad_branch",
		`{"repo":"` + repo + `","branch":"x","base":"nope","kind":"pi"}`:  "bad_base",
		`{"repo":"` + repo + `","branch":"x","base":"--all","kind":"pi"}`: "bad_base",
		`{"repo":"` + repo + `/sub","branch":"x","kind":"pi"}`:            "not_a_repo",
		`{"repo":"` + root + `","branch":"x","kind":"pi"}`:                "not_a_repo",
		`{"repo":"` + wtRoot + `","branch":"x","kind":"pi"}`:              "outside_roots",
		`{"repo":"` + repo + `","branch":"x","kind":"codex"}`:             "bad_kind",
		`{"repo":"` + repo + `","branch":"x","kind":"pi","name":"Bad"}`:   "bad_name",
		`{"repo":"` + repo + `","branch":"main","kind":"pi"}`:             "branch_in_primary",
		`{"repo":"` + repo + `","branch":"squatter","kind":"pi"}`:         "path_exists",
	} {
		if code, r := startWorkerReq(t, s, body); code < 400 || r["code"] != want {
			t.Errorf("%s: %d %v, want %s", body, code, r, want)
		}
	}
	for _, m := range fh.methods() {
		if m == "worktree.create" || m == "agent.start" {
			t.Fatalf("a refused worker reached %s", m)
		}
	}
}

// removalSetup makes a worker worktree for branch "w" and a fake herdr
// with the worker agent (w9:p1) in workspace w9 on that worktree.
func removalSetup(t *testing.T) (s *Server, fh *fakeLaunchHerdr, repo, wt string) {
	t.Helper()
	root, repo, wtRoot := workerTree(t)
	wt = filepath.Join(wtRoot, "proj", "w")
	git(t, repo, "worktree", "add", "-q", "-b", "w", wt, "main")
	fh, client := startFakeLaunchHerdr(t)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{{Name: "w", PaneID: "w9:p1", WorkspaceID: "w9", Agent: "pi", Status: herdr.StatusIdle, CWD: wt}}
		f.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
			switch method {
			case "workspace.list":
				return map[string]any{"type": "workspace_list", "workspaces": []any{
					map[string]any{"workspace_id": "w1", "label": "proj", "worktree": map[string]any{"repo_name": "proj", "repo_root": repo, "checkout_path": repo, "is_linked_worktree": false, "repo_key": "k"}},
					map[string]any{"workspace_id": "w9", "label": "proj/w", "worktree": map[string]any{"repo_name": "proj", "repo_root": repo, "checkout_path": wt, "is_linked_worktree": true, "repo_key": "k"}},
				}}, nil, true
			case "worktree.list":
				return map[string]any{"type": "worktree_list", "worktrees": []any{}}, nil, true
			case "workspace.close", "pane.close":
				// Closed agents leave the list (the hook runs under f's lock).
				kept := f.agents[:0]
				for _, a := range f.agents {
					if a.WorkspaceID != p["workspace_id"] && a.PaneID != p["pane_id"] {
						kept = append(kept, a)
					}
				}
				f.agents = kept
				return map[string]any{"type": "ok"}, nil, true
			}
			return nil, nil, false
		}
	})
	return newWorkerServer(t, client, root, wtRoot), fh, repo, wt
}

func removeReq(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	w := do(s, "POST", "/api/remove", nil, body)
	var r map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return w.Code, r
}

func TestRemoveCleanMerged(t *testing.T) {
	s, fh, repo, wt := removalSetup(t)
	code, r := removeReq(t, s, `{"agent":"w9:p1","name":"w","remove_worktree":true}`)
	if code != 200 || r["closed"] != "workspace" || r["worktree_removed"] != true || r["worktree"] != wt {
		t.Fatalf("remove = %d %v", code, r)
	}
	if c := fh.call("workspace.close"); c == nil || c.Params["workspace_id"] != "w9" || c.Params["close_group"] != false {
		t.Fatalf("workspace.close = %+v", c)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("the worktree is still there")
	}
	// The branch is never deleted.
	if git(t, repo, "branch", "--list", "w") == "" {
		t.Fatal("the branch was deleted")
	}
}

func TestRemoveKeepsDirtyOrUnmerged(t *testing.T) {
	s, _, repo, wt := removalSetup(t)
	os.WriteFile(wt+"/new.txt", []byte("x"), 0o644)
	code, r := removeReq(t, s, `{"agent":"w9:p1","remove_worktree":true}`)
	if code != 200 || r["closed"] != "workspace" || r["worktree_removed"] != false || !strings.Contains(r["kept"].(string), "uncommitted") {
		t.Fatalf("dirty = %d %v", code, r)
	}
	if _, err := os.Stat(wt + "/new.txt"); err != nil {
		t.Fatal("the dirty worktree was touched")
	}
	// Committed but not merged: kept too (the agent is gone by now).
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "work")
	code, r = removeReq(t, s, `{"workspace_id":"w9","worktree":"`+wt+`","remove_worktree":true}`)
	if code != 200 || r["worktree_removed"] != false || !strings.Contains(r["kept"].(string), "1 commits not in main") {
		t.Fatalf("unmerged = %d %v", code, r)
	}
	// Merged into main: removable.
	git(t, repo, "merge", "-q", "--ff-only", "w")
	if w := do(s, "GET", "/api/checks?path="+wt+"&branch=w", nil, ""); !strings.Contains(w.Body.String(), `"unmerged_commits":0`) || !strings.Contains(w.Body.String(), `"dirty":false`) {
		t.Fatalf("checks after merge: %s", w.Body)
	}
	if code, r = removeReq(t, s, `{"worktree":"`+wt+`","branch":"w","remove_worktree":true}`); code != 200 || r["worktree_removed"] != true {
		t.Fatalf("merged = %d %v", code, r)
	}
}

// A workspace the user arranged into tabs and splits can hold plain shells
// next to the worker; they are not agents, but closing the workspace would
// close them.
func TestRemoveKeepsWorkspaceWithOtherPanes(t *testing.T) {
	s, fh, _, wt := removalSetup(t)
	inner := fh.hook
	fh.set(func(f *fakeLaunchHerdr) {
		f.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
			res, herr, ok := inner(method, p)
			if method == "worktree.list" {
				// herdr reports the worktree as still open in w9.
				return map[string]any{"type": "worktree_list", "worktrees": []any{
					map[string]any{"path": wt, "open_workspace_id": "w9"},
				}}, nil, true
			}
			if method == "workspace.list" {
				// w9 now has the agent's pane plus a shell in a second tab.
				for _, w := range res.(map[string]any)["workspaces"].([]any) {
					if m := w.(map[string]any); m["workspace_id"] == "w9" {
						m["pane_count"], m["tab_count"] = 2, 2
					}
				}
			}
			return res, herr, ok
		}
	})
	code, r := removeReq(t, s, `{"agent":"w9:p1","name":"w","remove_worktree":true}`)
	if code != 200 || r["closed"] != "pane" || r["worktree_removed"] != false {
		t.Fatalf("remove = %d %v", code, r)
	}
	if fh.call("workspace.close") != nil {
		t.Fatalf("the workspace with another pane was closed: %v", fh.methods())
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("the worktree was removed while a pane still uses it")
	}
}

func TestRemoveConservative(t *testing.T) {
	s, fh, repo, wt := removalSetup(t)
	// A working agent is refused unless allowed.
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = herdr.StatusWorking })
	if code, r := removeReq(t, s, `{"agent":"w9:p1","remove_worktree":true}`); code != http.StatusConflict || r["code"] != "agent_working" {
		t.Fatalf("working = %d %v", code, r)
	}
	// Another agent's pane in the pane now: refused.
	if code, r := removeReq(t, s, `{"agent":"w9:p1","name":"other","allow_working":true}`); code != http.StatusConflict || r["code"] != "agent_changed" {
		t.Fatalf("changed = %d %v", code, r)
	}
	for _, m := range fh.methods() {
		if strings.HasSuffix(m, ".close") {
			t.Fatalf("refused removal closed something: %v", fh.methods())
		}
	}
	// Another live agent in the workspace: only the pane closes, and the
	// worktree it works in stays.
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "helper", PaneID: "w9:p2", WorkspaceID: "w9", Agent: "pi", Status: herdr.StatusIdle, CWD: wt})
	})
	code, r := removeReq(t, s, `{"agent":"w9:p1","allow_working":true,"remove_worktree":true}`)
	if code != 200 || r["closed"] != "pane" || r["worktree_removed"] != false || !strings.Contains(r["kept"].(string), "helper") {
		t.Fatalf("shared = %d %v", code, r)
	}
	if c := fh.call("pane.close"); c == nil || c.Params["pane_id"] != "w9:p1" || fh.call("workspace.close") != nil {
		t.Fatalf("closed = %v", fh.methods())
	}
	// The primary checkout: its workspace is never closed (only the pane)
	// and the checkout is never removed.
	fh.set(func(f *fakeLaunchHerdr) {
		f.calls = nil
		f.agents = []herdr.AgentInfo{{Name: "home", PaneID: "w1:p1", WorkspaceID: "w1", Agent: "pi", Status: herdr.StatusIdle, CWD: repo}}
	})
	code, r = removeReq(t, s, `{"agent":"w1:p1","worktree":"`+repo+`","remove_worktree":true}`)
	if code != 200 || r["closed"] != "pane" || r["worktree_removed"] != false || !strings.Contains(r["kept"].(string), "primary") {
		t.Fatalf("primary = %d %v", code, r)
	}
	if _, err := os.Stat(repo + "/a.txt"); err != nil {
		t.Fatal("the primary checkout was touched")
	}
	// Paths outside the roots are refused.
	if code, r := removeReq(t, s, `{"worktree":"/etc","remove_worktree":true}`); code != http.StatusForbidden || r["code"] != "outside_roots" {
		t.Fatalf("outside = %d %v", code, r)
	}
}

func TestChecksAndRepos(t *testing.T) {
	s, _, repo, wt := removalSetup(t)
	for path, want := range map[string]int{
		wt:                         200,
		repo:                       200,
		"/etc":                     http.StatusForbidden,
		wt + "/../../../..":        http.StatusForbidden,
		filepath.Dir(wt) + "/gone": 200, // missing inside a root: exists=false
		"relative":                 http.StatusBadRequest,
	} {
		if w := do(s, "GET", "/api/checks?path="+path, nil, ""); w.Code != want {
			t.Errorf("checks %s: %d %s", path, w.Code, w.Body)
		}
	}
	var c worktreeChecks
	w := do(s, "GET", "/api/checks?path="+repo, nil, "")
	json.Unmarshal(w.Body.Bytes(), &c)
	if !c.Exists || !c.Primary || c.Branch != "main" {
		t.Fatalf("primary checks = %+v", c)
	}
	w = do(s, "GET", "/api/checks?path="+filepath.Dir(wt)+"/gone", nil, "")
	if !strings.Contains(w.Body.String(), `"exists":false`) {
		t.Fatalf("missing = %s", w.Body)
	}
	// Repositories: the primary checkout only, with its default branch.
	w = do(s, "GET", "/api/repos", nil, "")
	var rs struct{ Repos []repoEntry }
	json.Unmarshal(w.Body.Bytes(), &rs)
	if w.Code != 200 || len(rs.Repos) != 1 || rs.Repos[0].Path != repo || rs.Repos[0].DefaultBranch != "main" {
		t.Fatalf("repos = %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/hosts/desk/repos", nil, ""); w.Code != 200 {
		t.Fatalf("host repos: %d", w.Code)
	}
}

func TestInspectAndDiscard(t *testing.T) {
	s, fh, repo, wt := removalSetup(t)
	// Committed work on the branch, an uncommitted change and a new file.
	os.WriteFile(wt+"/a.txt", []byte("a\nwork\n"), 0o644)
	git(t, wt, "commit", "-q", "-am", "work on a")
	os.WriteFile(wt+"/a.txt", []byte("a\nwork\nmore\n"), 0o644)
	os.MkdirAll(wt+"/deep/er", 0o755)
	os.WriteFile(wt+"/deep/er/new.txt", []byte("x\n"), 0o644)

	// Inspected from a directory inside the worktree (within=1).
	var c worktreeChecks
	w := do(s, "GET", "/api/checks?path="+wt+"/deep/er&within=1&diff=1", nil, "")
	json.Unmarshal(w.Body.Bytes(), &c)
	d := c.Detail
	if w.Code != 200 || c.Path != wt || c.Branch != "w" || c.Tip == "" || d == nil || len(d.Commits) != 1 || d.Commits[0].Subject != "work on a" ||
		len(d.Status) != 2 || d.Status[0] != " M a.txt" || d.Unpushed != 1 || !strings.HasPrefix(d.DiffStat, " a.txt") ||
		!strings.Contains(d.Diff, "+work") || !strings.Contains(d.WorkDiff, "+more") {
		t.Fatalf("inspect = %d %s", w.Code, w.Body)
	}
	// Plain checks carry no detail.
	if w := do(s, "GET", "/api/checks?path="+wt, nil, ""); strings.Contains(w.Body.String(), `"detail"`) {
		t.Fatalf("plain checks = %s", w.Body)
	}

	// A rebase stopped on a conflict: in progress, the branch still known.
	os.Remove(wt + "/deep/er/new.txt")
	git(t, wt, "checkout", "-q", "--", "a.txt")
	os.WriteFile(repo+"/a.txt", []byte("a\nmain\n"), 0o644)
	git(t, repo, "commit", "-q", "-am", "main moves")
	if out, err := exec.Command("git", "-C", wt, "rebase", "main").CombinedOutput(); err == nil {
		t.Fatalf("rebase did not stop: %s", out)
	}
	w = do(s, "GET", "/api/checks?path="+wt, nil, "")
	c = worktreeChecks{}
	json.Unmarshal(w.Body.Bytes(), &c)
	if c.InProgress != "rebase" || c.Branch != "w" || c.Tip == "" {
		t.Fatalf("rebasing = %s", w.Body)
	}
	if ok, why := c.Removable(); ok || !strings.Contains(why, "rebase") {
		t.Fatalf("removable while rebasing: %v %q", ok, why)
	}

	// A discard against a tip the user did not review: refused, nothing
	// closed.
	if code, r := removeReq(t, s, `{"agent":"w9:p1","remove_worktree":true,"discard":true,"discard_tip":"0123abc"}`); code != http.StatusConflict || r["code"] != "discard_changed" {
		t.Fatalf("stale discard = %d %v", code, r)
	}
	if fh.call("workspace.close") != nil {
		t.Fatal("a refused discard closed the workspace")
	}
	// The reviewed tip: the rebase is aborted, the worktree removed and
	// the branch deleted.
	code, r := removeReq(t, s, `{"agent":"w9:p1","remove_worktree":true,"discard":true,"discard_tip":"`+c.Tip+`"}`)
	if code != 200 || r["closed"] != "workspace" || r["worktree_removed"] != true || r["discarded"] != true || r["branch_deleted"] != "w" {
		t.Fatalf("discard = %d %v", code, r)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("the worktree is still there")
	}
	if git(t, repo, "branch", "--list", "w") != "" {
		t.Fatal("the branch is still there")
	}
	if git(t, repo, "log", "-1", "--format=%s") != "main moves" {
		t.Fatal("the primary checkout changed")
	}
	// The primary checkout is never discarded.
	tip := git(t, repo, "rev-parse", "HEAD")
	fh.set(func(f *fakeLaunchHerdr) {
		f.calls = nil
		f.agents = []herdr.AgentInfo{{Name: "home", PaneID: "w1:p1", WorkspaceID: "w1", Agent: "pi", Status: herdr.StatusIdle, CWD: repo}}
	})
	if code, r := removeReq(t, s, `{"agent":"w1:p1","worktree":"`+repo+`","remove_worktree":true,"discard":true,"discard_tip":"`+tip+`"}`); code != http.StatusConflict || r["code"] != "primary_checkout" {
		t.Fatalf("primary discard = %d %v", code, r)
	}
}

func TestInspectMarkDiscardConfirm(t *testing.T) {
	s0, fh, repo, wt := removalSetup(t)
	client := s0.client
	os.WriteFile(wt+"/a.txt", []byte("a\nwork\n"), 0o644)
	git(t, wt, "commit", "-q", "-am", "superseded work")
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "coordinator", PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: herdr.StatusIdle, CWD: repo})
	})
	m := managerWith(t, client, "desk", 2)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), AgentToken: testToken,
		LaunchRoots: []string{filepath.Dir(repo)}, WorktreeRoot: filepath.Dir(filepath.Dir(wt))}, client, m)

	// The coordinator inspects the worker: read-only, allowed.
	w := do(s, "GET", "/api/inspect?agent=desk/w9:p1", agentHdr("w1:p1"), "")
	var in inspection
	json.Unmarshal(w.Body.Bytes(), &in)
	if w.Code != 200 || in.Host != "desk" || in.Checks == nil || in.Checks.Path != wt || in.Checks.Detail == nil ||
		len(in.Checks.Detail.Commits) != 1 || in.Checks.Detail.Diff != "" {
		t.Fatalf("inspect = %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/inspect?agent=desk/nope", agentHdr("w1:p1"), ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown agent = %d", w.Code)
	}
	// It proposes discarding; it cannot confirm.
	if w := do(s, "POST", "/api/marks", agentHdr("w1:p1"), `{"agent_id":"desk/w9:p1","reason":"superseded","discard":true}`); w.Code != 200 {
		t.Fatalf("mark = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/removals/confirm", agentHdr("w1:p1"), `{"ids":["desk/w9:p1"],"discard":{"desk/w9:p1":"x"}}`); w.Code != http.StatusForbidden {
		t.Fatalf("agent confirm = %d", w.Code)
	}
	// The review shows the proposal and what would be lost.
	w = do(s, "GET", "/api/removals", userHdr, "")
	var rv struct{ Removals []removal }
	json.Unmarshal(w.Body.Bytes(), &rv)
	if len(rv.Removals) != 1 {
		t.Fatalf("removals = %s", w.Body)
	}
	it := rv.Removals[0]
	if !it.Mark.Discard || !it.Discardable || it.Safe || it.Checks == nil || it.Checks.Tip == "" || it.Checks.Detail == nil ||
		it.Checks.Detail.Commits[0].Subject != "superseded work" {
		t.Fatalf("review = %s", w.Body)
	}
	// Confirmed as a plain removal, the worktree would be kept; confirmed
	// as a discard with the reviewed tip, the work goes.
	w = do(s, "POST", "/api/removals/confirm", userHdr, `{"ids":["desk/w9:p1"],"discard":{"desk/w9:p1":"`+it.Checks.Tip+`"}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"discarded":true`) || !strings.Contains(w.Body.String(), `"branch_deleted":"w"`) {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	if git(t, repo, "branch", "--list", "w") != "" {
		t.Fatal("the branch is still there")
	}
}

// originRepo builds a repository with a local bare origin, main at two
// commits (a, then someone else's b), a branch w off b with one commit of
// its own, all pushed. It returns the repo, the worktree and the two
// main commits' hashes.
func originRepo(t *testing.T) (repo, wt, a, b string) {
	t.Helper()
	gitEnv(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	repo, wt = filepath.Join(dir, "proj"), filepath.Join(dir, "wt")
	origin := filepath.Join(dir, "origin.git")
	git(t, dir, "init", "-q", "-b", "main", "proj")
	git(t, dir, "init", "-q", "-b", "main", "--bare", "origin.git")
	git(t, repo, "remote", "add", "origin", origin)
	write := func(name, text string) string {
		t.Helper()
		os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644)
		git(t, repo, "add", name)
		git(t, repo, "commit", "-q", "-m", "commit "+name)
		return git(t, repo, "rev-parse", "HEAD")
	}
	a = write("a.txt", "a\n")
	git(t, repo, "push", "-q", "origin", "main")
	b = write("b.txt", "b\n") // someone else's work, merged upstream
	git(t, repo, "push", "-q", "origin", "main")
	git(t, repo, "worktree", "add", "-q", "-b", "w", wt, "main")
	os.WriteFile(wt+"/w.txt", []byte("w\n"), 0o644)
	git(t, wt, "add", "w.txt")
	git(t, wt, "commit", "-q", "-m", "the branch's own work")
	git(t, wt, "push", "-q", "origin", "w")
	return repo, wt, a, b
}

// A squash-merged branch on a host whose local main and remote-tracking
// refs are both stale: the fetch must let the checks see the squash (git
// cherry matches the patch), so the branch counts as merged and removable.
func TestCheckWorktreeSquashMergedUpstream(t *testing.T) {
	repo, wt, _, b := originRepo(t)
	// Squash-merge w onto main upstream, as GitHub would.
	git(t, repo, "merge", "-q", "--squash", "w")
	git(t, repo, "commit", "-q", "-m", "the branch's own work (#1)")
	git(t, repo, "push", "-q", "origin", "main")
	// The stale host: local main and origin/main both back at b.
	git(t, repo, "reset", "-q", "--hard", b)
	git(t, repo, "update-ref", "refs/remotes/origin/main", b)

	c := checkWorktree(context.Background(), wt, "", "")
	if c.Error != "" {
		t.Fatalf("checks: %s", c.Error)
	}
	if c.Unmerged != 0 || c.BaseUsed != "origin/main" {
		t.Fatalf("unmerged = %d base = %s, want 0 origin/main", c.Unmerged, c.BaseUsed)
	}
	if ok, why := c.Removable(); !ok {
		t.Fatalf("not removable: %s", why)
	}
	inspectWorktree(context.Background(), &c, false)
	if len(c.Detail.Commits) != 0 {
		t.Fatalf("commits listed: %+v", c.Detail.Commits)
	}
}

// Not merged (an open PR), but origin's main is fresher than the stale
// local one: the count, the base named and the commit list all come from
// origin/main, so the detail lists only the branch's own commit — never
// the other author's work the local main lacks — and the worktree stays
// blocked, with nothing only here.
func TestCheckWorktreeOpenPRAgainstFresherOrigin(t *testing.T) {
	repo, wt, a, _ := originRepo(t)
	// The stale host: local main way back at a; origin/main still at b.
	git(t, repo, "reset", "-q", "--hard", a)

	c := checkWorktree(context.Background(), wt, "", "")
	if c.Error != "" {
		t.Fatalf("checks: %s", c.Error)
	}
	if c.Unmerged != 1 || c.BaseUsed != "origin/main" {
		t.Fatalf("unmerged = %d base = %s, want 1 origin/main", c.Unmerged, c.BaseUsed)
	}
	if ok, _ := c.Removable(); ok {
		t.Fatal("removable with an open PR")
	}
	inspectWorktree(context.Background(), &c, false)
	if len(c.Detail.Commits) != 1 || c.Detail.Commits[0].Subject != "the branch's own work" {
		t.Fatalf("commits = %+v, want only the branch's own work", c.Detail.Commits)
	}
	if c.Detail.Unpushed != 0 {
		t.Fatalf("unpushed = %d, want 0", c.Detail.Unpushed)
	}
}
