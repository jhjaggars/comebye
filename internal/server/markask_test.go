package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// askServer is coordServer with a fast ask and a scripted latest message:
// before until the session is prompted, then reply (when not "").
func askServer(t *testing.T, reply string) (*Server, *fakeLaunchHerdr) {
	t.Helper()
	s, fh, _ := coordServer(t)
	s.askPoll, s.askWait = 10*time.Millisecond, 300*time.Millisecond
	var mu sync.Mutex
	s.latest = func(ctx context.Context, host, pane string) *transcript.Latest {
		mu.Lock()
		defer mu.Unlock()
		if reply != "" && promptCount(fh) > 0 {
			return &transcript.Latest{Text: reply, Time: "2026-10-23T16:00:05Z"}
		}
		return &transcript.Latest{Text: "Done: the tests pass.", Time: "2026-10-23T16:00:00Z"}
	}
	return s, fh
}

func markOf(t *testing.T, s *Server, id string) ledger.Mark {
	t.Helper()
	for _, m := range s.ledger.Marks() {
		if m.AgentID == id {
			return m
		}
	}
	t.Fatalf("no mark on %s", id)
	return ledger.Mark{}
}

func answered(t *testing.T, s *Server, id string) *ledger.Ask {
	t.Helper()
	var a *ledger.Ask
	waitFor(t, func() bool {
		a = markOf(t, s, id).Ask
		return a != nil && a.State != ledger.AskAsking
	})
	return a
}

func reviewOf(t *testing.T, s *Server, id string) removal {
	t.Helper()
	var r struct {
		Removals []removal `json:"removals"`
	}
	w := do(s, "GET", "/api/removals", userHdr, "")
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	for _, it := range r.Removals {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("%s not in the review: %s", id, w.Body)
	return removal{}
}

func TestMarkAsksSafe(t *testing.T) {
	s, fh := askServer(t, "SAFE — nothing pending.")
	w := do(s, "POST", "/api/marks", agentHdr("w1:p1"), `{"agent_id":"desk/w2:p1","reason":"done"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"asking"`) {
		t.Fatalf("mark = %d %s", w.Code, w.Body)
	}
	if !s.quietAgent("desk/w2:p1") {
		t.Error("a session being asked should not notify")
	}
	if a := answered(t, s, "desk/w2:p1"); a.State != ledger.AskSafe || a.Reason != "nothing pending." || a.AskedAt == nil {
		t.Fatalf("ask = %+v", a)
	}
	p := fh.call("agent.prompt").Params
	if p["target"] != "w2:p1" || !strings.Contains(p["text"].(string), "Do not take any other action") {
		t.Fatalf("prompt = %v", p)
	}
	if it := reviewOf(t, s, "desk/w2:p1"); !it.Safe {
		t.Fatalf("review = %+v", it)
	}
}

func TestMarkAsksUnsafe(t *testing.T) {
	s, _ := askServer(t, "**UNSAFE**: I'm watching PR #12 until it merges.")
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if a := answered(t, s, "desk/w2:p1"); a.State != ledger.AskUnsafe || a.Reason != "I'm watching PR #12 until it merges." {
		t.Fatalf("ask = %+v", a)
	}
	it := reviewOf(t, s, "desk/w2:p1")
	if it.Safe || !strings.Contains(strings.Join(it.Warnings, "|"), "the session says UNSAFE: I'm watching PR #12") {
		t.Fatalf("review = %+v", it)
	}
}

func TestMarkAskNoAnswer(t *testing.T) {
	s, fh := askServer(t, "")
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if a := answered(t, s, "desk/w2:p1"); a.State != ledger.AskNoAnswer {
		t.Fatalf("ask = %+v", a)
	}
	if promptCount(fh) != 1 {
		t.Fatalf("prompts = %d", promptCount(fh))
	}
	// No answer is not safe, but it does not block one-tap removal either.
	it := reviewOf(t, s, "desk/w2:p1")
	if !it.Safe || !strings.Contains(strings.Join(it.Notes, "|"), "did not answer") {
		t.Fatalf("review = %+v", it)
	}
}

func TestMarkAskSkipped(t *testing.T) {
	s, fh := askServer(t, "SAFE")
	// --no-ask: no prompt, no answer.
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w2:p1","no_ask":true}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if a := markOf(t, s, "desk/w2:p1").Ask; a != nil {
		t.Fatalf("no_ask asked: %+v", a)
	}
	// A working session is not asked.
	fh.set(func(f *fakeLaunchHerdr) { f.agents[1].Status = herdr.StatusWorking })
	s.manager.Refresh()
	waitFor(t, func() bool {
		a := s.liveAgent("desk/w2:p1")
		return a != nil && a.Status == herdr.StatusWorking
	})
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if a := markOf(t, s, "desk/w2:p1").Ask; a == nil || a.State != ledger.AskNotAsked || a.Reason != "the session was working" {
		t.Fatalf("working = %+v", a)
	}
	time.Sleep(50 * time.Millisecond)
	if promptCount(fh) != 0 {
		t.Fatalf("prompts = %d", promptCount(fh))
	}
}

func TestMarkAskUnmarked(t *testing.T) {
	s, _ := askServer(t, "")
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	at := markOf(t, s, "desk/w2:p1").At
	if w := do(s, "POST", "/api/marks/clear", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	// Marked again without asking: the first ask must not write over it.
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w2:p1","no_ask":true}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	time.Sleep(2 * s.askWait)
	if m := markOf(t, s, "desk/w2:p1"); m.Ask != nil || m.At.Equal(at) {
		t.Fatalf("mark = %+v", m)
	}
}

func TestEndStaleAsks(t *testing.T) {
	s, _ := askServer(t, "")
	at := time.Now().UTC()
	if _, err := s.ledger.SetMark(ledger.Mark{AgentID: "desk/w2:p1", At: at, Ask: &ledger.Ask{State: ledger.AskAsking, At: at}}); err != nil {
		t.Fatal(err)
	}
	s.endStaleAsks()
	if a := markOf(t, s, "desk/w2:p1").Ask; a.State != ledger.AskNoAnswer || !strings.Contains(a.Reason, "restarted") {
		t.Fatalf("ask = %+v", a)
	}
}

func TestParseAnswer(t *testing.T) {
	cases := []struct{ in, state, reason string }{
		{"SAFE", ledger.AskSafe, ""},
		{"safe.", ledger.AskSafe, ""},
		{"**SAFE**\n\nNothing pending.", ledger.AskSafe, "Nothing pending."},
		{"UNSAFE: open PR #9702 I'm babysitting", ledger.AskUnsafe, "open PR #9702 I'm babysitting"},
		{"UNSAFE — 3 unpushed commits", ledger.AskUnsafe, "3 unpushed commits"},
		{"I think it's probably fine.", ledger.AskUnclear, "I think it's probably fine."},
		{"Unsafeish", ledger.AskUnclear, "Unsafeish"},
	}
	for _, c := range cases {
		if st, why := parseAnswer(c.in); st != c.state || why != c.reason {
			t.Errorf("parseAnswer(%q) = %q %q, want %q %q", c.in, st, why, c.state, c.reason)
		}
	}
	if _, why := parseAnswer("UNSAFE " + strings.Repeat("é", 400)); len(why) > maxAskReason+len("…") {
		t.Errorf("reason not clipped: %d bytes", len(why))
	}
}
