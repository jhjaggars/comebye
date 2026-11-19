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

// Two coordinators: A (desk/w1:p1, "coordinator", registered first) and B
// (desk/w3:p1, "coord-b", registered later), both idle, plus a plain agent
// "home" (desk/w2:p1). The notice senders record who was told what.
type routeRig struct {
	tt   *testing.T
	s    *Server
	fh   *fakeLaunchHerdr
	mu   sync.Mutex
	sent []string // "to-id: text"
}

var (
	refA = &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}
	refB = &ledger.Ref{AgentID: "desk/w3:p1", Name: "coord-b"}
)

func newRouteRig(t *testing.T) *routeRig {
	t.Helper()
	s, fh, _ := coordServer(t)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "coord-b", PaneID: "w3:p1", Agent: "claude", Status: herdr.StatusIdle})
	})
	s.manager.Refresh()
	waitFor(t, func() bool { return len(s.fleet.Agents()) == 3 })
	if err := s.recordRole(refA.AgentID, refA.Name, RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := s.recordRole(refB.AgentID, refB.Name, RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	r := &routeRig{tt: t, s: s, fh: fh}
	rec := func(_ context.Context, a herdr.AgentInfo, text string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sent = append(r.sent, a.ID+": "+text)
		return nil
	}
	s.todos.send = rec
	s.pq.send = rec
	return r
}

func (r *routeRig) status(pane string, st herdr.AgentStatus) {
	r.fh.set(func(f *fakeLaunchHerdr) {
		for i := range f.agents {
			if f.agents[i].PaneID == pane {
				f.agents[i].Status = st
			}
		}
	})
	r.s.manager.Refresh()
	waitFor(r.t(), func() bool {
		for _, a := range r.s.fleet.Agents() {
			if a.PaneID == pane {
				return a.Status == st
			}
		}
		return false
	})
}

func (r *routeRig) t() *testing.T { return r.tt }

func (r *routeRig) remove(pane string) {
	r.fh.set(func(f *fakeLaunchHerdr) {
		var keep []herdr.AgentInfo
		for _, a := range f.agents {
			if a.PaneID != pane {
				keep = append(keep, a)
			}
		}
		f.agents = keep
	})
	r.s.manager.Refresh()
	waitFor(r.t(), func() bool {
		for _, a := range r.s.fleet.Agents() {
			if a.PaneID == pane {
				return false
			}
		}
		return true
	})
}

// to returns what the recorder sent to id, and what the fake herdr was
// prompted with for pane (the hub's own prompt path).
func (r *routeRig) to(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.sent {
		if strings.HasPrefix(m, id+": ") {
			out = append(out, m)
		}
	}
	return out
}

func (r *routeRig) prompts(pane string) []string {
	r.fh.mu.Lock()
	defer r.fh.mu.Unlock()
	var out []string
	for _, c := range r.fh.calls {
		if c.Method == "agent.prompt" && c.Params["target"] == pane {
			txt, _ := c.Params["text"].(string)
			out = append(out, txt)
		}
	}
	return out
}

func rigOf(t *testing.T) *routeRig { return newRouteRig(t) }

func (r *routeRig) idle() []herdr.AgentInfo { return r.s.fleet.Agents() }

// A hands off, B must not hear: finished notices (and the report-N and
// landing variants, which are the same job with other words) go to the
// requester, wait while it is busy, and never to the newer idle coordinator.
func TestRouteCallbackGoesToTheAskingCoordinator(t *testing.T) {
	r := rigOf(t)
	e := cbFinished(t, r.s, refA, r.idle()...)

	r.status("w1:p1", herdr.StatusWorking)
	r.s.deliverCallbacks(r.s.fleet.Agents())
	time.Sleep(60 * time.Millisecond)
	if n := len(r.prompts("w1:p1")) + len(r.prompts("w3:p1")); n != 0 {
		t.Fatalf("a busy requester was replaced: %d prompts", n)
	}
	r.status("w1:p1", herdr.StatusIdle)
	r.s.deliverCallbacks(r.s.fleet.Agents())
	waitFor(t, func() bool { g, _ := r.s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	if len(r.prompts("w1:p1")) != 1 || len(r.prompts("w3:p1")) != 0 {
		t.Fatalf("A=%v B=%v", r.prompts("w1:p1"), r.prompts("w3:p1"))
	}
}

func TestRoutePlanCallbackGoesToItsCreator(t *testing.T) {
	r := rigOf(t)
	p, err := r.s.ledger.AddPlan(ledger.Plan{Title: "ship", Size: 1, ReplyTo: refB})
	if err != nil {
		t.Fatal(err)
	}
	rt := r.s.routeCoordinator(p.ReplyTo)
	if rt.Agent == nil || rt.Agent.ID != refB.AgentID || rt.Gone != "" {
		t.Fatalf("route = %+v", rt)
	}
}

// The requester is a coordinator that closed: after the stand-in delay the
// notice goes to the main coordinator, and says whose it was.
func TestRouteCallbackGoneOwnerFallsBackToThePoolLabelled(t *testing.T) {
	r := rigOf(t)
	r.remove("w3:p1")
	cb := &ledger.Callback{At: time.Now().Add(-callbackFallback - time.Minute)}
	tgt, standIn, gone := callbackTarget(refB, cb, r.s.fleet.Agents(), time.Now(), r.s.routeCoordinator)
	if tgt == nil || tgt.ID != refA.AgentID || !standIn || gone != "coord-b" {
		t.Fatalf("target = %v standIn=%v gone=%q", tgt, standIn, gone)
	}
	// Before the delay nothing moves.
	cb.At = time.Now()
	if tgt, _, _ := callbackTarget(refB, cb, r.s.fleet.Agents(), time.Now(), r.s.routeCoordinator); tgt != nil {
		t.Fatalf("early stand-in = %v", tgt)
	}
	// A worker that was handed off by B and is gone: B's notice, not A's.
	r.fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "coord-b", PaneID: "w3:p1", Agent: "claude", Status: herdr.StatusIdle})
	})
	r.s.manager.Refresh()
	waitFor(t, func() bool { return len(r.s.fleet.Agents()) == 3 })
	if _, err := r.s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "sub", AgentID: "desk/w8:p1", PaneID: "w8:p1", AgentName: "sub", Status: ledger.Running, ReplyTo: refB}); err != nil {
		t.Fatal(err)
	}
	cb.At = time.Now().Add(-callbackFallback - time.Minute)
	sub := &ledger.Ref{AgentID: "desk/w8:p1", Name: "sub"}
	if tgt, _, _ := callbackTarget(sub, cb, r.s.fleet.Agents(), time.Now(), r.s.routeCoordinator); tgt == nil || tgt.ID != refB.AgentID {
		t.Fatalf("sub-worker stand-in = %v", tgt)
	}
}

// A to-do answer goes to the coordinator the item waits on, not to the
// newer idle one; while that one is busy it waits.
func TestRouteTodoAnswerGoesToTheWaitingCoordinator(t *testing.T) {
	r := rigOf(t)
	it := todoOf(t, do(r.s, "POST", "/api/todos", agentHdr("w1:p1"), `{"question":"Which key?","options":[{"label":"a"},{"label":"b"}]}`).Body.Bytes())
	r.s.todos.mutate(it.ID, func(i *Todo) error { i.Waiting = &TodoAgent{ID: refA.AgentID, Name: refA.Name}; return nil })
	it, _ = r.s.todos.get(it.ID)
	if it.Waiting == nil || it.Waiting.ID != refA.AgentID {
		t.Fatalf("waiting = %+v", it.Waiting)
	}
	r.status("w1:p1", herdr.StatusWorking)
	if w := do(r.s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok 1"}`); w.Code != 200 {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	time.Sleep(80 * time.Millisecond)
	if n := len(r.to(refA.AgentID)) + len(r.to(refB.AgentID)); n != 0 {
		t.Fatalf("a busy waiting coordinator was replaced: %v", r.sent)
	}
	r.status("w1:p1", herdr.StatusIdle)
	r.s.todos.mu.Lock()
	r.s.todos.last = time.Time{}
	r.s.todos.mu.Unlock()
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	waitFor(t, func() bool { return len(r.to(refA.AgentID)) == 1 })
	if len(r.to(refB.AgentID)) != 0 || !strings.Contains(r.to(refA.AgentID)[0], it.ID) {
		t.Fatalf("sent = %v", r.sent)
	}
}

// Items filed by agents that two different coordinators handed work to (a
// decision from a report, a mark's ready-to-remove ask: both wait on the
// worker) go to the coordinator that asked, each in its own note.
func TestRouteTodoAboutAWorkerGoesToItsRequester(t *testing.T) {
	r := rigOf(t)
	for _, w := range []struct {
		pane, name string
		to         *ledger.Ref
	}{{"w2:p1", "home", refB}, {"w4:p1", "other", refA}} {
		if _, err := r.s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: w.name, AgentID: "desk/" + w.pane, PaneID: w.pane, AgentName: w.name, Status: ledger.Running, ReplyTo: w.to}); err != nil {
			t.Fatal(err)
		}
	}
	r.fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "other", PaneID: "w4:p1", Agent: "pi", Status: herdr.StatusIdle})
	})
	r.s.manager.Refresh()
	waitFor(t, func() bool { return len(r.s.fleet.Agents()) == 4 })
	ids := map[string]string{}
	for pane, q := range map[string]string{"w2:p1": "Land it now?", "w4:p1": "Ship it?"} {
		it := todoOf(t, do(r.s, "POST", "/api/todos", agentHdr(pane), `{"question":"`+q+`"}`).Body.Bytes())
		ids[pane] = it.ID
		do(r.s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"go ahead"}`) // a gate: the coordinator relays it
	}
	waitFor(t, func() bool {
		r.s.todos.mu.Lock()
		r.s.todos.last = time.Time{}
		r.s.todos.mu.Unlock()
		r.s.deliverTodoNotices(r.s.fleet.Agents())
		return len(r.to(refA.AgentID)) == 1 && len(r.to(refB.AgentID)) == 1
	})
	if !strings.Contains(r.to(refB.AgentID)[0], ids["w2:p1"]) || strings.Contains(r.to(refB.AgentID)[0], ids["w4:p1"]) ||
		!strings.Contains(r.to(refA.AgentID)[0], ids["w4:p1"]) || strings.Contains(r.to(refA.AgentID)[0], ids["w2:p1"]) {
		t.Fatalf("sent = %v", r.sent)
	}
}

// A coordinator's prompt to an agent that is not its handoff makes it the
// agent's asker: that agent's notices go there, until another coordinator
// prompts it. A prompt to someone else's handoff changes nothing.
func TestRoutePromptRecordsTheAsker(t *testing.T) {
	r := rigOf(t)
	id := "desk/w2:p1"
	prompt := func(from, target string) {
		t.Helper()
		if w := do(r.s, "POST", "/api/agents/"+target+"/prompt", agentHdr(from), `{"text":"look at this"}`); w.Code != 200 {
			t.Fatalf("prompt = %d %s", w.Code, w.Body)
		}
	}
	asker := func() string { a, _ := r.s.askers.get(id); return a.Coord.AgentID }
	if asker() != "" {
		t.Fatal("asker before any prompt")
	}
	prompt("w3:p1", "w2:p1")
	if asker() != refB.AgentID {
		t.Fatalf("asker = %q", asker())
	}
	// Its to-do (a gate) now goes to B, not to the older A.
	it := todoOf(t, do(r.s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"Land it now?"}`).Body.Bytes())
	do(r.s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"go ahead"}`)
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	waitFor(t, func() bool { return len(r.to(refB.AgentID)) == 1 })
	if len(r.to(refA.AgentID)) != 0 {
		t.Fatalf("A was told: %v", r.sent)
	}
	prompt("w1:p1", "w2:p1") // another coordinator prompts: it asks now
	if asker() != refA.AgentID {
		t.Fatalf("asker after A = %q", asker())
	}
	// An agent's own prompt (not a coordinator) records nothing; a handoff
	// worker keeps its requester.
	if _, err := r.s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "w", AgentID: "desk/w4:p1", PaneID: "w4:p1", AgentName: "other", Status: ledger.Running, ReplyTo: refB}); err != nil {
		t.Fatal(err)
	}
	r.fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "other", PaneID: "w4:p1", Agent: "pi", Status: herdr.StatusIdle})
	})
	r.s.manager.Refresh()
	waitFor(t, func() bool { return len(r.s.fleet.Agents()) == 4 })
	prompt("w1:p1", "w4:p1")
	if _, ok := r.s.askers.get("desk/w4:p1"); ok {
		t.Fatal("recorded an asker for another coordinator's handoff")
	}
	if got := r.s.ownerOfAgent("desk/w4:p1", "other"); got == nil || got.AgentID != refB.AgentID {
		t.Fatalf("owner = %v", got)
	}
}

// The asker survives a restart (askers.json).
func TestRouteAskersPersist(t *testing.T) {
	path := askersPath(t.TempDir())
	st, err := openAskers(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.set("desk/w2:p1", Asker{Name: "home", Coord: *refB, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st2, err := openAskers(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := st2.get("desk/w2:p1"); !ok || a.Coord != *refB {
		t.Fatalf("reloaded = %+v %v", a, ok)
	}
}

// Notices nobody owns go to the main coordinator: the one marked, else the
// first registered live one. Busy main waits; a main that is not running is
// replaced by an idle coordinator, labelled.
func TestRouteMainCoordinator(t *testing.T) {
	r := rigOf(t)
	pool := func() coordRoute { return r.s.routeCoordinator(nil) }
	// Unmarked: A registered first, B is newer and idle: A.
	if rt := pool(); rt.Agent == nil || rt.Agent.ID != refA.AgentID || rt.Gone != "" {
		t.Fatalf("unmarked = %+v", rt)
	}
	if !r.s.liveRoles()[refA.AgentID].Main || r.s.liveRoles()[refB.AgentID].Main {
		t.Fatalf("roles = %+v", r.s.liveRoles())
	}
	// The user marks B main (POST /api/roles); persisted, one main only.
	if w := do(r.s, "POST", "/api/roles", userHdr, `{"agent_id":"`+refB.AgentID+`","role":"coordinator","main":true}`); w.Code != 200 {
		t.Fatalf("mark main = %d %s", w.Code, w.Body)
	}
	if rt := pool(); rt.Agent == nil || rt.Agent.ID != refB.AgentID {
		t.Fatalf("marked = %+v", rt)
	}
	if r.s.liveRoles()[refA.AgentID].Main || !r.s.liveRoles()[refB.AgentID].Main {
		t.Fatalf("roles = %+v", r.s.liveRoles())
	}
	// Re-registering B keeps the mark.
	if err := r.s.recordRole(refB.AgentID, refB.Name, RoleCoordinator, "me"); err != nil || !r.s.roles.isMain(refB.AgentID) {
		t.Fatalf("mark lost: %v", err)
	}
	// Main busy: the notice waits, A (idle) is not given it.
	r.status("w3:p1", herdr.StatusWorking)
	if rt := pool(); rt.Agent != nil || rt.Why != missBusy {
		t.Fatalf("busy main = %+v", rt)
	}
	// Main gone: A takes it, labelled as main's.
	r.remove("w3:p1")
	rt := pool()
	if rt.Agent == nil || rt.Agent.ID != refA.AgentID || rt.Gone != "coord-b" {
		t.Fatalf("gone main = %+v", rt)
	}
	if n := goneMainNote(rt.Gone); !strings.Contains(n, "coord-b") || !strings.Contains(n, "main") {
		t.Fatalf("note = %q", n)
	}
}

// An owner that is gone: its to-do answer goes to the pool and names it.
func TestRouteTodoGoneOwnerFallsBackLabelled(t *testing.T) {
	r := rigOf(t)
	it := todoOf(t, do(r.s, "POST", "/api/todos", agentHdr("w3:p1"), `{"question":"Which key?"}`).Body.Bytes())
	r.s.todos.mutate(it.ID, func(i *Todo) error { i.Waiting = &TodoAgent{ID: refB.AgentID, Name: refB.Name}; return nil })
	r.remove("w3:p1")
	do(r.s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok 1"}`)
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	waitFor(t, func() bool { return len(r.to(refA.AgentID)) == 1 })
	if m := r.to(refA.AgentID)[0]; !strings.Contains(m, "coord-b") || !strings.Contains(m, "gone") {
		t.Fatalf("note = %q", m)
	}
}

// A queued prompt that expired is reported to the coordinator that sent it;
// when that one is gone, to the main coordinator, labelled.
func TestRouteQueueExpiryNoticeGoesToTheSender(t *testing.T) {
	r := rigOf(t)
	now := time.Now()
	it := QueuedPrompt{ID: "q1", Agent: "desk/w2:p1", Name: "home", Text: "x", From: "coord-b", FromID: refB.AgentID, State: QueueExpired, Notice: true, SettledAt: &now}
	r.s.sendNotice(r.s.fleet.Agents(), it, now)
	waitFor(t, func() bool { return len(r.to(refB.AgentID)) == 1 })
	if len(r.to(refA.AgentID)) != 0 {
		t.Fatalf("sent = %v", r.sent)
	}
	r.remove("w3:p1")
	it.ID = "q2"
	r.s.sendNotice(r.s.fleet.Agents(), it, now)
	waitFor(t, func() bool { return len(r.to(refA.AgentID)) == 1 })
	if !strings.Contains(r.to(refA.AgentID)[0], "coord-b") {
		t.Fatalf("note = %v", r.to(refA.AgentID))
	}
}

// A dead loop is told to the coordinator that handed the worker off, waits
// while that one is busy, and goes to main (labelled) only when it is gone.
func TestRouteLoopNoticeGoesToTheRequester(t *testing.T) {
	r := rigOf(t)
	e := ledger.Entry{ID: "h1", Host: "desk", RepoName: "proj", Branch: "b", ReplyTo: refB}
	ctx := context.Background()
	r.status("w3:p1", herdr.StatusWorking)
	if to, err := r.s.loopNotifyRequester(ctx, e, "dead"); err != nil || to != "" {
		t.Fatalf("busy: to=%q err=%v", to, err)
	}
	if len(r.prompts("w1:p1")) != 0 {
		t.Fatal("A got B's notice")
	}
	r.status("w3:p1", herdr.StatusIdle)
	if to, err := r.s.loopNotifyRequester(ctx, e, "dead"); err != nil || to != refB.AgentID {
		t.Fatalf("idle: to=%q err=%v", to, err)
	}
	r.remove("w3:p1")
	to, err := r.s.loopNotifyRequester(ctx, e, "dead")
	if err != nil || to != refA.AgentID {
		t.Fatalf("gone: to=%q err=%v", to, err)
	}
	if ps := r.prompts("w1:p1"); len(ps) != 1 || !strings.Contains(ps[0], "coord-b") {
		t.Fatalf("A prompts = %v", ps)
	}
}
