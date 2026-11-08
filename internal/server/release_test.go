package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/release"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

const (
	relSHA  = "0123456789abcdef0123456789abcdef01234567"
	relSHA2 = "fedcba9876543210fedcba9876543210fedcba98"
)

// fakeTool is a release.Tool that records what it was asked and never runs
// anything, so no test here can tag, push or install.
type fakeTool struct {
	mu     sync.Mutex
	check  release.Check
	build  release.Build
	tagErr error
	tags   []string // "version commit dry"
	checks int
	gate   chan struct{} // when set, Tag waits for it
}

func okCheck() release.Check {
	return release.Check{OK: true, Short: "0123456", Subject: "s", Previous: "v0.5.8", CIStatus: "completed", CIConclusion: "success", CIURL: "https://ci/1", Changes: []string{"0123456 s"}}
}

func (f *fakeTool) Check(_ context.Context, v, c string) (release.Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	ck := f.check
	ck.Version, ck.Commit = v, c
	return ck, nil
}

func (f *fakeTool) Tag(_ context.Context, v, c string, dry bool) (release.TagResult, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags = append(f.tags, v+" "+c+" "+map[bool]string{true: "dry", false: "real"}[dry])
	if f.tagErr != nil {
		return release.TagResult{}, f.tagErr
	}
	return release.TagResult{Pushed: !dry, DryRun: dry, RunURL: "https://run/1", Output: "Pushed"}, nil
}

func (f *fakeTool) Build(_ context.Context, v string) (release.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.build, nil
}

func (f *fakeTool) tagged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tags...)
}

type fakeRunner struct {
	mu   sync.Mutex
	runs []string
	err  error
	done upgrade.Record // written as the finished record when set
}

func (f *fakeRunner) Start(_ context.Context, rec upgrade.Record, path string, rollback, dry bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, rec.Version+map[bool]string{true: " rollback", false: ""}[rollback]+map[bool]string{true: " dry", false: ""}[dry])
	if f.done.State != "" {
		d := f.done
		d.ID, d.Version, d.By, d.Started = rec.ID, rec.Version, rec.By, rec.Started
		return upgrade.WriteRecord(path, d)
	}
	return f.err
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// relServer is todoServer (w1:p1 is a recorded coordinator) with the
// release buttons wired to fakes.
func relServer(t *testing.T) (*Server, *fakeTool, *fakeRunner) {
	t.Helper()
	s, _, _ := todoServer(t)
	tool := &fakeTool{check: okCheck(), build: release.Build{State: "success", URL: "https://run/1"}}
	run := &fakeRunner{}
	s.cfg.Release = ReleaseConfig{Tool: tool, Upgrade: true, Runner: run}
	return s, tool, run
}

func relBody(version, sha string) string {
	return `{"question":"","kind":"release","release":{"version":"` + version + `","commit":"` + sha + `"}}`
}

func newRelease(t *testing.T, s *Server, version, sha string) Todo {
	t.Helper()
	w := do(s, "POST", "/api/todos", agentHdr("w1:p1"), relBody(version, sha))
	if w.Code != http.StatusCreated {
		t.Fatalf("raise release = %d %s", w.Code, w.Body)
	}
	return todoOf(t, w.Body.Bytes())
}

// token reads the CSRF token the way the page does.
func token(t *testing.T, s *Server, id string) string {
	t.Helper()
	w := do(s, "GET", "/api/release/"+id, userHdr, "")
	var v struct {
		CSRF string `json:"csrf"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.CSRF == "" {
		t.Fatalf("view = %d %s", w.Code, w.Body)
	}
	return v.CSRF
}

func withCSRF(tok string) map[string]string {
	return map[string]string{"Tailscale-User-Login": "me@example.com", HeaderCSRF: tok}
}

func approve(s *Server, id, tok, version, sha string) (int, string) {
	w := do(s, "POST", "/api/release/"+id+"/approve", withCSRF(tok), `{"version":"`+version+`","commit":"`+sha+`"}`)
	return w.Code, w.Body.String()
}

func waitRel(t *testing.T, s *Server, id, state string) Todo {
	t.Helper()
	var it Todo
	waitUntil(t, "release "+state, func() bool {
		it, _ = s.todos.get(id)
		return it.Release != nil && it.Release.State == state
	})
	return it
}

func actions(s *Server) []string {
	var out []string
	for _, e := range s.ledger.AuditLog("") {
		out = append(out, e.Action)
	}
	return out
}

// Every route that mentions /release is registered with s.auth, the user
// role alone. A new one added with another wrapper fails here.
func TestReleaseRoutesAreUserOnly(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(src), "\n") {
		if !regexp.MustCompile(`HandleFunc\("[A-Z]+ /api/(release|hosts/\{host\}/release)`).MatchString(line) {
			continue
		}
		n++
		if !strings.Contains(line, ", s.auth(s.") {
			t.Errorf("a release route is not user-only: %s", strings.TrimSpace(line))
		}
	}
	if n != 5 {
		t.Fatalf("found %d release routes, want 5: update this test with the route", n)
	}
}

func TestReleaseRefusesAgentsAndPeers(t *testing.T) {
	s, tool, run := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approveBody := `{"version":"v0.5.9","commit":"` + relSHA + `"}`
	hostBody := `{"todo":"` + it.ID + `","version":"v0.5.9"}`
	calls := []struct{ method, path, body string }{
		{"GET", "/api/release/" + it.ID, ""},
		{"POST", "/api/release/" + it.ID + "/approve", approveBody},
		{"GET", "/api/release/upgrade", ""},
		{"POST", "/api/release/upgrade", `{"version":"v0.5.9"}`},
		{"POST", "/api/hosts/desk/release/upgrade", hostBody},
	}
	callers := map[string]map[string]string{
		// A coordinator, with the agent token and even the page's token.
		"coordinator token": {HeaderAgentToken: testToken, HeaderAgent: "w1:p1", HeaderCSRF: tok},
		"agent token":       {HeaderAgentToken: testToken, HeaderAgent: "w2:p1", HeaderCSRF: tok},
		"token, no pane":    {HeaderAgentToken: testToken, HeaderCSRF: tok},
		// A peer forwarding for its agents (authenticated as the hub's user).
		"peer forwarding": {"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgent: "w1:p1", HeaderAgentHost: "kube", HeaderCSRF: tok},
		// A wrong token is refused outright, never downgraded to the user.
		"wrong token": {HeaderAgentToken: "nope", "Tailscale-User-Login": "me@example.com", HeaderCSRF: tok},
		// No identity at all.
		"nobody": {HeaderCSRF: tok},
	}
	for who, hdr := range callers {
		for _, c := range calls {
			w := do(s, c.method, c.path, hdr, c.body)
			want := http.StatusForbidden
			if who == "wrong token" || who == "nobody" {
				want = http.StatusUnauthorized
			}
			if w.Code != want {
				t.Errorf("%s %s %s = %d %s, want %d", who, c.method, c.path, w.Code, w.Body, want)
			}
		}
	}
	if got := tool.tagged(); len(got) != 0 || run.count() != 0 {
		t.Fatalf("a refused caller got something done: tags %v, upgrades %d", got, run.count())
	}
	if it, _ := s.todos.get(it.ID); it.Release.State != relPending {
		t.Fatalf("state = %s", it.Release.State)
	}
}

func TestReleaseRefusesTaggedNodes(t *testing.T) {
	tool := &fakeTool{check: okCheck()}
	run := &fakeRunner{}
	r := newCapRig2(t, tool, run)
	r.who.setHolder(r.node, "tag:worker")
	r.addPeer("vm1", r.workerURL(), "tag:worker") // a registered peer: admitted, as the forwarded agent
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/release/tx", ""},
		{"POST", "/api/release/tx/approve", `{"version":"v0.5.9","commit":"` + relSHA + `"}`},
		{"POST", "/api/release/upgrade", `{"version":"v0.5.9"}`},
		{"GET", "/api/release/upgrade", ""},
		{"POST", "/api/hosts/desk/release/upgrade", `{"todo":"tx","version":"v0.5.9"}`},
	} {
		code, body := r.serve(c.method, c.path, c.body, map[string]string{HeaderCSRF: "x"})
		if code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("tagged node %s %s = %d %s", c.method, c.path, code, body)
		}
	}
	if len(tool.tagged()) != 0 || run.count() != 0 {
		t.Fatal("a tagged node got something done")
	}
}

func TestReleaseNeedsTheCSRFToken(t *testing.T) {
	s, tool, run := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approveBody := `{"version":"v0.5.9","commit":"` + relSHA + `"}`
	for name, hdr := range map[string]map[string]string{
		"missing":         {"Tailscale-User-Login": "me@example.com"},
		"wrong":           {"Tailscale-User-Login": "me@example.com", HeaderCSRF: "0000"},
		"another login's": {"Tailscale-User-Login": "other@example.com", HeaderCSRF: tok},
	} {
		if name == "another login's" {
			s.cfg.AllowLogins = append(s.cfg.AllowLogins, "other@example.com")
		}
		if w := do(s, "POST", "/api/release/"+it.ID+"/approve", hdr, approveBody); w.Code != 403 || !strings.Contains(w.Body.String(), `"csrf"`) {
			t.Errorf("approve with %s token = %d %s", name, w.Code, w.Body)
		}
		if w := do(s, "POST", "/api/hosts/desk/release/upgrade", hdr, `{"todo":"`+it.ID+`","version":"v0.5.9"}`); w.Code != 403 {
			t.Errorf("upgrade with %s token = %d %s", name, w.Code, w.Body)
		}
	}
	if len(tool.tagged()) != 0 || run.count() != 0 {
		t.Fatal("something ran without a valid token")
	}
}

// The full handler, with the cross-origin and JSON-only wrappers a request
// from a browser meets: another site's form post does not get through.
func TestReleaseCrossSiteIsRefused(t *testing.T) {
	s, tool, _ := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	h := s.base()
	for name, hdr := range map[string]map[string]string{
		"cross-site":   {"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"},
		"wrong origin": {"Origin": "https://evil.example", "Content-Type": "application/json"},
		"form post":    {"Content-Type": "application/x-www-form-urlencoded"},
	} {
		r := httptestReq("POST", "/api/release/"+it.ID+"/approve", `{"version":"v0.5.9","commit":"`+relSHA+`"}`)
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		r.Header.Set(HeaderCSRF, tok)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := serveTo(h, r)
		if w.Code < 400 {
			t.Errorf("%s = %d", name, w.Code)
		}
	}
	if len(tool.tagged()) != 0 {
		t.Fatal("tagged")
	}
}

func TestReleasePreconditions(t *testing.T) {
	s, tool, _ := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	for name, problem := range map[string]string{
		"CI pending": "CI for 0123456 is still in_progress: https://ci/1",
		"CI failed":  "CI for 0123456 finished with failure: https://ci/1",
		"off main":   "commit 0123456 is not on origin/main",
		"tag exists": "tag v0.5.9 already exists on origin",
		"not newer":  "v0.5.9 is not greater than the latest tag v0.6.0",
	} {
		tool.mu.Lock()
		tool.check = release.Check{Problem: problem}
		tool.mu.Unlock()
		code, body := approve(s, it.ID, tok, "v0.5.9", relSHA)
		if code != http.StatusConflict || !strings.Contains(body, "precondition_failed") || !strings.Contains(body, problem) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if len(tool.tagged()) != 0 {
		t.Fatalf("tagged despite a failed precondition: %v", tool.tagged())
	}
	if got, _ := s.todos.get(it.ID); got.Release.State != relPending {
		t.Fatalf("state = %s", got.Release.State)
	}
	// Refusals are in the ledger too.
	if a := actions(s); len(a) != 5 || a[0] != "release.refused" {
		t.Fatalf("audit = %v", a)
	}
	// The confirm step must name this item's version and commit.
	tool.mu.Lock()
	tool.check = okCheck()
	tool.mu.Unlock()
	for _, c := range [][2]string{{"v0.5.8", relSHA}, {"v0.5.9", relSHA2}, {"", ""}} {
		if code, body := approve(s, it.ID, tok, c[0], c[1]); code != 409 || !strings.Contains(body, "confirm_mismatch") {
			t.Errorf("confirm %v = %d %s", c, code, body)
		}
	}
	if len(tool.tagged()) != 0 {
		t.Fatal("tagged on a mismatched confirm")
	}
}

func TestReleaseApproveTagsOnceAndWritesTheLedger(t *testing.T) {
	s, tool, _ := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	code, body := approve(s, it.ID, tok, "v0.5.9", relSHA)
	if code != http.StatusAccepted {
		t.Fatalf("approve = %d %s", code, body)
	}
	got := waitRel(t, s, it.ID, relTagged)
	if tags := tool.tagged(); len(tags) != 1 || tags[0] != "v0.5.9 "+relSHA+" real" {
		t.Fatalf("tag calls = %v", tags)
	}
	if got.Release.ApprovedBy != "me@example.com" || got.Release.RunURL != "https://run/1" || got.Release.TaggedAt == nil {
		t.Fatalf("item = %+v", got.Release)
	}
	// A second approval is refused and tags nothing.
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "release_state") {
		t.Fatalf("again = %d %s", code, body)
	}
	if len(tool.tagged()) != 1 {
		t.Fatalf("tagged twice: %v", tool.tagged())
	}
	log := s.ledger.AuditLog(it.ID)
	if len(log) != 2 || log[0].Action != "release.approve" || log[1].Action != "release.tagged" ||
		log[0].By != "me@example.com" || !log[0].OK || !strings.Contains(log[0].Detail, "v0.5.9") || !strings.Contains(log[1].Detail, relSHA) || log[0].At.IsZero() {
		t.Fatalf("audit = %+v", log)
	}
	// The ledger file has them, for the next start.
	l2, err := ledger.Open(s.cfg.StateDir + "/ledger.json")
	if err != nil || len(l2.AuditLog(it.ID)) != 2 {
		t.Fatalf("persisted audit: %v %v", err, l2.AuditLog(""))
	}
}

func TestReleaseTagFailureIsRecordedAndRetryable(t *testing.T) {
	s, tool, _ := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	tool.mu.Lock()
	tool.tagErr = os.ErrPermission
	tool.mu.Unlock()
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	got := waitRel(t, s, it.ID, relFailed)
	if got.Release.Error == "" {
		t.Fatal("no error recorded")
	}
	if a := actions(s); a[len(a)-1] != "release.failed" {
		t.Fatalf("audit = %v", a)
	}
	tool.mu.Lock()
	tool.tagErr = nil
	tool.mu.Unlock()
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("retry = %d %s", code, body)
	}
	waitRel(t, s, it.ID, relTagged)
}

func TestReleaseDryRunTagsNothing(t *testing.T) {
	s, tool, _ := relServer(t)
	s.cfg.Release.DryRun = true
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	waitUntil(t, "dry run done", func() bool { return len(tool.tagged()) == 1 && len(actions(s)) == 2 })
	if tags := tool.tagged(); tags[0] != "v0.5.9 "+relSHA+" dry" {
		t.Fatalf("tags = %v", tags)
	}
	got, _ := s.todos.get(it.ID)
	if got.Release.State != relPending || got.Release.DryRun == "" {
		t.Fatalf("item = %+v", got.Release)
	}
	if a := actions(s); a[1] != "release.dry_run" {
		t.Fatalf("audit = %v", a)
	}
}

func TestReleaseOnlyOneTaggingAtATime(t *testing.T) {
	s, tool, _ := relServer(t)
	tool.gate = make(chan struct{})
	a := newRelease(t, s, "v0.5.9", relSHA)
	b := newRelease(t, s, "v0.5.10", relSHA2)
	tok := token(t, s, a.ID)
	if code, body := approve(s, a.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("first = %d %s", code, body)
	}
	if code, body := approve(s, b.ID, tok, "v0.5.10", relSHA2); code != 409 || !strings.Contains(body, "release_busy") {
		t.Fatalf("second = %d %s", code, body)
	}
	// The item being tagged cannot be dropped from under the script.
	if w := do(s, "POST", "/api/todos/"+a.ID+"/drop", userHdr, `{}`); w.Code != 409 {
		t.Fatalf("drop while tagging = %d %s", w.Code, w.Body)
	}
	close(tool.gate)
	waitRel(t, s, a.ID, relTagged)
}

func TestReleaseItemIsRaisedOnlyByTheUserOrACoordinator(t *testing.T) {
	s, tool, _ := relServer(t)
	// A plain agent, a pane-less agent: no.
	for _, hdr := range []map[string]string{agentHdr("w2:p1"), agentHdr("")} {
		if w := do(s, "POST", "/api/todos", hdr, relBody("v0.5.9", relSHA)); w.Code != 403 || !strings.Contains(w.Body.String(), "coordinator_only") {
			t.Errorf("agent raise = %d %s", w.Code, w.Body)
		}
	}
	// Bad data.
	for _, body := range []string{
		relBody("0.5.9", relSHA),                        // version
		relBody("v0.5.9", "0123456"),                    // short id
		relBody("v0.5.9", "main"),                       // a ref
		relBody("v0.5.9", strings.ToUpper(relSHA)+"zz"), // not hex
		`{"question":"q","kind":"release"}`,             // no data
		`{"question":"q","kind":"decision","release":{"version":"v0.5.9","commit":"` + relSHA + `"}}`, // data on another kind
	} {
		if w := do(s, "POST", "/api/todos", agentHdr("w1:p1"), body); w.Code != 400 {
			t.Errorf("%.80s = %d %s", body, w.Code, w.Body)
		}
	}
	it := newRelease(t, s, "v0.5.9", relSHA)
	if it.Release == nil || it.Release.State != relPending || it.Release.Commit != relSHA || !strings.Contains(it.Question, "v0.5.9") {
		t.Fatalf("item = %+v", it)
	}
	// One open item per version.
	if w := do(s, "POST", "/api/todos", agentHdr("w1:p1"), relBody("v0.5.9", relSHA2)); w.Code != 409 {
		t.Fatalf("duplicate version = %d %s", w.Code, w.Body)
	}
	// A coordinator relaying an "answer", finishing or dropping the item
	// approves nothing and tags nothing.
	if w := do(s, "POST", "/api/todos/"+it.ID+"/answer", agentHdr("w1:p1"), `{"text":"yes, release it"}`); w.Code != 200 {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
	if len(tool.tagged()) != 0 {
		t.Fatal("an answer tagged")
	}
	got, _ := s.todos.get(it.ID)
	if got.Release.State != relPending {
		t.Fatalf("state = %s", got.Release.State)
	}
	// Tagged nodes' route to add a to-do is open; a release is not.
}

func TestReleaseViewShowsTheLiveCheck(t *testing.T) {
	s, _, _ := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	w := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
	var v struct {
		Check  release.Check `json:"check"`
		CanTag bool          `json:"can_tag"`
		CSRF   string        `json:"csrf"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != 200 {
		t.Fatalf("%v %d %s", err, w.Code, w.Body)
	}
	if !v.CanTag || !v.Check.OK || v.Check.CIURL == "" || v.Check.Previous != "v0.5.8" || v.Check.Commit != relSHA || v.CSRF == "" {
		t.Fatalf("view = %+v", v)
	}
	if w := do(s, "GET", "/api/release/tnope", userHdr, ""); w.Code != 404 {
		t.Fatalf("missing = %d", w.Code)
	}
	// A plain to-do has no release view.
	plain := todoOf(t, do(s, "POST", "/api/todos", userHdr, `{"question":"plain"}`).Body.Bytes())
	if w := do(s, "GET", "/api/release/"+plain.ID, userHdr, ""); w.Code != 404 {
		t.Fatalf("plain item = %d", w.Code)
	}
}

func TestReleaseApproveOffWithoutARepo(t *testing.T) {
	s, _, _ := relServer(t)
	s.cfg.Release.Tool = nil
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "release_disabled") {
		t.Fatalf("approve = %d %s", code, body)
	}
}

func tagIt(t *testing.T, s *Server, id string) string {
	t.Helper()
	tok := token(t, s, id)
	if code, body := approve(s, id, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	waitRel(t, s, id, relTagged)
	return tok
}

func TestUpgradeNeedsATaggedReleaseWithAGreenBuild(t *testing.T) {
	s, tool, run := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	body := `{"todo":"` + it.ID + `","version":"v0.5.9"}`
	// Not tagged yet.
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), body); w.Code != 409 || !strings.Contains(w.Body.String(), "not_released") {
		t.Fatalf("untagged = %d %s", w.Code, w.Body)
	}
	tagIt(t, s, it.ID)
	// The build is not green.
	tool.mu.Lock()
	tool.build = release.Build{State: "in_progress", URL: "https://run/1"}
	tool.mu.Unlock()
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), body); w.Code != 409 || !strings.Contains(w.Body.String(), "build_not_green") {
		t.Fatalf("building = %d %s", w.Code, w.Body)
	}
	// Wrong version, unknown host.
	tool.mu.Lock()
	tool.build = release.Build{State: "success"}
	tool.mu.Unlock()
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), `{"todo":"`+it.ID+`","version":"v0.6.0"}`); w.Code != 409 {
		t.Fatalf("wrong version = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/hosts/nowhere/release/upgrade", withCSRF(tok), body); w.Code != 404 {
		t.Fatalf("unknown host = %d %s", w.Code, w.Body)
	}
	if run.count() != 0 {
		t.Fatalf("started %d upgrades", run.count())
	}
	// Green: it starts, once, and the host's record says running.
	w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), body)
	if w.Code != 202 {
		t.Fatalf("upgrade = %d %s", w.Code, w.Body)
	}
	if run.count() != 1 || run.runs[0] != "v0.5.9" {
		t.Fatalf("runs = %v", run.runs)
	}
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), body); w.Code != 409 || !strings.Contains(w.Body.String(), "upgrade_running") {
		t.Fatalf("second = %d %s", w.Code, w.Body)
	}
	st := s.upgradeStatus()
	if st.Record == nil || st.Record.State != upgrade.RecRunning || st.Record.By != "me@example.com" || st.Record.Version != "v0.5.9" {
		t.Fatalf("status = %+v", st.Record)
	}
	log := s.ledger.AuditLog(it.ID)
	var acts []string
	for _, e := range log {
		acts = append(acts, e.Action)
	}
	if strings.Join(acts, ",") != "release.approve,release.tagged,upgrade.refused,upgrade.request" && !strings.Contains(strings.Join(acts, ","), "upgrade.request") {
		t.Fatalf("audit = %v", acts)
	}
	all := actions(s)
	if !strings.Contains(strings.Join(all, ","), "upgrade.start") {
		t.Fatalf("no upgrade.start in %v", all)
	}
}

func TestUpgradeResultIsAuditedOnceWhenItEnds(t *testing.T) {
	s, _, run := relServer(t)
	run.done = upgrade.Record{State: upgrade.RecRolledBack, From: "v0.5.8", To: "v0.5.8", Error: "smoke check failed", Finished: time.Now()}
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := tagIt(t, s, it.ID)
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), `{"todo":"`+it.ID+`","version":"v0.5.9"}`); w.Code != 202 {
		t.Fatalf("upgrade = %d %s", w.Code, w.Body)
	}
	for i := 0; i < 3; i++ { // the page polls; the result is written once
		w := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
		if !strings.Contains(w.Body.String(), `"rolled_back"`) {
			t.Fatalf("view lacks the verdict: %s", w.Body)
		}
	}
	n := 0
	for _, e := range s.ledger.AuditLog("") {
		if e.Action == "upgrade.result" {
			n++
			if e.OK || !strings.Contains(e.Detail, "rolled_back") || e.By != "me@example.com" {
				t.Fatalf("result event = %+v", e)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d result events", n)
	}
	// A rollback is the same route with rollback set.
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), `{"todo":"`+it.ID+`","version":"v0.5.9","rollback":true}`); w.Code != 202 {
		t.Fatalf("rollback = %d %s", w.Code, w.Body)
	}
	if run.runs[len(run.runs)-1] != "v0.5.9 rollback" {
		t.Fatalf("runs = %v", run.runs)
	}
}

func TestUpgradeLocalValidation(t *testing.T) {
	s, _, run := relServer(t)
	for _, v := range []string{"latest", "", "v1", "v0.5.9; rm -rf /", "../v0.5.9"} {
		b, _ := json.Marshal(map[string]string{"version": v})
		if w := do(s, "POST", "/api/release/upgrade", userHdr, string(b)); w.Code != 400 {
			t.Errorf("version %q = %d %s", v, w.Code, w.Body)
		}
	}
	s.cfg.Release.Upgrade = false
	if w := do(s, "POST", "/api/release/upgrade", userHdr, `{"version":"v0.5.9"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "upgrade_disabled") {
		t.Fatalf("disabled = %d %s", w.Code, w.Body)
	}
	if run.count() != 0 {
		t.Fatal("ran")
	}
}

func TestStaleUpgradeRecordIsNotRunning(t *testing.T) {
	s, _, _ := relServer(t)
	rec := upgrade.Record{ID: "a", Version: "v0.5.9", State: upgrade.RecRunning, Started: time.Now().Add(-time.Hour)}
	if err := upgrade.WriteRecord(s.upgradeRecordPath(), rec); err != nil {
		t.Fatal(err)
	}
	if st := s.upgradeStatus(); st.Record.State != upgrade.RecFailed {
		t.Fatalf("stale record = %+v", st.Record)
	}
	// ...and does not block a new run.
	if w := do(s, "POST", "/api/release/upgrade", userHdr, `{"version":"v0.5.9"}`); w.Code != 202 {
		t.Fatalf("start after stale = %d %s", w.Code, w.Body)
	}
}

// A peer's upgrade is asked of the peer by the hub on the user's click, with
// none of the browser's headers, and the hub audits it.
func TestUpgradeAPeerThroughTheHub(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	var hdrs []http.Header
	rec := `{"enabled":true,"version":"v0.5.8","record":{"id":"r1","version":"v0.5.9","state":"ok","from":"v0.5.8","to":"v0.5.9","by":"me@example.com","started":"2026-01-01T00:00:00Z"}}`
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: agents\ndata: {\"type\":\"agents\",\"version\":\"v0.5.8\",\"agents\":[]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == "POST" && r.URL.Path == "/api/release/upgrade":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			posts, hdrs = append(posts, string(b)), append(hdrs, r.Header.Clone())
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			fmt.Fprint(w, `{"record":{"id":"r1"}}`)
		case r.Method == "GET" && r.URL.Path == "/api/release/upgrade":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, rec)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peer.Close)
	store, err := fleet.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(nil, "desk", nil, fleet.Options{HubMode: true, Logf: func(string, ...any) {}})
	fl.AttachStore(store, fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	tool := &fakeTool{check: okCheck(), build: release.Build{State: "success"}}
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken, StateDir: t.TempDir(),
		Release: ReleaseConfig{Tool: tool}}, nil, nil)
	b, _ := json.Marshal(map[string]any{"name": "kube", "urls": []string{peer.URL}})
	if w := do(s, "POST", "/api/peers", userHdr, string(b)); w.Code != 201 {
		t.Fatalf("add peer = %d %s", w.Code, w.Body)
	}
	waitUntil(t, "peer up", func() bool { return fl.HasPeer("kube") })
	it := todoOf(t, do(s, "POST", "/api/todos", userHdr, relBody("v0.5.9", relSHA)).Body.Bytes())
	tok := tagIt(t, s, it.ID)

	w := do(s, "POST", "/api/hosts/kube/release/upgrade", withCSRF(tok), `{"todo":"`+it.ID+`","version":"v0.5.9"}`)
	if w.Code != 202 {
		t.Fatalf("upgrade kube = %d %s", w.Code, w.Body)
	}
	mu.Lock()
	if len(posts) != 1 || posts[0] != `{"version":"v0.5.9"}` {
		t.Fatalf("peer got %v", posts)
	}
	if hdrs[0].Get(HeaderCSRF) != "" || hdrs[0].Get(HeaderAgentToken) != "" {
		t.Fatalf("the browser's headers reached the peer: %v", hdrs[0])
	}
	mu.Unlock()
	// The view shows the peer's host with its record; its result is audited once.
	for i := 0; i < 2; i++ {
		v := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
		var out struct {
			Hosts []relHost `json:"hosts"`
		}
		json.Unmarshal(v.Body.Bytes(), &out)
		var kube *relHost
		for i := range out.Hosts {
			if out.Hosts[i].Name == "kube" {
				kube = &out.Hosts[i]
			}
		}
		if kube == nil || !kube.Enabled || kube.Record == nil || kube.Record.State != "ok" {
			t.Fatalf("hosts = %s", v.Body)
		}
	}
	n := 0
	for _, e := range s.ledger.AuditLog("") {
		if e.Action == "upgrade.result" && e.Host == "kube" && e.OK {
			n++
		}
		if e.Action == "upgrade.request" && (e.Host != "kube" || !e.OK) {
			t.Fatalf("request event = %+v", e)
		}
	}
	if n != 1 {
		t.Fatalf("%d result events for kube", n)
	}
}
