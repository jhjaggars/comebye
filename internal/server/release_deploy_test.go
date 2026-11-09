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
