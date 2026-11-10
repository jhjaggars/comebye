package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/modkit"
)

type recClaude struct{ calls []string }

func (r *recClaude) Run(args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	if len(args) >= 3 && args[1] == "marketplace" && args[2] == "list" || len(args) >= 2 && args[1] == "list" {
		return "[]", nil
	}
	return "", nil
}

func TestModInstallStatusUninstall(t *testing.T) {
	_, e, out, errb := newFake(t)
	home := t.TempDir()
	env := e.Getenv
	e.Getenv = func(k string) string {
		switch k {
		case "HOME":
			return home
		case "XDG_DATA_HOME":
			return ""
		}
		return env(k)
	}
	rc := &recClaude{}
	old := modClaude
	modClaude = rc
	t.Cleanup(func() { modClaude = old })

	if code := run(e, "mod", "install"); code != 0 {
		t.Fatalf("install: %d %s", code, errb)
	}
	dir := filepath.Join(home, ".local", "share", "comebye", "mods")
	if _, err := os.Stat(filepath.Join(dir, "comebye-hub", "hooks", "register.tsx")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "folder: "+dir) {
		t.Fatalf("out = %s", out)
	}
	out.Reset()
	if code := run(e, "mod", "status", "--json", "--no-claude"); code != 0 {
		t.Fatalf("status: %d %s", code, errb)
	}
	var r modkit.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || len(r.Files) == 0 {
		t.Fatalf("status json: %v %s", err, out)
	}
	for _, f := range r.Files {
		if f.State != modkit.Current {
			t.Errorf("%s is %s", f.Rel, f.State)
		}
	}
	if code := run(e, "mod", "status", "--force"); code != 2 {
		t.Fatalf("status --force: %d", code)
	}
	if code := run(e, "mod", "install", "--with", "bogus"); code == 0 {
		t.Fatal("bogus mod accepted")
	}
	if code := run(e, "mod", "uninstall"); code != 0 {
		t.Fatalf("uninstall: %d %s", code, errb)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("folder stayed: %v", err)
	}
}

func TestModBand(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["GET /api/ledger"] = answer{body: `{"entries":[
	 {"id":"h0","agent_id":"laharl/w9:p1","status":"running"},
	 {"id":"h1","agent_id":"laharl/w1:p1","status":"running","progress":{"text":"pushed, CI running","phase":"ci"},
	  "land_check":{"state":"landed","ok":true,"ci":{"state":"running"}}}],
	 "roles":{"laharl/w1:p1":{"role":"worker"}}}`}
	f.answers["GET /api/todos"] = answer{body: `{"todos":[
	 {"id":"t1","question":"may I delete the old key?","state":"open","waiting":{"id":"laharl/w1:p1"}},
	 {"id":"t2","question":"someone else's","state":"open","waiting":{"id":"laharl/w2:p1"}}]}`}
	f.answers["GET /api/queue"] = answer{body: `{"queue":[{"id":"q1","state":"pending"},{"id":"q2","state":"delivered"}]}`}
	if code := run(e, "mod", "band"); code != 0 {
		t.Fatalf("band: %d %s", code, errb)
	}
	var b bandState
	if err := json.Unmarshal(out.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Todos != 1 || b.FirstTodo != "may I delete the old key?" || b.Queued != 1 || b.Coordinator || b.Headline != "" {
		t.Fatalf("band = %+v", b)
	}
	if b.Handoff == nil || b.Handoff.ID != "h1" || b.Handoff.CI != "running" || b.Handoff.Landing != "landed" || b.Handoff.Phase != "ci" {
		t.Fatalf("handoff = %+v", b.Handoff)
	}
	if r := f.find("GET", "/api/queue"); r == nil || !strings.Contains(r.query, "agent=") {
		t.Fatalf("queue asked without the agent: %+v", r)
	}
}

func TestModBandCoordinatorAndFailures(t *testing.T) {
	f, e, out, _ := newFake(t)
	f.answers["GET /api/ledger"] = answer{body: `{"entries":[],"roles":{"laharl/w1:p1":{"role":"coordinator"}}}`}
	f.answers["GET /api/todos"] = answer{body: `{"todos":[{"id":"t1","question":"q","state":"open"},{"id":"t2","question":"q","state":"open"}]}`}
	f.answers["GET /api/queue"] = answer{code: 500, body: `{"error":"boom"}`}
	f.answers["GET /api/brief"] = answer{body: `{"brief":{"markdown":"\n**Tue 19:38. Nothing waits on you.** Usage landed.\n\n## Needs you\n"}}`}
	if code := run(e, "mod", "band"); code != 0 {
		t.Fatalf("band: %d", code)
	}
	var b bandState
	json.Unmarshal(out.Bytes(), &b)
	if !b.Coordinator || b.OpenTodos != 2 || b.Todos != 0 || b.Queued != 0 || b.Headline != "Tue 19:38. Nothing waits on you. Usage landed." || b.Handoff != nil {
		t.Fatalf("band = %+v", b)
	}
	// Without an answer from whoami there is no band at all (non-zero exit).
	f.answers["GET /api/whoami"] = answer{code: 500, body: `{}`}
	out.Reset()
	if code := run(e, "mod", "band"); code == 0 {
		t.Fatal("band without a whoami succeeded")
	}
}

func TestHeadline(t *testing.T) {
	if got := headline("# Title here\n\nbody"); got != "Title here" {
		t.Fatal(got)
	}
	if got := headline(strings.Repeat("x", 400)); len([]rune(got)) != 160 {
		t.Fatal(len(got))
	}
	if headline("\n\n") != "" {
		t.Fatal("empty")
	}
}

func TestModInstallIfInstalled(t *testing.T) {
	_, e, out, errb := newFake(t)
	home := t.TempDir()
	env := e.Getenv
	e.Getenv = func(k string) string {
		switch k {
		case "HOME":
			return home
		case "XDG_DATA_HOME":
			return ""
		}
		return env(k)
	}
	rc := &recClaude{}
	old := modClaude
	modClaude = rc
	t.Cleanup(func() { modClaude = old })
	dir := filepath.Join(home, ".local", "share", "comebye", "mods")

	if code := run(e, "mod", "install", "--if-installed"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if !strings.Contains(out.String(), "not installed here") {
		t.Fatalf("out: %s", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || len(rc.calls) != 0 {
		t.Fatalf("something was done on a host without mods: %v %v", err, rc.calls)
	}
	if code := run(e, "mod", "install", "--if-installed", "--with", "coord-window"); code != 2 {
		t.Fatalf("with: %d", code)
	}
	if code := run(e, "mod", "status", "--if-installed"); code != 2 {
		t.Fatalf("status: %d", code)
	}
	// Installed, then refreshed: no claude calls the second time.
	if code := run(e, "mod", "install"); code != 0 {
		t.Fatalf("install: %d %s", code, errb)
	}
	rc.calls = nil
	out.Reset()
	if code := run(e, "mod", "install", "--if-installed"); code != 0 || len(rc.calls) != 0 {
		t.Fatalf("refresh: %d %v %s", code, rc.calls, errb)
	}
	if !strings.Contains(out.String(), "files current") {
		t.Fatalf("out: %s", out)
	}
}
