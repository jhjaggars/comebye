package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/provision"
)

func hubSetup(t *testing.T) (*Server, *provPeer, *fakeWorkerDir, map[string]string) {
	t.Helper()
	wk := newFakeWorker(t)
	wk.set(t, "create.events", plannedEvents())
	wk.set(t, "create.stderr", "DRY RUN: nothing is created\n")
	// the node's own region is the one the catalog fixture is for
	wk.set(t, "preflight.json", strings.Replace(string(mustRead(t, "../provision/testdata/preflight.json")), "us-east-2", "us-east-1", -1))
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	var n nodesResp
	if getJSON(t, s, "/api/provision/nodes", userHdr, &n) != 200 || n.CSRF == "" {
		t.Fatal("no csrf from nodes")
	}
	hdr := map[string]string{"Tailscale-User-Login": "me@example.com", HeaderCSRF: n.CSRF}
	return s, prov, wk, hdr
}

type hubJobResp struct {
	ID         string           `json:"id"`
	State      string           `json:"state"`
	Dry        bool             `json:"dry_run"`
	Attempt    int              `json:"attempt"`
	Class      string           `json:"class"`
	Zone       string           `json:"zone"`
	Next       int              `json:"next"`
	Items      []provision.Item `json:"items"`
	Alt        *jobAlternatives `json:"alternatives"`
	CanRetry   bool             `json:"can_retry"`
	Superseded string           `json:"superseded"`
	Spec       provision.Spec   `json:"spec"`
}

func hubWait(t *testing.T, s *Server, id, state string) hubJobResp {
	t.Helper()
	var v hubJobResp
	waitUntil(t, "hub job "+state, func() bool {
		v = hubJobResp{}
		getJSON(t, s, "/api/provision/jobs/"+id, userHdr, &v)
		return v.State == state
	})
	return v
}

func TestHubPlanRelaysThePlanAndCost(t *testing.T) {
	s, prov, wk, hdr := hubSetup(t)
	rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+okSpec+`,"region":"us-east-1"}`)
	if rec.Code != 200 {
		t.Fatalf("plan = %d %s", rec.Code, rec.Body)
	}
	var a struct {
		OK     bool            `json:"ok"`
		DryRun bool            `json:"dry_run"`
		Ready  bool            `json:"ready"`
		Plan   provision.Plan  `json:"plan"`
		Cost   *provision.Cost `json:"cost"`
	}
	json.Unmarshal(rec.Body.Bytes(), &a)
	if !a.OK || !a.DryRun || !a.Ready || a.Cost == nil || len(a.Plan.Stages) != 9 || a.Plan.Text == "" {
		t.Fatalf("plan: %s", rec.Body)
	}
	if prov.asked("/api/provisioner/plan") != 1 || wk.runs("create --dry-run") != 1 {
		t.Errorf("asked %v", prov.paths)
	}
	// nothing was started: a plan changes nothing
	if len(s.hjobs.jobs) != 0 {
		t.Error("a plan created a job")
	}
}

func TestHubPlanAndCreateNeedTheCSRFTokenAndAValidSpec(t *testing.T) {
	s, prov, _, hdr := hubSetup(t)
	for _, p := range []string{"/api/provision/plan", "/api/provision"} {
		if rec := do(s, "POST", p, userHdr, `{"spec":`+okSpec+`}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "csrf") {
			t.Errorf("%s without the token = %d %s", p, rec.Code, rec.Body)
		}
	}
	bad := map[string]string{
		"unknown field": `{"spec":` + okSpec + `,"extra":1,"key":"abcdefgh12"}`,
		"unknown node":  `{"via":"nobody","spec":` + okSpec + `,"key":"abcdefgh12"}`,
		"bad key":       `{"spec":` + okSpec + `,"key":"x"}`,
		"bad region":    `{"spec":` + okSpec + `,"region":"us east","key":"abcdefgh12"}`,
		"bad spec":      `{"spec":{"kind":"aws","name":"Bad Name","type":"t3.large"},"key":"abcdefgh12"}`,
	}
	for name, body := range bad {
		rec := do(s, "POST", "/api/provision", hdr, body)
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if prov.asked("jobs") != 0 {
		t.Fatalf("a refused request reached the provisioner: %v", prov.paths)
	}
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":{"kind":"aws","name":"Bad Name","type":"t3.large"},"key":"abcdefgh12"}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"field":"name"`) {
		t.Errorf("bad name = %d %s", rec.Code, rec.Body)
	}
}

func TestHubCreateRunsADryRunEndToEndAndIsIdempotent(t *testing.T) {
	s, prov, wk, hdr := hubSetup(t)
	body := `{"spec":` + okSpec + `,"region":"us-east-1","key":"hub-key-12345"}`
	rec := do(s, "POST", "/api/provision", hdr, body)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusAccepted || !hubJobIDRe.MatchString(v.ID) || !v.Dry {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	done := hubWait(t, s, v.ID, jobOK)
	planned := 0
	for _, it := range done.Items {
		if it.State == "planned" {
			planned++
		}
	}
	if planned != 9 {
		t.Errorf("planned %d: %+v", planned, done.Items)
	}
	// a double click returns the same job, and the node runs nothing again
	rec = do(s, "POST", "/api/provision", hdr, body)
	var again hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != 200 || again.ID != v.ID {
		t.Errorf("repeat = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("create --dry-run") != 1 || prov.asked("/api/provisioner/jobs") != 1 {
		t.Errorf("the node ran it more than once: %v %v", wk.argvLines(), prov.paths)
	}
	// the cursor returns only what is new
	var tail hubJobResp
	getJSON(t, s, "/api/provision/jobs/"+v.ID+"?after="+itoa(done.Next-2), userHdr, &tail)
	if len(tail.Items) != 2 {
		t.Errorf("tail: %d items", len(tail.Items))
	}
	if code := getJSON(t, s, "/api/provision/jobs/hj_00000000000000ff", userHdr, nil); code != 404 {
		t.Errorf("unknown job = %d", code)
	}
}

func TestHubRefusesARealCreateWhenCredentialsAreMissingButNotADryRun(t *testing.T) {
	s, prov, wk, hdr := hubSetup(t)
	// the node is in dry-run mode: missing credentials do not block the dry run
	pf := string(mustRead(t, "../provision/testdata/preflight.json"))
	pf = strings.Replace(pf, "us-east-2", "us-east-1", -1)
	wk.set(t, "preflight.json", strings.Replace(pf, `"state": "ok"`, `"state": "missing"`, 1))
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+okSpec+`,"region":"us-east-1","key":"hub-key-dry1"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dry run with a missing credential = %d %s", rec.Code, rec.Body)
	}
	// a node with the real switch on does not accept it
	prov.peer.cfg.Provision.Tool.Real = true
	rec = do(s, "POST", "/api/provision", hdr, `{"spec":`+strings.Replace(okSpec, "demo1", "demo2", 1)+`,"region":"us-east-1","key":"hub-key-real"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "creds_missing") {
		t.Fatalf("real create without credentials = %d %s", rec.Code, rec.Body)
	}
	if prov.asked("/api/provisioner/jobs") != 1 {
		t.Errorf("the refused real create reached the node: %v", prov.paths)
	}
}

func TestHubFailureOffersAlternativesRetryAndChange(t *testing.T) {
	s, _, wk, hdr := hubSetup(t)
	wk.set(t, "create.exit", "1")
	wk.set(t, "create.events", `{"v":1,"seq":1,"stage":"instance","step":"4/9","state":"failed","class":"spot_capacity","zone":"us-east-1b","msg":"no capacity in us-east-1b"}`+"\n")
	// the hub has the catalog of the region cached from the price browser
	var cat struct{}
	getJSON(t, s, "/api/provision/catalog?kind=aws&region=us-east-1&via=prov", userHdr, &cat)
	body := `{"spec":` + okSpec + `,"region":"us-east-1","key":"hub-key-fail1"}`
	rec := do(s, "POST", "/api/provision", hdr, body)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	f := hubWait(t, s, v.ID, jobFailed)
	if f.Class != "spot_capacity" || f.Zone != "us-east-1b" || !f.CanRetry || f.Alt == nil || !f.Alt.OnDemand {
		t.Fatalf("failed: %+v alt %+v", f, f.Alt)
	}
	for _, z := range f.Alt.Zones {
		if z == "us-east-1b" {
			t.Errorf("the zone that had no capacity is offered again: %v", f.Alt.Zones)
		}
	}
	if len(f.Alt.Zones) == 0 {
		t.Errorf("no other zones offered from the cached catalog")
	}
	// change the pricing mode: a new job, the old one is superseded
	wk.set(t, "create.exit", "0")
	wk.set(t, "create.events", plannedEvents())
	rec = do(s, "POST", "/api/provision/jobs/"+v.ID+"/change", hdr, `{"spot":false}`)
	var nv hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &nv)
	if rec.Code != http.StatusAccepted || nv.ID == v.ID || nv.Spec.Spot {
		t.Fatalf("change = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, nv.ID, jobOK)
	var old hubJobResp
	getJSON(t, s, "/api/provision/jobs/"+v.ID, userHdr, &old)
	if old.Superseded != nv.ID || old.CanRetry {
		t.Errorf("old job: %+v", old)
	}
	// two fields at once, or none, is refused
	if rec := do(s, "POST", "/api/provision/jobs/"+nv.ID+"/change", hdr, `{"spot":true,"zone":"us-east-1c"}`); rec.Code != 400 {
		t.Errorf("two changes = %d", rec.Code)
	}
	// retry of a job that is not failed
	if rec := do(s, "POST", "/api/provision/jobs/"+nv.ID+"/retry", hdr, ``); rec.Code != 409 {
		t.Errorf("retry of an ok job = %d %s", rec.Code, rec.Body)
	}
}

func TestHubRetryAndCleanupRelayToTheNode(t *testing.T) {
	s, _, wk, hdr := hubSetup(t)
	wk.set(t, "create.exit", "1")
	wk.set(t, "create.events", `{"v":1,"seq":1,"stage":"bootstrap","step":"6/9","state":"failed","class":"bootstrap_failed","msg":"apt failed"}`+"\n")
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+okSpec+`,"region":"us-east-1","key":"hub-key-retr1"}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	hubWait(t, s, v.ID, jobFailed)
	wk.set(t, "create.exit", "0")
	wk.set(t, "create.events", plannedEvents())
	rec = do(s, "POST", "/api/provision/jobs/"+v.ID+"/retry", hdr, ``)
	if rec.Code != 200 {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body)
	}
	ok := hubWait(t, s, v.ID, jobOK)
	if ok.Attempt != 2 {
		t.Errorf("attempt %d", ok.Attempt)
	}
	// the log of both attempts is in the one journal
	attempts := map[int]bool{}
	for _, it := range ok.Items {
		attempts[it.Attempt] = true
	}
	if !attempts[1] || !attempts[2] {
		t.Errorf("attempts in the journal: %v", attempts)
	}
}

func TestHubKeepsJobsAcrossARestartAndShowsAnOfflineNode(t *testing.T) {
	s, prov, _, hdr := hubSetup(t)
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+okSpec+`,"region":"us-east-1","key":"hub-key-rest1"}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	hubWait(t, s, v.ID, jobOK)
	h2 := openHubJobs(s.cfg.StateDir)
	if j := h2.jobs[v.ID]; j == nil || j.State != jobOK || len(j.Items) < 9 || j.Key != "hub-key-rest1" {
		t.Fatalf("after a hub restart: %+v", j)
	}
	// a job that is open while the node is unreachable keeps what it has
	j := s.hjobs.jobs[v.ID]
	s.hjobs.mu.Lock()
	j.State = jobRunning
	j.polled = time.Time{}
	s.hjobs.mu.Unlock()
	prov.setHook(func(w http.ResponseWriter, r *http.Request) bool { // the node drops the connection
		if strings.HasPrefix(r.URL.Path, "/api/provisioner/job") {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return true
		}
		return false
	})
	var out struct {
		State   string `json:"state"`
		Offline string `json:"offline"`
	}
	getJSON(t, s, "/api/provision/jobs/"+v.ID, userHdr, &out)
	if out.State != jobRunning || !strings.Contains(out.Offline, "not answering") {
		t.Errorf("offline: %+v", out)
	}
}

// The guards a real create relies on live in comebye: the node's own region,
// the acknowledgement, a cost that must be priced, and a ceiling on hosts.
func TestHubGuardsForARealCreate(t *testing.T) {
	s, prov, wk, hdr := hubSetup(t)
	prov.peer.cfg.Provision.Tool.Real = true
	prov.peer.cfg.Provision.Tool.MaxHosts = 1
	s.probeNode(t.Context(), "prov", true) // the hub learns the node is not in dry-run mode
	body := func(name, extra string) string {
		return `{"spec":` + strings.Replace(okSpec, "demo1", name, 1) + `,` + extra + `}`
	}
	// no acknowledgement: refused before the node hears of it
	rec := do(s, "POST", "/api/provision", hdr, body("real1", `"key":"real-key-0001"`))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ack_required") {
		t.Fatalf("without ack = %d %s", rec.Code, rec.Body)
	}
	// a region other than the node's own
	rec = do(s, "POST", "/api/provision", hdr, body("real1", `"region":"us-west-2","ack":true,"key":"real-key-0002"`))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "bad_region") {
		t.Fatalf("other region = %d %s", rec.Code, rec.Body)
	}
	if prov.asked("/api/provisioner/jobs") != 0 {
		t.Fatalf("a refused request reached the node: %v", prov.paths)
	}
	// acknowledged, in the node's region: a real create (no --dry-run)
	rec = do(s, "POST", "/api/provision", hdr, body("real1", `"ack":true,"key":"real-key-0003"`))
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusAccepted || v.Dry {
		t.Fatalf("real create = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, v.ID, jobOK)
	last := wk.argvLines()
	if strings.Contains(last[len(last)-1], "--dry-run") || !strings.Contains(last[len(last)-1], "create --events --spec -") {
		t.Errorf("argv: %v", last)
	}
	// the node has built as many hosts as it may
	rec = do(s, "POST", "/api/provision", hdr, body("real2", `"ack":true,"key":"real-key-0004"`))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "too_many_hosts") {
		t.Fatalf("over the host ceiling = %d %s", rec.Code, rec.Body)
	}
}
