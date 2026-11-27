package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

type loopRig struct {
	t      *testing.T
	s      *Server
	mu     sync.Mutex
	nudges []string // text of each nudge
	told   []string // text of each message to the requester
	id     string
	typing bool
}

func newLoopRig(t *testing.T, loop string) *loopRig {
	t.Helper()
	s, _, _ := todoServer(t)
	s.sent = openSent("")
	s.initLoopNudge(10*time.Minute, 20*time.Minute)
	r := &loopRig{t: t, s: s}
	s.loops.send = func(_ context.Context, a herdr.AgentInfo, text string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.typing { // the shared input guard holds it (typing.go)
			return errInputBusy
		}
		r.nudges = append(r.nudges, text)
		return nil
	}
	s.loops.notify = func(_ context.Context, e ledger.Entry, text string) (string, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.told = append(r.told, text)
		return "desk/w1:p1", nil
	}
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "pr-watch", AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "pr-watch", Status: ledger.Running, Loop: loop})
	if err != nil {
		t.Fatal(err)
	}
	r.id = e.ID
	return r
}

// at applies a snapshot m minutes after the start, the worker in status st.
func (r *loopRig) at(m float64, st herdr.AgentStatus, bg ...herdr.BackgroundTask) {
	r.t.Helper()
	now := autoT0.Add(time.Duration(m * float64(time.Minute)))
	a := rAgent("w9:p1", "pr-watch", st, now)
	a.Background = bg
	r.s.loopStep(context.Background(), []herdr.AgentInfo{a}, now)
}

func (r *loopRig) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.nudges), len(r.told)
}

func (r *loopRig) want(nudges, told int, why string) {
	r.t.Helper()
	if n, d := r.counts(); n != nudges || d != told {
		r.t.Fatalf("%s: nudges=%d told=%d, want %d and %d", why, n, d, nudges, told)
	}
}

func TestLoopNudgeClockAndCoordinatorMessage(t *testing.T) {
	r := newLoopRig(t, "watch PR 118 until merged or CI red")
	r.at(0, herdr.StatusIdle)
	r.at(9, herdr.StatusIdle)
	r.want(0, 0, "before 10 minutes")
	r.at(10, herdr.StatusIdle)
	r.want(1, 0, "at 10 minutes")
	if !strings.Contains(r.nudges[0], "watch PR 118 until merged or CI red") || !strings.HasPrefix(r.nudges[0], "Comebye (hub nudge)") {
		t.Fatalf("first nudge = %q", r.nudges[0])
	}
	r.at(11, herdr.StatusIdle)
	r.at(29, herdr.StatusIdle)
	r.want(1, 0, "before the second nudge is due")
	r.at(30, herdr.StatusIdle) // 20 minutes after the first
	r.want(2, 0, "second nudge")
	if !strings.Contains(r.nudges[1], "second") {
		t.Fatalf("second nudge = %q", r.nudges[1])
	}
	r.at(39, herdr.StatusIdle)
	r.want(2, 0, "grace after the second nudge")
	r.at(40, herdr.StatusIdle)
	r.want(2, 1, "the requester is told")
	if !strings.Contains(r.told[0], "is dead") || !strings.Contains(r.told[0], r.id) {
		t.Fatalf("message = %q", r.told[0])
	}
	// Once only, however long it stays idle.
	r.at(60, herdr.StatusIdle)
	r.at(600, herdr.StatusIdle)
	r.want(2, 1, "nothing more")
}

func TestLoopNudgeResetsWhenTheAgentWorks(t *testing.T) {
	r := newLoopRig(t, "watch PR 118")
	r.at(0, herdr.StatusIdle)
	r.at(8, herdr.StatusWorking) // before any nudge: reset
	r.at(12, herdr.StatusIdle)   // a new spell from here
	r.at(21, herdr.StatusIdle)
	r.want(0, 0, "the clock restarted at 12")
	r.at(22, herdr.StatusIdle)
	r.want(1, 0, "10 minutes after the new spell began")
	// Working for real (longer than the nudge's own answer) resets, too.
	r.at(23, herdr.StatusWorking)
	r.at(26, herdr.StatusWorking) // 3 minutes: past the allowance
	r.at(27, herdr.StatusIdle)
	r.at(36, herdr.StatusIdle)
	r.want(1, 0, "the spell began at 27: not due before 37")
	r.at(37, herdr.StatusIdle)
	r.want(2, 0, "a first nudge of the new spell, not a second one")
	if strings.Contains(r.nudges[1], "second") {
		t.Fatalf("nudge = %q", r.nudges[1])
	}
}

func TestLoopNudgeAnswerToANudgeIsNotWork(t *testing.T) {
	r := newLoopRig(t, "watch PR 118")
	r.at(0, herdr.StatusIdle)
	r.at(10, herdr.StatusIdle)
	r.want(1, 0, "first nudge")
	// It answers the nudge in a moment ("ok") and is idle again: the count goes on.
	r.at(10.5, herdr.StatusWorking)
	r.at(11, herdr.StatusIdle)
	r.at(30, herdr.StatusIdle)
	r.want(2, 0, "second nudge still comes 20 minutes after the first")
	r.at(40, herdr.StatusIdle)
	r.want(2, 1, "and the message")
}

func TestLoopNudgeBackgroundTaskResets(t *testing.T) {
	r := newLoopRig(t, "watch PR 118")
	r.at(0, herdr.StatusIdle)
	r.at(9, herdr.StatusIdle, herdr.BackgroundTask{Kind: "bash", Label: "gh pr checks --watch"})
	r.at(15, herdr.StatusIdle, herdr.BackgroundTask{Kind: "bash"})
	r.want(0, 0, "a background task is the loop running")
	r.at(16, herdr.StatusIdle)
	r.at(25, herdr.StatusIdle)
	r.want(0, 0, "the clock restarted when the task ended")
	r.at(26, herdr.StatusIdle)
	r.want(1, 0, "10 minutes after")
}

func TestLoopNudgeStopsWhenTheHandoffIsOver(t *testing.T) {
	for name, stop := range map[string]func(*loopRig){
		"finished with a report": func(r *loopRig) {
			r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.Status, e.ReportSource = ledger.Finished, "explicit" })
		},
		"removed": func(r *loopRig) {
			r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.Status = ledger.Removed })
		},
		"landed": func(r *loopRig) {
			r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.Status = ledger.Landed })
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t, "watch PR 118")
			r.at(0, herdr.StatusIdle)
			r.at(10, herdr.StatusIdle)
			r.want(1, 0, "first nudge")
			stop(r)
			r.at(30, herdr.StatusIdle)
			r.at(40, herdr.StatusIdle)
			r.at(100, herdr.StatusIdle)
			r.want(1, 0, "no more once it is "+name)
		})
	}
}

// A worker whose turn ended with no report reads as finished to the ledger:
// that is the dead loop, so the clock runs on.
func TestLoopNudgeRunsOnForAnImplicitFinish(t *testing.T) {
	r := newLoopRig(t, "watch PR 118")
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.Status = ledger.Finished })
	r.at(0, herdr.StatusIdle)
	r.at(10, herdr.StatusIdle)
	r.want(1, 0, "nudged though the ledger says finished")
}

func TestLoopNudgeNotWithoutLoop(t *testing.T) {
	r := newLoopRig(t, "")
	for _, m := range []float64{0, 10, 30, 40, 100} {
		r.at(m, herdr.StatusIdle)
	}
	r.want(0, 0, "a worker with no --loop")
}

func TestLoopNudgePausesWhileTypingBlockedOrParked(t *testing.T) {
	r := newLoopRig(t, "watch PR 118")
	r.at(0, herdr.StatusIdle)
	r.typing = true
	r.at(10, herdr.StatusIdle)
	r.want(0, 0, "someone is typing in its box")
	r.typing = false
	r.at(10.1, herdr.StatusBlocked)
	r.want(0, 0, "blocked at a dialog")
	r.at(11, herdr.StatusIdle)
	r.want(1, 0, "typing over, nudged at once: the spell is old enough")
	// Parked: gone from the fleet; the clock starts again when it is back.
	r.s.loopStep(context.Background(), nil, autoT0.Add(20*time.Minute))
	r.at(30, herdr.StatusIdle)
	r.at(39, herdr.StatusIdle)
	r.want(1, 0, "a returning worker starts a new spell")
	r.at(40, herdr.StatusIdle)
	r.want(2, 0, "nudged again after its new 10 minutes")
}

func TestLoopNudgeFailedSendIsRetriedAndClaimedOnce(t *testing.T) {
	r := newLoopRig(t, "watch PR 118")
	fail := true
	r.s.loops.send = func(_ context.Context, a herdr.AgentInfo, text string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if fail {
			return context.DeadlineExceeded
		}
		r.nudges = append(r.nudges, text)
		return nil
	}
	r.at(0, herdr.StatusIdle)
	r.at(10, herdr.StatusIdle)
	r.want(0, 0, "send failed")
	r.at(10.2, herdr.StatusIdle)
	r.want(0, 0, "not retried inside the retry gap")
	fail = false
	r.at(11, herdr.StatusIdle)
	r.want(1, 0, "retried and sent")
	// A restarted hub (a fresh clock in the same spell) does not send it again.
	r.s.loops.m = map[string]*loopSt{}
	r.at(12, herdr.StatusIdle) // new spell start: 12 -> a new subject
	r.want(1, 0, "new spell, clock restarted")
}

func TestCleanLoop(t *testing.T) {
	got := cleanLoop("watch PR 118\nuntil `merged` or CI red\x1b[31m")
	if strings.ContainsAny(got, "`\n\x1b") || !strings.HasPrefix(got, "watch PR 118 until") {
		t.Fatalf("cleanLoop = %q", got)
	}
	if n := len(cleanLoop(strings.Repeat("a", 500))); n != loopSpecMax {
		t.Fatalf("length = %d", n)
	}
}

// A standing loop (loop set --every 1h) sits idle between passes: the hub
// leaves it alone until its pass is overdue by the margin (a quarter of the
// interval, at least the 10-minute default), nudges it once, and a margin
// later tells the requester. The clock counts from the ledger's last pass
// end, so a hub restart does not push the nudge out.
func TestLoopNudgeStandingLoopOnSchedule(t *testing.T) {
	r := newLoopRig(t, "run a steward pass")
	pass := autoT0
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LoopEveryMinutes, e.LastPassAt = 60, &pass })
	r.at(1, herdr.StatusIdle) // first seen a minute after the pass ended
	r.at(30, herdr.StatusIdle)
	r.at(74, herdr.StatusIdle)
	r.want(0, 0, "between passes, not overdue")
	r.at(75, herdr.StatusIdle)
	r.want(1, 0, "an hour plus 15 minutes after the pass ended")
	if !strings.Contains(r.nudges[0], "overdue") || !strings.Contains(r.nudges[0], "every 1h") {
		t.Fatalf("nudge = %q", r.nudges[0])
	}
	r.at(89, herdr.StatusIdle)
	r.want(1, 0, "one nudge only, then the margin")
	r.at(90, herdr.StatusIdle)
	r.want(1, 1, "the requester is told")
	if !strings.Contains(r.told[0], "missed its pass") || !strings.Contains(r.told[0], "loop clear "+r.id) {
		t.Fatalf("message = %q", r.told[0])
	}
	r.at(300, herdr.StatusIdle)
	r.want(1, 1, "nothing more")
}

func TestLoopNudgeStandingLoopPassResets(t *testing.T) {
	r := newLoopRig(t, "run a steward pass")
	pass := autoT0
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LoopEveryMinutes, e.LastPassAt = 60, &pass })
	r.at(10, herdr.StatusIdle)
	r.at(60, herdr.StatusWorking) // its next pass, on time
	next := autoT0.Add(65 * time.Minute)
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LastPassAt = &next })
	r.at(65, herdr.StatusIdle)
	r.at(139, herdr.StatusIdle)
	r.want(0, 0, "the clock counts from the new pass end")
	r.at(140, herdr.StatusIdle)
	r.want(1, 0, "overdue from the new pass end")
}

func TestLoopSetRoute(t *testing.T) {
	r := newLoopRig(t, "")
	pass := autoT0
	r.s.ledger.Update(r.id, func(e *ledger.Entry) {
		e.Loop, e.Status, e.FinishedAt = "", ledger.Finished, &pass
		e.Callback = &ledger.Callback{State: ledger.CallbackPending, At: pass}
	})
	hdr := agentHdr("w1:p1")
	if w := do(r.s, "POST", "/api/handoffs/pr-watch/loop", nil, `{"every":"1h"}`); w.Code == 200 {
		t.Fatal("unauthenticated loop set allowed")
	}
	if w := do(r.s, "POST", "/api/handoffs/pr-watch/loop", hdr, `{"every":"10s"}`); w.Code != 400 {
		t.Fatalf("bad every: %d %s", w.Code, w.Body)
	}
	if w := do(r.s, "POST", "/api/handoffs/nobody/loop", hdr, `{"every":"1h"}`); w.Code != 404 {
		t.Fatalf("unknown name: %d %s", w.Code, w.Body)
	}
	w := do(r.s, "POST", "/api/handoffs/pr-watch/loop", hdr, `{"every":"1h","loop":"run a steward pass"}`)
	if w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	e, _ := r.s.ledger.Get(r.id)
	if e.Loop != "run a steward pass" || e.LoopEveryMinutes != 60 || e.Status != ledger.Running || e.Callback != nil || !e.BetweenPasses() {
		t.Fatalf("after set: %+v", e)
	}
	if w := do(r.s, "POST", "/api/handoffs/"+r.id+"/loop", hdr, `{"clear":true}`); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if e, _ := r.s.ledger.Get(r.id); e.Loop != "" || e.LoopEveryMinutes != 0 {
		t.Fatalf("after clear: %+v", e)
	}
}
