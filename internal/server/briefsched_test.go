package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/state"
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

// schedPrompts counts the scheduler's prompts only (a to-do answer prompts
// its waiting agent too).
func schedPrompts(fh *fakeLaunchHerdr) int {
	n := 0
	fh.mu.Lock()
	defer fh.mu.Unlock()
	for _, c := range fh.calls {
		if txt, _ := c.Params["text"].(string); c.Method == "agent.prompt" && strings.HasPrefix(txt, "Scheduled briefing") {
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

// When the hub cannot tell whether a coordinator is running (the recorded
// one is not in the fleet), the slot is neither missed nor dismissed: the
// next tick asks again and prompts the coordinator once it is back.
func TestBriefScheduleUnknownRetries(t *testing.T) {
	s, fh, _ := coordServer(t)
	ctx := context.Background()
	base := time.Now().Add(-10 * time.Hour).Truncate(time.Minute)
	schedAt(t, s, base)
	if err := s.roles.set("desk/gone:p9", Role{Role: RoleCoordinator, Name: "coordinator", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	s.briefTick(ctx, base.Add(time.Hour+90*time.Second))
	if v := schedView(t, s); v.Missed != nil || promptCount(fh) != 0 {
		t.Fatalf("counted a miss when it could not tell: %+v", v)
	}
	// The coordinator is back (a vanished name still counts).
	if err := s.roles.set("desk/gone:p9", Role{}); err != nil {
		t.Fatal(err)
	}
	if err := s.roles.set("desk/w1:p1", Role{Role: RoleCoordinator, Name: "coordinator", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.briefTick(ctx, base.Add(time.Hour+150*time.Second))
	if v := schedView(t, s); v.State != "prompted" || v.Missed != nil || promptCount(fh) != 1 {
		t.Fatalf("not prompted after it was back: %+v", v)
	}
}

// quietSetup: an idle coordinator, an hourly schedule set just now, a
// briefing published (its fingerprint is taken), and the first slot after it.
func quietSetup(t *testing.T) (s *Server, fh *fakeLaunchHerdr, base time.Time) {
	t.Helper()
	s, fh, _ = coordServer(t)
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a, _ := s.briefCoordinator(); return a != nil })
	base = time.Now().Truncate(time.Minute)
	schedAt(t, s, base)
	if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), `{"markdown":"## all quiet"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	return s, fh, base
}

func TestBriefScheduleSkipsUnchanged(t *testing.T) {
	s, fh, base := quietSetup(t)
	ctx := context.Background()
	// Nothing changed (the coordinator going working/idle does not count):
	// no prompt, however many ticks, and the briefing is marked checked.
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = herdr.StatusWorking })
	s.manager.Refresh()
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	s.briefTick(ctx, base.Add(time.Hour+90*time.Second))
	v := schedView(t, s)
	if schedPrompts(fh) != 0 || v.CheckedAt == nil || v.State != "ok" || v.Missed != nil {
		t.Fatalf("unchanged = %+v (prompts %d)", v, schedPrompts(fh))
	}
	// The next slot looks again and is also left alone; still no miss.
	s.briefTick(ctx, base.Add(2*time.Hour+30*time.Second))
	if v = schedView(t, s); schedPrompts(fh) != 0 || v.Missed != nil || !v.CheckedAt.After(base.Add(2*time.Hour)) {
		t.Fatalf("second slot = %+v", v)
	}
	// A newer briefing replaces the note.
	time.Sleep(1100 * time.Millisecond)
	if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), `{"markdown":"## still quiet"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if v = schedView(t, s); v.CheckedAt != nil {
		t.Fatalf("checked note outlived its briefing: %+v", v)
	}
}

func TestBriefScheduleSkipNoCoordinatorIsNoMiss(t *testing.T) {
	s, fh, base := quietSetup(t)
	fh.set(func(f *fakeLaunchHerdr) { f.agents = nil })
	s.manager.Refresh()
	s.briefTick(context.Background(), base.Add(time.Hour+30*time.Second))
	if v := schedView(t, s); v.Missed != nil {
		t.Fatalf("a quiet slot counted as missed: %+v", v.Missed)
	}
}

func TestBriefSchedulePromptsOnChange(t *testing.T) {
	changes := map[string]func(t *testing.T, s *Server, fh *fakeLaunchHerdr){
		"a to-do appears": func(t *testing.T, s *Server, _ *fakeLaunchHerdr) {
			if w := do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"ship it?"}`); w.Code != 201 {
				t.Fatal(w.Body)
			}
		},
		"a to-do is answered": nil, // set below: needs a to-do before the briefing
		"a worker files a report": func(t *testing.T, s *Server, _ *fakeLaunchHerdr) {
			if _, ok, err := s.ledger.Report("desk/w9:p1", "done", time.Now()); err != nil || !ok {
				t.Fatalf("report: %v %v", ok, err)
			}
		},
		"an agent is done": func(t *testing.T, s *Server, fh *fakeLaunchHerdr) {
			fh.set(func(f *fakeLaunchHerdr) { f.agents[1].Status = herdr.StatusDone })
			s.manager.Refresh()
			waitFor(t, func() bool {
				for _, a := range s.fleet.Agents() {
					if a.PaneID == "w2:p1" {
						return a.Status == herdr.StatusDone
					}
				}
				return false
			})
		},
		"a repository is damaged": func(t *testing.T, s *Server, _ *fakeLaunchHerdr) {
			s.fleet = fleet.New(s.manager, "desk", nil, fleet.Options{Logf: func(string, ...any) {}, RepoDamage: func() []state.RepoDamage {
				return []state.RepoDamage{{Repo: "proj", Path: "/r/proj", Source: "fetch", Since: time.Now()}}
			}})
		},
		"a host runs another version": func(t *testing.T, s *Server, _ *fakeLaunchHerdr) {
			s.fleet = fleet.New(s.manager, "desk", nil, fleet.Options{Logf: func(string, ...any) {}, Version: "v9.9.9"})
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, fh, _ := coordServer(t)
			if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
				t.Fatal(err)
			}
			// State the briefing is published against.
			var todoID string
			if w := do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"first?"}`); w.Code != 201 {
				t.Fatal(w.Body)
			} else {
				todoID = todoOf(t, w.Body.Bytes()).ID
			}
			if _, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "b", AgentID: "desk/w9:p1", Status: ledger.Running}); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { a, _ := s.briefCoordinator(); return a != nil })
			base := time.Now().Truncate(time.Minute)
			schedAt(t, s, base)
			if w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), `{"markdown":"## now"}`); w.Code != 200 {
				t.Fatal(w.Body)
			}
			if change == nil {
				change = func(t *testing.T, s *Server, _ *fakeLaunchHerdr) {
					if w := do(s, "POST", "/api/todos/"+todoID+"/answer", userHdr, `{"text":"yes"}`); w.Code != 200 {
						t.Fatalf("answer = %d %s", w.Code, w.Body)
					}
				}
			}
			change(t, s, fh)
			s.briefTick(context.Background(), base.Add(time.Hour+30*time.Second))
			if v := schedView(t, s); schedPrompts(fh) != 1 || v.State != "prompted" || v.CheckedAt != nil {
				t.Fatalf("after %q: prompts %d, %+v", name, schedPrompts(fh), v)
			}
		})
	}
}

func TestBriefScheduleSafetyNetAndSwitch(t *testing.T) {
	s, fh, base := quietSetup(t)
	ctx := context.Background()
	// Quiet for under 6h (the default): left alone. Past it: asked anyway.
	s.briefTick(ctx, base.Add(5*time.Hour+30*time.Second))
	if schedPrompts(fh) != 0 {
		t.Fatal("prompted inside the safety net")
	}
	s.briefTick(ctx, base.Add(7*time.Hour))
	if schedPrompts(fh) != 1 {
		t.Fatalf("prompts = %d after the safety net", schedPrompts(fh))
	}

	// A shorter net, set through the API; the briefing is kept.
	s, fh, base = quietSetup(t)
	if w := do(s, "POST", "/api/brief/schedule", userHdr, `{"enabled":true,"every":"1h","max_quiet":"2h"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	_ = s.briefSched.update(func(f *briefSchedFile) bool { f.Schedule.Updated, f.Run.Floor = base, base; return true })
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	s.briefTick(ctx, base.Add(3*time.Hour))
	if schedPrompts(fh) != 1 {
		t.Fatalf("prompts = %d with max_quiet 2h", schedPrompts(fh))
	}
	for _, bad := range []string{`"max_quiet":"30m"`, `"max_quiet":"soon"`} {
		if w := do(s, "POST", "/api/brief/schedule", userHdr, `{"enabled":true,"every":"1h",`+bad+`}`); w.Code != 400 {
			t.Errorf("%s = %d", bad, w.Code)
		}
	}

	// always: every slot prompts although nothing changed.
	s, fh, base = quietSetup(t)
	if w := do(s, "POST", "/api/brief/schedule", userHdr, `{"enabled":true,"every":"1h","always":true}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	_ = s.briefSched.update(func(f *briefSchedFile) bool { f.Schedule.Updated, f.Run.Floor = base, base; return true })
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	if v := schedView(t, s); schedPrompts(fh) != 1 || !v.Always {
		t.Fatalf("always: prompts %d, %+v", schedPrompts(fh), v)
	}
}

func TestBriefScheduleFingerprintSurvivesRestart(t *testing.T) {
	s, fh, base := quietSetup(t)
	ctx := context.Background()
	// The hub restarts: the schedule file is read again.
	re, err := openBriefSched(s.briefSched.path)
	if err != nil {
		t.Fatal(err)
	}
	s.briefSched = re
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	if schedPrompts(fh) != 0 {
		t.Fatal("a restart woke the coordinator for nothing")
	}
	// Something changed while the hub was down: the restart does not hide it.
	if w := do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"while you were out"}`); w.Code != 201 {
		t.Fatal(w.Body)
	}
	re, err = openBriefSched(s.briefSched.path)
	if err != nil {
		t.Fatal(err)
	}
	s.briefSched = re
	s.briefTick(ctx, base.Add(2*time.Hour+30*time.Second))
	if schedPrompts(fh) != 1 {
		t.Fatalf("prompts = %d, want 1", schedPrompts(fh))
	}

	// No fingerprint on file (a briefing from before this feature): ask.
	s, fh, base = quietSetup(t)
	_ = s.briefSched.update(func(f *briefSchedFile) bool { f.Run.Fingerprint, f.Run.FingerprintFor = "", time.Time{}; return true })
	s.briefTick(ctx, base.Add(time.Hour+30*time.Second))
	if schedPrompts(fh) != 1 {
		t.Fatalf("no fingerprint: prompts = %d", schedPrompts(fh))
	}
}
