package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
)

// runningHub is stoppedHub with demo1 running in the cloud, and the tool's
// stop flipping it to stopped.
func runningHub(t *testing.T) (*Server, *provPeer, *fakeWorkerDir, map[string]string) {
	t.Helper()
	s, prov, wk, hdr := stoppedHub(t)
	wk.set(t, "status.json", statusDoc("demo1", "RUNNING", ""))
	wk.set(t, "stop.flip", statusDoc("demo1", "STOPPED", ""))
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if len(h.Hosts) != 1 || h.Hosts[0].State != "running" {
		t.Fatalf("hosts: %+v", h.Hosts)
	}
	return s, prov, wk, hdr
}

func waitHubJob(t *testing.T, s *Server, id, state string) hubJobView {
	t.Helper()
	var j hubJobView
	waitUntil(t, "job "+state, func() bool {
		j = hubJobView{}
		getJSON(t, s, "/api/provision/jobs/"+id, userHdr, &j)
		return j.State == state
	})
	return j
}

func TestStopRunsTheToolOnTheProvisionerAndTheHostReadsStopped(t *testing.T) {
	s, prov, wk, hdr := runningHub(t)
	wk.set(t, "stop.out", "stopping demo1\ndemo1 stopped\n")
	rec := do(s, "POST", "/api/provision/hosts/demo1/stop", hdr, `{}`)
	var j hubJobView
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != http.StatusAccepted || j.Op != opStop || j.Via != "prov" || j.ID == "" {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body)
	}
	j = waitHubJob(t, s, j.ID, jobOK)
	if len(j.Items) < 2 || !strings.Contains(j.Items[len(j.Items)-1].Msg, "demo1 stopped") {
		t.Errorf("job items: %+v", j.Items)
	}
	if j.CanRetry || j.CanCleanup {
		t.Errorf("a stop job offers retry or clean-up: %+v", j)
	}
	if wk.runs("stop demo1") != 1 || prov.asked("/api/provisioner/host/stop") != 1 {
		t.Errorf("argv %v, asked %d", wk.argv(), prov.asked("/api/provisioner/host/stop"))
	}
	// the provisioner's book says stopped at once, and so does the next look
	tick(s)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if len(h.Hosts) != 1 || h.Hosts[0].State != "stopped" {
		t.Errorf("hosts after: %+v", h.Hosts)
	}
	// a second tap is refused: it is not running
	if rec := do(s, "POST", "/api/provision/hosts/demo1/stop", hdr, `{}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_running") {
		t.Errorf("stop of a stopped host = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("stop demo1") != 1 {
		t.Errorf("a refused stop ran the tool: %v", wk.argv())
	}
}

func TestStopIsRefusedWithoutTheTokenOrForAnUnknownHost(t *testing.T) {
	s, _, wk, hdr := runningHub(t)
	if rec := do(s, "POST", "/api/provision/hosts/demo1/stop", userHdr, `{}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "csrf") {
		t.Errorf("no token = %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/api/provision/hosts/ghost/stop", hdr, `{}`); rec.Code != 404 {
		t.Errorf("unknown = %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/api/provision/hosts/demo1/stop", hdr, `{"shell":"x"}`); rec.Code != 400 {
		t.Errorf("unknown field = %d", rec.Code)
	}
	if rec := do(s, "POST", "/api/provision/hosts/demo1/stop", hdr, `{"via":"nobody"}`); rec.Code != 404 || !strings.Contains(rec.Body.String(), "not_provisioned") {
		t.Errorf("via a node that does not list it = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("stop demo1") != 0 {
		t.Fatalf("a refused stop ran the tool: %v", wk.argv())
	}
}

func TestStopNeedsTheUserOrTheCoordinator(t *testing.T) {
	s, _, wk, _ := runningHub(t)
	if rec := do(s, "POST", "/api/provision/hosts/demo1/stop", agentHdr("w9:p1"), `{}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "agent_forbidden") {
		t.Errorf("agent = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("stop demo1") != 0 {
		t.Fatalf("an agent's stop ran the tool")
	}
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	rec := do(s, "POST", "/api/provision/hosts/demo1/stop", agentHdr("w1:p1"), `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("coordinator = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "stop ran", func() bool { return wk.runs("stop demo1") == 1 })
}

func TestAFailedStopSaysWhy(t *testing.T) {
	s, _, wk, hdr := runningHub(t)
	wk.set(t, "stop.fail", "1")
	rec := do(s, "POST", "/api/provision/hosts/demo1/stop", hdr, `{}`)
	var j hubJobView
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body)
	}
	j = waitHubJob(t, s, j.ID, jobFailed)
	if !strings.Contains(j.Msg, "would not stop") {
		t.Errorf("msg = %q", j.Msg)
	}
}

func TestPeerStopRouteRefusesWhatTheBookDoesNotAllow(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "status.json", kvStatusDoc("STOPPED"))
	p := herdrlessProvServer(t, w)
	if ok := p.observeHosts(context.Background()); len(ok) != 1 { // kv1 was made with the worker CLI: observing adopts it
		t.Fatal("not read")
	}
	if rec := do(p, "POST", "/api/provisioner/host/stop", hubCall, `{"name":"kv1"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_running") {
		t.Errorf("stopped = %d %s", rec.Code, rec.Body)
	}
	if rec := do(p, "POST", "/api/provisioner/host/stop", hubCall, `{"name":"ghost"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown = %d %s", rec.Code, rec.Body)
	}
	if rec := do(p, "POST", "/api/provisioner/host/stop", userHdr, `{"name":"kv1"}`); rec.Code == http.StatusAccepted {
		t.Errorf("a call that is not the hub's = %d", rec.Code)
	}
	if w.runs("stop kv1") != 0 {
		t.Fatalf("argv: %v", w.argv())
	}
}

// sharedVMHub: a workstation's "prov" and the in-cluster "kube" (a -hub-only node)
// both drive kv1, the way a workstation's worker config and the in-cluster
// provisioner do.
func sharedVMHub(t *testing.T, state string) (s *Server, deskW, kvW *fakeWorkerDir, hdr map[string]string) {
	t.Helper()
	s, deskW, kvW, hdr, _ = twoProvisionerHub(t)
	deskW.set(t, "status.json", kvStatusDoc(state))
	deskW.set(t, "start.flip", kvStatusDoc("RUNNING"))
	deskW.set(t, "stop.flip", kvStatusDoc("STOPPED"))
	kvW.set(t, "status.json", kvStatusDoc(state))
	kvW.set(t, "stop.flip", kvStatusDoc("STOPPED"))
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	vias := map[string]bool{}
	for _, x := range h.Hosts {
		if x.Name == "kv1" {
			vias[x.Via] = true
		}
	}
	if !vias["prov"] || !vias["kube"] {
		t.Fatalf("kv1 is not listed by both: %+v", h.Hosts)
	}
	return
}

// Both up: the dedicated provisioner is the one, with no via given.
func TestAVMTwoProvisionersDriveStartsThroughTheDedicatedOne(t *testing.T) {
	s, deskW, kvW, hdr := sharedVMHub(t, "STOPPED")
	rec := do(s, "POST", "/api/provision/hosts/kv1/start", hdr, `{}`)
	var a wakeResp
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != http.StatusAccepted || a.Wake.Via != "kube" {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	waitWake(t, s, "kv1", wakeUp)
	if kvW.runs("start kv1") != 1 || deskW.runs("start kv1") != 0 {
		t.Errorf("kube %v; desk %v", kvW.argv(), deskW.argv())
	}
}

// The same for a stop; and a via still picks the other one.
func TestAVMTwoProvisionersDriveStopsThroughTheDedicatedOneUnlessToldOtherwise(t *testing.T) {
	s, deskW, kvW, hdr := sharedVMHub(t, "RUNNING")
	rec := do(s, "POST", "/api/provision/hosts/kv1/stop", hdr, `{}`)
	var j hubJobView
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != http.StatusAccepted || j.Via != "kube" {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body)
	}
	waitHubJob(t, s, j.ID, jobOK)
	if kvW.runs("stop kv1") != 1 || deskW.runs("stop kv1") != 0 {
		t.Errorf("kube %v; desk %v", kvW.argv(), deskW.argv())
	}
	// the workstation, named
	kvW.set(t, "status.json", kvStatusDoc("RUNNING"))
	deskW.set(t, "status.json", kvStatusDoc("RUNNING"))
	tick(s)
	rec = do(s, "POST", "/api/provision/hosts/kv1/stop", hdr, `{"via":"prov"}`)
	j = hubJobView{}
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != http.StatusAccepted || j.Via != "prov" {
		t.Fatalf("stop via prov = %d %s", rec.Code, rec.Body)
	}
	waitHubJob(t, s, j.ID, jobOK)
	if deskW.runs("stop kv1") != 1 {
		t.Errorf("desk %v", deskW.argv())
	}
}

// One of them down: the other one, whichever it is.
func TestAVMTwoProvisionersDriveStartsThroughTheOneThatIsUp(t *testing.T) {
	s, deskW, kvW, hdr := sharedVMHub(t, "STOPPED")
	down := map[string]bool{"kube": true}
	s.peerStatusHook = func(n string) string {
		if down[n] {
			return fleet.StatusDown
		}
		for _, h := range s.fleet.Hosts() {
			if h.Name == n {
				return h.Status
			}
		}
		return ""
	}
	rec := do(s, "POST", "/api/provision/hosts/kv1/start", hdr, `{}`)
	var a wakeResp
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != http.StatusAccepted || a.Wake.Via != "prov" {
		t.Fatalf("start with kube down = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "desk ran start", func() bool { return deskW.runs("start kv1") == 1 })
	if kvW.runs("start kv1") != 0 {
		t.Errorf("kube was asked: %v", kvW.argv())
	}
}

// Two that are up and neither dedicated: still the user's choice.
func TestTwoOrdinaryProvisionersOfOneVMAreStillAmbiguous(t *testing.T) {
	s, _, _, hdr := sharedVMHub(t, "STOPPED")
	s.prov.mu.Lock()
	if n := s.prov.nodes["kube"]; n != nil && n.Preflight != nil {
		cp := *n.Preflight
		cp.HubOnly = false
		n.Preflight = &cp
	}
	s.prov.mu.Unlock()
	if rec := do(s, "POST", "/api/provision/hosts/kv1/start", hdr, `{}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ambiguous") {
		t.Errorf("start = %d %s", rec.Code, rec.Body)
	}
}

func TestPreflightSaysAHubOnlyNodeIsDedicated(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "status.json", kvStatusDoc("STOPPED"))
	rec := do(herdrlessProvServer(t, w), "GET", "/api/provisioner/preflight", hubCall, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hub_only":true`) {
		t.Errorf("hub-only preflight = %d %s", rec.Code, rec.Body)
	}
	rec = do(provPeerServer(t, w), "GET", "/api/provisioner/preflight", hubCall, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `hub_only`) {
		t.Errorf("ordinary preflight = %d %s", rec.Code, rec.Body)
	}
}
