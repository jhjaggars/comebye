package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// deliverPrompt tests: verified delivery of a first prompt. The fake
// herdr's hook scripts agent.prompt answers the way a real herdr reports
// them — success with the agent's post-wait status, or errors like
// agent_prompt_stalled (text written, no observed activity) and
// agent_name_not_active (launch not promptable yet).

// deliverServer is a server with a fake herdr holding one idle,
// interactive-ready agent named w, and fast delivery pacing.
func deliverServer(t *testing.T) (*Server, *fakeLaunchHerdr) {
	t.Helper()
	fh, client := startFakeLaunchHerdr(t)
	s := New(Config{ReadLines: 100, HostName: "desk"}, client, nil)
	s.briefBackoff = time.Millisecond
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{{Name: "w", PaneID: "w9:p1", WorkspaceID: "w9", Agent: "pi", Status: herdr.StatusIdle, InteractiveReady: true}}
	})
	return s, fh
}

// prompts counts the agent.prompt calls the fake has seen.
func prompts(fh *fakeLaunchHerdr) int {
	n := 0
	for _, m := range fh.methods() {
		if m == "agent.prompt" {
			n++
		}
	}
	return n
}

// promptAnswer is one scripted agent.prompt answer: the error herdr
// returns (nil for success) and, on success, the agent's status after the
// wait.
type promptAnswer struct {
	err    *herdr.Error
	status herdr.AgentStatus
}

// onPrompt scripts agent.prompt: the nth call (1-based) gets answers[n];
// answers[0] is the default for calls without an entry. also, when set,
// runs inside the fake's lock before each answer is built, to change the
// fake's state the way a real agent would (the brief arriving late, say).
func onPrompt(fh *fakeLaunchHerdr, answers map[int]promptAnswer, also func(f *fakeLaunchHerdr)) {
	fh.set(func(f *fakeLaunchHerdr) {
		n := 0
		prev := f.hook // a hook already installed (worktree answers, say) keeps answering the rest
		f.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
			if method != "agent.prompt" {
				if prev != nil {
					return prev(method, p)
				}
				return nil, nil, false
			}
			n++
			if also != nil {
				also(f)
			}
			ans, ok := answers[n]
			if !ok {
				ans = answers[0]
			}
			if ans.err != nil {
				return nil, ans.err, true
			}
			f.agents[0].Status = ans.status
			return map[string]any{"type": "agent_prompted", "agent": f.agents[0]}, nil, true
		}
	})
}

// Send before ready, then ready: herdr refuses the first send with
// agent_name_not_active (the launch had not activated yet); the brief is
// waited for and resent, and goes through.
func TestDeliverPromptSendBeforeReadyThenReady(t *testing.T) {
	s, fh := deliverServer(t)
	notActive := &herdr.Error{Code: "agent_name_not_active", Message: "agent w is not an active named agent"}
	onPrompt(fh, map[int]promptAnswer{
		1: {err: notActive},
		2: {status: herdr.StatusWorking},
	}, nil)

	if err := s.deliverPrompt(context.Background(), "w", "You are a worker on proj.\nFix the thing."); err != nil {
		t.Fatalf("deliverPrompt: %v", err)
	}
	if n := prompts(fh); n != 2 {
		t.Fatalf("agent.prompt calls = %d, want 2 (refused once, resent once)", n)
	}
}

// Silent drop then retry: herdr reports the send (agent_prompt_stalled —
// the text was written but the agent never reacted, the Oct 29 race); the
// resend goes through and the agent takes it.
func TestDeliverPromptRetriesSilentDrop(t *testing.T) {
	s, fh := deliverServer(t)
	stalled := &herdr.Error{Code: "agent_prompt_stalled", Message: "no working or blocked activity within 5s"}
	onPrompt(fh, map[int]promptAnswer{
		1: {err: stalled},
		2: {status: herdr.StatusWorking},
	}, nil)

	if err := s.deliverPrompt(context.Background(), "w", "You are a worker on proj.\nFix the thing."); err != nil {
		t.Fatalf("deliverPrompt: %v", err)
	}
	if n := prompts(fh); n != 2 {
		t.Fatalf("agent.prompt calls = %d, want 2 (dropped once, resent once)", n)
	}
}

// Late arrival is not duplicated: the wait says stalled, but the agent
// picked the brief up anyway (its status changed, or the prompt reached
// its session transcript) — the retry must not paste it twice.
func TestDeliverPromptLateArrivalNotDuplicated(t *testing.T) {
	brief := "You are a worker on proj.\nFix the thing."
	stalled := &herdr.Error{Code: "agent_prompt_stalled", Message: "no working or blocked activity within 5s"}

	// The agent went working right after the stalled answer.
	t.Run("status", func(t *testing.T) {
		s, fh := deliverServer(t)
		onPrompt(fh, map[int]promptAnswer{1: {err: stalled}}, func(f *fakeLaunchHerdr) {
			// The brief arrives late: the agent starts working even
			// though herdr's answer said it never reacted.
			f.agents[0].Status = herdr.StatusWorking
		})
		if err := s.deliverPrompt(context.Background(), "w", brief); err != nil {
			t.Fatalf("deliverPrompt: %v", err)
		}
		if n := prompts(fh); n != 1 {
			t.Fatalf("agent.prompt calls = %d, want 1 (the late arrival must not be resent)", n)
		}
	})

	// The prompt reached the agent's session transcript (herdr has not
	// seen the turn yet, but the message is in the file).
	t.Run("transcript", func(t *testing.T) {
		s, fh := deliverServer(t)
		sess := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(sess, []byte(`{"type":"user","text":"`+brief+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		fh.set(func(f *fakeLaunchHerdr) {
			f.agents[0].AgentSession = &herdr.AgentSessionInfo{Source: "herdr:pi", Agent: "pi", Kind: "path", Value: sess}
		})
		onPrompt(fh, map[int]promptAnswer{1: {err: stalled}}, nil)
		if err := s.deliverPrompt(context.Background(), "w", brief); err != nil {
			t.Fatalf("deliverPrompt: %v", err)
		}
		if n := prompts(fh); n != 1 {
			t.Fatalf("agent.prompt calls = %d, want 1 (the late arrival must not be resent)", n)
		}
	})
}

// Bounded failure: the agent never takes the brief; delivery gives up
// after the configured number of tries, and the handoff records a
// running worker with a clear error on its ledger entry (what the Oct 29
// incident lacked), in the worker result and in comebye ledger's answer.
func TestDeliverPromptBoundedFailureRecorded(t *testing.T) {
	s, fh := deliverServer(t)
	stalled := &herdr.Error{Code: "agent_prompt_stalled", Message: "no working or blocked activity within 5s"}
	onPrompt(fh, map[int]promptAnswer{0: {err: stalled}}, nil) // every send stalls

	err := s.deliverPrompt(context.Background(), "w", "You are a worker on proj.\nFix the thing.")
	if err == nil || !strings.Contains(err.Error(), "after 3 tries") || !strings.Contains(err.Error(), "agent_prompt_stalled") {
		t.Fatalf("deliverPrompt error = %v, want bounded failure naming the tries and last error", err)
	}
	if n := prompts(fh); n != 3 {
		t.Fatalf("agent.prompt calls = %d, want 3 (bounded)", n)
	}

	// Through a handoff: the entry stays running (the agent did start and
	// can be rescued with comebye prompt) but carries the error.
	root, repo, wtRoot := workerTree(t)
	fh2, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh2.set(func(f *fakeLaunchHerdr) {
		f.hook = worktreeHook(t, &wts, nil)
	})
	s2 := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot, StateDir: t.TempDir()}, client, nil)
	s2.findExec = func(name string) bool { return name == "pi" }
	s2.briefBackoff = time.Millisecond
	onPrompt(fh2, map[int]promptAnswer{0: {err: stalled}}, nil)

	w := do(s2, "POST", "/api/handoffs", map[string]string{"Tailscale-User-Login": "me@example.com"},
		`{"repo":"`+repo+`","branch":"feat/x","kind":"pi","brief":"Add the thing."}`)
	var r struct {
		Entry  ledger.Entry `json:"entry"`
		Worker workerResult `json:"worker"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || w.Code != 200 {
		t.Fatalf("handoff = %d %s", w.Code, w.Body)
	}
	if r.Entry.Status != ledger.Running {
		t.Fatalf("entry status = %s, want running (the agent started)", r.Entry.Status)
	}
	for _, want := range []string{"brief was not delivered", "after 3 tries"} {
		if !strings.Contains(r.Entry.Error, want) || !strings.Contains(r.Worker.PromptError, want) {
			t.Fatalf("entry error %q / prompt_error %q lack %q", r.Entry.Error, r.Worker.PromptError, want)
		}
	}
	// And comebye ledger's answer carries it.
	w = do(s2, "GET", "/api/ledger", nil, "")
	if !strings.Contains(w.Body.String(), "brief was not delivered") {
		t.Fatalf("ledger answer hides the error: %s", w.Body)
	}
}

// A blocked agent (a dialog is open) is not retried: resending cannot
// help, and the error says what happened.
func TestDeliverPromptBlockedFailsFast(t *testing.T) {
	s, fh := deliverServer(t)
	onPrompt(fh, map[int]promptAnswer{
		0: {err: &herdr.Error{Code: "agent_blocked", Message: "the agent is waiting at a dialog"}},
	}, nil)
	err := s.deliverPrompt(context.Background(), "w", "You are a worker on proj.\nFix the thing.")
	if err == nil || !strings.Contains(err.Error(), "agent_blocked") {
		t.Fatalf("deliverPrompt error = %v, want agent_blocked", err)
	}
	if n := prompts(fh); n != 1 {
		t.Fatalf("agent.prompt calls = %d, want 1 (no retries against a dialog)", n)
	}
}

func TestPromptMarker(t *testing.T) {
	for _, c := range []struct {
		text, want string
	}{
		{"You are a worker agent on proj, handed this task by laharl/w21:p1.\n\n## Your task\n\nDo it.", "You are a worker agent on proj, handed this task"},
		{"first line\nsecond line", "first line"},
		{"\n\n  trimmed  \nrest", "trimmed"},
		{`the quote " cuts the marker here`, "the quote"},
		{"short\nx", ""}, // first line too short
		{"ab\ncdefghijklmnopqrstuvwxyz", "cdefghijklmnopqrstuvwxyz"}, // falls through to a usable line
		{strings.Repeat("y", 100), strings.Repeat("y", 48)},          // capped
	} {
		if got := promptMarker(c.text); got != c.want {
			t.Errorf("promptMarker(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
