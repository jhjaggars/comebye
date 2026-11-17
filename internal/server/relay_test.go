package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
)

// ansRig is todoServer plus a recorder for what the hub relays itself.
type ansRig struct {
	s     *Server
	coord *[]string // what reached the coordinator
	cmu   *sync.Mutex
	mu    sync.Mutex
	got   []string // relays: "agent id: text"
	err   error    // what the relay send returns (nil: delivered)
}

func newAnsRig(t *testing.T) *ansRig {
	t.Helper()
	s, coord, cmu := todoServer(t)
	r := &ansRig{s: s, coord: coord, cmu: cmu}
	s.todos.relaySend = func(_ context.Context, a herdr.AgentInfo, text string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.err != nil {
			return r.err
		}
		r.got = append(r.got, a.ID+": "+text)
		return nil
	}
	return r
}

func (r *ansRig) relays() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func (r *ansRig) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *ansRig) told() []string {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	return append([]string(nil), *r.coord...)
}

// add files an item waiting on the worker w2:p1 (name "home").
func (r *ansRig) add(t *testing.T, body string) Todo {
	t.Helper()
	return todoOf(t, do(r.s, "POST", "/api/todos", agentHdr("w2:p1"), body).Body.Bytes())
}

func (r *ansRig) answer(t *testing.T, id, text string) {
	t.Helper()
	if w := do(r.s, "POST", "/api/todos/"+id+"/answer", userHdr, `{"text":"`+text+`"}`); w.Code != 200 {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
}

// until polls cond for two seconds.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *ansRig) state(id string) Todo {
	it, _ := r.s.todos.get(id)
	return it
}

func (r *ansRig) events(kind string) []Event {
	return r.s.events.query(time.Time{}, []string{kind}, 0)
}

// pause lets a wrong send show up.
func pause() { time.Sleep(120 * time.Millisecond) }

func TestRelayPassesTheAnswerClosesAndLogs(t *testing.T) {
	r := newAnsRig(t)
	it := r.add(t, `{"question":"Which key should the service use?","options":[{"label":"Mint a key","detail":"fresh, rotates"},{"label":"Reuse the old key"}]}`)
	r.answer(t, it.ID, "ok 2")
	until(t, "the relay", func() bool { return len(r.relays()) == 1 })
	got := r.relays()[0]
	for _, want := range []string{"desk/w2:p1: Comebye (hub relay)", it.ID, "Which key should the service use?", `"ok 2"`, "Reuse the old key", "the user's own answer, typed by the hub"} {
		if !strings.Contains(got, want) {
			t.Errorf("relay lacks %q: %s", want, got)
		}
	}
	until(t, "the item to close", func() bool { return r.state(it.ID).State == TodoDone })
	if g := r.state(it.ID); g.ClosedBy != "hub" || g.Notice != nil {
		t.Fatalf("closed = %+v", g)
	}
	ev := r.events(evRelay)
	if len(ev) != 1 || ev[0].Subject != it.ID || ev[0].Who != "home" || !strings.Contains(ev[0].Text, "ok 2") || !strings.Contains(ev[0].Text, "closed") {
		t.Fatalf("events = %+v", ev)
	}
	pause()
	if n := r.told(); len(n) != 0 {
		t.Fatalf("the coordinator was told: %v", n)
	}
	// A later pass sends nothing more.
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	pause()
	if n := len(r.relays()); n != 1 {
		t.Fatalf("relays = %d", n)
	}
}

func TestRelayFreeTextIsTheUsersOwnWords(t *testing.T) {
	r := newAnsRig(t)
	it := r.add(t, `{"question":"Branch?"}`)
	r.answer(t, it.ID, "yes, but do it on a branch")
	until(t, "the relay", func() bool { return len(r.relays()) == 1 })
	if g := r.relays()[0]; !strings.Contains(g, `Their words: "yes, but do it on a branch"`) {
		t.Fatalf("relay = %s", g)
	}
}

func TestRelayGatesGoToTheCoordinator(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, r *ansRig, it Todo)
		body   string
		answer string
	}{
		{name: "coordinator waiting", answer: "yes", body: `{"question":"q"}`,
			setup: func(t *testing.T, r *ansRig, it Todo) {
				r.s.todos.mutate(it.ID, func(i *Todo) error { i.Waiting = &TodoAgent{ID: "desk/w1:p1", Name: "coordinator"}; return nil })
			}},
		{name: "release", answer: "yes", body: `{"question":"q"}`,
			setup: func(t *testing.T, r *ansRig, it Todo) {
				r.s.todos.mutate(it.ID, func(i *Todo) error { i.Kind = "release"; return nil })
			}},
		{name: "approval", answer: "yes", body: `{"question":"q"}`,
			setup: func(t *testing.T, r *ansRig, it Todo) {
				r.s.todos.mutate(it.ID, func(i *Todo) error { i.Kind = "approval"; return nil })
			}},
		{name: "option text says land", answer: "ok 1", body: `{"question":"q","options":[{"label":"Land it now"},{"label":"Wait"}]}`},
		{name: "option detail says merge", answer: "ok 2", body: `{"question":"q","options":[{"label":"Wait"},{"label":"Go","detail":"then merge the PR"}]}`},
		{name: "free text says approve", answer: "I approve", body: `{"question":"q"}`},
		{name: "free text says start", answer: "yes, start on it", body: `{"question":"q"}`},
		{name: "decision set with a plan", answer: "", body: `{"question":"q"}`,
			setup: func(t *testing.T, r *ansRig, it Todo) {
				r.s.todos.mutate(it.ID, func(i *Todo) error { i.Set = &TodoSet{Plan: "pl_1"}; return nil })
			}},
		{name: "item about a handoff awaiting landing", answer: "yes", body: `{"question":"q"}`,
			setup: func(t *testing.T, r *ansRig, it Todo) {
				e, err := r.s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "p", Branch: "b", AgentID: "desk/w9:p1", Status: ledger.Finished})
				if err != nil {
					t.Fatal(err)
				}
				r.s.todos.mutate(it.ID, func(i *Todo) error { i.Links = []TodoLink{{Handoff: e.ID}}; return nil })
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newAnsRig(t)
			it := r.add(t, c.body)
			if c.setup != nil {
				c.setup(t, r, it)
			}
			if c.name == "decision set with a plan" {
				// answered as a whole: submit-style answer on the item
				r.s.todos.mutate(it.ID, func(i *Todo) error {
					i.State, i.Answer, i.Notice = TodoAnswered, &TodoAnswer{Text: "Decisions: all", By: "me", Via: "app", At: time.Now()}, &TodoNotice{State: todoNoticePending, At: time.Now()}
					return nil
				})
				r.s.deliverTodoNotices(r.s.fleet.Agents())
			} else {
				r.answer(t, it.ID, c.answer)
			}
			until(t, "the coordinator notice", func() bool { return len(r.told()) == 1 })
			if n := r.told()[0]; !strings.Contains(n, "the user answered to-do item(s) "+it.ID) {
				t.Fatalf("notice = %s", n)
			}
			pause()
			if n := r.relays(); len(n) != 0 {
				t.Fatalf("the hub relayed a gate: %v", n)
			}
			if g := r.state(it.ID); g.State != TodoAnswered {
				t.Fatalf("state = %s", g.State)
			}
			if ev := r.events(evRelay); len(ev) != 0 {
				t.Fatalf("events = %+v", ev)
			}
		})
	}
}

func TestRelayToAParkedAgentQueuesAndClosesOnDelivery(t *testing.T) {
	r := newAnsRig(t)
	rec := park.Record{ID: "pk_ghost00001", Name: "ghost", Kind: "claude", Host: "desk", PaneID: "w5:p1", ParkedAt: time.Now().Add(-time.Hour)}
	if err := r.s.parkStore().Add(rec); err != nil {
		t.Fatal(err)
	}
	it := todoOf(t, do(r.s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"Cover egress too?","waiting":"desk/w5:p1"}`).Body.Bytes())
	r.answer(t, it.ID, "yes")
	until(t, "the queued relay", func() bool { return r.state(it.ID).Relay != nil && r.state(it.ID).Relay.State == relayQueued })
	q := r.s.pq.list("desk/w5:p1", false)
	if len(q) != 1 || !strings.Contains(q[0].Text, "Comebye (hub relay)") || !strings.Contains(q[0].Text, `"yes"`) {
		t.Fatalf("queue = %+v", q)
	}
	if !strings.Contains(queueText(q[0]), "[Queued by the hub") {
		t.Fatalf("stamp = %q", queueText(q[0]))
	}
	if g := r.state(it.ID); g.State != TodoAnswered || len(r.relays()) != 0 || len(r.told()) != 0 {
		t.Fatalf("answered=%v relays=%v told=%v", g.State, r.relays(), r.told())
	}
	// The queue delivers it (after the resume): the next pass closes the item.
	if _, err := r.s.pq.settle(q[0].ID, QueueDelivered, false); err != nil {
		t.Fatal(err)
	}
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	until(t, "the item to close", func() bool { return r.state(it.ID).State == TodoDone })
	if ev := r.events(evRelay); len(ev) != 2 { // queued, then relayed
		t.Fatalf("events = %+v", ev)
	}
	pause()
	if n := r.told(); len(n) != 0 {
		t.Fatalf("told = %v", n)
	}
}

func TestRelayQueueExpiryTellsTheCoordinator(t *testing.T) {
	r := newAnsRig(t)
	r.setErr(errDialogOpen) // the agent sits at a dialog: queued
	it := r.add(t, `{"question":"Which?"}`)
	r.answer(t, it.ID, "the second")
	until(t, "the queued relay", func() bool { return r.state(it.ID).Relay != nil && r.state(it.ID).Relay.State == relayQueued })
	q := r.s.pq.list("desk/w2:p1", false)
	if len(q) != 1 {
		t.Fatalf("queue = %+v", q)
	}
	// It expires on the queue (2 hours without the agent taking it).
	if _, err := r.s.pq.settle(q[0].ID, QueueExpired, true); err != nil {
		t.Fatal(err)
	}
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	until(t, "the coordinator notice", func() bool { return len(r.told()) == 1 })
	n := r.told()[0]
	if !strings.Contains(n, it.ID) || !strings.Contains(n, "could not") || !strings.Contains(n, "expired after 2 hours") {
		t.Fatalf("notice = %s", n)
	}
	if g := r.state(it.ID); g.State != TodoAnswered {
		t.Fatalf("state = %s", g.State)
	}
	if ev := r.events(evRelayFailed); len(ev) != 1 || !strings.Contains(ev[0].Text, "expired") {
		t.Fatalf("events = %+v", ev)
	}
}

func TestRelayToAGoneAgentTellsTheCoordinator(t *testing.T) {
	r := newAnsRig(t)
	it := r.add(t, `{"question":"Which?"}`)
	r.s.todos.mutate(it.ID, func(i *Todo) error { i.Waiting.Instance = "an-older-incarnation"; return nil })
	r.answer(t, it.ID, "the second")
	until(t, "the coordinator notice", func() bool { return len(r.told()) == 1 })
	if n := r.told()[0]; !strings.Contains(n, "the waiting agent is gone") || !strings.Contains(n, "relay it by hand") {
		t.Fatalf("notice = %s", n)
	}
	if g := r.state(it.ID); g.State != TodoAnswered || len(r.relays()) != 0 {
		t.Fatalf("state = %s relays = %v", g.State, r.relays())
	}
	if ev := r.events(evRelayFailed); len(ev) != 1 {
		t.Fatalf("events = %+v", ev)
	}
}

func TestRelayFailedSendsKeepTheItemAndTellTheCoordinator(t *testing.T) {
	r := newAnsRig(t)
	r.setErr(errors.New("boom: write failed"))
	it := r.add(t, `{"question":"Which?"}`)
	r.answer(t, it.ID, "the second")
	fail := func(n int) {
		t.Helper()
		until(t, "a failed try", func() bool {
			g := r.state(it.ID)
			return g.Relay != nil && (g.Relay.Tries == n || g.Relay.State == relayGaveUp)
		})
		// Let the retry pause pass.
		r.s.todos.mu.Lock()
		r.s.todos.relayFail = map[string]time.Time{}
		r.s.todos.mu.Unlock()
	}
	fail(1)
	if n := r.told(); len(n) != 0 {
		t.Fatalf("told after one failure: %v", n)
	}
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	fail(2)
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	until(t, "the coordinator notice", func() bool { return len(r.told()) == 1 })
	if n := r.told()[0]; !strings.Contains(n, "3 sends failed") || !strings.Contains(n, "boom") {
		t.Fatalf("notice = %s", n)
	}
	if g := r.state(it.ID); g.State != TodoAnswered || g.Relay.State != relayGaveUp {
		t.Fatalf("item = %+v", g)
	}
	if _, held := r.s.sent.get(relaySubject(r.state(it.ID))); held {
		t.Fatalf("the claim of a failed send was kept")
	}
	if ev := r.events(evRelayFailed); len(ev) != 1 {
		t.Fatalf("events = %+v", ev)
	}
	// The coordinator told once; no more passes resend it.
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	pause()
	if n := r.told(); len(n) != 1 {
		t.Fatalf("told = %v", n)
	}
}

func TestRelayAFailureThatRecoversIsRelayedOnce(t *testing.T) {
	r := newAnsRig(t)
	r.setErr(errors.New("flaky"))
	it := r.add(t, `{"question":"Which?"}`)
	r.answer(t, it.ID, "the second")
	until(t, "a failed try", func() bool { g := r.state(it.ID); return g.Relay != nil && g.Relay.Tries == 1 })
	r.setErr(nil)
	r.s.todos.mu.Lock()
	r.s.todos.relayFail = map[string]time.Time{}
	r.s.todos.mu.Unlock()
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	until(t, "the item to close", func() bool { return r.state(it.ID).State == TodoDone })
	if n := len(r.relays()); n != 1 || len(r.told()) != 0 {
		t.Fatalf("relays = %d told = %v", n, r.told())
	}
}

func TestRelayAClaimedAnswerIsNotSentAgain(t *testing.T) {
	r := newAnsRig(t)
	it := r.add(t, `{"question":"Which?"}`)
	// A hub that died after claiming (or after sending, before closing) left the claim.
	r.s.todos.mutate(it.ID, func(i *Todo) error {
		i.State, i.Answer, i.Notice = TodoAnswered, &TodoAnswer{Text: "the second", By: "me", Via: "app", At: time.Now()}, &TodoNotice{State: todoNoticePending, At: time.Now()}
		return nil
	})
	if !r.s.sent.claim(relaySubject(r.state(it.ID)), "desk/w2:p1") {
		t.Fatal("claim")
	}
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	until(t, "the coordinator notice", func() bool { return len(r.told()) == 1 })
	if n := r.told()[0]; !strings.Contains(n, "may or may not have arrived") {
		t.Fatalf("notice = %s", n)
	}
	pause()
	if n := r.relays(); len(n) != 0 {
		t.Fatalf("sent again: %v", n)
	}
}

func TestRelayConcurrentPassesSendOnce(t *testing.T) {
	r := newAnsRig(t)
	it := r.add(t, `{"question":"Which?"}`)
	r.answer(t, it.ID, "the second")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.s.deliverTodoNotices(r.s.fleet.Agents()) }()
	}
	wg.Wait()
	until(t, "the item to close", func() bool { return r.state(it.ID).State == TodoDone })
	pause()
	if n := len(r.relays()); n != 1 {
		t.Fatalf("relays = %d", n)
	}
}

func TestRelayDecisionSetEarlyThenFinal(t *testing.T) {
	r := newAnsRig(t)
	it := setAdd(t, r.s, setBody)
	putDraft(r.s, it.ID, `{"choices":{"D1":{"option":1,"confirmed":true}}}`)
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	until(t, "the early relay", func() bool { return len(r.relays()) == 1 })
	got := r.relays()[0]
	if !strings.Contains(got, "desk/w2:p1: Comebye (hub relay)") || !strings.Contains(got, "D1 Layout: One bucket") || !strings.Contains(got, "ahead of Submit") {
		t.Fatalf("early relay = %s", got)
	}
	until(t, "the early notice to settle", func() bool { g := r.state(it.ID); return g.Notice != nil && g.Notice.State == todoNoticeSent })
	if g := r.state(it.ID); g.State != TodoOpen || len(g.Set.EarlyPending) != 0 {
		t.Fatalf("after early = %s %v", g.State, g.Set.EarlyPending)
	}
	pause()
	if n := r.told(); len(n) != 0 {
		t.Fatalf("told = %v", n)
	}
	// Saving the same early choice again does not relay again.
	putDraft(r.s, it.ID, `{"choices":{"D1":{"option":1,"confirmed":true},"D3":{"option":1,"confirmed":true},"D6":{"option":1,"confirmed":true}}}`)
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	pause()
	if n := len(r.relays()); n != 1 {
		t.Fatalf("relays = %d", n)
	}
	// Submit: the final answer is relayed and the item closes.
	if w := do(r.s, "POST", "/api/todos/"+it.ID+"/submit", userHdr, ``); w.Code != 200 {
		t.Fatalf("submit = %d %s", w.Code, w.Body)
	}
	until(t, "the final relay", func() bool { return len(r.relays()) == 2 })
	if g := r.relays()[1]; !strings.Contains(g, "the user answered your to-do") || !strings.Contains(g, "Layout") {
		t.Fatalf("final relay = %s", g)
	}
	until(t, "the item to close", func() bool { return r.state(it.ID).State == TodoDone })
	if n := r.told(); len(n) != 0 {
		t.Fatalf("told = %v", n)
	}
	if ev := r.events(evRelay); len(ev) != 2 {
		t.Fatalf("events = %+v", ev)
	}
}

func TestRelayDecisionSetEarlyGateGoesToTheCoordinator(t *testing.T) {
	r := newAnsRig(t)
	body := strings.Replace(setBody, `{"label":"One bucket"}`, `{"label":"Start with one bucket"}`, 1)
	it := setAdd(t, r.s, body)
	putDraft(r.s, it.ID, `{"choices":{"D1":{"option":1,"confirmed":true}}}`)
	r.s.deliverTodoNotices(r.s.fleet.Agents())
	until(t, "the early notice", func() bool { return len(r.told()) == 1 })
	if n := r.told()[0]; !strings.Contains(n, "settled decision(s) D1") {
		t.Fatalf("notice = %s", n)
	}
	pause()
	if n := r.relays(); len(n) != 0 {
		t.Fatalf("relayed a gate: %v", n)
	}
}
