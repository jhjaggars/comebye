package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/provision"
)

// realHub is a hub with a provisioner whose real path is on (the fake worker
// stands in for the tool), and one host created through the app.
func realHub(t *testing.T) (*Server, *provPeer, *fakeWorkerDir, map[string]string, string) {
	t.Helper()
	s, prov, wk, hdr := hubSetup(t)
	prov.peer.cfg.Provision.Tool.Real = true
	launched := time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	ev := ""
	for i, st := range provision.Stages {
		ev += `{"v":1,"seq":` + string(rune('1'+i)) + `,"stage":"` + st + `","step":"` + string(rune('1'+i)) + `/9","state":"ok"}` + "\n"
	}
	wk.set(t, "create.events", ev)
	wk.set(t, "create.stderr", "")
	wk.set(t, "status.json", `{"v":1,"provider":"aws","workers":[{"name":"demo1","state":"RUNNING","type":"t3.large","mode":"spot","managed":true,"launched_at":"`+launched+`","last_stop_at":null,"caps":{"budget_cap_usd":10,"destroy_after_hours":72,"launch_price_usd_hour":0.03}}]}`)
	body := `{"spec":` + okSpec + `,"region":"us-east-1","key":"hub-key-12345","ack":true}`
	rec := do(s, "POST", "/api/provision", hdr, body)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 202 || v.Dry {
		t.Fatalf("real create = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, v.ID, jobOK)
	return s, prov, wk, hdr, v.ID
}

// tick moves the hub's clock on, so that a refresh is not held back as a repeat.
func tick(s *Server) {
	s.prov.mu.Lock()
	defer s.prov.mu.Unlock()
	old := s.prov.now
	s.prov.now = func() time.Time { return old().Add(3 * time.Second) }
}

type hostsResp struct {
	Hosts []struct {
		provision.HostView
		Via  string `json:"via"`
		AgeS int    `json:"age_s"`
	} `json:"hosts"`
	Nodes []hostsNodeView `json:"nodes"`
	Total provision.Total `json:"total"`
	CSRF  string          `json:"csrf"`
}

func TestHubShowsAHostAndItsSpend(t *testing.T) {
	s, _, _, _, _ := realHub(t)
	var h hostsResp
	if getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h) != 200 || len(h.Hosts) != 1 {
		t.Fatalf("hosts: %+v", h)
	}
	x := h.Hosts[0]
	// 90 min at $0.03 + the recorded IPv4 and disks, from the cloud's launch time
	if x.Name != "demo1" || x.Via != "prov" || x.State != "running" || x.Mode != "spot" || !x.FromTag || x.BudgetCapUSD != 10 {
		t.Errorf("host: %+v", x)
	}
	if x.Spend.USD <= 0.05 || x.Spend.USD > 0.2 || x.Spend.Hours < 1.4 || x.Spend.Hours > 1.6 || x.Spend.Final || x.Spend.USDHour <= 0.03 {
		t.Errorf("spend: %+v", x.Spend)
	}
	if h.Total.USD != x.Spend.USD || h.Total.Live != 1 || len(h.Nodes) != 1 || h.Nodes[0].Name != "prov" {
		t.Errorf("total: %+v nodes %+v", h.Total, h.Nodes)
	}
}

func TestHubKeepsTheLastHostsWhenTheProvisionerIsGone(t *testing.T) {
	s, prov, _, _, _ := realHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	tick(s)
	prov.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/api/provisioner/") {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	h = hostsResp{}
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if len(h.Hosts) != 1 || h.Nodes[0].Error == "" || h.Hosts[0].Spend.USD <= 0 || h.Hosts[0].AgeS < 3 {
		t.Errorf("offline: %+v", h)
	}
}

func destroyBody2(name, confirm string, extra string) string {
	return `{"confirm":"` + confirm + `"` + extra + `}`
}

func TestHubDestroyNeedsTheTypedNameAndTheCSRFToken(t *testing.T) {
	s, _, wk, hdr, _ := realHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if rec := do(s, "POST", "/api/provision/hosts/demo1/destroy", userHdr, destroyBody2("demo1", "demo1", "")); rec.Code != 403 || !strings.Contains(rec.Body.String(), "csrf") {
		t.Errorf("no token = %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{"wrong name": destroyBody2("demo1", "demo2", ""), "empty": destroyBody2("demo1", "", ""), "case": destroyBody2("demo1", "Demo1", "")} {
		rec := do(s, "POST", "/api/provision/hosts/demo1/destroy", hdr, body)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "confirm_mismatch") {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := do(s, "POST", "/api/provision/hosts/ghost/destroy", hdr, destroyBody2("ghost", "ghost", "")); rec.Code != 404 {
		t.Errorf("unknown host = %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/api/provision/hosts/demo1/destroy", hdr, `{"confirm":"demo1","shell":"x"}`); rec.Code != 400 {
		t.Errorf("unknown field = %d", rec.Code)
	}
	if wk.runs("destroy") != 0 {
		t.Fatalf("a refused destroy ran the tool: %v", wk.argv())
	}
}

func TestHubDestroyRunsOnTheNodeThatBuiltItAndFreezesTheBill(t *testing.T) {
	s, _, wk, hdr, _ := realHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	wk.set(t, "destroy.events", `{"v":1,"seq":1,"stage":"destroy","step":"1/1","state":"ok","msg":"saved the sessions"}`+"\n")
	rec := do(s, "POST", "/api/provision/hosts/demo1/destroy", hdr, destroyBody2("demo1", "demo1", ""))
	var j struct {
		hubJobResp
		Op string `json:"op"`
	}
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != 202 || j.Op != "destroy" || !hubJobIDRe.MatchString(j.ID) {
		t.Fatalf("destroy = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, j.ID, jobOK)
	argv := strings.Join(wk.argvLines(), "\n")
	if !strings.Contains(argv, "destroy demo1 --yes --events") || strings.Contains(argv, "--no-archive") || strings.Contains(argv, "--delete-data-disk") {
		t.Errorf("argv: %s", argv)
	}
	h = hostsResp{}
	getJSON(t, s, "/api/provision/hosts", userHdr, &h)
	if len(h.Hosts) != 1 || h.Hosts[0].State != "destroyed" || !h.Hosts[0].Spend.Final || h.Hosts[0].Spend.USDHour != 0 || h.Total.Live != 0 {
		t.Fatalf("after: %+v", h)
	}
	final := h.Hosts[0].Spend.USD
	tick(s)
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if h.Hosts[0].Spend.USD != final || !h.Hosts[0].DataKept {
		t.Errorf("the final bill moved or the data disk is not shown as kept: %+v", h.Hosts[0])
	}
	// a destroyed host cannot be destroyed again, and its record stays
	if rec := do(s, "POST", "/api/provision/hosts/demo1/destroy", hdr, destroyBody2("demo1", "demo1", "")); rec.Code != 404 {
		t.Errorf("again = %d %s", rec.Code, rec.Body)
	}
	// the dry-run-free retry/cleanup of a destroy job is refused
	if rec := do(s, "POST", "/api/provision/jobs/"+j.ID+"/retry", hdr, "{}"); rec.Code != 409 {
		t.Errorf("retry of a destroy = %d %s", rec.Code, rec.Body)
	}
}

func TestDestroyFlagsReachTheTool(t *testing.T) {
	s, _, wk, hdr, _ := realHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	rec := do(s, "POST", "/api/provision/hosts/demo1/destroy", hdr, destroyBody2("demo1", "demo1", `,"delete_data_disk":true,"lose_sessions":true`))
	var j hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &j)
	hubWait(t, s, j.ID, jobOK)
	if argv := strings.Join(wk.argvLines(), "\n"); !strings.Contains(argv, "destroy demo1 --yes --events --delete-data-disk --no-archive") {
		t.Errorf("argv: %s", argv)
	}
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if h.Hosts[0].DataKept {
		t.Errorf("the data disk was deleted but shown as kept: %+v", h.Hosts[0])
	}
}

func TestAFailedDestroyLeavesTheHostAndSaysWhy(t *testing.T) {
	s, _, wk, hdr, _ := realHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	wk.set(t, "destroy.exit", "1")
	rec := do(s, "POST", "/api/provision/hosts/demo1/destroy", hdr, destroyBody2("demo1", "demo1", ""))
	var j hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &j)
	hubWait(t, s, j.ID, jobFailed)
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if h.Hosts[0].State != "running" || h.Hosts[0].Destroyed != "" || h.Hosts[0].Spend.Final {
		t.Errorf("a failed destroy changed the host: %+v", h.Hosts[0])
	}
	// and it can be asked again
	wk.set(t, "destroy.exit", "0")
	rec = do(s, "POST", "/api/provision/hosts/demo1/destroy", hdr, destroyBody2("demo1", "demo1", ""))
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != 202 {
		t.Fatalf("second try = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, j.ID, jobOK)
}

func TestAProvisionerRefusesToDestroyWhatItDidNotBuild(t *testing.T) {
	w := newFakeWorker(t)
	s := provPeerServer(t, w)
	rec := do(s, "POST", "/api/provisioner/host/destroy", hubCall, `{"name":"someone-elses"}`)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "no_host") {
		t.Errorf("destroy = %d %s", rec.Code, rec.Body)
	}
	if w.runs("destroy") != 0 {
		t.Errorf("tool ran: %v", w.argv())
	}
}

func TestACleanedUpFailedCreateEndsItsRecord(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", `{"v":1,"seq":1,"stage":"instance","step":"4/9","state":"ok"}`+"\n"+`{"v":1,"seq":2,"stage":"bootstrap","step":"6/9","state":"failed","class":"bootstrap_failed","msg":"apt failed"}`+"\n")
	w.set(t, "create.exit", "1")
	s := provPeerServer(t, w)
	s.cfg.Provision.Tool.Real = true
	w.set(t, "catalog.json", string(mustRead(t, provGolden)))
	code, v := startPeerJob(t, s, "key-aaaaaaaa")
	if code != 202 {
		t.Fatalf("create = %d", code)
	}
	waitJob(t, s, v.ID, jobFailed)
	if h, ok := s.phosts.Get("demo1"); !ok || h.Destroyed != nil {
		t.Fatalf("the instance that exists is not on the books: %+v %v", h, ok)
	}
	do(s, "POST", "/api/provisioner/job/cleanup", hubCall, `{"id":"`+v.ID+`"}`)
	waitJob(t, s, v.ID, jobCleaned)
	waitUntil(t, "record ended", func() bool { h, _ := s.phosts.Get("demo1"); return h.Destroyed != nil })
}
