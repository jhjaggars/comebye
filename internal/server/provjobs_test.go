package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/provision"
)

const okSpec = `{"kind":"aws","name":"demo1","type":"t3.large","spot":true,"boot_disk_gb":20,"data_disk_gb":50,"idle_minutes":120,"max_run":"24h","budget_cap_usd":10,"destroy_after_hours":72}`

func jobBody(key string) string {
	return `{"spec":` + okSpec + `,"region":"us-east-1","key":"` + key + `"}`
}

func plannedEvents() string {
	var b strings.Builder
	for i, st := range provision.Stages {
		b.WriteString(`{"v":1,"seq":` + string(rune('1'+i)) + `,"stage":"` + st + `","step":"` + string(rune('1'+i)) + `/9","state":"planned"}` + "\n")
	}
	return b.String()
}

func peerJob(t *testing.T, s *Server, id string, after int) pjobView {
	t.Helper()
	rec := do(s, "GET", "/api/provisioner/job?id="+id+"&after="+string(rune('0'+after)), hubCall, "")
	var v pjobView
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("job = %d %s", rec.Code, rec.Body)
	}
	return v
}

func waitJob(t *testing.T, s *Server, id, state string) pjobView {
	t.Helper()
	var v pjobView
	waitUntil(t, "job "+state, func() bool { v = peerJob(t, s, id, 0); return v.State == state })
	return v
}

func startPeerJob(t *testing.T, s *Server, key string) (int, pjobView) {
	t.Helper()
	rec := do(s, "POST", "/api/provisioner/jobs", hubCall, jobBody(key))
	var v pjobView
	json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v
}

func TestPeerPlanRunsTheDryRunAndPricesFromItsOwnCatalog(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", plannedEvents())
	w.set(t, "create.stderr", "DRY RUN: nothing is created\nworker      demo1\n")
	s := provPeerServer(t, w)
	rec := do(s, "POST", "/api/provisioner/plan", hubCall, `{"spec":`+okSpec+`,"region":"us-east-1"}`)
	if rec.Code != 200 {
		t.Fatalf("plan = %d %s", rec.Code, rec.Body)
	}
	var a struct {
		OK     bool            `json:"ok"`
		DryRun bool            `json:"dry_run"`
		Plan   provision.Plan  `json:"plan"`
		Cost   provision.Cost  `json:"cost"`
		Warn   []string        `json:"warnings"`
		Raw    json.RawMessage `json:"-"`
	}
	json.Unmarshal(rec.Body.Bytes(), &a)
	if !a.OK || !a.DryRun || len(a.Plan.Stages) != 9 || !strings.Contains(a.Plan.Text, "nothing is created") {
		t.Fatalf("plan: %s", rec.Body)
	}
	// t3.large in the recorded catalog: the cheapest zone plus the IPv4 and 70 GB of disk
	if a.Cost.Mode != "spot" || a.Cost.USDHour < 0.03 || a.Cost.USDHour > 0.04 || a.Cost.USDDay <= 0 || a.Cost.BudgetHours <= 0 {
		t.Errorf("cost: %+v", a.Cost)
	}
	args := strings.Join(w.argv(), " ")
	if !strings.Contains(args, "create --dry-run --events --spec -") {
		t.Errorf("the plan did not run the dry run: %s", args)
	}
	if got := string(mustRead(t, filepath.Join(w.dir, "spec.in"))); !strings.Contains(got, `"name":"demo1"`) || strings.Contains(got, "key") {
		t.Errorf("spec on stdin: %s", got)
	}
}

func TestPeerPlanHoldsTheCostToItsCeiling(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", plannedEvents())
	s := provPeerServer(t, w)
	s.cfg.Provision.Tool.MaxUSDHour = 0.01
	rec := do(s, "POST", "/api/provisioner/plan", hubCall, `{"spec":`+okSpec+`,"region":"us-east-1"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"refused_code":"over_ceiling"`) || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("plan = %d %s", rec.Code, rec.Body)
	}
	// ...and a job over the ceiling is not started
	rec = do(s, "POST", "/api/provisioner/jobs", hubCall, jobBody("key-over-ceiling"))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "over_ceiling") {
		t.Fatalf("job = %d %s", rec.Code, rec.Body)
	}
	if w.runs("create") != 1 { // the plan's dry run only
		t.Errorf("create ran %d times", w.runs("create"))
	}
}

func TestPeerRefusesBadSpecsBeforeTheTool(t *testing.T) {
	w := newFakeWorker(t)
	s := provPeerServer(t, w)
	bad := map[string]string{
		"unknown field": `{"spec":{"kind":"aws","name":"demo1","type":"t3.large","shell":"x"}}`,
		"shell name":    strings.Replace(okSpec, `"demo1"`, `"a;rm -rf"`, 1),
		"type shape":    strings.Replace(okSpec, "t3.large", "t4g", 1),
		"huge budget":   strings.Replace(okSpec, `"budget_cap_usd":10`, `"budget_cap_usd":100000`, 1),
		"no budget":     strings.Replace(okSpec, `"budget_cap_usd":10`, `"budget_cap_usd":0`, 1),
		"huge disk":     strings.Replace(okSpec, `"data_disk_gb":50`, `"data_disk_gb":999999`, 1),
		"bad zone":      strings.Replace(okSpec, `"spot":true`, `"spot":true,"zone":"x y"`, 1),
		"gce":           strings.Replace(okSpec, `"aws"`, `"gcp"`, 1),
	}
	for name, spec := range bad {
		body := spec
		if !strings.HasPrefix(spec, `{"spec"`) {
			body = `{"spec":` + spec + `}`
		}
		rec := do(s, "POST", "/api/provisioner/plan", hubCall, body)
		if rec.Code != 422 && rec.Code != 400 {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if w.runs("create") != 0 {
		t.Fatalf("the tool ran for a refused spec: %v", w.argv())
	}
}

func TestPeerJobRunsDryJournalsAndIsIdempotent(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", plannedEvents())
	w.set(t, "create.stderr", "DRY RUN: nothing is created, no key is minted\n")
	s := provPeerServer(t, w)
	code, v := startPeerJob(t, s, "key-one-12345")
	if code != http.StatusAccepted || !v.Dry || !jobIDRe.MatchString(v.ID) {
		t.Fatalf("start = %d %+v", code, v)
	}
	done := waitJob(t, s, v.ID, jobOK)
	events := 0
	for _, it := range done.Items {
		if it.Kind == "event" && it.State == "planned" {
			events++
		}
		if it.Seq == 0 || it.Attempt != 1 {
			t.Errorf("item: %+v", it)
		}
	}
	if events != 9 || done.Next != len(done.Items) {
		t.Errorf("events %d next %d items %d", events, done.Next, len(done.Items))
	}
	// the same key returns the same job and runs nothing again
	code2, again := startPeerJob(t, s, "key-one-12345")
	if code2 != http.StatusOK || again.ID != v.ID {
		t.Errorf("repeat = %d %s, want 200 %s", code2, again.ID, v.ID)
	}
	if w.runs("create --dry-run") != 1 {
		t.Errorf("create ran %d times: %v", w.runs("create --dry-run"), w.argvLines())
	}
	// a cursor returns only what is new
	if tail := peerJob(t, s, v.ID, 5); len(tail.Items) != len(done.Items)-5 {
		t.Errorf("after 5: %d of %d", len(tail.Items), len(done.Items))
	}
}

func TestPeerWithoutTheRealSwitchNeverRunsARealCreate(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", plannedEvents())
	s := provPeerServer(t, w)
	_, v := startPeerJob(t, s, "key-dry-1234")
	waitJob(t, s, v.ID, jobOK)
	for _, l := range w.argvLines() {
		if strings.Contains(l, " create ") && !strings.Contains(l, "--dry-run") {
			t.Errorf("a create without --dry-run: %s", l)
		}
	}
	// a cleanup of a dry job does not run the tool's destroy either
	w.set(t, "create.exit", "1")
	w.set(t, "create.events", `{"v":1,"seq":1,"stage":"network","step":"1/9","state":"failed","class":"quota","msg":"too many"}`+"\n")
	_, f := startPeerJob(t, s, "key-fail-1234")
	waitJob(t, s, f.ID, jobFailed)
	rec := do(s, "POST", "/api/provisioner/job/cleanup", hubCall, `{"id":"`+f.ID+`"}`)
	if rec.Code != 200 || w.runs("destroy") != 0 || !strings.Contains(rec.Body.String(), jobCleaned) {
		t.Fatalf("cleanup = %d %s; destroy ran %d", rec.Code, rec.Body, w.runs("destroy"))
	}
}

func TestPeerRealSwitchRunsCreateWithoutDryRunAndCleanupDestroys(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.exit", "1")
	w.set(t, "create.events", `{"v":1,"seq":1,"stage":"instance","step":"4/9","state":"failed","class":"spot_capacity","zone":"us-east-1b","msg":"InsufficientInstanceCapacity"}`+"\n")
	s := provPeerServer(t, w)
	s.cfg.Provision.Tool.Real = true
	_, v := startPeerJob(t, s, "key-real-1234")
	f := waitJob(t, s, v.ID, jobFailed)
	if f.Dry || f.Class != "spot_capacity" || f.Zone != "us-east-1b" {
		t.Fatalf("failed job: %+v", f)
	}
	lines := w.argvLines()
	if !strings.Contains(strings.Join(lines, "\n"), "create --events --spec -") || strings.Contains(lines[len(lines)-1], "--dry-run") {
		t.Errorf("argv: %v", lines)
	}
	rec := do(s, "POST", "/api/provisioner/job/cleanup", hubCall, `{"id":"`+v.ID+`"}`)
	if rec.Code != 200 {
		t.Fatalf("cleanup = %d %s", rec.Code, rec.Body)
	}
	waitJob(t, s, v.ID, jobCleaned)
	if !strings.Contains(strings.Join(w.argvLines(), "\n"), "destroy demo1 --yes --no-archive --delete-data-disk --events") {
		t.Errorf("argv: %v", w.argvLines())
	}
}

func TestPeerJobFailureThenRetry(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.exit", "1")
	w.set(t, "create.stderr", "worker: instance: InsufficientInstanceCapacity\n")
	w.set(t, "create.events", `{"v":1,"seq":1,"stage":"network","step":"1/9","state":"running"}`+"\n"+`{"v":1,"seq":2,"stage":"network","step":"1/9","state":"ok"}`+"\n"+`{"v":1,"seq":3,"stage":"instance","step":"4/9","state":"failed","class":"spot_capacity","zone":"us-east-1b","msg":"no capacity"}`+"\n")
	s := provPeerServer(t, w)
	_, v := startPeerJob(t, s, "key-retry-1234")
	f := waitJob(t, s, v.ID, jobFailed)
	if f.Class != "spot_capacity" || f.Zone != "us-east-1b" || f.Attempt != 1 {
		t.Fatalf("failed: %+v", f)
	}
	if rec := do(s, "POST", "/api/provisioner/job/retry", hubCall, `{"id":"pj_0000000000000000"}`); rec.Code != 404 {
		t.Errorf("retry of nothing = %d", rec.Code)
	}
	w.set(t, "create.exit", "0")
	w.set(t, "create.events", plannedEvents())
	if rec := do(s, "POST", "/api/provisioner/job/retry", hubCall, `{"id":"`+v.ID+`"}`); rec.Code != 200 {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body)
	}
	ok := waitJob(t, s, v.ID, jobOK)
	if ok.Attempt != 2 || ok.Class != "" {
		t.Errorf("after retry: %+v", ok)
	}
	// a finished job cannot be retried
	if rec := do(s, "POST", "/api/provisioner/job/retry", hubCall, `{"id":"`+v.ID+`"}`); rec.Code != 409 {
		t.Errorf("retry of an ok job = %d", rec.Code)
	}
}

func TestPeerRunsOneJobAtATime(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", plannedEvents())
	w.set(t, "create.sleep", "1")
	s := provPeerServer(t, w)
	_, v := startPeerJob(t, s, "key-first-1234")
	code, _ := startPeerJob(t, s, "key-second-1234")
	if code != http.StatusConflict {
		t.Errorf("second job = %d, want 409", code)
	}
	waitJob(t, s, v.ID, jobOK)
	code, third := startPeerJob(t, s, "key-third-1234")
	if code != http.StatusAccepted {
		t.Fatalf("job after the first finished = %d", code)
	}
	// it journals to the state dir after the response: let it finish before the temp dir goes
	waitJob(t, s, third.ID, jobOK)
}

func TestPeerRestartMarksARunningJobInterrupted(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", plannedEvents())
	w.set(t, "create.sleep", "3")
	dir := t.TempDir()
	cfg := Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "prov", StateDir: dir, NoLedger: true, AgentToken: testToken,
		Provision: ProvisionConfig{Tool: &provision.Tool{Bin: w.bin, Config: "/etc/worker.conf"}}}
	s := New(cfg, nil, nil)
	_, v := startPeerJob(t, s, "key-restart-1234")
	time.Sleep(200 * time.Millisecond)
	s2 := New(cfg, nil, nil) // the same state directory, as after a restart
	got := peerJob(t, s2, v.ID, 0)
	if got.State != jobInterrupted || !strings.Contains(got.Msg, "restarted") {
		t.Fatalf("after restart: %+v", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "provision", "jobs", v.ID+".json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("journal: %v %v", fi, err)
	}
}

func TestPeerKeepsSecretsOutOfTheJournal(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "create.events", `{"v":1,"seq":1,"stage":"tailnet","step":"7/9","state":"running","msg":"key tskey-auth-ABCDEF123456 and AKIAABCDEFGHIJKLMNOP"}`+"\n"+`{"v":1,"seq":2,"stage":"mystery","state":"ok"}`+"\n"+"not json\n")
	w.set(t, "create.stderr", "using token=hunter2 now\n")
	s := provPeerServer(t, w)
	_, v := startPeerJob(t, s, "key-secret-1234")
	done := waitJob(t, s, v.ID, jobOK)
	b, _ := json.Marshal(done)
	for _, bad := range []string{"tskey-auth-ABCDEF", "hunter2", "mystery"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("%q reached the journal: %s", bad, b)
		}
	}
}
