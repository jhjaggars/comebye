package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Marking a session for removal must not interrupt one still on its task.
// The cases are from 2026-11-23 (UTC):
//
//   - stuck-scratch, landing PR #96 as told by a queued prompt, pushed to
//     main, saw one flaky browser check, started `gh run rerun … && gh run
//     watch` in the background (11:25:54) and its turn ended (11:26:02):
//     the ledger called it finished by its going idle. The 11:32 sweep
//     marked it and asked "what is left?"; the ask said to take no other
//     action, so it listed the landing report it owed, "when told to go
//     ahead", and was asked the same three more times, never filing it.
//   - bee-plugin, marked and asked at 12:32, was told by the user to carry
//     on; at 14:31 it opened PR #379 and watched its checks in the
//     background. The 14:36 re-ask came in, and it stopped its own CI watch
//     ("stop your own throwaway processes") to answer.
//   - quiet-thinking and codemode had filed their final reports before the
//     sweep: marking them was right.

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func tp(t time.Time) *time.Time { return &t }

func TestSweepHold(t *testing.T) {
	report := utc("2026-11-23T11:15:19Z") // stuck-scratch's PR report (before the landing prompt)
	idleAt := utc("2026-11-23T11:26:02Z") // its landing turn ended, the CI re-run watched in the background
	sweepAt := utc("2026-11-23T11:32:25Z")
	stuck := ledger.Entry{Status: ledger.Finished, ReportSource: "explicit", FinalReport: "PR #96 is open", ReportAt: tp(report), FinishedAt: tp(utc("2026-11-23T11:26:08Z"))}
	idle := &herdr.AgentInfo{Status: herdr.StatusDone, StatusSince: tp(idleAt)}
	ciWatch := []transcript.BackgroundTask{{ID: "bfg0tygm6", Tool: "Bash", Cmd: true, Desc: "gh run rerun 45164015645 --failed && sleep 5 && gh run watch 45164015645", Since: "2026-11-23T11:25:55Z"}}
	quietAt := utc("2026-11-23T11:15:21Z")
	quiet := ledger.Entry{Status: ledger.Finished, ReportSource: "explicit", FinalReport: "PR #95 is on main", ReportAt: tp(quietAt), FinishedAt: tp(quietAt)}
	quietIdle := &herdr.AgentInfo{Status: herdr.StatusIdle, StatusSince: tp(utc("2026-11-23T11:15:26Z"))}

	for _, c := range []struct {
		name string
		e    ledger.Entry
		live *herdr.AgentInfo
		bg   []transcript.BackgroundTask
		now  time.Time
		want string // "" or a part of the reason
	}{
		{"stuck-scratch: a CI re-run watched in the background", stuck, idle, ciWatch, sweepAt, "waiting on a background task (gh run)"},
		{"a scheduled wakeup", quiet, quietIdle, []transcript.BackgroundTask{{Tool: "Wakeup", Desc: "watching CI run"}}, sweepAt, "a scheduled wakeup (watching CI run)"},
		{"stuck-scratch with nothing in the background: the turn after its report ended moments ago", stuck, idle, nil, sweepAt, "last turn ended without a report"},
		{"the same, idle long enough", stuck, idle, nil, idleAt.Add(defaultSweepIdle + time.Minute), ""},
		{"quiet-thinking: it reported, then went idle", quiet, quietIdle, nil, sweepAt, ""},
		{"landed, its landing report not filed yet", ledger.Entry{Status: ledger.Landed, AwaitLandReport: true, ReportSource: "explicit", ReportAt: tp(report), FinishedAt: tp(report)}, idle, nil, sweepAt, "landing report is not filed"},
		{"never reported, finished by going idle", ledger.Entry{Status: ledger.Finished, FinishedAt: tp(idleAt)}, idle, nil, sweepAt, "without a report"},
		{"its agent is gone: nothing to interrupt", stuck, nil, ciWatch, sweepAt, ""},
	} {
		got := sweepHold(c.e, c.live, c.bg, defaultSweepIdle, c.now)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// bgLatest makes every session report the tasks bg() returns as running.
func bgLatest(s *Server, bg func(pane string) []transcript.BackgroundTask) {
	prev := s.latest
	s.latest = func(ctx context.Context, host, pane string) *transcript.Latest {
		var l transcript.Latest
		if prev != nil {
			if p := prev(ctx, host, pane); p != nil {
				l = *p
			}
		}
		l.Background, l.BackgroundKnown = bg(pane), true
		return &l
	}
}

func TestSweepDoesNotMarkAWorkerWaitingOnABackgroundTask(t *testing.T) {
	f := newSweepFix(t)
	var mu sync.Mutex
	running := true
	merged, _ := f.s.ledger.ByAgent(f.ids["merged"])
	bgLatest(f.s, func(pane string) []transcript.BackgroundTask {
		mu.Lock()
		defer mu.Unlock()
		if running && pane == merged.PaneID {
			return []transcript.BackgroundTask{{ID: "bfg0tygm6", Tool: "Bash", Desc: "re-run CI and watch it", Since: time.Now().UTC().Format(time.RFC3339Nano)}}
		}
		return nil
	})
	r := sweepOf(t, f.s, `{}`)
	if c, why := classOf(r, f.ids["merged"]); c != SweepSkip || !strings.Contains(why, "still on its task: waiting on a background task (re-run CI and watch it)") {
		t.Fatalf("a worker waiting on its CI watch: %s %q", c, why)
	}
	if st := statusOf(f.s, f.ids["merged"]); st != ledger.Finished {
		t.Fatalf("it was marked: %s", st)
	}
	if n := removalAsks(f.fh); n != 0 {
		t.Fatalf("it was asked %d times", n)
	}
	// The task ended: the next sweep marks it as before.
	mu.Lock()
	running = false
	mu.Unlock()
	f.s.coord.mu.Lock()
	delete(f.s.coord.bg, f.ids["merged"]) // the read is cached for bgTTL
	f.s.coord.mu.Unlock()
	r = sweepOf(t, f.s, `{}`)
	if c, why := classOf(r, f.ids["merged"]); c != SweepReady {
		t.Fatalf("after the task ended: %s %q", c, why)
	}
}

// bee-plugin: the hub's re-ask waits while the session watches its CI; a
// person's re-ask is theirs to send.
func TestHubDoesNotReaskASessionWaitingOnATask(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.askPoll, s.askWait = 10*time.Millisecond, 400*time.Millisecond
	t.Cleanup(s.asking.Wait)
	scriptedSession(s, fh, func(string) string { return "LEFT\n- pr jhjaggars/homelab#379 open; waiting on CI, then me" })
	var mu sync.Mutex
	watching := true
	bgLatest(s, func(string) []transcript.BackgroundTask {
		mu.Lock()
		defer mu.Unlock()
		if watching {
			return []transcript.BackgroundTask{{ID: "bngoxbzw5", Tool: "Bash", Desc: "wait for #379's checks"}}
		}
		return nil
	})
	m := markUnsafe(t, s, 2*s.reaskCap, ledger.AskItem{Type: "pr", Repo: "jhjaggars/homelab", Number: 379, Text: "open, waiting on CI"})
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 0 || promptCount(fh) != 0 {
		t.Fatalf("re-asked while it watches CI: %v, %d prompts", got, promptCount(fh))
	}
	if err := s.reask(m, true, ""); err == nil || !strings.Contains(err.Error(), "waiting on a background task (wait for #379's checks)") {
		t.Fatalf("hub re-ask: %v", err)
	}
	mu.Lock()
	watching = false
	mu.Unlock()
	s.coord.mu.Lock()
	delete(s.coord.bg, "desk/w2:p1")
	s.coord.mu.Unlock()
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 1 {
		t.Fatalf("not re-asked once the watch ended: %v", got)
	}
	waitFor(t, func() bool { return promptCount(fh) == 1 })
}

// The hub's go-ahead for scratch waits for a task too.
func TestScratchGoAheadWaitsForATask(t *testing.T) {
	s, fh, _ := coordServer(t)
	scriptedSession(s, fh, func(string) string { return "Deleted it." })
	bgLatest(s, func(string) []transcript.BackgroundTask {
		return []transcript.BackgroundTask{{Tool: "Wakeup", Desc: "check CI again"}}
	})
	markUnsafe(t, s, time.Minute, ledger.AskItem{Type: "path", Name: scratchFile(t), Text: "my scratch"})
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 0 || promptCount(fh) != 0 {
		t.Fatalf("a go-ahead while a wakeup is pending: %v, %d prompts", got, promptCount(fh))
	}
}

// stuck-scratch's answer, word for word.
const owedLanding = "I still owe coordinator-2 the landing report: main is at 8dadd0e, and the re-run of CI run https://github.com/jhjaggars/comebye/actions/runs/45164015645 passed after one flaky browser check (\"split at 1280px dark\", a Pages decision check). I can send it with `comebye report` when told to go ahead. Waiting on a go-ahead."

func TestIsOwedReport(t *testing.T) {
	for _, c := range []struct {
		item ledger.AskItem
		want bool
	}{
		{ledger.AskItem{Type: "other", Text: owedLanding}, true},
		{ledger.AskItem{Type: "other", Text: "the final report: not filed yet"}, true},
		{ledger.AskItem{Type: "cleanup", Text: "send my report with comebye report"}, false}, // nothing says it is owed
		{ledger.AskItem{Type: "other", Text: "a request I owe the user"}, false},
		{ledger.AskItem{Type: "followup", Text: "the user has to read the report I filed and decide; waiting on the user, I still owe nothing"}, false},
		{ledger.AskItem{Type: "pr", Text: "I owe the report on #12"}, false},
	} {
		if got := isOwedReport(c.item); got != c.want {
			t.Errorf("%s %q: %v, want %v", c.item.Type, c.item.Text, got, c.want)
		}
	}
}

// stuck-scratch: an answer whose only item is the report it owes gets the
// hub's go-ahead to file it, even with the re-asks spent, not the same
// question again; filing it ends the mark (the worker reported: a new round).
func TestOwedReportGetsAGoAheadNotAReask(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.askPoll = 10 * time.Millisecond
	t.Cleanup(s.asking.Wait)
	const id = "desk/w2:p1"
	if _, err := s.ledger.Add(ledger.Entry{Host: "desk", AgentID: id, PaneID: "w2:p1", AgentName: "stuck-scratch", Status: ledger.Finished, FinalReport: "PR #96 is open", ReportSource: "explicit"}); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	scriptedSession(s, fh, func(p string) string {
		if strings.HasPrefix(p, "Go ahead") {
			once.Do(func() {
				s.ledger.Report(id, "## Summary\nLanded #96 on main as 8dadd0e; CI green on the re-run.", time.Now())
			})
			return "Filed the landing report."
		}
		return "LEFT\n- other " + owedLanding
	})
	m := markUnsafe(t, s, 2*s.reaskCap, ledger.AskItem{Type: "other", Text: owedLanding})
	s.ledger.UpdateAsk(m.AgentID, m.At, func(a *ledger.Ask) bool { a.Reasks = s.reaskMax; return true })
	it := markedItem(t, s, id)
	if !goAheadDue(it) {
		t.Fatal("no go-ahead due")
	}
	it.AskReview = &askReview{State: ledger.AskUnsafe}
	if s.reaskStale(it, time.Now()) {
		t.Fatal("the review re-asked instead of the go-ahead")
	}
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 1 {
		t.Fatalf("asked = %v", got)
	}
	waitFor(t, func() bool { return len(goAheads(fh)) == 1 })
	p := goAheads(fh)[0]
	for _, want := range []string{"1. " + owedLanding, "Filing the report your task asks for, with `comebye report` as the task says, is part of this."} {
		if !strings.Contains(p, want) {
			t.Errorf("go-ahead lacks %q:\n%s", want, p)
		}
	}
	waitFor(t, func() bool { e, _ := s.ledger.ByAgent(id); return e.Mark == nil })
	s.asking.Wait()
	e, _ := s.ledger.ByAgent(id)
	if e.Status != ledger.Finished || !strings.Contains(e.FinalReport, "8dadd0e") {
		t.Fatalf("entry = %s %q", e.Status, e.FinalReport)
	}
	if n := removalAsks(fh); n != 0 {
		t.Fatalf("asked again %d times", n)
	}
}

// An owed report gets one go-ahead per answer.
func TestOwedReportGoAheadOncePerAnswer(t *testing.T) {
	s, fh, _ := coordServer(t)
	scriptedSession(s, fh, func(string) string { return "Done." })
	m := markUnsafe(t, s, time.Minute, ledger.AskItem{Type: "other", Text: owedLanding})
	s.ledger.UpdateAsk(m.AgentID, m.At, func(a *ledger.Ask) bool { at := a.At; a.AutoCleanFor = &at; return true })
	if goAheadDue(markedItem(t, s, "desk/w2:p1")) {
		t.Fatal("still due after it was sent")
	}
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 0 || promptCount(fh) != 0 {
		t.Fatalf("sent twice: %v, %d prompts", got, promptCount(fh))
	}
}

// removalAsks counts the "what is left?" asks sent.
func removalAsks(fh *fakeLaunchHerdr) int {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	n := 0
	for _, c := range fh.calls {
		if p, _ := c.Params["text"].(string); c.Method == "agent.prompt" && strings.Contains(p, "proposes removing") {
			n++
		}
	}
	return n
}

func markedItem(t *testing.T, s *Server, id string) *removal {
	t.Helper()
	for _, it := range s.marked() {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("%s not marked", id)
	return nil
}

// The ask does not treat a task the session waits on as a throwaway, and
// says how to list a report it owes.
func TestAskPromptKeepsTasksAndOwedReports(t *testing.T) {
	p := askPrompt("Sweep: finished", "hub", "")
	for _, want := range []string{
		"Not scratch, so leave it and list it: a background task or wakeup you are waiting on for this work (a CI watch, a test run)",
		"the report your task asks for if you still owe it (you will be told to file it)",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("ask lacks %q", want)
		}
	}
}

// The automatic removal reads the session's tasks itself, not the agent
// list's cache (empty until its first read lands).
func TestAutoFactsReadTasksLive(t *testing.T) {
	s, _, _ := coordServer(t)
	bgLatest(s, func(string) []transcript.BackgroundTask {
		return []transcript.BackgroundTask{{Tool: "Monitor", Desc: "CI"}}
	})
	it := &removal{ID: "desk/w2:p1", Host: "desk", PaneID: "w2:p1", Live: liveInfo{Present: true}}
	if f := s.autoFactsOf(context.Background(), it); f.Background != 1 {
		t.Fatalf("background = %d", f.Background)
	}
}
