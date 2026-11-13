package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/park"
)

// parkedGhost parks a record for an agent "ghost" that had pane w5:p1.
func parkedGhost(t *testing.T, r *pqRig) park.Record {
	t.Helper()
	rec := park.Record{ID: "pk_ghost00001", Name: "ghost", Kind: "claude", Host: "desk", PaneID: "w5:p1", ParkedAt: r.now.Add(-time.Hour)}
	if err := r.s.parkStore().Add(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func resumeGhost(t *testing.T, r *pqRig, pane string) {
	t.Helper()
	now := r.now
	if _, err := r.s.parkStore().Update("pk_ghost00001", func(x *park.Record) { x.ResumedAt, x.ResumedPane = &now, pane }); err != nil {
		t.Fatal(err)
	}
}

func queueGhost(t *testing.T, r *pqRig, agent string) (int, map[string]any) {
	t.Helper()
	w := do(r.s, "POST", "/api/queue", agentHdr("w1:p1"), `{"agent":"`+agent+`","name":"ghost","text":"when you are back"}`)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestQueueToParkedAgentWaitsAndFollowsTheResume(t *testing.T) {
	r, _ := newPQRig(t)
	parkedGhost(t, r)
	// Queued by the park id: accepted, and the sender is told it waits for a resume.
	code, out := queueGhost(t, r, "pk_ghost00001")
	if code != 202 || out["parked"] != true || !strings.Contains(out["note"].(string), "resume") {
		t.Fatalf("queue = %d %v", code, out)
	}
	live := []herdr.AgentInfo{cbAgent("w1:p1", "coordinator", herdr.StatusIdle)}
	// Past the normal 2 h it is still pending, not expired or dropped.
	r.advance(queueTTL + time.Hour)
	r.s.deliverQueue(live)
	l := r.s.pq.list("", true)
	if len(l) != 1 || l[0].State != QueuePending || !l[0].Parked || l[0].Agent != "desk/w5:p1" {
		t.Fatalf("after 3 h: %+v", l)
	}
	// Its old pane is taken by another agent: still not "gone", nothing is sent to it.
	r.s.deliverQueue(append(live, cbAgent("w5:p1", "stranger", herdr.StatusIdle)))
	time.Sleep(50 * time.Millisecond)
	if l := r.s.pq.list("", true); l[0].State != QueuePending || r.sentN() != 0 {
		t.Fatalf("a reused pane took it: %+v sent %v", l, r.sent)
	}
	// The agent resumes in a new pane: the prompt follows it and is delivered there.
	resumeGhost(t, r, "w8:p1")
	agents := append(live, cbAgent("w8:p1", "ghost", herdr.StatusIdle))
	r.s.rebindParked(agents)
	r.s.deliverQueue(agents)
	waitFor(t, func() bool { return r.sentAndSettled(1) })
	if !strings.HasPrefix(r.sent[0], "desk/w8:p1: [Queued by coordinator") || !strings.HasSuffix(r.sent[0], "when you are back") {
		t.Fatalf("sent = %q", r.sent)
	}
}

func TestQueueToParkedAgentExpiresAfterSevenDays(t *testing.T) {
	r, _ := newPQRig(t)
	parkedGhost(t, r)
	queueGhost(t, r, "desk/w5:p1")
	live := []herdr.AgentInfo{cbAgent("w1:p1", "coordinator", herdr.StatusIdle)}
	r.advance(parkQueueTTL + time.Hour)
	r.s.deliverQueue(live)
	waitFor(t, func() bool { return r.sentAndSettled(1) })
	if !strings.Contains(r.sent[0], "expired after 7 days") {
		t.Fatalf("notice = %q", r.sent[0])
	}
	if l := r.s.pq.list("", true); len(l) != 1 || l[0].State != QueueExpired {
		t.Fatalf("list = %+v", l)
	}
}

func TestQueueToUnknownAgentStillRefused(t *testing.T) {
	r, _ := newPQRig(t)
	if code, out := queueGhost(t, r, "desk/w5:p1"); code != 404 || out["code"] != "agent_not_found" {
		t.Fatalf("queue = %d %v", code, out)
	}
	// A record that was already resumed is not a parked target.
	parkedGhost(t, r)
	resumeGhost(t, r, "w8:p1")
	if code, _ := queueGhost(t, r, "pk_ghost00001"); code != 404 {
		t.Fatalf("resumed record queued: %d", code)
	}
}

func TestTodoWaitingOnParkedAgentIsParkedThenRebound(t *testing.T) {
	s, sent, mu := todoServer(t)
	rec := park.Record{ID: "pk_ghost00001", Name: "ghost", Kind: "claude", Host: "desk", PaneID: "w5:p1", ParkedAt: time.Now().Add(-time.Hour)}
	if err := s.parkStore().Add(rec); err != nil {
		t.Fatal(err)
	}
	it := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"Cover egress too?","waiting":"desk/w5:p1"}`).Body.Bytes())
	got := s.todoGone(it)
	if got.Waiting == nil || !got.Waiting.Parked || got.Waiting.ParkID != "pk_ghost00001" || got.Waiting.Gone || got.Waiting.Name != "ghost" {
		t.Fatalf("waiting = %+v", got.Waiting)
	}
	// The user's answer reaches the coordinator with a hint to queue it for the parked agent.
	do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"yes"}`)
	msgs := waitSent(t, sent, mu, 1)
	if !strings.Contains(msgs[0], "is waiting on ghost, which is parked") || !strings.Contains(msgs[0], "comebye prompt ghost --queue") {
		t.Fatalf("notice = %q", msgs[0])
	}
	// After the resume the item points at the new pane and is no longer parked.
	now := time.Now()
	s.parkStore().Update("pk_ghost00001", func(x *park.Record) { x.ResumedAt, x.ResumedPane = &now, "w8:p1" })
	s.rebindParked([]herdr.AgentInfo{cbAgent("w8:p1", "ghost", herdr.StatusIdle)})
	cur, _ := s.todos.get(it.ID)
	if cur.Waiting.ID != "desk/w8:p1" || s.todoGone(cur).Waiting.Parked {
		t.Fatalf("after resume: %+v", cur.Waiting)
	}
}
