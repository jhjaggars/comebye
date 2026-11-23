package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/apipy"
)

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 on PATH")
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.py")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAPIHelpAndTypes(t *testing.T) {
	_, e, out, _ := newFake(t)
	for _, args := range [][]string{{"api"}, {"api", "--help"}, {"api", "run", "--help"}} {
		out.Reset()
		if code := run(e, args...); code != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
		for _, want := range []string{"comebye api run FILE|-", "cb.entries(", "cb.latest_reply(entry)", "Forbidden", "python3 is not on PATH", "mini steward check"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: no %q in the guide", args, want)
			}
		}
	}
	out.Reset()
	if code := run(e, "api", "types", "todo", "Entry"); code != 0 {
		t.Fatalf("types: exit %d", code)
	}
	for _, want := range []string{"Todo  (server.Todo)", "  question str", "Entry  (server.entryView)", "  report_at time?", "  newer_reply_age str"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("types: no %q in\n%s", want, out.String())
		}
	}
	_, e, _, errb := newFake(t)
	if code := run(e, "api", "types", "Nope"); code != 1 || !strings.Contains(errb.String(), `no record "Nope"`) || !strings.Contains(errb.String(), "QueuedPrompt") {
		t.Errorf("unknown record: exit %d, %s", code, errb.String())
	}
	_, e, _, errb = newFake(t)
	if code := run(e, "api", "frobnicate"); code != 2 {
		t.Errorf("unknown subcommand: exit %d, %s", code, errb.String())
	}
}

// The guide's example is valid Python (the e2e test runs it against a hub).
func TestAPIHelpExampleParses(t *testing.T) {
	needPython(t)
	script := helpExample(t)
	cmd := exec.Command("python3", "-c", "import ast,sys; ast.parse(sys.stdin.read())")
	cmd.Stdin = strings.NewReader(script)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the --help example does not parse: %v\n%s\n%s", err, b, script)
	}
}

// helpExample is the script in the guide's "mini steward check".
func helpExample(t *testing.T) string {
	t.Helper()
	_, body, ok := strings.Cut(apipy.Help, "cat > check.py <<'PY'\n")
	body, _, ok2 := strings.Cut(body, "\n  PY\n")
	if !ok || !ok2 {
		t.Fatal("no example script in the guide")
	}
	var b strings.Builder
	for _, l := range strings.Split(body, "\n") {
		b.WriteString(strings.TrimPrefix(l, "  ") + "\n")
	}
	return b.String()
}

func TestAPIRunReadsTheHub(t *testing.T) {
	needPython(t)
	f, e, out, errb := newFake(t)
	f.answers["GET /api/todos"] = answer{body: `{"todos":[{"id":"t0123456789","question":"Land it?","state":"open","created_at":"2026-10-22T08:00:00.123456789Z","waiting":{"id":"laharl/w3:p1","name":"fix-a"}}],"open":1,"answered":0,"auto":{}}`}
	f.answers["GET /api/removals"] = answer{code: 403, body: `{"code":"agent_forbidden","message":"left to the user"}`}
	script := writeScript(t, `
import os, sys, subprocess
import comebye as cb
print("who", cb.whoami().agent)
print("agents", len(cb.agents()), cb.agent("solo").host)
e = cb.entry("fix-a")
print("entry", e.status, e.live.agent_status, e.from_, repr(e.report_at))
t = cb.todos("open")[0]
print("todo", t.question, t.created_at.isoformat(), t.waiting.name)
try:
    t.title
except AttributeError as x:
    print("strict", "did you mean 'question'" in str(x))
try:
    cb.removals()
except cb.Forbidden as x:
    print("forbidden", x.status)
tok = open(os.path.join(os.environ["COMEBYE_STATE_DIR"], "agent-token")).read().strip()
print("token in env", any(tok in v for v in os.environ.values()))
print("args", sys.argv[1:])
sys.exit(7)
`)
	// The script reads COMEBYE_STATE_DIR itself to look for the token.
	t.Setenv("COMEBYE_STATE_DIR", e.Getenv("COMEBYE_STATE_DIR"))
	code := run(e, "api", "run", script, "one", "two")
	if code != 7 {
		t.Fatalf("exit %d, want the script's 7\nstdout: %s\nstderr: %s", code, out, errb)
	}
	for _, want := range []string{
		"who laharl/w1:p1",
		"agents 5 kube",
		"entry finished idle  None",
		"todo Land it? 2026-10-22T08:00:00.123456+00:00 fix-a",
		"strict True",
		"forbidden 403",
		"token in env False",
		"args ['one', 'two']",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s\nstderr: %s", want, out, errb)
		}
	}
	// Every request was a GET, as the agent, from this pane, with the token.
	if r := f.find("GET", "/api/removals"); r == nil || r.query != "dry=1" {
		t.Errorf("removals asked without dry=1: %+v", r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no requests")
	}
	for _, r := range f.reqs {
		if r.method != "GET" || r.hdr.Get(headerToken) != f.token || r.hdr.Get(headerAgent) != "w1:p1" || r.hdr.Get("User-Agent") != "comebye-api/1" {
			t.Errorf("request %s %s: agent %q, ua %q", r.method, r.path, r.hdr.Get(headerAgent), r.hdr.Get("User-Agent"))
		}
	}
}

func TestAPIRunHubErrorAndStdin(t *testing.T) {
	needPython(t)
	_, e, out, errb := newFake(t)
	e.Stdin = strings.NewReader("import comebye as cb\nprint('before')\ncb.todo('tnope')\nprint('after')\n")
	code := run(e, "api", "run", "-")
	if code != 1 || !strings.Contains(out.String(), "before") || strings.Contains(out.String(), "after") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if got := errb.String(); !strings.Contains(got, "comebye api: GET /api/todos/tnope: 404 no route /api/todos/tnope") || strings.Contains(got, "Traceback") {
		t.Errorf("stderr: %s", got)
	}
}

func TestAPIRunNoToken(t *testing.T) {
	needPython(t)
	_, e, out, errb := newFake(t)
	os.Remove(filepath.Join(e.Getenv("COMEBYE_STATE_DIR"), "agent-token"))
	script := writeScript(t, "import comebye as cb\nprint('runs')\ncb.whoami()\n")
	if code := run(e, "api", "run", script); code != 1 || !strings.Contains(out.String(), "runs") || !strings.Contains(errb.String(), "no agent token at") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

func TestAPIRunWithoutPython(t *testing.T) {
	_, e, _, errb := newFake(t)
	t.Setenv("PATH", t.TempDir())
	if code := run(e, "api", "run", writeScript(t, "print(1)\n")); code != 1 || !strings.Contains(errb.String(), "python3 is not on PATH") || !strings.Contains(errb.String(), "Nothing was run") {
		t.Fatalf("exit %d: %s", code, errb)
	}
}

func TestAPIRunMissingScript(t *testing.T) {
	needPython(t)
	_, e, _, errb := newFake(t)
	if code := run(e, "api", "run", filepath.Join(t.TempDir(), "nope.py")); code != 1 || !strings.Contains(errb.String(), "no such file") {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if code := run(e, "api", "run"); code != 2 {
		t.Fatalf("no script: exit %d", code)
	}
}

func TestAPIEnv(t *testing.T) {
	got := apiEnv([]string{"PATH=/bin", "PYTHONPATH=/x", "COMEBYE_API_TOKEN_FD=9", "COMEBYE_API_URL=http://evil", "HOME=/h"}, "/mod")
	want := []string{"PATH=/bin", "HOME=/h", "PYTHONPATH=/mod" + string(os.PathListSeparator) + "/x", "PYTHONDONTWRITEBYTECODE=1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("apiEnv = %q, want %q", got, want)
	}
}
