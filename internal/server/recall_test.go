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
	return []string{"/api/recall/sessions?agent=x/w1:p1", "/api/recall/search?q=zebra&mode=work", "/api/recall/status", "/api/recall/facets", "/api/recall/search?q=zebra", "/api/recall/recent", s, s + "/hits?q=zebra", s + "/produced", "/api/recall/produced?ref=pg_0123456789"}
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
	code, _, body = recallGet(t, ts, "/api/recall/recent?limit=9999&host=desk&since=2026-01-01")
	if code != 200 || !strings.Contains(body, `"total":1`) || !strings.Contains(body, recallSID) {
		t.Errorf("%d %s", code, body)
	}
	if code, _, body := recallGet(t, ts, "/api/recall/recent?host=elsewhere"); code != 200 || !strings.Contains(body, `"total":0`) || !strings.Contains(body, `"hits":[]`) {
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
		"/api/recall/search?q=a&mode=threads",
		"/api/recall/search?q=a&role=everything",
		"/api/recall/search?q=a&sort=random",
		"/api/recall/search?q=a&limit=-1",
		"/api/recall/search?q=a&skip=x",
		"/api/recall/search?q=a&since=" + url.QueryEscape("'; DROP TABLE sessions; --"),
		"/api/recall/search?q=a&host=" + strings.Repeat("h", recallMaxValue+1),
		"/api/recall/recent?limit=-1",
		"/api/recall/recent?skip=x",
		"/api/recall/recent?since=" + url.QueryEscape("'; DROP TABLE sessions; --"),
		"/api/recall/recent?host=" + strings.Repeat("h", recallMaxValue+1),
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

// withProduced adds rows to the index as the extractor would have left them.
func withProduced(t *testing.T, path string) {
	t.Helper()
	sql := `INSERT INTO produced(host,kind,session_id,seq,sub,tool_id,ts,type,ref,url,repo,branch,title,rel,patch_id,pr,detail) VALUES
('desk','claude','` + recallSID + `',0,'','t1','2026-10-03T08:01:00Z','page','pg_0123456789','','','','Drill','','','','version 2'),
('desk','claude','` + recallSID + `',0,'','t2','2026-10-03T08:02:00Z','commit','9c1e4ab0000000000000000000000000000000aa','','','ops/x','Add notes','made','','Acme/widget#212',''),
('desk','claude','` + recallSID + `',0,'','t3','2026-10-03T08:03:00Z','issue','Acme/widget#5491','https://github.com/Acme/widget/issues/5491','Acme/widget','','Bug','','','',''),
('desk','claude','` + recallSID + `',0,'','t4','2026-10-03T08:04:00Z','commit','9c1e4ab0000000000000000000000000000000aa','','','','seen','seen','','','');
INSERT INTO outcomes VALUES('t-x','commit-landed','9c1e4ab0000000000000000000000000000000aa','9c1e4ab0 Add notes (on main as 4d02f170)','patch-id','');`
	cmd := exec.Command("sqlite3", path)
	cmd.Stdin = strings.NewReader(sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func TestRecallProduced(t *testing.T) {
	r := newReadRig(t)
	path := recallIndex(t)
	withProduced(t, path)
	r.s.recall = archive.NewReader(path)
	hdr := map[string]string{"Tailscale-User-Login": "me@example.com"}
	get := func(p string, into any) int {
		code, body := r.call(r.main, "GET", p, "", hdr)
		if code == http.StatusOK && into != nil {
			if err := json.Unmarshal([]byte(body), into); err != nil {
				t.Fatalf("%s: %v: %s", p, err, body)
			}
		}
		return code
	}
	type resp struct {
		Produced []archive.ProducedItem `json:"produced"`
	}
	var got resp
	if c := get("/api/recall/sessions/desk/claude/"+recallSID+"/produced", &got); c != 200 || len(got.Produced) != 4 {
		t.Fatalf("session produced: %d %+v", c, got)
	}
	if got.Produced[0].Type != "page" || got.Produced[0].Session.ID != recallSID {
		t.Errorf("first: %+v", got.Produced[0])
	}
	if c := get("/api/recall/sessions/desk/claude/nope/produced", nil); c != http.StatusNotFound {
		t.Errorf("unknown session: %d", c)
	}
	// By page id, SHA prefix, issue ref, PR ref (finds the commit that shipped in it), URL.
	for ref, want := range map[string]string{
		"pg_0123456789": "page", "9c1e4ab": "commit", "Acme/widget%235491": "issue", "Acme/widget%23212": "commit",
		"https://github.com/Acme/widget/pull/212": "commit", "https://github.com/Acme/widget/issues/5491": "issue",
	} {
		got = resp{}
		if c := get("/api/recall/produced?ref="+ref, &got); c != 200 || len(got.Produced) == 0 || got.Produced[0].Type != want {
			t.Errorf("ref %s: %d %+v", ref, c, got)
		}
	}
	got = resp{}
	get("/api/recall/produced?ref=9c1e4ab", &got)
	if len(got.Produced) != 2 || got.Produced[0].Rel != "made" || got.Produced[0].Verdict != "landed" || got.Produced[0].How != "patch-id" || got.Produced[0].LandedAs != "4d02f170" || got.Produced[0].PR != "Acme/widget#212" {
		t.Errorf("commit: %+v", got.Produced)
	}
	if c := get("/api/recall/produced?ref=zebra", nil); c != http.StatusBadRequest {
		t.Errorf("not a ref: %d", c)
	}
	// Search returns the artifact above the sessions, and the session row's counts.
	var sr archive.SearchResult
	get("/api/recall/search?q=pg_0123456789", &sr)
	if len(sr.Produced) != 1 || sr.Produced[0].Ref != "pg_0123456789" {
		t.Errorf("search by page id: %+v", sr.Produced)
	}
	sr = archive.SearchResult{}
	get("/api/recall/search?q=zebra", &sr)
	if len(sr.Hits) != 1 || sr.Hits[0].Produced["page"] != 1 || sr.Hits[0].Produced["commit"] != 1 || sr.Hits[0].Produced["issue"] != 1 || len(sr.Produced) != 0 {
		t.Errorf("counts: %+v", sr)
	}
}

// Work mode, the thread page and the link lookup answer, and refuse bad input.
func TestRecallWorkAndLinks(t *testing.T) {
	ts := recallServer(t, recallIndex(t))
	code, _, body := recallGet(t, ts, "/api/recall/search?q=zebra&mode=work")
	if code != 200 || !strings.Contains(body, `"threads"`) {
		t.Errorf("%d %s", code, body)
	}
	for _, p := range []string{"/api/recall/search?q=zebra&mode=nope", "/api/recall/threads/zzz", "/api/recall/threads/t-%25%25%25%25%25%25%25%25", "/api/recall/sessions", "/api/recall/sessions?ledger=a&agent=b"} {
		if code, _, body := recallGet(t, ts, p); code != 400 {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	if code, _, body := recallGet(t, ts, "/api/recall/threads/t-00000000"); code != 404 || !strings.Contains(body, "no_thread") {
		t.Errorf("%d %s", code, body)
	}
	if code, _, body := recallGet(t, ts, "/api/recall/sessions?ledger=h-none"); code != 200 || !strings.Contains(body, `"sessions":[]`) {
		t.Errorf("%d %s", code, body)
	}
}
