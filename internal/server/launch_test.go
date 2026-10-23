package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// fakeLaunchHerdr answers the herdr calls a launch makes on a unix socket
// and records them in order, shaped after herdr's protocol 22 schema.
type fakeLaunchHerdr struct {
	mu       sync.Mutex
	agents   []herdr.AgentInfo
	calls    []fakeCall
	startErr []*herdr.Error // agent.start errors, consumed one per call
	noRoot   bool           // workspace.create reports no root_pane
	// hook, when set, answers a call first (ok=true), under the lock.
	hook func(method string, params map[string]any) (res any, err *herdr.Error, ok bool)
}

type fakeCall struct {
	Method string
	Params map[string]any
}

// set changes the fake's state under its lock.
func (f *fakeLaunchHerdr) set(fn func(f *fakeLaunchHerdr)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeLaunchHerdr) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeLaunchHerdr) call(method string) *fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.calls {
		if f.calls[i].Method == method {
			return &f.calls[i]
		}
	}
	return nil
}

func (f *fakeLaunchHerdr) answer(method string, params map[string]any) (any, *herdr.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{method, params})
	if f.hook != nil {
		if res, err, ok := f.hook(method, params); ok {
			return res, err
		}
	}
	switch method {
	case "agent.list":
		return map[string]any{"type": "agent_list", "agents": f.agents}, nil
	case "session.snapshot":
		return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"workspaces": []any{}}}, nil
	case "workspace.create":
		res := map[string]any{
			"type":      "workspace_created",
			"workspace": map[string]any{"workspace_id": "w9", "number": 9, "label": params["label"], "focused": false, "pane_count": 1, "tab_count": 1, "active_tab_id": "w9:t1", "agent_status": "unknown"},
			"tab":       map[string]any{"tab_id": "w9:t1", "workspace_id": "w9"},
		}
		if !f.noRoot {
			res["root_pane"] = map[string]any{"pane_id": "w9:p1", "terminal_id": "t1", "workspace_id": "w9", "tab_id": "w9:t1", "focused": true, "agent_status": "unknown", "revision": 0}
		}
		return res, nil
	case "pane.list":
		return map[string]any{"type": "pane_list", "panes": []any{
			map[string]any{"pane_id": "w9:p2", "terminal_id": "t2", "workspace_id": "w9", "tab_id": "w9:t1", "focused": true, "agent_status": "unknown", "revision": 0},
		}}, nil
	case "agent.start":
		if len(f.startErr) > 0 {
			e := f.startErr[0]
			f.startErr = f.startErr[1:]
			if e != nil {
				return nil, e
			}
		}
		agent := herdr.AgentInfo{Name: params["name"].(string), PaneID: params["pane_id"].(string), WorkspaceID: "w9", Agent: params["kind"].(string), Status: herdr.StatusIdle, InteractiveReady: true}
		f.agents = append(f.agents, agent)
		return map[string]any{"type": "agent_started", "agent": agent, "argv": []string{params["kind"].(string)}}, nil
	case "agent.get":
		target, _ := params["target"].(string)
		for i := range f.agents {
			if f.agents[i].Name == target || f.agents[i].PaneID == target {
				return map[string]any{"type": "agent_info", "agent": f.agents[i]}, nil
			}
		}
		return nil, &herdr.Error{Code: "agent_not_found", Message: "agent target " + target + " not found"}
	case "agent.prompt":
		return map[string]any{"type": "agent_prompted", "agent": map[string]any{"name": params["target"], "pane_id": "w9:p1", "agent_status": "working"}}, nil
	}
	return nil, &herdr.Error{Code: "unknown_method", Message: method}
}

func startFakeLaunchHerdr(t *testing.T) (*fakeLaunchHerdr, *herdr.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hp") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f := &fakeLaunchHerdr{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID     string         `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				_ = json.Unmarshal(line, &req)
				res, herr := f.answer(req.Method, req.Params)
				var b []byte
				if herr != nil {
					b, _ = json.Marshal(map[string]any{"id": req.ID, "error": herr})
				} else {
					b, _ = json.Marshal(map[string]any{"id": req.ID, "result": res})
				}
				c.Write(append(b, '\n'))
			}()
		}
	}()
	return f, herdr.NewClient(sock)
}

// launchTree builds a root with a few directories (one a git checkout,
// one hidden, a symlink out of the root and one inside it) and a
// directory outside the root.
func launchTree(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "code")
	outside = filepath.Join(base, "secret")
	for _, d := range []string{
		root + "/repo/.git", root + "/repo/src", root + "/Notes", root + "/.hidden", root + "/my.Project!",
		outside + "/inner",
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(root+"/file.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root+"/escape"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root+"/repo", root+"/alias"); err != nil {
		t.Fatal(err)
	}
	// Return the resolved paths (the temp dir may sit behind a symlink).
	root, _ = filepath.EvalSymlinks(root)
	outside, _ = filepath.EvalSymlinks(outside)
	return root, outside
}

func newLaunchServer(t *testing.T, client *herdr.Client, manager *state.Manager, roots ...string) *Server {
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: roots}, client, manager)
	s.findExec = func(name string) bool { return name == "pi" || name == "codex" || name == "cursor-agent" }
	return s
}

func decode(t *testing.T, w interface{ Result() *http.Response }, v any) {
	t.Helper()
	if err := json.NewDecoder(w.Result().Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

type optionsBody struct {
	Enabled     bool        `json:"enabled"`
	Reason      string      `json:"reason"`
	Roots       []launchDir `json:"roots"`
	Kinds       []string    `json:"kinds"`
	DefaultKind string      `json:"default_kind"`
	Recent      []launchDir `json:"recent"`
}

func TestLaunchOptions(t *testing.T) {
	root, outside := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	// Session files give the agents distinct LastActive times.
	sess := func(name string, age time.Duration) *herdr.AgentSessionInfo {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, nil, 0o644)
		ts := time.Now().Add(-age)
		os.Chtimes(p, ts, ts)
		return &herdr.AgentSessionInfo{Kind: "path", Value: p}
	}
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{
			{Name: "a", PaneID: "w1:p1", Agent: "claude", CWD: root + "/Notes", AgentSession: sess("a", time.Hour)},
			{Name: "b", PaneID: "w2:p1", Agent: "claude", CWD: root + "/repo", AgentSession: sess("b", time.Minute)},
			{Name: "c", PaneID: "w3:p1", Agent: "pi", CWD: root + "/repo", AgentSession: sess("c", 2*time.Hour)},
			{Name: "d", PaneID: "w4:p1", Agent: "pi", CWD: outside},
			{Name: "e", PaneID: "w5:p1", Agent: "claude", CWD: root + "/gone"},
		}
	})
	m := state.New(client, nil)
	m.SetHost("desk")
	m.Refresh()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Agents()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s := newLaunchServer(t, client, m, root, "/does/not/exist")

	var o optionsBody
	w := do(s, "GET", "/api/launch/options", nil, "")
	decode(t, w, &o)
	if w.Code != 200 || !o.Enabled || len(o.Roots) != 1 || o.Roots[0].Path != root {
		t.Fatalf("options = %d %+v", w.Code, o)
	}
	// Installed (pi, codex, cursor via cursor-agent) plus running (claude),
	// in herdr's order; claude runs most often.
	if strings.Join(o.Kinds, ",") != "pi,claude,codex,cursor" || o.DefaultKind != "claude" {
		t.Fatalf("kinds = %v default %q", o.Kinds, o.DefaultKind)
	}
	// Distinct, inside the root, existing, most recent first.
	if len(o.Recent) != 2 || o.Recent[0].Path != root+"/repo" || !o.Recent[0].Git || o.Recent[1].Path != root+"/Notes" {
		t.Fatalf("recent = %+v", o.Recent)
	}
	// The host-qualified route answers the same for the local host.
	if w := do(s, "GET", "/api/hosts/desk/launch/options", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("host route: %d %s", w.Code, w.Body)
	}
}

func TestLaunchDisabled(t *testing.T) {
	for _, roots := range [][]string{nil, {"/does/not/exist"}} {
		s := newLaunchServer(t, nil, nil, roots...)
		var o optionsBody
		w := do(s, "GET", "/api/launch/options", nil, "")
		decode(t, w, &o)
		if w.Code != 200 || o.Enabled || o.Reason == "" || len(o.Kinds) != 0 {
			t.Fatalf("roots %v: %d %+v", roots, w.Code, o)
		}
		if w := do(s, "GET", "/api/launch/dirs", nil, ""); w.Code != http.StatusForbidden {
			t.Fatalf("dirs: %d", w.Code)
		}
		if w := do(s, "POST", "/api/launch", nil, `{"dir":"/tmp","kind":"pi"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "launch_disabled") {
			t.Fatalf("launch: %d %s", w.Code, w.Body)
		}
	}
}

func TestLaunchDirs(t *testing.T) {
	root, outside := launchTree(t)
	s := newLaunchServer(t, nil, nil, root)

	var roots struct {
		Path    string      `json:"path"`
		Entries []launchDir `json:"entries"`
	}
	w := do(s, "GET", "/api/launch/dirs", nil, "")
	decode(t, w, &roots)
	if w.Code != 200 || roots.Path != "" || len(roots.Entries) != 1 || roots.Entries[0].Path != root {
		t.Fatalf("roots = %d %+v", w.Code, roots)
	}

	var d struct {
		Path    string      `json:"path"`
		Parent  string      `json:"parent"`
		Entries []launchDir `json:"entries"`
	}
	w = do(s, "GET", "/api/launch/dirs?path="+root, nil, "")
	decode(t, w, &d)
	var names []string
	for _, e := range d.Entries {
		names = append(names, e.Name)
	}
	// Sorted case-insensitively; no files, hidden dirs or escaping links;
	// the link inside the root stays.
	if w.Code != 200 || d.Path != root || d.Parent != "" || strings.Join(names, ",") != "alias,my.Project!,Notes,repo" {
		t.Fatalf("listing = %d %+v", w.Code, d)
	}
	if !d.Entries[3].Git || d.Entries[2].Git || !d.Entries[0].Git {
		t.Fatalf("git flags = %+v", d.Entries)
	}
	w = do(s, "GET", "/api/launch/dirs?path="+root+"/repo", nil, "")
	d.Parent = ""
	decode(t, w, &d)
	if w.Code != 200 || d.Parent != root || len(d.Entries) != 1 || d.Entries[0].Name != "src" {
		t.Fatalf("repo = %d %+v", w.Code, d)
	}

	for path, want := range map[string]int{
		outside:                        http.StatusForbidden,  // outside the root
		root + "/escape":               http.StatusForbidden,  // a symlink out of the root
		root + "/escape/inner":         http.StatusForbidden,  // ...and below it
		root + "/../secret":            http.StatusForbidden,  // traversal
		root + "/repo/../../secret":    http.StatusForbidden,  // traversal
		"/nonexistent/x":               http.StatusForbidden,  // nothing said about the rest of the disk
		root + "/missing":              http.StatusNotFound,   // missing inside the root
		"code/repo":                    http.StatusBadRequest, // relative
		"../code":                      http.StatusBadRequest,
		root + "/file.txt":             http.StatusBadRequest, // not a directory
		filepath.Dir(root):             http.StatusForbidden,  // above the root
		root + "/repo/../../code/repo": http.StatusOK,
	} {
		w := do(s, "GET", "/api/launch/dirs?path="+path, nil, "")
		if w.Code != want {
			t.Errorf("%s: %d %s, want %d", path, w.Code, w.Body, want)
		}
	}
	// A symlink inside the root resolves to its target.
	w = do(s, "GET", "/api/launch/dirs?path="+root+"/alias", nil, "")
	decode(t, w, &d)
	if w.Code != 200 || d.Path != root+"/repo" {
		t.Fatalf("alias = %d %+v", w.Code, d)
	}
}

func TestLaunchDirsCapped(t *testing.T) {
	root := t.TempDir()
	for i := range maxDirEntries + 5 {
		os.Mkdir(filepath.Join(root, fmt.Sprintf("d%03d", i)), 0o755)
	}
	s := newLaunchServer(t, nil, nil, root)
	var d struct {
		Entries   []launchDir `json:"entries"`
		Truncated bool        `json:"truncated"`
	}
	w := do(s, "GET", "/api/launch/dirs?path="+root, nil, "")
	decode(t, w, &d)
	if len(d.Entries) != maxDirEntries || !d.Truncated {
		t.Fatalf("got %d entries, truncated %v", len(d.Entries), d.Truncated)
	}
}

type launchResp struct {
	Agent       browserAgent `json:"agent"`
	WorkspaceID string       `json:"workspace_id"`
	PromptError string       `json:"prompt_error"`
	Code        string       `json:"code"`
	Message     string       `json:"message"`
}

func launch(t *testing.T, s *Server, body string) (int, launchResp) {
	t.Helper()
	w := do(s, "POST", "/api/launch", nil, body)
	var r launchResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return w.Code, r
}

func TestLaunchHappyPath(t *testing.T) {
	root, _ := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	s := newLaunchServer(t, client, nil, root)

	code, r := launch(t, s, `{"dir":"`+root+`/repo","kind":"codex","name":"fixer","prompt":"fix the tests"}`)
	if code != 200 || r.WorkspaceID != "w9" || r.Agent.ID != "desk/w9:p1" || r.Agent.Host != "desk" || r.Agent.PaneID != "w9:p1" ||
		r.Agent.Name != "fixer" || r.Agent.Agent != "codex" || r.PromptError != "" {
		t.Fatalf("launch = %d %+v", code, r)
	}
	if got := strings.Join(fh.methods(), ","); got != "agent.list,workspace.create,agent.start,agent.get,agent.prompt" {
		t.Fatalf("calls = %s", got)
	}
	ws := fh.call("workspace.create").Params
	if ws["cwd"] != root+"/repo" || ws["label"] != "fixer" || ws["focus"] != false {
		t.Fatalf("workspace.create params = %v", ws)
	}
	st := fh.call("agent.start").Params
	if st["name"] != "fixer" || st["kind"] != "codex" || st["pane_id"] != "w9:p1" || st["timeout_ms"] != float64(60000) {
		t.Fatalf("agent.start params = %v", st)
	}
	// args is always sent, even when empty, matching the herdr CLI.
	if args, ok := st["args"].([]any); !ok || len(args) != 0 {
		t.Fatalf("agent.start args = %v", st["args"])
	}
	if p := fh.call("agent.prompt").Params; p["target"] != "fixer" || p["text"] != "fix the tests" || p["wait"] != nil {
		t.Fatalf("agent.prompt params = %v", p)
	}
}

func TestLaunchDefaultsAndNaming(t *testing.T) {
	root, _ := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{{Name: "repo", PaneID: "w1:p1", Agent: "pi"}, {Name: "repo-2", PaneID: "w2:p1", Agent: "pi"}}
		f.noRoot = true // the pane is found with pane.list
	})
	s := newLaunchServer(t, client, nil, root)

	// No name: the directory's, made unique; no prompt: none sent; no
	// kind: the default (first installed, nothing running locally).
	code, r := launch(t, s, `{"dir":"`+root+`/alias"}`)
	if code != 200 || r.Agent.Name != "repo-3" || r.Agent.Agent != "pi" || r.Agent.PaneID != "w9:p2" {
		t.Fatalf("launch = %d %+v", code, r)
	}
	if got := strings.Join(fh.methods(), ","); got != "agent.list,workspace.create,pane.list,agent.start,agent.get" {
		t.Fatalf("calls = %s", got)
	}
	ws := fh.call("workspace.create").Params
	if ws["cwd"] != root+"/repo" || ws["label"] != "repo" {
		t.Fatalf("workspace.create params = %v", ws)
	}
	if p := fh.call("pane.list").Params; p["workspace_id"] != "w9" {
		t.Fatalf("pane.list params = %v", p)
	}

	// A name that is taken is refused before anything is created.
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	if code, r := launch(t, s, `{"dir":"`+root+`/repo","kind":"pi","name":"repo"}`); code != http.StatusConflict || r.Code != "agent_name_taken" {
		t.Fatalf("taken = %d %+v", code, r)
	}
	if got := strings.Join(fh.methods(), ","); got != "agent.list" {
		t.Fatalf("calls = %s", got)
	}

	for in, want := range map[string]string{
		"my.Project!":                       "my-project",
		"123":                               "agent-123",
		"---":                               "agent",
		"ÜberTool":                          "bertool",
		strings.Repeat("abcdefghij", 5):     "abcdefghijabcdefghijabcdefgh",
		"x_y-z":                             "x_y-z",
		"__init__":                          "init",
		"a" + strings.Repeat("-", 30) + "b": "a-b",
	} {
		if got := defaultAgentName("/r/"+in, func(string) bool { return false }); got != want || !agentNameRe.MatchString(got) {
			t.Errorf("defaultAgentName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLaunchValidation(t *testing.T) {
	root, outside := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	s := newLaunchServer(t, client, nil, root)
	for body, want := range map[string]string{
		`{"dir":"` + root + `/repo","kind":"claude"}`:                                      "bad_kind", // not installed, not running
		`{"dir":"` + root + `/repo","kind":"rm -rf"}`:                                      "bad_kind",
		`{"dir":"` + root + `/repo","kind":"pi","name":"Fixer"}`:                           "bad_name",
		`{"dir":"` + root + `/repo","kind":"pi","name":"1st"}`:                             "bad_name",
		`{"dir":"` + root + `/repo","kind":"pi","name":"a/b"}`:                             "bad_name",
		`{"dir":"` + outside + `","kind":"pi"}`:                                            "outside_roots",
		`{"dir":"` + root + `/escape","kind":"pi"}`:                                        "outside_roots",
		`{"dir":"` + root + `/../secret","kind":"pi"}`:                                     "outside_roots",
		`{"dir":"repo","kind":"pi"}`:                                                       "bad_path",
		`{"kind":"pi"}`:                                                                    "bad_path",
		`{"dir":"` + root + `/file.txt","kind":"pi"}`:                                      "not_a_directory",
		`{"dir":"` + root + `/repo","kind":"pi","name":"` + strings.Repeat("a", 33) + `"}`: "bad_name",
	} {
		if code, r := launch(t, s, body); code < 400 || r.Code != want {
			t.Errorf("%s: %d %+v, want %s", body, code, r, want)
		}
	}
	big := `{"dir":"` + root + `/repo","kind":"pi","prompt":"` + strings.Repeat("x", maxLaunchPrompt+1) + `"}`
	if code, r := launch(t, s, big); code != http.StatusRequestEntityTooLarge || r.Code != "prompt_too_large" {
		t.Errorf("big prompt: %d %+v", code, r)
	}
	if w := do(s, "POST", "/api/launch", nil, `{"dir":"`+root+`/repo","prompt":"`+strings.Repeat("x", 2<<20)+`"}`); w.Code != http.StatusBadRequest {
		t.Errorf("oversized body: %d", w.Code)
	}
	if n := len(fh.methods()); n != 0 {
		t.Errorf("refused launches reached herdr: %v", fh.methods())
	}
}

func TestLaunchFailures(t *testing.T) {
	root, _ := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	s := newLaunchServer(t, client, nil, root)

	// agent.start fails after the workspace exists: the error says so.
	fh.set(func(f *fakeLaunchHerdr) {
		f.startErr = []*herdr.Error{{Code: "agent_start_failed", Message: "pi did not become ready"}}
	})
	code, r := launch(t, s, `{"dir":"`+root+`/repo","kind":"pi","prompt":"hi"}`)
	if code != http.StatusBadGateway || r.Code != "agent_start_failed" || !strings.Contains(r.Message, "pi did not become ready") ||
		!strings.Contains(r.Message, `workspace "repo" is left open`) || r.WorkspaceID != "w9" {
		t.Fatalf("start failure = %d %+v", code, r)
	}
	if got := strings.Join(fh.methods(), ","); got != "agent.list,workspace.create,agent.start" {
		t.Fatalf("calls = %s (no prompt after a failed start)", got)
	}

	// A pane whose shell is not ready yet is retried, whether herdr
	// reports it unavailable or busy (its shell still initializing).
	for _, busyCode := range []string{"agent_pane_unavailable", "agent_pane_busy"} {
		fh.set(func(f *fakeLaunchHerdr) {
			f.calls = nil
			f.agents = nil // the previous retry left its agent behind
			f.startErr = []*herdr.Error{{Code: busyCode, Message: "not at a shell prompt"}, nil}
		})
		if code, r := launch(t, s, `{"dir":"`+root+`/Notes","kind":"pi"}`); code != 200 || r.Agent.Name != "notes" {
			t.Fatalf("retry %s = %d %+v", busyCode, code, r)
		}
		if got := strings.Join(fh.methods(), ","); got != "agent.list,workspace.create,agent.start,agent.start,agent.get" {
			t.Fatalf("retry %s calls = %s", busyCode, got)
		}
	}

	// A launch that never becomes an active named agent — herdr admits
	// agent.start but the name never activates — fails the launch with a
	// clear error instead of leaving a worker no prompt can reach.
	s.agentReadyPoll, s.agentReadyWait = 20*time.Millisecond, 100*time.Millisecond
	fh.set(func(f *fakeLaunchHerdr) {
		f.calls = nil
		f.hook = func(method string, params map[string]any) (res any, err *herdr.Error, ok bool) {
			if method == "agent.get" {
				return nil, &herdr.Error{Code: "agent_not_found", Message: "agent target not found"}, true
			}
			return nil, nil, false
		}
	})
	code, r = launch(t, s, `{"dir":"`+root+`/Notes","kind":"pi","prompt":"hi"}`)
	if code != http.StatusBadGateway || r.Code != "launch_failed" || !strings.Contains(r.Message, "did not become an active named agent") ||
		!strings.Contains(r.Message, `workspace "Notes" is left open`) {
		t.Fatalf("never active = %d %+v", code, r)
	}
	if got := strings.Join(fh.methods(), ","); !strings.HasPrefix(got, "agent.list,workspace.create,agent.start,agent.get") || fh.call("agent.prompt") != nil {
		t.Fatalf("never active calls = %s", got)
	}

	// The name resolving is not enough: herdr answers agent.get as soon
	// as the launch is registered, so the brief must wait for the agent
	// to report ready for input before the prompt is sent.
	s.agentReadyPoll = 5 * time.Millisecond
	var gets atomic.Int32
	fh.set(func(f *fakeLaunchHerdr) {
		f.calls = nil
		f.agents = nil // the earlier launches left their agents behind
		f.hook = func(method string, params map[string]any) (res any, err *herdr.Error, ok bool) {
			if method == "agent.get" && gets.Add(1) < 3 {
				// Registered but not yet ready (herdr's launch still
				// pending, the agent not detected in its pane).
				return map[string]any{"type": "agent_info", "agent": herdr.AgentInfo{Name: params["target"].(string), PaneID: "w9:p1", Agent: "pi", Status: herdr.StatusUnknown}}, nil, true
			}
			return nil, nil, false
		}
	})
	code, r = launch(t, s, `{"dir":"`+root+`/Notes","kind":"pi","prompt":"hi\nthere"}`)
	if code != 200 || r.PromptError != "" {
		t.Fatalf("wait for ready = %d %+v", code, r)
	}
	if n := gets.Load(); n < 3 {
		t.Fatalf("prompted after %d gets, want at least 3", n)
	}
	if p := fh.call("agent.prompt").Params; p["target"] != "notes" || p["text"] != "hi\nthere" {
		t.Fatalf("prompt = %v", p)
	}
}

func TestLaunchHostRoutes(t *testing.T) {
	root, _ := launchTree(t)
	s := newLaunchServer(t, nil, nil, root)
	if w := do(s, "GET", "/api/hosts/desk/launch/dirs?path="+root, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"repo"`) {
		t.Fatalf("local dirs: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/hosts/desk/launch", nil, `{"dir":"/etc","kind":"pi"}`); w.Code != http.StatusForbidden {
		t.Fatalf("local launch: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/api/hosts/kube/launch/options", "/api/hosts/kube/launch/dirs"} {
		if w := do(s, "GET", p, nil, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_host") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
}

func TestLaunchRequiresAuthAndCSRF(t *testing.T) {
	root, _ := launchTree(t)
	s := New(Config{AllowLogins: []string{"me@example.com"}, AllowHosts: testHosts, HostName: "desk", LaunchRoots: []string{root}}, nil, nil)
	h := s.Handler()
	send := func(method, path string, hdr map[string]string, body string) int {
		r, _ := http.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	me := map[string]string{"Tailscale-User-Login": "me@example.com", "Content-Type": "application/json"}
	if c := send("GET", "/api/launch/options", nil, ""); c != http.StatusUnauthorized {
		t.Errorf("options without login: %d", c)
	}
	if c := send("POST", "/api/launch", map[string]string{"Content-Type": "application/json"}, `{}`); c != http.StatusUnauthorized {
		t.Errorf("launch without login: %d", c)
	}
	cross := map[string]string{"Tailscale-User-Login": "me@example.com", "Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}
	if c := send("POST", "/api/launch", cross, `{}`); c != http.StatusForbidden {
		t.Errorf("cross-site launch: %d", c)
	}
	if c := send("POST", "/api/launch", map[string]string{"Tailscale-User-Login": "me@example.com", "Content-Type": "text/plain"}, `{}`); c != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain launch: %d", c)
	}
	if c := send("GET", "/api/launch/options", me, ""); c != 200 {
		t.Errorf("options: %d", c)
	}
}

func TestFirstPromptArgs(t *testing.T) {
	long := strings.Repeat("x", maxArgPrompt+1)
	for _, c := range []struct {
		kind, prompt string
		want         []string
	}{
		{"claude", "fix `it`", []string{"--", "fix `it`"}},
		{"pi", "-v looks like a flag", []string{"--", "-v looks like a flag"}},
		{"codex", "fix it", nil},      // no known initial-prompt argument
		{"claude", "two\nlines", nil}, // typed into a shell: agent.prompt instead
		{"claude", long, nil},
		{"claude", "  ", nil},
	} {
		if got := firstPromptArgs(c.kind, c.prompt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("firstPromptArgs(%q, %.20q) = %q, want %q", c.kind, c.prompt, got, c.want)
		}
	}
}
