package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
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

// The testdata files are `herdr pane process-info --pane <id>` output captured
// from live panes (a claude launched with flags and a prompt, and a pi).
func TestParseProcessInfo(t *testing.T) {
	read := func(name string) ProcessInfo {
		t.Helper()
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatal(err)
		}
		pi, err := parseProcessInfo(env.Result)
		if err != nil {
			t.Fatal(err)
		}
		return pi
	}

	pi := read("process_info_claude.json")
	if pi.PaneID != "wCM:p1" || len(pi.Processes) != 3 {
		t.Fatalf("claude pane: %+v", pi)
	}
	p, ok := pi.Find("claude")
	if !ok || p.PID != 108182 || !strings.HasSuffix(p.CWD, "/scratchpad/live") {
		t.Fatalf("Find(claude) = %+v, %v", p, ok)
	}
	want := []string{"claude", "--model", "claude-opus-5-5", "--permission-mode", "auto", "--append-system-prompt", "be", "brief", "--", "say", "hi"}
	if !reflect.DeepEqual(p.Argv, want) {
		t.Errorf("argv = %q", p.Argv)
	}
	if _, ok := pi.Find("pi"); ok {
		t.Error("Find(pi) matched in a claude pane")
	}

	pp := read("process_info_pi.json")
	p, ok = pp.Find("pi")
	if !ok || p.PID != 108456 || !reflect.DeepEqual(p.Argv, []string{"pi"}) {
		t.Fatalf("Find(pi) = %+v, %v", p, ok)
	}

	if _, err := parseProcessInfo(json.RawMessage(`[1]`)); err == nil {
		t.Error("parsed garbage")
	}
}
