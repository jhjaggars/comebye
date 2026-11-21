package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func TestLastActivePrefersSessionMtime(t *testing.T) {
	f := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(f, want, want); err != nil {
		t.Fatal(err)
	}
	a := &herdr.AgentInfo{AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: f}}
	got := lastActive(a, time.Now())
	if got == nil || !got.Equal(want) {
		t.Fatalf("got %v, want session mtime %v", got, want)
	}
}

func TestLastActiveFallsBackToStatusSince(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	a := &herdr.AgentInfo{AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: "/nonexistent"}}
	if got := lastActive(a, since); got == nil || !got.Equal(since) {
		t.Fatalf("got %v, want %v", got, since)
	}
	if got := lastActive(&herdr.AgentInfo{}, time.Time{}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// A resync while the pane set changes must not hang: the stream's loop calls
// onResync, which resubscribes with the new set, which restarts the stream.
// The restart used to wait for that same loop to finish (risk R1), so the
// stream never came back and Run never returned.
func TestResyncWithChangedPanesDoesNotHang(t *testing.T) {
	fh, client := startFakeHerdr(t)
	fh.set(herdr.AgentInfo{Name: "a", PaneID: "wS:p1", Status: herdr.StatusWorking, CWD: "/x/a"})
	m := New(client, nil)
	m.SetDebounce(time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- m.Run(ctx) }()

	waitFor := func(what string, ok func([]int) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if ok(fh.subscribeSizes()) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: subscribes = %v", what, fh.subscribeSizes())
	}
	base := len(herdr.GlobalSubscriptions())
	waitFor("first subscribe", func(s []int) bool { return len(s) > 0 && s[0] == base+1 })

	// A second pane appears; the fake's hang-up makes the stream resync,
	// and the resync must reconnect with both panes subscribed.
	fh.set(
		herdr.AgentInfo{Name: "a", PaneID: "wS:p1", Status: herdr.StatusWorking, CWD: "/x/a"},
		herdr.AgentInfo{Name: "b", PaneID: "wS:p2", Status: herdr.StatusWorking, CWD: "/x/b"},
	)
	waitFor("subscribe with the new pane", func(s []int) bool { return len(s) > 0 && s[len(s)-1] == base+2 })

	// And the pane set shrinks again: a second restart from inside the loop.
	fh.set(herdr.AgentInfo{Name: "a", PaneID: "wS:p1", Status: herdr.StatusWorking, CWD: "/x/a"})
	waitFor("subscribe after the pane closed", func(s []int) bool {
		for i := 1; i < len(s); i++ {
			if s[i-1] == base+2 && s[i] == base+1 {
				return true
			}
		}
		return false
	})

	cancel()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel: the stream is stuck")
	}
}
