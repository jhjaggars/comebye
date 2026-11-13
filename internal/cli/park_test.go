package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func lastReq(reqs *[]recorded, method, path string) *recorded {
	for i := len(*reqs) - 1; i >= 0; i-- {
		if r := (*reqs)[i]; r.method == method && r.path == path {
			return &r
		}
	}
	return nil
}

func TestParkResumeCommands(t *testing.T) {
	answers := map[string]answer{
		"GET /api/agents":               {200, agentsJSON},
		"GET /api/whoami":               {200, `{"host":"laharl","role":"agent"}`},
		"POST /api/hosts/laharl/park":   {200, `{"record":{"id":"pk_0123456789","name":"fix-a","kind":"pi","worktree":"/home/u/.worktrees/proj/fix-a","branch":"fix/a"},"closed":"workspace","scratch":"moved 2.0 MB to disk"}`},
		"GET /api/hosts/laharl/parked":  {200, `{"host":"laharl","parked":[{"id":"pk_0123456789","name":"fix-a","kind":"pi","branch":"fix/a","parked_at":"2026-10-21T09:00:00Z"}]}`},
		"POST /api/hosts/laharl/resume": {200, `{"agent":{"id":"laharl/w7:p1","name":"fix-a"},"note":"Comebye: you were parked","scratch":"scratchpad moved back"}`},
	}
	// Closing an agent is the user's: without a login nothing is sent.
	e, out, errb, reqs := peerEnv(t, nil, answers)
	if code := run(e, "park", "laharl/w3:p1"); code != 2 || !strings.Contains(errb.String(), "login") {
		t.Fatalf("no login: %d %s", code, errb)
	}
	if lastReq(reqs, "POST", "/api/hosts/laharl/park") != nil {
		t.Fatal("parked without a login")
	}

	e, out, errb, reqs = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	if code := run(e, "park", "laharl/w3:p1", "--dirty"); code != 0 ||
		!strings.Contains(out.String(), "Parked fix-a (pi) as pk_0123456789") || !strings.Contains(out.String(), "comebye resume pk_0123456789 --host laharl") {
		t.Fatalf("park: %d %s %s", code, out, errb)
	}
	pr := lastReq(reqs, "POST", "/api/hosts/laharl/park")
	var body map[string]any
	json.Unmarshal([]byte(pr.body), &body)
	if body["agent"] != "w3:p1" || body["name"] != "fix-a" || body["dirty"] != true || pr.hdr.Get("Tailscale-User-Login") != "me@example.com" {
		t.Fatalf("body = %v, headers %v", body, pr.hdr)
	}

	// Resume by name finds the record in the host's parked list.
	out.Reset()
	if code := run(e, "resume", "fix-a", "--no-note"); code != 0 || !strings.Contains(out.String(), "Resumed fix-a as laharl/w7:p1") {
		t.Fatalf("resume: %d %s %s", code, out, errb)
	}
	body = nil
	json.Unmarshal([]byte(lastReq(reqs, "POST", "/api/hosts/laharl/resume").body), &body)
	if body["id"] != "pk_0123456789" || body["no_note"] != true {
		t.Fatalf("body = %v", body)
	}
	out.Reset()
	if code := run(e, "parked"); code != 0 || !strings.Contains(out.String(), "pk_0123456789  fix-a") {
		t.Fatalf("parked: %d %s", code, out)
	}
	errb.Reset()
	if code := run(e, "resume", "nobody"); code != 1 || !strings.Contains(errb.String(), `called "nobody"`) {
		t.Fatalf("unknown name: %d %s", code, errb)
	}
}

func TestPromptQueueToParkedAgent(t *testing.T) {
	answers := map[string]answer{
		"GET /api/agents":     {200, agentsJSON},
		"GET /api/whoami":     {200, `{"host":"laharl","role":"agent"}`},
		"GET /api/parked/all": {200, `{"hosts":[{"host":"laharl","online":true,"parked":[{"id":"pk_0123456789","name":"sleepy","kind":"claude","pane_id":"w5:p1"}]}]}`},
		"POST /api/queue":     {202, `{"queued":{"id":"q1"},"parked":true,"note":"sleepy is parked: your prompt waits (up to 7 days) and is delivered after it is resumed."}`},
	}
	e, out, errb, reqs := peerEnv(t, nil, answers)
	// Without --queue a parked agent is not prompted; the error says what to do.
	if code := run(e, "prompt", "sleepy", "hello"); code != 1 || !strings.Contains(errb.String(), "is parked") || !strings.Contains(errb.String(), "--queue") {
		t.Fatalf("no queue: %d %s", code, errb)
	}
	if lastReq(reqs, "POST", "/api/queue") != nil {
		t.Fatal("queued without --queue")
	}
	if code := run(e, "prompt", "sleepy", "hello", "--queue"); code != 0 || !strings.Contains(out.String(), "is parked: your prompt waits") {
		t.Fatalf("queue: %d %s %s", code, out, errb)
	}
	var body map[string]string
	json.Unmarshal([]byte(lastReq(reqs, "POST", "/api/queue").body), &body)
	if body["agent"] != "laharl/w5:p1" || body["name"] != "sleepy" || body["text"] != "hello" {
		t.Fatalf("body = %v", body)
	}
	// An unknown name is still the usual error.
	errb.Reset()
	if code := run(e, "prompt", "nobody", "hi", "--queue"); code != 1 || strings.Contains(errb.String(), "is parked") {
		t.Fatalf("unknown: %d %s", code, errb)
	}
}
