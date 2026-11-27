package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- a host whose upgrade fails after the canary stops holding up rollouts (issue #141) ----

// skipFailRig is a ledger hub with the real alarms wired, a fake fleet of
// peers, and the canary a, which soaks for no time.
func skipFailRig(t *testing.T, names ...string) (*Server, *fakeFleet, Todo) {
	t.Helper()
	s, f, _, it, _ := rolloutRouteRig(t, names...)
	s.rollout.onSkipFail = s.alarmSkipFail
	s.rollout.onSoakFail = s.alarmSoak
	s.rollout.soak = 0
	plan, err := planRollout(f.Hosts(context.Background(), "v0.5.9"), nil, "a")
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Wave != waveCanary {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := s.rollout.Start(it.ID, "v0.5.9", relSHA, "jesse", plan); err != nil {
		t.Fatal(err)
	}
	return s, f, it
}

func roPhases(s *Server) string {
	j, _ := s.rollout.Get()
	var p []string
	for _, st := range j.Plan {
		p = append(p, st.Host+"="+st.Phase)
	}
	return j.State + " " + strings.Join(p, " ")
}

func rolloutAlarms(s *Server) []Todo {
	var out []Todo
	for _, a := range s.todos.list("") {
		if a.active() && a.Source == SourceRollout {
			out = append(out, a)
		}
	}
	return out
}

func TestRolloutSkipsAPeerThatFailsAfterTheCanaryAndGoesOn(t *testing.T) {
	s, f, it := skipFailRig(t, "a", "b", "c", "d")
	ctx := context.Background()
	tick := func() { s.rollout.tick(ctx) }
	want := func(w string) {
		t.Helper()
		if got := roPhases(s); got != w {
			t.Fatalf("got  %s\nwant %s", got, w)
		}
	}

	tick()
	f.finish("a", upgrade.RecOK, "v0.5.9")
	tick()
	want("running a=ok b=upgrading c=pending d=pending")

	// b's upgrade is rolled back: the host's own record settles first (the
	// page or the sweep reads it), and files nothing of its own.
	f.finish("b", upgrade.RecRolledBack, "")
	bRec := *f.hosts["b"].Record
	s.alarmUpgrade("b", bRec)
	if a := rolloutAlarms(s); len(a) != 0 {
		t.Fatalf("a per-host alarm was filed: %+v", a)
	}
	tick()
	want("running a=ok b=skipped c=pending d=pending")
	tick()
	want("running a=ok b=skipped c=upgrading d=pending")

	// c fails at the start: a refusal the host gave.
	j, _ := s.rollout.Get()
	if st := j.Plan[1]; !st.Failed || st.CatchUp || !strings.Contains(st.Detail, "failed: rolled_back") || st.Output != "smoke output" {
		t.Fatalf("b = %+v", st)
	}
	f.finish("c", upgrade.RecFailed, "")
	s.alarmUpgrade("c", *f.hosts["c"].Record)
	tick()
	want("running a=ok b=skipped c=skipped d=pending")
	tick()
	f.finish("d", upgrade.RecOK, "v0.5.9")
	tick()
	want("done a=ok b=skipped c=skipped d=ok")

	// Exactly one To do, naming both hosts and what failed.
	al := rolloutAlarms(s)
	if len(al) != 1 {
		t.Fatalf("alarms = %d: %+v", len(al), al)
	}
	if q := al[0].Question; !strings.Contains(q, "v0.5.9 rollout skipped b, c") {
		t.Fatalf("question = %q", q)
	}
	if c := al[0].Context; !strings.Contains(c, "**b**: rolled back: smoke: journal has errors") || !strings.Contains(c, "**c**: failed: smoke") || !strings.Contains(c, "Roll out again") {
		t.Fatalf("context = %q", c)
	}
	// Settling the same records again, or the job's hook again, files nothing more.
	s.alarmUpgrade("b", bRec)
	j, _ = s.rollout.Get()
	s.alarmSkipFail(j)
	if n := len(rolloutAlarms(s)); n != 1 {
		t.Fatalf("alarms = %d", n)
	}

	// The job finished not ok, and the item says to roll it out again.
	found := false
	for _, ev := range s.ledger.AuditLog(it.ID) {
		if ev.Action == "rollout.finish" {
			found = !ev.OK && strings.Contains(ev.Detail, "2 skipped (b, c)")
		}
	}
	if !found {
		t.Fatal("no rollout.finish not ok")
	}
	if n := itemOf(t, s, it.ID).Release.RolloutNote; !strings.Contains(n, "b, c skipped") || !strings.Contains(n, "roll it out again") {
		t.Fatalf("note = %q", n)
	}

	// A failed host is not caught up by this release: it would loop.
	f.advance(10 * time.Minute)
	if got := s.rollout.CatchUpNow(ctx); len(got) != 0 {
		t.Fatalf("caught up %v", got)
	}
	tick()
	want("done a=ok b=skipped c=skipped d=ok")
	if len(f.starts) != 4 {
		t.Fatalf("starts = %v", f.starts)
	}
}

func TestRolloutCanaryFailureStillStops(t *testing.T) {
	s, f, _ := skipFailRig(t, "a", "b", "c")
	ctx := context.Background()
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecRolledBack, "")
	s.rollout.tick(ctx)
	if got := roPhases(s); got != "stopped a=failed b=pending c=pending" {
		t.Fatalf("got %s", got)
	}
	// Its own per-host alarm is filed, as before.
	s.alarmUpgrade("a", *f.hosts["a"].Record)
	al := rolloutAlarms(s)
	if len(al) != 1 || !strings.Contains(al[0].Question, "upgrade of a failed") {
		t.Fatalf("alarms = %+v", al)
	}
	if len(f.starts) != 1 {
		t.Fatalf("starts = %v", f.starts)
	}
}

// The canary skipped while offline proved nothing: a later failure stops.
func TestRolloutFailureStopsWhenTheCanaryWasSkipped(t *testing.T) {
	s, f, _ := skipFailRig(t, "a", "b", "c")
	ctx := context.Background()
	f.set("a", func(h *relHost) { h.Status = "down" })
	s.rollout.tick(ctx)
	f.advance(3 * time.Minute)
	s.rollout.tick(ctx)
	s.rollout.tick(ctx)
	f.finish("b", upgrade.RecRolledBack, "")
	s.rollout.tick(ctx)
	if got := roPhases(s); got != "stopped a=skipped b=failed c=pending" {
		t.Fatalf("got %s", got)
	}
}

// A failure of the hub wave after the canary still stops the rollout.
func TestRolloutHubFailureAfterTheCanaryStillStops(t *testing.T) {
	r := newHubRig(t, "a", "b")
	r.e.soak = 0
	hosts := r.f.Hosts(context.Background(), "v0.6.4")
	plan, err := rolloutPlan(hosts, nil, "a", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Start("t1", "v0.6.4", "", "jesse", plan); err != nil {
		t.Fatal(err)
	}
	r.peersDone("a", "b")
	r.f.endDeploy(depFailed, "exit status 1", "")
	r.tick()
	r.want("stopped a=ok b=ok hub=failed")
}

// ---- a newer release's rollout closes the older items that never started one ----

func TestNewerRolloutClosesOlderUnstartedReleaseItems(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()
	tagged := func(v string) Todo {
		o := newRelease(t, s, v, relSHA)
		if _, err := s.todos.mutate(o.ID, func(t *Todo) error {
			rel := *t.Release
			rel.State = relTagged
			t.Release = &rel
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return o
	}
	// v0.5.7's rollout stopped; v0.5.8 was tagged meanwhile and never got
	// one (the stop held it back); v0.5.9 is due.
	for _, n := range []string{"a", "b"} {
		f.set(n, func(h *relHost) { h.Version = "v0.5.6" })
	}
	stopped := tagged("v0.5.7")
	never := tagged("v0.5.8")
	plan, err := planRollout(f.Hosts(ctx, "v0.5.7"), []string{"a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.rollout.Start(stopped.ID, "v0.5.7", relSHA, "jesse", plan); err != nil {
		t.Fatal(err)
	}
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecFailed, "")
	s.rollout.tick(ctx)
	if j, _ := s.rollout.Get(); j.State != roStopped {
		t.Fatalf("old job = %s", j.State)
	}

	if got := s.autoRolloutTick(ctx); !strings.HasPrefix(got, "started r") {
		t.Fatalf("tick = %q", got)
	}
	o := itemOf(t, s, never.ID)
	if o.State != TodoDone || !strings.Contains(o.Release.RolloutNote, "v0.5.8 was never rolled out: superseded by v0.5.9") {
		t.Fatalf("never-started item %s %q", o.State, o.Release.RolloutNote)
	}
	if o := itemOf(t, s, stopped.ID); o.State != TodoDone || !strings.Contains(o.Release.RolloutNote, "superseded by v0.5.9") {
		t.Fatalf("stopped item %s %q", o.State, o.Release.RolloutNote)
	}
	if o := itemOf(t, s, it.ID); !o.active() {
		t.Fatal("the newer release's item was closed")
	}
	n := 0
	for _, ev := range s.ledger.AuditLog(never.ID) {
		if ev.Action == "release.superseded" && strings.Contains(ev.Detail, "no rollout had started") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("release.superseded audits = %d", n)
	}
}

// A tapped Roll out to all closes them too.
func TestTappedRolloutClosesOlderUnstartedReleaseItems(t *testing.T) {
	s, _, _, it, tok := rolloutRouteRig(t, "a", "b")
	older := newRelease(t, s, "v0.5.8", relSHA)
	newer := newRelease(t, s, "v0.5.10", relSHA) // newer than the one rolled out: left alone
	w := rec(do(s, "POST", "/api/release/rollout", withCSRF(tok), startBody(it, `["a","b"]`)))
	if w.Code != 202 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	if o := itemOf(t, s, older.ID); o.State != TodoDone || !strings.Contains(o.Release.RolloutNote, "superseded by v0.5.9") {
		t.Fatalf("older item %s %q", o.State, o.Release.RolloutNote)
	}
	if o := itemOf(t, s, newer.ID); !o.active() {
		t.Fatal("a newer item was closed")
	}
	if o := itemOf(t, s, it.ID); !o.active() {
		t.Fatal("the rolled-out item was closed")
	}
}

// ---- the hub's step before the deploy: a peer skipped as offline does not hold it ----

func TestHubStepProceedsWithAPeerSkippedAsOffline(t *testing.T) {
	r := newHubRig(t, "a", "b", "c")
	r.f.set("b", func(h *relHost) { h.Status = "down" }) // a worker powered off when idle
	r.startWithHub("a", "b", "c")
	r.e.soak = 0
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=ok b=pending c=pending hub=pending")
	r.f.advance(3 * time.Minute)
	r.tick()
	r.tick()
	r.want("running a=ok b=skipped c=upgrading hub=pending")
	// a, proven on the version, powers off too before the deploy.
	r.f.set("a", func(h *relHost) { h.Status = "down" })
	r.f.finish("c", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=ok b=skipped c=ok hub=upgrading")
	if r.f.deploys != 1 {
		t.Fatalf("deploys = %d", r.f.deploys)
	}
	// Long after, the hub's step is not stopped by them.
	r.f.endDeploy(depOK, "", "v0.6.4")
	r.f.advance(6 * time.Minute)
	r.tick()
	r.tick()
	j, _ := r.e.Get()
	if j.State != roDone || j.Plan[3].Phase != roOK {
		t.Fatalf("job = %s %+v", j.State, j.Plan[3])
	}
	if !strings.Contains(j.Plan[3].Detail, "not reachable now") || !strings.Contains(j.Plan[3].Detail, "a") {
		t.Fatalf("hub detail = %q", j.Plan[3].Detail)
	}
}
