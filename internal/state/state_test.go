package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
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
