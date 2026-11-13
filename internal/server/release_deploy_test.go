package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/release"
)

// deployScript writes a program that records how it was called, and returns
// its path and the file it writes.
func deployScript(t *testing.T, body string) (script, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "ran")
	script = filepath.Join(dir, "deploy.sh")
	src := "#!/bin/sh\necho \"args=$* env=$COMEBYE_RELEASE_VERSION\" >> " + marker + "\n" + body + "\n"
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func deployHub(t *testing.T, cmd ...string) (*Server, *fakeTool) {
	t.Helper()
	s, tool, _ := relServer(t)
	s.cfg.Release.DeployCmd = cmd
	s.cfg.Release.DeployLabel = "Bump hub"
	return s, tool
}

func doDeploy(s *Server, id, tok, version, sha string) (int, string) {
	w := do(s, "POST", "/api/release/"+id+"/deploy", withCSRF(tok), `{"version":"`+version+`","commit":"`+sha+`"}`)
	return w.Code, w.Body.String()
}

func deployState(s *Server, id string) (deployStatus, bool) {
	w := do(s, "GET", "/api/release/"+id, userHdr, "")
	var v struct {
		Deploy *deployStatus `json:"deploy"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Deploy == nil {
		return deployStatus{}, false
	}
	return *v.Deploy, true
}

// Approve release must never deploy; the second click does, and only for a
// tagged item whose build passed.
func TestDeployIsASeparateClickAfterTheBuild(t *testing.T) {
	script, marker := deployScript(t, "echo deploying")
	s, tool := deployHub(t, script)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)

	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "not_released") {
		t.Fatalf("deploy before tagging = %d %s", code, body)
	}
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	waitRel(t, s, it.ID, relTagged)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Approve release ran the deploy command")
	}

	if code, body := doDeploy(s, it.ID, tok, "v0.5.8", relSHA); code != 409 || !strings.Contains(body, "confirm_mismatch") {
		t.Fatalf("wrong version = %d %s", code, body)
	}
	tool.mu.Lock()
	tool.build = release.Build{State: "in_progress"}
	tool.mu.Unlock()
	s.rel.cmu.Lock()
	s.rel.checks = map[string]relCached{}
	s.rel.cmu.Unlock()
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "build_not_green") {
		t.Fatalf("deploy while building = %d %s", code, body)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a deploy ran while the build was not green")
	}

	tool.mu.Lock()
	tool.build = release.Build{State: "success", URL: "https://run/1"}
	tool.mu.Unlock()
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("deploy = %d %s", code, body)
	}
	var st deployStatus
	waitUntil(t, "deploy done", func() bool {
		var ok bool
		st, ok = deployState(s, it.ID)
		return ok && st.Record != nil && st.Record.State == depOK
	})
	got, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(got)) != "args=v0.5.9 env=v0.5.9" {
		t.Fatalf("command saw %q", got)
	}
	if st.Label != "Bump hub" || !strings.Contains(st.Record.Output, "deploying") || st.Command != "deploy.sh" {
		t.Fatalf("status = %+v", st)
	}
	found := false
	for _, a := range actions(s) {
		found = found || strings.Contains(a, "deploy.result")
	}
	if !found {
		t.Fatalf("no deploy.result in the audit log: %v", actions(s))
	}
}

func TestDeployFailureAndSingleFlight(t *testing.T) {
	script, _ := deployScript(t, "echo boom >&2\nexit 3")
	s, _ := deployHub(t, script)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approve(s, it.ID, tok, "v0.5.9", relSHA)
	waitRel(t, s, it.ID, relTagged)
	s.rel.deploying.Store(true)
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "deploy_busy") {
		t.Fatalf("second deploy = %d %s", code, body)
	}
	s.rel.deploying.Store(false)
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("deploy = %d %s", code, body)
	}
	waitUntil(t, "deploy failed", func() bool {
		st, ok := deployState(s, it.ID)
		return ok && st.Record != nil && st.Record.State == depFailed && strings.Contains(st.Record.Output, "boom")
	})
}

func TestDeployDryRunRunsNothing(t *testing.T) {
	script, marker := deployScript(t, "")
	s, tool := deployHub(t, script)
	s.cfg.Release.DryRun = true
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approve(s, it.ID, tok, "v0.5.9", relSHA) // a dry run tags nothing: the item goes back to pending
	waitUntil(t, "dry run settled", func() bool {
		cur, _ := s.todos.get(it.ID)
		return len(tool.tagged()) == 1 && cur.Release.State == relPending && cur.Release.DryRun != ""
	})
	s.todos.mutate(it.ID, func(td *Todo) error { r := *td.Release; r.State = relTagged; td.Release = &r; return nil })
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("deploy = %d %s", code, body)
	}
	st, _ := deployState(s, it.ID)
	if st.Record == nil || st.Record.State != depDryRun {
		t.Fatalf("record = %+v", st.Record)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a dry run ran the command")
	}
}

func TestDeployOffWithoutACommand(t *testing.T) {
	s, _ := deployHub(t) // no command
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approve(s, it.ID, tok, "v0.5.9", relSHA)
	waitRel(t, s, it.ID, relTagged)
	if _, ok := deployState(s, it.ID); ok {
		t.Fatal("the page offers a deploy with no command configured")
	}
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "deploy_disabled") {
		t.Fatalf("deploy = %d %s", code, body)
	}
}

func TestStaleRunningDeployRecordIsFailed(t *testing.T) {
	script, _ := deployScript(t, "")
	s, _ := deployHub(t, script)
	s.cfg.StateDir = t.TempDir()
	s.writeDeployRecord(DeployRecord{Version: "v0.5.9", State: depRunning, Started: time.Now().UTC()})
	st := s.deployStatusLocal()
	if st.Record == nil || st.Record.State != depFailed || !strings.Contains(st.Record.Error, "stopped without a verdict") {
		t.Fatalf("record = %+v", st.Record)
	}
}

// The peer's side: the hub's call runs it, only after the peer saw the build
// pass; anything else is refused.
func TestPeerDeployRoutes(t *testing.T) {
	script, marker := deployScript(t, "echo ok")
	s, tool := peerTagServer(t)
	s.cfg.Release.DeployCmd = []string{script}
	body := `{"version":"v0.5.9","todo":"t1","by":"me"}`
	if w := do(s, "POST", "/api/release/deploy", userHdr, body); w.Code != 403 {
		t.Fatalf("a browser = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/release/deploy", map[string]string{HeaderAgentToken: testToken, HeaderAgent: "w1:p1"}, body); w.Code != 403 {
		t.Fatalf("an agent = %d", w.Code)
	}
	tool.mu.Lock()
	tool.build = release.Build{State: "failure"}
	tool.mu.Unlock()
	if w := do(s, "POST", "/api/release/deploy", hubCall, body); w.Code != 409 || !strings.Contains(w.Body.String(), "build_not_green") {
		t.Fatalf("red build = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/release/deploy", hubCall, `{"version":"v0.5.9; rm -rf /"}`); w.Code != 400 {
		t.Fatalf("bad version = %d", w.Code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused call ran the command")
	}
	tool.mu.Lock()
	tool.build = release.Build{State: "success"}
	tool.mu.Unlock()
	if w := do(s, "POST", "/api/release/deploy", hubCall, body); w.Code != 202 {
		t.Fatalf("hub call = %d %s", w.Code, w.Body)
	}
	waitUntil(t, "peer deploy done", func() bool {
		w := do(s, "GET", "/api/release/deploy", hubCall, "")
		var st deployStatus
		return w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &st) == nil && st.Record != nil && st.Record.State == depOK
	})
}

// The deploy's "by" names a person once when the hub and the peer agree, and
// keeps the hub's claim only when it differs.
func TestPeerDeployByIsSaidOnce(t *testing.T) {
	script, _ := deployScript(t, "echo ok")
	for _, c := range []struct{ body, want string }{
		{`{"version":"v0.5.9","by":"me@example.com"}`, "me@example.com"},
		{`{"version":"v0.5.9","by":"Me@Example.com"}`, "me@example.com"},
		{`{"version":"v0.5.9","by":"someone else"}`, "me@example.com (hub says: by someone else)"},
	} {
		s, _ := peerTagServer(t)
		s.cfg.Release.DeployCmd = []string{script}
		if w := do(s, "POST", "/api/release/deploy", hubCall, c.body); w.Code != 202 {
			t.Fatalf("%s = %d %s", c.body, w.Code, w.Body)
		}
		var rec *DeployRecord
		waitUntil(t, "peer deploy done", func() bool {
			w := do(s, "GET", "/api/release/deploy", hubCall, "")
			var st deployStatus
			ok := w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &st) == nil && st.Record != nil && st.Record.State == depOK
			if ok {
				rec = st.Record
			}
			return ok
		})
		if rec.By != c.want {
			t.Errorf("by for %s = %q, want %q", c.body, rec.By, c.want)
		}
	}
}

// The hub asks its release host; the command runs there, the hub's page reads
// the peer's record (also after the hub itself was replaced), and Approve
// release alone ran nothing.
func TestHubDeploysThroughItsReleaseHost(t *testing.T) {
	script, marker := deployScript(t, "sleep 1\necho rolled")
	p := newRelPeer(t)
	p.peer.cfg.Release.DeployCmd = []string{script}
	p.peer.cfg.Release.DeployLabel = "Bump hub"
	s := relHub(t, p, nil)
	it := raiseOnHub(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	if code, body := approve(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("approve = %d %s", code, body)
	}
	waitRel(t, s, it.ID, relTagged)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Approve release ran the deploy command on the peer")
	}
	if st, ok := deployState(s, it.ID); !ok || st.Label != "Bump hub" || st.Record != nil {
		t.Fatalf("status before = %+v %v", st, ok)
	}
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("deploy = %d %s", code, body)
	}
	reqs := p.requests("/api/release/deploy")
	if len(reqs) == 0 || reqs[0].Header.Get(fleet.HeaderHubCall) != "1" || reqs[0].Header.Get(HeaderCSRF) != "" {
		t.Fatalf("deploy request = %+v", reqs)
	}
	// still running on the peer: a second click is refused there
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "deploy_busy") {
		t.Fatalf("second deploy = %d %s", code, body)
	}
	// the verdict is read from the peer
	waitUntil(t, "peer deploy done", func() bool {
		st, ok := deployState(s, it.ID)
		return ok && st.Record != nil && st.Record.State == depOK && strings.Contains(st.Record.Output, "rolled")
	})
	var sawReq, sawResult bool
	for _, e := range s.ledger.AuditLog(it.ID) {
		sawReq = sawReq || (e.Action == "deploy.request" && e.OK && e.Host == "kube")
		sawResult = sawResult || (e.Action == "deploy.result" && e.OK)
	}
	if !sawReq || !sawResult {
		t.Fatalf("audit = %v", actions(s))
	}
	// A peer's red build stops it, whatever the hub thinks.
	p.tool.mu.Lock()
	p.tool.build = release.Build{State: "failure"}
	p.tool.mu.Unlock()
	forgetChecks(s)
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 409 || !strings.Contains(body, "build_not_green") {
		t.Fatalf("deploy on a red build = %d %s", code, body)
	}
}

// -release-simulate's deploy runs nothing and takes the verdict from the
// version, so the demo and the browser tests see both.
func TestSimulatedDeploy(t *testing.T) {
	for _, c := range []struct{ version, state string }{{"v0.5.9", depOK}, {"v0.5.13", depFailed}} {
		s, _ := deployHub(t)
		s.cfg.Release.Simulated = true
		s.cfg.TestMode = true // the shorter simulated delay (0.8 s, not 2 s)
		rec, code, _ := s.startDeployLocal("me", "t1", c.version)
		if code != 202 || rec.State != depRunning {
			t.Fatalf("%s: start = %d %+v", c.version, code, rec)
		}
		if st := s.deployStatusLocal(); !st.Enabled || st.Label != "Bump hub" && st.Label == "" {
			t.Fatalf("status = %+v", st)
		}
		waitUntil(t, c.version+" settles", func() bool {
			st := s.deployStatusLocal()
			return st.Record != nil && st.Record.State == c.state
		})
	}
}

func TestCleanDeployOutput(t *testing.T) {
	in := "\x1b[31mred\x1b[0m line\nprogress 50%\rprogress 100%\n" +
		"token=abc123secretvalue\n" +
		"GITHUB_TOKEN: ghp_" + strings.Repeat("a", 30) + "\n" +
		"Authorization: Bearer abcdefghijkl.mnop\n" +
		"git clone https://bob:hunter2@example.com/x.git\n" +
		`{"password": "p w"}` + "\n" +
		"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\nBBBB\n-----END OPENSSH PRIVATE KEY-----\n" +
		"bump: FAILED phase=reconcile reason=flux: command not found\x07\n"
	out := cleanDeployOutput(in)
	for _, bad := range []string{"\x1b", "\x07", "abc123secretvalue", "ghp_", "abcdefghijkl", "hunter2", "p w", "AAAA", "progress 50"} {
		if strings.Contains(out, bad) {
			t.Errorf("output still has %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"red line", "progress 100%", "FAILED phase=reconcile reason=flux: command not found", "[redacted]", "https://bob:[redacted]@example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
	// capped to its tail, on a line boundary
	long := strings.Repeat("0123456789abcdef\n", 1000) + "last line"
	c := cleanDeployOutput(long)
	if len(c) > deployOutMax || !strings.HasSuffix(c, "last line") || !strings.HasPrefix(c, "0123456789abcdef") {
		t.Errorf("cap: len %d, %q...", len(c), c[:20])
	}
}

func TestDeployHeadline(t *testing.T) {
	for _, c := range []struct {
		rec  DeployRecord
		want string
	}{
		{DeployRecord{State: depFailed, Error: "exit status 1", Output: "a\nbump: FAILED phase=reconcile reason=x\ntrailing"}, "bump: FAILED phase=reconcile reason=x"},
		{DeployRecord{State: depFailed, Error: "exit status 1", Output: "a\nb"}, "exit status 1"},
		{DeployRecord{State: depFailed, Error: "exit status 1"}, "exit status 1"},
		{DeployRecord{State: depOK, Output: "bump: edit\nbump: ok version=v1.2.3 total=2s\nbye"}, "bump: ok version=v1.2.3 total=2s"},
		{DeployRecord{State: depOK, Output: "one\ntwo"}, "two"},
		{DeployRecord{State: depRunning, Output: "bump: edit\nbump: push +1s\n\n"}, "bump: push +1s"},
		{DeployRecord{State: depDryRun, Output: "dry run: would run x"}, ""},
	} {
		if got := deployHeadline(c.rec); got != c.want {
			t.Errorf("%+v: headline %q, want %q", c.rec, got, c.want)
		}
	}
}

// The API: a failed run carries the cleaned output and a headline; the
// secrets the command printed are not served.
func TestDeployStatusCarriesHeadlineAndRedactedOutput(t *testing.T) {
	script, _ := deployScript(t, "printf '\\033[1mstep\\033[0m\\n'\necho 'token=sekrit-value-123' >&2\necho 'bump: FAILED phase=reconcile reason=flux: command not found'\nexit 1")
	s, _ := deployHub(t, script)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approve(s, it.ID, tok, "v0.5.9", relSHA)
	waitRel(t, s, it.ID, relTagged)
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("deploy = %d %s", code, body)
	}
	waitUntil(t, "deploy failed", func() bool {
		st, ok := deployState(s, it.ID)
		return ok && st.Record != nil && st.Record.State == depFailed
	})
	w := do(s, "GET", "/api/release/"+it.ID, userHdr, "")
	var got struct {
		Deploy struct {
			Record map[string]any `json:"record"`
		} `json:"deploy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	r := got.Deploy.Record
	if r["headline"] != "bump: FAILED phase=reconcile reason=flux: command not found" || r["error"] != "exit status 1" {
		t.Fatalf("record = %v", r)
	}
	out, _ := r["output"].(string)
	if strings.Contains(out, "sekrit") || strings.Contains(out, "\x1b") || !strings.Contains(out, "step") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(w.Body.String(), "sekrit") {
		t.Fatal("the secret is in the response")
	}
}

// A peer on an older version sends no headline, and whatever it sends is
// cleaned by the hub.
func TestHubCleansAPeersDeployRecord(t *testing.T) {
	rec := DeployRecord{Version: "v0.5.9", State: depFailed, Error: "exit status 1", Started: time.Now(),
		Headline: "<b>forged</b>", Output: "\x1b[31mx\x1b[0m\npassword=hunter22\nbump: FAILED phase=a reason=b"}
	p := newRelPeer(t)
	p.peer.cfg.Release.DeployCmd = []string{"/bin/true"}
	p.peer.writeDeployRecord(rec)
	s := relHub(t, p, nil)
	st, err := s.deployStatusFor(t.Context())
	if err != nil || st.Record == nil {
		t.Fatalf("status = %+v %v", st, err)
	}
	if st.Record.Headline != "bump: FAILED phase=a reason=b" || strings.Contains(st.Record.Output, "hunter22") || strings.Contains(st.Record.Output, "\x1b") {
		t.Fatalf("record = %+v", st.Record)
	}
}
