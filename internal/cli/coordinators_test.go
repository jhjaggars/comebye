package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// Two coordinators: "coordinator" (main) owns h1 and t1; "coord-b" (this
	// agent, laharl/w21:p1) owns h2 and the plan pl1.
	twoCoordsJSON = `{"coordinators":[
 {"id":"laharl/w1:p1","host":"laharl","pane_id":"w1:p1","name":"coordinator","main":true,"status":"idle","at":"2026-11-01T09:00:00Z","handoffs":["h1"],"closed":["h9"],"todos":["t1"],"plans":[]},
 {"id":"laharl/w21:p1","host":"laharl","pane_id":"w21:p1","name":"coord-b","main":false,"status":"working","at":"2026-11-02T09:00:00Z","handoffs":["h2"],"closed":[],"todos":[],"plans":["pl1"]}]}`
	plansJSON = `{"plans":[{"id":"pl1","title":"Split the API","size":3,"state":"open","done":1},{"id":"pl2","title":"Other plan","size":2,"state":"open","done":0}]}`
)

func twoCoordAnswers(me string) map[string]answer {
	a := coordAnswers(rolesCoord)
	a["GET /api/whoami"] = answer{200, strings.Replace(whoamiMe, "laharl/w21:p1", me, 1)}
	a["GET /api/roles"] = answer{200, `{"roles":{"laharl/w1:p1":{"role":"coordinator","name":"coordinator","main":true},"laharl/w21:p1":{"role":"coordinator","name":"coord-b"}}}`}
	a["GET /api/coordinators"] = answer{200, twoCoordsJSON}
	a["GET /api/plans"] = answer{200, plansJSON}
	return a
}

func TestCoordStatePath(t *testing.T) {
	e, _, _ := coordEnvWithState(t, nil, "")
	st := StateDir(e.Getenv)
	for _, c := range []struct {
		name string
		main bool
		want string
	}{
		{"coordinator", true, filepath.Join(st, "coordinator", "state.md")}, // main: unchanged
		{"coord-b", true, filepath.Join(st, "coordinator", "state.md")},
		{"coord-b", false, filepath.Join(st, "coordinators", "coord-b", "state.md")},
		{"../evil", false, filepath.Join(st, "coordinator", "state.md")}, // never a path outside
		{"", false, filepath.Join(st, "coordinator", "state.md")},
	} {
		if got := coordStatePath(e, c.name, c.main); got != c.want {
			t.Errorf("coordStatePath(%q, %v) = %s, want %s", c.name, c.main, got, c.want)
		}
	}
}

// Each coordinator's digest keeps to its own work and notes, and says how many
// the others have open.
func TestCoordinatorDigestScopedToTheCoordinator(t *testing.T) {
	// The second coordinator: its own state file, handoff h2, plan pl1.
	e, out, _ := coordEnvWithState(t, twoCoordAnswers("laharl/w21:p1"), "")
	st := StateDir(e.Getenv)
	os.MkdirAll(filepath.Join(st, "coordinators", "coord-b"), 0o700)
	os.WriteFile(filepath.Join(st, "coordinators", "coord-b", "state.md"), []byte("## b\n- b's own note\n"), 0o600)
	os.WriteFile(filepath.Join(st, "coordinator", "state.md"), []byte("## main\n- main's note\n"), 0o600)
	if code := run(e, "coordinator", "digest"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b := out.String()
	for _, want := range []string{"one of several coordinators", "`h2` comebye feat/b", "`pl1` Split the API (1 of 3 done)", "b's own note", filepath.Join(st, "coordinators", "coord-b", "state.md"), "- coordinator (main): 1 handoffs, 1 to-dos, 0 plans open"} {
		if !strings.Contains(b, want) {
			t.Errorf("coord-b's digest lacks %q:\n%s", want, b)
		}
	}
	for _, bad := range []string{"`h1`", "`t1`", "Other plan", "main's note", "feat/a", "feat/done", "Which key"} {
		if strings.Contains(b, bad) {
			t.Errorf("coord-b's digest has %q:\n%s", bad, b)
		}
	}

	// Main: the original state path, its own work, and what nobody owns.
	e, out, _ = coordEnvWithState(t, twoCoordAnswers("laharl/w1:p1"), "")
	st = StateDir(e.Getenv)
	os.WriteFile(filepath.Join(st, "coordinator", "state.md"), []byte("## main\n- main's note\n"), 0o600)
	if code := run(e, "coordinator", "digest"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b = out.String()
	for _, want := range []string{"the main coordinator", "`h1` comebye feat/a", "`t1` [decision", "main's note", filepath.Join(st, "coordinator", "state.md"), "feat/done", "- coord-b: 1 handoffs, 0 to-dos, 1 plans open"} {
		if !strings.Contains(b, want) {
			t.Errorf("main's digest lacks %q:\n%s", want, b)
		}
	}
	for _, bad := range []string{"`h2`", "Split the API", "coordinators/coord-b"} {
		if strings.Contains(b, bad) {
			t.Errorf("main's digest has %q:\n%s", bad, b)
		}
	}
}

// A lone coordinator, or one the hub does not list (an old hub), gets the
// whole digest and the original state path.
func TestCoordinatorDigestUnscopedWhenAlone(t *testing.T) {
	a := coordAnswers(rolesCoord)
	a["GET /api/coordinators"] = answer{200, `{"coordinators":[{"id":"laharl/w21:p1","name":"coordinator","main":true,"handoffs":["h1"],"closed":[],"todos":[],"plans":[]}]}`}
	e, out, dir := coordEnvWithState(t, a, "solo notes\n")
	if code := run(e, "coordinator", "digest"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b := out.String()
	if strings.Contains(b, "one of several") || strings.Contains(b, "Whose work") || !strings.Contains(b, "`h2`") || !strings.Contains(b, "`t1`") || !strings.Contains(b, filepath.Join(dir, "state.md")) {
		t.Fatalf("a lone coordinator's digest:\n%s", b)
	}
}

func TestCoordinatorList(t *testing.T) {
	e, out, _ := coordEnvWithState(t, twoCoordAnswers("laharl/w21:p1"), "")
	st := StateDir(e.Getenv)
	if code := run(e, "coordinator", "list"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	for _, want := range []string{"coordinator  main", "coord-b *", "laharl/w21:p1", filepath.Join(st, "coordinator", "state.md"), filepath.Join(st, "coordinators", "coord-b", "state.md"), "* you"} {
		if !strings.Contains(got, want) {
			t.Errorf("list lacks %q:\n%s", want, got)
		}
	}
	lines := strings.Split(got, "\n")
	if !strings.Contains(strings.Join(strings.Fields(lines[1]), " "), "idle 1 1 0 ") || !strings.Contains(strings.Join(strings.Fields(lines[2]), " "), "working 1 0 1 ") {
		t.Errorf("counts:\n%s", got)
	}
}

func TestCoordinatorNew(t *testing.T) {
	launch := `{"agent":{"id":"laharl/w9:p1","host":"laharl","pane_id":"w9:p1","name":"proj-a","cwd":"/home/x/.local/state/comebye/coordinators/proj-a"},"workspace_id":"w9","coordinator":true,"warnings":["the coordinator reset hook is not installed on this host"]}`
	e, out, _, reqs := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{
		"POST /api/launch": {200, launch}, "POST /api/hosts/kube/launch": {200, launch},
		"GET /api/coordinators": {200, `{"coordinators":[{"id":"laharl/w9:p1","name":"proj-a","main":true,"handoffs":[],"closed":[],"todos":[],"plans":[]}]}`},
	})
	if code := run(e, "coordinator", "new", "proj-a"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var body string
	for _, r := range *reqs {
		if r.method == "POST" && r.path == "/api/launch" {
			body = r.body
		}
	}
	for _, want := range []string{`"coordinator":true`, `"new_coordinator":true`, `"name":"proj-a"`} {
		if !strings.Contains(body, want) {
			t.Errorf("launch body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"dir"`) {
		t.Errorf("a dir was sent: %s", body)
	}
	for _, want := range []string{"Started coordinator proj-a (laharl/w9:p1)", "/coordinator/state.md (on laharl; it is the main coordinator", "warning: the coordinator reset hook"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// --host and --dir.
	if code := run(e, "coordinator", "new", "proj-b", "--host", "kube", "--dir", "/srv/code/proj"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var last recorded
	for _, r := range *reqs {
		if r.method == "POST" {
			last = r
		}
	}
	if last.path != "/api/hosts/kube/launch" || !strings.Contains(last.body, `"dir":"/srv/code/proj"`) {
		t.Errorf("request = %s %s", last.path, last.body)
	}
	// The user's alone: no login, no call.
	e2, _, _, reqs2 := peerEnv(t, nil, nil)
	if code := run(e2, "coordinator", "new", "x"); code == 0 || len(*reqs2) != 0 {
		t.Errorf("without a login: exit %d, %d requests", code, len(*reqs2))
	}
	// Bad names and relative dirs are refused before anything is sent.
	for _, args := range [][]string{{"Proj"}, {"1a"}, {"a", "b"}, {"ok", "--dir", "rel/dir"}, {}} {
		n := len(*reqs)
		if code := run(e, append([]string{"coordinator", "new"}, args...)...); code == 0 || len(*reqs) != n {
			t.Errorf("new %v: exit %d, sent %d", args, code, len(*reqs)-n)
		}
	}
}
