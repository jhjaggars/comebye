package state

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// herdr reports Pi's "Trust project folder?" startup prompt as idle; the
// screen says otherwise, so the list shows the agent as blocked.
func TestIdlePiAtDialogIsBlocked(t *testing.T) {
	screen, err := os.ReadFile("../dialog/testdata/pi_trust_initial.txt")
	if err != nil {
		t.Fatal(err)
	}
	fh, client := startFakeHerdr(t)
	fh.screen = string(screen)
	fh.set(
		herdr.AgentInfo{Name: "pi1", Agent: "pi", PaneID: "wS:p1", Status: herdr.StatusIdle},
		herdr.AgentInfo{Name: "cl1", Agent: "claude", PaneID: "wS:p2", Status: herdr.StatusIdle},
	)
	m := New(client, nil)
	m.SetHost("h")
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]herdr.AgentStatus{}
	for _, a := range m.Agents() {
		got[a.Name] = a.Status
	}
	if got["pi1"] != herdr.StatusBlocked || got["cl1"] != herdr.StatusIdle {
		t.Fatalf("statuses = %v", got)
	}
	// Once the prompt is gone the agent reads as herdr says: idle.
	fh.mu.Lock()
	fh.screen = " all done\n\n > \n"
	fh.mu.Unlock()
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.Agents()[0].Status; st != herdr.StatusIdle {
		t.Fatalf("after answering: %v", st)
	}
}

// herdr decides "blocked" from text anywhere in the recent screen, so a busy
// agent whose command output contains a dialog phrase is called blocked
// while nobody is asked anything (rollout-v0518, 2026-11-09, during long
// gh/ssh/podman commands). The screen shows no dialog and a running
// spinner: the agent is working.
func TestBlockedWithoutDialogAndWithSpinnerIsWorking(t *testing.T) {
	const longCommand = `● Bash(gh run watch 42669596484 --exit-status)
  ⎿  Run CI (42669596484) · main
     Do you want to proceed? yes, the old release notes said so
     ✓ build in 2m1s

✻ Running… (3m 2s · ↓ 2.1k tokens)

────────────────────────────────────────
 >
────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)
`
	fh, client := startFakeHerdr(t)
	fh.screen = longCommand
	fh.set(herdr.AgentInfo{Name: "rollout", Agent: "claude", PaneID: "wS:p1", Status: herdr.StatusBlocked})
	m := New(client, nil)
	m.SetHost("h")
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := m.Agents()[0]
	if a.Status != herdr.StatusWorking || !strings.Contains(a.StatusNote, "running: Running") {
		t.Fatalf("status = %v, note = %q; want working with a note", a.Status, a.StatusNote)
	}

	// A real permission prompt stays blocked, spinner or not.
	prompt, err := os.ReadFile("../dialog/testdata/pi_trust_initial.txt")
	if err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	fh.screen = string(prompt) + "\n⠋ Working... (esc to cancel)\n"
	fh.mu.Unlock()
	fh.set(herdr.AgentInfo{Name: "pi1", Agent: "pi", PaneID: "wS:p2", Status: herdr.StatusBlocked})
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.Agents()[0].Status; st != herdr.StatusBlocked || m.Agents()[0].StatusNote != "" {
		t.Fatalf("with a dialog: %v %q", st, m.Agents()[0].StatusNote)
	}

	// Blocked, nothing recognised and no spinner: it may be a dialog this
	// app does not know, so it stays blocked.
	fh.mu.Lock()
	fh.screen = "Some unknown prompt\n[a] accept  [r] reject\n"
	fh.mu.Unlock()
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.Agents()[0].Status; st != herdr.StatusBlocked {
		t.Fatalf("unknown dialog: %v", st)
	}
}

// The status event's fast path does not apply "blocked": it would notify
// before the screen check has run.
func TestBlockedEventWaitsForTheScreenCheck(t *testing.T) {
	fh, client := startFakeHerdr(t)
	fh.screen = "✻ Running… (1m 2s)\n\n > \n"
	fh.set(herdr.AgentInfo{Name: "rollout", Agent: "claude", PaneID: "wS:p1", Status: herdr.StatusWorking})
	m := New(client, nil)
	m.SetHost("h")
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.onEvent(herdr.PushEvent{Event: "pane.agent_status_changed", Data: []byte(`{"pane_id":"wS:p1","agent_status":"blocked"}`)})
	if st := m.Agents()[0].Status; st != herdr.StatusWorking {
		t.Fatalf("a blocked event flipped the cached status to %v before the check", st)
	}
}
