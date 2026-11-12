package server

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// fakeFleet is the engine's world: hosts with versions and upgrade records,
// and a clock the test moves.
type fakeFleet struct {
	mu       sync.Mutex
	now      time.Time
	hosts    map[string]*relHost
	order    []string
	starts   []string // hosts asked to start, in order
	startErr map[string]error
	nextID   int
	// the hub wave
	deploy    *DeployRecord
	deploys   int
	deployErr error
	statusErr error
}

func newFakeFleet(target string, names ...string) *fakeFleet {
	f := &fakeFleet{now: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC), hosts: map[string]*relHost{}, startErr: map[string]error{}}
	for _, n := range names {
		f.hosts[n] = &relHost{Name: n, Status: "ok", Version: "v0.6.3", Enabled: true}
		f.order = append(f.order, n)
	}
	return f
}

func (f *fakeFleet) clock() time.Time        { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fakeFleet) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func (f *fakeFleet) Hosts(_ context.Context, version string) []relHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []relHost
	for _, n := range f.order {
		h := *f.hosts[n]
		out = append(out, h)
	}
	markRollout(out, version)
	return out
}

func (f *fakeFleet) StartUpgrade(_ context.Context, job Rollout, host string) (upgrade.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.startErr[host]; err != nil {
		return upgrade.Record{}, err
	}
	f.nextID++
	rec := upgrade.Record{ID: "run" + string(rune('a'+f.nextID)), Version: job.Version, State: upgrade.RecRunning, From: f.hosts[host].Version, Started: f.now}
	f.hosts[host].Record = &rec
	f.starts = append(f.starts, host)
	return rec, nil
}

// finish ends the host's run with a state; ok also moves its version.
func (f *fakeFleet) finish(host, state, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[host]
	r := *h.Record
	r.State, r.Finished = state, f.now
	if state == upgrade.RecFailed || state == upgrade.RecRolledBack {
		r.Error, r.Output = "smoke: journal has errors", "smoke output"
	}
	h.Record = &r
	if version != "" {
		h.Version = version
	}
}

func (f *fakeFleet) StartDeploy(_ context.Context, job Rollout) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deployErr != nil {
		return "", f.deployErr
	}
	f.deploy = &DeployRecord{Version: job.Version, State: depRunning, Started: f.now.UTC()}
	f.deploys++
	return f.now.UTC().Format(time.RFC3339Nano), nil
}

func (f *fakeFleet) DeployStatus(context.Context) (deployStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return deployStatus{}, f.statusErr
	}
	st := deployStatus{Enabled: true}
	if f.deploy != nil {
		c := *f.deploy
		st.Record = &c
	}
	return st, nil
}

// endDeploy finishes the deploy; hubVersion moves the hub's version (the new pod is up).
func (f *fakeFleet) endDeploy(state, errText, hubVersion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.now
	f.deploy.State, f.deploy.Finished, f.deploy.Error = state, &t, errText
	if state == depFailed {
		f.deploy.Output = "bump: FAILED phase=reconcile"
		f.deploy.Headline = "bump: FAILED phase=reconcile"
	}
	if hubVersion != "" {
		f.hosts["hub"].Version = hubVersion
	}
}

func (f *fakeFleet) set(host string, fn func(h *relHost)) {
	f.mu.Lock()
	fn(f.hosts[host])
	f.mu.Unlock()
}

type rolloutRig struct {
	t      *testing.T
	f      *fakeFleet
	e      *rolloutEngine
	path   string
	audit  []ledger.AuditEvent
	states []Rollout
}

func newRolloutRig(t *testing.T, names ...string) *rolloutRig {
	t.Helper()
	r := &rolloutRig{t: t, f: newFakeFleet("v0.6.4", names...), path: filepath.Join(t.TempDir(), "release", "rollout.json")}
	r.e = r.engine()
	return r
}

// engine is a fresh engine on the same file: what a restarted hub has.
func (r *rolloutRig) engine() *rolloutEngine {
	e := newRolloutEngine(r.path, r.f, func(ev ledger.AuditEvent) { r.audit = append(r.audit, ev) })
	e.now = r.f.clock
	e.onState = func(j Rollout) { r.states = append(r.states, j) }
	return e
}

func (r *rolloutRig) start(order ...string) Rollout {
	r.t.Helper()
	plan, err := planRollout(r.f.Hosts(context.Background(), "v0.6.4"), order, "")
	if err != nil {
		r.t.Fatal(err)
	}
	j, err := r.e.Start("t1", "v0.6.4", strings.Repeat("a", 40), "jesse", plan)
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

func (r *rolloutRig) tick() Rollout {
	r.e.tick(context.Background())
	j, _ := r.e.Get()
	return j
}

func (r *rolloutRig) phases() string {
	j, _ := r.e.Get()
	var p []string
	for _, st := range j.Plan {
		p = append(p, st.Host+"="+st.Phase)
	}
	return j.State + " " + strings.Join(p, " ")
}

func (r *rolloutRig) want(s string) {
	r.t.Helper()
	if got := r.phases(); got != s {
		r.t.Fatalf("got  %s\nwant %s", got, s)
	}
}

func (r *rolloutRig) stateSeq() string {
	var a []string
	for _, j := range r.states {
		a = append(a, j.State)
	}
	return strings.Join(a, ",")
}

func (r *rolloutRig) actions() string {
	var a []string
	for _, ev := range r.audit {
		if ev.Host != "" {
			a = append(a, ev.Action+":"+ev.Host)
		} else {
			a = append(a, ev.Action)
		}
	}
	return strings.Join(a, " ")
}

func TestRolloutHappyPath(t *testing.T) {
	r := newRolloutRig(t, "a", "b", "c")
	r.start()
	r.tick()
	r.want("running a=upgrading b=pending c=pending")
	// nothing else is touched while a runs
	r.f.advance(10 * time.Second)
	r.tick()
	if len(r.f.starts) != 1 {
		t.Fatalf("starts = %v", r.f.starts)
	}
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=soaking b=pending c=pending")
	r.f.advance(time.Minute)
	r.tick()
	r.want("running a=soaking b=pending c=pending")
	r.f.advance(70 * time.Second)
	r.tick()
	r.want("running a=ok b=upgrading c=pending")
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=ok b=ok c=upgrading")
	r.f.finish("c", upgrade.RecOK, "v0.6.4")
	j := r.tick()
	r.want("done a=ok b=ok c=ok")
	if j.Finished == nil || r.stateSeq() != "running,done" {
		t.Fatalf("finish not reported: %+v %v", j.Finished, r.stateSeq())
	}
	if got, want := strings.Join(r.f.starts, ","), "a,b,c"; got != want {
		t.Fatalf("order %s", got)
	}
	for _, ev := range r.audit {
		if !strings.Contains(ev.Detail, "id="+j.ID) || ev.Todo != "t1" {
			t.Fatalf("audit entry without the rollout id or item: %+v", ev)
		}
	}
	a := r.actions()
	for _, w := range []string{"rollout.start", "rollout.step:a", "rollout.gate:a", "rollout.step:c", "rollout.finish"} {
		if !strings.Contains(a, w) {
			t.Errorf("audit lacks %s: %s", w, a)
		}
	}
	// a finished job is left alone by later ticks
	n := len(r.audit)
	r.tick()
	if len(r.audit) != n {
		t.Fatal("a done rollout wrote more audit")
	}
}

func TestRolloutStopsAtFirstFailureAndRetries(t *testing.T) {
	for _, state := range []string{upgrade.RecRolledBack, upgrade.RecFailed} {
		r := newRolloutRig(t, "a", "b", "c")
		r.start()
		r.tick()
		r.f.finish("a", upgrade.RecOK, "v0.6.4")
		r.tick()
		r.f.advance(3 * time.Minute)
		r.tick()
		r.f.finish("b", state, "")
		// b started on this tick; a later tick sees the verdict
		r.tick()
		r.tick()
		r.want("stopped a=ok b=failed c=pending")
		j, _ := r.e.Get()
		if j.Stop == nil || j.Stop.Host != "b" || !strings.Contains(j.Stop.Reason, state) || j.Plan[1].Output != "smoke output" {
			t.Fatalf("stop = %+v, step = %+v", j.Stop, j.Plan[1])
		}
		for i := 0; i < 3; i++ { // stopped is stable: c is never started
			r.f.advance(time.Minute)
			r.tick()
		}
		if len(r.f.starts) != 2 {
			t.Fatalf("a stopped rollout started more: %v", r.f.starts)
		}
		if r.stateSeq() != "running,stopped" {
			t.Fatalf("stop not reported: %s", r.stateSeq())
		}
		// Resume retries b, not a
		if _, err := r.e.Resume("jesse"); err != nil {
			t.Fatal(err)
		}
		r.want("running a=ok b=pending c=pending")
		r.tick()
		r.want("running a=ok b=upgrading c=pending")
		if got := r.f.starts; got[len(got)-1] != "b" || len(got) != 3 {
			t.Fatalf("starts %v", got)
		}
	}
}

func TestRolloutSkipsOfflineHostAndFinishesDoneWithSkip(t *testing.T) {
	r := newRolloutRig(t, "a", "b", "c")
	r.f.set("b", func(h *relHost) { h.Status = "down" })
	r.start()
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick() // a: soaking
	r.f.advance(3 * time.Minute)
	r.tick() // a ok; b offline: waits
	r.want("running a=ok b=pending c=pending")
	if len(r.f.starts) != 1 {
		t.Fatal("an offline host was started")
	}
	r.f.advance(90 * time.Second)
	r.tick()
	r.want("running a=ok b=pending c=pending") // still within the grace
	r.f.advance(40 * time.Second)
	r.tick()
	r.want("running a=ok b=skipped c=upgrading")
	r.f.finish("c", upgrade.RecOK, "v0.6.4")
	j := r.tick()
	r.want("done a=ok b=skipped c=ok")
	if !strings.Contains(j.Plan[1].Detail, "offline for") {
		t.Fatalf("detail %q", j.Plan[1].Detail)
	}
	if a := r.actions(); !strings.Contains(a, "rollout.skip:b") || !strings.Contains(r.audit[len(r.audit)-1].Detail, "1 skipped (b)") {
		t.Fatalf("audit %s / %+v", a, r.audit[len(r.audit)-1])
	}
}

func TestRolloutOfflineHostThatComesBackIsUpgraded(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.f.set("a", func(h *relHost) { h.Status = "down" })
	r.start()
	r.tick()
	r.f.advance(time.Minute)
	r.tick()
	r.want("running a=pending b=pending")
	r.f.set("a", func(h *relHost) { h.Status = "ok" })
	r.tick()
	r.want("running a=upgrading b=pending")
}

func TestRolloutAlreadyCurrentHostIsPassedWithoutSoak(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.f.set("a", func(h *relHost) { h.Version = "v0.6.4" })
	r.start("a", "b") // explicit order keeps the current host
	r.tick()
	r.want("running a=ok b=upgrading")
	if len(r.f.starts) != 1 || r.f.starts[0] != "b" {
		t.Fatalf("starts %v", r.f.starts)
	}
}

func TestRolloutAdoptsALiveRunInsteadOfStartingAnother(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.start()
	// The hub died after the peer started its run but before the job was saved.
	rec := upgrade.Record{ID: "orphan", Version: "v0.6.4", State: upgrade.RecRunning, Started: r.f.clock().Add(-20 * time.Second)}
	r.f.set("a", func(h *relHost) { h.Record = &rec })
	r.tick()
	r.want("running a=upgrading b=pending")
	j, _ := r.e.Get()
	if j.Plan[0].Run != "orphan" || len(r.f.starts) != 0 {
		t.Fatalf("run %q starts %v", j.Plan[0].Run, r.f.starts)
	}
	// A live record for another version is not adopted: the start is tried (and the host says busy).
	r2 := newRolloutRig(t, "a")
	r2.start()
	other := upgrade.Record{ID: "x", Version: "v0.6.2", State: upgrade.RecRunning, Started: r2.f.clock()}
	r2.f.set("a", func(h *relHost) { h.Record = &other })
	r2.f.startErr["a"] = &peerFault{host: "a", code: "upgrade_running", msg: "busy"}
	r2.tick()
	r2.want("running a=pending")
	r2.f.advance(6 * time.Minute)
	r2.tick()
	r2.want("stopped a=failed")
}

func TestRolloutResumesAfterARestartInEveryState(t *testing.T) {
	// Each tick is done by a brand-new engine on the same file, and the
	// outcome is the same as with one engine.
	r := newRolloutRig(t, "a", "b")
	r.start()
	step := func() { r.e = r.engine(); r.tick() }
	step()
	r.want("running a=upgrading b=pending")
	r.e = r.engine()
	r.e.resumeAfterRestart()
	if !strings.Contains(r.actions(), "rollout.resume-after-restart") {
		t.Fatalf("no resume entry: %s", r.actions())
	}
	r.f.finish("a", upgrade.RecOK, "v0.6.3") // restarted, still the old version
	step()
	r.want("running a=verifying b=pending")
	r.f.set("a", func(h *relHost) { h.Version = "v0.6.4" })
	step()
	r.want("running a=soaking b=pending")
	r.f.advance(3 * time.Minute)
	step()
	r.want("running a=ok b=upgrading")
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	step()
	r.want("done a=ok b=ok")
	if len(r.f.starts) != 2 {
		t.Fatalf("starts %v", r.f.starts)
	}
}

func TestRolloutVerifyTimesOutWhenTheVersionNeverShows(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.start()
	r.tick()
	r.f.finish("a", upgrade.RecOK, "") // ok, but the hub still sees the old version
	r.tick()
	r.want("running a=verifying b=pending")
	r.f.advance(4 * time.Minute)
	r.tick()
	r.want("running a=verifying b=pending")
	r.f.advance(70 * time.Second)
	r.tick()
	r.want("stopped a=failed b=pending")
	j, _ := r.e.Get()
	if !strings.Contains(j.Stop.Reason, "not back online on v0.6.4") || !strings.Contains(j.Stop.Reason, "v0.6.3") {
		t.Fatalf("reason %q", j.Stop.Reason)
	}
}

func TestRolloutRunWithNoVerdictStops(t *testing.T) {
	r := newRolloutRig(t, "a")
	r.start()
	r.tick()
	r.f.advance(10 * time.Minute)
	r.tick()
	r.want("running a=upgrading")
	r.f.advance(6 * time.Minute)
	r.tick()
	r.want("stopped a=failed")
}

func TestRolloutSoakWatchesTheCanary(t *testing.T) {
	for name, breakIt := range map[string]func(h *relHost){
		"offline":        func(h *relHost) { h.Status = "down" },
		"version":        func(h *relHost) { h.Version = "v0.6.3" },
		"new failed run": func(h *relHost) { h.Record = &upgrade.Record{ID: "zz", State: upgrade.RecFailed, Error: "boom"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRolloutRig(t, "a", "b")
			r.start()
			r.tick()
			r.f.finish("a", upgrade.RecOK, "v0.6.4")
			r.tick()
			r.want("running a=soaking b=pending")
			r.f.set("a", breakIt)
			r.f.advance(30 * time.Second)
			r.tick()
			r.want("stopped a=failed b=pending")
			if len(r.f.starts) != 1 {
				t.Fatalf("starts %v", r.f.starts)
			}
		})
	}
}

func TestRolloutPauseWaitsForTheStepInFlight(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.start()
	r.tick()
	j, err := r.e.Pause("jesse")
	if err != nil || !j.PauseRequested || j.State != roRunning || !strings.Contains(j.Note, "pausing after a") {
		t.Fatalf("pause: %+v %v", j, err)
	}
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.advance(3 * time.Minute)
	r.tick()
	r.want("paused a=ok b=pending")
	for i := 0; i < 3; i++ {
		r.f.advance(time.Minute)
		r.tick()
	}
	if len(r.f.starts) != 1 {
		t.Fatalf("a paused rollout started %v", r.f.starts)
	}
	if _, err := r.e.Pause("jesse"); err == nil {
		t.Fatal("pausing a paused rollout")
	}
	if _, err := r.e.Resume("jesse"); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.want("running a=ok b=upgrading")
	// no step in flight: pause is immediate
	r2 := newRolloutRig(t, "a")
	r2.start()
	if j, err := r2.e.Pause("jesse"); err != nil || j.State != roPaused {
		t.Fatalf("%+v %v", j, err)
	}
}

func TestRolloutCancelLeavesHostsAlone(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.start()
	r.tick()
	j, err := r.e.Cancel("jesse")
	if err != nil || j.State != roCancelled || j.Finished == nil {
		t.Fatalf("%+v %v", j, err)
	}
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("cancelled a=upgrading b=pending")
	if _, err := r.e.Cancel("jesse"); err == nil {
		t.Fatal("cancelling twice")
	}
	if r.stateSeq() != "running,cancelled" {
		t.Fatalf("states %s", r.stateSeq())
	}
	// a cancelled rollout is not live: a new one may start
	r.start()
}

func TestRolloutStartRefusedWhileOneIsLive(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.start()
	plan, _ := planRollout(r.f.Hosts(context.Background(), "v0.6.4"), nil, "")
	if _, err := r.e.Start("t1", "v0.6.4", "", "jesse", plan); err != errRolloutActive {
		t.Fatalf("err %v", err)
	}
	if _, err := newRolloutEngine("", r.f, nil).Start("t", "v", "", "x", plan); err == nil {
		t.Fatal("no state dir must fail")
	}
}

func TestRolloutTwoTicksAtOnceStartOneUpgrade(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.e.tick(context.Background()) }()
	}
	wg.Wait()
	if len(r.f.starts) != 1 {
		t.Fatalf("starts %v", r.f.starts)
	}
	r.want("running a=upgrading b=pending")
}

func TestRolloutStartRefusalStopsButTransientFailureWaits(t *testing.T) {
	r := newRolloutRig(t, "a")
	r.start()
	r.f.startErr["a"] = context.DeadlineExceeded
	r.tick()
	r.want("running a=pending")
	j, _ := r.e.Get()
	if !strings.Contains(j.Plan[0].Detail, "could not start yet") {
		t.Fatalf("detail %q", j.Plan[0].Detail)
	}
	r.f.startErr["a"] = &peerFault{host: "a", code: "build_not_green", msg: "the release build is failure"}
	r.tick()
	r.want("stopped a=failed")
}

func TestPlanRollout(t *testing.T) {
	hosts := []relHost{
		{Name: "hubhost", Status: "ok", Hub: true, Enabled: true},
		{Name: "a", Status: "ok", Enabled: true},
		{Name: "b", Status: "ok", Enabled: true},
		{Name: "c", Status: "ok", Enabled: true, Current: true},
		{Name: "d", Status: "down"},
	}
	names := func(p []RolloutStep) string {
		var s []string
		for _, st := range p {
			s = append(s, st.Host+"/"+st.Wave)
		}
		return strings.Join(s, " ")
	}
	p, err := planRollout(hosts, nil, "")
	if err != nil || names(p) != "a/canary b/peers d/peers" {
		t.Fatalf("default: %s %v", names(p), err)
	}
	p, _ = planRollout(hosts, nil, "b")
	if names(p) != "b/canary a/peers d/peers" {
		t.Fatalf("canary b: %s", names(p))
	}
	p, _ = planRollout(hosts, nil, "nobody") // an unknown canary is ignored
	if names(p) != "a/canary b/peers d/peers" {
		t.Fatalf("%s", names(p))
	}
	p, err = planRollout(hosts, []string{"b", "a"}, "")
	if err != nil || names(p) != "b/canary a/peers" {
		t.Fatalf("explicit: %s %v", names(p), err)
	}
	p, _ = planRollout(hosts, []string{"b"}, "") // one host is no canary: no soak for it
	if names(p) != "b/peers" {
		t.Fatalf("lone host: %s", names(p))
	}
	for _, bad := range [][]string{{"zz"}, {"a", "a"}, {"hubhost"}} {
		if _, err := planRollout(hosts, bad, ""); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
	hosts[2].Enabled = false
	if _, err := planRollout(hosts, nil, ""); err == nil || !strings.Contains(err.Error(), "b") {
		t.Fatalf("not enabled: %v", err)
	}
}

// ---- the hub wave ----

func newHubRig(t *testing.T, peers ...string) *rolloutRig {
	r := newRolloutRig(t, append(append([]string{}, peers...), "hub")...)
	r.f.set("hub", func(h *relHost) { h.Hub = true; h.Local = true })
	return r
}

func (r *rolloutRig) startWithHub(order ...string) {
	r.t.Helper()
	hosts := r.f.Hosts(context.Background(), "v0.6.4")
	plan, err := planRollout(hosts, order, "")
	if err != nil {
		r.t.Fatal(err)
	}
	hub, err := hubWave(hosts, true)
	if err != nil || hub == nil {
		r.t.Fatal(hub, err)
	}
	if _, err := r.e.Start("t1", "v0.6.4", "", "jesse", append(plan, *hub)); err != nil {
		r.t.Fatal(err)
	}
}

// peersDone runs the peers of the rig through their steps, with no soak.
func (r *rolloutRig) peersDone(peers ...string) {
	r.e.soak = 0
	for _, p := range peers {
		r.tick()
		r.f.finish(p, upgrade.RecOK, "v0.6.4")
		r.tick()
	}
}

func TestHubWaveRunsLastAndFinishes(t *testing.T) {
	r := newHubRig(t, "a", "b")
	r.startWithHub("a", "b")
	r.peersDone("a", "b")
	r.want("running a=ok b=ok hub=upgrading") // the hub wave starts the moment the last peer is ok
	if r.f.deploys != 1 || len(r.f.starts) != 2 {
		t.Fatalf("deploys %d starts %v", r.f.deploys, r.f.starts)
	}
	r.tick() // the deploy is still running: nothing else happens
	r.want("running a=ok b=ok hub=upgrading")
	r.f.endDeploy(depOK, "", "")
	r.tick()
	r.want("running a=ok b=ok hub=verifying")
	// the hub is not yet reporting the version: it waits
	r.f.advance(time.Minute)
	r.tick()
	r.want("running a=ok b=ok hub=verifying")
	r.f.set("hub", func(h *relHost) { h.Version = "v0.6.4" })
	r.tick()
	r.want("done a=ok b=ok hub=ok")
	if r.f.deploys != 1 {
		t.Fatal("deployed twice")
	}
}

func TestHubWaveSurvivesTheHubRestartingInsideIt(t *testing.T) {
	r := newHubRig(t, "a")
	r.startWithHub("a")
	r.peersDone("a")
	r.want("running a=ok hub=upgrading")
	// The deploy ends and the hub pod is replaced: a new process, the same file.
	r.f.endDeploy(depOK, "", "v0.6.4")
	r.e = r.engine()
	r.e.resumeAfterRestart()
	r.tick()
	r.want("done a=ok hub=ok")
	if r.f.deploys != 1 {
		t.Fatalf("deploys %d", r.f.deploys)
	}
	if !strings.Contains(r.actions(), "rollout.resume-after-restart") {
		t.Fatal(r.actions())
	}
	// And a restart before the deploy record has an answer (the release host
	// is asked again) does not start a second deploy.
	r2 := newHubRig(t, "a")
	r2.startWithHub("a")
	r2.peersDone("a")
	r2.e = r2.engine()
	r2.f.statusErr = context.DeadlineExceeded
	r2.tick()
	r2.want("running a=ok hub=upgrading")
	r2.f.statusErr = nil
	r2.tick()
	if r2.f.deploys != 1 {
		t.Fatalf("deploys %d", r2.f.deploys)
	}
}

func TestHubWaveAdoptsARunningDeploy(t *testing.T) {
	r := newHubRig(t, "a")
	r.startWithHub("a")
	r.e.soak = 0
	r.tick()
	r.f.deploy = &DeployRecord{Version: "v0.6.4", State: depRunning, Started: r.f.clock().Add(-10 * time.Second)}
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=ok hub=upgrading")
	if r.f.deploys != 0 {
		t.Fatal("started a second deploy")
	}
}

func TestHubWaveFailureStopsWithTheDeployOutput(t *testing.T) {
	r := newHubRig(t, "a")
	r.startWithHub("a")
	r.peersDone("a")
	r.f.endDeploy(depFailed, "exit status 1", "")
	r.tick()
	r.want("stopped a=ok hub=failed")
	j, _ := r.e.Get()
	if !strings.Contains(j.Stop.Reason, "the deploy failed: bump: FAILED") || j.Plan[1].Output == "" {
		t.Fatalf("stop %+v / %q", j.Stop, j.Plan[1].Output)
	}
	// Retry runs the deploy again
	if _, err := r.e.Resume("jesse"); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.want("running a=ok hub=upgrading")
	if r.f.deploys != 2 {
		t.Fatalf("deploys %d", r.f.deploys)
	}
}

func TestHubWaveStopsWhenTheHubComesBackOnTheOldVersion(t *testing.T) {
	r := newHubRig(t, "a")
	r.startWithHub("a")
	r.peersDone("a")
	r.f.endDeploy(depOK, "", "")
	r.tick()
	r.f.advance(6 * time.Minute)
	r.tick()
	r.want("stopped a=ok hub=failed")
	j, _ := r.e.Get()
	if !strings.Contains(j.Stop.Reason, "not back online on v0.6.4") {
		t.Fatalf("%q", j.Stop.Reason)
	}
}

func TestHubWaveWaitsForAPeerThatWentAway(t *testing.T) {
	r := newHubRig(t, "a", "b")
	r.startWithHub("a", "b")
	r.e.soak = 0
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.set("a", func(h *relHost) { h.Status = "down" })
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=ok b=ok hub=pending")
	if r.f.deploys != 0 {
		t.Fatal("deployed over a missing peer")
	}
	r.f.set("a", func(h *relHost) { h.Status = "ok" })
	r.tick()
	r.want("running a=ok b=ok hub=upgrading")
	// and it does not wait forever
	r2 := newHubRig(t, "a", "b")
	r2.startWithHub("a", "b")
	r2.e.soak = 0
	r2.tick()
	r2.f.finish("a", upgrade.RecOK, "v0.6.4")
	r2.tick()
	r2.f.set("a", func(h *relHost) { h.Status = "down" })
	r2.f.finish("b", upgrade.RecOK, "v0.6.4")
	r2.tick()
	r2.f.advance(6 * time.Minute)
	r2.tick()
	r2.want("stopped a=ok b=ok hub=failed")
}

func TestHubWaveStillRunsWhenAPeerWasSkipped(t *testing.T) {
	r := newHubRig(t, "a", "b")
	r.f.set("b", func(h *relHost) { h.Status = "down" })
	r.startWithHub("a", "b")
	r.e.soak = 0
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.advance(3 * time.Minute)
	r.tick()
	r.want("running a=ok b=skipped hub=upgrading")
}

func TestHubWaveRestartedDeployOnTheHubItselfDoesNotFailIt(t *testing.T) {
	r := newHubRig(t, "a")
	r.startWithHub("a")
	r.peersDone("a")
	r.f.endDeploy(depFailed, "the deploy stopped without a verdict (this server restarted while it ran)", "v0.6.4")
	r.tick()
	r.want("done a=ok hub=ok")
}

func TestHubWaveOnlyAndNoWave(t *testing.T) {
	r := newHubRig(t, "a")
	r.f.set("a", func(h *relHost) { h.Version = "v0.6.4" }) // the peer is there already
	hosts := r.f.Hosts(context.Background(), "v0.6.4")
	plan, _ := planRollout(hosts, []string{}, "")
	hub, err := hubWave(hosts, true)
	if len(plan) != 0 || err != nil || hub == nil || hub.Mode != stepModeDeploy || hub.Wave != waveHub {
		t.Fatalf("%v %v %v", plan, hub, err)
	}
	// a hub already on the version has no wave
	r.f.set("hub", func(h *relHost) { h.Version = "v0.6.4" })
	if hub, err := hubWave(r.f.Hosts(context.Background(), "v0.6.4"), true); hub != nil || err != nil {
		t.Fatalf("%v %v", hub, err)
	}
	// no deploy command and no -release-upgrade: refused, with the reason
	r.f.set("hub", func(h *relHost) { h.Version = "v0.6.3"; h.Enabled = false })
	if _, err := hubWave(r.f.Hosts(context.Background(), "v0.6.4"), false); err == nil {
		t.Fatal("a hub that cannot be upgraded was planned")
	}
}

func TestHubWaveAsAnOrdinaryHostUpgrade(t *testing.T) {
	// A single-host hub with -release-upgrade: its step is an upgrade like any
	// other, last, and survives its restart through the upgrade record.
	r := newHubRig(t, "a")
	hosts := r.f.Hosts(context.Background(), "v0.6.4")
	plan, _ := planRollout(hosts, []string{"a"}, "")
	hub, _ := hubWave(hosts, false)
	if hub.Mode != "" {
		t.Fatal(hub.Mode)
	}
	if _, err := r.e.Start("t1", "v0.6.4", "", "jesse", append(plan, *hub)); err != nil {
		t.Fatal(err)
	}
	r.peersDone("a")
	r.tick()
	r.want("running a=ok hub=upgrading")
	r.e = r.engine() // the hub restarted by its own upgrade
	r.f.finish("hub", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.tick()
	r.want("done a=ok hub=ok")
}
