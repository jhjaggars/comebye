package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/release"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- the automatic rollout (issue #141 slice 1) ----

func autoStarts(s *Server) int {
	n := 0
	for _, ev := range s.ledger.AuditLog("") {
		if ev.Action == "rollout.start" {
			n++
		}
	}
	return n
}

func autoMark(t *testing.T, s *Server, id string) string {
	t.Helper()
	it, ok := s.todos.get(id)
	if !ok || it.Release == nil {
		t.Fatal("no release item")
	}
	return it.Release.AutoRollout
}

func TestAutoRolloutStartsOnceForAnApprovedGreenRelease(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()
	got := s.autoRolloutTick(ctx)
	if !strings.HasPrefix(got, "started r") {
		t.Fatalf("tick = %q", got)
	}
	j, ok := s.rollout.Get()
	if !ok || !j.Auto || j.Todo != it.ID || j.Version != "v0.5.9" || j.State != roRunning || len(j.Plan) != 2 || j.By != autoRolloutBy {
		t.Fatalf("job = %+v", j)
	}
	if m := autoMark(t, s, it.ID); m != j.ID {
		t.Fatalf("mark = %q, want %s", m, j.ID)
	}
	// The same rollout Roll out to all runs: the engine upgrades the peers.
	s.rollout.tick(ctx)
	if len(f.starts) != 1 || f.starts[0] != "a" {
		t.Fatalf("starts = %v", f.starts)
	}
	// Never twice: not while it runs, nor once it is done.
	for range 3 {
		if got := s.autoRolloutTick(ctx); got != "none" {
			t.Fatalf("second tick = %q", got)
		}
	}
	f.finish("a", upgrade.RecOK, "v0.5.9")
	s.rollout.tick(ctx)
	s.rollout.tick(ctx)
	f.finish("b", upgrade.RecOK, "v0.5.9")
	s.rollout.tick(ctx)
	s.rollout.tick(ctx)
	if j, _ := s.rollout.Get(); j.State != roDone {
		t.Fatalf("job = %s %s", j.State, j.Note)
	}
	if got := s.autoRolloutTick(ctx); got != "none" || autoStarts(s) != 1 {
		t.Fatalf("after done = %q, starts=%d", got, autoStarts(s))
	}
	var auto int
	for _, ev := range s.ledger.AuditLog(it.ID) {
		if ev.Action == "rollout.auto" {
			auto++
		}
	}
	if auto != 1 {
		t.Fatalf("rollout.auto entries = %d", auto)
	}
}

func TestAutoRolloutWaitsForAGreenBuild(t *testing.T) {
	s, _, tool, it, _ := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()
	for _, state := range []string{"failure", "in_progress", "cancelled"} {
		tool.mu.Lock()
		tool.build = release.Build{State: state}
		tool.mu.Unlock()
		s.rel.cmu.Lock()
		delete(s.rel.checks, "build:"+it.ID) // the cached answer is not the point here
		s.rel.cmu.Unlock()
		if got := s.autoRolloutTick(ctx); got != "build "+state {
			t.Fatalf("%s: tick = %q", state, got)
		}
		if _, ok := s.rollout.Get(); ok {
			t.Fatalf("%s: a job was started", state)
		}
		if m := autoMark(t, s, it.ID); m != "" {
			t.Fatalf("%s: marked %q", state, m)
		}
	}
	if autoStarts(s) != 0 {
		t.Fatal("a rollout started on a build that is not green")
	}
}

func TestAutoRolloutSettingOff(t *testing.T) {
	s, _, _, it, _ := rolloutRouteRig(t, "a", "b")
	if !s.autoRolloutOn() {
		t.Fatal("the automatic rollout is not on by default")
	}
	w := do(s, "PUT", "/api/settings", userHdr, `{"auto_rollout":false}`)
	if w.Code != 200 {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	if got := s.autoRolloutTick(context.Background()); got != "off" {
		t.Fatalf("tick = %q", got)
	}
	if _, ok := s.rollout.Get(); ok || autoMark(t, s, it.ID) != "" {
		t.Fatal("a rollout started with the setting off")
	}
	// The view says so; a PUT without the field keeps it.
	if w := do(s, "PUT", "/api/settings", userHdr, `{}`); w.Code != 200 || s.autoRolloutOn() {
		t.Fatalf("put {} = %d on=%v", w.Code, s.autoRolloutOn())
	}
	if v := do(s, "GET", "/api/settings", userHdr, ""); !strings.Contains(v.Body.String(), `"auto_rollout":{"enabled":false,"hub":true}`) {
		t.Fatalf("view = %s", v.Body)
	}
	// Back on: the release that waited is rolled out.
	do(s, "PUT", "/api/settings", userHdr, `{"auto_rollout":true}`)
	if got := s.autoRolloutTick(context.Background()); !strings.HasPrefix(got, "started ") {
		t.Fatalf("on again = %q", got)
	}
}

func TestAutoRolloutSurvivesARestart(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()
	path := s.rollout.path
	restart := func() {
		e := newRolloutEngine(path, f, s.auditBy)
		e.now = f.clock
		e.onState = s.noteRollout
		s.rollout = e
	}
	// The hub restarts between the tag and the start: the item is still
	// there, unmarked, and the next look starts it.
	restart()
	if got := s.autoRolloutTick(ctx); !strings.HasPrefix(got, "started ") {
		t.Fatalf("after restart = %q", got)
	}
	j, _ := s.rollout.Get()
	// The hub restarts between the start and the mark: the job on file names
	// the item, so it is marked, not started again.
	s.markAuto(it.ID, "")
	restart()
	if got := s.autoRolloutTick(ctx); got != "marked "+j.ID {
		t.Fatalf("after a lost mark = %q", got)
	}
	// While it is live a second start is refused; once it has ended (and
	// the mark is lost too) StartAuto itself refuses the same item.
	if _, err := s.rollout.StartAuto(it.ID, "v0.5.9", relSHA, autoRolloutBy, []RolloutStep{{Host: "a", Wave: wavePeers, Phase: roPending}}); !errors.Is(err, errRolloutActive) {
		t.Fatalf("second start = %v", err)
	}
	_, _ = s.rollout.Cancel("me")
	if _, err := s.rollout.StartAuto(it.ID, "v0.5.9", relSHA, autoRolloutBy, []RolloutStep{{Host: "a", Wave: wavePeers, Phase: roPending}}); !errors.Is(err, errRolloutSame) {
		t.Fatalf("start after the job ended = %v", err)
	}
	if n := autoStarts(s); n != 1 {
		t.Fatalf("rollout.start entries = %d", n)
	}
}

func TestAutoRolloutDoesNotWakeAStoppedWorker(t *testing.T) {
	s, f, _, _, _ := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()
	f.stopped("b")
	f.set("b", func(h *relHost) { h.Stopped = true })
	if got := s.autoRolloutTick(ctx); !strings.HasPrefix(got, "started ") {
		t.Fatalf("tick = %q", got)
	}
	j, _ := s.rollout.Get()
	if j.Plan[1].Host != "b" || j.Plan[1].Phase != roSkipped || !j.Plan[1].CatchUp || !strings.Contains(j.Plan[1].Detail, "next starts") {
		t.Fatalf("b = %+v", j.Plan[1])
	}
	// And the engine does not ask for a wake for an automatic job's host
	// that went down without the book knowing it as stopped.
	f.set("b", func(h *relHost) { h.Stopped = false })
	for range 3 {
		s.rollout.tick(ctx)
	}
	f.finish("a", upgrade.RecOK, "v0.5.9")
	for range 3 {
		s.rollout.tick(ctx)
	}
	if len(f.wakeAsks) != 0 {
		t.Fatalf("woke %v", f.wakeAsks)
	}
	if j, _ := s.rollout.Get(); j.State != roDone {
		t.Fatalf("job = %s", j.State)
	}
}

func TestAutoRolloutLeavesAnOldReleaseAlone(t *testing.T) {
	s, _, _, it, _ := rolloutRouteRig(t, "a")
	_, _ = s.todos.mutate(it.ID, func(t *Todo) error {
		rel := *t.Release
		old := rel.TaggedAt.Add(-autoRolloutWindow - 1)
		rel.TaggedAt = &old
		t.Release = &rel
		return nil
	})
	if got := s.autoRolloutTick(context.Background()); got != "none" {
		t.Fatalf("tick = %q", got)
	}
}

func TestAutoRolloutWithNothingBehindMarksTheItem(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a")
	f.set("a", func(h *relHost) { h.Version = "v0.5.9" })
	if got := s.autoRolloutTick(context.Background()); got != "nothing to do" || !strings.HasPrefix(autoMark(t, s, it.ID), "none:") {
		t.Fatalf("tick = %q mark=%q", got, autoMark(t, s, it.ID))
	}
	if _, ok := s.rollout.Get(); ok {
		t.Fatal("a job was saved")
	}
}

// The engine itself: an automatic job never asks for a wake. A host that is
// down at its turn waits out the offline grace and is skipped for the
// catch-up, as a host that is simply offline is.
func TestAutoRolloutEngineDoesNotWake(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.canary = ""
	r.f.stopped("b")
	plan, err := planRollout(r.f.Hosts(context.Background(), "v0.6.4"), []string{"b", "a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.StartAuto("t1", "v0.6.4", "", autoRolloutBy, plan); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.want("running b=pending a=pending")
	r.f.advance(r.e.offlineGrace)
	r.tick()
	r.want("running b=skipped a=upgrading")
	if len(r.f.wakeAsks) != 0 {
		t.Fatalf("woke %v", r.f.wakeAsks)
	}
	if j, _ := r.e.Get(); !j.Plan[0].CatchUp {
		t.Fatal("b is not caught up later")
	}
}

// Under -test-mode (the browser tests, which tap Roll out to all themselves)
// the unset setting is off; set, it is what it says.
func TestAutoRolloutDefaultUnderTestMode(t *testing.T) {
	s, _, _, _, _ := rolloutRouteRig(t, "a")
	s.cfg.TestMode = true
	if s.autoRolloutOn() || s.autoRolloutTick(context.Background()) != "off" {
		t.Fatal("on under -test-mode with nothing set")
	}
	do(s, "PUT", "/api/settings", userHdr, `{"auto_rollout":true}`)
	if !s.autoRolloutOn() {
		t.Fatal("set on, yet off")
	}
}
