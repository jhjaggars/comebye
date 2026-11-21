package provision

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func tp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func newBook(t *testing.T, dir string) (*HostBook, *time.Time) {
	now := t0
	b := OpenHostBook(dir)
	b.Now = func() time.Time { return now }
	return b, &now
}

func addHost(b *HostBook) {
	sp := DefaultSpec("w1", "t3.small") // 20 + 50 GB
	b.Add("w1", "pj_0123456789abcdef", sp, "us-east-1", &Cost{Mode: "spot", ComputeUSDHour: 0.01, IPv4USDHour: 0.005, DiskUSDGBMonth: 0.073}, Estimate{})
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.0002 }

func TestSpendFollowsTheCloudsLaunchAndStopTimes(t *testing.T) {
	b, now := newBook(t, "")
	addHost(b)
	w := func(state, launched, stopped string) StatusWorker {
		x := StatusWorker{Name: "w1", State: state, Mode: "spot", LaunchedAt: launched, LastStopAt: stopped}
		return x
	}
	// Created at t0, launched at t0+1m, running for 2 h.
	*now = t0.Add(2 * time.Hour)
	b.Observe([]StatusWorker{w("RUNNING", tp(t0.Add(time.Minute)), "")})
	h, _ := b.Get("w1")
	sp := h.Spend(*now)
	// compute+ipv4: 119 min * 0.015/h; disks: 2 h * 70 GB * 0.073 / 730
	wantRun := 119.0 / 60 * 0.015
	wantDisk := 2 * 70 * 0.073 / 730
	if !near(sp.Compute, wantRun) || !near(sp.Disks, wantDisk) || !near(sp.USD, wantRun+wantDisk) || sp.Final || !sp.Estimate {
		t.Errorf("running: %+v want %.4f + %.4f", sp, wantRun, wantDisk)
	}
	if !near(sp.USDHour, 0.015+70*0.073/730) {
		t.Errorf("burn: %v", sp.USDHour)
	}
	// Stopped at t0+3h: the compute stops, the disks go on.
	*now = t0.Add(5 * time.Hour)
	b.Observe([]StatusWorker{w("STOPPED", tp(t0.Add(time.Minute)), tp(t0.Add(3*time.Hour)))})
	h, _ = b.Get("w1")
	sp = h.Spend(*now)
	wantRun = (179.0 / 60) * 0.015
	wantDisk = 5 * 70 * 0.073 / 730
	if !near(sp.Compute, wantRun) || !near(sp.Disks, wantDisk) || sp.USDHour > 0.01 {
		t.Errorf("stopped: %+v want %.4f + %.4f", sp, wantRun, wantDisk)
	}
	if h.StoppedAt == nil || !h.StoppedAt.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("stopped at: %v", h.StoppedAt)
	}
	// Started again at t0+6h (a new launch time), seen at t0+7h.
	*now = t0.Add(7 * time.Hour)
	b.Observe([]StatusWorker{w("RUNNING", tp(t0.Add(6*time.Hour)), "")})
	h, _ = b.Get("w1")
	if len(h.Intervals) != 2 || h.StoppedAt != nil {
		t.Fatalf("intervals: %+v stopped %v", h.Intervals, h.StoppedAt)
	}
	sp = h.Spend(*now)
	if !near(sp.Hours, 179.0/60+1) {
		t.Errorf("hours: %v", sp.Hours)
	}
}

func TestARestartBetweenTwoLooksClosesTheOldRunWhereItWasLastSeen(t *testing.T) {
	b, now := newBook(t, "")
	addHost(b)
	run := func(at time.Time, launched time.Time) {
		*now = at
		b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(launched)}})
	}
	run(t0.Add(10*time.Minute), t0)
	run(t0.Add(15*time.Minute), t0)
	run(t0.Add(40*time.Minute), t0.Add(30*time.Minute)) // stopped and started in between
	h, _ := b.Get("w1")
	if len(h.Intervals) != 2 || h.Intervals[0].End == nil || !h.Intervals[0].End.Equal(t0.Add(15*time.Minute)) {
		t.Errorf("intervals: %+v", h.Intervals)
	}
}

func TestDestroyFreezesTheBill(t *testing.T) {
	b, now := newBook(t, "")
	addHost(b)
	*now = t0.Add(time.Hour)
	b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}})
	if err := b.BeginDestroy("w1", "pj_1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.Views(); v[0].State != "destroying" {
		t.Errorf("state: %s", v[0].State)
	}
	b.EndDestroy("w1", false, false, "archive failed: bucket unreachable")
	if v, _ := b.Views(); v[0].State != "running" || !strings.Contains(v[0].DestroyFailed, "archive failed") || v[0].DestroyJob != "" {
		t.Errorf("failed destroy: %+v", v[0])
	}
	*now = t0.Add(2 * time.Hour)
	b.BeginDestroy("w1", "pj_2222222222222222")
	b.EndDestroy("w1", true, true, "")
	*now = t0.Add(30 * time.Hour) // a day later: the bill does not move
	v, tot := b.Views()
	if v[0].State != "destroyed" || !v[0].Spend.Final {
		t.Fatalf("destroyed: %+v", v[0])
	}
	// 2 h of compute, plus 2 h of the 20 GB boot disk only (the data disk is kept)
	want := 2*0.015 + 2*20*0.073/730
	if !near(v[0].Spend.USD, want) || !near(tot.USD, want) || tot.Live != 0 || v[0].Spend.USDHour != 0 {
		t.Errorf("final: %+v total %+v want %.4f", v[0].Spend, tot, want)
	}
	if err := b.BeginDestroy("w1", "pj_3333333333333333"); err == nil {
		t.Error("a destroyed host destroyed again")
	}
}

func TestAHostTheCloudNoLongerListsIsGone(t *testing.T) {
	b, now := newBook(t, "")
	addHost(b)
	*now = t0.Add(2 * time.Minute)
	b.Observe(nil) // just launched: not listed yet is not gone
	if h, _ := b.Get("w1"); h.State == "GONE" {
		t.Error("gone right after launch")
	}
	*now = t0.Add(time.Hour)
	b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}})
	*now = t0.Add(2 * time.Hour)
	b.Observe(nil)
	h, _ := b.Get("w1")
	if h.State != "GONE" || h.Intervals[0].End == nil {
		t.Errorf("gone: %+v", h)
	}
}

func TestThePriceTheCloudRecordsReplacesTheEstimate(t *testing.T) {
	b, now := newBook(t, "")
	addHost(b)
	p := 0.02
	w := StatusWorker{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}
	w.Caps.LaunchPriceUSDHr = &p
	*now = t0.Add(time.Hour)
	b.Observe([]StatusWorker{w})
	h, _ := b.Get("w1")
	if h.Rate.ComputeUSDHour != 0.02 || !h.Rate.FromTag {
		t.Errorf("rate: %+v", h.Rate)
	}
}

func TestHostBookSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	b, now := newBook(t, dir)
	addHost(b)
	*now = t0.Add(time.Hour)
	b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}})
	b2 := OpenHostBook(dir)
	h, ok := b2.Get("w1")
	if !ok || len(h.Intervals) != 1 || h.Spec.Type != "t3.small" {
		t.Errorf("reopened: %+v %v", h, ok)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "provision", "hosts.json")); err != nil {
		t.Error(err)
	}
}

func TestParseHostsBelievesNothing(t *testing.T) {
	in := `{"v":1,"hosts":[{"name":"w1","state":"running","type":"t3.small","mode":"spot","spend":{"usd":1.5,"usd_hour":-3,"compute_usd":1e99},"destroy_failed":"key tskey-abcdef123456 leaked\nline2","region":"us-east-1","created":"x","extra":"drop"},
	  {"name":"Bad Name","state":"running"},{"name":"w2","state":"hacked","budget_cap_usd":1e30}],"total":{"usd":NaN}}`
	if _, err := ParseHosts([]byte(in)); err == nil {
		t.Error("NaN accepted")
	}
	in = strings.Replace(in, "NaN", "3", 1)
	v, err := ParseHosts([]byte(in))
	if err != nil || len(v.Hosts) != 2 {
		t.Fatalf("%v %+v", err, v)
	}
	h := v.Hosts[0]
	if h.Spend.USD != 1.5 || h.Spend.USDHour != 0 || h.Spend.Compute != 0 || h.Created != "" || strings.Contains(h.DestroyFailed, "tskey") || strings.Contains(h.DestroyFailed, "\n") {
		t.Errorf("not cleaned: %+v", h)
	}
	if v.Hosts[1].State != "unknown" || v.Hosts[1].BudgetCapUSD != 0 {
		t.Errorf("w2: %+v", v.Hosts[1])
	}
}

func TestParseStatus(t *testing.T) {
	ws, err := ParseStatus([]byte(`{"v":1,"provider":"aws","workers":[{"name":"w1","state":"RUNNING","type":"t3.small","mode":"spot","launched_at":"2026-11-12T10:00:00Z","last_stop_at":null,"caps":{"budget_cap_usd":10,"destroy_after_hours":72,"launch_price_usd_hour":0.0123}},{"name":"A b","state":"RUNNING"},{"name":"w3","state":"weird"}]}`))
	if err != nil || len(ws) != 2 || ws[0].Caps.LaunchPriceUSDHr == nil || *ws[0].Caps.LaunchPriceUSDHr != 0.0123 || ws[1].State != "UNKNOWN" {
		t.Errorf("%+v %v", ws, err)
	}
	if _, err := ParseStatus([]byte(`{"v":2}`)); err == nil {
		t.Error("v2 accepted")
	}
}

func TestEveryManagedInstanceTheToolReportsIsAdopted(t *testing.T) {
	dir := t.TempDir()
	b, now := newBook(t, dir)
	addHost(b) // w1: made from the app
	doc := `{"v":1,"provider":"kubevirt","workers":[
	 {"name":"w1","state":"RUNNING","type":"4cpu","managed":true,"launched_at":"` + tp(t0) + `"},
	 {"name":"kv1","state":"STOPPED","type":"4cpu-16Gi","managed":true,"created_at":"` + tp(t0.Add(-48*time.Hour)) + `","last_stop_at":"` + tp(t0.Add(-3*time.Hour)) + `","caps":{"destroy_after_hours":1}},
	 {"name":"other","state":"RUNNING","type":"x","managed":false}]}`
	ws, err := ParseStatus([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	*now = t0.Add(time.Hour)
	b.Observe(ws)
	kv, ok := b.Get("kv1")
	if !ok || !kv.Adopted || kv.Provider != "kubevirt" || kv.State != "STOPPED" {
		t.Fatalf("kv1: %+v %v", kv, ok)
	}
	if _, ok := b.Get("other"); ok {
		t.Error("an instance the tool does not call managed was adopted")
	}
	if w1, _ := b.Get("w1"); w1.Adopted {
		t.Error("a host made from the app was marked adopted")
	}
	// adopted: no price, no timers, even with a destroy-after tag long past
	if acts := kv.Check(*now); len(acts) != 0 {
		t.Errorf("a timer would act on an adopted host: %v", acts)
	}
	for _, v := range func() []HostView { v, _ := b.Views(); return v }() {
		if v.Name == "kv1" && (!v.Adopted || v.State != "stopped" || v.Provider != "kubevirt" || v.DestroyDue != "") {
			t.Errorf("view: %+v", v)
		}
	}
	// it survives a restart of the node (a kubevirt spec is not a valid aws or gcp one)
	b2, _ := newBook(t, dir)
	if h, ok := b2.Get("kv1"); !ok || !h.Adopted {
		t.Errorf("reloaded: %+v %v", h, ok)
	}
	// once the instance is gone from the cloud it is closed like any other
	*now = t0.Add(5 * time.Hour)
	b.Observe(ws[:1])
	if h, _ := b.Get("kv1"); h.State != "GONE" {
		t.Errorf("gone: %+v", h)
	}
}

func TestRunStartRunsWorkerStartAndStreamsItsText(t *testing.T) {
	bin, dir := fakeWorker(t, "echo 'rewriting the ssh config'\necho 'comebye is active' >&2\n")
	tool := &Tool{Bin: bin, Config: "/etc/worker.conf"}
	var msgs []string
	var mu sync.Mutex
	r := tool.RunStart(context.Background(), "kv1", func(it Item) { mu.Lock(); msgs = append(msgs, it.Msg); mu.Unlock() })
	if !r.OK {
		t.Fatalf("result: %+v", r)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if got := strings.TrimSpace(string(argv)); got != "-config /etc/worker.conf -no-prompt start kv1" {
		t.Errorf("argv: %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(msgs) != 2 {
		t.Errorf("progress lines: %v", msgs)
	}
	if r := tool.RunStart(context.Background(), "bad name", func(Item) {}); r.OK || r.Msg != "bad name" {
		t.Errorf("bad name: %+v", r)
	}
}

func TestRunStartReportsTheToolsLastLineOnFailure(t *testing.T) {
	bin, _ := fakeWorker(t, "echo 'the instance would not start' >&2\nexit 1\n")
	tool := &Tool{Bin: bin, Config: "/etc/worker.conf"}
	r := tool.RunStart(context.Background(), "kv1", func(Item) {})
	if r.OK || !strings.Contains(r.Msg, "would not start") {
		t.Errorf("result: %+v", r)
	}
}

func TestObserveForOnlyClosesItsOwnHosts(t *testing.T) {
	b := OpenHostBook("")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Now = func() time.Time { return t0 }
	kv := StatusWorker{Name: "kv1", State: "STOPPED", Managed: true, Provider: "kubevirt", Type: "4 vCPU, 32Gi"}
	b.ObserveFor("kubevirt", false, []StatusWorker{kv})
	h, ok := b.Get("kv1")
	if !ok || h.Owner != "kubevirt" || !h.Adopted || h.State != "STOPPED" {
		t.Fatalf("adopted: %+v %v", h, ok)
	}
	// another config's look, which does not list kv1, leaves it alone, even long after
	b.Now = func() time.Time { return t0.Add(48 * time.Hour) }
	b.ObserveFor("aws", true, nil)
	if h, _ := b.Get("kv1"); h.State != "STOPPED" {
		t.Errorf("another config closed it: %+v", h)
	}
	// its own config not listing it does close it
	b.ObserveFor("kubevirt", false, nil)
	if h, _ := b.Get("kv1"); h.State != "GONE" {
		t.Errorf("its own look: %+v", h)
	}
}

func TestLegacyRecordsBelongToTheFirstConfig(t *testing.T) {
	b := OpenHostBook("")
	b.Add("w1", "j", Spec{Name: "w1", Kind: "aws"}, "us-east-1", nil, Estimate{})
	b.ObserveFor("kubevirt", false, nil)
	if h, _ := b.Get("w1"); h.Owner != "" {
		t.Errorf("a second config claimed it: %+v", h)
	}
	b.ObserveFor("aws", true, []StatusWorker{{Name: "w1", State: "RUNNING"}})
	if h, _ := b.Get("w1"); h.Owner != "aws" {
		t.Errorf("first config did not claim it: %+v", h)
	}
}

func TestParseToolEntriesAndNames(t *testing.T) {
	es, err := ParseToolEntries("/a/config", "/a/kv.conf:/bin/worker-kv, /b/other")
	if err != nil || len(es) != 3 || es[0].Bin != "" || es[1].Bin != "/bin/worker-kv" || es[1].Config != "/a/kv.conf" {
		t.Fatalf("%+v %v", es, err)
	}
	if es, _ := ParseToolEntries(""); len(es) != 0 {
		t.Errorf("empty: %+v", es)
	}
	dir := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	ts := []*Tool{{Config: w("a", "# c\nprovider=aws # x\n")}, {Config: w("b", "provider = kubevirt\n")}, {Config: w("c", "provider=aws\n")}, {Config: w("d", "project=p\n")}}
	NameTools(ts)
	var got []string
	for _, x := range ts {
		got = append(got, x.Name+"/"+x.Provider)
	}
	if strings.Join(got, " ") != "aws/aws kubevirt/kubevirt aws-2/aws gcp/gcp" {
		t.Errorf("names: %v", got)
	}
}
