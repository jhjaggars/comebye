package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// blockedRig is a hub whose fake herdr has "home" (w2:p1, pi) blocked at the
// permission gate, with an auto item filed for it.
func blockedRig(t *testing.T, screen string) (*Server, *fakeLaunchHerdr, Todo) {
	t.Helper()
	s, fh, _ := coordServer(t)
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents[1].Status = herdr.StatusBlocked
		f.screens = map[string]string{"w2:p1": screen}
		f.hook = func(method string, params map[string]any) (any, *herdr.Error, bool) {
			if method == "agent.send_keys" {
				return map[string]any{"type": "ok"}, nil, true
			}
			return nil, nil, false
		}
	})
	var home herdr.AgentInfo
	s.manager.Refresh() // debounced: ask once, then watch
	for deadline := time.Now().Add(5 * time.Second); home.Status != herdr.StatusBlocked; time.Sleep(20 * time.Millisecond) {
		for _, a := range s.manager.Agents() {
			if a.PaneID == "w2:p1" {
				home = a
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never blocked: %+v", home)
		}
	}
	s.initAutoTodo(AutoTodoConfig{Blocked: time.Minute})
	since := time.Now().Add(-5 * time.Minute)
	home.StatusSince = &since
	s.autoTodoStep(t.Context(), []herdr.AgentInfo{home}, time.Now())
	items := s.todos.list("")
	if len(items) != 1 || items[0].Dialog == nil {
		t.Fatalf("items = %+v", items)
	}
	return s, fh, items[0]
}

func sentKeys(fh *fakeLaunchHerdr) int {
	n := 0
	for _, c := range fh.calls {
		if c.Method == "agent.send_keys" {
			n++
		}
	}
	return n
}

func TestTodoAnswerDrivesBlockedDialog(t *testing.T) {
	s, fh, it := blockedRig(t, gateScreen)
	if len(it.Options) != 2 || it.Options[0].Label != "Allow once" {
		t.Fatalf("options = %+v", it.Options)
	}
	// The screen is read at answer time and the keys come from the current highlight.
	w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok 1"}`)
	if w.Code != 200 {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
	got := todoOf(t, w.Body.Bytes())
	if got.State != TodoDone || got.Answer == nil || got.Answer.Via != "dialog" || got.Notice != nil {
		t.Fatalf("item = %+v", got)
	}
	if n := sentKeys(fh); n != 1 {
		t.Fatalf("send_keys calls = %d", n)
	}
	// A second tap on a closed item sends nothing.
	if w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok 1"}`); w.Code != http.StatusConflict {
		t.Fatalf("again = %d %s", w.Code, w.Body)
	}
	if n := sentKeys(fh); n != 1 {
		t.Fatalf("send_keys after repeat = %d", n)
	}
}

func TestTodoAnswerDialogGone(t *testing.T) {
	s, fh, it := blockedRig(t, gateScreen)
	fh.set(func(f *fakeLaunchHerdr) { f.screens["w2:p1"] = "● Done.\n\n❯ \n" })
	w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok 1"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_dialog") {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
	if n := sentKeys(fh); n != 0 {
		t.Fatalf("keys were sent to a pane with no dialog: %d", n)
	}
	if got, _ := s.todos.get(it.ID); got.State != TodoDropped || !strings.Contains(got.DropReason, "already gone") {
		t.Fatalf("item = %+v", got)
	}
}

func TestTodoAnswerDialogChanged(t *testing.T) {
	s, fh, it := blockedRig(t, gateScreen)
	fh.set(func(f *fakeLaunchHerdr) {
		f.screens["w2:p1"] = strings.ReplaceAll(gateScreen, "git push origin feature", "git push --force origin main")
	})
	w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok 1"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "dialog_changed") || sentKeys(fh) != 0 {
		t.Fatalf("answer = %d %s sent=%d", w.Code, w.Body, sentKeys(fh))
	}
	if got, _ := s.todos.get(it.ID); got.State != TodoDropped {
		t.Fatalf("item = %+v", got)
	}
}

func TestTodoAnswerRejectsFreeText(t *testing.T) {
	s, fh, it := blockedRig(t, gateScreen)
	for _, txt := range []string{"yes please", "ok 3", "ok 1+2", "ok 0"} {
		w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"`+txt+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%q = %d %s", txt, w.Code, w.Body)
		}
	}
	if sentKeys(fh) != 0 {
		t.Fatal("keys sent")
	}
	if got, _ := s.todos.get(it.ID); got.State != TodoOpen {
		t.Fatalf("item = %+v", got)
	}
}

func TestTodoAnswerAgentGone(t *testing.T) {
	s, fh, it := blockedRig(t, gateScreen)
	fh.set(func(f *fakeLaunchHerdr) { f.agents = f.agents[:1] })
	waitAgents(t, s.manager, 1)
	w := do(s, "POST", "/api/todos/"+it.ID+"/answer", userHdr, `{"text":"ok 1"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "agent_gone") || sentKeys(fh) != 0 {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
}
