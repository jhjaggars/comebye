package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// settleOnce runs the due landing checks and waits for them to end.
func settleOnce(t *testing.T, s *Server) {
	t.Helper()
	s.checkLandings()
	waitFor(t, func() bool {
		s.coord.mu.Lock()
		defer s.coord.mu.Unlock()
		for k, v := range s.coord.checking {
			if strings.HasPrefix(k, "land/") && v {
				return false
			}
		}
		return true
	})
}

// told marks the requester's notice for the worker's first report delivered,
// so a notice found later is a new one.
func told(s *Server, id string) {
	if g, ok := s.ledger.Get(id); ok && g.Callback != nil {
		s.ledger.CallbackDone(id, g.Callback.At, ledger.CallbackDelivered, "desk/w1:p1", time.Now())
	}
}

func progress(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	w := do(s, "POST", "/api/report", agentHdr("w2:p1"), body)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// The bug: a worker mid-landing sent a progress note with `comebye report`;
// the hub took it for the landing report, found nothing on main and showed
// "land failed". Now a progress note is not a report at all.
func TestProgressNoteIsNotALandingReport(t *testing.T) {
	s, _, e, _ := landingFix(t)
	told(s, e.ID)
	if code, _ := land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`); code != 200 {
		t.Fatal("land")
	}
	if code, out := progress(t, s, `{"text":"the full UI suite is running; I will push once it passes","progress":true,"phase":"tests"}`); code != 200 {
		t.Fatalf("progress = %d %v", code, out)
	}
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Landing || g.FinishedAt != nil || g.Progress == nil || g.Progress.Phase != "tests" {
		t.Fatalf("after the note: %s finished=%v progress=%+v", g.Status, g.FinishedAt, g.Progress)
	}
	if g.Callback != nil && g.Callback.State == ledger.CallbackPending {
		t.Fatal("a progress note notified the requester")
	}
	settleOnce(t, s)
	g, _ = s.ledger.Get(e.ID)
	if g.Status != ledger.Landing {
		t.Fatalf("a check between the note and the push moved the entry to %s (%+v)", g.Status, g.LandCheck)
	}
	// The hub did look, and says what is true: nothing on main yet.
	if g.LandCheck == nil || g.LandCheck.StateOf() != ledger.LandPending || g.LandCheck.Unmerged < 1 {
		t.Fatalf("land check = %+v", g.LandCheck)
	}
	if code, out := progress(t, s, `{"text":"x","progress":true,"phase":"bogus"}`); code != 400 || out["code"] != "bad_phase" {
		t.Fatalf("bad phase = %d %v", code, out)
	}
}

// A plain report sent as a progress note by a worker that does not know the
// flag (or by mistake) still does not fail the landing: a report this fresh
// gets a grace, and the entry stays landing, "not pushed yet".
func TestFreshReportWithNothingPushedStaysLanding(t *testing.T) {
	s, _, e, _ := landingFix(t)
	told(s, e.ID)
	land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`)
	if _, ok, _ := s.ledger.Report("desk/w2:p1", "tests are running in the background; I will push once they pass", time.Now()); !ok {
		t.Fatal("report")
	}
	settleOnce(t, s)
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Landing || g.LandCheck == nil || g.LandCheck.StateOf() != ledger.LandPending {
		t.Fatalf("fresh report: %s %+v", g.Status, g.LandCheck)
	}
	if g.Callback != nil && g.Callback.State == ledger.CallbackPending {
		t.Fatal("a pending landing notified the requester")
	}
}

// The worker says it is done, nothing runs, and the work is not on main:
// that is a failure, with the reason in the worker's terms.
func TestDoneWithNothingPushedFails(t *testing.T) {
	s, _, e, _ := landingFix(t)
	told(s, e.ID)
	land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`)
	s.ledger.Report("desk/w2:p1", "## Summary\nI could not push", time.Now().Add(-10*time.Minute))
	settleOnce(t, s)
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Finished || g.LandCheck == nil || g.LandCheck.StateOf() != ledger.LandFailed {
		t.Fatalf("done, not pushed: %s %+v", g.Status, g.LandCheck)
	}
	if !strings.Contains(g.LandCheck.Reason, "said it was done") && !strings.Contains(g.LandCheck.Reason, "session is gone") {
		t.Errorf("reason = %q", g.LandCheck.Reason)
	}
	if g.Callback == nil || g.Callback.State != ledger.CallbackPending {
		t.Fatal("a failure is told to the requester")
	}
	if txt := callbackText(g, false); !strings.Contains(txt, "landing failed") || !strings.Contains(txt, "back to finished") {
		t.Fatalf("notice = %s", txt)
	}
}

// Landing is noticed when it happens, without the worker reporting.
func TestLandedBeforeTheWorkerReports(t *testing.T) {
	s, r, e, own := landingFix(t)
	told(s, e.ID)
	land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`)
	settleOnce(t, s) // not pushed yet
	if g, _ := s.ledger.Get(e.ID); g.Status != ledger.Landing {
		t.Fatalf("before the push: %s", g.Status)
	}
	// The worker pushes (and tidies up) while its tests/CI watch run; it has
	// not reported.
	r.landCherryPick(t)
	git(t, r.wt, "fetch", "-q", "origin")
	git(t, r.wt, "switch", "-q", "--detach", "origin/main")
	git(t, r.wt, "branch", "-q", "-D", "w")
	s.coord.mu.Lock()
	delete(s.coord.landSched, e.ID) // the schedule would wait; the origin moved
	s.coord.mu.Unlock()
	settleOnce(t, s)
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Landed || !g.AwaitLandReport || g.LandCheck == nil || !g.LandCheck.OK {
		t.Fatalf("landed early: %s await=%v %+v (tip %s)", g.Status, g.AwaitLandReport, g.LandCheck, own)
	}
	if g.Callback == nil || g.Callback.State != ledger.CallbackPending {
		t.Fatal("the requester is told as soon as it is on main")
	}
	if txt := callbackText(g, false); !strings.Contains(txt, "has not filed its landing report yet") || !strings.Contains(txt, "it is landed") {
		t.Fatalf("notice = %s", txt)
	}
	// A working worker does not start a new round, and its landing report
	// completes the turn.
	s.ledger.Observe([]herdr.AgentInfo{{ID: "desk/w2:p1", Status: herdr.StatusWorking}}, time.Now())
	if g, _ = s.ledger.Get(e.ID); g.Status != ledger.Landed {
		t.Fatalf("working after an early landing: %s", g.Status)
	}
	if _, ok, _ := s.ledger.Report("desk/w2:p1", "## Summary\nlanded, CI green", time.Now()); !ok {
		t.Fatal("report")
	}
	g, _ = s.ledger.Get(e.ID)
	if g.Status != ledger.Landed || g.AwaitLandReport || g.FinalReport == "" || !strings.Contains(g.FinalReport, "CI green") {
		t.Fatalf("after the landing report: %s await=%v", g.Status, g.AwaitLandReport)
	}
}

// CI of the landed commit is read, shown, and told once it ends.
func TestLandedCIIsFollowedAndTold(t *testing.T) {
	s, r, e, _ := landingFix(t)
	land(t, s, `{"agent_id":"desk/w2:p1","note":"ok"}`)
	r.landCherryPick(t)
	git(t, r.wt, "fetch", "-q", "origin")
	git(t, r.wt, "switch", "-q", "--detach", "origin/main")
	git(t, r.wt, "branch", "-q", "-D", "w")
	runs := `[{"status":"in_progress","conclusion":"","url":"https://ci/1","name":"test"}]`
	old := ghRuns
	ghRuns = func(_ context.Context, _, sha string) ([]byte, error) { return []byte(runs), nil }
	defer func() { ghRuns = old }()
	settleOnce(t, s) // landed
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Landed {
		t.Fatalf("landed: %s %+v", g.Status, g.LandCheck)
	}
	s.coord.mu.Lock()
	delete(s.coord.landSched, e.ID)
	s.coord.mu.Unlock()
	settleOnce(t, s) // CI
	g, _ = s.ledger.Get(e.ID)
	if g.LandCheck.CI == nil || g.LandCheck.CI.State != ledger.CIRunning {
		t.Fatalf("ci = %+v", g.LandCheck.CI)
	}
	s.ledger.CallbackDone(g.ID, g.Callback.At, ledger.CallbackDelivered, "desk/w1:p1", time.Now())
	runs = `[{"status":"completed","conclusion":"success","url":"https://ci/1","name":"test"}]`
	s.coord.mu.Lock()
	delete(s.coord.landSched, e.ID)
	s.coord.mu.Unlock()
	settleOnce(t, s)
	g, _ = s.ledger.Get(e.ID)
	if g.LandCheck.CI.State != ledger.CISuccess || !g.LandCheck.CI.Told || g.Callback.State != ledger.CallbackPending {
		t.Fatalf("ci ended: %+v callback %+v", g.LandCheck.CI, g.Callback)
	}
	if txt := callbackText(g, false); !strings.Contains(txt, "CI is green") {
		t.Fatalf("notice = %s", txt)
	}
	if got := s.ledger.LandingsToCheck(); len(got) != 0 {
		t.Fatalf("a landed entry with settled CI is still followed: %+v", got)
	}
}

func TestLandFailureDecision(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	idle := landSignals{Now: now, Present: true, Status: herdr.StatusIdle, BgKnown: true}
	cases := []struct {
		name   string
		e      ledger.Entry
		sg     landSignals
		failed bool
		why    string
	}{
		{"no report yet", ledger.Entry{}, idle, false, "has not reported"},
		{"just reported", ledger.Entry{FinishedAt: ago(30 * time.Second), ReportSource: "explicit"}, idle, false, "checking again"},
		{"done, idle, nothing running", ledger.Entry{FinishedAt: ago(5 * time.Minute), ReportSource: "explicit"}, idle, true, "said it was done"},
		{"went idle after working, nothing running", ledger.Entry{FinishedAt: ago(5 * time.Minute)}, idle, true, "stopped"},
		{"still working", ledger.Entry{FinishedAt: ago(5 * time.Minute), ReportSource: "explicit"}, landSignals{Now: now, Present: true, Status: herdr.StatusWorking, BgKnown: true}, false, "still working"},
		{"waiting on a background task", ledger.Entry{FinishedAt: ago(5 * time.Minute), ReportSource: "explicit"}, landSignals{Now: now, Present: true, Status: herdr.StatusIdle, BgKnown: true, Bg: 1, BgDesc: "ui suite"}, false, "background task (ui suite)"},
		{"newer progress note", ledger.Entry{FinishedAt: ago(5 * time.Minute), ReportSource: "explicit", Progress: &ledger.Progress{Phase: "tests", At: now.Add(-time.Minute)}}, idle, false, "tests running"},
		{"idle, background unreadable, not a report", ledger.Entry{FinishedAt: ago(5 * time.Minute)}, landSignals{Now: now, Present: true, Status: herdr.StatusIdle}, false, "cannot be read"},
		{"done, background unreadable", ledger.Entry{FinishedAt: ago(5 * time.Minute), ReportSource: "explicit"}, landSignals{Now: now, Present: true, Status: herdr.StatusIdle}, true, "said it was done"},
		{"session gone", ledger.Entry{FinishedAt: ago(5 * time.Minute)}, landSignals{Now: now}, true, "session is gone"},
	}
	for _, c := range cases {
		failed, why := landFailure(c.e, c.sg)
		if failed != c.failed || !strings.Contains(why, c.why) {
			t.Errorf("%s: failed=%v why=%q, want failed=%v containing %q", c.name, failed, why, c.failed, c.why)
		}
	}
}

func TestLandSchedulerBacksOffAndResetsOnAReport(t *testing.T) {
	s, _, e, _ := landingFix(t)
	now := time.Now()
	if !s.landDue(e, now) {
		t.Fatal("never checked: due")
	}
	s.landNext(e, now, landFirst, landMax, false)
	if s.landDue(e, now.Add(landFirst-time.Second)) || !s.landDue(e, now.Add(landFirst)) {
		t.Fatal("first re-check after landFirst")
	}
	at := now
	var prev time.Duration
	for i := 0; i < 30; i++ {
		at = at.Add(s.coord.landSched[e.ID].wait)
		s.landNext(e, at, landFirst, landMax, false)
		w := s.coord.landSched[e.ID].wait
		if w < prev || w > landMax {
			t.Fatalf("wait %v after %v (max %v)", w, prev, landMax)
		}
		prev = w
	}
	if prev != landMax {
		t.Fatalf("the wait settles at the cap, got %v", prev)
	}
	// stopped: not due until a report
	s.landNext(e, at, landFirst, landMax, true)
	if s.landDue(e, at.Add(time.Hour)) {
		t.Fatal("a stopped landing is checked again")
	}
	t2 := at.Add(time.Minute)
	e.FinishedAt = &t2
	if !s.landDue(e, at) {
		t.Fatal("a new report checks at once")
	}
}

func TestBackgroundTasksOfAWorker(t *testing.T) {
	s, _, _, _ := landingFix(t)
	e, _ := s.ledger.ByAgent("desk/w2:p1")
	s.latest = func(_ context.Context, host, pane string) *transcript.Latest {
		return &transcript.Latest{Background: []transcript.BackgroundTask{{ID: "b1", Tool: "Bash", Desc: "ui suite"}}, BackgroundKnown: true}
	}
	tasks, known := s.backgroundOf(context.Background(), e, true)
	if !known || len(tasks) != 1 || tasks[0].Desc != "ui suite" {
		t.Fatalf("tasks = %+v known=%v", tasks, known)
	}
	// An agent kind that cannot say is "unknown", not "none".
	s.coord.mu.Lock()
	delete(s.coord.bg, e.AgentID)
	s.coord.mu.Unlock()
	s.latest = func(context.Context, string, string) *transcript.Latest { return &transcript.Latest{} }
	if _, known := s.backgroundOf(context.Background(), e, true); known {
		t.Fatal("a session that cannot say reported as known")
	}
}

func TestCIFromRuns(t *testing.T) {
	for _, c := range []struct{ name, json, want string }{
		{"none", `[]`, ledger.CINone},
		{"running", `[{"status":"in_progress","conclusion":""},{"status":"completed","conclusion":"success"}]`, ledger.CIRunning},
		{"queued", `[{"status":"queued","conclusion":""}]`, ledger.CIRunning},
		{"green", `[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"skipped"}]`, ledger.CISuccess},
		{"failed", `[{"status":"completed","conclusion":"failure","url":"https://ci/f"},{"status":"in_progress"}]`, ledger.CIFailure},
		{"cancelled", `[{"status":"completed","conclusion":"cancelled"}]`, ledger.CIFailure},
	} {
		ci := ciFromRuns([]byte(c.json), "abc1234")
		if ci == nil || ci.State != c.want {
			t.Errorf("%s: %+v, want %s", c.name, ci, c.want)
		}
	}
	if ciFromRuns([]byte("not json"), "abc1234") != nil {
		t.Error("garbage is not a state")
	}
	if ci := ciFromRuns([]byte(`[{"status":"completed","conclusion":"failure","url":"https://ci/f"}]`), "abc1234"); ci.URL != "https://ci/f" {
		t.Errorf("the failing run's link: %+v", ci)
	}
}
