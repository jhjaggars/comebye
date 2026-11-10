package dialog

import (
	"reflect"
	"strings"
	"testing"
)

// Claude Code's startup "trust this folder" dialog, captured from a real
// claude 2.1.322 (herdr pane.read, visible): unnumbered rows, "No, exit"
// highlighted by default, no digit shortcuts.
func TestClaudeTrustFolder(t *testing.T) {
	for _, c := range []struct {
		file, folder string
	}{
		{"claude_trust_initial.txt", "/tmp/ct/untrusted"},
		{"claude_trust_home.txt", "/tmp/ct/fh"},
	} {
		d := Detect(readFixture(t, c.file), "claude")
		if d == nil {
			t.Fatalf("%s: not detected", c.file)
		}
		if d.Kind != KindClaudeTrust || d.Selected != 0 || !d.Answerable || d.Keys != KeysArrows {
			t.Errorf("%s: %+v", c.file, d)
		}
		if d.Detail != c.folder {
			t.Errorf("%s: detail %q want %q", c.file, d.Detail, c.folder)
		}
		if !strings.HasPrefix(d.Question, "Quick safety check: Is this a project you created or one you trust?") {
			t.Errorf("%s: question %q", c.file, d.Question)
		}
		var labels []string
		for _, o := range d.Options {
			labels = append(labels, o.Label)
		}
		if want := []string{"No, exit", "Yes, I trust this folder"}; !reflect.DeepEqual(labels, want) {
			t.Errorf("%s: options %q", c.file, labels)
		}
		// Trusting is one arrow press from the default highlight, then Enter.
		keys, err := Keys(d, ActionSelect, 1)
		if err != nil || !reflect.DeepEqual(keys, []string{"down", "enter"}) {
			t.Errorf("%s: trust keys %v, %v", c.file, keys, err)
		}
		keys, err = Keys(d, ActionSelect, 0)
		if err != nil || !reflect.DeepEqual(keys, []string{"enter"}) {
			t.Errorf("%s: exit keys %v, %v", c.file, keys, err)
		}
		// Other agents' screens are not read this way, and Pi's gate/select
		// detectors do not claim it.
		if d := Detect(readFixture(t, c.file), "pi"); d != nil && d.Kind == KindClaudeTrust {
			t.Errorf("%s: pi saw a claude trust dialog", c.file)
		}
	}
}

// With the highlight moved to "Yes", the same dialog keeps its fingerprint
// (the highlight is not part of it) and answers with no arrow press.
func TestClaudeTrustHighlightMoved(t *testing.T) {
	screen := readFixture(t, "claude_trust_initial.txt")
	moved := strings.Replace(strings.Replace(screen, "❯ No, exit", "  No, exit", 1), "  Yes, I trust this folder", "❯ Yes, I trust this folder", 1)
	a, b := Detect(screen, "claude"), Detect(moved, "claude")
	if a == nil || b == nil || b.Selected != 1 || a.Fingerprint != b.Fingerprint {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	keys, err := Keys(b, ActionSelect, 1)
	if err != nil || !reflect.DeepEqual(keys, []string{"enter"}) {
		t.Errorf("keys %v, %v", keys, err)
	}
}
