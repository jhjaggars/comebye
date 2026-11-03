package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// peerEnv runs the CLI against a server that records requests and
// answers by "METHOD path".
func peerEnv(t *testing.T, env map[string]string, answers map[string]answer) (*Env, *bytes.Buffer, *bytes.Buffer, *[]recorded) {
	t.Helper()
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()})
		a, ok := answers[r.Method+" "+r.URL.Path]
		if !ok {
			a = answer{404, `{"code":"not_found"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.code)
		io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	m := map[string]string{"COMEBYE_URL": srv.URL, "COMEBYE_STATE_DIR": t.TempDir()}
	for k, v := range env {
		m[k] = v
	}
	var out, errb bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
		Getenv:   func(k string) string { return m[k] },
		Hostname: func() (string, error) { return "laharl", nil },
		Now:      func() time.Time { return time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC) }}
	return e, &out, &errb, &reqs
}

const peersJSON = `{"version":"v1.2.0","peers":[
 {"name":"kube","urls":["https://kube.t.ts.net"],"source":"static","status":"ok","agents":3,"version":"v1.2.0"},
 {"name":"w1","urls":["https://w1.t.ts.net","https://100.64.0.9"],"source":"dynamic","added_by":"me@example.com","added_at":"2026-11-02T10:00:00Z","tag":"tag:worker","status":"ok","agents":1,"version":"v1.1.0","version_mismatch":true},
 {"name":"w2","urls":["https://w2.t.ts.net"],"source":"dynamic","status":"down","error":"cannot resolve w2.t.ts.net","agents":0}
]}`

func TestPeerListShowsVersions(t *testing.T) {
	e, out, _, reqs := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{"GET /api/peers": {200, peersJSON}})
	if code := run(e, "peer", "list"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	for _, want := range []string{"kube", "static", "v1.2.0", "w1", "dynamic", "v1.1.0 (differs from the hub's v1.2.0)", "w2", "unknown", "peer w2 is down: cannot resolve"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	// As the user, never as an agent: no token, the login header instead.
	h := (*reqs)[0].hdr
	if h.Get(headerToken) != "" || h.Get("Tailscale-User-Login") != "me@example.com" {
		t.Fatalf("headers = %v", h)
	}
	e, out, _, _ = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{"GET /api/peers": {200, peersJSON}})
	if code := run(e, "peer", "list", "--json"); code != 0 || !strings.Contains(out.String(), `"version_mismatch": true`) {
		t.Fatalf("json: %d %s", code, out)
	}
}

func TestPeerAddRemove(t *testing.T) {
	e, out, _, reqs := peerEnv(t, map[string]string{"COMEBYE_TS_ALLOW_LOGIN": "me@example.com,you@example.com"}, map[string]answer{
		"POST /api/peers":      {201, `{"name":"w1"}`},
		"DELETE /api/peers/w1": {204, ``},
	})
	if code := run(e, "peer", "add", "w1", "https://w1.t.ts.net|https://100.64.0.9", "--tag", "tag:worker"); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	r := (*reqs)[0]
	if r.method != "POST" || r.hdr.Get("Tailscale-User-Login") != "me@example.com" ||
		!strings.Contains(r.body, `"name":"w1"`) || !strings.Contains(r.body, `"https://w1.t.ts.net","https://100.64.0.9"`) || !strings.Contains(r.body, `"tag":"tag:worker"`) {
		t.Fatalf("add request = %+v", r)
	}
	if code := run(e, "peer", "remove", "w1"); code != 0 || !strings.Contains(out.String(), "removed") {
		t.Fatalf("remove: %d %s", code, out)
	}
}

func TestPeerErrorsAreReadable(t *testing.T) {
	e, _, errb, _ := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{
		"DELETE /api/peers/kube": {409, `{"code":"static_peer","message":"this peer is set in -peers / COMEBYE_PEERS; edit the hub's environment and restart to change it"}`},
	})
	if code := run(e, "peer", "remove", "kube"); code != 1 || !strings.Contains(errb.String(), "COMEBYE_PEERS") {
		t.Fatalf("static: %d %s", code, errb)
	}
	// No login known: a usage error that says what to pass.
	e, _, errb, _ = peerEnv(t, nil, nil)
	if code := run(e, "peer", "list"); code != 2 || !strings.Contains(errb.String(), "--login") {
		t.Fatalf("no login: %d %s", code, errb)
	}
	e, _, _, _ = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "x"}, nil)
	for _, args := range [][]string{{"peer"}, {"peer", "add", "only-name"}, {"peer", "remove"}, {"peer", "frob"}, {"peer", "list", "--tag", "x"}} {
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// Version visibility in `comebye agents`: match, mismatch and unknown.
func TestAgentsVersionWarnings(t *testing.T) {
	hosts := func(peer string) []apiHost {
		return []apiHost{{Name: "laharl", Local: true, Status: "ok", Version: "v1.2.0"}, {Name: "kube", Status: "ok", Version: peer}}
	}
	if w := versionWarnings(hosts("v1.2.0")); len(w) != 0 {
		t.Errorf("match warned: %v", w)
	}
	if w := versionWarnings(hosts("v1.1.0")); len(w) != 1 || !strings.Contains(w[0], "kube runs v1.1.0, the hub runs v1.2.0") {
		t.Errorf("mismatch: %v", w)
	}
	if w := versionWarnings(hosts("")); len(w) != 1 || !strings.Contains(w[0], "does not report its version") {
		t.Errorf("unknown: %v", w)
	}
	// A peer that is down says nothing about versions; a hub that does not
	// know its own version cannot compare.
	down := []apiHost{{Name: "laharl", Local: true, Status: "ok", Version: "v1.2.0"}, {Name: "kube", Status: "down", Version: "v0"}}
	if len(versionWarnings(down)) != 0 {
		t.Error("down peer warned")
	}
	if len(versionWarnings([]apiHost{{Name: "laharl", Local: true, Status: "ok"}, {Name: "kube", Status: "ok", Version: "v1"}})) != 0 {
		t.Error("compared against an unknown hub version")
	}
}

func TestAgentsPrintsVersionWarning(t *testing.T) {
	f, e, out, _ := newFake(t)
	f.answers["GET /api/agents"] = answer{body: `{"agents":[],"hosts":[{"name":"laharl","local":true,"status":"ok","version":"v1.2.0"},{"name":"kube","status":"ok","version":"v1.1.0","version_mismatch":true}]}`}
	if code := run(e, "agents"); code != 0 || !strings.Contains(out.String(), "warning: host kube runs v1.1.0, the hub runs v1.2.0") {
		t.Fatalf("agents: %d %s", code, out)
	}
}
