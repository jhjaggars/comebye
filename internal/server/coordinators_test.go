package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/ledger"
)

type coordListBody struct {
	Coordinators []coordinatorInfo `json:"coordinators"`
}

func coordsOf(t *testing.T, s *Server) map[string]coordinatorInfo {
	t.Helper()
	w := do(s, "GET", "/api/coordinators", agentHdr("w1:p1"), "")
	if w.Code != 200 {
		t.Fatalf("GET /api/coordinators = %d %s", w.Code, w.Body)
	}
	var b coordListBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	out := map[string]coordinatorInfo{}
	for _, c := range b.Coordinators {
		out[c.Name] = c
	}
	return out
}

func has(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// Each coordinator is listed with the open work that is its own: a handoff's
// requester, a to-do's waiting worker's owner, a plan's creator. What nobody
// owns, and what a removed coordinator owned, is main's.
func TestCoordinatorsListOwnWork(t *testing.T) {
	r := newRouteRig(t)
	add := func(branch, pane string, st ledger.Status, to *ledger.Ref) string {
		e, err := r.s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: branch, AgentID: "desk/" + pane, PaneID: pane, AgentName: branch, Status: st, ReplyTo: to})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	hA := add("a1", "w5:p1", ledger.Running, refA)
	hB := add("b1", "w6:p1", ledger.Finished, refB)
	hNone := add("n1", "w7:p1", ledger.Running, nil)
	hDone := add("b0", "w4:p1", ledger.Landed, refB)
	// A sub-worker of B's worker is B's.
	hSub := add("b-sub", "w8:p1", ledger.Running, &ledger.Ref{AgentID: "desk/w6:p1", Name: "b1"})
	p, err := r.s.ledger.AddPlan(ledger.Plan{Title: "p", Size: 2, ReplyTo: refB})
	if err != nil {
		t.Fatal(err)
	}
	tB, _, _ := r.s.todos.add(Todo{Question: "q", Kind: "decision", State: TodoOpen, CreatedBy: "w", Waiting: &TodoAgent{ID: refB.AgentID, Name: refB.Name}})
	tNone, _, _ := r.s.todos.add(Todo{Question: "q2", Kind: "decision", State: TodoOpen, CreatedBy: "w"})

	cs := coordsOf(t, r.s)
	a, b := cs["coordinator"], cs["coord-b"]
	if !a.Main || b.Main {
		t.Fatalf("main: a=%v b=%v", a.Main, b.Main)
	}
	if !has(a.Handoffs, hA) || !has(a.Handoffs, hNone) || has(a.Handoffs, hB) || len(a.Handoffs) != 2 {
		t.Fatalf("a handoffs = %v", a.Handoffs)
	}
	if !has(b.Handoffs, hB) || !has(b.Handoffs, hSub) || len(b.Handoffs) != 2 || !has(b.Closed, hDone) {
		t.Fatalf("b handoffs = %v closed %v", b.Handoffs, b.Closed)
	}
	if !has(b.Todos, tB.ID) || !has(a.Todos, tNone.ID) || len(a.Todos)+len(b.Todos) != 2 {
		t.Fatalf("todos a=%v b=%v", a.Todos, b.Todos)
	}
	if !has(b.Plans, p.ID) || len(a.Plans) != 0 {
		t.Fatalf("plans a=%v b=%v", a.Plans, b.Plans)
	}

	// B's role is removed: what it owned falls back to main.
	if err := r.s.roles.set(refB.AgentID, Role{}); err != nil {
		t.Fatal(err)
	}
	cs = coordsOf(t, r.s)
	if len(cs) != 1 {
		t.Fatalf("after removal: %v", cs)
	}
	a = cs["coordinator"]
	if !has(a.Handoffs, hB) || !has(a.Handoffs, hSub) || !has(a.Todos, tB.ID) || !has(a.Plans, p.ID) {
		t.Fatalf("main did not take the removed coordinator's work: %+v", a)
	}
}

// A new coordinator is launched in its own directory under the state dir (or
// in a directory inside a launch root), with the playbook prompt, the hook
// when it is installed, and the coordinator role; it never pulls.
func TestLaunchNewCoordinator(t *testing.T) {
	root, outside := launchTree(t)
	fh, client := startFakeLaunchHerdr(t)
	state := t.TempDir()
	s := newServer(t, Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, StateDir: state, CoordinatorDir: t.TempDir()}, client, nil)
	s.findExec = func(name string) bool { return name == "claude" }

	// No name: refused.
	if w := do(s, "POST", "/api/launch", nil, `{"coordinator":true,"new_coordinator":true,"kind":"claude"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "bad_name") {
		t.Fatalf("no name = %d %s", w.Code, w.Body)
	}
	// A directory outside the roots: refused.
	if w := do(s, "POST", "/api/launch", nil, `{"coordinator":true,"new_coordinator":true,"kind":"claude","name":"proj-a","dir":"`+outside+`"}`); w.Code != 403 {
		t.Fatalf("outside = %d %s", w.Code, w.Body)
	}
	if fh.call("agent.start") != nil {
		t.Fatal("started something")
	}

	// Default directory, no hook installed: a warning.
	w := do(s, "POST", "/api/launch", nil, `{"coordinator":true,"new_coordinator":true,"kind":"claude","name":"proj-a"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hook is not installed") {
		t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(state, "coordinators", "proj-a"))
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("no directory %s: %v", want, err)
	}
	if ws := fh.call("workspace.create"); ws.Params["cwd"] != want || ws.Params["label"] != "proj-a" {
		t.Fatalf("workspace.create = %v", ws.Params)
	}
	st := fh.call("agent.start")
	var argv []string
	b, _ := json.Marshal(st.Params["args"])
	json.Unmarshal(b, &argv)
	if st.Params["name"] != "proj-a" || len(argv) != 4 || argv[0] != "--allowedTools" || !strings.Contains(argv[3], "coordinator proj-a") || !strings.Contains(argv[3], "comebye coordinator list") {
		t.Fatalf("agent.start = %v", st.Params)
	}
	if !s.isCoordinator("desk/w9:p1", "proj-a") {
		t.Fatal("role not recorded")
	}

	// With the hook installed and a directory in a root: --settings, no warning.
	os.MkdirAll(filepath.Join(state, "coordinator"), 0o700)
	os.WriteFile(filepath.Join(state, "coordinator", "settings.json"), []byte("{}"), 0o600)
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	proj := filepath.Join(root, "repo")
	w = do(s, "POST", "/api/launch", nil, `{"coordinator":true,"new_coordinator":true,"kind":"claude","name":"proj-b","dir":"`+proj+`"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "hook is not installed") {
		t.Fatalf("launch 2 = %d %s", w.Code, w.Body)
	}
	if ws := fh.call("workspace.create"); !strings.HasSuffix(ws.Params["cwd"].(string), "/repo") {
		t.Fatalf("workspace.create = %v", ws.Params)
	}
	b, _ = json.Marshal(fh.call("agent.start").Params["args"])
	if !strings.Contains(string(b), "--settings") || !strings.Contains(string(b), "settings.json") {
		t.Fatalf("args = %s", b)
	}

	// A taken name is refused.
	if w := do(s, "POST", "/api/launch", nil, `{"coordinator":true,"new_coordinator":true,"kind":"claude","name":"proj-b"}`); w.Code != 409 {
		t.Fatalf("taken = %d %s", w.Code, w.Body)
	}
}
