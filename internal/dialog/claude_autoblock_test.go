package dialog

import (
	"reflect"
	"strings"
	"testing"
)

// Claude Code's auto-mode permission prompt for a command its classifier
// flagged: after the command box come a "│" gutter line with the
// classifier's reason and a live "will automatically block this request in
// 1:17" warning, then two blank lines and the question. Reconstructed from
// a screenshot (worker laharl/w8Y:p1, 2026-11-10); not a raw capture.
func TestClaudeAutoBlockWarning(t *testing.T) {
	screen := readFixture(t, "claude_autoblock.txt")
	d := Detect(screen, "claude")
	if d == nil {
		t.Fatal("not detected")
	}
	if d.Kind != KindClaudePermission || d.Keys != KeysDigits || !d.Answerable || d.Selected != 0 {
		t.Errorf("shape: %+v", d)
	}
	if got, want := labels(d), []string{"Yes", "No"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels %q", got)
	}
	if d.Question != "Do you want to proceed?" {
		t.Errorf("question %q", d.Question)
	}
	if !strings.Contains(d.Detail, "git add internal/server/hubcheck.go") || !strings.Contains(d.Detail, "git log --oneline origin/main..HEAD") {
		t.Errorf("command missing from detail: %q", d.Detail)
	}
	if strings.Contains(d.Detail, "kill $(cat $T/cbpid) $(sed -n 1p $T/pids) 2>/dev/null; rm -rf $(cat $T/sd)\n    git worktree") {
		t.Errorf("previous command's text leaked: %q", d.Detail)
	}
	if strings.Contains(d.Detail, "automatically block") || strings.Contains(d.Detail, "Dangerous rm") {
		t.Errorf("warning leaked into detail: %q", d.Detail)
	}
	if !strings.Contains(d.Note, "Dangerous rm operation on statically-unresolvable target") || !strings.Contains(d.Note, "automatically block this request") {
		t.Errorf("note %q", d.Note)
	}
	// The countdown ticking changes neither the fingerprint nor the dialog.
	tick := Detect(strings.Replace(screen, "in 1:17,", "in 1:16,", 1), "claude")
	if tick.Fingerprint != d.Fingerprint {
		t.Error("countdown tick changed the fingerprint")
	}
	if !strings.Contains(tick.Note, "1:16") {
		t.Errorf("note not live: %q", tick.Note)
	}
	// Keys: "No" is digit 2.
	keys, err := Keys(d, ActionSelect, 1)
	if err != nil || !reflect.DeepEqual(keys, []string{"2"}) {
		t.Errorf("keys %v %v", keys, err)
	}
}

// Non-breaking spaces in place of the blanks around the marker and numbers
// must not hide the dialog. Not seen in a capture; a hardening.
func TestClaudePermissionNBSP(t *testing.T) {
	screen := readFixture(t, "claude_autoblock.txt")
	screen = strings.Replace(screen, "❯ 1. Yes", "❯\u00a01.\u00a0Yes", 1)
	screen = strings.Replace(screen, "  2. No", "\u00a0\u00a02.\u00a0No", 1)
	if d := Detect(screen, "claude"); d == nil || d.Kind != KindClaudePermission || len(d.Options) != 2 {
		t.Errorf("%+v", d)
	}
}
