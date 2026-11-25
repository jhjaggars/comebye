package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- the release item closes itself; hosts behind catch up (issue #141 slice 4) ----

// sweepHosts is a fleet answer with a hub on v0.5.9 and the given peers.
func sweepHosts(peers ...relHost) []relHost {
	return append([]relHost{{Name: "hub", Local: true, Hub: true, Status: "ok", Version: "v0.5.9", Enabled: true}}, peers...)
}

func itemOf(t *testing.T, s *Server, id string) Todo {
	t.Helper()
	it, ok := s.todos.get(id)
	if !ok {
		t.Fatal("no item")
	}
	return it
}

func TestSweepClosesTheItemNamingTheHostsBehind(t *testing.T) {
	s, _, _, it, _ := rolloutRouteRig(t, "a")
	s.sweepReleasesWith(sweepHosts(
		relHost{Name: "a", Status: "ok", Version: "v0.5.9", Enabled: true},
		relHost{Name: "wk1", Status: "down", Version: "v0.5.8", Stopped: true},
		relHost{Name: "mac", Status: "down", Version: "v0.5.8"},
	))
	got := itemOf(t, s, it.ID)
	if got.State != TodoDone || got.ClosedBy != "comebye" {
		t.Fatalf("state %s by %q", got.State, got.ClosedBy)
	}
	if n := got.Release.RolloutNote; !strings.Contains(n, "behind: wk1 (stopped), mac (offline)") {
		t.Fatalf("note %q", n)
	}
	found := false
	for _, ev := range s.ledger.AuditLog(it.ID) {
		found = found || (ev.Action == "release.complete" && strings.Contains(ev.Detail, "behind: wk1 (stopped), mac (offline)"))
	}
	if !found {
		t.Fatal("no release.complete audit naming them")
	}
}

func TestSweepKeepsTheItemOpen(t *testing.T) {
	cases := []struct {
		name  string
		hosts []relHost
		setup func(s *Server)
	}{
		{"a reachable host behind", sweepHosts(
			relHost{Name: "a", Status: "ok", Version: "v0.5.8", Enabled: true},
			relHost{Name: "wk1", Status: "down", Version: "v0.5.8", Stopped: true}), nil},
		{"the hub behind", []relHost{
			{Name: "hub", Local: true, Hub: true, Status: "ok", Version: "v0.5.8"},
			{Name: "a", Status: "ok", Version: "v0.5.9", Enabled: true}}, nil},
		{"the provisioner offline behind", sweepHosts(
			relHost{Name: "prov", Status: "down", Version: "v0.5.8", WithHub: true}), nil},
		{"the automatic rollout off", sweepHosts(
			relHost{Name: "wk1", Status: "down", Version: "v0.5.8", Stopped: true}),
			func(s *Server) { do(s, "PUT", "/api/settings", userHdr, `{"auto_rollout":false}`) }},
		{"a rollout still live", sweepHosts(
			relHost{Name: "wk1", Status: "down", Version: "v0.5.8", Stopped: true}),
			func(s *Server) {
				if _, err := s.rollout.Start("tx", "v0.5.9", "", "jesse", []RolloutStep{{Host: "a", Wave: wavePeers, Phase: roPending}}); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, _, it, _ := rolloutRouteRig(t, "a")
			if c.setup != nil {
				c.setup(s)
			}
			s.sweepReleasesWith(c.hosts)
			if got := itemOf(t, s, it.ID); !got.active() {
				t.Fatalf("closed: %s", got.State)
			}
		})
	}
}

// v0.5.58: every running host was current, so the automatic rollout had
// nothing to do and made no job. A stopped host started later, more than a
// week on, is still caught up.
func TestCatchUpWithNoRolloutJobAfterMoreThanAWeek(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "wk1")
	ctx := context.Background()
	f.set("a", func(h *relHost) { h.Version = "v0.5.9" })
	f.set("wk1", func(h *relHost) { h.Status, h.Stopped = "down", true })
	if got := s.autoRolloutTick(ctx); got != "nothing to do" {
		t.Fatalf("auto tick = %q", got)
	}
	if _, ok := s.rollout.Get(); ok {
		t.Fatal("a job was saved")
	}
	if got := s.autoCatchUpTick(ctx); got != "current" {
		t.Fatalf("catch-up while stopped = %q", got)
	}
	// The item closed (all reachable current), and eight days pass.
	s.sweepReleasesWith(sweepHosts(relHost{Name: "a", Status: "ok", Version: "v0.5.9"}, relHost{Name: "wk1", Status: "down", Version: "v0.5.8", Stopped: true}))
	if itemOf(t, s, it.ID).active() {
		t.Fatal("the item is still open")
	}
	_, _ = s.todos.mutate(it.ID, func(t *Todo) error {
		rel := *t.Release
		old := rel.TaggedAt.Add(-8 * 24 * time.Hour)
		rel.TaggedAt = &old
		t.Release = &rel
		return nil
	})
	f.advance(8 * 24 * time.Hour)
	// wk1 starts, on the old version.
	f.set("wk1", func(h *relHost) { h.Status, h.Stopped = "ok", false })
	if got := s.autoCatchUpTick(ctx); got != "wait" {
		t.Fatalf("within the interval = %q", got)
	}
	s.lastAutoCatchUp = time.Time{}
	got := s.autoCatchUpTick(ctx)
	if !strings.HasPrefix(got, "started r") {
		t.Fatalf("catch-up tick = %q", got)
	}
	j, _ := s.rollout.Get()
	if !j.Auto || j.Todo != it.ID || len(j.Plan) != 1 || j.Plan[0].Host != "wk1" {
		t.Fatalf("job %+v", j)
	}
	s.rollout.tick(ctx)
	if len(f.starts) != 1 || f.starts[0] != "wk1" {
		t.Fatalf("starts %v", f.starts)
	}
	f.finish("wk1", upgrade.RecOK, "v0.5.9")
	s.rollout.tick(ctx)
	s.rollout.tick(ctx)
	if j, _ := s.rollout.Get(); j.State != roDone {
		t.Fatalf("job %s %s", j.State, j.Note)
	}
	// Caught up: nothing more, and never a second job.
	s.lastAutoCatchUp = time.Time{}
	if got := s.autoCatchUpTick(ctx); got != "current" || autoStarts(s) != 1 {
		t.Fatalf("after = %q starts=%d", got, autoStarts(s))
	}
}

// A stopped host skipped by the automatic rollout is caught up by its job
// when it starts, also more than a week after the job finished; a worker
// that came up on the old version, not in the plan at all, is too.
func TestCatchUpOfTheJobHasNoWeekLimitWhileAutoIsOn(t *testing.T) {
	s, f, _, _, _ := rolloutRouteRig(t, "a", "wk1")
	s.rollout.catchUpAlways = s.autoRolloutOn
	s.rollout.catchUpEvery = 0
	ctx := context.Background()
	f.set("wk1", func(h *relHost) { h.Status, h.Stopped = "down", true })
	if got := s.autoRolloutTick(ctx); !strings.HasPrefix(got, "started ") {
		t.Fatalf("auto tick = %q", got)
	}
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecOK, "v0.5.9")
	for range 3 {
		s.rollout.tick(ctx)
	}
	j, _ := s.rollout.Get()
	if j.State != roDone || j.Plan[1].Phase != roSkipped || !j.Plan[1].CatchUp {
		t.Fatalf("job %s %+v", j.State, j.Plan)
	}
	// Stopped-only skips finish ok.
	var fin bool
	for _, ev := range s.ledger.AuditLog("") {
		if ev.Action == "rollout.finish" {
			fin = ev.OK && strings.Contains(ev.Detail, "ok: 1 of 2 hosts upgraded, 1 behind until they are back (wk1)")
		}
	}
	if !fin {
		t.Fatal("the finish is not ok, or does not name wk1")
	}
	f.advance(rolloutCatchUpFor + 24*time.Hour)
	f.set("wk1", func(h *relHost) { h.Status, h.Stopped = "ok", false })
	s.rollout.tick(ctx) // the engine's own catch-up
	if j, _ := s.rollout.Get(); j.State != roRunning || j.Plan[1].Phase != roPending {
		t.Fatalf("not reopened after 8 days: %s %+v", j.State, j.Plan[1])
	}
	s.rollout.tick(ctx)
	f.finish("wk1", upgrade.RecOK, "v0.5.9")
	for range 3 {
		s.rollout.tick(ctx)
	}
	if j, _ := s.rollout.Get(); j.State != roDone || j.Plan[1].Phase != roOK {
		t.Fatalf("wk1 not caught up: %s %+v", j.State, j.Plan[1])
	}
	// A new worker, made on the old version.
	f.hosts["wk"] = &relHost{Name: "wk", Status: "ok", Version: "v0.5.8", Enabled: true}
	f.order = append(f.order, "wk")
	s.lastAutoCatchUp = time.Time{}
	if got := s.autoCatchUpTick(ctx); got != "caught up wk" {
		t.Fatalf("new worker tick = %q", got)
	}
	s.rollout.tick(ctx)
	if f.starts[len(f.starts)-1] != "wk" {
		t.Fatalf("starts %v", f.starts)
	}
}

// With the automatic rollout off, the week's limit stands.
func TestCatchUpKeepsTheWeekLimitWhenAutoIsOff(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.canary = ""
	r.e.catchUpAlways = func() bool { return false }
	r.e.catchUpEvery = 0
	r.f.set("b", func(h *relHost) { h.Status = "down" })
	r.start()
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.tick()
	r.f.advance(r.e.offlineGrace)
	r.tick()
	r.tick()
	r.want("done a=ok b=skipped")
	r.f.advance(rolloutCatchUpFor + time.Hour)
	r.f.set("b", func(h *relHost) { h.Status = "ok" })
	r.tick()
	r.want("done a=ok b=skipped")
	r.e.catchUpAlways = func() bool { return true }
	r.tick()
	r.want("running a=ok b=pending")
}

// A skip for a real reason (the hub) is still not ok, beside an expected one.
func TestFinishWithAHubSkipIsStillNotOk(t *testing.T) {
	j := Rollout{Plan: []RolloutStep{
		{Host: "wk1", Wave: wavePeers, Phase: roSkipped, CatchUp: true},
		{Host: "hub", Wave: waveHub, Mode: stepModeDeploy, Phase: roSkipped},
	}}
	if behindOnly(j) != nil {
		t.Fatal("a hub skip read as expected")
	}
	j.Plan[1] = RolloutStep{Host: "b", Wave: wavePeers, Phase: roSkipped, Detail: "the app is not allowed to upgrade it"}
	if behindOnly(j) != nil {
		t.Fatal("a not-allowed skip read as expected")
	}
	j.Plan = j.Plan[:1]
	if got := behindOnly(j); len(got) != 1 || got[0] != "wk1" {
		t.Fatalf("behind %v", got)
	}
	if n := rolloutNoteOf(Rollout{Version: "v1.0.0", State: roDone, Plan: j.Plan}); !strings.Contains(n, "behind until they are back: wk1") {
		t.Fatalf("note %q", n)
	}
}

// The newest tagged release item outlives the retention: the catch-up
// upgrades against it.
func TestPruneKeepsTheNewestTaggedRelease(t *testing.T) {
	s, _, _, it, _ := rolloutRouteRig(t, "a")
	if _, err := s.todos.close(it.ID, TodoDone, "comebye", ""); err != nil {
		t.Fatal(err)
	}
	s.todos.now = func() time.Time { return time.Now().Add(todoRetention + 24*time.Hour) }
	s.todos.mu.Lock()
	s.todos.pruneLocked()
	s.todos.mu.Unlock()
	if _, ok := s.todos.get(it.ID); !ok {
		t.Fatal("pruned")
	}
}
