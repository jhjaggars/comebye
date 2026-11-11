package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/transcript"
)

var autoT0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func autoAgent(id, name string, st herdr.AgentStatus, since time.Time) herdr.AgentInfo {
	host, pane, _ := strings.Cut(id, "/")
	return herdr.AgentInfo{ID: id, Host: host, PaneID: pane, Name: name, Agent: "claude", Status: st, StatusSince: &since}
}

type autoRig struct {
	s     *Server
	reads int
	view  map[string]bool
	dlg   *dialog.Dialog
	// the agents' last message, and how often it was read
	text    string
	msgTime string
	lreads  int
	clock   time.Time
}

func newAutoRig(t *testing.T) *autoRig {
	t.Helper()
	s, _, _ := todoServer(t)
	s.initAutoTodo(AutoTodoConfig{Blocked: 10 * time.Minute, Question: 3 * time.Minute})
	r := &autoRig{s: s, view: map[string]bool{}, dlg: &dialog.Dialog{
		Kind: dialog.KindClaudePermission, Title: "Bash command", Detail: "rm -rf build/", Question: "Do you want to proceed?",
		Options: []dialog.Option{{Label: "Yes"}, {Label: "Yes, and don't ask again"}, {Label: "No"}, {Label: "Type something", Role: dialog.RoleOther}},
	}}
	s.auto.readDialog = func(context.Context, herdr.AgentInfo) (*dialog.Dialog, error) { r.reads++; return r.dlg, nil }
	s.auto.viewing = func(a herdr.AgentInfo) bool { return r.view[a.ID] }
	s.auto.latest = func(context.Context, herdr.AgentInfo) *transcript.Latest {
		r.lreads++
		return &transcript.Latest{Text: r.text, Time: r.msgTime}
	}
	r.clock = autoT0
	s.todos.now = func() time.Time { return r.clock }
	return r
}

func (r *autoRig) step(now time.Time, agents ...herdr.AgentInfo) {
	r.clock = now
	r.s.autoTodoStep(context.Background(), agents, now)
}

func (r *autoRig) items(state string) []Todo { return r.s.todos.list(state) }

func TestAutoBlockedThreshold(t *testing.T) {
	r := newAutoRig(t)
	a := autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0)
	r.step(autoT0.Add(9*time.Minute), a)
	if len(r.items("all")) != 0 || r.reads != 0 {
		t.Fatalf("9 min: %d items, %d screen reads; want none and none", len(r.items("all")), r.reads)
	}
	r.step(autoT0.Add(11*time.Minute), a)
	got := r.items("")
	if len(got) != 1 || r.reads != 1 {
		t.Fatalf("11 min: %d items, %d reads; want 1 and 1", len(got), r.reads)
	}
	it := got[0]
	if it.Kind != "action" || it.CreatedBy != "hub" || it.Source != SourceBlocked || it.State != TodoOpen ||
		it.Waiting == nil || it.Waiting.ID != "desk/w2:p1" || it.Waiting.Name != "home" {
		t.Fatalf("item = %+v", it)
	}
	if len(it.Links) != 1 || it.Links[0].URL != "/#/agent/w2:p1" {
		t.Fatalf("links = %+v", it.Links)
	}
	if len(it.Options) != 3 || it.Options[0].Label != "Yes" || it.Options[2].Label != "No" {
		t.Fatalf("options = %+v", it.Options)
	}
	for _, want := range []string{"rm -rf build/", "1. **Yes**", "3. **No**", "11 minutes", "in its pane"} {
		if !strings.Contains(it.Context, want) {
			t.Errorf("context lacks %q:\n%s", want, it.Context)
		}
	}
	if strings.Contains(it.Context, "Type something") {
		t.Errorf("the free-text row is not an option:\n%s", it.Context)
	}
	// Ticks while it is still blocked change nothing and read nothing more.
	r.step(autoT0.Add(20*time.Minute), a)
	if len(r.items("")) != 1 || r.reads != 1 {
		t.Fatalf("later tick: %d items, %d reads", len(r.items("")), r.reads)
	}
}

func TestAutoBlockedFlappingMakesOne(t *testing.T) {
	r := newAutoRig(t)
	blocked := autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0)
	r.step(autoT0.Add(11*time.Minute), blocked)
	r.step(autoT0.Add(12*time.Minute), blocked) // no change
	if n := len(r.items("")); n != 1 {
		t.Fatalf("items = %d", n)
	}
	// The agent flaps: each blocked spell is shorter than the threshold, so
	// the one item (closed when it first left blocked) is all there is.
	for i := 0; i < 5; i++ {
		at := autoT0.Add(time.Hour + time.Duration(i)*4*time.Minute)
		r.step(at, autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, at.Add(-2*time.Minute)))
		r.step(at.Add(time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusWorking, at.Add(time.Minute)))
	}
	if n := len(r.items("all")); n != 1 || len(r.items("")) != 0 {
		t.Fatalf("after flapping: %d items in all, %d active; want the one, closed", n, len(r.items("")))
	}
	if r.reads != 1 {
		t.Fatalf("reads = %d, want 1", r.reads)
	}
}

func TestAutoBlockedClosedItemIsNotRefiled(t *testing.T) {
	r := newAutoRig(t)
	blocked := autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0)
	r.step(autoT0.Add(11*time.Minute), blocked)
	id := r.items("")[0].ID
	if _, err := r.s.todos.close(id, TodoDropped, "me", "not needed"); err != nil {
		t.Fatal(err)
	}
	r.step(autoT0.Add(14*time.Minute), blocked)
	if n := len(r.items("")); n != 0 || r.reads != 1 {
		t.Fatalf("a closed item came back: %d active, %d reads", n, r.reads)
	}
	// A later block of the same agent is a new episode and gets its own.
	later := autoT0.Add(time.Hour)
	r.step(later.Add(11*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, later))
	if n := len(r.items("")); n != 1 {
		t.Fatalf("new episode: %d active", n)
	}
}

func TestAutoBlockedClosesWhenAgentLeaves(t *testing.T) {
	r := newAutoRig(t)
	r.step(autoT0.Add(11*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0))
	id := r.items("")[0].ID
	later := autoT0.Add(15 * time.Minute)
	r.step(later, autoAgent("desk/w2:p1", "home", herdr.StatusWorking, later))
	it, ok := r.s.todos.get(id)
	if !ok || it.State != TodoDropped || it.ClosedBy != "hub" || it.DropReason != "the agent is no longer blocked" || it.ClosedAt == nil {
		t.Fatalf("after leaving: %+v", it)
	}
	// Gone from a host that still lists others: closed too; a host that
	// lists nobody is probably down, so its items stay.
	r.step(autoT0.Add(time.Hour), autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0.Add(30*time.Minute)))
	id2 := r.items("")[0].ID
	r.step(autoT0.Add(2 * time.Hour))
	if it, _ := r.s.todos.get(id2); it.State != TodoOpen {
		t.Fatalf("host not listed at all: %+v", it)
	}
	r.step(autoT0.Add(2*time.Hour), autoAgent("desk/w1:p1", "coordinator", herdr.StatusIdle, autoT0))
	if it, _ := r.s.todos.get(id2); it.State != TodoDropped || it.DropReason != "the agent is gone" {
		t.Fatalf("vanished agent: %+v", it)
	}
}

func TestAutoBlockedSkips(t *testing.T) {
	r := newAutoRig(t)
	a := autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0)
	late := autoT0.Add(30 * time.Minute)

	r.view["desk/w2:p1"] = true
	r.step(late, a)
	if len(r.items("all")) != 0 || r.reads != 0 {
		t.Fatalf("viewed agent: %d items, %d reads", len(r.items("all")), r.reads)
	}
	r.view["desk/w2:p1"] = false
	r.s.MuteAutoTodo("desk/w2:p1", true)
	r.step(late, a)
	if len(r.items("all")) != 0 {
		t.Fatalf("muted agent got an item")
	}
	r.s.MuteAutoTodo("desk/w2:p1", false)

	// A person's (or the agent's own) item already waits on it.
	if _, _, err := r.s.todos.add(Todo{Question: "may I?", Kind: "decision", CreatedBy: "me", Waiting: &TodoAgent{ID: "desk/w2:p1", Instance: r.s.hostInstance("desk")}}); err != nil {
		t.Fatal(err)
	}
	r.step(late, a)
	if len(r.items("")) != 1 || r.reads != 0 {
		t.Fatalf("human item waiting: %d items, %d reads", len(r.items("")), r.reads)
	}
	// Not blocked: nothing, however long.
	r.step(late, autoAgent("desk/w1:p1", "coordinator", herdr.StatusIdle, autoT0))
	if len(r.items("")) != 1 {
		t.Fatal("an idle agent got an item")
	}
}

func TestAutoBlockedUnrecognisedAndUnreadable(t *testing.T) {
	r := newAutoRig(t)
	r.dlg = nil
	r.step(autoT0.Add(11*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0))
	it := r.items("")[0]
	if !strings.Contains(it.Context, "no dialog recognised") {
		t.Fatalf("context = %q", it.Context)
	}
	r2 := newAutoRig(t)
	r2.s.auto.readDialog = func(context.Context, herdr.AgentInfo) (*dialog.Dialog, error) { return nil, fmt.Errorf("pane gone") }
	r2.step(autoT0.Add(11*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0))
	if it := r2.items("")[0]; !strings.Contains(it.Context, "could not be read") || !strings.Contains(it.Context, "pane gone") {
		t.Fatalf("context = %q", it.Context)
	}
}

func TestAutoCaps(t *testing.T) {
	r := newAutoRig(t)
	var agents []herdr.AgentInfo
	for i := 0; i < 30; i++ {
		agents = append(agents, autoAgent(fmt.Sprintf("desk/w%d:p1", 100+i), fmt.Sprintf("a%d", i), herdr.StatusBlocked, autoT0))
	}
	r.step(autoT0.Add(time.Hour), agents...)
	if n := len(r.items("")); n != autoMaxActive {
		t.Fatalf("auto items = %d, want the cap %d", n, autoMaxActive)
	}
	// Human items still fit: the auto cap leaves room, the global cap is separate.
	for i := 0; i < 15; i++ { // the 20 auto adds did not use up the rate limit
		if _, _, err := r.s.todos.add(Todo{Question: fmt.Sprintf("q%d", i), Kind: "decision", CreatedBy: "me"}); err != nil {
			t.Fatalf("human add %d: %v", i, err)
		}
	}
	// Per agent: five active items for one agent is the limit.
	st := newTodoStore("")
	w := &TodoAgent{ID: "desk/w9:p1"}
	for i := 0; i < autoMaxPerAgent+1; i++ {
		_, err := st.addAuto(Todo{Question: "q", CreatedBy: "hub", Source: SourceBlocked, AutoKey: fmt.Sprint("k", i), Waiting: w})
		if (i < autoMaxPerAgent) != (err == nil) {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	// The global cap of 200 holds against auto items too.
	st = newTodoStore("")
	for i := 0; i < todoMaxActive; i++ {
		st.data.Items = append(st.data.Items, Todo{ID: fmt.Sprint("x", i), State: TodoOpen, CreatedBy: "me"})
	}
	if _, err := st.addAuto(Todo{Question: "q", CreatedBy: "hub", AutoKey: "k", Waiting: w}); err != errTodoFull {
		t.Fatalf("global cap: %v", err)
	}
}

func TestAutoStubForAgentReaders(t *testing.T) {
	r := newAutoRig(t)
	s := r.s
	// w2:p1 ("home") is a plain session; w1:p1 is the coordinator.
	r.step(autoT0.Add(11*time.Minute),
		autoAgent("desk/w2:p1", "home", herdr.StatusBlocked, autoT0),
		autoAgent("desk/w1:p1", "coordinator", herdr.StatusBlocked, autoT0))
	read := func(hdr map[string]string) []Todo {
		w := do(s, "GET", "/api/todos", hdr, "")
		var l struct{ Todos []Todo }
		if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		return l.Todos
	}
	byWho := func(items []Todo, who string) Todo {
		for _, it := range items {
			if it.Waiting != nil && it.Waiting.ID == who {
				return it
			}
		}
		t.Fatalf("no item for %s in %+v", who, items)
		return Todo{}
	}
	user := read(userHdr)
	if it := byWho(user, "desk/w2:p1"); !strings.Contains(it.Context, "rm -rf build/") {
		t.Fatalf("the user sees the full text: %+v", it)
	}
	agent := read(agentHdr("w1:p1"))
	plain := byWho(agent, "desk/w2:p1")
	if plain.Context != "" || len(plain.Links) != 0 || !strings.Contains(plain.Question, "waiting on you") || strings.Contains(plain.Question, "blocked at a dialog") {
		t.Fatalf("a plain session's item is a stub for agents: %+v", plain)
	}
	if it := byWho(agent, "desk/w1:p1"); !strings.Contains(it.Context, "rm -rf build/") {
		t.Fatalf("the coordinator's own item is whole: %+v", it)
	}
	// show goes through the same filter.
	w := do(s, "GET", "/api/todos/"+plain.ID, agentHdr("w1:p1"), "")
	if strings.Contains(w.Body.String(), "rm -rf") {
		t.Fatalf("show leaked: %s", w.Body)
	}
	w = do(s, "GET", "/api/todos/"+plain.ID, userHdr, "")
	if !strings.Contains(w.Body.String(), "rm -rf") {
		t.Fatalf("show for the user lacks the text: %s", w.Body)
	}
}

// ---- B2/E2: idle on a question ----

const askText = "I rebased and the tests pass.\n\nShould I push it to main, or open a PR first?"

func TestAutoQuestionThresholdAndClassifier(t *testing.T) {
	r := newAutoRig(t)
	r.text = askText
	idle := autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0)
	r.step(autoT0.Add(2*time.Minute), idle)
	if len(r.items("all")) != 0 || r.lreads != 0 {
		t.Fatalf("2 min: %d items, %d reads; want none and none", len(r.items("all")), r.lreads)
	}
	r.step(autoT0.Add(4*time.Minute), idle)
	got := r.items("")
	if len(got) != 1 || r.lreads != 1 {
		t.Fatalf("4 min: %d items, %d reads", len(got), r.lreads)
	}
	it := got[0]
	if it.Source != SourceQuestion || !it.Suggested || it.CreatedBy != "hub" || it.Kind != "decision" ||
		it.Waiting == nil || it.Waiting.ID != "desk/w2:p1" || !strings.Contains(it.Question, "Should I push it to main") ||
		!strings.Contains(it.Context, "> Should I push it to main") {
		t.Fatalf("item = %+v", it)
	}
	// Later ticks neither refile nor read again.
	r.step(autoT0.Add(30*time.Minute), idle)
	if len(r.items("")) != 1 || r.lreads != 1 {
		t.Fatalf("later: %d items, %d reads", len(r.items("")), r.lreads)
	}
}

func TestAutoQuestionDoneCountsAndNonQuestionsAreReadOnce(t *testing.T) {
	r := newAutoRig(t)
	r.text = "All done. Anything else?"
	done := autoAgent("desk/w2:p1", "home", herdr.StatusDone, autoT0)
	for i := 4; i < 40; i += 6 {
		r.step(autoT0.Add(time.Duration(i)*time.Minute), done)
	}
	if len(r.items("all")) != 0 || r.lreads != 1 {
		t.Fatalf("a closer: %d items, %d reads; want none and one read", len(r.items("all")), r.lreads)
	}
	r2 := newAutoRig(t)
	r2.text = askText
	r2.step(autoT0.Add(5*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusDone, autoT0))
	if len(r2.items("")) != 1 {
		t.Fatal("a done agent on a question gets a suggestion")
	}
}

func TestAutoQuestionClosesWhenAgentWorks(t *testing.T) {
	r := newAutoRig(t)
	r.text = askText
	r.step(autoT0.Add(4*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0))
	id := r.items("")[0].ID
	at := autoT0.Add(6 * time.Minute)
	r.step(at, autoAgent("desk/w2:p1", "home", herdr.StatusWorking, at))
	it, _ := r.s.todos.get(id)
	if it.State != TodoDropped || it.ClosedBy != "hub" || it.DropReason != "the agent is no longer idle" {
		t.Fatalf("after the agent worked: %+v", it)
	}
	// Idle again later on the same question: a new episode, one new suggestion.
	later := autoT0.Add(time.Hour)
	r.step(later.Add(4*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, later))
	if n := len(r.items("")); n != 1 {
		t.Fatalf("new episode: %d active", n)
	}
}

func TestAutoQuestionSkips(t *testing.T) {
	r := newAutoRig(t)
	r.text = askText
	a := autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0)
	late := autoT0.Add(20 * time.Minute)
	r.view[a.ID] = true
	r.step(late, a)
	r.view[a.ID] = false
	r.s.MuteAutoTodo(a.ID, true)
	r.step(late, a)
	r.s.MuteAutoTodo(a.ID, false)
	if len(r.items("all")) != 0 || r.lreads != 0 {
		t.Fatalf("viewed or muted: %d items, %d reads", len(r.items("all")), r.lreads)
	}
	// A person's item already waits on it.
	if _, _, err := r.s.todos.add(Todo{Question: "may I?", Kind: "decision", CreatedBy: "me", Waiting: &TodoAgent{ID: a.ID, Instance: r.s.hostInstance("desk")}}); err != nil {
		t.Fatal(err)
	}
	r.step(late, a)
	if len(r.items("")) != 1 || r.lreads != 0 {
		t.Fatalf("an item waits already: %d items, %d reads", len(r.items("")), r.lreads)
	}
	// A working agent is not read.
	r.step(late, autoAgent("desk/w1:p1", "coordinator", herdr.StatusWorking, autoT0))
	if r.lreads != 0 {
		t.Fatal("a working agent was read")
	}
}

func TestAutoQuestionCoordinatorFiledItsOwn(t *testing.T) {
	// E2: the coordinator filed an item during the turn: nothing is suggested.
	r := newAutoRig(t)
	r.text = askText
	r.msgTime = autoT0.Add(-time.Minute).Format(time.RFC3339Nano)
	r.clock = autoT0.Add(-5 * time.Minute)
	if _, _, err := r.s.todos.add(Todo{Question: "pick one", Kind: "decision", CreatedBy: "coordinator", CreatedByID: "desk/w1:p1"}); err != nil {
		t.Fatal(err)
	}
	coord := autoAgent("desk/w1:p1", "coordinator", herdr.StatusIdle, autoT0)
	r.step(autoT0.Add(4*time.Minute), coord)
	if n := len(r.items("")); n != 1 {
		t.Fatalf("items = %d, want only the coordinator's own", n)
	}

	// Filed later: the suggestion goes, superseded.
	r2 := newAutoRig(t)
	r2.text = askText
	r2.msgTime = autoT0.Format(time.RFC3339Nano)
	coord2 := autoAgent("desk/w1:p1", "coordinator", herdr.StatusIdle, autoT0)
	r2.step(autoT0.Add(4*time.Minute), coord2)
	sug := r2.items("")
	if len(sug) != 1 || !sug[0].Suggested {
		t.Fatalf("no suggestion: %+v", sug)
	}
	r2.clock = autoT0.Add(5 * time.Minute)
	filed, _, err := r2.s.todos.add(Todo{Question: "pick one", Kind: "decision", CreatedBy: "coordinator", CreatedByID: "desk/w1:p1"})
	if err != nil {
		t.Fatal(err)
	}
	r2.step(autoT0.Add(5*time.Minute), coord2)
	it, _ := r2.s.todos.get(sug[0].ID)
	if it.State != TodoDropped || it.DropReason != "superseded by "+filed.ID {
		t.Fatalf("not superseded: %+v", it)
	}
}

func TestAutoQuestionKeepAndDismiss(t *testing.T) {
	r := newAutoRig(t)
	r.text = askText
	a := autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0)
	r.step(autoT0.Add(4*time.Minute), a)
	id := r.items("")[0].ID

	w := do(r.s, "POST", "/api/todos/"+id+"/keep", userHdr, "")
	if w.Code != 200 || todoOf(t, w.Body.Bytes()).Suggested {
		t.Fatalf("keep = %d %s", w.Code, w.Body)
	}
	// A kept item stays when the agent works.
	at := autoT0.Add(6 * time.Minute)
	r.step(at, autoAgent("desk/w2:p1", "home", herdr.StatusWorking, at))
	if it, _ := r.s.todos.get(id); it.State != TodoOpen {
		t.Fatalf("a kept item was closed: %+v", it)
	}
	// While the kept item is open, the agent is already waited on: no new
	// suggestion. Once it is done, a new episode gets one.
	r.step(autoT0.Add(30*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0.Add(20*time.Minute)))
	if n := len(r.items("")); n != 1 {
		t.Fatalf("a suggestion beside a kept item: %d active", n)
	}
	if _, err := r.s.todos.close(id, TodoDone, "me", ""); err != nil {
		t.Fatal(err)
	}
	// Dismiss another episode.
	r.step(autoT0.Add(time.Hour+4*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0.Add(time.Hour)))
	var sug Todo
	for _, it := range r.items("") {
		if it.Suggested {
			sug = it
		}
	}
	if sug.ID == "" {
		t.Fatal("no second suggestion")
	}
	if w := do(r.s, "POST", "/api/todos/"+sug.ID+"/dismiss", userHdr, `{"reason":"nope"}`); w.Code != 400 {
		t.Fatalf("bad reason = %d", w.Code)
	}
	w = do(r.s, "POST", "/api/todos/"+sug.ID+"/dismiss", userHdr, `{"reason":"not_a_question"}`)
	if w.Code != 200 {
		t.Fatalf("dismiss = %d %s", w.Code, w.Body)
	}
	it, _ := r.s.todos.get(sug.ID)
	if it.State != TodoDropped || it.DropReason != "dismissed: not a question for me" {
		t.Fatalf("dismissed item = %+v", it)
	}
	stats := r.s.todos.autoStats()
	if stats["kept:hub-question"] != 1 || stats["dismissed:hub-question"] != 1 || stats["dismissed:hub-question:not_a_question"] != 1 {
		t.Fatalf("stats = %v", stats)
	}
	if !strings.Contains(do(r.s, "GET", "/api/todos", userHdr, "").Body.String(), `"dismissed:hub-question":1`) {
		t.Fatal("the list lacks the counts")
	}
	// A dismissed episode is not refiled.
	r.step(autoT0.Add(time.Hour+30*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0.Add(time.Hour)))
	for _, it := range r.items("") {
		if it.Suggested {
			t.Fatalf("refiled: %+v", it)
		}
	}
	// Answering a suggestion keeps it.
	r.step(autoT0.Add(3*time.Hour+4*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0.Add(3*time.Hour)))
	for _, it := range r.items("") {
		if it.Suggested {
			if _, err := r.s.todos.answer(it.ID, TodoAnswer{Text: "ok", By: "me", Via: "app"}); err != nil {
				t.Fatal(err)
			}
			if got, _ := r.s.todos.get(it.ID); got.Suggested {
				t.Fatal("an answered suggestion is still a suggestion")
			}
		}
	}
	if r.s.todos.autoStats()["kept:hub-question"] != 2 {
		t.Fatalf("stats = %v", r.s.todos.autoStats())
	}
}

func TestAutoQuestionStubForInteractiveSessions(t *testing.T) {
	r := newAutoRig(t)
	r.text = askText + " The key is sk-secret-123."
	s := r.s
	r.step(autoT0.Add(4*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0))
	list := func(hdr map[string]string) string { return do(s, "GET", "/api/todos", hdr, "").Body.String() }
	if !strings.Contains(list(userHdr), "sk-secret-123") {
		t.Fatal("the user sees the text")
	}
	agent := list(agentHdr("w1:p1"))
	if strings.Contains(agent, "secret") || strings.Contains(agent, "Should I push") || !strings.Contains(agent, "session home is waiting on you") {
		t.Fatalf("agents see a stub of an interactive session: %s", agent)
	}
	// The coordinator's own suggestion is whole for agent readers (it is not an interactive session).
	r.step(autoT0.Add(5*time.Minute), autoAgent("desk/w1:p1", "coordinator", herdr.StatusIdle, autoT0.Add(time.Minute)))
	if !strings.Contains(list(agentHdr("w1:p1")), "Should I push") {
		t.Fatal("the coordinator's suggestion should be readable by agents")
	}
}

// ---- one item per question ----

const provenanceText = "I put three mockups on the Pages screen. They share one data model.\n\n" +
	"## Decisions for the user\n\n" +
	"1. **Where should the provenance live?** In the artifact header, or as a footer line?\n" +
	"   1. Header chip - always visible, costs a row\n" +
	"   2. Footer line - quieter, easy to miss\n" +
	"   Recommend: 1\n" +
	"2. **Should the chip show the full commit SHA or seven characters?**\n" +
	"3. Which hosts get the badge: only remote ones, or every host?\n\n" +
	"Or should I change any of the mockups first?"

func TestAutoQuestionOneItemPerQuestion(t *testing.T) {
	r := newAutoRig(t)
	r.text = provenanceText
	a := autoAgent("desk/w2:p1", "provenance", herdr.StatusIdle, autoT0)
	r.step(autoT0.Add(4*time.Minute), a)
	got := r.items("")
	if len(got) != 4 || r.lreads != 1 {
		t.Fatalf("%d items, %d reads; want 4 and 1: %+v", len(got), r.lreads, got)
	}
	first := got[0]
	if !strings.Contains(first.Question, "Where should the provenance live?") || !first.Suggested || first.Source != SourceQuestion {
		t.Fatalf("first = %+v", first)
	}
	if len(first.Options) != 2 || first.Options[0].Label != "Header chip" || len(first.Recommend) != 1 || first.Recommend[0] != 1 {
		t.Fatalf("options %+v recommend %v", first.Options, first.Recommend)
	}
	// The choices are buttons, not a numbered list in the text; the
	// agent's own words and the session link are there.
	for _, bad := range []string{"1. ", "Header chip", "Recommend"} {
		if strings.Contains(first.Context, bad) {
			t.Errorf("context repeats the options (%q):\n%s", bad, first.Context)
		}
	}
	for _, want := range []string{"They share one data model", "What it said before asking"} {
		if !strings.Contains(first.Context, want) {
			t.Errorf("context lacks %q:\n%s", want, first.Context)
		}
	}
	if len(first.Links) != 1 || first.Links[0].URL != "/#/agent/w2:p1" {
		t.Fatalf("links = %+v", first.Links)
	}
	if !strings.Contains(got[3].Question, "should I change any of the mockups first?") {
		t.Fatalf("the trailing question is item 4: %q", got[3].Question)
	}
	// Later ticks refile nothing and read nothing more.
	r.step(autoT0.Add(30*time.Minute), a)
	if len(r.items("")) != 4 || r.lreads != 1 {
		t.Fatalf("later: %d items, %d reads", len(r.items("")), r.lreads)
	}
	// A hub restart forgets what it read; the items already filed are not
	// filed twice, per agent and question.
	r.s.auto.checked = map[string]string{}
	r.s.todos.mu.Lock()
	for i := range r.s.todos.data.Items {
		r.s.todos.data.Items[i].Waiting.ID = "desk/w2:p1"
	}
	r.s.todos.mu.Unlock()
	r.step(autoT0.Add(31*time.Minute), a)
	if n := len(r.items("")); n != 4 {
		t.Fatalf("after a restart: %d items, want the same 4", n)
	}
	// Closing the agent's episode closes the suggestions with it.
	at := autoT0.Add(40 * time.Minute)
	r.step(at, autoAgent("desk/w2:p1", "provenance", herdr.StatusWorking, at))
	if n := len(r.items("")); n != 0 {
		t.Fatalf("%d still open after the agent went on", n)
	}
}

func TestAutoQuestionSingleOneItemWithOptions(t *testing.T) {
	r := newAutoRig(t)
	r.text = "The level test needs a baseline. Should I record it from the current build, or from the last release tag?\n\n1. Current build - fastest\n2. Last release tag - clean\n\nRecommend: 2"
	r.step(autoT0.Add(4*time.Minute), autoAgent("desk/w2:p1", "mac pi", herdr.StatusIdle, autoT0))
	got := r.items("")
	if len(got) != 1 || len(got[0].Options) != 2 || len(got[0].Recommend) != 1 || got[0].Recommend[0] != 2 ||
		!strings.Contains(got[0].Context, "needs a baseline") {
		t.Fatalf("%+v", got)
	}
}

func TestAutoQuestionDedupesPerQuestionAcrossWordings(t *testing.T) {
	st := newTodoStore("")
	w := &TodoAgent{ID: "desk/w9:p1"}
	if _, err := st.addAuto(Todo{Question: "home asks: Should I commit?", CreatedBy: "hub", Source: SourceQuestion, AutoKey: "k1", Waiting: w}); err != nil {
		t.Fatal(err)
	}
	if !st.activeQuestion("desk/w9:p1", "home asks:  should I COMMIT ?") || st.activeQuestion("desk/w8:p1", "home asks: Should I commit?") ||
		st.activeQuestion("desk/w9:p1", "Should I push?") {
		t.Fatal("same agent and question only")
	}
}

func TestAutoQuestionSkipsAnAgentTheUserIsChattingIn(t *testing.T) {
	r := newAutoRig(t)
	r.text = askText
	a := autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0)
	r.s.auto.active = r.s.userActive
	pres := push.NewPresence()
	r.s.cfg.Presence = pres
	// The user had the agent open 5 minutes ago and closed the tab.
	pres.Update("tab", "w2:p1", true)
	pres.Update("tab", "", false)
	now := time.Now()
	r.step(now.Add(4*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, now))
	if len(r.items("all")) != 0 || r.lreads != 0 {
		t.Fatalf("viewed moments ago: %d items, %d reads", len(r.items("all")), r.lreads)
	}
	// Typing in its input box counts too.
	pres2 := push.NewPresence()
	r.s.cfg.Presence = pres2
	r.s.typing = newTypingTracker()
	r.s.typing.observe("w2:p1", dialog.Input{Known: true, Busy: true})
	r.step(now.Add(5*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, now))
	if len(r.items("all")) != 0 || r.lreads != 0 {
		t.Fatalf("typing: %d items, %d reads", len(r.items("all")), r.lreads)
	}
	// Long after (past the window) the agent is fair game.
	r.s.typing = newTypingTracker()
	r.s.cfg.Presence = push.NewPresence()
	_ = a
	r.step(now.Add(20*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, now))
	if len(r.items("")) != 1 {
		t.Fatalf("no suggestion once the user was away: %d", len(r.items("")))
	}
}

func TestAutoQuestionManyItemsStubbedForAgentReaders(t *testing.T) {
	r := newAutoRig(t)
	r.text = provenanceText + " The key is sk-secret-123."
	r.step(autoT0.Add(4*time.Minute), autoAgent("desk/w2:p1", "home", herdr.StatusIdle, autoT0))
	body := do(r.s, "GET", "/api/todos", agentHdr("w1:p1"), "").Body.String()
	if strings.Contains(body, "secret") || strings.Contains(body, "provenance live") || strings.Count(body, "session home is waiting on you") != 4 {
		t.Fatalf("agents see only stubs: %s", body)
	}
}
