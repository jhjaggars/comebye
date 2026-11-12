package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// served runs the full handler (host check, CSRF, and the page headers),
// as a browser reaching the loopback port would.
func served(s *Server, method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "localhost:8741"
	r.RemoteAddr = "100.64.0.1:1234"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func pageOf(t *testing.T, body []byte) Page {
	t.Helper()
	var r struct {
		Page Page `json:"page"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r.Page
}

func pubJSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestPagesPublishUpdateListDelete(t *testing.T) {
	s, _, _ := todoServer(t)
	dir := s.cfg.StateDir
	w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "<title>Failover &amp; test</title><h1>hi</h1>", "handoff": "h1"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", w.Code, w.Body)
	}
	p := pageOf(t, w.Body.Bytes())
	if !pageIDRE.MatchString(p.ID) || p.Kind != "html" || p.Title != "Failover & test" || p.Version != 1 || p.Agent != "desk/w2:p1" || p.Host != "desk" || p.Handoff != "h1" {
		t.Fatalf("page = %+v", p)
	}
	if !strings.Contains(w.Body.String(), `"path":"/p/`+p.ID+`"`) {
		t.Fatalf("no path: %s", w.Body)
	}
	// The publisher is the caller, whatever the body says.
	w = do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "# notes\ntext", "by": "someone", "agent": "x/y", "host": "elsewhere"}))
	n := pageOf(t, w.Body.Bytes())
	if n.Kind != "md" || n.Title != "notes" || n.Agent != "desk/w2:p1" || n.Host != "desk" {
		t.Fatalf("md page = %+v", n)
	}
	// Update in place by id: the same URL, a new version.
	w = do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"id": p.ID, "body": "<h1>v2</h1>", "if_version": 1}))
	u := pageOf(t, w.Body.Bytes())
	if w.Code != http.StatusOK || u.ID != p.ID || u.Version != 2 || u.Title != "Failover & test" || len(u.History) != 2 {
		t.Fatalf("update = %d %+v", w.Code, u)
	}
	// A stale if_version loses the race.
	w = do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"id": p.ID, "body": "<h1>v3</h1>", "if_version": 1}))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "version_conflict") {
		t.Fatalf("stale = %d %s", w.Code, w.Body)
	}
	// A key makes publishing idempotent: the same key is the same page.
	a := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"key": "nightly", "body": "# one"})).Body.Bytes())
	b := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"key": "nightly", "body": "# two"})).Body.Bytes())
	if a.ID != b.ID || b.Version != 2 {
		t.Fatalf("key: %+v then %+v", a, b)
	}
	// The files are in the state directory, private.
	st, err := os.Stat(filepath.Join(dir, "pages", p.ID, "v2.html"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("v2.html: %v %v", st, err)
	}
	// List, mine, handoff filter.
	var l struct{ Pages []Page }
	json.Unmarshal(do(s, "GET", "/api/pages", agentHdr("w2:p1"), "").Body.Bytes(), &l)
	if len(l.Pages) != 3 {
		t.Fatalf("list = %+v", l.Pages)
	}
	json.Unmarshal(do(s, "GET", "/api/pages?handoff=h1", userHdr, "").Body.Bytes(), &l)
	if len(l.Pages) != 1 || l.Pages[0].ID != p.ID {
		t.Fatalf("handoff filter = %+v", l.Pages)
	}
	json.Unmarshal(do(s, "GET", "/api/pages?mine=1", userHdr, "").Body.Bytes(), &l)
	if len(l.Pages) != 0 {
		t.Fatalf("the user published none: %+v", l.Pages)
	}
	// Delete removes the files.
	if w := do(s, "DELETE", "/api/pages/"+p.ID, agentHdr("w2:p1"), ""); w.Code != 200 {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "pages", p.ID)); !os.IsNotExist(err) {
		t.Fatalf("files remain: %v", err)
	}
	if w := do(s, "GET", "/api/pages/"+p.ID, userHdr, ""); w.Code != 404 {
		t.Fatalf("deleted page = %d", w.Code)
	}
	// The index survives a restart.
	st2, err := openPageStore(filepath.Join(dir, "pages"))
	if err != nil || len(st2.list()) != 2 {
		t.Fatalf("reopen: %v %d", err, len(st2.list()))
	}
}

func TestPagesWhoMayChange(t *testing.T) {
	s, _, _ := todoServer(t)
	// A coordinator is w1:p1 (todoServer); w2:p1 and w3:p1 are workers.
	p := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"key": "k", "body": "<p>x"})).Body.Bytes())
	other := agentHdr("w3:p1")
	// Another agent can neither update (by id or by key) nor delete it.
	for _, body := range []map[string]any{{"id": p.ID, "body": "<p>y"}, {"key": "k", "body": "<p>y"}} {
		if w := do(s, "POST", "/api/pages", other, pubJSON(body)); w.Code != 403 || !strings.Contains(w.Body.String(), "page_forbidden") {
			t.Errorf("other updates %v: %d %s", body, w.Code, w.Body)
		}
	}
	if w := do(s, "DELETE", "/api/pages/"+p.ID, other, ""); w.Code != 403 {
		t.Errorf("other deletes: %d", w.Code)
	}
	// It may read the list but not the body of a page from another host, and
	// on the same host it may (as with output).
	if w := do(s, "GET", "/api/pages/"+p.ID+"?body=1", other, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"body":"\u003cp\u003ex"`) {
		t.Errorf("same-host body read: %d %s", w.Code, w.Body)
	}
	fwd := map[string]string{"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgent: "w3:p1", HeaderAgentHost: "elsewhere"}
	if w := do(s, "GET", "/api/pages/"+p.ID+"?body=1", fwd, ""); w.Code != 403 || !strings.Contains(w.Body.String(), "read_forbidden") {
		t.Errorf("other-host body read: %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/pages/"+p.ID, fwd, ""); w.Code != 200 {
		t.Errorf("other-host metadata: %d", w.Code)
	}
	// The publisher, the user and a coordinator may.
	if w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"id": p.ID, "body": "<p>2"})); w.Code != 200 {
		t.Errorf("publisher updates: %d", w.Code)
	}
	if w := do(s, "POST", "/api/pages", userHdr, pubJSON(map[string]any{"id": p.ID, "body": "<p>3"})); w.Code != 200 {
		t.Errorf("user updates: %d", w.Code)
	}
	if w := do(s, "POST", "/api/pages", agentHdr("w1:p1"), pubJSON(map[string]any{"id": p.ID, "body": "<p>4"})); w.Code != 200 {
		t.Errorf("coordinator updates: %d", w.Code)
	}
	if w := do(s, "DELETE", "/api/pages/"+p.ID, agentHdr("w1:p1"), ""); w.Code != 200 {
		t.Errorf("coordinator deletes: %d", w.Code)
	}
	// No credentials: nothing.
	if w := do(s, "POST", "/api/pages", nil, pubJSON(map[string]any{"body": "x"})); w.Code != 401 {
		t.Errorf("anonymous publish: %d", w.Code)
	}
	if w := do(s, "POST", "/api/pages", map[string]string{HeaderAgentToken: "wrong"}, pubJSON(map[string]any{"body": "x"})); w.Code != 401 {
		t.Errorf("wrong token: %d", w.Code)
	}
}

func TestPagesRejectBadInput(t *testing.T) {
	s, _, _ := todoServer(t)
	cases := []struct {
		name string
		body map[string]any
		code int
		want string
	}{
		{"empty", map[string]any{"body": ""}, 400, "bad_body"},
		{"both bodies", map[string]any{"body": "a", "body_b64": "YQ=="}, 400, "bad_body"},
		{"not base64", map[string]any{"body_b64": "!!"}, 400, "bad_body"},
		{"binary", map[string]any{"body_b64": base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00})}, 400, "bad_body"},
		{"kind", map[string]any{"body": "x", "kind": "pdf"}, 400, "bad_kind"},
		{"key", map[string]any{"body": "x", "key": "../x"}, 400, "bad_key"},
		{"id", map[string]any{"body": "x", "id": "../../etc"}, 400, "bad_id"},
		{"handoff", map[string]any{"body": "x", "handoff": "a b"}, 400, "bad_handoff"},
		{"no such id", map[string]any{"body": "x", "id": "pg_0123456789"}, 404, "not_found"},
		{"too big", map[string]any{"body": strings.Repeat("a", pageMaxBytes+1)}, 413, "page_too_large"},
	}
	for _, c := range cases {
		w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(c.body))
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}
	// Exactly the limit is fine; a long title is cut; base64 works.
	if w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": strings.Repeat("a", pageMaxBytes), "kind": "md"})); w.Code != 201 {
		t.Errorf("at the limit: %d %.100s", w.Code, w.Body)
	}
	w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"title": strings.Repeat("é", 300), "body_b64": base64.StdEncoding.EncodeToString([]byte("<p>ok"))}))
	if p := pageOf(t, w.Body.Bytes()); w.Code != 201 || utf8.RuneCountInString(p.Title) > pageMaxTitle || !strings.HasPrefix(p.Title, "é") {
		t.Errorf("title: %d %q", w.Code, p.Title)
	}
	// A request body past the cap is refused before it is read in full.
	w = do(s, "POST", "/api/pages", agentHdr("w2:p1"), `{"body":"`+strings.Repeat("a", pageMaxRequest)+`"}`)
	if w.Code != 413 {
		t.Errorf("huge request: %d", w.Code)
	}
}

func TestPagesRetentionQuotaRate(t *testing.T) {
	dir := t.TempDir()
	st := newPageStore(dir)
	user := pageWho{user: true, by: "me"}
	var id string
	for i := 1; i <= pageMaxVersions+3; i++ {
		st.now = func() time.Time { return time.Unix(1000+int64(i)*100, 0) } // a fresh minute each time
		p, _, err := st.publish(pageIn{ID: id, Kind: "md", Body: []byte("# v" + strings.Repeat("x", i))}, user)
		if err != nil {
			t.Fatal(err)
		}
		id = p.ID
	}
	p, _ := st.get(id)
	if p.Version != pageMaxVersions+3 || len(p.History) != pageMaxVersions || p.History[0].N != 4 {
		t.Fatalf("history = %+v", p.History)
	}
	for n := 1; n <= pageMaxVersions+3; n++ {
		_, err := os.Stat(filepath.Join(dir, id, "v"+strconv.Itoa(n)+".md"))
		if (n < 4) != os.IsNotExist(err) {
			t.Errorf("v%d on disk: %v", n, err)
		}
	}
	// The oldest kept version is still readable; a pruned one is gone.
	if _, _, b, err := st.body(id, 4); err != nil || !strings.HasPrefix(string(b), "# vxxxx") {
		t.Errorf("v4: %v %q", err, b)
	}
	if _, _, _, err := st.body(id, 2); err == nil {
		t.Errorf("v2 should be gone")
	}

	// The quota counts the kept versions, not the ones dropped.
	q := newPageStore(t.TempDir())
	q.quota = 100
	a, _, err := q.publish(pageIn{Kind: "md", Body: []byte(strings.Repeat("a", 60))}, user)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.publish(pageIn{Kind: "md", Body: []byte(strings.Repeat("b", 60))}, user); err != errPageQuota {
		t.Errorf("second page over the quota: %v", err)
	}
	if _, _, err := q.publish(pageIn{ID: a.ID, Kind: "md", Body: []byte(strings.Repeat("c", 40))}, user); err != nil {
		t.Errorf("a new version within the quota: %v", err)
	}
	if err := q.remove(a.ID, user); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.publish(pageIn{Kind: "md", Body: []byte(strings.Repeat("b", 60))}, user); err != nil {
		t.Errorf("after delete: %v", err)
	}

	// 20 a minute per publisher, and not for another one.
	r := newPageStore("")
	now := time.Unix(5000, 0)
	r.now = func() time.Time { return now }
	for i := 0; i < pageRate; i++ {
		if _, _, err := r.publish(pageIn{Kind: "md", Body: []byte("x")}, pageWho{agent: "desk/w2:p1", host: "desk"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.publish(pageIn{Kind: "md", Body: []byte("x")}, pageWho{agent: "desk/w2:p1", host: "desk"}); err != errPageRate {
		t.Errorf("21st: %v", err)
	}
	if _, _, err := r.publish(pageIn{Kind: "md", Body: []byte("x")}, pageWho{agent: "desk/w3:p1", host: "desk"}); err != nil {
		t.Errorf("another agent: %v", err)
	}
	now = now.Add(time.Minute)
	if _, _, err := r.publish(pageIn{Kind: "md", Body: []byte("x")}, pageWho{agent: "desk/w2:p1", host: "desk"}); err != nil {
		t.Errorf("a minute later: %v", err)
	}
}

// Every response under /p/ carries the sandbox policy: pages, errors, the
// auth failure, a wrong Host, a path that matches no route.
func TestPagesEveryResponseIsSandboxed(t *testing.T) {
	s, _, _ := todoServer(t)
	p := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "<script>1</script>"})).Body.Bytes())
	md := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "# T\n</script><b>x", "kind": "md"})).Body.Bytes())

	type req struct {
		name, method, path string
		hdr                map[string]string
		host               string
		want               int
	}
	reqs := []req{
		{"page", "GET", "/p/" + p.ID, userHdr, "", 200},
		{"version", "GET", "/p/" + p.ID + "/v1", userHdr, "", 200},
		{"version number", "GET", "/p/" + p.ID + "/1", userHdr, "", 200},
		{"markdown", "GET", "/p/" + md.ID, userHdr, "", 200},
		{"lib", "GET", "/p/_/marked.js", nil, "", 200},
		{"lib, mermaid", "GET", "/p/_/mermaid.js", nil, "", 200},
		{"lib, unknown", "GET", "/p/_/other.js", nil, "", 404},
		{"unknown id", "GET", "/p/pg_0000000000", userHdr, "", 404},
		{"bad id", "GET", "/p/..%2f..%2fetc%2fpasswd", userHdr, "", 404},
		{"bad id 2", "GET", "/p/x", userHdr, "", 404},
		{"no such version", "GET", "/p/" + p.ID + "/v9", userHdr, "", 404},
		{"bad version", "GET", "/p/" + p.ID + "/..", userHdr, "", 307},
		{"deep path", "GET", "/p/" + p.ID + "/v1/more/stuff", userHdr, "", 404},
		{"directory", "GET", "/p/", userHdr, "", 404},
		{"anonymous", "GET", "/p/" + p.ID, nil, "", 401},
		{"an agent", "GET", "/p/" + p.ID, agentHdr("w2:p1"), "", 403},
		{"wrong host", "GET", "/p/" + p.ID, userHdr, "evil.example.com", 421},
		{"POST", "POST", "/p/" + p.ID, userHdr, "", 415},
	}
	for _, c := range reqs {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Host = "localhost:8741"
		if c.host != "" {
			r.Host = c.host
		}
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		h := w.Header()
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d (%.80s)", c.name, w.Code, c.want, w.Body)
		}
		csp := h.Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox;") || strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "allow-top-navigation") || strings.Contains(csp, "allow-forms") || strings.Contains(csp, "allow-modals") {
			t.Errorf("%s: CSP %q", c.name, csp)
		}
		if !strings.Contains(csp, "connect-src 'none'") || !strings.Contains(csp, "form-action 'none'") || !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP lacks the fetch/form lockdown: %q", c.name, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", c.name, h)
		}
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: CORS header %s on a page response", c.name, k)
			}
		}
		if c.want == 200 && !strings.HasPrefix(c.path, "/p/_/") {
			if h.Get("Cache-Control") != "no-store" || h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cross-Origin-Resource-Policy") != "same-origin" {
				t.Errorf("%s: headers %v", c.name, h)
			}
		}
	}
	// The HTML is served as the agent wrote it; the Markdown through the
	// shell, whose source block cannot be closed by the page text.
	if w := served(s, "GET", "/p/"+p.ID, userHdr, ""); w.Body.String() != "<script>1</script>" {
		t.Errorf("html body = %q", w.Body)
	}
	w := served(s, "GET", "/p/"+md.ID, userHdr, "")
	if !strings.Contains(w.Body.String(), `\u003c/script\u003e\u003cb\u003ex`) || strings.Count(w.Body.String(), "</script>") != 4 {
		t.Errorf("markdown shell = %s", w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'unsafe-inline' 'self'") || strings.Contains(csp, "cdnjs") {
		t.Errorf("markdown CSP = %q", csp)
	}
	// The page CSP for HTML allows no 'self': a page cannot load the
	// hub's own scripts.
	csp := served(s, "GET", "/p/"+p.ID, userHdr, "").Header().Get("Content-Security-Policy")
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src") && strings.Contains(d, "'self'") {
			t.Errorf("html CSP lets a page load the hub's scripts: %q", csp)
		}
	}
	if !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("html CSP lets any site frame a page: %q", csp)
	}
	// An old version stays addressable after an update.
	do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"id": p.ID, "body": "<p>new"}))
	if w := served(s, "GET", "/p/"+p.ID+"/v1", userHdr, ""); w.Body.String() != "<script>1</script>" {
		t.Errorf("v1 = %q", w.Body)
	}
	if w := served(s, "GET", "/p/"+p.ID, userHdr, ""); w.Body.String() != "<p>new" {
		t.Errorf("latest = %q", w.Body)
	}
	// And a page with CORS asked for gets none.
	r := httptest.NewRequest("GET", "/p/"+p.ID, nil)
	r.Host = "localhost"
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Tailscale-User-Login", "me@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS granted: %v", rec.Header())
	}
}

// No pages on an instance without the ledger (a peer): agents are told to
// use the hub, and the page URLs 404 under the same policy.
func TestPagesOffWithoutTheHub(t *testing.T) {
	s := newTest("me@example.com")
	if w := do(s, "POST", "/api/pages", userHdr, pubJSON(map[string]any{"body": "x"})); w.Code != 503 {
		t.Errorf("publish: %d %s", w.Code, w.Body)
	}
	w := served(s, "GET", "/p/pg_0123456789", userHdr, "")
	if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("view: %d %v", w.Code, w.Header())
	}
}

func TestPagesTaggedNodeSeesOnlyItsOwnHost(t *testing.T) {
	r := newRigWith(t, false, func(c *Config) { c.AllowCap = testCap; c.StateDir = t.TempDir() })
	r.addPeer("w1", "https://w1.t.ts.net", "tag:worker")
	r.who.setHolder("w1.t.ts.net", "tag:worker")
	user := map[string]string{"Tailscale-User-Login": "me@example.com"}

	code, body := r.serve("POST", "/api/pages", `{"body":"<p>from w1","key":"k"}`, map[string]string{HeaderAgent: "w:1"})
	if code != http.StatusCreated {
		t.Fatalf("tagged publish: %d %s", code, body)
	}
	mine := pageOf(t, []byte(body))
	if mine.Host != "w1" || !mine.Tagged || mine.Agent != "w1/w:1" {
		t.Fatalf("attribution = %+v", mine)
	}
	// The user publishes one too; the tagged node neither sees nor changes it.
	code, body = r.call(r.main, "POST", "/api/pages", `{"body":"# user"}`, user)
	if code != http.StatusCreated {
		t.Fatalf("user publish: %d %s", code, body)
	}
	theirs := pageOf(t, []byte(body))
	var l struct{ Pages []Page }
	_, body = r.serve("GET", "/api/pages", "", nil)
	json.Unmarshal([]byte(body), &l)
	if len(l.Pages) != 1 || l.Pages[0].ID != mine.ID {
		t.Errorf("tagged list = %+v", l.Pages)
	}
	if code, _ := r.serve("GET", "/api/pages/"+theirs.ID, "", nil); code != 404 {
		t.Errorf("tagged get of the user's page: %d", code)
	}
	if code, _ := r.serve("DELETE", "/api/pages/"+theirs.ID, "", nil); code != 404 {
		t.Errorf("tagged delete of the user's page: %d", code)
	}
	if code, _ := r.serve("POST", "/api/pages", `{"id":"`+theirs.ID+`","body":"x"}`, nil); code != 403 {
		t.Errorf("tagged update of the user's page: %d", code)
	}
	// Its own page it may update and delete; it cannot read pages in the
	// browser's way.
	if code, body := r.serve("POST", "/api/pages", `{"key":"k","body":"<p>again"}`, map[string]string{HeaderAgent: "w:2"}); code != 200 {
		t.Errorf("tagged update of its own: %d %s", code, body)
	}
	if code, _ := r.serve("GET", "/p/"+mine.ID, "", nil); code != 404 {
		t.Errorf("tagged view: %d", code)
	}
	if code, _ := r.serve("DELETE", "/api/pages/"+mine.ID, "", nil); code != 200 {
		t.Errorf("tagged delete of its own: %d", code)
	}
}

// The pages listener has /p and nothing else: no /api, no app, whatever the
// method, and the page policy on every answer.
func TestPagesListenerServesOnlyPages(t *testing.T) {
	s, _, _ := todoServer(t)
	s.cfg.PagesURL = "https://hub.t.ts.net:8443"
	s.cfg.PublicURL = "https://hub.t.ts.net"
	p := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "<p>hi"})).Body.Bytes())
	h := s.PagesHandler()
	get := func(method, path string, hdr map[string]string, host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(""))
		r.Host = host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("GET", "/p/"+p.ID, userHdr, "hub.t.ts.net:8443"); w.Code != 200 || w.Body.String() != "<p>hi" {
		t.Fatalf("page = %d %q", w.Code, w.Body)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/agents"}, {"GET", "/api/pages"}, {"GET", "/api/whoami"}, {"POST", "/api/pages"}, {"POST", "/api/todos"},
		{"DELETE", "/api/pages/" + p.ID}, {"GET", "/api/events"}, {"GET", "/"}, {"GET", "/app.js"}, {"GET", "/vendor/marked.umd.js"}, {"GET", "/index.html"},
	} {
		w := get(c.method, c.path, userHdr, "localhost")
		if w.Code != 404 || strings.Contains(w.Body.String(), "agents") || !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox;") {
			t.Errorf("%s %s = %d %.60q csp=%q", c.method, c.path, w.Code, w.Body, w.Header().Get("Content-Security-Policy"))
		}
	}
	// Only the app may frame a page (its own origin is not this one).
	csp := get("GET", "/p/"+p.ID, userHdr, "localhost").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://hub.t.ts.net") || strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("csp = %q", csp)
	}
	if w := get("GET", "/p/"+p.ID, nil, "localhost"); w.Code != 401 {
		t.Errorf("anonymous = %d", w.Code)
	}
	if w := get("GET", "/p/"+p.ID, agentHdr("w2:p1"), "localhost"); w.Code != 403 {
		t.Errorf("agent = %d", w.Code)
	}
	if w := get("GET", "/p/"+p.ID, userHdr, "evil.example.com"); w.Code != 421 || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("wrong host = %d", w.Code)
	}
	// The app may frame the pages origin, and only that one.
	if c := appCSP(s.pagesOrigin()); !strings.Contains(c, "frame-src 'self' https://hub.t.ts.net:8443;") {
		t.Errorf("app csp = %q", c)
	}
	if c := appCSP(""); !strings.Contains(c, "frame-src 'self';") {
		t.Errorf("app csp = %q", c)
	}
	// The API gives the pages origin, and links use it.
	var l struct {
		PagesURL string `json:"pages_url"`
	}
	json.Unmarshal(do(s, "GET", "/api/pages", userHdr, "").Body.Bytes(), &l)
	if l.PagesURL != "https://hub.t.ts.net:8443" {
		t.Errorf("pages_url = %q", l.PagesURL)
	}
	if w := do(s, "GET", "/api/pages/"+p.ID, userHdr, ""); !strings.Contains(w.Body.String(), `"url":"https://hub.t.ts.net:8443/p/`+p.ID+`"`) {
		t.Errorf("link = %s", w.Body)
	}
	// Refusals to listen.
	s.cfg.Addr = "127.0.0.1:8741"
	if err := s.ListenPages("0.0.0.0:8742"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("non-loopback: %v", err)
	}
	if err := s.ListenPages("127.0.0.1:8741"); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("same address: %v", err)
	}
}

func TestPagesBodySearch(t *testing.T) {
	s, _, _ := todoServer(t)
	pub := func(m map[string]any) Page {
		w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(m))
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("publish = %d %s", w.Code, w.Body)
		}
		return pageOf(t, w.Body.Bytes())
	}
	drill := pub(map[string]any{"title": "Failover drill", "body": "<title>Failover drill</title><style>.lease{color:red}</style><script>var lease30s=1</script><p>Keep the 30 s lease so a stale leader can't write.</p><p>The lease is renewed.</p>"})
	plan := pub(map[string]any{"body": "# Plan\n\nA lease of 30 s on the claim. <b>not markup</b>"})
	pub(map[string]any{"body": "# Other\n\nnothing relevant"})
	// A page's old version is not searched: its newest is.
	w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"id": plan.ID, "body": "# Plan\n\nthe claim has no lease any more, only a zebra", "if_version": 1}))
	if w.Code != http.StatusOK {
		t.Fatalf("update %d %s", w.Code, w.Body)
	}
	type resp struct {
		Pages   []Page      `json:"pages"`
		Matches []PageMatch `json:"matches"`
		Partial bool        `json:"partial"`
	}
	search := func(q string, hdr map[string]string) (int, resp) {
		w := do(s, "GET", "/api/pages?q="+q, hdr, "")
		var r resp
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r
	}
	code, r := search("lease+30", userHdr)
	if code != 200 || len(r.Matches) != 1 || r.Matches[0].ID != drill.ID || r.Matches[0].Version != 1 || len(r.Pages) != 1 || r.Pages[0].ID != drill.ID {
		t.Fatalf("lease 30: %d %+v", code, r)
	}
	if sn := r.Matches[0].Snippet; !strings.Contains(sn, "Keep the \x0230\x03 s \x02lease\x03 so") || strings.ContainsAny(sn, "<>") || strings.Contains(sn, "lease30s") {
		t.Errorf("snippet %q", sn)
	}
	if r.Matches[0].Count != 2 {
		t.Errorf("count %d", r.Matches[0].Count)
	}
	// "lease" alone: the drill (script and style text does not count) and the updated plan.
	code, r = search("LEASE", userHdr)
	if code != 200 || len(r.Matches) != 2 || r.Matches[0].Count != 2 && r.Matches[1].Count != 2 {
		t.Fatalf("lease: %d %+v", code, r)
	}
	code, r = search("color", userHdr)
	if code != 200 || len(r.Matches) != 0 || r.Pages == nil || r.Matches == nil {
		t.Fatalf("style text matched or null lists: %d %+v", code, r)
	}
	// An agent may not read bodies through search; bad input; the rate limit.
	if code, _ := search("lease", agentHdr("w2:p1")); code != http.StatusForbidden {
		t.Errorf("agent search = %d", code)
	}
	if code, _ := search("", userHdr); code != http.StatusBadRequest {
		t.Errorf("empty q = %d", code)
	}
	if code, _ := search(strings.Repeat("a", 300), userHdr); code != http.StatusBadRequest {
		t.Errorf("long q = %d", code)
	}
	var last int
	for i := 0; i < pageSearchRate+2; i++ {
		last, _ = search("zebra", userHdr)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("rate = %d", last)
	}
	// With no q the listing is as before and has no matches.
	w = do(s, "GET", "/api/pages", userHdr, "")
	if strings.Contains(w.Body.String(), `"matches"`) {
		t.Errorf("matches without q: %s", w.Body)
	}
}

func TestMatchPageTextSnippetKeepsRunes(t *testing.T) {
	text := strings.Repeat("é", 100) + " needle " + strings.Repeat("ü", 300)
	n, sn, ok := matchPageText(text, []string{"needle"})
	if !ok || n != 1 || !utf8.ValidString(sn) || !strings.Contains(sn, "\x02needle\x03") || !strings.HasPrefix(sn, "…") || !strings.HasSuffix(sn, "…") {
		t.Fatalf("%v %d %q", ok, n, sn)
	}
	if _, _, ok := matchPageText("abc", []string{"abc", "zzz"}); ok {
		t.Error("every word must appear")
	}
}
