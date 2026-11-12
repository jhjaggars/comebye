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
	whoamiMe        = `{"host":"laharl","role":"agent","agent":"laharl/w21:p1","ledger":false,"hub":"https://hub"}`
	rolesCoord      = `{"roles":{"laharl/w21:p1":{"role":"coordinator","name":"coordinator"}}}`
	rolesNone       = `{"roles":{"laharl/w99:p1":{"role":"coordinator"}}}`
	coordState      = `{"threshold":150000,"last":{"at":"2026-11-02T09:00:00Z","name":"coordinator","trigger":"briefing","tokens":161000,"digest_landed":true},"coordinators":[]}`
	briefJSON       = `{"brief":{"markdown":"# Briefing\n\nNeeds you: t1","by":"coordinator","at":"2026-11-03T08:00:00Z"}}`
	todosJSON       = `{"todos":[{"id":"t1","question":"Which key?","kind":"decision","state":"open","created_at":"2026-11-03T07:00:00Z","waiting":{"id":"laharl/w9:p1","name":"keysmith"}}]}`
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

// The start-up note is left before the role is recorded (the preset records
// it after the start), so it does not depend on the role.
func TestCoordinatorNoteDoesNotNeedTheRole(t *testing.T) {
	e, out, _ := coordEnvWithState(t, coordAnswers(rolesNone), "")
	if code := run(e, "coordinator", "digest", "--hook", "--note"); code != 0 || !strings.Contains(out.String(), "comebye-hook:v1") {
		t.Fatalf("exit %d, printed %q", code, out)
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

const sampleReport = `## Summary
Built the reset. It works.

## Changes
- a.go
- b.go

## Verification
Verified live: x

## Follow-ups
None.

## Decisions for the user
1. Install the hook?
   Recommend: 1
`

func TestReadReportSummary(t *testing.T) {
	got := summarizeReport(sampleReport, "h1")
	for _, want := range []string{"## Summary\nBuilt the reset", "## Decisions for the user\n1. Install the hook?", "Changes: 2 lines", "Follow-ups: none", "comebye read h1 --report"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "a.go") || strings.Contains(got, "Verified live") {
		t.Errorf("summary carries other sections:\n%s", got)
	}
	if len(got) >= len(sampleReport)+120 {
		t.Errorf("summary is not smaller than the report")
	}
	// A report without sections is cut to its first lines; "Decisions: None." is not shown.
	long := strings.Repeat("line\n", 40)
	if g := summarizeReport(long, "h2"); strings.Count(g, "line") != 12 || !strings.Contains(g, "--report") {
		t.Errorf("sectionless = %q", g)
	}
	if g := summarizeReport("## Summary\nok\n\n## Decisions for the user\nNone.\n", "h3"); strings.Contains(g, "## Decisions") || !strings.Contains(g, "Decisions for the user: none") {
		t.Errorf("none decisions = %q", g)
	}
	// "None." with more text, and a sub-heading with a list after it: still none.
	for _, body := range []string{
		"None. All are decided (t8fa20f7215 and the earlier answers).\n",
		"None.\n\n### Answers\n1. one\n2. two\n",
	} {
		if g := summarizeReport("## Summary\nok\n\n## Decisions for the user\n"+body, "h4"); strings.Contains(g, "## Decisions") || strings.Contains(g, "Answers") || !strings.Contains(g, "Decisions for the user: none") {
			t.Errorf("none decisions %q = %q", body, g)
		}
	}
	// A sub-heading ends the decisions; what came before it is still shown.
	if g := summarizeReport("## Decisions for the user\n1. Which?\n### Notes\nx\n", "h5"); !strings.Contains(g, "1. Which?") || strings.Contains(g, "Notes") {
		t.Errorf("decisions cut at sub-heading = %q", g)
	}
}

func TestReadSummaryFlag(t *testing.T) {
	ledger := `{"entries":[{"id":"h1","agent_id":"laharl/w9:p1","agent_name":"w","repo_name":"r","branch":"b","status":"finished","final_report":` + string(mustJSON(sampleReport)) + `}]}`
	e, out, _, _ := peerEnv(t, nil, map[string]answer{"GET /api/ledger": {200, ledger}})
	if code := run(e, "read", "h1", "--report", "--summary"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "Summary of the final report of w on r/b") || strings.Contains(out.String(), "a.go") {
		t.Fatalf("output = %s", out)
	}
	e, _, errb, _ := peerEnv(t, nil, nil)
	if code := run(e, "read", "h1", "--summary"); code != 2 || !strings.Contains(errb.String(), "--report") {
		t.Fatalf("--summary alone: %d %s", code, errb)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestReadReportShowsHubChecked(t *testing.T) {
	ledger := `{"entries":[{"id":"h1","agent_id":"laharl/w9:p1","agent_name":"w","repo_name":"r","branch":"b","status":"finished","final_report":"## Summary\nPushed. CI green."}]}`
	hub := `{"id":"h1","items":[{"kind":"pushed","state":"bad","text":"the report says pushed, but 2 commit(s) are on no remote"},{"kind":"worktree","state":"ok","text":"the worktree is clean"}],"contradictions":1}`
	e, out, _, _ := peerEnv(t, nil, map[string]answer{"GET /api/ledger": {200, ledger}, "GET /api/ledger/h1/hubcheck": {200, hub}})
	if code := run(e, "read", "h1", "--report"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Pushed. CI green.", "Hub-checked: 1 claim(s) in the report CONTRADICT", "✗ the report says pushed, but 2 commit(s)", "✓ the worktree is clean"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A hub that cannot answer leaves the report as it was.
	e, out, _, _ = peerEnv(t, nil, map[string]answer{"GET /api/ledger": {200, ledger}})
	if code := run(e, "read", "h1", "--report"); code != 0 || strings.Contains(out.String(), "Hub-checked") || !strings.Contains(out.String(), "Pushed. CI green.") {
		t.Fatalf("no hubcheck route: %d\n%s", code, out)
	}
	e, out, _, _ = peerEnv(t, nil, map[string]answer{"GET /api/ledger": {200, ledger}, "GET /api/ledger/h1/hubcheck": {200, hub}})
	if code := run(e, "read", "h1", "--report", "--json"); code != 0 || !strings.Contains(out.String(), `"hub_checked"`) || !strings.Contains(out.String(), `"contradictions": 1`) {
		t.Fatalf("json: %d\n%s", code, out)
	}
}
