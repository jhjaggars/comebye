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
}

func newAutoRig(t *testing.T) *autoRig {
	t.Helper()
	s, _, _ := todoServer(t)
	s.initAutoTodo(AutoTodoConfig{Blocked: 10 * time.Minute})
	r := &autoRig{s: s, view: map[string]bool{}, dlg: &dialog.Dialog{
		Kind: dialog.KindClaudePermission, Title: "Bash command", Detail: "rm -rf build/", Question: "Do you want to proceed?",
		Options: []dialog.Option{{Label: "Yes"}, {Label: "Yes, and don't ask again"}, {Label: "No"}, {Label: "Type something", Role: dialog.RoleOther}},
	}}
	s.auto.readDialog = func(context.Context, herdr.AgentInfo) (*dialog.Dialog, error) { r.reads++; return r.dlg, nil }
	s.auto.viewing = func(a herdr.AgentInfo) bool { return r.view[a.ID] }
	return r
}

func (r *autoRig) step(now time.Time, agents ...herdr.AgentInfo) {
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
	for _, want := range []string{"rm -rf build/", "1. Yes\n", "3. No\n", "11 minutes"} {
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
	r.step(autoT0.Add(2*time.Hour))
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

