package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
)

func statusDoc(name, state string, extra string) string {
	return `{"v":1,"provider":"aws","workers":[{"name":"` + name + `","state":"` + state + `","type":"t3.large","mode":"spot","managed":true,"launched_at":"` +
		time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339) + `","last_stop_at":"` + time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339) + `","caps":{"budget_cap_usd":10,"destroy_after_hours":72,"launch_price_usd_hour":0.03}` + extra + `}]}`
}

// stoppedHub is realHub with demo1 stopped in the cloud, and demo1 itself a
// peer of the hub (another in-process node: the worker the hub talks to).
func stoppedHub(t *testing.T) (*Server, *provPeer, *fakeWorkerDir, map[string]string) {
	t.Helper()
	s, prov, wk, hdr, _ := realHub(t)
	wk.set(t, "status.json", statusDoc("demo1", "STOPPED", ""))
	wk.set(t, "start.flip", statusDoc("demo1", "RUNNING", ""))
	worker := newProvPeer(t, newFakeWorker(t))
	b, _ := json.Marshal(map[string]any{"name": "demo1", "urls": []string{worker.srv.URL}})
	if w := do(s, "POST", "/api/peers", userHdr, string(b)); w.Code != 201 {
		t.Fatalf("add peer = %d %s", w.Code, w.Body)
	}
	// the hub's stream to this peer must end before its server closes
	t.Cleanup(func() { do(s, "DELETE", "/api/peers/demo1", userHdr, "") })
	waitUntil(t, "demo1 up", func() bool { return s.peerStatus("demo1") == fleet.StatusOK })
	s.wake.pollGap, s.wake.retryGap = 20*time.Millisecond, 20*time.Millisecond
	return s, prov, wk, hdr
}

type wakeResp struct {
	Wake  wakeView            `json:"wake"`
	Wakes map[string]wakeView `json:"wakes"`
	Code  string              `json:"code"`
	Msg   string              `json:"message"`
}

func waitWake(t *testing.T, s *Server, host, state string) wakeView {
	t.Helper()
	var v wakeView
	waitUntil(t, "wake "+state, func() bool {
		var a wakeResp
		getJSON(t, s, "/api/provision/wake", userHdr, &a)
		v = a.Wakes[host]
		return v.State == state
	})
	return v
}

func TestStartRunsTheToolOnTheProvisionerAndTheWakeGoesUp(t *testing.T) {
	s, prov, wk, hdr := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if len(h.Hosts) != 1 || h.Hosts[0].State != "stopped" {
		t.Fatalf("hosts: %+v", h.Hosts)
	}
	wk.set(t, "start.out", "starting demo1\ndemo1 is up, comebye v1\n")
	rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`)
	var a wakeResp
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != 202 || a.Wake.Host != "demo1" || a.Wake.State != wakeStarting || a.Wake.Via != "prov" {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	v := waitWake(t, s, "demo1", wakeUp)
	if v.By == "" || v.Job == "" {
		t.Errorf("wake: %+v", v)
	}
	argv := strings.Join(wk.argvLines(), "\n")
	if !strings.Contains(argv, "start demo1") || strings.Contains(argv, "start demo1 --events") {
		t.Errorf("argv: %s", argv)
	}
	if prov.asked("/api/provisioner/host/start") != 1 {
		t.Errorf("provisioner asked %d times", prov.asked("/api/provisioner/host/start"))
	}
	// the tool's text is the job's journal
	var j hubJobResp
	getJSON(t, s, "/api/provision/jobs/"+v.Job, userHdr, &j)
	if j.State != jobOK || len(j.Items) < 2 || !strings.Contains(j.Items[len(j.Items)-1].Msg, "is up") {
		t.Errorf("job: %+v", j)
	}
	// the hosts answer carries the wake book, and the host now reads running
	h = hostsResp{}
	var raw struct {
		hostsResp
		Wakes map[string]wakeView `json:"wakes"`
	}
	tick(s)
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &raw)
	if raw.Wakes["demo1"].State != wakeUp || len(raw.Hosts) != 1 || raw.Hosts[0].State != "running" {
		t.Errorf("hosts after: %+v wakes %+v", raw.Hosts, raw.Wakes)
	}
	// a second tap while it is up asks the cloud again and is refused
	tick(s)
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "not_stopped") {
		t.Errorf("start of a running host = %d %s", rec.Code, rec.Body)
	}
}

func TestStartIsRefusedForWhatIsNotAStoppedProvisionedWorker(t *testing.T) {
	s, _, wk, hdr := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	// no CSRF token
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", userHdr, `{}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "csrf") {
		t.Errorf("no token = %d %s", rec.Code, rec.Body)
	}
	// not a peer of the hub
	if rec := do(s, "POST", "/api/provision/hosts/ghost/start", hdr, `{}`); rec.Code != 404 {
		t.Errorf("unknown = %d %s", rec.Code, rec.Body)
	}
	// a peer no provisioner lists
	other := newProvPeer(t, newFakeWorker(t))
	b, _ := json.Marshal(map[string]any{"name": "stray", "urls": []string{other.srv.URL}})
	do(s, "POST", "/api/peers", userHdr, string(b))
	t.Cleanup(func() { do(s, "DELETE", "/api/peers/stray", userHdr, "") })
	if rec := do(s, "POST", "/api/provision/hosts/stray/start", hdr, `{}`); rec.Code != 404 || !strings.Contains(rec.Body.String(), "not_provisioned") {
		t.Errorf("not provisioned = %d %s", rec.Code, rec.Body)
	}
	// running in the cloud: the network is the problem, so no start
	wk.set(t, "status.json", statusDoc("demo1", "RUNNING", ""))
	tick(s)
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "not_stopped") {
		t.Errorf("running = %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{"shell":"x"}`); rec.Code != 400 {
		t.Errorf("unknown field = %d", rec.Code)
	}
	if wk.runs("start demo1") != 0 {
		t.Fatalf("a refused start ran the tool: %v", wk.argv())
	}
}

func TestStartNeedsTheUserOrTheCoordinator(t *testing.T) {
	s, _, wk, _ := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	// an agent that is not the coordinator
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", agentHdr("w9:p1"), `{}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "agent_forbidden") {
		t.Errorf("agent = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("start demo1") != 0 {
		t.Fatalf("an agent's start ran the tool")
	}
	// the coordinator (no CSRF token: it is not a page)
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	rec := do(s, "POST", "/api/provision/hosts/demo1/start", agentHdr("w1:p1"), `{}`)
	if rec.Code != 202 {
		t.Fatalf("coordinator = %d %s", rec.Code, rec.Body)
	}
	v := waitWake(t, s, "demo1", wakeUp)
	if !strings.Contains(v.By, "w1:p1") {
		t.Errorf("by = %q", v.By)
	}
}

func TestStartOfACapStoppedHostAsksForConfirmation(t *testing.T) {
	s, prov, wk, hdr := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	prov.peer.phosts.MarkStopped("demo1", true) // the watchdog stopped it at its cap
	tick(s)
	rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`)
	var a map[string]any
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != 409 || a["code"] != "needs_confirm" || !strings.Contains(rec.Body.String(), "budget cap") || a["cap_usd"].(float64) != 10 {
		t.Fatalf("no confirm = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("start demo1") != 0 {
		t.Fatal("started without the confirmation")
	}
	rec = do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{"confirm_cap":true}`)
	if rec.Code != 202 {
		t.Fatalf("confirmed = %d %s", rec.Code, rec.Body)
	}
	waitWake(t, s, "demo1", wakeUp)
	if wk.runs("start demo1") != 1 {
		t.Errorf("runs: %v", wk.argv())
	}
}

func TestAFailedStartShowsWhyAndStartAgainWorks(t *testing.T) {
	s, _, wk, hdr := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	wk.set(t, "start.fail", "1")
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`); rec.Code != 202 {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	v := waitWake(t, s, "demo1", wakeFailed)
	if !strings.Contains(v.Reason, "would not start") || v.Fails != 1 {
		t.Errorf("failed wake: %+v", v)
	}
	os.Remove(filepath.Join(wk.dir, "start.fail"))
	tick(s)
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`); rec.Code != 202 {
		t.Fatalf("start again = %d %s", rec.Code, rec.Body)
	}
	waitWake(t, s, "demo1", wakeUp)
}

func TestAStartWaitsWhileTheProvisionerIsBusyAndSaysSo(t *testing.T) {
	s, prov, _, hdr := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	release, ok := prov.peer.pjobs.hold()
	if !ok {
		t.Fatal("could not hold the slot")
	}
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`); rec.Code != 202 {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "the wait is shown", func() bool {
		v, _ := s.wake.get("demo1")
		return v.State == wakeStarting && strings.Contains(v.Step, "waiting for the provisioner")
	})
	// a second tap joins the one running
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`); rec.Code != 200 {
		t.Errorf("join = %d %s", rec.Code, rec.Body)
	}
	release()
	waitWake(t, s, "demo1", wakeUp)
	if n := prov.asked("/api/provisioner/host/start"); n < 2 {
		t.Errorf("asked %d times, expected a retry", n)
	}
}

func TestAStartThatNeverJoinsTheHubFails(t *testing.T) {
	s, _, wk, hdr := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	// a peer whose address never answers stays down
	b, _ := json.Marshal(map[string]any{"name": "demo1", "urls": []string{"http://127.0.0.1:1"}})
	do(s, "DELETE", "/api/peers/demo1", userHdr, "")
	do(s, "POST", "/api/peers", userHdr, string(b))
	waitUntil(t, "demo1 not ok", func() bool { return s.peerStatus("demo1") != fleet.StatusOK })
	s.wake.joinWait = 300 * time.Millisecond
	if rec := do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`); rec.Code != 202 {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	v := waitWake(t, s, "demo1", wakeFailed)
	if !strings.Contains(v.Reason, "did not join") {
		t.Errorf("reason: %q", v.Reason)
	}
	if wk.runs("start demo1") != 1 {
		t.Errorf("runs: %v", wk.argv())
	}
}

func TestProvisionerAnswerFailureFailsTheWakeWithItsReason(t *testing.T) {
	s, prov, _, hdr := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	prov.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/provisioner/host/start" {
			writeJSONStatus(w, 409, map[string]string{"code": "bad_state", "message": "this host is destroyed or being destroyed"})
			return true
		}
		return false
	})
	do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{}`)
	v := waitWake(t, s, "demo1", wakeFailed)
	if !strings.Contains(v.Reason, "destroyed or being destroyed") {
		t.Errorf("reason: %q", v.Reason)
	}
}

func TestWakeBookSurvivesARestartAndFollowsItsJob(t *testing.T) {
	dir := t.TempDir()
	b := openWakeBook(dir)
	b.begin("demo1", "prov", "me", "tap")
	b.update("demo1", func(e *wakeEntry) { e.Job = "hj_0123456789abcdef"; e.Step = "starting demo1" })
	b.begin("demo2", "prov", "me", "tap") // never reached the provisioner
	b2 := openWakeBook(dir)
	if v, ok := b2.get("demo1"); !ok || v.State != wakeStarting || v.Job != "hj_0123456789abcdef" || v.Step != "starting demo1" {
		t.Errorf("reloaded: %+v %v", v, ok)
	}
	if _, joined := b2.begin("demo1", "prov", "x", "tap"); !joined {
		t.Error("a second begin did not join the running wake")
	}
	// resuming: the one without a job fails with the reason, the one with a job is followed
	s := &Server{wake: b2, hjobs: openHubJobs("")}
	s.wakeResume() // demo1 has no hub job in this bare server: it ends failed, not stuck
	waitUntil(t, "demo1 followed its job (which this server never had) and ended", func() bool {
		v, _ := b2.get("demo1")
		return v.State == wakeFailed && strings.Contains(v.Reason, "lost track")
	})
	waitUntil(t, "demo2 failed", func() bool { v, _ := b2.get("demo2"); return v.State == wakeFailed })
	if v, _ := b2.get("demo2"); !strings.Contains(v.Reason, "hub restarted") {
		t.Errorf("demo2: %+v", v)
	}
}

func TestWakeLimitsAreReadyForAutomaticWakes(t *testing.T) {
	b := openWakeBook("")
	now := time.Now()
	b.now = func() time.Time { return now }
	if err := b.wakeAllow("kv1"); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	for i := 0; i < wakeHostPerHour; i++ {
		b.noteAuto("kv1")
	}
	if err := b.wakeAllow("kv1"); err == nil || !strings.Contains(err.Error(), "tap Start to start it anyway") {
		t.Errorf("per host: %v", err)
	}
	if err := b.wakeAllow("kv2"); err != nil {
		t.Errorf("another host is not held back: %v", err)
	}
	now = now.Add(61 * time.Minute)
	if err := b.wakeAllow("kv1"); err != nil {
		t.Errorf("after the hour: %v", err)
	}
	for i := 0; i < wakeFleetPerHour; i++ {
		b.noteAuto([]string{"a1", "a2", "a3", "a4", "a5"}[i%5])
	}
	if err := b.wakeAllow("kv9"); err == nil || !strings.Contains(err.Error(), "workers have woken this hour") {
		t.Errorf("fleet: %v", err)
	}
	// backoff after a failed wake: 2 min, then 10
	c := openWakeBook("")
	c.now = func() time.Time { return now }
	c.begin("kv1", "prov", "me", "rollout")
	c.fail("kv1", "no capacity")
	if err := c.wakeAllow("kv1"); err == nil || !strings.Contains(err.Error(), "tries again") {
		t.Errorf("backoff: %v", err)
	}
	now = now.Add(3 * time.Minute)
	if err := c.wakeAllow("kv1"); err != nil {
		t.Errorf("after the backoff: %v", err)
	}
}

func TestPeerStartRouteRefusesWhatTheBookDoesNotAllow(t *testing.T) {
	w := newFakeWorker(t)
	s := provPeerServer(t, w)
	s.cfg.Provision.Tool.Real = true
	w.set(t, "status.json", statusDoc("kv1", "STOPPED", ""))
	// kv1 was made with the worker CLI: observing adopts it
	if err := s.observeHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h, ok := s.phosts.Get("kv1"); !ok || !h.Adopted || h.State != "STOPPED" {
		t.Fatalf("adoption: %+v %v", h, ok)
	}
	if _, err := s.pjobs.destroy(s.cfg.Provision.Tool, "kv1", destroyReq{}); err == nil || !strings.Contains(err.Error(), "worker tool") {
		t.Errorf("destroying an adopted host: %v", err)
	}
}

// ---- the rollout's wake (slice 2): the hub side of rolloutOps ----

func rolloutJobFor() Rollout { return Rollout{ID: "r1", Todo: "t1", By: "jesse"} }

func TestRolloutWakeStartsAStoppedProvisionedWorkerOnce(t *testing.T) {
	s, prov, wk, _ := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	wk.set(t, "start.out", "starting demo1\ndemo1 is up, comebye v1\n")
	ops := serverRolloutOps{s}
	v := ops.Wake(context.Background(), rolloutJobFor(), "demo1")
	if v.Kind != wakeStarting {
		t.Fatalf("wake = %+v", v)
	}
	// a second ask joins the wake that is going
	if v2 := ops.Wake(context.Background(), rolloutJobFor(), "demo1"); v2.Kind != wakeStarting {
		t.Fatalf("second wake = %+v", v2)
	}
	waitWake(t, s, "demo1", wakeUp)
	if st := ops.WakeStatus(context.Background(), "demo1"); st.Kind != wakeUp {
		t.Errorf("status = %+v", st)
	}
	if wk.runs("start demo1") != 1 || prov.asked("/api/provisioner/host/start") != 1 {
		t.Errorf("the tool ran %d times: %v", wk.runs("start demo1"), wk.argv())
	}
	if e, _ := s.wake.get("demo1"); e.Why != "rollout" || e.By != "jesse" {
		t.Errorf("entry %+v", e)
	}
	if len(s.wake.auto) != 1 {
		t.Errorf("the automatic wake was not counted: %v", s.wake.auto)
	}
}

func TestRolloutWakeRefusesWhatItMustNotStart(t *testing.T) {
	s, prov, wk, _ := stoppedHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	ops := serverRolloutOps{s}
	ctx := context.Background()
	// not a provisioned worker: none, the old grace
	if v := ops.Wake(ctx, rolloutJobFor(), "stray"); v.Kind != wakeNone {
		t.Errorf("unknown host = %+v", v)
	}
	// the rate limit
	for i := 0; i < wakeHostPerHour; i++ {
		s.wake.noteAuto("demo1")
	}
	if v := ops.Wake(ctx, rolloutJobFor(), "demo1"); v.Kind != wakeRefused || !strings.Contains(v.Reason, "tap Start") {
		t.Errorf("rate limited = %+v", v)
	}
	// a budget cap is never woken automatically, rate limit or not
	prov.peer.phosts.MarkStopped("demo1", true)
	tick(s)
	if v := ops.Wake(ctx, rolloutJobFor(), "demo1"); v.Kind != wakeRefused || !strings.Contains(v.Reason, "budget cap") {
		t.Errorf("cap-stopped = %+v", v)
	}

	// running in the cloud: a network fault, not ours
	wk.set(t, "status.json", statusDoc("demo1", "RUNNING", ""))
	tick(s)
	if v := ops.Wake(ctx, rolloutJobFor(), "demo1"); v.Kind != wakeNone {
		t.Errorf("running = %+v", v)
	}
	if wk.runs("start demo1") != 0 {
		t.Fatalf("a refused wake ran the tool: %v", wk.argv())
	}
}
