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
	t       *testing.T
	s       *Server
	mu      sync.Mutex
	nudges  []string // text of each nudge
	told    []string // text of each message to the requester
	id      string
	typing  bool
	harness string // the agent's harness; "" is claude
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
	if r.harness != "" {
		a.Agent = r.harness
	}
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

// A standing loop (loop set --every 1h) is scheduled by the hub: an hour
// after its last pass ended, the hub sends the loop's text as a prompt, once.
// A pass the worker did not take goes again an interval after the last one,
// and nothing goes in between.
func TestLoopPassDueIsSentOnce(t *testing.T) {
	r := newLoopRig(t, "run a steward pass")
	pass := autoT0
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LoopEveryMinutes, e.LastPassAt = 60, &pass })
	r.at(1, herdr.StatusIdle)
	r.at(30, herdr.StatusIdle)
	r.at(59.9, herdr.StatusIdle)
	r.want(0, 0, "between passes")
	r.at(60, herdr.StatusIdle)
	r.want(1, 0, "an hour after the pass ended")
	if p := r.nudges[0]; !strings.HasPrefix(p, loopPassLead) || !strings.Contains(p, "run a steward pass") || !strings.Contains(p, "every 1h") || !strings.Contains(p, "do not schedule") {
		t.Fatalf("pass = %q", p)
	}
	r.at(60.2, herdr.StatusIdle)
	r.at(61, herdr.StatusIdle)
	r.at(119, herdr.StatusIdle)
	r.want(1, 0, "one pass per interval, and no missed-pass notice")
	r.at(120, herdr.StatusIdle)
	r.want(2, 0, "not taken: sent again an interval later")
	// A restarted hub (a fresh clock) does not send the same pass again.
	r.s.loops.m = map[string]*loopSt{}
	r.at(120.5, herdr.StatusIdle)
	r.want(2, 0, "the restarted hub counts the pass it sent")
}

// A pass that is running, or one not yet delivered, is never stacked: the
// next one counts from the end of the pass that ran.
func TestLoopPassNotStackedOnABusyAgent(t *testing.T) {
	r := newLoopRig(t, "run a steward pass")
	pass := autoT0
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LoopEveryMinutes, e.LastPassAt = 60, &pass })
	r.at(10, herdr.StatusIdle)
	r.at(60, herdr.StatusWorking) // already in a pass (it was prompted by hand)
	r.at(65, herdr.StatusWorking)
	r.at(66, herdr.StatusBlocked)
	r.at(67, herdr.StatusIdle, herdr.BackgroundTask{Kind: "bash", Label: "gh pr checks --watch"})
	r.want(0, 0, "working, blocked or with a task running: no pass")
	end := autoT0.Add(68 * time.Minute)
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LastPassAt = &end })
	r.at(68, herdr.StatusIdle)
	r.at(127.9, herdr.StatusIdle)
	r.want(0, 0, "the clock counts from the new pass end")
	// A prompt that waits in the queue for it: the pass waits too.
	if _, _, err := r.s.pq.add(QueuedPrompt{Agent: "desk/w9:p1", Name: "pr-watch", Text: "look at PR 7", From: "comebye"}); err != nil {
		t.Fatal(err)
	}
	r.at(128, herdr.StatusIdle)
	r.want(0, 0, "a queued prompt is not delivered yet")
	for _, p := range r.s.pq.list("desk/w9:p1", false) {
		r.s.pq.settle(p.ID, QueueDelivered, false)
	}
	r.at(128.5, herdr.StatusIdle)
	r.want(1, 0, "the queue is clear: the pass goes")
}

func TestLoopPassHeldWhileTyping(t *testing.T) {
	r := newLoopRig(t, "run a steward pass")
	pass := autoT0
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LoopEveryMinutes, e.LastPassAt = 60, &pass })
	r.at(1, herdr.StatusIdle)
	r.typing = true
	r.at(60, herdr.StatusIdle)
	r.at(60.5, herdr.StatusIdle)
	r.want(0, 0, "someone is typing in its box")
	r.s.typing.mu.Lock()
	h := r.s.typing.held["desk/w9:p1"]
	noted := h != nil && h.senders[senderLoop]
	r.s.typing.mu.Unlock()
	if !noted {
		t.Fatal("the hold was not noted for the shared retry")
	}
	r.typing = false
	r.at(60.6, herdr.StatusIdle) // the retry runs the step at once: no failed-try gap
	r.want(1, 0, "sent once the box clears")
	r.at(61, herdr.StatusIdle)
	r.want(1, 0, "once")
}

// pi has no ScheduleWakeup: the hub's pass is how it loops.
func TestLoopPassReachesAPiWorker(t *testing.T) {
	r := newLoopRig(t, "run a steward pass")
	r.harness = "pi"
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LoopEveryMinutes = 1 })
	r.at(0, herdr.StatusIdle) // no pass yet: the first counts from now
	r.at(0.9, herdr.StatusIdle)
	r.want(0, 0, "not due")
	r.at(1, herdr.StatusIdle)
	r.want(1, 0, "the first pass")
	r.at(1.2, herdr.StatusWorking) // pi works inside its turn
	r.at(1.5, herdr.StatusWorking)
	end := autoT0.Add(2 * time.Minute)
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LastPassAt = &end })
	r.at(2, herdr.StatusIdle)
	r.at(2.9, herdr.StatusIdle)
	r.want(1, 0, "between passes")
	r.at(3, herdr.StatusIdle)
	r.want(2, 0, "the second pass, a minute after the first ended")
}

func TestLoopPassStopsOnLoopClear(t *testing.T) {
	r := newLoopRig(t, "run a steward pass")
	pass := autoT0
	r.s.ledger.Update(r.id, func(e *ledger.Entry) { e.LoopEveryMinutes, e.LastPassAt = 60, &pass })
	r.at(60, herdr.StatusIdle)
	r.want(1, 0, "the first pass")
	if w := do(r.s, "POST", "/api/handoffs/"+r.id+"/loop", agentHdr("w1:p1"), `{"clear":true}`); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	for _, m := range []float64{120, 180, 600} {
		r.at(m, herdr.StatusIdle)
	}
	r.want(1, 0, "no passes after loop clear")
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
