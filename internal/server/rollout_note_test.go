package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- one note per rollout (issue #141, slice 5) ----

// ends records the engine's onEnd calls.
func (r *rolloutRig) ends() *[]Rollout {
	var got []Rollout
	r.e.onEnd = func(j Rollout) { got = append(got, j) }
	return &got
}

func TestRolloutSendsOneNoteWhenItFinishes(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.canary = ""
	r.e.soak = 0
	ends := r.ends()
	r.start()
	r.tick()
	if len(*ends) != 0 {
		t.Fatal("a note while it runs")
	}
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("done a=ok b=ok")
	if len(*ends) != 1 || (*ends)[0].State != roDone || (*ends)[0].Told == nil {
		t.Fatalf("ends %+v", *ends)
	}
	// Later ticks, and a restarted hub on the same file, send nothing more.
	r.tick()
	r.e = r.engine()
	again := r.ends()
	r.e.resumeAfterRestart()
	r.tick()
	if len(*ends) != 1 || len(*again) != 0 {
		t.Fatalf("ends %d, after the restart %d", len(*ends), len(*again))
	}
	if n := rolloutNoteOf((*ends)[0]); n != "v0.6.4 is on every host" {
		t.Fatalf("note %q", n)
	}
}

func TestRolloutSendsOneNoteWhenItStopsAndNoneAfterTheRetry(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.canary = ""
	r.e.soak = 0
	ends := r.ends()
	r.start()
	r.tick()
	r.f.finish("a", upgrade.RecFailed, "")
	r.tick()
	r.want("stopped a=failed b=pending")
	if len(*ends) != 1 || (*ends)[0].State != roStopped {
		t.Fatalf("ends %+v", *ends)
	}
	if n := rolloutNoteOf((*ends)[0]); !strings.HasPrefix(n, "v0.6.4 stopped at a: ") {
		t.Fatalf("note %q", n)
	}
	// A stopped job stays stopped: no second note.
	r.f.advance(time.Minute)
	r.tick()
	// Retry: the resumed rollout finishes without another note.
	if _, err := r.e.Resume("jesse"); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("done a=ok b=ok")
	if len(*ends) != 1 {
		t.Fatalf("a resumed rollout sent another note: %d", len(*ends))
	}
}

func TestRolloutCatchUpSendsNoNote(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.canary = ""
	r.e.soak, r.e.catchUpEvery = 0, 0
	ends := r.ends()
	r.f.set("b", func(h *relHost) { h.Status = "down" })
	r.start()
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.advance(r.e.offlineGrace)
	r.tick()
	r.want("done a=ok b=skipped")
	if len(*ends) != 1 {
		t.Fatalf("ends %d", len(*ends))
	}
	if n := rolloutNoteOf((*ends)[0]); n != "v0.6.4 is on every host (1 behind until they're back: b)" {
		t.Fatalf("note %q", n)
	}
	// b is back: the catch-up upgrades it and the job is done again, quietly.
	r.f.set("b", func(h *relHost) { h.Status = "ok" })
	r.tick()
	r.tick()
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("done a=ok b=ok")
	if len(*ends) != 1 {
		t.Fatalf("a catch-up sent a note: %d", len(*ends))
	}

	// A job started only to catch a host up sends none either.
	r2 := newRolloutRig(t, "a")
	r2.canary = ""
	r2.e.soak = 0
	ends2 := r2.ends()
	plan, err := planRollout(r2.f.Hosts(context.Background(), "v0.6.4"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.e.StartCatchUp("t1", "v0.6.4", strings.Repeat("a", 40), autoRolloutBy, plan); err != nil {
		t.Fatal(err)
	}
	r2.tick()
	r2.f.finish("a", upgrade.RecOK, "v0.6.4")
	r2.tick()
	r2.want("done a=ok")
	if len(*ends2) != 0 {
		t.Fatalf("a catch-up job sent a note: %d", len(*ends2))
	}
}

// A job saved before Told existed, finished, then caught up: no note for it.
func TestRolloutOldDoneJobCaughtUpSendsNoNote(t *testing.T) {
	r := newRolloutRig(t, "a", "b")
	r.canary = ""
	r.e.soak, r.e.catchUpEvery = 0, 0
	r.f.set("b", func(h *relHost) { h.Status = "down" })
	r.start()
	r.tick()
	r.f.finish("a", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.f.advance(r.e.offlineGrace)
	r.tick()
	r.want("done a=ok b=skipped")
	j, _ := r.e.Get()
	j.Told = nil // as an older hub saved it
	if err := r.e.save(&j); err != nil {
		t.Fatal(err)
	}
	ends := r.ends()
	r.f.set("b", func(h *relHost) { h.Status = "ok" })
	r.tick()
	r.tick()
	r.f.finish("b", upgrade.RecOK, "v0.6.4")
	r.tick()
	r.want("done a=ok b=ok")
	if len(*ends) != 0 {
		t.Fatalf("ends %d", len(*ends))
	}
}

func TestTellRolloutPushesTheItemsLine(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "b")
	type sent struct {
		todo, run, title, body string
		stopped                bool
	}
	var got []sent
	s.rolloutPush = func(todo, run, title, body string, stopped bool) {
		got = append(got, sent{todo, run, title, body, stopped})
	}
	s.rollout.onEnd = s.tellRollout
	s.rollout.soak = 0
	f.set("b", func(h *relHost) { h.Status = "down" })
	plan, err := planRollout(f.Hosts(context.Background(), "v0.5.9"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.rollout.Start(it.ID, "v0.5.9", relSHA, "jesse", plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecOK, "v0.5.9")
	s.rollout.tick(ctx)
	f.advance(s.rollout.offlineGrace)
	s.rollout.tick(ctx)
	s.rollout.tick(ctx)
	want := "v0.5.9 is on every host (1 behind until they're back: b)"
	if len(got) != 1 || got[0] != (sent{it.ID, j.ID, "v0.5.9 rolled out", want, false}) {
		t.Fatalf("pushes %+v", got)
	}
	if n := itemOf(t, s, it.ID).Release.RolloutNote; n != want {
		t.Fatalf("item note %q", n)
	}
	told := 0
	for _, ev := range s.ledger.AuditLog(it.ID) {
		if ev.Action == "rollout.told" {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("rollout.told audits %d", told)
	}
	// A todo for a stop already exists (the alarm); the note adds none.
	before := len(s.todos.list("all"))
	s.tellRollout(Rollout{ID: "r1", Todo: it.ID, Version: "v0.5.9", State: roStopped,
		Stop: &RolloutStop{Host: "a", Reason: "comebye restarted on a during the soak: a crash loop"}, Plan: []RolloutStep{{Host: "a", Phase: roFailed}}})
	if len(got) != 2 || !got[1].stopped || got[1].title != "v0.5.9 rollout stopped" || got[1].body != "v0.5.9 stopped at a: comebye restarted on a during the soak: a crash loop" {
		t.Fatalf("stop push %+v", got[len(got)-1])
	}
	if after := len(s.todos.list("all")); after != before {
		t.Fatalf("the note filed a To do: %d -> %d", before, after)
	}
}
