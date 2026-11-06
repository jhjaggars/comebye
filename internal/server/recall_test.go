package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/archive"
)

const recallSID = "44444444-4444-4444-8444-444444444444"

// recallIndex builds a real index with one session holding a marker.
func recallIndex(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
	d := t.TempDir()
	sess := filepath.Join(d, "claude", "-p", recallSID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": recallSID, "cwd": "/w/p", "timestamp": "2026-10-03T08:00:00Z", "message": map[string]any{"role": "user", "content": peerText + " zebra"}})
	if err := os.WriteFile(sess, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := archive.Config{Encryption: archive.ModeNone, Dir: filepath.Join(d, "store"), ClaudeDir: filepath.Join(d, "claude"), PiDir: filepath.Join(d, "pi"), Host: "desk"}
	a, err := cfg.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunFinal(cfg.Sources()); err != nil {
		t.Fatal(err)
	}
	o, err := cfg.OpenIdentity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d, "index.db")
	if _, err := (&archive.Index{Path: path}).Sync(a, o, nil, archive.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	return path
}

func recallPaths() []string {
	s := "/api/recall/sessions/desk/claude/" + recallSID
	return []string{"/api/recall/status", "/api/recall/facets", "/api/recall/search?q=zebra", s, s + "/hits?q=zebra"}
}

// The API is the user's alone. An agent with the local token, a request a peer
// forwarded, and a tagged worker through Serve all get 403 and no text. The
// routes are the same on a peer: nothing is forwarded to a hub.
func TestRecallIsUserOnly(t *testing.T) {
	r := newReadRig(t)
	r.s.recall = archive.NewReader(recallIndex(t))
	for _, p := range recallPaths() {
		code, body := r.call(r.main, "GET", p, "", map[string]string{"Tailscale-User-Login": "me@example.com"})
		if code != http.StatusOK {
			t.Errorf("user %s: %d %s", p, code, body)
		}
		for name, hdr := range map[string]map[string]string{
			"agent token":  {HeaderAgentToken: testToken, HeaderAgent: "wH:p1"},
			"forwarded":    forwarded("kube", "wC:p1"),
			"coordinator":  {HeaderAgentToken: testToken, HeaderAgent: "wH:p1", HeaderAgentHost: "desk"},
			"forwarded w1": forwarded("w1", "wS:p1"),
		} {
			code, body := r.call(r.main, "GET", p, "", hdr)
			if code != http.StatusForbidden || strings.Contains(body, peerText) {
				t.Errorf("%s %s: %d %s", name, p, code, body)
			}
		}
		code, body = r.asTagged("GET", p, "")
		if code != http.StatusForbidden || strings.Contains(body, peerText) {
			t.Errorf("tagged %s: %d %s", p, code, body)
		}
		code, _ = r.call(r.main, "GET", p, "", nil)
		if code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("nobody %s: %d", p, code)
		}
	}
	if n := r.w1.count("/recall") + r.kube.count("/recall"); n != 0 {
		t.Errorf("%d recall requests reached a peer", n)
	}
}

func recallServer(t *testing.T, index string) *httptest.Server {
	t.Helper()
	s := New(Config{InsecureNoAuth: true, RecallIndex: index, StateDir: t.TempDir(), ReadLines: 10}, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func recallGet(t *testing.T, ts *httptest.Server, path string) (int, http.Header, string) {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(res.Body)
	return res.StatusCode, res.Header, b.String()
}

func TestRecallAnswers(t *testing.T) {
	ts := recallServer(t, recallIndex(t))
	for _, p := range recallPaths() {
		code, h, body := recallGet(t, ts, p)
		if code != 200 {
			t.Errorf("%s: %d %s", p, code, body)
		}
		if h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", p, h.Get("Cache-Control"))
		}
	}
	code, _, body := recallGet(t, ts, "/api/recall/search?q=zebra&limit=9999&role=said&sort=new&since=2026-01-01&host=desk")
	if code != 200 || !strings.Contains(body, `"total":1`) || !strings.Contains(body, `\u0002zebra\u0003`) {
		t.Errorf("%d %s", code, body)
	}
	if code, _, body := recallGet(t, ts, "/api/recall/sessions/desk/claude/nope"); code != 404 || !strings.Contains(body, "no_session") {
		t.Errorf("%d %s", code, body)
	}
	code, _, body = recallGet(t, ts, "/api/recall/sessions/desk/claude/"+recallSID+"?seq=-1&dir=after&q=zebra&limit=9999")
	if code != 200 || !strings.Contains(body, `\u0002zebra\u0003`) {
		t.Errorf("%d %s", code, body)
	}
}

func TestRecallRefusesBadInput(t *testing.T) {
	ts := recallServer(t, recallIndex(t))
	long := strings.Repeat("x", recallMaxQ+1)
	s := "/api/recall/sessions/desk/claude/" + recallSID
	for _, p := range []string{
		"/api/recall/search",
		"/api/recall/search?q=" + long,
		"/api/recall/search?q=a&mode=work",
		"/api/recall/search?q=a&role=everything",
		"/api/recall/search?q=a&sort=random",
		"/api/recall/search?q=a&limit=-1",
		"/api/recall/search?q=a&skip=x",
		"/api/recall/search?q=a&since=" + url.QueryEscape("'; DROP TABLE sessions; --"),
		"/api/recall/search?q=a&host=" + strings.Repeat("h", recallMaxValue+1),
		"/api/recall/sessions/" + url.PathEscape("de sk") + "/claude/" + recallSID,
		"/api/recall/sessions/desk/" + url.PathEscape("cl'aude") + "/" + recallSID,
		"/api/recall/sessions/desk/claude/" + url.PathEscape("x' OR '1'='1"),
		s + "?seq=x",
		s + "?seq=-2",
		s + "?dir=sideways",
		s + "?sub=" + url.QueryEscape("../x"),
		s + "?q=" + long,
		s + "/hits",
		s + "/hits?q=" + long,
	} {
		if code, _, body := recallGet(t, ts, p); code != 400 {
			t.Errorf("%.80s: %d %s", p, code, body)
		}
	}
}

func TestRecallWithoutAnIndex(t *testing.T) {
	ts := recallServer(t, filepath.Join(t.TempDir(), "none.db"))
	if code, _, body := recallGet(t, ts, "/api/recall/status"); code != 200 || !strings.Contains(body, `"missing"`) {
		t.Errorf("%d %s", code, body)
	}
	if code, _, body := recallGet(t, ts, "/api/recall/search?q=a"); code != 503 || !strings.Contains(body, "no_index") {
		t.Errorf("%d %s", code, body)
	}
	if code, _, _ := recallGet(t, recallServer(t, ""), "/api/recall/status"); code != 200 {
		t.Errorf("no index configured: %d", code)
	}
}

// What was searched for stays out of the log, even when the query fails.
func TestRecallDoesNotLogQueries(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	path := recallIndex(t)
	ts := recallServer(t, path)
	for _, q := range []string{"sekritquery", `"sekritquery`} {
		recallGet(t, ts, "/api/recall/search?q="+url.QueryEscape(q))
	}
	if err := os.WriteFile(path, []byte("not a database, sekritquery"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, body := recallGet(t, ts, "/api/recall/search?q=sekritquery")
	if code != 500 || strings.Contains(body, "sekritquery") {
		t.Errorf("%d %s", code, body)
	}
	if strings.Contains(buf.String(), "sekritquery") {
		t.Errorf("logged: %s", buf.String())
	}
}
