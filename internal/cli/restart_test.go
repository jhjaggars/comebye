package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRestartCommand(t *testing.T) {
	ok := `{"agent":{"id":"laharl/w3:p1","pane_id":"w3:p1","name":"fix-a"},"session":"1fef1782","old_pid":11,"new_pid":22,"args":["--model","x"],"args_from":"process"}`
	answers := map[string]answer{
		"GET /api/agents":                {200, agentsJSON},
		"GET /api/whoami":                {200, `{"host":"laharl","role":"agent"}`},
		"POST /api/hosts/laharl/restart": {200, ok},
	}
	env := map[string]string{"COMEBYE_LOGIN": "me@example.com"}

	// Usage errors: no agent, two agents, no login. Nothing is sent.
	e, _, errb, reqs := peerEnv(t, nil, answers)
	if code := run(e, "restart"); code != 2 {
		t.Fatalf("no agent: %d %s", code, errb)
	}
	if code := run(e, "restart", "a", "b"); code != 2 {
		t.Fatalf("two agents: %d", code)
	}
	errb.Reset()
	if code := run(e, "restart", "laharl/w3:p1"); code != 2 || !strings.Contains(errb.String(), "login") {
		t.Fatalf("no login: %d %s", code, errb)
	}
	if lastReq(reqs, "POST", "/api/hosts/laharl/restart") != nil {
		t.Fatal("restarted without a login")
	}

	// The body and the plain output.
	e, out, errb, reqs := peerEnv(t, env, answers)
	if code := run(e, "restart", "laharl/w3:p1", "--force-schedule"); code != 0 {
		t.Fatalf("restart: %d %s", code, errb)
	}
	for _, w := range []string{"Restarted fix-a", "laharl/w3:p1", "session 1fef1782", "pid 11 -> 22"} {
		if !strings.Contains(out.String(), w) {
			t.Fatalf("output lacks %q: %s", w, out)
		}
	}
	if strings.Contains(out.String(), "could not be recovered") {
		t.Fatalf("flags were recovered: %s", out)
	}
	rr := lastReq(reqs, "POST", "/api/hosts/laharl/restart")
	var body map[string]any
	json.Unmarshal([]byte(rr.body), &body)
	if body["agent"] != "w3:p1" || body["name"] != "fix-a" || body["force_schedule"] != true || rr.hdr.Get("Tailscale-User-Login") != "me@example.com" {
		t.Fatalf("body = %v, headers %v", body, rr.hdr)
	}

	// --json prints the raw result.
	out.Reset()
	if code := run(e, "restart", "laharl/w3:p1", "--json"); code != 0 {
		t.Fatalf("json: %d %q", code, out)
	}
	var raw map[string]any
	if json.Unmarshal(out.Bytes(), &raw) != nil || raw["new_pid"] != float64(22) || raw["args_from"] != "process" {
		t.Fatalf("json output: %q", out)
	}
	body = nil
	json.Unmarshal([]byte(lastReq(reqs, "POST", "/api/hosts/laharl/restart").body), &body)
	if body["force_schedule"] != false {
		t.Fatalf("force_schedule default: %v", body)
	}
}

func TestRestartUnrecoveredFlagsAndRefusals(t *testing.T) {
	env := map[string]string{"COMEBYE_LOGIN": "me@example.com"}
	answers := map[string]answer{
		"GET /api/agents":                {200, agentsJSON},
		"GET /api/whoami":                {200, `{"host":"laharl","role":"agent"}`},
		"POST /api/hosts/laharl/restart": {200, `{"agent":{"id":"laharl/w3:p1","pane_id":"w3:p1","name":"fix-a"},"session":"s1","args_from":"session"}`},
	}
	e, out, errb, _ := peerEnv(t, env, answers)
	if code := run(e, "restart", "laharl/w3:p1"); code != 0 || !strings.Contains(out.String(), "flags could not be recovered") {
		t.Fatalf("session-only: %d %s %s", code, out, errb)
	}

	for code, want := range map[string]string{
		"agent_working":      "when it is idle",
		"background_task":    "background task",
		"schedule_pending":   "--force-schedule",
		"no_session":         "no known session",
		"dialog_open":        "dialog is open",
		"exit_not_confirmed": "nothing was killed",
	} {
		status := 409
		if code == "exit_not_confirmed" {
			status = 504
		}
		answers["POST /api/hosts/laharl/restart"] = answer{status, `{"code":"` + code + `","message":"server words"}`}
		e, _, errb, _ := peerEnv(t, env, answers)
		if c := run(e, "restart", "laharl/w3:p1"); c != 1 || !strings.Contains(errb.String(), want) || strings.Contains(errb.String(), "server words") {
			t.Errorf("%s: %d %s", code, c, errb)
		}
	}
	// An unknown code falls back to the server's own message.
	answers["POST /api/hosts/laharl/restart"] = answer{409, `{"code":"newthing","message":"server words"}`}
	e, _, errb, _ = peerEnv(t, env, answers)
	if c := run(e, "restart", "laharl/w3:p1"); c != 1 || !strings.Contains(errb.String(), "server words") {
		t.Errorf("unknown code: %d %s", c, errb)
	}
}

func TestRestartAllCommand(t *testing.T) {
	env := map[string]string{"COMEBYE_LOGIN": "me@example.com"}
	bulk := `{"host":"laharl","kind":"pi","results":[
	 {"agent":"laharl/w1:p1","pane_id":"w1:p1","name":"one","outcome":"restarted","old_pid":11,"new_pid":22,"flags_recovered":false},
	 {"agent":"laharl/w2:p1","pane_id":"w2:p1","name":"busy","outcome":"refused","code":"agent_working","message":"x"},
	 {"agent":"laharl/w3:p1","pane_id":"w3:p1","name":"boss","outcome":"skipped","code":"coordinator","message":"a coordinator is never restarted in bulk; restart it by name"},
	 {"agent":"laharl/w4:p1","pane_id":"w4:p1","outcome":"restarted","old_pid":5,"new_pid":6,"flags_recovered":true}],
	 "restarted":2,"would_restart":0,"refused":1,"failed":0,"skipped":1}`
	answers := map[string]answer{
		"GET /api/whoami":                     {200, `{"host":"laharl","role":"agent"}`},
		"POST /api/hosts/laharl/restart/bulk": {200, bulk},
	}
	// Usage: --all needs kind and host and no agent; the other flags need --all.
	e, _, errb, reqs := peerEnv(t, env, answers)
	for _, a := range [][]string{
		{"restart", "--all"}, {"restart", "--all", "--kind", "pi"}, {"restart", "--all", "--kind", "pi", "--host", "laharl", "x"},
		{"restart", "--kind", "pi", "x"}, {"restart", "--dry-run", "x"},
	} {
		if code := run(e, a...); code != 2 {
			t.Fatalf("%v = %d %s", a, code, errb)
		}
	}
	if lastReq(reqs, "POST", "/api/hosts/laharl/restart/bulk") != nil {
		t.Fatal("a usage error sent a request")
	}
	e, _, errb, _ = peerEnv(t, nil, answers)
	if code := run(e, "restart", "--all", "--kind", "pi", "--host", "laharl"); code != 2 || !strings.Contains(errb.String(), "login") {
		t.Fatalf("no login: %d %s", code, errb)
	}

	// One line per agent, then a total.
	e, out, errb, reqs := peerEnv(t, env, answers)
	if code := run(e, "restart", "--all", "--kind", "pi", "--host", "laharl", "--dry-run"); code != 0 {
		t.Fatalf("bulk: %d %s", code, errb)
	}
	var body map[string]any
	rr := lastReq(reqs, "POST", "/api/hosts/laharl/restart/bulk")
	json.Unmarshal([]byte(rr.body), &body)
	if body["kind"] != "pi" || body["dry_run"] != true || rr.hdr.Get("Tailscale-User-Login") != "me@example.com" {
		t.Fatalf("body = %v, headers %v", body, rr.hdr)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("want 4 agent lines and a total: %q", out)
	}
	for i, w := range []string{
		"restarted  laharl/w1:p1 (one): pid 11 -> 22, flags not recovered",
		"refused  laharl/w2:p1 (busy): it is working or blocked",
		"skipped  laharl/w3:p1 (boss): a coordinator",
		"restarted  laharl/w4:p1: pid 5 -> 6",
		"0 would restart, 1 refused, 0 failed, 1 skipped (4 pi agents on laharl)",
	} {
		if !strings.Contains(lines[i], w) {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}

	// --json prints the raw answer.
	out.Reset()
	if code := run(e, "restart", "--all", "--kind", "pi", "--host", "laharl", "--json"); code != 0 {
		t.Fatalf("json: %d", code)
	}
	var raw map[string]any
	if json.Unmarshal(out.Bytes(), &raw) != nil || raw["restarted"] != float64(2) {
		t.Fatalf("json output: %q", out)
	}

	// A failed agent makes the exit status 1.
	answers["POST /api/hosts/laharl/restart/bulk"] = answer{200, `{"results":[{"agent":"laharl/w1:p1","outcome":"failed","code":"start_failed","message":"boom"}],"failed":1}`}
	e, out, _, _ = peerEnv(t, env, answers)
	if code := run(e, "restart", "--all", "--kind", "pi", "--host", "laharl"); code != 1 || !strings.Contains(out.String(), "FAILED  laharl/w1:p1: boom") {
		t.Fatalf("failed: %d %q", code, out)
	}
}
