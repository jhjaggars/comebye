package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- busy hosts (issue #141 slice 3) ----

// busyRig is the engine rig with no canary, so host a's step starts first.
func busyRig(t *testing.T, names ...string) *rolloutRig {
	t.Helper()
	r := newRolloutRig(t, names...)
	r.canary = ""
	return r
}

func (r *rolloutRig) step(host string) RolloutStep {
	r.t.Helper()
	j, _ := r.e.Get()
	for _, st := range j.Plan {
		if st.Host == host {
			return st
		}
	}
	r.t.Fatalf("no step for %s", host)
	return RolloutStep{}
}

// A sign-in in progress holds the upgrade until it ends; then the host is
// upgraded at once, and the step says it waited.
func TestRolloutWaitsWhileASignInIsInProgress(t *testing.T) {
	r := busyRig(t, "a", "b")
	r.f.set("a", func(h *relHost) { h.Busy = []string{"claude sign-in"} })
	r.start()
	r.tick()
	r.want("running a=pending b=pending")
	st := r.step("a")
	if st.Detail != "waiting: busy (claude sign-in)" || st.BusySince == nil {
		t.Fatalf("step = %+v", st)
	}
	j, _ := r.e.Get()
	if j.Note != "a is busy (claude sign-in): waiting up to 15m more" {
		t.Fatalf("note = %q", j.Note)
	}
	// Still busy 10 minutes on: nothing started, not even the next host.
	for range 10 {
		r.f.advance(time.Minute)
		r.tick()
	}
	r.want("running a=pending b=pending")
	if len(r.f.starts) != 0 {
		t.Fatalf("started %v while busy", r.f.starts)
	}
	j, _ = r.e.Get()
	if j.Note != "a is busy (claude sign-in): waiting up to 5m more" {
		t.Fatalf("note = %q", j.Note)
	}
	// The sign-in ends: the upgrade starts on the next tick.
	r.f.set("a", func(h *relHost) { h.Busy = nil })
	r.f.advance(5 * time.Second)
	r.tick()
	r.want("running a=upgrading b=pending")
	st = r.step("a")
	if st.Detail != "waited 10m5s while it was busy" || st.BusySince != nil {
		t.Fatalf("step = %+v", st)
	}
	if got := r.actions(); !strings.Contains(got, "rollout.busy:a rollout.step:a") {
		t.Fatalf("audit %s", got)
	}
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=ok b=upgrading")
	if st := r.step("a"); st.Detail != "waited 10m5s while it was busy" { // the finished row keeps it
		t.Fatalf("done detail %q", st.Detail)
	}
}

// A deploy running on the host (the release host's Bump hub) holds it too.
func TestRolloutWaitsWhileADeployIsRunning(t *testing.T) {
	r := busyRig(t, "a")
	r.f.set("a", func(h *relHost) { h.Busy = []string{"deploy"} })
	r.start()
	r.tick()
	r.f.advance(3 * time.Minute)
	r.tick()
	r.want("running a=pending")
	if st := r.step("a"); st.Detail != "waiting: busy (deploy)" {
		t.Fatalf("detail %q", st.Detail)
	}
	r.f.set("a", func(h *relHost) { h.Busy = nil })
	r.tick()
	r.want("running a=upgrading")
}

// The limit: a host still busy after 15 minutes is upgraded anyway, and
// the step keeps why.
func TestRolloutGoesAheadAfterTheBusyLimit(t *testing.T) {
	r := busyRig(t, "a")
	r.f.set("a", func(h *relHost) { h.Busy = []string{"claude sign-in", "tagging"} })
	r.start()
	r.tick()
	r.f.advance(15*time.Minute - time.Second)
	r.tick()
	r.want("running a=pending")
	if st := r.step("a"); st.Detail != "waiting: busy (claude sign-in, tagging)" {
		t.Fatalf("detail %q", st.Detail)
	}
	r.f.advance(time.Second)
	r.tick()
	r.want("running a=upgrading")
	if st := r.step("a"); st.Detail != "went ahead after waiting 15m: still busy (claude sign-in, tagging)" {
		t.Fatalf("detail %q", st.Detail)
	}
	if len(r.f.starts) != 1 {
		t.Fatalf("starts %v", r.f.starts)
	}
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("done a=ok")
	if st := r.step("a"); st.Detail != "went ahead after waiting 15m: still busy (claude sign-in, tagging)" {
		t.Fatalf("done detail %q", st.Detail)
	}
}

// The wait survives a hub restart: the clock is the one saved in the step.
func TestRolloutBusyWaitSurvivesAHubRestart(t *testing.T) {
	r := busyRig(t, "a")
	r.f.set("a", func(h *relHost) { h.Busy = []string{"claude sign-in"} })
	r.start()
	r.tick()
	r.f.advance(14 * time.Minute)
	r.tick()
	r.e = r.engine()
	r.e.resumeAfterRestart()
	r.tick()
	r.want("running a=pending")
	r.f.advance(time.Minute)
	r.tick()
	r.want("running a=upgrading") // 15 minutes in all, not 15 more
}

// A start that fails after the wait ended does not begin another wait.
func TestRolloutBusyWaitIsNotRestartedByAFailedStart(t *testing.T) {
	r := busyRig(t, "a")
	r.f.set("a", func(h *relHost) { h.Busy = []string{"claude sign-in"} })
	r.start()
	r.tick()
	r.f.advance(15 * time.Minute)
	r.f.startErr["a"] = errFake("connection refused")
	r.tick()
	r.want("running a=pending")
	if st := r.step("a"); !strings.HasPrefix(st.Detail, "could not start yet") {
		t.Fatalf("detail %q", st.Detail)
	}
	delete(r.f.startErr, "a")
	r.f.advance(5 * time.Second)
	r.tick()
	r.want("running a=upgrading")
}

type errFake string

func (e errFake) Error() string { return string(e) }

// The hub's step waits for the hub's own sign-in the same way.
func TestHubWaveWaitsWhileTheHubIsBusy(t *testing.T) {
	r := newHubRig(t, "a")
	r.startWithHub("a")
	r.f.set("hub", func(h *relHost) { h.Busy = []string{"claude sign-in"} })
	r.peersDone("a")
	r.want("running a=ok hub=pending")
	if r.f.deploys != 0 {
		t.Fatal("deployed while busy")
	}
	if st := r.step("hub"); st.Detail != "waiting: busy (claude sign-in)" {
		t.Fatalf("detail %q", st.Detail)
	}
	r.f.set("hub", func(h *relHost) { h.Busy = nil })
	r.tick()
	r.want("running a=ok hub=upgrading")
	if r.f.deploys != 1 {
		t.Fatalf("deploys %d", r.f.deploys)
	}
}

// An automatic rollout never wakes a stopped worker, busy or not: it is
// skipped as offline and marked to catch up.
func TestAutoRolloutWithABusyHostStillDoesNotWake(t *testing.T) {
	r := busyRig(t, "a", "b")
	r.f.stopped("b")
	r.f.set("a", func(h *relHost) { h.Busy = []string{"claude sign-in"} })
	plan, err := planRollout(r.f.Hosts(t.Context(), "v0.6.4"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.StartAuto("t1", "v0.6.4", strings.Repeat("a", 40), "jesse", plan); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.f.set("a", func(h *relHost) { h.Busy = nil })
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.advance(3 * time.Minute)
	r.tick()
	r.want("done a=ok b=skipped")
	if len(r.f.wakeAsks) != 0 {
		t.Fatalf("woke %v", r.f.wakeAsks)
	}
	if st := r.step("b"); !st.CatchUp {
		t.Fatalf("b = %+v", st)
	}
}

// The host itself says it is busy: a sign-in in progress shows in its
// upgrade status, and goes when the sign-in ends; a tagging and a deploy
// show too.
func TestUpgradeStatusNamesWhatARestartWouldCut(t *testing.T) {
	s, _, _ := signinHost(t, "")
	busy := func() []string {
		t.Helper()
		w := do(s, "GET", "/api/release/upgrade", userHdr, "")
		if w.Code != 200 {
			t.Fatalf("status %d %s", w.Code, w.Body)
		}
		var st upgradeStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st.Busy
	}
	if b := busy(); len(b) != 0 {
		t.Fatalf("idle host busy: %v", b)
	}
	w := do(s, "POST", "/api/hosts/desk/signin", userHdr, `{"agent":"claude"}`)
	if w.Code != 200 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	id := jsonOf(t, w)["id"].(string)
	if b := busy(); strings.Join(b, ",") != "claude sign-in" {
		t.Fatalf("busy %v", b)
	}
	if w := do(s, "DELETE", "/api/hosts/desk/signin/"+id, userHdr, ""); w.Code != 200 {
		t.Fatalf("cancel = %d %s", w.Code, w.Body)
	}
	if b := busy(); len(b) != 0 {
		t.Fatalf("busy after the sign-in ended: %v", b)
	}
	s.setTagging("t1", true)
	s.rel.deploying.Store(true)
	if b := busy(); strings.Join(b, ",") != "tagging,deploy" {
		t.Fatalf("busy %v", b)
	}
	s.setTagging("t1", false)
	s.rel.deploying.Store(false)
	s.rel.peerTagging.Store(true)
	if b := busy(); strings.Join(b, ",") != "tagging" {
		t.Fatalf("busy %v", b)
	}
	s.rel.peerTagging.Store(false)
}
