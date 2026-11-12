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

// ---- the OpenShell kind ----

const sandboxSpec = `{"kind":"openshell","name":"box1","idle_minutes":120,"destroy_after_hours":72}`

func sandboxHub(t *testing.T, osState string) (*Server, *provPeer, *fakeWorkerDir, map[string]string) {
	t.Helper()
	s, prov, wk, hdr := hubSetup(t)
	prov.peer.cfg.Provision.Tool.Real = true
	pf := strings.Replace(string(mustRead(t, "../provision/testdata/preflight.json")), "us-east-2", "us-east-1", -1)
	pf = strings.Replace(pf, `"openshell": {"state": "missing", "note": "no openshell gateway, workspace and image in the config"}`,
		`"openshell": {"state": "`+osState+`", "note": "gateway answers", "gateway": "homelab", "workspace": "ws", "image": "registry.example/sbx:{version}", "policy": "sandbox-policy.yaml", "cpu": "2", "memory": "6Gi"}`, 1)
	wk.set(t, "preflight.json", pf)
	ev := ""
	for i, st := range provision.SandboxStages {
		ev += `{"v":1,"seq":` + string(rune('1'+i)) + `,"stage":"` + st + `","step":"` + string(rune('1'+i)) + `/4","state":"ok"}` + "\n"
	}
	wk.set(t, "create.events", ev)
	wk.set(t, "create.stderr", "")
	wk.set(t, "status.json", `{"v":1,"provider":"openshell","workers":[{"name":"box1","state":"RUNNING","type":"","managed":true,"launched_at":"`+time.Now().Add(-30*time.Minute).UTC().Format(time.RFC3339)+`","last_stop_at":null,"caps":{}}]}`)
	tick(s)
	do(s, "POST", "/api/provision/refresh", hdr, `{"what":"nodes"}`)
	return s, prov, wk, hdr
}

func TestNodesOfferTheKindsTheirCredentialsAllow(t *testing.T) {
	s, _, _, _ := sandboxHub(t, "ok")
	var n nodesResp2
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	got := map[string]kindView{}
	for _, k := range n.Nodes[0].Kinds {
		got[k.Kind] = k
	}
	if !got["aws"].OK || !got["openshell"].OK || n.Nodes[0].Preflight.OpenShell.Image == "" || n.Nodes[0].Preflight.OpenShell.Policy != "sandbox-policy.yaml" {
		t.Errorf("kinds: %+v %+v", got, n.Nodes[0].Preflight.OpenShell)
	}
	s2, _, _, _ := sandboxHub(t, "expired")
	getJSON(t, s2, "/api/provision/nodes", userHdr, &n)
	for _, k := range n.Nodes[0].Kinds {
		if k.Kind == "openshell" && (k.OK || !strings.Contains(k.Why, "openshell expired")) {
			t.Errorf("an expired gateway login is offered: %+v", k)
		}
	}
}

type nodesResp2 struct {
	Nodes []struct {
		Name      string               `json:"name"`
		Kinds     []kindView           `json:"kinds"`
		Preflight *provision.Preflight `json:"preflight"`
	} `json:"nodes"`
}

func TestASandboxIsPlannedBuiltAndDestroyedThroughTheSamePath(t *testing.T) {
	s, _, wk, hdr := sandboxHub(t, "ok")
	rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+sandboxSpec+`}`)
	var pl struct {
		OK    bool            `json:"ok"`
		Ready bool            `json:"ready"`
		Cost  *provision.Cost `json:"cost"`
	}
	json.Unmarshal(rec.Body.Bytes(), &pl)
	if rec.Code != 200 || !pl.OK || !pl.Ready || pl.Cost == nil || pl.Cost.Mode != "sandbox" || pl.Cost.USDHour != 0 {
		t.Fatalf("plan = %d %s", rec.Code, rec.Body)
	}
	// the tool is handed only the three fields a sandbox may have
	if b := mustRead(t, wk.dir+"/spec.in"); false {
		_ = b
	}
	rec = do(s, "POST", "/api/provision", hdr, `{"spec":`+sandboxSpec+`,"key":"sandbox-key-1","ack":true}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 202 || v.Dry {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var stages struct {
		Stages []string `json:"stages"`
	}
	json.Unmarshal(rec.Body.Bytes(), &stages)
	if strings.Join(stages.Stages, ",") != "sandbox,tailnet,settings,hub" {
		t.Errorf("stages: %v", stages.Stages)
	}
	hubWait(t, s, v.ID, jobOK)
	if in := string(mustRead(t, wk.dir+"/spec.in")); in != `{"destroy_after_hours":72,"idle_minutes":120,"kind":"openshell","name":"box1"}` {
		t.Errorf("the tool read: %s", in)
	}
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if len(h.Hosts) != 1 || h.Hosts[0].Kind != "openshell" || h.Hosts[0].Mode != "sandbox" || h.Hosts[0].Spend.USD != 0 || h.Hosts[0].State != "running" {
		t.Fatalf("host: %+v", h.Hosts)
	}
	// a stopped sandbox is destroyed after its wait just like a VM; destroy goes through the same job
	rec = do(s, "POST", "/api/provision/hosts/box1/destroy", hdr, `{"confirm":"box1"}`)
	var j hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != 202 {
		t.Fatalf("destroy = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, j.ID, jobOK)
	if !strings.Contains(strings.Join(wk.argvLines(), "\n"), "destroy box1 --yes --events") {
		t.Errorf("argv: %v", wk.argvLines())
	}
}

func TestASandboxNeedsTheGatewayLoginNotAnAWSOne(t *testing.T) {
	s, _, wk, hdr := sandboxHub(t, "expired")
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+sandboxSpec+`,"key":"sandbox-key-2","ack":true}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "openshell expired") {
		t.Errorf("create with an expired gateway login = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("create") != 0 {
		t.Error("the tool ran")
	}
	// a VM spec still needs aws, not the gateway: it is not held back by it
	rec = do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+okSpec+`,"region":"us-east-1"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ready":true`) {
		t.Errorf("a VM plan: %d %s", rec.Code, rec.Body)
	}
}

// ---- the Google Cloud kind ----

const gceSpec = `{"kind":"gcp","name":"gbox1","type":"e2-standard-4","spot":true,"zone":"us-central1-b","boot_disk_gb":20,"data_disk_gb":50,"idle_minutes":120,"max_run":"24h","budget_cap_usd":10,"destroy_after_hours":72}`

func gceHub(t *testing.T, gcpState string) (*Server, *provPeer, *fakeWorkerDir, map[string]string) {
	t.Helper()
	s, prov, wk, hdr := hubSetup(t)
	prov.peer.cfg.Provision.Tool.Real = true
	pf := strings.Replace(string(mustRead(t, "../provision/testdata/preflight.json")), "us-east-2", "us-east-1", -1)
	pf = strings.Replace(pf, `"gcp": {"state": "missing", "note": "no active gcloud account"}`, `"gcp": {"state": "`+gcpState+`", "note": "an account is active"}`, 1)
	pf = strings.Replace(pf, `"region":`, `"gcp_zone": "us-central1-a", "region":`, 1)
	wk.set(t, "preflight.json", pf)
	wk.set(t, "catalog.json", string(mustRead(t, "../provision/testdata/catalog-gcp.json")))
	ev := ""
	for i, st := range provision.Stages {
		ev += `{"v":1,"seq":` + string(rune('1'+i)) + `,"stage":"` + st + `","step":"` + string(rune('1'+i)) + `/9","state":"ok"}` + "\n"
	}
	wk.set(t, "create.events", ev)
	wk.set(t, "create.stderr", "")
	wk.set(t, "status.json", `{"v":1,"provider":"gcp","workers":[{"name":"gbox1","state":"RUNNING","type":"e2-standard-4","mode":"spot","managed":true,"launched_at":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`","last_stop_at":null,"caps":{}}]}`)
	tick(s)
	do(s, "POST", "/api/provision/refresh", hdr, `{"what":"nodes"}`)
	return s, prov, wk, hdr
}

func TestGCEPriceListComesFromTheNodesOwnZone(t *testing.T) {
	s, _, wk, _ := gceHub(t, "ok")
	var cv struct {
		Region  string             `json:"region"`
		Kind    string             `json:"kind"`
		Catalog *provision.Catalog `json:"catalog"`
	}
	if code := getJSON(t, s, "/api/provision/catalog?kind=gcp&via=prov", userHdr, &cv); code != 200 {
		t.Fatalf("catalog = %d", code)
	}
	if cv.Kind != "gcp" || cv.Region != "us-central1" || cv.Catalog.Kind != "gcp" || len(cv.Catalog.Rows) != 3 || cv.Catalog.Rows[1].Type != "e2-standard-4" {
		t.Errorf("catalog: %+v", cv)
	}
	if !strings.Contains(strings.Join(wk.argvLines(), "\n"), "catalog --kind gcp --region us-central1 --json") {
		t.Errorf("argv: %v", wk.argvLines())
	}
	if !strings.Contains(cv.Catalog.Sources["on_demand"].Source, "not live") {
		t.Errorf("the table is not marked as not live: %+v", cv.Catalog.Sources)
	}
}

func TestAGCEHostIsPlannedBuiltAndPriced(t *testing.T) {
	s, _, wk, hdr := gceHub(t, "ok")
	rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+gceSpec+`}`)
	var pl struct {
		OK     bool            `json:"ok"`
		Ready  bool            `json:"ready"`
		Region string          `json:"region"`
		Cost   *provision.Cost `json:"cost"`
	}
	json.Unmarshal(rec.Body.Bytes(), &pl)
	if rec.Code != 200 || !pl.OK || !pl.Ready || pl.Region != "us-central1" || pl.Cost == nil || pl.Cost.Mode != "spot" || pl.Cost.USDHour <= 0.03 {
		t.Fatalf("plan = %d %s", rec.Code, rec.Body)
	}
	// a request for another region is refused, not priced for one and built in another
	if rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+gceSpec+`,"region":"us-east1"}`); rec.Code != 422 || !strings.Contains(rec.Body.String(), "builds only in us-central1") {
		t.Errorf("other region = %d %s", rec.Code, rec.Body)
	}
	rec = do(s, "POST", "/api/provision", hdr, `{"spec":`+gceSpec+`,"key":"gce-key-0001","ack":true}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 202 || v.Dry {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, v.ID, jobOK)
	var in provision.Spec
	json.Unmarshal(mustRead(t, wk.dir+"/spec.in"), &in)
	if in.Kind != "gcp" || in.Type != "e2-standard-4" || !in.Spot || in.Zone != "us-central1-b" {
		t.Errorf("the tool read %+v", in)
	}
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if len(h.Hosts) != 1 || h.Hosts[0].Kind != "gcp" || h.Hosts[0].Mode != "spot" || h.Hosts[0].Spend.USD <= 0 || h.Hosts[0].Region != "us-central1" {
		t.Fatalf("host: %+v", h.Hosts)
	}
}

func TestAGCEHostNeedsTheGoogleLoginNotTheAWSOne(t *testing.T) {
	s, _, wk, hdr := gceHub(t, "expired")
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+gceSpec+`,"key":"gce-key-0002","ack":true}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "gcp expired") {
		t.Errorf("create = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("create") != 0 {
		t.Error("the tool ran")
	}
	// the type must be a machine type of its own cloud
	if rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+strings.Replace(gceSpec, "e2-standard-4", "t3.large", 1)+`}`); rec.Code != 422 {
		t.Errorf("an aws type for gcp = %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+strings.Replace(okSpec, "t3.large", "e2-standard-4", 1)+`,"region":"us-east-1"}`); rec.Code != 422 {
		t.Errorf("a gcp type for aws = %d %s", rec.Code, rec.Body)
	}
}
