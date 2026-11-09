package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/release"
)

// ---- the peer's side ----

const ghRun = "https://github.com/jhjaggars/comebye/actions/runs/"

// peerTagServer is a peer that owns the release clone: no ledger, a fake
// release tool, the app's upgrade off.
func peerTagServer(t *testing.T) (*Server, *fakeTool) {
	t.Helper()
	ck := okCheck()
	ck.CIURL = ghRun + "100"
	tool := &fakeTool{check: ck, build: release.Build{State: "success", URL: ghRun + "1"}, runURL: ghRun + "1"}
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "kube", StateDir: t.TempDir(), NoLedger: true, AgentToken: testToken,
		Release: ReleaseConfig{Tool: tool}}, nil, nil)
	return s, tool
}

var peerRelCalls = []struct{ name, method, path, body string }{
	{"check", "POST", "/api/release/check", `{"version":"v0.5.9","commit":"` + relSHA + `"}`},
	{"tag", "POST", "/api/release/tag", `{"version":"v0.5.9","commit":"` + relSHA + `"}`},
	{"build", "GET", "/api/release/build?version=v0.5.9", ""},
}

func TestPeerReleaseOpsAnswerOnlyAHub(t *testing.T) {
	s, tool := peerTagServer(t)
	hub, hubTool, _ := relServer(t) // a host that keeps the ledger
	callers := []struct {
		who  string
		srv  *Server
		hdr  map[string]string
		want int
		code string
	}{
		{"the login alone (a browser, a script)", s, userHdr, 403, "hub_only"},
		{"the marker alone", s, map[string]string{fleet.HeaderHubCall: "1"}, 401, ""},
		{"agent token with the marker", s, map[string]string{HeaderAgentToken: testToken, HeaderAgent: "w1:p1", fleet.HeaderHubCall: "1"}, 403, "agent_forbidden"},
		{"agent token, no pane, with the marker", s, map[string]string{HeaderAgentToken: testToken, fleet.HeaderHubCall: "1"}, 403, "agent_forbidden"},
		{"a peer forwarding for its agent", s, map[string]string{"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgent: "w1:p1", HeaderAgentHost: "other", fleet.HeaderHubCall: "1"}, 403, "agent_forbidden"},
		{"a wrong token with the login and marker", s, map[string]string{HeaderAgentToken: "nope", "Tailscale-User-Login": "me@example.com", fleet.HeaderHubCall: "1"}, 401, ""},
		{"a login outside the allowlist", s, map[string]string{"Tailscale-User-Login": "eve@example.com", fleet.HeaderHubCall: "1"}, 401, ""},
		{"a host that keeps the ledger, with the hub's marker", hub, hubCall, 403, "hub_only"},
	}
	for _, c := range callers {
		for _, call := range peerRelCalls {
			w := do(c.srv, call.method, call.path, c.hdr, call.body)
			if w.Code != c.want || (c.code != "" && !strings.Contains(w.Body.String(), c.code)) {
				t.Errorf("%s: %s = %d %s, want %d %s", c.who, call.name, w.Code, w.Body, c.want, c.code)
			}
		}
	}
	if len(tool.tagged()) != 0 || tool.checks != 0 || len(hubTool.tagged()) != 0 {
		t.Fatalf("a refused caller got something done: peer %v checks %d, hub %v", tool.tagged(), tool.checks, hubTool.tagged())
	}
}

func TestPeerReleaseOpsRefuseTaggedNodes(t *testing.T) {
	tool := &fakeTool{check: okCheck()}
	r := newCapRig2(t, tool, &fakeRunner{})
	r.who.setHolder(r.node, "tag:worker")
	r.addPeer("vm1", r.workerURL(), "tag:worker")
	for _, c := range peerRelCalls {
		code, body := r.serve(c.method, c.path, c.body, map[string]string{fleet.HeaderHubCall: "1"})
		if code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("tagged node %s = %d %s", c.name, code, body)
		}
	}
	if len(tool.tagged()) != 0 || tool.checks != 0 {
		t.Fatal("a tagged node got something done")
	}
}

func TestPeerReleaseOpsAreOnlyEverRegisteredUserOnly(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, ` /api/release/check"`) || strings.Contains(line, ` /api/release/tag"`) || strings.Contains(line, ` /api/release/build"`) || strings.Contains(line, ` /api/release/deploy"`) {
			n++
			if !strings.Contains(line, "s.auth(") || strings.Contains(line, "authAgent") {
				t.Errorf("not user-only: %s", line)
			}
		}
	}
	if n != 5 {
		t.Fatalf("found %d peer release routes", n)
	}
	// ...and the hub's browser forward cannot reach them.
	for _, op := range []string{"release-check", "release-tag", "release-build", "release-deploy", "release-deploy-status"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
		fleet.New(nil, "desk", nil, fleet.Options{}).ProxyHost(w, r, "kube", op)
		if w.Code != 404 {
			t.Errorf("ProxyHost %s = %d", op, w.Code)
		}
	}
}

func TestPeerReleaseOpsRunTheToolForAHubCall(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s, tool := peerTagServer(t)

	w := do(s, "POST", "/api/release/check", hubCall, peerRelCalls[0].body)
	var ck peerCheckResp
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ck) != nil || !ck.Check.OK || ck.Check.Commit != relSHA {
		t.Fatalf("check = %d %s", w.Code, w.Body)
	}
	w = do(s, "GET", "/api/release/build?version=v0.5.9", hubCall, "")
	var bd release.Build
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &bd) != nil || bd.State != "success" {
		t.Fatalf("build = %d %s", w.Code, w.Body)
	}
	w = do(s, "POST", "/api/release/tag", hubCall, `{"version":"v0.5.9","commit":"`+relSHA+`","todo":"t1","by":"me@example.com"}`)
	var tg peerTagResp
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tg) != nil || !tg.Result.Pushed || tg.DryRun {
		t.Fatalf("tag = %d %s", w.Code, w.Body)
	}
	if got := tool.tagged(); len(got) != 1 || got[0] != "v0.5.9 "+relSHA+" real" {
		t.Fatalf("tagged = %v", got)
	}
	// The peer's own audit line, in its journal (it keeps no ledger).
	if l := logs.String(); !strings.Contains(l, "audit: release.tagged by me@example.com on kube") || !strings.Contains(l, "v0.5.9") {
		t.Fatalf("no journal line for the tagging:\n%s", l)
	}

	// The peer's own dry-run flag wins; the hub can also ask for a dry run.
	s.cfg.Release.DryRun = true
	w = do(s, "POST", "/api/release/tag", hubCall, `{"version":"v0.5.10","commit":"`+relSHA+`"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"dry_run":true`) || tool.tagged()[1] != "v0.5.10 "+relSHA+" dry" {
		t.Fatalf("peer dry run = %d %s %v", w.Code, w.Body, tool.tagged())
	}
	s.cfg.Release.DryRun = false
	w = do(s, "POST", "/api/release/tag", hubCall, `{"version":"v0.5.11","commit":"`+relSHA+`","dry_run":true}`)
	if w.Code != 200 || tool.tagged()[2] != "v0.5.11 "+relSHA+" dry" {
		t.Fatalf("asked dry run = %d %s %v", w.Code, w.Body, tool.tagged())
	}
	n := len(tool.tagged())

	// The same validation as the hub's own: only vMAJOR.MINOR.PATCH and a full commit id.
	for _, b := range []string{
		`{"version":"v1","commit":"` + relSHA + `"}`, `{"version":"latest","commit":"` + relSHA + `"}`,
		`{"version":"v0.5.9; rm -rf /","commit":"` + relSHA + `"}`, `{"version":"v0.5.9","commit":"main"}`,
		`{"version":"v0.5.9","commit":"0123456"}`, `{"version":"v0.5.9","commit":"--force"}`, `{}`, `nope`,
	} {
		for _, p := range []string{"/api/release/tag", "/api/release/check"} {
			if w := do(s, "POST", p, hubCall, b); w.Code != 400 {
				t.Errorf("%s %s = %d %s", p, b, w.Code, w.Body)
			}
		}
	}
	if w := do(s, "GET", "/api/release/build?version=main", hubCall, ""); w.Code != 400 {
		t.Errorf("build version main = %d", w.Code)
	}
	if len(tool.tagged()) != n {
		t.Fatal("a bad request tagged something")
	}

	// A failing tool is a 500 with its reason, and a host with no clone says so.
	tool.mu.Lock()
	tool.tagErr = errors.New("push rejected")
	tool.mu.Unlock()
	if w := do(s, "POST", "/api/release/tag", hubCall, `{"version":"v0.5.12","commit":"`+relSHA+`"}`); w.Code != 500 || !strings.Contains(w.Body.String(), "push rejected") {
		t.Fatalf("failing tool = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(logs.String(), "release.failed") {
		t.Fatalf("no journal line for the failure:\n%s", logs.String())
	}
	s.cfg.Release.Tool = nil
	for _, c := range peerRelCalls {
		if w := do(s, c.method, c.path, hubCall, c.body); w.Code != 409 || !strings.Contains(w.Body.String(), "release_disabled") {
			t.Errorf("no clone: %s = %d %s", c.name, w.Code, w.Body)
		}
	}
}

func TestPeerTagsOneAtATimeAndFinishesWhenTheHubHangsUp(t *testing.T) {
	s, tool := peerTagServer(t)
	tool.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() {
		r := httptestReq("POST", "/api/release/tag", `{"version":"v0.5.9","commit":"`+relSHA+`"}`).WithContext(ctx)
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		r.Header.Set(fleet.HeaderHubCall, "1")
		first <- serveTo(s.mux, r).Code
	}()
	waitUntil(t, "tagging started", func() bool { return !s.rel.mu.TryLock() || func() bool { s.rel.mu.Unlock(); return false }() })
	if w := do(s, "POST", "/api/release/tag", hubCall, `{"version":"v0.5.10","commit":"`+relSHA+`"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "release_busy") {
		t.Fatalf("second tagging = %d %s", w.Code, w.Body)
	}
	cancel() // the hub gives up waiting: the push is not abandoned half way
	close(tool.gate)
	if code := <-first; code != 200 {
		t.Fatalf("first = %d", code)
	}
	if got := tool.tagged(); len(got) != 1 {
		t.Fatalf("tagged = %v", got)
	}
}

// ---- the hub's side ----

// relPeer is a peer's HTTP face for a hub's tests: the real peer handlers
// behind a stand-in for Tailscale Serve (which stamps the hub device
// owner's login), with a hook to make one op misbehave.
type relPeer struct {
	srv  *httptest.Server
	peer *Server
	tool *fakeTool

	mu   sync.Mutex
	reqs []*http.Request
	hook func(w http.ResponseWriter, r *http.Request) bool // true: handled
}

func newRelPeer(t *testing.T) *relPeer {
	t.Helper()
	p := &relPeer{}
	p.peer, p.tool = peerTagServer(t)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		hook := p.hook
		if strings.HasPrefix(r.URL.Path, "/api/release/") {
			p.reqs = append(p.reqs, r.Clone(context.Background()))
		}
		p.mu.Unlock()
		if hook != nil && hook(w, r) {
			return
		}
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		p.peer.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *relPeer) setHook(h func(w http.ResponseWriter, r *http.Request) bool) {
	p.mu.Lock()
	p.hook = h
	p.mu.Unlock()
}

func (p *relPeer) requests(path string) []*http.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*http.Request
	for _, r := range p.reqs {
		if r.URL.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// relHub is a hub whose release host is the peer "kube".
func relHub(t *testing.T, p *relPeer, mod func(*Config)) *Server {
	t.Helper()
	store, err := fleet.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(nil, "desk", nil, fleet.Options{HubMode: true, Logf: func(string, ...any) {}})
	fl.AttachStore(store, fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	cfg := Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken, StateDir: t.TempDir(),
		Release: ReleaseConfig{Host: "kube"}}
	if mod != nil {
		mod(&cfg)
	}
	s := New(cfg, nil, nil)
	b, _ := json.Marshal(map[string]any{"name": "kube", "urls": []string{p.srv.URL}})
	if w := do(s, "POST", "/api/peers", userHdr, string(b)); w.Code != 201 {
		t.Fatalf("add peer = %d %s", w.Code, w.Body)
	}
	waitUntil(t, "peer known", func() bool { return fl.HasPeer("kube") })
	return s
}

// forgetChecks drops the hub's cached checks, as the passing of the cache TTL would.
func forgetChecks(s *Server) {
	s.rel.cmu.Lock()
	s.rel.checks = map[string]relCached{}
	s.rel.cmu.Unlock()
}

func raiseOnHub(t *testing.T, s *Server, version, sha string) Todo {
	t.Helper()
	w := do(s, "POST", "/api/todos", userHdr, relBody(version, sha))
	if w.Code != http.StatusCreated {
		t.Fatalf("raise = %d %s", w.Code, w.Body)
	}
	return todoOf(t, w.Body.Bytes())
}

func TestHubTagsThroughItsReleaseHost(t *testing.T) {
	p := newRelPeer(t)
	s := relHub(t, p, nil)
	it := raiseOnHub(t, s, "v0.5.9", relSHA)

	// The page's view is the peer's check, and says who tags.
	w := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
	var v struct {
		CSRF    string        `json:"csrf"`
		CanTag  bool          `json:"can_tag"`
		Host    string        `json:"release_host"`
		DryRun  bool          `json:"dry_run"`
		Check   release.Check `json:"check"`
		CheckEr string        `json:"check_error"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || !v.CanTag || v.Host != "kube" || !v.Check.OK || v.Check.Commit != relSHA || v.CheckEr != "" {
		t.Fatalf("view = %d %s", w.Code, w.Body)
	}

	// Approving asks the peer, which runs its script; the hub runs nothing.
	if code, body := approve(s, it.ID, v.CSRF, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	got := waitRel(t, s, it.ID, relTagged)
	if got.Release.Host != "kube" || got.Release.RunURL != ghRun+"1" || got.Release.ApprovedBy != "me@example.com" {
		t.Fatalf("item = %+v", got.Release)
	}
	if tags := p.tool.tagged(); len(tags) != 1 || tags[0] != "v0.5.9 "+relSHA+" real" {
		t.Fatalf("the peer tagged %v", tags)
	}
	// The hub's own audit trail says which host did it.
	var sawApprove, sawTagged bool
	for _, e := range s.ledger.AuditLog(it.ID) {
		switch e.Action {
		case "release.approve":
			sawApprove = e.Host == "kube" && e.OK
		case "release.tagged":
			sawTagged = e.Host == "kube" && e.OK && strings.Contains(e.Detail, ghRun+"1")
		}
	}
	if !sawApprove || !sawTagged {
		t.Fatalf("audit = %v", actions(s))
	}
	// What the peer was sent: the hub's marker and a small JSON body, never the
	// browser's CSRF token or an agent token; the peer's journal learns who it was for.
	reqs := p.requests("/api/release/tag")
	if len(reqs) != 1 || reqs[0].Header.Get(fleet.HeaderHubCall) != "1" || reqs[0].Header.Get(HeaderCSRF) != "" || reqs[0].Header.Get(HeaderAgentToken) != "" {
		t.Fatalf("tag request = %+v", reqs)
	}

	// Once tagged, the build gate and the build chip come from the peer's gh.
	w = do(s, "GET", "/api/release/"+it.ID, userHdr, "")
	var after struct {
		Build release.Build `json:"build"`
	}
	if json.Unmarshal(w.Body.Bytes(), &after) != nil || after.Build.State != "success" || len(p.requests("/api/release/build")) == 0 {
		t.Fatalf("view after = %s", w.Body)
	}
	run := &fakeRunner{}
	s.cfg.Release.Upgrade, s.cfg.Release.Runner = true, run
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(v.CSRF), `{"todo":"`+it.ID+`","version":"v0.5.9"}`); w.Code != 202 || run.count() != 1 {
		t.Fatalf("upgrade = %d %s", w.Code, w.Body)
	}
	p.tool.mu.Lock()
	p.tool.build = release.Build{State: "failure", URL: ghRun + "1"}
	p.tool.mu.Unlock()
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(v.CSRF), `{"todo":"`+it.ID+`","version":"v0.5.9"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "build_not_green") || run.count() != 1 {
		t.Fatalf("upgrade with a failed build = %d %s", w.Code, w.Body)
	}
}

func TestHubKeepsItsGuardsWhenDelegating(t *testing.T) {
	p := newRelPeer(t)
	s := relHub(t, p, nil)
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	// CSRF, the confirm body and the user role are the hub's, checked before the peer is asked.
	if w := do(s, "POST", "/api/release/"+it.ID+"/approve", userHdr, `{"version":"v0.5.9","commit":"`+relSHA+`"}`); w.Code != 403 {
		t.Errorf("no csrf = %d", w.Code)
	}
	if code, _ := approve(s, it.ID, tok, "v0.5.9", relSHA2); code != 409 {
		t.Errorf("wrong commit = %d", code)
	}
	if w := do(s, "POST", "/api/release/"+it.ID+"/approve", map[string]string{HeaderAgentToken: testToken, HeaderAgent: "w1:p1", HeaderCSRF: tok}, `{"version":"v0.5.9","commit":"`+relSHA+`"}`); w.Code != 403 {
		t.Errorf("agent = %d", w.Code)
	}
	if len(p.requests("/api/release/tag")) != 0 || len(p.tool.tagged()) != 0 {
		t.Fatal("the peer was asked despite a refusal on the hub")
	}
	// A failing precondition on the peer blocks the click, with the peer's reason.
	p.tool.mu.Lock()
	p.tool.check = release.Check{Problem: "CI for 0123456 finished with failure: https://ci/9"}
	p.tool.mu.Unlock()
	code, body := approve(s, it.ID, tok, "v0.5.9", relSHA)
	if code != 409 || !strings.Contains(body, "precondition_failed") || !strings.Contains(body, "CI for 0123456 finished with failure") || len(p.requests("/api/release/tag")) != 0 {
		t.Fatalf("failing check = %d %s", code, body)
	}
}

func TestHubTagFailuresAreRecordedAndRetryable(t *testing.T) {
	retryOK := func(t *testing.T, p *relPeer, s *Server, id, tok, version string) {
		t.Helper()
		p.setHook(nil)
		p.tool.mu.Lock()
		p.tool.tagErr = nil
		p.tool.mu.Unlock()
		if code, body := approve(s, id, tok, version, relSHA); code != 202 {
			t.Fatalf("retry = %d %s", code, body)
		}
		it := waitRel(t, s, id, relTagged)
		if it.Release.Error != "" || it.Release.Host != "kube" {
			t.Fatalf("after retry = %+v", it.Release)
		}
	}
	cases := []struct {
		name  string
		setup func(p *relPeer)
		want  []string // in the item's reason
	}{
		{"the push fails on the peer", func(p *relPeer) {
			p.tool.mu.Lock()
			p.tool.tagErr = errors.New("remote: permission denied")
			p.tool.mu.Unlock()
		}, []string{"kube", "permission denied"}},
		{"the peer refuses", func(p *relPeer) {
			p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/release/tag" {
					return false
				}
				writeJSONStatus(w, 403, map[string]string{"code": "hub_only", "message": "no thanks"})
				return true
			})
		}, []string{"kube refused", "hub_only", "no thanks"}},
		{"the peer rejects the hub's login", func(p *relPeer) {
			p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/release/tag" {
					return false
				}
				http.Error(w, "forbidden", 403)
				return true
			})
		}, []string{"rejected the hub"}},
		{"the peer is busy", func(p *relPeer) {
			p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/release/tag" {
					return false
				}
				writeJSONStatus(w, 409, map[string]string{"code": "release_busy", "message": "a release is being tagged here"})
				return true
			})
		}, []string{"release_busy"}},
		{"the connection drops after the check", func(p *relPeer) {
			p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/release/tag" {
					return false
				}
				c, _, _ := w.(http.Hijacker).Hijack()
				c.Close()
				return true
			})
		}, []string{"kube", "may have pushed v0.5.9", "try again"}},
		{"the peer says it pushed without saying so", func(p *relPeer) {
			p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/release/tag" {
					return false
				}
				writeJSON(w, peerTagResp{Result: release.TagResult{Output: "ok"}})
				return true
			})
		}, []string{"without saying it pushed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newRelPeer(t)
			s := relHub(t, p, nil)
			it := raiseOnHub(t, s, "v0.5.9", relSHA)
			tok := token(t, s, it.ID)
			c.setup(p)
			if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
				t.Fatalf("approve = %d %s", code, body)
			}
			got := waitRel(t, s, it.ID, relFailed)
			for _, w := range c.want {
				if !strings.Contains(got.Release.Error, w) {
					t.Errorf("error %q lacks %q", got.Release.Error, w)
				}
			}
			if got.Release.TaggedAt != nil || got.Release.RunURL != "" {
				t.Errorf("a failed tagging holds a result: %+v", got.Release)
			}
			waitUntil(t, "flag cleared", func() bool { return !s.tagInFlight(it.ID) })
			var failed bool
			for _, e := range s.ledger.AuditLog(it.ID) {
				failed = failed || (e.Action == "release.failed" && e.Host == "kube" && !e.OK)
			}
			if !failed {
				t.Errorf("no failed event: %v", actions(s))
			}
			retryOK(t, p, s, it.ID, tok, "v0.5.9")
		})
	}
}

func TestHubWithAnUnreachableReleaseHost(t *testing.T) {
	p := newRelPeer(t)
	s := relHub(t, p, nil)
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	forgetChecks(s)
	p.setHook(func(w http.ResponseWriter, r *http.Request) bool { // the peer stops answering
		if strings.HasPrefix(r.URL.Path, "/api/release/") {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return true
		}
		return false
	})
	// The page says why it cannot check; approving is refused (nothing to go failed: nothing started).
	w := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
	if !strings.Contains(w.Body.String(), "check_error") || !strings.Contains(w.Body.String(), "kube") {
		t.Fatalf("view = %s", w.Body)
	}
	code, body := approve(s, it.ID, tok, "v0.5.9", relSHA)
	if code != 409 || !strings.Contains(body, "precondition_failed") || !strings.Contains(body, "kube") {
		t.Fatalf("approve = %d %s", code, body)
	}
	if cur, _ := s.todos.get(it.ID); cur.Release.State != relPending {
		t.Fatalf("state = %s", cur.Release.State)
	}
}

func TestHubDistrustsAPeersAnswer(t *testing.T) {
	p := newRelPeer(t)
	s := relHub(t, p, nil)
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	// A check about another commit is refused; links that are not https are dropped.
	forgetChecks(s)
	p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/release/check" {
			return false
		}
		writeJSON(w, peerCheckResp{Check: release.Check{OK: true, Commit: relSHA2}})
		return true
	})
	if w := do(s, "GET", "/api/release/"+it.ID+"?refresh=1", userHdr, ""); !strings.Contains(w.Body.String(), "checked "+relSHA2+", not "+relSHA) {
		t.Fatalf("view = %s", w.Body)
	}
	p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/release/tag":
			writeJSON(w, peerTagResp{Result: release.TagResult{Pushed: true, RunURL: "javascript:alert(1)"}})
			return true
		case "/api/release/check":
			writeJSON(w, peerCheckResp{Check: release.Check{OK: true, CIURL: "javascript:alert(2)", Commit: relSHA}})
			return true
		}
		return false
	})
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	if got := waitRel(t, s, it.ID, relTagged); got.Release.RunURL != "" {
		t.Fatalf("run url = %q", got.Release.RunURL)
	}
	// A link to anywhere but GitHub is dropped too.
	for _, u := range []string{"https://evil.example/actions/runs/1", "https://github.com.evil.example/x", "http://github.com/x"} {
		if httpsOnly(u) != "" {
			t.Errorf("httpsOnly(%q) kept", u)
		}
	}
	if httpsOnly(ghRun+"7") == "" {
		t.Error("a GitHub run link was dropped")
	}
	// A dry run that pushed is a failure, not a success.
	rt := s.cfg.Release.Tool.(*remoteTool)
	p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		writeJSON(w, peerTagResp{Result: release.TagResult{Pushed: true}, DryRun: true})
		return true
	})
	if _, err := rt.Tag(context.Background(), "v0.5.10", relSHA, true); err == nil || !strings.Contains(err.Error(), "although a dry run") {
		t.Fatalf("dry run that pushed: %v", err)
	}
	p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		writeJSON(w, release.Build{State: "<script>", URL: "http://x"})
		return true
	})
	if bd, err := rt.Build(context.Background(), "v0.5.9"); err != nil || bd.State != "none" || bd.URL != "" {
		t.Fatalf("build = %+v %v", bd, err)
	}
}

func TestHubDryRunThroughTheReleaseHost(t *testing.T) {
	// The peer's own -release-dry-run: the hub learns of it and tags nothing.
	p := newRelPeer(t)
	p.peer.cfg.Release.DryRun = true
	s := relHub(t, p, nil)
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	var v struct {
		CSRF   string `json:"csrf"`
		DryRun bool   `json:"dry_run"`
	}
	w := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || !v.DryRun {
		t.Fatalf("the page does not say the peer is in dry-run mode: %s", w.Body)
	}
	if code, body := approve(s, it.ID, v.CSRF, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	waitUntil(t, "dry run recorded", func() bool { c, _ := s.todos.get(it.ID); return c.Release.DryRun != "" || c.Release.State == relFailed })
	got, _ := s.todos.get(it.ID)
	if got.Release.State != relPending || got.Release.TaggedAt != nil || got.Release.RunURL != "" {
		t.Fatalf("after a peer dry run = %+v", got.Release)
	}
	if tags := p.tool.tagged(); len(tags) != 1 || !strings.HasSuffix(tags[0], " dry") {
		t.Fatalf("peer = %v", tags)
	}

	// The hub's own -release-dry-run asks the peer for one too.
	p2 := newRelPeer(t)
	s2 := relHub(t, p2, func(c *Config) { c.Release.DryRun = true })
	it2 := raiseOnHub(t, s2, "v0.5.9", relSHA)
	if code, body := approve(s2, it2.ID, token(t, s2, it2.ID), "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	waitUntil(t, "peer asked", func() bool { return len(p2.tool.tagged()) == 1 })
	if !strings.HasSuffix(p2.tool.tagged()[0], " dry") {
		t.Fatalf("peer = %v", p2.tool.tagged())
	}
}

func TestReleaseHostConfigIsNotAmbiguous(t *testing.T) {
	// Both a local tool and a release host: tagging is off, not guessed.
	p := newRelPeer(t)
	local := &fakeTool{check: okCheck()}
	s := relHub(t, p, func(c *Config) { c.Release.Tool = local })
	if s.cfg.Release.Tool != nil || s.cfg.Release.Host != "" {
		t.Fatalf("config = %+v", s.cfg.Release)
	}
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	if code, body := approve(s, it.ID, token(t, s, it.ID), "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "release_disabled") {
		t.Fatalf("approve = %d %s", code, body)
	}
	if len(local.tagged()) != 0 || len(p.tool.tagged()) != 0 || len(p.requests("/api/release/tag")) != 0 {
		t.Fatal("something was tagged under an ambiguous configuration")
	}
}

func TestReleaseHostThatIsNotAPeerFailsCleanly(t *testing.T) {
	p := newRelPeer(t)
	s := relHub(t, p, func(c *Config) { c.Release.Host = "nowhere" })
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	w := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
	if !strings.Contains(w.Body.String(), "no such host: nowhere") {
		t.Fatalf("view = %s", w.Body)
	}
	if code, body := approve(s, it.ID, token(t, s, it.ID), "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "no such host") {
		t.Fatalf("approve = %d %s", code, body)
	}
	if fmt.Sprint(p.requests("/api/release/tag")) != "[]" {
		t.Fatal("asked a peer that is not the release host")
	}
}

func TestPeerRefusesACheckWhileItTags(t *testing.T) {
	s, tool := peerTagServer(t)
	tool.gate = make(chan struct{})
	done := make(chan int, 1)
	go func() { done <- do(s, "POST", "/api/release/tag", hubCall, peerRelCalls[1].body).Code }()
	waitUntil(t, "tagging", func() bool { return s.rel.peerTagging.Load() })
	if w := do(s, "POST", "/api/release/check", hubCall, peerRelCalls[0].body); w.Code != 409 || !strings.Contains(w.Body.String(), "release_busy") {
		t.Fatalf("check while tagging = %d %s", w.Code, w.Body)
	}
	if tool.checks != 0 {
		t.Fatal("the clone was used by a check during the tagging")
	}
	close(tool.gate)
	if <-done != 200 {
		t.Fatal("tag failed")
	}
	if w := do(s, "POST", "/api/release/check", hubCall, peerRelCalls[0].body); w.Code != 200 {
		t.Fatalf("check after = %d %s", w.Code, w.Body)
	}
}

func TestViewDoesNotCheckWhileTagging(t *testing.T) {
	s, tool, _ := relServer(t)
	tool.gate = make(chan struct{})
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	n := tool.checks
	forgetChecks(s)
	w := do(s, "GET", "/api/release/"+it.ID+"?refresh=1", userHdr, "")
	if w.Code != 200 || tool.checks != n || strings.Contains(w.Body.String(), `"check"`) {
		t.Fatalf("view while tagging = %d (checks %d -> %d) %s", w.Code, n, tool.checks, w.Body)
	}
	close(tool.gate)
	waitRel(t, s, it.ID, relTagged)
}

func TestFailedItemWhosePushWentThroughIsReconciled(t *testing.T) {
	p := newRelPeer(t)
	s := relHub(t, p, nil)
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	// The push goes through, but the hub never gets the answer.
	p.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/release/tag" {
			return false
		}
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		rec := httptest.NewRecorder()
		p.peer.mux.ServeHTTP(rec, r)
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
		return true
	})
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	waitRel(t, s, it.ID, relFailed)
	if len(p.tool.tagged()) != 1 {
		t.Fatalf("peer = %v", p.tool.tagged())
	}
	p.setHook(nil)
	view := func() Todo {
		forgetChecks(s)
		if w := do(s, "GET", "/api/release/"+it.ID, userHdr, ""); w.Code != 200 {
			t.Fatalf("view = %d %s", w.Code, w.Body)
		}
		cur, _ := s.todos.get(it.ID)
		return cur
	}
	// Now the script refuses the version, and says where the tag is.
	setCheck := func(ck release.Check) {
		p.tool.mu.Lock()
		p.tool.check = ck
		p.tool.mu.Unlock()
	}
	// A tag at another commit changes nothing.
	setCheck(release.Check{Problem: "tag v0.5.9 already exists on origin", TagOnOrigin: relSHA2})
	if got := view(); got.Release.State != relFailed {
		t.Fatalf("a tag at another commit: %s", got.Release.State)
	}
	// An item the user never approved is not touched.
	setCheck(release.Check{Problem: "tag v0.5.9 already exists on origin", TagOnOrigin: relSHA})
	s.todos.mutate(it.ID, func(t *Todo) error { r := *t.Release; r.ApprovedBy = ""; t.Release = &r; return nil })
	if got := view(); got.Release.State != relFailed {
		t.Fatalf("unapproved item: %s", got.Release.State)
	}
	s.todos.mutate(it.ID, func(t *Todo) error { r := *t.Release; r.ApprovedBy = "me@example.com"; t.Release = &r; return nil })
	// The tag at the item's commit: tagged, once, audited.
	got := view()
	if got.Release.State != relTagged || got.Release.Error != "" || got.Release.TaggedAt == nil || got.Release.Host != "kube" {
		t.Fatalf("reconciled = %+v", got.Release)
	}
	view()
	n := 0
	for _, e := range s.ledger.AuditLog(it.ID) {
		if e.Action == "release.reconciled" && e.OK && e.Host == "kube" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d reconciled events: %v", n, actions(s))
	}
	// A pending item is never moved by a tag on origin.
	other := raiseOnHub(t, s, "v0.5.10", relSHA2)
	setCheck(release.Check{Problem: "tag exists", TagOnOrigin: relSHA2})
	forgetChecks(s)
	do(s, "GET", "/api/release/"+other.ID, userHdr, "")
	if cur, _ := s.todos.get(other.ID); cur.Release.State != relPending {
		t.Fatalf("pending item = %s", cur.Release.State)
	}
}
