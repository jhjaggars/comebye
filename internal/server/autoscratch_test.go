package server

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// scriptedSession makes the marked session answer each prompt the hub sends
// it with reply(prompt): its latest message changes once per prompt.
func scriptedSession(s *Server, fh *fakeLaunchHerdr, reply func(prompt string) string) {
	s.latest = func(ctx context.Context, host, pane string) *transcript.Latest {
		n := promptCount(fh)
		if n == 0 {
			return &transcript.Latest{Text: "Done: the tests pass.", Time: "0"}
		}
		fh.mu.Lock()
		var p string
		for _, c := range fh.calls {
			if c.Method == "agent.prompt" {
				p, _ = c.Params["text"].(string)
			}
		}
		fh.mu.Unlock()
		return &transcript.Latest{Text: reply(p), Time: strconv.Itoa(n)}
	}
}

// scratchFile is a file that exists, so its path item stays open.
func scratchFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scratch.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func goAheads(fh *fakeLaunchHerdr) []string {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	var out []string
	for _, c := range fh.calls {
		if p, _ := c.Params["text"].(string); c.Method == "agent.prompt" && strings.HasPrefix(p, "Go ahead with the cleanup") {
			out = append(out, p)
		}
	}
	return out
}

func TestIsScratch(t *testing.T) {
	for _, c := range []struct {
		item ledger.AskItem
		want bool
	}{
		{ledger.AskItem{Type: "path", Name: "/tmp/fetch-cache", Text: "mine, scratch"}, true},
		{ledger.AskItem{Type: "path", Name: "/home/u/notes.md.bak", Text: "my pre-edit backup"}, true},
		{ledger.AskItem{Type: "resource", Kind: "pid", Name: "4242", Text: "the dev server I started"}, true},
		{ledger.AskItem{Type: "resource", Kind: "herdr-session", Name: "probe", Text: "throwaway session I opened"}, true},
		{ledger.AskItem{Type: "cleanup", Text: "delete /tmp/x, my scratch"}, true},
		{ledger.AskItem{Type: "resource", Kind: "pid", Name: "1", Text: "stop it", Was: "cleanup"}, true},
		// Not its own, or kept, or shared work.
		{ledger.AskItem{Type: "path", Name: "/tmp/cache", Text: "left behind"}, false},
		{ledger.AskItem{Type: "resource", Kind: "k3d-cluster", Name: "dev", Text: "mine; the user asked to keep it"}, false},
		{ledger.AskItem{Type: "resource", Kind: "container", Name: "db", Text: "started by another session"}, false},
		{ledger.AskItem{Type: "path", Name: "/home/u/.config/gh/hosts.yml", Text: "my credentials for the test account"}, false},
		{ledger.AskItem{Type: "cleanup", Text: "close my PR"}, false},
		{ledger.AskItem{Type: "cleanup", Text: "delete branch feat/x"}, false},
		{ledger.AskItem{Type: "cleanup", Text: "remove /tmp/x, don't delete /tmp/y"}, false},
		{ledger.AskItem{Type: "other", Text: "my scratch loop"}, false},
	} {
		if got := isScratch(c.item); got != c.want {
			t.Errorf("%+v = %v; want %v", c.item, got, c.want)
		}
	}
}

// The whole way: a LEFT answer with only its own scratch gets the go-ahead
// by itself, once; the session cleans up, answers NOTHING LEFT when asked
// again, and the hub removes it.
func TestScratchOnlyAnswerIsCleanedUpAndRemoved(t *testing.T) {
	s, r, e := workerFix(t)
	fh := lastHerdr
	r.landCherryPick(t)
	at := time.Now().UTC().Add(-time.Minute)
	if _, err := s.ledger.SetMark(ledger.Mark{AgentID: arAgent, Host: "desk", PaneID: "w2:p1", Name: "home", Worktree: r.wt, By: "desk/w1:p1", Reason: "landed", At: at,
		Ask: &ledger.Ask{State: ledger.AskSafe, At: at}}); err != nil {
		t.Fatal(err)
	}
	tmp := scratchFile(t)
	scriptedSession(s, fh, func(p string) string {
		switch {
		case strings.HasPrefix(p, "Go ahead with the cleanup"):
			return "Deleted it and closed the session."
		case len(goAheads(fh)) > 0:
			return "NOTHING LEFT"
		}
		return "LEFT\n- path " + tmp + " mine, scratch copy of the fixtures\n- resource herdr-session probe a throwaway session I opened"
	})
	mark := func() ledger.Mark { return *entryOf(t, s).Mark }
	// Asked again (a person's re-ask), it lists its scratch.
	if err := s.reask(mark(), false, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(goAheads(fh)) == 1 })
	p := goAheads(fh)[0]
	for _, want := range []string{"Do exactly this, nothing else", "path " + tmp + ": mine, scratch copy", "herdr-session probe: a throwaway session I opened", "Do not commit, push, merge or touch anything that is not listed"} {
		if !strings.Contains(p, want) {
			t.Errorf("go-ahead lacks %q:\n%s", want, p)
		}
	}
	waitFor(t, func() bool {
		a := mark().Ask
		return a.State == ledger.AskSafe && a.ReaskedAt == nil && a.CleaningAt == nil
	})
	if a := mark().Ask; a.Reasks != 1 {
		t.Errorf("reasks = %d; the ask after the go-ahead counts", a.Reasks)
	}
	if n := len(goAheads(fh)); n != 1 {
		t.Fatalf("go-aheads = %d", n)
	}
	if got := s.autoRemoveStep(context.Background(), time.Now()); len(got) != 1 || got[0] != arAgent {
		t.Fatalf("removed = %v", got)
	}
	if g, _ := s.ledger.Get(e.ID); g.Status != ledger.Removed {
		t.Fatalf("status = %s", g.Status)
	}
}

// Mixed: the go-ahead names the scratch only; the decision, the PR clean-up
// and the cluster the user asked to keep stay on the answer, and the next
// answer listing only those gets no go-ahead.
func TestMixedAnswerGetsAGoAheadForTheScratchOnly(t *testing.T) {
	s, fh, _ := coordServer(t)
	open, _, _ := s.todos.add(Todo{Question: "which key?", Kind: "decision", State: TodoOpen, CreatedBy: "w"})
	tmp := scratchFile(t)
	kept := "- resource k3d-cluster dev mine, the user asked to keep it\n- decision " + open.ID + " waiting on the user\n- cleanup close my PR"
	scriptedSession(s, fh, func(p string) string {
		if strings.HasPrefix(p, "Go ahead with the cleanup") {
			return "Deleted it."
		}
		return "LEFT\n" + kept
	})
	markUnsafe(t, s, time.Minute,
		ledger.AskItem{Type: "path", Name: tmp, Text: "my scratch"},
		ledger.AskItem{Type: "resource", Kind: "k3d-cluster", Name: "dev", Text: "mine, the user asked to keep it"},
		ledger.AskItem{Type: "decision", ID: open.ID, Text: "waiting on the user"},
		ledger.AskItem{Type: "cleanup", Text: "close my PR"})
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 1 {
		t.Fatalf("asked = %v", got)
	}
	waitFor(t, func() bool { return len(goAheads(fh)) == 1 })
	p := goAheads(fh)[0]
	if !strings.Contains(p, "1. path "+tmp+": my scratch\nDo not") {
		t.Errorf("go-ahead is not the scratch alone:\n%s", p)
	}
	for _, not := range []string{"k3d", "dev", open.ID, "PR"} {
		if strings.Contains(strings.SplitN(p, "Do not commit", 2)[0], not) {
			t.Errorf("go-ahead names %q:\n%s", not, p)
		}
	}
	// The ask after the cleanup records what is really left.
	waitFor(t, func() bool {
		a := markOf(t, s, "desk/w2:p1").Ask
		return a.CleaningAt == nil && a.ReaskedAt == nil && len(a.Items) == 3
	})
	a := markOf(t, s, "desk/w2:p1").Ask
	if a.State != ledger.AskUnsafe || a.Items[0].Type != "resource" || a.Items[1].Type != "decision" || a.Items[2].Type != "cleanup" {
		t.Fatalf("answer = %+v", a)
	}
	// No scratch left in it: the loop sends nothing more.
	n := promptCount(fh)
	s.reaskStep(context.Background(), time.Now())
	time.Sleep(50 * time.Millisecond)
	if promptCount(fh) != n || len(goAheads(fh)) != 1 {
		t.Fatalf("prompts %d -> %d", n, promptCount(fh))
	}
}

// A resource the user asked to keep is never cleaned up by the hub, and an
// answer gets the go-ahead once, within reaskMax.
func TestScratchGoAheadLimits(t *testing.T) {
	s, fh, _ := coordServer(t)
	scriptedSession(s, fh, func(string) string { return "Done." })
	markUnsafe(t, s, time.Minute, ledger.AskItem{Type: "resource", Kind: "k3d-cluster", Name: "demo", Text: "mine; keep it, the user is demoing on it"})
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 0 || promptCount(fh) != 0 {
		t.Fatalf("kept resource: asked %v, %d prompts", got, promptCount(fh))
	}
	// Re-asks spent: no go-ahead either.
	m := markUnsafe(t, s, time.Minute, ledger.AskItem{Type: "path", Name: scratchFile(t), Text: "mine"})
	s.ledger.UpdateAsk(m.AgentID, m.At, func(a *ledger.Ask) bool { a.Reasks = s.reaskMax; return true })
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 0 || promptCount(fh) != 0 {
		t.Fatalf("cap spent: asked %v, %d prompts", got, promptCount(fh))
	}
	// Already sent for this answer: not again.
	s.ledger.UpdateAsk(m.AgentID, m.At, func(a *ledger.Ask) bool { a.Reasks = 0; at := a.At; a.AutoCleanFor = &at; return true })
	if got := s.reaskStep(context.Background(), time.Now()); len(got) != 0 || promptCount(fh) != 0 {
		t.Fatalf("sent twice: asked %v, %d prompts", got, promptCount(fh))
	}
}

// A gone session (page-chat-s5, 11-22) whose answer lists only its own
// throwaway processes and its scratch folder, all already gone: the hub
// checks each on the host (the pids are not running, the folder is not
// there), settles the answer, and the sweep's proof removes the session.
func TestGoneSessionsOwnGoneScratchIsSettledAndRemoved(t *testing.T) {
	f := newGoneFix(t, true, "")
	_, tmp := probeRoots(t)
	pidRunning(t, false)
	folder := filepath.Join(tmp, "claude-1000", "-home-u--worktrees-proj-w", "c78d827b", "scratchpad") + "/"
	items := []ledger.AskItem{
		{Type: "resource", Kind: "pid", Name: "1518018", Text: "a throwaway herdr_up.py pty client I started for the live check. It is still running, and I can stop it if told to go ahead."},
		{Type: "resource", Kind: "pid", Name: "1518096", Text: "a herdr --session cb5live client I started for the live check. It is still running, and I can stop it if told to go ahead."},
		{Type: "cleanup", Text: "throwaway files from the live check under `" + folder + "` (state, code, bin, logs). They are scratch only, and I can delete them if told to go ahead."},
	}
	if _, _, err := f.s.ledger.Update(f.e.ID, func(e *ledger.Entry) {
		e.Mark.Ask = &ledger.Ask{State: ledger.AskUnsafe, Reason: "see items", Items: items, At: e.Mark.At, Reasks: 12}
	}); err != nil {
		t.Fatal(err)
	}
	// What a hub on another host asks this one: both pids are dead, the folder absent.
	for _, q := range []string{"kind=pid&pid=1518018", "kind=pid&pid=1518096", "kind=path&path=" + folder} {
		if w := do(f.s, "GET", "/api/probe?"+q, userHdr, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"dead"`) && !strings.Contains(w.Body.String(), `"absent"`) {
			t.Fatalf("probe %s: %d %s", q, w.Code, w.Body)
		}
	}
	// The review checks each item: all settled, and they are its own scratch.
	for _, it := range f.s.marked() {
		ar := f.s.askReviewOf(ctx(), it)
		if !ar.Resolved {
			t.Fatalf("not settled: %+v", ar.Items)
		}
		for _, v := range ar.Items {
			if !isScratch(v.AskItem) {
				t.Errorf("not scratch: %+v", v.AskItem)
			}
		}
	}
	f.setAuto(t, true)
	f.s.sweepLoopStep(ctx(), time.Now())
	if got := f.status(t); got != ledger.Removed {
		t.Fatalf("status = %s, ask %+v", got, f.ask(t))
	}
	// A pid still running keeps such a session.
	g := newGoneFix(t, true, "")
	pidRunning(t, true)
	if _, _, err := g.s.ledger.Update(g.e.ID, func(e *ledger.Entry) {
		e.Mark.Ask = &ledger.Ask{State: ledger.AskUnsafe, Reason: "see items", At: e.Mark.At,
			Items: []ledger.AskItem{{Type: "resource", Kind: "pid", Name: "4242", Text: "a client I started"}}}
	}); err != nil {
		t.Fatal(err)
	}
	g.setAuto(t, true)
	g.s.sweepLoopStep(ctx(), time.Now())
	if got := g.status(t); got != ledger.ReadyToRemove {
		t.Fatalf("a running pid: status = %s", got)
	}
}
