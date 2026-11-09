package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/digest"
)

const (
	whoamiMe   = `{"host":"laharl","role":"agent","agent":"laharl/w21:p1","ledger":false,"hub":"https://hub"}`
	rolesCoord = `{"roles":{"laharl/w21:p1":{"role":"coordinator","name":"coordinator"}}}`
	rolesNone  = `{"roles":{"laharl/w99:p1":{"role":"coordinator"}}}`
	coordState = `{"threshold":150000,"last":{"at":"2026-11-02T09:00:00Z","name":"coordinator","trigger":"briefing","tokens":161000,"digest_landed":true},"coordinators":[]}`
	briefJSON  = `{"brief":{"markdown":"# Briefing\n\nNeeds you: t1","by":"coordinator","at":"2026-11-03T08:00:00Z"}}`
	todosJSON  = `{"todos":[{"id":"t1","question":"Which key?","kind":"decision","state":"open","created_at":"2026-11-03T07:00:00Z","waiting":{"id":"laharl/w9:p1","name":"keysmith"}}]}`
	coordLedgerJSON = `{"entries":[
 {"id":"h1","host":"laharl","repo_name":"comebye","branch":"feat/a","status":"running","agent_name":"alpha","created_at":"2026-11-03T06:00:00Z","live":{"present":true,"agent_status":"working"}},
 {"id":"h2","host":"laharl","repo_name":"comebye","branch":"feat/b","status":"finished","agent_name":"beta","created_at":"2026-11-02T06:00:00Z","finished_at":"2026-11-03T05:00:00Z"},
 {"id":"h3","host":"laharl","repo_name":"comebye","branch":"feat/old","status":"removed","created_at":"2026-10-01T06:00:00Z","removed_at":"2026-10-02T06:00:00Z"}
 ]}`
)

func coordAnswers(roles string) map[string]answer {
	return map[string]answer{
		"GET /api/whoami": {200, whoamiMe}, "GET /api/roles": {200, roles}, "GET /api/coordinator": {200, coordState},
		"GET /api/brief": {200, briefJSON}, "GET /api/todos": {200, todosJSON}, "GET /api/ledger": {200, coordLedgerJSON},
	}
}

func coordEnvWithState(t *testing.T, answers map[string]answer, stateMD string) (*Env, *strings.Builder, string) {
	t.Helper()
	e, out, _, _ := peerEnv(t, map[string]string{"HOME": t.TempDir()}, answers)
	dir := filepath.Join(StateDir(e.Getenv), "coordinator")
	os.MkdirAll(dir, 0o700)
	if stateMD != "" {
		os.WriteFile(filepath.Join(dir, "state.md"), []byte(stateMD), 0o600)
	}
	sb := &strings.Builder{}
	e.Stdout = sb
	_ = out
	return e, sb, dir
}

func TestCoordinatorDigestPrintsTheSections(t *testing.T) {
	e, out, dir := coordEnvWithState(t, coordAnswers(rolesCoord), "# Coordinator state\n\n## Notes\n- remember the keys\n")
	if code := run(e, "coordinator", "digest"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	for _, want := range []string{
		digest.Marker, "comebye playbook --harness claude", "Needs you: t1", "`t1` [decision, open", "(waiting: keysmith)",
		"`h1` comebye feat/a — worker alpha — running: agent working", "`h2` comebye feat/b", "waiting for the user's OK to land",
		"remember the keys", filepath.Join(dir, "state.md"), "the previous fresh start was",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q:\n%s", want, got)
		}
	}
	// h3 was removed long before the last reset: not an event, not running.
	if strings.Contains(got, "feat/old") {
		t.Error("an old removed handoff is in the digest")
	}
	if len(got) > digest.MaxBytes {
		t.Errorf("digest is %d bytes", len(got))
	}
}

func TestCoordinatorDigestHook(t *testing.T) {
	e, out, _ := coordEnvWithState(t, coordAnswers(rolesCoord), "notes\n")
	if code := run(e, "coordinator", "digest", "--hook"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var h struct {
		SystemMessage string `json:"systemMessage"`
		Out           struct {
			Event string `json:"hookEventName"`
			Ctx   string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &h); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	if h.Out.Event != "SessionStart" || !strings.Contains(h.Out.Ctx, digest.Marker) || !strings.Contains(h.SystemMessage, "fresh start") {
		t.Fatalf("hook output = %+v", h)
	}

	// The startup note carries the marker the hub looks for, no digest.
	e, out, _ = coordEnvWithState(t, coordAnswers(rolesCoord), "")
	if code := run(e, "coordinator", "digest", "--hook", "--note"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	h.SystemMessage, h.Out.Ctx = "", ""
	json.Unmarshal([]byte(out.String()), &h)
	if !strings.Contains(h.Out.Ctx, "comebye-hook:v1") || strings.Contains(h.Out.Ctx, digest.Marker) || h.SystemMessage != "" {
		t.Fatalf("note = %+v", h)
	}
}

// The hook is for the coordinator alone: any other agent that somehow runs
// it gets nothing.
func TestCoordinatorDigestHookIsForTheCoordinatorOnly(t *testing.T) {
	e, out, _ := coordEnvWithState(t, coordAnswers(rolesNone), "notes\n")
	if code := run(e, "coordinator", "digest", "--hook"); code != 0 || out.Len() != 0 {
		t.Fatalf("exit %d, printed %q", code, out)
	}
}

// With the hub out of reach the hook still gives the state file and says
// what is missing; it never fails.
func TestCoordinatorDigestHookWithoutTheHub(t *testing.T) {
	e, out, _ := coordEnvWithState(t, map[string]answer{}, "# state\n- keep going\n")
	if code := run(e, "coordinator", "digest", "--hook"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "keep going") || !strings.Contains(out.String(), "Not available") {
		t.Fatalf("output = %s", out)
	}
}

func TestCoordinatorInstallHook(t *testing.T) {
	e, out, dir := coordEnvWithState(t, nil, "")
	if code := run(e, "coordinator", "install-hook", "--bin", "/opt/comebye bin/comebye"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks struct {
			SessionStart []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type, Command string
					Timeout       int
				} `json:"hooks"`
			}
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	ss := s.Hooks.SessionStart
	if len(ss) != 2 || ss[0].Matcher != "clear" || ss[1].Matcher != "startup|resume|compact" {
		t.Fatalf("settings = %s", b)
	}
	// The path with a space is quoted for the shell; the digest is the clear
	// hook, the note the others.
	if c := ss[0].Hooks[0].Command; c != `'/opt/comebye bin/comebye' coordinator digest --hook` || ss[0].Hooks[0].Type != "command" || ss[0].Hooks[0].Timeout == 0 {
		t.Fatalf("clear hook = %+v", ss[0].Hooks[0])
	}
	if c := ss[1].Hooks[0].Command; !strings.HasSuffix(c, "coordinator digest --hook --note") {
		t.Fatalf("note hook = %s", c)
	}
	// Only the coordinator directory is touched, and the user is told so.
	if !strings.Contains(out.String(), "scoped to the coordinator") {
		t.Fatalf("output = %s", out)
	}
	if code := run(e, "coordinator", "install-hook", "--uninstall"); code != 0 {
		t.Fatal("uninstall")
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err == nil {
		t.Fatal("uninstall left the file")
	}
	// A relative binary path is refused.
	if code := run(e, "coordinator", "install-hook", "--bin", "comebye"); code != 2 {
		t.Fatalf("relative --bin: exit %d", code)
	}
}

func TestCoordinatorResetIsTheUsers(t *testing.T) {
	// Without a login: refused before any request.
	e, _, errb, reqs := peerEnv(t, nil, nil)
	if code := run(e, "coordinator", "reset"); code != 2 || !strings.Contains(errb.String(), "login") || len(*reqs) != 0 {
		t.Fatalf("no login: %d %s", code, errb)
	}
	// With one: as the user, never with the agent token.
	e, out, _, reqs := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{
		"POST /api/coordinator/reset": {200, `{"tokens":161000,"digest_landed":true,"record":{"agent_id":"laharl/w21:p1","name":"coordinator"}}`},
	})
	if code := run(e, "coordinator", "reset", "laharl/w21:p1"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	r := (*reqs)[0]
	if r.hdr.Get(headerToken) != "" || r.hdr.Get("Tailscale-User-Login") != "me@example.com" || !strings.Contains(r.body, `"agent_id":"laharl/w21:p1"`) {
		t.Fatalf("request = %+v", r)
	}
	if !strings.Contains(out.String(), "cleared coordinator") || !strings.Contains(out.String(), "161k") {
		t.Fatalf("output = %s", out)
	}
	// The server's refusal reaches the user.
	e, _, errb, _ = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{
		"POST /api/coordinator/reset": {409, `{"code":"queued","message":"the hub holds messages for the coordinator"}`},
	})
	if code := run(e, "coordinator", "reset"); code != 1 || !strings.Contains(errb.String(), "holds messages") {
		t.Fatalf("exit %d %s", code, errb)
	}
}

func TestCoordinatorStatus(t *testing.T) {
	e, out, _, _ := peerEnv(t, nil, map[string]answer{"GET /api/coordinator": {200, coordState}})
	if code := run(e, "coordinator", "status"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"150k tokens", "last fresh start: 24h ago, briefing, at 161k tokens", "digest landed: true"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}
