package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// Regression: `comebye prompt` to an agent sitting at a permission prompt
// typed the text (and Enter) into the dialog. The guard now looks at the
// pane on the owning host right before sending.

func showScreen(fh *fakeLaunchHerdr, pane, screen string) {
	fh.set(func(f *fakeLaunchHerdr) {
		if f.screens == nil {
			f.screens = map[string]string{}
		}
		f.screens[pane] = screen
	})
}

func TestPromptRefusedWhileDialogOpen(t *testing.T) {
	for name, screen := range map[string]string{
		"permission prompt": claudePermScreen,
		"pi gate":           piGateScreen,
		"multi-select":      claudeMultiScreen,
	} {
		s, fh, _ := coordServer(t)
		// The cached status says idle: the dialog opened after the last poll.
		showScreen(fh, "w2:p1", screen)
		w := do(s, "POST", "/api/agents/home/prompt", agentHdr("w1:p1"), `{"text":"hello"}`)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "dialog_open") || !strings.Contains(w.Body.String(), "--queue") {
			t.Errorf("%s: prompt = %d %s, want 409 dialog_open", name, w.Code, w.Body)
		}
		if fh.call("agent.prompt") != nil {
			t.Errorf("%s: text was sent to the agent", name)
		}
		// Through the host-qualified route too (what the CLI uses).
		w = do(s, "POST", "/api/hosts/desk/agents/w2:p1/prompt", agentHdr("w1:p1"), `{"text":"hello"}`)
		if w.Code != http.StatusConflict || fh.call("agent.prompt") != nil {
			t.Errorf("%s: host route = %d, prompt sent = %v", name, w.Code, fh.call("agent.prompt") != nil)
		}
	}
}

func TestPromptByPaneNotInCacheStillGuarded(t *testing.T) {
	s, fh, _ := coordServer(t)
	// An agent herdr knows but the poll has not listed yet.
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "fresh", PaneID: "w7:p1", Agent: "claude", Status: herdr.StatusIdle})
	})
	showScreen(fh, "w7:p1", claudePermScreen)
	w := do(s, "POST", "/api/agents/w7:p1/prompt", agentHdr("w1:p1"), `{"text":"hello"}`)
	if w.Code != http.StatusConflict || fh.call("agent.prompt") != nil {
		t.Fatalf("uncached target: %d %s, sent=%v", w.Code, w.Body, fh.call("agent.prompt") != nil)
	}
}

func TestPromptGoesThroughWhenClear(t *testing.T) {
	s, fh, _ := coordServer(t)
	w := do(s, "POST", "/api/agents/home/prompt", agentHdr("w1:p1"), `{"text":"hello"}`)
	if w.Code != 200 || fh.call("agent.prompt") == nil {
		t.Fatalf("clear prompt = %d %s", w.Code, w.Body)
	}
}

// The queue drainer must re-check at delivery time: the agent reads idle
// (the status lags) but a dialog is on screen, so nothing is typed and the
// item stays queued; once the dialog is gone it is delivered.
func TestQueueDrainerRechecksPane(t *testing.T) {
	s, fh, _ := coordServer(t)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s.pq.now = func() time.Time { return now }
	showScreen(fh, "w2:p1", claudePermScreen)
	body := `{"agent":"desk/w2:p1","name":"home","text":"please rebase"}`
	if w := do(s, "POST", "/api/queue", agentHdr("w1:p1"), body); w.Code != 202 {
		t.Fatalf("queue = %d %s", w.Code, w.Body)
	}
	waitFor(t, func() bool { l := s.pq.list("", false); return len(l) == 1 && l[0].Tries >= 1 })
	if fh.call("agent.prompt") != nil {
		t.Fatal("drainer typed into an open dialog")
	}
	showScreen(fh, "w2:p1", "● Done.\n\n❯ \n")
	now = now.Add(queueRetry + time.Second)
	s.deliverQueue([]herdr.AgentInfo{home(herdr.StatusIdle)})
	waitFor(t, func() bool { return len(s.pq.list("", false)) == 0 })
	p := fh.call("agent.prompt")
	if p == nil || !strings.Contains(p.Params["text"].(string), "please rebase") {
		t.Fatalf("not delivered after the dialog closed: %v", p)
	}
}

// A launch's first prompt or a handoff brief must not be typed over a
// dialog on a fresh agent either (a trust-this-folder prompt, say).
func TestDeliverPromptRefusesDialog(t *testing.T) {
	s, fh := deliverServer(t)
	showScreen(fh, "w9:p1", claudePermScreen)
	err := s.deliverPrompt(context.Background(), "w", "do the thing")
	var he *herdr.Error
	if !errors.As(err, &he) || he.Code != "dialog_open" {
		t.Fatalf("deliverPrompt = %v, want dialog_open", err)
	}
	if prompts(fh) != 0 {
		t.Fatal("brief typed into a dialog")
	}
}
