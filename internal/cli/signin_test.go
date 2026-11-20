package cli

import (
	"strings"
	"testing"
)

func TestSigninClaude(t *testing.T) {
	answers := map[string]answer{
		"GET /api/whoami":                      {200, `{"host":"kv1","role":"user"}`},
		"POST /api/hosts/kv1/signin":           {200, `{"id":"si_1","agent":"claude","state":"waiting_code","url":"https://claude.com/cai/oauth/authorize?x=1","code_input":true}`},
		"POST /api/hosts/kv1/signin/si_1/code": {200, `{"id":"si_1","state":"signed_in","method":"claude.ai"}`},
	}
	// Without a login nothing is sent.
	e, _, errb, reqs := peerEnv(t, nil, answers)
	if code := run(e, "signin", "claude"); code != 2 || !strings.Contains(errb.String(), "login") || len(*reqs) != 0 {
		t.Fatalf("no login: %d %s", code, errb)
	}

	e, out, errb, reqs := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	e.Stdin = strings.NewReader("SENTINEL-CODE-1\n")
	if code := run(e, "signin", "claude"); code != 0 || !strings.Contains(out.String(), "claude is signed in on kv1 (claude.ai)") {
		t.Fatalf("signin: %d out=%s err=%s", code, out, errb)
	}
	// The link goes to stderr (a prompt), the code is never printed.
	if !strings.Contains(errb.String(), "https://claude.com/cai/oauth/authorize?x=1") || strings.Contains(out.String()+errb.String(), "SENTINEL-CODE-1") {
		t.Fatalf("stderr = %s", errb)
	}
	if r := lastReq(reqs, "POST", "/api/hosts/kv1/signin/si_1/code"); r == nil || !strings.Contains(r.body, "SENTINEL-CODE-1") || r.hdr.Get("Tailscale-User-Login") != "me@example.com" {
		t.Fatalf("code request = %+v", r)
	}
}

func TestSigninClaudeFailureAndCancel(t *testing.T) {
	answers := map[string]answer{
		"POST /api/hosts/kv1/signin":           {200, `{"id":"si_1","state":"waiting_code","url":"https://claude.com/x"}`},
		"POST /api/hosts/kv1/signin/si_1/code": {200, `{"id":"si_1","state":"failed","reason":"code_rejected","message":"the code was not accepted"}`},
		"DELETE /api/hosts/kv1/signin/si_1":    {200, `{"id":"si_1","state":"failed","reason":"cancelled"}`},
	}
	e, _, errb, reqs := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	e.Stdin = strings.NewReader("WRONG\n")
	if code := run(e, "signin", "claude", "--host", "kv1"); code != 1 || !strings.Contains(errb.String(), "the code was not accepted") {
		t.Fatalf("failed: %d %s", code, errb)
	}
	// No code (stdin closed): the sign-in is cancelled.
	e, _, errb, reqs = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	if code := run(e, "signin", "claude", "--host", "kv1"); code != 1 || lastReq(reqs, "DELETE", "/api/hosts/kv1/signin/si_1") == nil {
		t.Fatalf("no code: %d %s", code, errb)
	}
	// A bad shape is refused by the host; the CLI cancels.
	answers["POST /api/hosts/kv1/signin/si_1/code"] = answer{400, `{"code":"bad_code","message":"the code is empty"}`}
	e, _, errb, reqs = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	e.Stdin = strings.NewReader("a b\n")
	if code := run(e, "signin", "claude", "--host", "kv1"); code != 1 || lastReq(reqs, "DELETE", "/api/hosts/kv1/signin/si_1") == nil || strings.Contains(errb.String(), "a b") {
		t.Fatalf("bad code: %d %s", code, errb)
	}
}

func TestSigninClaudeAttachesToRunning(t *testing.T) {
	answers := map[string]answer{
		"POST /api/hosts/kv1/signin":           {409, `{"code":"busy","message":"running","session":{"id":"si_9","state":"waiting_code","url":"https://claude.com/y"}}`},
		"POST /api/hosts/kv1/signin/si_9/code": {200, `{"id":"si_9","state":"signed_in"}`},
	}
	e, out, errb, _ := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	e.Stdin = strings.NewReader("GOOD\n")
	if code := run(e, "signin", "claude", "--host", "kv1"); code != 0 || !strings.Contains(out.String(), "signed in") || !strings.Contains(errb.String(), "already running") {
		t.Fatalf("attach: %d %s %s", code, out, errb)
	}
}
