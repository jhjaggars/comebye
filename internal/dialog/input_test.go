package dialog

import (
	"os"
	"strings"
	"testing"
)

// The screens in testdata/input are real captures (herdr pane read
// --source visible, with and without --ansi) of a scratch Claude Code 2.x
// (vim mode on) and a scratch pi, started in a throwaway herdr server; none
// is from a real session.

func inputFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/input/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetectInputFixtures(t *testing.T) {
	cases := []struct {
		file, kind string
		busy       bool
		text       string // substring of Text when busy
	}{
		// Claude Code, with styling.
		{"claude_empty.ansi", "claude", false, ""},
		{"claude_empty2.ansi", "claude", false, ""},
		{"claude_cleared.ansi", "claude", false, ""},
		{"claude_afterpaste.ansi", "claude", false, ""},
		{"claude_typed.ansi", "claude", true, "I think we should consider a way to"},
		{"claude_multiline.ansi", "claude", true, "line one line two"},
		{"claude_paste.ansi", "claude", true, "[Pasted text #1 +29 lines]"},
		{"claude_wrapped.ansi", "claude", true, "word word word"},
		{"claude_bash.ansi", "claude", true, "ls -la"},
		{"claude_slash.ansi", "claude", true, "/mod"},
		{"claude_vimnormal.ansi", "claude", true, "normal mode text"},
		// Claude Code, plain text only (no styling to tell the placeholder).
		{"claude_empty.txt", "claude", false, ""},
		{"claude_cleared.txt", "claude", false, ""},
		{"claude_typed.txt", "claude", true, "I think we should consider a way to"},
		{"claude_multiline.txt", "claude", true, "line one line two"},
		{"claude_paste.txt", "claude", true, "Pasted text"},
		{"claude_bash.txt", "claude", true, "ls -la"},
		// pi, both forms.
		{"pi_empty2.ansi", "pi", false, ""},
		{"pi_empty2.txt", "pi", false, ""},
		{"pi_afterclear.txt", "pi", false, ""},
		{"pi_typed.ansi", "pi", true, "I think we should consider a way to"},
		{"pi_typed.txt", "pi", true, "I think we should consider a way to"},
		{"pi_multiline.txt", "pi", true, "first second"},
		{"pi_wrapped.ansi", "pi", true, "word word"},
		{"pi_slash.txt", "pi", true, "/mo"},
	}
	for _, c := range cases {
		got := DetectInput(inputFixture(t, c.file), c.kind)
		if !got.Known {
			t.Errorf("%s: not recognised", c.file)
			continue
		}
		if got.Busy != c.busy {
			t.Errorf("%s: busy = %v, want %v (text %q)", c.file, got.Busy, c.busy, got.Text)
		}
		if c.busy && !strings.Contains(got.Text, c.text) {
			t.Errorf("%s: text %q lacks %q", c.file, got.Text, c.text)
		}
		if !c.busy && got.Text != "" {
			t.Errorf("%s: empty box reads %q", c.file, got.Text)
		}
	}
}

func TestDetectInputUnknown(t *testing.T) {
	for name, c := range map[string]struct{ screen, kind string }{
		"other kind":      {inputFixture(t, "claude_typed.txt"), "codex"},
		"no kind":         {inputFixture(t, "claude_typed.txt"), ""},
		"no box":          {"hello\nworld\n", "claude"},
		"one rule":        {"hello\n" + strings.Repeat("─", 40) + "\n❯ x\n", "claude"},
		"claude no glyph": {strings.Repeat("─", 40) + "\nsome dialog row\n" + strings.Repeat("─", 40) + "\n", "claude"},
		"empty screen":    {"", "pi"},
	} {
		if got := DetectInput(c.screen, c.kind); got.Known {
			t.Errorf("%s: want unknown, got %+v", name, got)
		}
	}
}

// A user who types text shaped like the placeholder is still typing: with
// styling the dim attribute decides, not the words.
func TestDetectInputPlaceholderLookalike(t *testing.T) {
	rule := strings.Repeat("─", 40)
	typed := rule + "\n❯ Try \"fix typecheck errors\"\n" + rule + "\n"
	if !DetectInput("\x1b[0m"+typed, "claude").Busy {
		t.Error("typed text shaped like the placeholder read as empty")
	}
	dimmed := rule + "\n❯ \x1b[0m\x1b[2mTry \"fix typecheck errors\"\x1b[0m\n" + rule + "\n"
	if DetectInput(dimmed, "claude").Busy {
		t.Error("the dimmed placeholder read as typed text")
	}
}

func TestSplitStyledColourOperands(t *testing.T) {
	// 38;2;r;g;b contains a "2" that is not the dim attribute.
	_, live, styled := splitStyled("\x1b[38;2;136;136;136mhi\x1b[0m")
	if live != "hi" || !styled {
		t.Errorf("live = %q styled = %v", live, styled)
	}
	_, live, _ = splitStyled("a\x1b[2mb\x1b[22mc")
	if live != "ac" {
		t.Errorf("dim run not removed: %q", live)
	}
}
