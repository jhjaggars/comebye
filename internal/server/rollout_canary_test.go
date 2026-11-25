package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- the canary and its soak (issue #141 slice 2) ----

// soakRig is the engine rig with the 10-minute default soak, process start
// times on every host, and the soak failures counted.
func soakRig(t *testing.T, names ...string) (*rolloutRig, *[]RolloutStep) {
	t.Helper()
	r := newRolloutRig(t, names...)
	boot := r.f.clock().Add(-time.Hour)
	for _, n := range names {
		b := boot
		r.f.set(n, func(h *relHost) { h.ProcStart = &b })
	}
	var fails []RolloutStep
	r.e.soak = rolloutSoak
	r.e.onSoakFail = func(_ Rollout, st RolloutStep) { fails = append(fails, st) }
	return r, &fails
}

// upgradeCanary takes host a through its upgrade into the soak: the new
// binary starts (a new process start time), the record is ok, it is back.
func upgradeCanary(r *rolloutRig) time.Time {
	r.t.Helper()
	r.tick()
	r.want("running a=upgrading b=pending c=pending")
	r.f.advance(20 * time.Second)
	boot := r.f.clock()
	r.f.set("a", func(h *relHost) { h.ProcStart = &boot })
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=soaking b=pending c=pending")
	return boot
}

func TestCanarySoakPassesThenTheOthers(t *testing.T) {
	r, fails := soakRig(t, "a", "b", "c")
	j := r.start()
	if j.Canary != "a" || j.Plan[0].Wave != waveCanary {
		t.Fatalf("canary = %q plan = %+v", j.Canary, j.Plan)
	}
	boot := upgradeCanary(r)
	j, _ = r.e.Get()
	st := j.Plan[0]
	if st.SoakUntil == nil || !st.SoakUntil.Equal(r.f.clock().Add(10*time.Minute)) || st.ProcStart == nil || !st.ProcStart.Equal(boot) {
		t.Fatalf("soak step = %+v", st)
	}
	if st.Detail != "soak: 10m0s left of 10m0s" || j.Note != "soaking a" {
		t.Fatalf("detail = %q note = %q", st.Detail, j.Note)
	}
	// Healthy all along: the time left counts down, nobody else is touched.
	for range 9 {
		r.f.advance(time.Minute)
		r.tick()
	}
	j, _ = r.e.Get()
	if j.Plan[0].Phase != roSoaking || j.Plan[0].Detail != "soak: 1m0s left of 10m0s" || len(r.f.starts) != 1 {
		t.Fatalf("at 9 minutes: %+v starts=%v", j.Plan[0], r.f.starts)
	}
	r.f.advance(time.Minute)
	r.tick()
	r.want("running a=ok b=upgrading c=pending")
	j, _ = r.e.Get()
	if j.Plan[0].Detail != "soaked 10m0s" {
		t.Fatalf("done detail = %q", j.Plan[0].Detail)
	}
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.finish("c", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("done a=ok b=ok c=ok")
	if len(*fails) != 0 || !strings.Contains(auditText(r), "rollout.gate a soak=ok 10m0s") {
		t.Fatalf("fails = %v actions:\n%s", *fails, r.actions())
	}
}

func TestCanarySoakFollowsTheSetting(t *testing.T) {
	r, _ := soakRig(t, "a", "b", "c")
	soak := 3 * time.Minute
	r.e.soakFor = func() time.Duration { return soak }
	r.start()
	upgradeCanary(r)
	soak = 30 * time.Minute // a change now does not stretch the soak in progress
	r.f.advance(3 * time.Minute)
	r.tick()
	r.want("running a=ok b=upgrading c=pending")
}

func TestCanaryCrashLoopStopsTheRollout(t *testing.T) {
	r, fails := soakRig(t, "a", "b", "c")
	r.start()
	upgradeCanary(r)
	r.f.advance(4 * time.Minute)
	r.tick()
	r.want("running a=soaking b=pending c=pending")
	// comebye on a crashed and systemd brought it back between two ticks:
	// the hub never saw it offline, only a new process start time.
	r.f.advance(5 * time.Second)
	again := r.f.clock().Add(-2 * time.Second)
	r.f.set("a", func(h *relHost) { h.ProcStart = &again })
	j := r.tick()
	r.want("stopped a=failed b=pending c=pending")
	if j.Stop == nil || j.Stop.Host != "a" || !strings.Contains(j.Stop.Reason, "restarted on a during the soak") || !strings.Contains(j.Stop.Reason, "crash loop") {
		t.Fatalf("stop = %+v", j.Stop)
	}
	if len(*fails) != 1 || (*fails)[0].Host != "a" {
		t.Fatalf("soak failures = %+v", *fails)
	}
	// Nothing else is touched afterwards, however long it waits.
	for range 5 {
		r.f.advance(time.Minute)
		r.tick()
	}
	if len(r.f.starts) != 1 || len(*fails) != 1 {
		t.Fatalf("starts = %v fails = %d", r.f.starts, len(*fails))
	}
	r.want("stopped a=failed b=pending c=pending")
}

func TestCanarySoakFailsOnHealthAndVersion(t *testing.T) {
	cases := []struct {
		name string
		hurt func(h *relHost)
		want string
	}{
		{"offline", func(h *relHost) { h.Status = "down" }, "went offline during the soak"},
		{"no answer", func(h *relHost) { h.Unknown = true }, "did not answer its health check"},
		// Its smoke check, or a person, put comebye.prev back: it is left there.
		{"went back", func(h *relHost) { h.Version = "v0.6.3" }, "went back to v0.6.3 during the soak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, fails := soakRig(t, "a", "b", "c")
			r.start()
			upgradeCanary(r)
			r.f.advance(time.Minute)
			r.f.set("a", c.hurt)
			j := r.tick()
			r.want("stopped a=failed b=pending c=pending")
			if j.Stop == nil || !strings.Contains(j.Stop.Reason, c.want) || len(*fails) != 1 {
				t.Fatalf("stop = %+v fails = %d", j.Stop, len(*fails))
			}
			if len(r.f.starts) != 1 {
				t.Fatalf("starts = %v", r.f.starts)
			}
		})
	}
}

// A restart of the hub in the middle of the soak carries on with the time
// that was left: not from zero, and not skipped.
func TestCanarySoakResumesAfterAHubRestart(t *testing.T) {
	r, _ := soakRig(t, "a", "b", "c")
	r.start()
	upgradeCanary(r)
	until := r.f.clock().Add(10 * time.Minute)
	r.f.advance(4 * time.Minute)
	r.tick()

	// The hub is down for a minute; a new one reads the file.
	r.f.advance(time.Minute)
	r.e = r.engine()
	r.e.soak = rolloutSoak
	r.e.resumeAfterRestart()
	j := r.tick()
	r.want("running a=soaking b=pending c=pending")
	if st := j.Plan[0]; st.SoakUntil == nil || !st.SoakUntil.Equal(until) || st.Detail != "soak: 5m0s left of 10m0s" {
		t.Fatalf("after the restart: %+v", st)
	}
	if !strings.Contains(r.actions(), "rollout.resume-after-restart") {
		t.Fatalf("actions:\n%s", r.actions())
	}
	// Not skipped: still soaking a second before the end.
	r.f.advance(5*time.Minute - time.Second)
	r.tick()
	r.want("running a=soaking b=pending c=pending")
	if len(r.f.starts) != 1 {
		t.Fatalf("starts = %v", r.f.starts)
	}
	// Not from zero: done at the original end.
	r.f.advance(time.Second)
	r.tick()
	r.want("running a=ok b=upgrading c=pending")
}

// A crash loop the hub missed while it was down still shows: the start time
// it saved is the one it compares with.
func TestCanaryRestartWhileTheHubWasDownStillFails(t *testing.T) {
	r, fails := soakRig(t, "a", "b", "c")
	r.start()
	upgradeCanary(r)
	r.f.advance(2 * time.Minute)
	r.tick()
	again := r.f.clock().Add(30 * time.Second)
	r.f.set("a", func(h *relHost) { h.ProcStart = &again })
	r.f.advance(time.Minute)
	r.e = r.engine()
	r.e.onSoakFail = func(_ Rollout, st RolloutStep) { *fails = append(*fails, st) }
	r.tick()
	r.want("stopped a=failed b=pending c=pending")
	if len(*fails) != 1 {
		t.Fatalf("fails = %d", len(*fails))
	}
}

func TestCanarySkipSoak(t *testing.T) {
	// At the confirm: the canary goes first and does not soak.
	r, _ := soakRig(t, "a", "b", "c")
	plan, err := planRollout(r.f.Hosts(context.Background(), "v0.6.4"), nil, "a")
	if err != nil {
		t.Fatal(err)
	}
	j, err := r.e.StartSkipSoak("t1", "v0.6.4", strings.Repeat("a", 40), "jesse", plan)
	if err != nil || !j.SoakSkipped || j.Canary != "" || j.Plan[0].Host != "a" || j.Plan[0].Wave != wavePeers {
		t.Fatalf("job = %+v err = %v", j, err)
	}
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("running a=ok b=upgrading c=pending")
	if !strings.Contains(auditText(r), "soak=skipped") {
		t.Fatalf("actions:\n%s", r.actions())
	}

	// While it soaks: the user ends the soak, the others follow.
	r2, _ := soakRig(t, "a", "b", "c")
	r2.start()
	upgradeCanary(r2)
	r2.f.advance(2 * time.Minute)
	j, err = r2.e.SkipSoak("jesse")
	if err != nil || !j.SoakSkipped || j.Plan[0].Phase != roOK || !strings.Contains(j.Plan[0].Detail, "soak skipped by jesse with 8m0s left") {
		t.Fatalf("skip = %+v err = %v", j.Plan[0], err)
	}
	r2.tick()
	r2.want("running a=ok b=upgrading c=pending")
	if _, err := r2.e.SkipSoak("jesse"); err != errRolloutState {
		t.Fatalf("skip with nothing soaking = %v", err)
	}
}

func TestCanaryAutoRolloutCannotSkipTheSoak(t *testing.T) {
	r, _ := soakRig(t, "a", "b", "c")
	plan, _ := planRollout(r.f.Hosts(context.Background(), "v0.6.4"), nil, "a")
	if _, err := r.e.StartAuto("t1", "v0.6.4", strings.Repeat("a", 40), autoRolloutBy, plan); err != nil {
		t.Fatal(err)
	}
	upgradeCanary(r)
	if _, err := r.e.SkipSoak("jesse"); err != errRolloutState {
		t.Fatalf("skip on an automatic rollout = %v", err)
	}
	r.want("running a=soaking b=pending c=pending")
}

// auditText is the rig's audit, one "action host detail" line each.
func auditText(r *rolloutRig) string {
	var b strings.Builder
	for _, ev := range r.audit {
		b.WriteString(ev.Action + " " + ev.Host + " " + ev.Detail + "\n")
	}
	return b.String()
}

// ---- the hub's side: the setting, the default canary, the routes, the To do ----

func TestCanaryIsTheReleaseHostByDefault(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "b", "c")
	ctx := context.Background()
	if c, from := s.rolloutCanary(); c != "" || from != "none" {
		t.Fatalf("no release host: %q %q", c, from)
	}
	s.cfg.Release.Host = "b" // the peer that tags and runs the hub bump
	if c, from := s.rolloutCanary(); c != "b" || from != "release host" {
		t.Fatalf("release host: %q %q", c, from)
	}
	if got := s.autoRolloutTick(ctx); !strings.HasPrefix(got, "started r") {
		t.Fatalf("tick = %q", got)
	}
	j, _ := s.rollout.Get()
	if j.Canary != "b" || j.Plan[0].Host != "b" || j.Plan[0].Wave != waveCanary || j.Todo != it.ID {
		t.Fatalf("job = %+v", j)
	}
	s.rollout.tick(ctx)
	if len(f.starts) != 1 || f.starts[0] != "b" {
		t.Fatalf("starts = %v", f.starts)
	}

	// The flag wins over the release host, and the setting over both.
	s.cfg.Release.Canary = "c"
	if c, from := s.rolloutCanary(); c != "c" || from != "flag" {
		t.Fatalf("flag: %q %q", c, from)
	}
	if _, err := s.settings.put(hostSettings{Rollout: &rolloutSettings{Canary: "a"}}, nil, "me", time.Now()); err != nil {
		t.Fatal(err)
	}
	if c, from := s.rolloutCanary(); c != "a" || from != "setting" {
		t.Fatalf("setting: %q %q", c, from)
	}
	if _, err := s.settings.put(hostSettings{Rollout: &rolloutSettings{Canary: "none"}}, nil, "me", time.Now()); err != nil {
		t.Fatal(err)
	}
	if c, from := s.rolloutCanary(); c != "" || from != "none" {
		t.Fatalf("none: %q %q", c, from)
	}
}

func TestCanarySettingsRoute(t *testing.T) {
	s, _, _, _, _ := rolloutRouteRig(t, "a", "b")
	var v settingsView
	if code := getJSON(t, s, "/api/settings", userHdr, &v); code != 200 || !v.Rollout.Hub || v.Rollout.SoakMinutes != 10 || v.Rollout.SoakSet || v.Rollout.CanaryFrom != "none" {
		t.Fatalf("view = %d %+v", code, v.Rollout)
	}
	if s.rolloutSoakSetting() != 0 {
		t.Fatalf("soak setting = %s", s.rolloutSoakSetting())
	}
	for _, bad := range []string{`{"rollout":{"soak_minutes":0}}`, `{"rollout":{"soak_minutes":121}}`, `{"rollout":{"canary":"a b"}}`} {
		if w := do(s, "PUT", "/api/settings", userHdr, bad); w.Code != 400 || !strings.Contains(w.Body.String(), "rollout.") {
			t.Errorf("%s = %d %s", bad, w.Code, w.Body)
		}
	}
	if w := do(s, "PUT", "/api/settings", userHdr, `{"rollout":{"canary":"a","soak_minutes":15}}`); w.Code != 200 {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	if s.rolloutSoakSetting() != 15*time.Minute || s.canaryName() != "a" {
		t.Fatalf("soak = %s canary = %q", s.rolloutSoakSetting(), s.canaryName())
	}
	// A PUT without "rollout" keeps it; {} puts the defaults back.
	if w := do(s, "PUT", "/api/settings", userHdr, `{}`); w.Code != 200 || s.canaryName() != "a" {
		t.Fatalf("put {} = %d canary %q", w.Code, s.canaryName())
	}
	if w := do(s, "PUT", "/api/settings", userHdr, `{"rollout":{}}`); w.Code != 200 || s.canaryName() != "" || s.rolloutSoakSetting() != 0 {
		t.Fatalf("defaults back = %d canary %q soak %s", w.Code, s.canaryName(), s.rolloutSoakSetting())
	}
}

// A crash loop in the soak stops the rollout with exactly one To do.
func TestCanaryCrashLoopFilesOneTodo(t *testing.T) {
	s, f, _, it, tok := rolloutRouteRig(t, "a", "b", "c")
	ctx := context.Background()
	s.rollout.onSoakFail = s.alarmSoak
	s.rollout.soakFor = s.rolloutSoakSetting
	if w := do(s, "PUT", "/api/settings", userHdr, `{"rollout":{"canary":"a"}}`); w.Code != 200 {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	boot := f.clock().Add(-time.Hour)
	for _, n := range []string{"a", "b", "c"} {
		b := boot
		f.set(n, func(h *relHost) { h.ProcStart = &b })
	}
	if w := do(s, "POST", "/api/release/rollout", withCSRF(tok), startBody(it, `["a","b","c"]`)); w.Code != 202 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	s.rollout.tick(ctx)
	f.advance(20 * time.Second)
	newBoot := f.clock()
	f.set("a", func(h *relHost) { h.ProcStart = &newBoot })
	f.finish("a", upgrade.RecOK, "v0.5.9")
	s.rollout.tick(ctx)
	j, _ := s.rollout.Get()
	if j.Plan[0].Phase != roSoaking || j.Plan[0].SoakUntil == nil || !j.Plan[0].SoakUntil.Equal(f.clock().Add(10*time.Minute)) {
		t.Fatalf("soak = %+v", j.Plan[0])
	}
	// The view shows the soak and its time left.
	var view map[string]any
	if code := getJSON(t, s, "/api/release/"+it.ID, userHdr, &view); code != 200 || !strings.Contains(view["needs_text"].(string), "soaking a") {
		t.Fatalf("view = %d %v", code, view["needs_text"])
	}
	// Two restarts in a row (a crash loop): one stop, one To do.
	for range 2 {
		f.advance(30 * time.Second)
		again := f.clock()
		f.set("a", func(h *relHost) { h.ProcStart = &again })
		s.rollout.tick(ctx)
	}
	j, _ = s.rollout.Get()
	if j.State != roStopped || j.Stop == nil || j.Stop.Host != "a" {
		t.Fatalf("job = %s %+v", j.State, j.Stop)
	}
	items := activeRollout(rolloutItems(s))
	if len(items) != 1 || items[0].Question != "The v0.5.9 upgrade of a failed" || !strings.Contains(items[0].Context, "canary soak of a") || !strings.Contains(items[0].Context, "crash loop") {
		t.Fatalf("items = %+v", items)
	}
	if len(f.starts) != 1 {
		t.Fatalf("starts = %v", f.starts)
	}
}

func TestCanarySkipSoakRoute(t *testing.T) {
	s, f, _, it, tok := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()
	s.cfg.Release.Canary = "a"
	if w := do(s, "POST", "/api/release/rollout", withCSRF(tok), startBody(it, `["a","b"]`)); w.Code != 202 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecOK, "v0.5.9")
	s.rollout.tick(ctx)
	j, _ := s.rollout.Get()
	if j.Plan[0].Phase != roSoaking {
		t.Fatalf("a = %+v", j.Plan[0])
	}
	if w := do(s, "POST", "/api/release/rollout/skip-soak", userHdr, `{"id":"`+j.ID+`"}`); w.Code != 403 {
		t.Fatalf("no csrf = %d", w.Code)
	}
	if w := do(s, "POST", "/api/release/rollout/skip-soak", withCSRF(tok), `{"id":"`+j.ID+`"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"soak_skipped":true`) {
		t.Fatalf("skip = %d %s", w.Code, w.Body)
	}
	s.rollout.tick(ctx)
	if len(f.starts) != 2 || f.starts[1] != "b" {
		t.Fatalf("starts = %v", f.starts)
	}
}

func TestCanarySkipSoakAtTheConfirm(t *testing.T) {
	s, f, _, it, tok := rolloutRouteRig(t, "a", "b")
	ctx := context.Background()
	s.cfg.Release.Canary = "a"
	body := strings.TrimSuffix(startBody(it, `["a","b"]`), "}") + `,"skip_soak":true}`
	if w := do(s, "POST", "/api/release/rollout", withCSRF(tok), body); w.Code != 202 || !strings.Contains(w.Body.String(), `"soak_skipped":true`) {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecOK, "v0.5.9")
	s.rollout.tick(ctx)
	if len(f.starts) != 2 {
		t.Fatalf("starts = %v", f.starts)
	}
}
