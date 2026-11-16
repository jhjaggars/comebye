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
