package herdr

import (
	"context"
	"errors"
	"testing"
)

func TestValidKey(t *testing.T) {
	for _, k := range []string{"esc", "enter", "tab", "space", "up", "down", "left", "right", "ctrl+c", "1", "4", "9", "y", "n"} {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false, want true", k)
		}
	}
	// Anything that could type free text or escape the dialog keys stays out.
	for _, k := range []string{"", "0", "10", "a", "Y", "N", "yes", "ctrl+x", "alt+y", "shift+tab", "rm -rf /", "\n", "\x1b"} {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true, want false", k)
		}
	}
}

func TestClientWithNoSocketIsNoHerdr(t *testing.T) {
	c := NewClient("")
	if _, err := c.ListAgents(context.Background()); !errors.Is(err, ErrNoHerdr) {
		t.Fatalf("ListAgents = %v", err)
	}
	var he *Error
	if _, err := c.Call(context.Background(), "ping", nil); !errors.As(err, &he) || he.Code != "no_herdr" {
		t.Fatalf("Call = %v", err)
	}
}
