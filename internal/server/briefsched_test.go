package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func TestBriefSchedulePlan(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	set := time.Date(2026, 10, 20, 15, 4, 0, 0, chi)
	cases := []struct {
		name      string
		c         BriefSchedule
		now       time.Time
		slot, nxt time.Time
	}{
		{"daily at 08:00, after", BriefSchedule{Every: "24h", At: "08:00", TZ: "America/Chicago", Updated: set},
			time.Date(2026, 10, 23, 9, 0, 0, 0, chi), time.Date(2026, 10, 23, 8, 0, 0, 0, chi), time.Date(2026, 10, 24, 8, 0, 0, 0, chi)},
		{"daily at 08:00, before", BriefSchedule{Every: "24h", At: "08:00", TZ: "America/Chicago", Updated: set},
			time.Date(2026, 10, 23, 7, 59, 0, 0, chi), time.Date(2026, 10, 22, 8, 0, 0, 0, chi), time.Date(2026, 10, 23, 8, 0, 0, 0, chi)},
		// DST ends Nov 1 2026: 08:00 stays 08:00 local.
		{"daily across DST", BriefSchedule{Every: "24h", At: "08:00", TZ: "America/Chicago", Updated: set},
			time.Date(2026, 11, 2, 12, 0, 0, 0, chi), time.Date(2026, 11, 2, 8, 0, 0, 0, chi), time.Date(2026, 11, 3, 8, 0, 0, 0, chi)},
		{"every 2 days from the day it was set", BriefSchedule{Every: "48h", At: "08:00", TZ: "America/Chicago", Updated: set},
			time.Date(2026, 10, 23, 9, 0, 0, 0, chi), time.Date(2026, 10, 22, 8, 0, 0, 0, chi), time.Date(2026, 10, 24, 8, 0, 0, 0, chi)},
		{"every 12h at 06:00", BriefSchedule{Every: "12h", At: "06:00", TZ: "America/Chicago", Updated: set},
			time.Date(2026, 10, 21, 19, 0, 0, 0, chi), time.Date(2026, 10, 21, 18, 0, 0, 0, chi), time.Date(2026, 10, 22, 6, 0, 0, 0, chi)},
		{"every 5h from when it was set", BriefSchedule{Every: "5h", Updated: set},
			set.Add(11 * time.Hour), set.Add(10 * time.Hour), set.Add(15 * time.Hour)},
	}
	for _, c := range cases {
		p, err := planOf(c.c)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		slot := p.slotAt(c.now)
		if !slot.Equal(c.slot) || !p.nextAfter(slot).Equal(c.nxt) {
			t.Errorf("%s: slot %v next %v, want %v %v", c.name, slot, p.nextAfter(slot), c.slot, c.nxt)
		}
	}
	for _, bad := range []BriefSchedule{{Every: "30m"}, {Every: "721h"}, {Every: "daily"}, {Every: "24h", At: "24:00"}, {Every: "24h", TZ: "Mars/Olympus"}} {
		if _, err := planOf(bad); err == nil {
			t.Errorf("planOf(%+v) accepted", bad)
		}
	}
	for d, want := range map[time.Duration]string{24 * time.Hour: "24h", 90 * time.Minute: "1h30m", 70 * time.Minute: "1h10m", time.Hour: "1h"} {
		if got := fmtEvery(d); got != want {
			t.Errorf("fmtEvery(%v) = %q, want %q", d, got, want)
		}
	}
}

// schedAt sets an hourly schedule as if it was set at base.
func schedAt(t *testing.T, s *Server, base time.Time) {
	t.Helper()
	if w := do(s, "POST", "/api/brief/schedule", agentHdr("w1:p1"), `{"enabled":true,"every":"1h"}`); w.Code != 200 {
		t.Fatalf("set = %d %s", w.Code, w.Body)
	}
	_ = s.briefSched.update(func(f *briefSchedFile) bool {
		f.Schedule.Updated, f.Run.Floor = base, base
		return true
	})
}

func schedView(t *testing.T, s *Server) briefSchedView {
	t.Helper()
	var r struct {
		Schedule briefSchedView `json:"schedule"`
	}
	w := do(s, "GET", "/api/brief", userHdr, "")
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return r.Schedule
}

func promptCount(fh *fakeLaunchHerdr) int {
	n := 0
	for _, m := range fh.methods() {
		if m == "agent.prompt" {
			n++
		}
	}
	return n
}

func TestBriefScheduleNoCoordinator(t *testing.T) {
	s, fh, _ := coordServer(t)
	ctx := context.Background()
	if v := schedView(t, s); v.State != "off" || v.Missed != nil {
		t.Fatalf("default = %+v", v)
	}
	// Set ten hours ago (the slots are in the past, so the briefing
	// published now satisfies them).
	base := time.Now().Add(-10 * time.Hour).Truncate(time.Minute)
	schedAt(t, s, base)
	if v := schedView(t, s); v.State != "ok" || !strings.Contains(v.By, "coordinator") || v.Every != "1h" {
		t.Fatalf("set = %+v", v)
	}

	// The slot under way when it was set does not fire.
	s.briefTick(ctx, base.Add(30*time.Second))
	if v := schedView(t, s); v.Missed != nil || promptCount(fh) != 0 {
		t.Fatalf("floor slot fired: %+v", v)
	}
	// No coordinator: missed at once, counted once per slot.
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	s.briefTick(ctx, base.Add(time.Hour+90*time.Second))
	v := schedView(t, s)
	if v.Missed == nil || v.Missed.Count != 1 || v.Missed.Reason != missNoCoordinator || v.Missed.Dismissed {
		t.Fatalf("first miss = %+v", v.Missed)
	}
	s.briefTick(ctx, base.Add(2*time.Hour+30*time.Second))
	if v = schedView(t, s); v.Missed.Count != 2 || !v.Missed.Since.Equal(base.Add(time.Hour)) {
		t.Fatalf("second miss = %+v", v.Missed)
	}
	// Dismissed on the hub, for this streak; a wrong id is refused.
	if w := do(s, "POST", "/api/brief/schedule/dismiss", userHdr, `{"id":"nope"}`); w.Code != http.StatusNotFound {
		t.Fatalf("wrong id = %d", w.Code)
	}
	if w := do(s, "POST", "/api/brief/schedule/dismiss", userHdr, `{"id":"`+v.Missed.ID+`"}`); w.Code != 200 {
		t.Fatalf("dismiss = %d %s", w.Code, w.Body)
	}
	if v = schedView(t, s); !v.Missed.Dismissed {
		t.Fatalf("not dismissed: %+v", v.Missed)
	}
	// A coordinator appears: the next slot prompts it, once; missed slots
	// are not replayed.
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	s.briefTick(ctx, base.Add(3*time.Hour+30*time.Second))
	s.briefTick(ctx, base.Add(3*time.Hour+90*time.Second))
	if n := promptCount(fh); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
	p := fh.call("agent.prompt").Params
	if p["target"] != "w1:p1" || !strings.Contains(p["text"].(string), "comebye brief set") {
		t.Fatalf("prompt = %v", p)
	}
	if v = schedView(t, s); v.State != "prompted" || !strings.Contains(v.PromptedTo, "desk/w1:p1") || v.Missed.Count != 2 {
		t.Fatalf("prompted = %+v", v)
	}
	// It publishes: the streak ends.
	if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), `{"markdown":"## all quiet"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	s.briefTick(ctx, base.Add(3*time.Hour+150*time.Second))
	if v = schedView(t, s); v.Missed != nil || v.State != "ok" {
		t.Fatalf("after the briefing = %+v", v)
	}
}

func TestBriefScheduleBusyAndUnanswered(t *testing.T) {
	s, fh, _ := coordServer(t)
	ctx := context.Background()
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = herdr.StatusWorking })
	s.manager.Refresh()
	waitFor(t, func() bool {
		for _, a := range s.fleet.Agents() {
			if a.PaneID == "w1:p1" {
				return a.Status == herdr.StatusWorking
			}
		}
		return false
	})
	base := time.Now().Add(-10 * time.Hour).Truncate(time.Minute)
	schedAt(t, s, base)

	// Busy: waiting, not missed, until the next slot.
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	if v := schedView(t, s); v.State != "waiting" || v.Missed != nil || promptCount(fh) != 0 {
		t.Fatalf("busy = %+v", v)
	}
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = herdr.StatusIdle })
	s.manager.Refresh()
	waitFor(t, func() bool {
		a, _ := s.briefCoordinator()
		return a != nil
	})
	s.briefTick(ctx, base.Add(2*time.Hour+30*time.Second))
	v := schedView(t, s)
	if v.Missed == nil || v.Missed.Count != 1 || v.Missed.Reason != missBusy || v.State != "prompted" || promptCount(fh) != 1 {
		t.Fatalf("busy slot closed = %+v (prompts %d)", v, promptCount(fh))
	}
	// Prompted but nothing published by the next slot.
	s.briefTick(ctx, base.Add(3*time.Hour+30*time.Second))
	if v = schedView(t, s); v.Missed.Count != 2 || v.Missed.Reason != missNoBriefing || promptCount(fh) != 2 {
		t.Fatalf("unanswered = %+v (prompts %d)", v, promptCount(fh))
	}
	// Turning it off clears the state but keeps the interval.
	if w := do(s, "POST", "/api/brief/schedule", userHdr, `{"enabled":false}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if v = schedView(t, s); v.State != "off" || v.Missed != nil || v.Every != "1h" {
		t.Fatalf("off = %+v", v)
	}
	s.briefTick(ctx, base.Add(4*time.Hour+30*time.Second))
	if promptCount(fh) != 2 {
		t.Fatal("prompted while off")
	}
}

func TestBriefScheduleValidation(t *testing.T) {
	s, _, _ := coordServer(t)
	for _, body := range []string{
		`{"enabled":true}`,
		`{"enabled":true,"every":"10m"}`,
		`{"enabled":true,"every":"24h","at":"8am"}`,
		`{"enabled":true,"every":"24h","tz":"Nowhere/Land"}`,
		`{"enabled":false,"every":"1m"}`,
	} {
		if w := do(s, "POST", "/api/brief/schedule", userHdr, body); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_schedule") {
			t.Errorf("%s = %d %s", body, w.Code, w.Body)
		}
	}
	w := do(s, "POST", "/api/brief/schedule", userHdr, `{"enabled":true,"every":"1440m","at":"7:05","tz":"UTC"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"every":"24h"`) || !strings.Contains(w.Body.String(), `"at":"07:05"`) || !strings.Contains(w.Body.String(), `"by":"me@example.com"`) {
		t.Fatalf("normalized = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/brief/schedule/dismiss", userHdr, `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("dismiss nothing = %d", w.Code)
	}
	// It survives a restart.
	s2 := New(Config{HostName: "desk", StateDir: s.cfg.StateDir}, nil, nil)
	if v := s2.briefSched.get().Schedule; !v.Enabled || v.At != "07:05" {
		t.Fatalf("reloaded = %+v", v)
	}
}
