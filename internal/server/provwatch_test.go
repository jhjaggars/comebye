package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/provision"
)

// watchPeer is a provisioner with its real path on, one host on the books
// (running since an hour before the clock's start, at $0.10 an hour), and a
// clock the test moves.
func watchPeer(t *testing.T, capUSD float64) (*Server, *fakeWorkerDir, *time.Time) {
	t.Helper()
	w := newFakeWorker(t)
	s := provPeerServer(t, w)
	s.cfg.Provision.Tool.Real = true
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	s.phosts.Now = func() time.Time { return now }
	sp := provision.DefaultSpec("demo1", "t3.large")
	sp.BudgetCapUSD = capUSD
	sp.DestroyAfterHours = 72
	s.phosts.Add("demo1", "pj_0123456789abcdef", sp, "us-east-1", &provision.Cost{Mode: "spot", ComputeUSDHour: 0.10}, provision.Estimate{})
	run := func(state, launched, stopped string) {
		st := `null`
		if stopped != "" {
			st = `"` + stopped + `"`
		}
		w.set(t, "status.json", `{"v":1,"provider":"aws","workers":[{"name":"demo1","state":"`+state+`","type":"t3.large","mode":"spot","managed":true,"launched_at":"`+launched+`","last_stop_at":`+st+`,"caps":{}}]}`)
	}
	run("RUNNING", now.Format(time.RFC3339), "")
	if err := s.observeHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, w, &now
}

func TestWatchdogStopsAHostAtItsCapAndKeepsItsDisks(t *testing.T) {
	s, w, now := watchPeer(t, 1) // $1 at $0.10/h: 10 h
	ctx := context.Background()
	launched := now.Format(time.RFC3339)
	status := func() {
		w.set(t, "status.json", `{"v":1,"provider":"aws","workers":[{"name":"demo1","state":"RUNNING","type":"t3.large","mode":"spot","managed":true,"launched_at":"`+launched+`","last_stop_at":null,"caps":{}}]}`)
		s.observeHosts(ctx)
	}
	*now = now.Add(8*time.Hour + 5*time.Minute)
	status()
	s.watchHosts(ctx)
	h, _ := s.phosts.Get("demo1")
	if !h.Warned80 || h.Warned80At == nil || h.CapStopped != nil {
		t.Fatalf("at 80%%: %+v", h)
	}
	if w.runs(" stop ") != 0 {
		t.Fatalf("stopped too early: %v", w.argv())
	}
	*now = now.Add(2 * time.Hour)
	status()
	s.watchHosts(ctx)
	h, _ = s.phosts.Get("demo1")
	if h.CapStopped == nil || h.State != "STOPPED" || h.Destroyed != nil || h.Destroying != "" {
		t.Fatalf("at 100%%: %+v", h)
	}
	if got := strings.Join(w.argvLines(), "\n"); !strings.Contains(got, " stop demo1") || strings.Contains(got, "destroy") {
		t.Errorf("argv: %s", got)
	}
}

func TestWatchdogWaitsWhileAJobHoldsTheSlot(t *testing.T) {
	s, w, now := watchPeer(t, 1)
	*now = now.Add(11 * time.Hour)
	s.observeHosts(context.Background())
	s.pjobs.mu.Lock()
	s.pjobs.busy = "pj_busy"
	s.pjobs.mu.Unlock()
	s.watchHosts(context.Background())
	if w.runs(" stop ") != 0 {
		t.Fatalf("stopped under a running job: %v", w.argv())
	}
	if h, _ := s.phosts.Get("demo1"); h.CapStopped != nil {
		t.Error("recorded a stop that did not happen")
	}
	s.pjobs.mu.Lock()
	s.pjobs.busy = ""
	s.pjobs.mu.Unlock()
	s.watchHosts(context.Background())
	if w.runs(" stop ") != 1 {
		t.Errorf("next look: %v", w.argv())
	}
}

func TestWatchdogDoesNothingInADryRun(t *testing.T) {
	s, w, now := watchPeer(t, 1)
	s.cfg.Provision.Tool.Real = false
	*now = now.Add(11 * time.Hour)
	s.observeHosts(context.Background())
	s.watchHosts(context.Background())
	if w.runs(" stop ") != 0 {
		t.Errorf("a dry-run node stopped something: %v", w.argv())
	}
}

func stoppedFor(t *testing.T, s *Server, w *fakeWorkerDir, now *time.Time, d time.Duration) {
	t.Helper()
	stop := now.Add(2 * time.Hour)
	*now = stop.Add(d)
	w.set(t, "status.json", `{"v":1,"provider":"aws","workers":[{"name":"demo1","state":"STOPPED","type":"t3.large","mode":"spot","managed":true,"launched_at":"`+stop.Add(-2*time.Hour).Format(time.RFC3339)+`","last_stop_at":"`+stop.Format(time.RFC3339)+`","caps":{}}]}`)
	if err := s.observeHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdogStartsASessionSavingDestroyAfterTheWait(t *testing.T) {
	s, w, now := watchPeer(t, 100)
	stoppedFor(t, s, w, now, 71*time.Hour)
	s.watchHosts(context.Background())
	if w.runs("destroy") != 0 || w.runs(" start ") != 0 {
		t.Fatalf("early: %v", w.argv())
	}
	*now = now.Add(2 * time.Hour)
	s.watchHosts(context.Background())
	waitUntil(t, "destroyed", func() bool { h, _ := s.phosts.Get("demo1"); return h.Destroyed != nil })
	argv := w.argvLines()
	var iStart, iDestroy = -1, -1
	for i, l := range argv {
		if strings.Contains(l, " start demo1") {
			iStart = i
		}
		if strings.Contains(l, "destroy demo1 --yes --events") {
			iDestroy = i
		}
	}
	if iStart < 0 || iDestroy < iStart || strings.Contains(strings.Join(argv, "\n"), "--no-archive") {
		t.Errorf("start must come before destroy, and sessions are saved: %v", argv)
	}
	h, _ := s.phosts.Get("demo1")
	if !strings.Contains(h.AutoDestroy, "stopped since") || !h.DataKept {
		t.Errorf("record: %+v", h)
	}
}

func TestWatchdogDoesNotDestroyWhenTheHostWillNotStart(t *testing.T) {
	s, w, now := watchPeer(t, 100)
	// the fake tool fails every "start"
	w.set(t, "start.fail", "")
	stoppedFor(t, s, w, now, 73*time.Hour)
	s.watchHosts(context.Background())
	waitUntil(t, "failed", func() bool { h, _ := s.phosts.Get("demo1"); return h.DestroyFailed != "" })
	if w.runs("destroy") != 0 {
		t.Fatalf("destroyed without saving its sessions: %v", w.argv())
	}
	h, _ := s.phosts.Get("demo1")
	if h.Destroyed != nil || !strings.Contains(h.DestroyFailed, "start the host to save its sessions") {
		t.Errorf("record: %+v", h)
	}
	// the next look does not try again at once
	s.pjobs.mu.Lock()
	s.pjobs.busy = ""
	s.pjobs.mu.Unlock()
	s.phosts.MarkAutoFailed("demo1")
	before := w.runs(" start ")
	s.watchHosts(context.Background())
	time.Sleep(50 * time.Millisecond)
	if w.runs(" start ") != before {
		t.Error("tried again straight after a failure")
	}
}

// ---- the hub's to-dos ----

func TestHubRaisesTodosOnceForWhatTheProvisionersRecorded(t *testing.T) {
	s, _, _ := relServer(t)
	cap := "2026-11-12T10:00:00Z"
	views := []provision.HostView{
		{Name: "w1", State: "running", BudgetCapUSD: 10, Warned80At: cap, Spend: provision.Spend{USD: 8.2}},
		{Name: "w2", State: "stopped", BudgetCapUSD: 10, CapStopped: cap, Spend: provision.Spend{USD: 10.1, USDHour: 0.01}},
		{Name: "w3", State: "destroyed", AutoDestroy: "stopped since x", Destroyed: cap, Spend: provision.Spend{USD: 1.5}, DataKept: true, DataDiskGB: 50},
		{Name: "w4", State: "stopped", BudgetCapUSD: 10, AutoDestroy: "stopped since y", DestroyFailed: "the host would not start", DestroyFailAt: cap},
		{Name: "w5", State: "destroyed", Destroyed: cap}, // destroyed by a person: no to-do
	}
	s.prov.mu.Lock()
	s.prov.hosts["laharl"] = &provHostsCache{At: time.Now(), View: &provision.HostsView{V: 1, Hosts: views, Total: provision.Total{Live: 3}}}
	s.prov.mu.Unlock()
	s.raiseProvTodos([]string{"laharl"})
	items := s.todos.list("")
	want := map[string]bool{"w1 is at 80%": false, "w2 was stopped at its $10.00 budget cap": false, "w3 was destroyed": false, "automatic destroy of w4": false}
	for _, it := range items {
		if it.CreatedBy != AutoCreator || it.Source != "hub-provision" {
			t.Errorf("source: %+v", it)
		}
		for k := range want {
			if strings.Contains(it.Question, k) {
				want[k] = true
			}
		}
		if strings.Contains(it.Question, "w5") {
			t.Errorf("a to-do for a host a person destroyed: %s", it.Question)
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("no to-do for %q: %v", k, titles(items))
		}
	}
	n := len(items)
	// raising again, even after the first was closed, adds nothing
	s.todos.close(items[0].ID, TodoDropped, "me", "seen")
	s.raiseProvTodos([]string{"laharl"})
	if len(s.todos.list("")) != n-1 { // the closed one stays closed; nothing new
		t.Errorf("raised again: %v", titles(s.todos.list("")))
	}
}

func titles(items []Todo) []string {
	var o []string
	for _, it := range items {
		o = append(o, it.Question)
	}
	return o
}

func TestHubSaysWhenAProvisionerWithHostsHasBeenOfflineTooLong(t *testing.T) {
	s, _, _ := relServer(t)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	s.prov.now = func() time.Time { return now }
	s.prov.mu.Lock()
	s.prov.hosts["laharl"] = &provHostsCache{At: now.Add(-time.Hour), Err: "laharl did not answer", View: &provision.HostsView{V: 1, Total: provision.Total{Live: 2}}}
	s.prov.mu.Unlock()
	s.raiseProvTodos([]string{"laharl"}) // first look at the outage
	if len(s.todos.list("")) != 0 {
		t.Fatalf("raised at once: %v", titles(s.todos.list("")))
	}
	now = now.Add(16 * time.Minute)
	s.raiseProvTodos([]string{"laharl"})
	items := s.todos.list("")
	if len(items) != 1 || !strings.Contains(items[0].Question, "laharl has been offline for 16 minutes, 2 hosts unwatched") {
		t.Fatalf("to-do: %v", titles(items))
	}
	now = now.Add(time.Hour)
	s.raiseProvTodos([]string{"laharl"})
	if len(s.todos.list("")) != 1 {
		t.Error("raised twice for one outage")
	}
}
