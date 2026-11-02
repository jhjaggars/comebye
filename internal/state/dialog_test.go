package state

import (
	"context"
	"os"
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
