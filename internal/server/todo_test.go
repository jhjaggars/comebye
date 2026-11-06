package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func todoOf(t *testing.T, body []byte) Todo {
	t.Helper()
	var r struct {
		Todo Todo `json:"todo"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r.Todo
}

// todoServer is coordServer with w1:p1 recorded as the coordinator and the
// notice sender replaced by a recorder.
func todoServer(t *testing.T) (*Server, *[]string, *sync.Mutex) {
	t.Helper()
	s, _, _ := coordServer(t)
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	s.todos.send = func(ctx context.Context, a herdr.AgentInfo, text string) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, a.ID+": "+text)
		return nil
	}
	return s, &sent, &mu
}

func TestTodoAgentAddsAndLists(t *testing.T) {
	s, _, _ := todoServer(t)
	w := do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"may the worker delete the old `+"`key`"+`?","kind":"decision","context":"## why\nbecause","links":[{"label":"PR","url":"https://example.com/pr/1"},{"handoff":"h1"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	it := todoOf(t, w.Body.Bytes())
	if it.State != TodoOpen || it.Waiting == nil || it.Waiting.ID != "desk/w2:p1" || it.Waiting.Name != "home" || !strings.HasPrefix(it.ID, "t") {
		t.Fatalf("item = %+v", it)
	}
	// An identical add is not a second item.
	w = do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"may the worker delete the old `+"`key`"+`?"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("dup = %d %s", w.Code, w.Body)
	}
	// --no-wait.
	w = do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"someday","kind":"followup","no_wait":true}`)
	if it := todoOf(t, w.Body.Bytes()); it.Waiting != nil || it.Kind != "followup" {
		t.Fatalf("no_wait = %+v", it)
	}
	w = do(s, "GET", "/api/todos", agentHdr("w2:p1"), "")
	var l struct {
		Todos []Todo `json:"todos"`
		Open  int    `json:"open"`
	}
	json.Unmarshal(w.Body.Bytes(), &l)
	if w.Code != 200 || len(l.Todos) != 2 || l.Open != 2 {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/todos/"+it.ID, agentHdr("w2:p1"), ""); w.Code != 200 {
		t.Fatalf("get = %d", w.Code)
	}
	if w := do(s, "GET", "/api/todos/tnope", userHdr, ""); w.Code != 404 {
		t.Fatalf("get missing = %d", w.Code)
	}
}

func TestTodoCoordinatorAddsForAnother(t *testing.T) {
	s, _, _ := todoServer(t)
	w := do(s, "POST", "/api/todos", agentHdr("w1:p1"), `{"question":"run aws login","kind":"action"}`)
	if it := todoOf(t, w.Body.Bytes()); w.Code != 201 || it.Waiting != nil {
		t.Fatalf("coordinator default = %d %+v", w.Code, it)
	}
	w = do(s, "POST", "/api/todos", agentHdr("w1:p1"), `{"question":"close it?","waiting":"desk/w2:p1"}`)
	if it := todoOf(t, w.Body.Bytes()); it.Waiting == nil || it.Waiting.Name != "home" {
		t.Fatalf("waiting = %+v", it)
	}
}

func TestTodoValidation(t *testing.T) {
	s, _, _ := todoServer(t)
	for _, body := range []string{
		`{"question":"  "}`,
		`{"question":"q","kind":"nope"}`,
		`{"question":"q","links":[{"url":"javascript:alert(1)"}]}`,
		`{"question":"q","links":[{"url":"ftp://x/y"}]}`,
		`{"question":"q","links":[{"label":"empty"}]}`,
		`{"question":"q","waiting":"not an id"}`,
		`{"question":"q","context":"` + strings.Repeat("x", 20<<10) + `"}`,
	} {
		if w := do(s, "POST", "/api/todos", userHdr, body); w.Code != 400 {
			t.Errorf("%.60s = %d %s, want 400", body, w.Code, w.Body)
		}
	}
	w := do(s, "POST", "/api/todos", userHdr, `{"question":"line one\nline two\u0007 bell"}`)
	if it := todoOf(t, w.Body.Bytes()); it.Question != "line one line two bell" {
		t.Fatalf("question = %q", it.Question)
	}
}

func TestTodoRoles(t *testing.T) {
	s, _, _ := todoServer(t)
	it := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"q"}`).Body.Bytes())
	// A plain agent (and an agent with no pane) can neither answer, finish nor drop.
	for _, hdr := range []map[string]string{agentHdr("w2:p1"), agentHdr("")} {
		for _, p := range []string{"/answer", "/done", "/drop"} {
			w := do(s, "POST", "/api/todos/"+it.ID+p, hdr, `{"text":"yes"}`)
			if w.Code != 403 || !strings.Contains(w.Body.String(), "coordinator_only") {
				t.Errorf("agent %s = %d %s", p, w.Code, w.Body)
			}
		}
	}
	// Nor can an unauthenticated or wrong-token caller.
	if w := do(s, "POST", "/api/todos/"+it.ID+"/done", map[string]string{HeaderAgentToken: "wrong"}, `{}`); w.Code != 401 {
		t.Errorf("wrong token = %d", w.Code)
	}
	if got, _ := s.todos.get(it.ID); got.State != TodoOpen {
		t.Fatalf("state = %s", got.State)
	}
	// The coordinator can.
	if w := do(s, "POST", "/api/todos/"+it.ID+"/done", agentHdr("w1:p1"), `{}`); w.Code != 200 {
		t.Fatalf("coordinator done = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/todos/"+it.ID+"/done", agentHdr("w1:p1"), `{}`); w.Code != 409 {
		t.Fatalf("done twice = %d", w.Code)
	}
	if w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"late"}`); w.Code != 409 {
		t.Fatalf("answer after done = %d", w.Code)
	}
}

func waitSent(t *testing.T, sent *[]string, mu *sync.Mutex, n int) []string {
	t.Helper()
	for i := 0; i < 200; i++ {
		mu.Lock()
		if len(*sent) >= n {
			out := append([]string(nil), *sent...)
			mu.Unlock()
			return out
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no notice sent")
	return nil
}

func TestTodoUserAnswerNotifiesCoordinator(t *testing.T) {
	s, sent, mu := todoServer(t)
	it := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"SECRET-QUESTION?"}`).Body.Bytes())
	w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"SECRET-ANSWER yes"}`)
	if w.Code != 200 {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
	got := todoOf(t, w.Body.Bytes())
	if got.State != TodoAnswered || got.Answer.Via != "app" || got.Answer.By != "me@example.com" || got.Answer.Text != "SECRET-ANSWER yes" {
		t.Fatalf("answered = %+v", got)
	}
	msgs := waitSent(t, sent, mu, 1)
	if !strings.HasPrefix(msgs[0], "desk/w1:p1: Comebye (hub notice)") || !strings.Contains(msgs[0], it.ID) {
		t.Fatalf("notice = %q", msgs[0])
	}
	if strings.Contains(msgs[0], "SECRET") {
		t.Fatalf("the notice carries item text: %q", msgs[0])
	}
	// Sent once; the item stays answered until the coordinator closes it.
	time.Sleep(50 * time.Millisecond)
	s.deliverTodoNotices(s.fleet.Agents())
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := len(*sent)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	if g, _ := s.todos.get(it.ID); g.State != TodoAnswered || g.Notice.State != todoNoticeSent {
		t.Fatalf("after notice = %+v %+v", g, g.Notice)
	}
	// Answered items still show in the default list.
	var l struct {
		Todos    []Todo `json:"todos"`
		Answered int    `json:"answered"`
	}
	json.Unmarshal(do(s, "GET", "/api/todos", userHdr, "").Body.Bytes(), &l)
	if len(l.Todos) != 1 || l.Answered != 1 {
		t.Fatalf("list = %+v", l)
	}
}

func TestTodoRelayedAnswerSendsNoNotice(t *testing.T) {
	s, sent, mu := todoServer(t)
	it := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"q"}`).Body.Bytes())
	w := do(s, "POST", "/api/todos/"+it.ID+"/answer", agentHdr("w1:p1"), `{"text":"the user said no"}`)
	got := todoOf(t, w.Body.Bytes())
	if w.Code != 200 || got.Answer.Via != "relay" || got.Notice != nil || !strings.Contains(got.Answer.By, "coordinator") {
		t.Fatalf("relay = %d %+v", w.Code, got)
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*sent) != 0 {
		t.Fatalf("notices = %v", *sent)
	}
}

func TestTodoNoticeWaitsForCoordinatorAndExpires(t *testing.T) {
	s, sent, mu := todoServer(t)
	// No coordinator: the notice stays pending.
	s.recordRole("desk/w1:p1", "coordinator", "", "me")
	it := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"q"}`).Body.Bytes())
	do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok"}`)
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	if len(*sent) != 0 {
		t.Fatalf("sent without a coordinator: %v", *sent)
	}
	mu.Unlock()
	if g, _ := s.todos.get(it.ID); g.Notice.State != todoNoticePending {
		t.Fatalf("notice = %+v", g.Notice)
	}
	// Past the TTL it expires, and the item is still answered (listed).
	s.todos.mutate(it.ID, func(i *Todo) error { i.Notice.At = time.Now().Add(-3 * time.Hour); return nil })
	s.deliverTodoNotices(s.fleet.Agents())
	if g, _ := s.todos.get(it.ID); g.Notice.State != todoNoticeExpired || g.State != TodoAnswered {
		t.Fatalf("expired = %+v", g)
	}
}

func TestTodoDropAndStates(t *testing.T) {
	s, _, _ := todoServer(t)
	a := todoOf(t, do(s, "POST", "/api/todos", userHdr, `{"question":"a"}`).Body.Bytes())
	b := todoOf(t, do(s, "POST", "/api/todos", userHdr, `{"question":"b"}`).Body.Bytes())
	w := do(s, "POST", "/api/todos/"+a.ID+"/drop", userHdr, `{"reason":"no longer\nneeded"}`)
	if g := todoOf(t, w.Body.Bytes()); g.State != TodoDropped || g.DropReason != "no longer needed" || g.ClosedBy != "me@example.com" {
		t.Fatalf("drop = %+v", g)
	}
	count := func(q string) int {
		var l struct{ Todos []Todo }
		json.Unmarshal(do(s, "GET", "/api/todos"+q, userHdr, "").Body.Bytes(), &l)
		return len(l.Todos)
	}
	if count("") != 1 || count("?state=dropped") != 1 || count("?state=all") != 2 || count("?state=open") != 1 {
		t.Fatal("lists")
	}
	if w := do(s, "GET", "/api/todos?state=bogus", userHdr, ""); w.Code != 400 {
		t.Fatalf("bad state = %d", w.Code)
	}
	_ = b
}

func TestTodoStoreLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todo.json")
	st := newTodoStore(path)
	now := time.Now()
	st.now = func() time.Time { return now }
	for i := 0; i < todoRate; i++ {
		if _, _, err := st.add(Todo{Question: string(rune('a'+i)) + "q", CreatedBy: "x", CreatedByID: "h/p"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.add(Todo{Question: "over", CreatedBy: "x", CreatedByID: "h/p"}); err != errTodoRate {
		t.Fatalf("err = %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, _, err := st.add(Todo{Question: "later", CreatedBy: "x", CreatedByID: "h/p"}); err != nil {
		t.Fatal(err)
	}
	// Persisted and reloaded.
	re, err := openTodoStore(path)
	if err != nil || len(re.list("")) != todoRate+1 {
		t.Fatalf("reload = %v %d", err, len(re.list("")))
	}
	// Settled items go after the retention.
	id := re.list("")[0].ID
	re.close(id, TodoDone, "me", "")
	re.now = func() time.Time { return now.Add(todoRetention + time.Hour) }
	if len(re.list("all")) != todoRate {
		t.Fatalf("not pruned: %d", len(re.list("all")))
	}
	// The cap on active items.
	full := newTodoStore("")
	for i := 0; i < todoMaxActive; i++ {
		full.data.Items = append(full.data.Items, Todo{ID: "t", State: TodoOpen, Question: "q"})
	}
	if _, _, err := full.add(Todo{Question: "one more"}); err != errTodoFull {
		t.Fatalf("err = %v", err)
	}
}
