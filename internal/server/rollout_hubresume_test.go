package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- the hub that stopped its own step after the deploy restarted it (v0.5.64) ----

// The sequence the hub's log showed for v0.5.64: the peers are done, the
// hub step deploys, a peer of an earlier wave powers off while the deploy
// runs, the deploy restarts the hub on the version, the new hub resumes the
// job. The hub is up on the version, so its step passes on the first tick:
// it used to wait out the 5 minutes for the peer and stop with "not back
// online on v0.6.4 within 5m0s (it runs v0.6.4)".
func TestRolloutHubStepPassesAfterTheRestartWithAPeerAway(t *testing.T) {
	r := newHubRig(t, "a", "b")
	r.startWithHub("a", "b")
	r.peersDone("a", "b")
	r.want("running a=ok b=ok hub=upgrading")

	// b, upgraded and proven, powers off when idle; the deploy finishes and
	// the new pod is up on the version. This process ends with the old pod.
	r.f.set("b", func(h *relHost) { h.Status = "down" })
	r.f.endDeploy(depOK, "", "v0.6.4")

	r.e = r.engine()
	r.e.resumeAfterRestart()
	r.tick()
	r.want("done a=ok b=ok hub=ok")
	j, _ := r.e.Get()
	if hub := j.Plan[2]; hub.To != "v0.6.4" || !strings.Contains(hub.Detail, "not reachable now") || !strings.Contains(hub.Detail, "b") {
		t.Fatalf("hub step = %+v", hub)
	}
	if !strings.Contains(r.actions(), "rollout.resume-after-restart") {
		t.Fatalf("actions = %s", r.actions())
	}

	// Long after, nothing changes: the job is done, not stopped.
	r.f.advance(10 * time.Minute)
	r.tick()
	r.want("done a=ok b=ok hub=ok")
}

// The same, with every peer back: the gate passes at once too.
func TestRolloutHubStepPassesAtOnceAfterTheRestart(t *testing.T) {
	r := newHubRig(t, "a", "b")
	r.startWithHub("a", "b")
	r.peersDone("a", "b")
	r.f.endDeploy(depOK, "", "v0.6.4")
	r.e = r.engine()
	r.e.resumeAfterRestart()
	r.tick()
	r.want("done a=ok b=ok hub=ok")
}

// A peer reachable on an older version went back after its step: that is
// still trouble, and the stop says what it is, not "it runs v0.6.4".
func TestRolloutHubStepStopsOnAPeerThatWentBack(t *testing.T) {
	r := newHubRig(t, "a", "b")
	r.startWithHub("a", "b")
	r.peersDone("a", "b")
	r.f.set("b", func(h *relHost) { h.Version = "v0.6.3" })
	r.f.endDeploy(depOK, "", "v0.6.4")
	r.e = r.engine()
	r.e.resumeAfterRestart()
	r.tick()
	r.want("running a=ok b=ok hub=verifying")
	r.f.advance(rolloutBackWait)
	r.tick()
	r.want("stopped a=ok b=ok hub=failed")
	j, _ := r.e.Get()
	if why := j.Stop.Reason; !strings.Contains(why, "the hub is back on v0.6.4") || !strings.Contains(why, "b (v0.6.3)") {
		t.Fatalf("reason = %q", why)
	}
}

// ---- a stopped rollout of an older release does not hold the next one back ----

func TestAutoRolloutSupersedesAStoppedOlderRollout(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()

	// The older release: its item, its rollout stopped at a host, its alarm.
	for _, n := range []string{"a", "b"} {
		f.set(n, func(h *relHost) { h.Version = "v0.5.7" })
	}
	old := newRelease(t, s, "v0.5.8", relSHA)
	if _, err := s.todos.mutate(old.ID, func(t *Todo) error {
		rel := *t.Release
		rel.State = relTagged
		t.Release = &rel
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := planRollout(f.Hosts(ctx, "v0.5.8"), []string{"a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	oj, err := s.rollout.Start(old.ID, "v0.5.8", relSHA, "jesse", plan)
	if err != nil {
		t.Fatal(err)
	}
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecFailed, "")
	s.rollout.tick(ctx)
	if j, _ := s.rollout.Get(); j.State != roStopped {
		t.Fatalf("old job = %s", j.State)
	}
	oldItem := itemOf(t, s, old.ID)
	s.rolloutFailed(oldItem, "a", "runx", "The upgrade", "", "smoke", "")

	// The newer release is due: the stopped one is superseded, and it starts.
	got := s.autoRolloutTick(ctx)
	if !strings.HasPrefix(got, "started r") {
		t.Fatalf("tick = %q", got)
	}
	j, _ := s.rollout.Get()
	if j.Todo != it.ID || j.Version != "v0.5.9" || j.State != roRunning {
		t.Fatalf("job = %+v", j)
	}
	o := itemOf(t, s, old.ID)
	if o.State != TodoDone || !strings.Contains(o.Release.RolloutNote, "superseded by v0.5.9") {
		t.Fatalf("old item %s note %q", o.State, o.Release.RolloutNote)
	}
	for _, a := range s.todos.list("") {
		if a.Source == SourceRollout && strings.HasPrefix(a.AutoKey, SourceRollout+":"+old.ID+":") {
			t.Fatalf("the old release's alarm is still open: %+v", a)
		}
	}
	found := false
	for _, ev := range s.ledger.AuditLog("") {
		found = found || (ev.Action == "rollout.supersede" && strings.Contains(ev.Detail, "superseded by v0.5.9") && strings.Contains(ev.Detail, "stopped at a"))
	}
	if !found {
		t.Fatal("no rollout.supersede audit")
	}
	if oj.ID == j.ID {
		t.Fatal("the old job was reused")
	}

	// A paused job, or a stopped one for the same version or newer, is
	// left alone.
	e := s.rollout
	if _, err := e.Supersede(j.ID, "v0.5.10", "comebye"); err == nil {
		t.Fatal("a running job was superseded")
	}
}
