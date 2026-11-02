package dialog

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Pi's startup "Trust project folder?" prompt: a built-in selector whose
// rows are marked "→ ", with no rule above it.
func TestPiTrustFolder(t *testing.T) {
	for _, c := range []struct {
		file string
		sel  int
	}{{"pi_trust_initial.txt", 0}, {"pi_trust_on_dont.txt", 3}} {
		d := Detect(readFixture(t, c.file), "pi")
		if d == nil {
			t.Fatalf("%s: not detected", c.file)
		}
		if d.Kind != KindPiSelect || d.Title != "Trust project folder?" || d.Selected != c.sel || !d.Answerable || d.Keys != KeysArrows {
			t.Errorf("%s: %+v", c.file, d)
		}
		if !strings.HasPrefix(d.Detail, "/home/jjaggars/.worktrees/hypershell-gitops/fix-gateway-creator-for-humans") {
			t.Errorf("%s: detail %q", c.file, d.Detail)
		}
		var labels []string
		for _, o := range d.Options {
			labels = append(labels, o.Label)
		}
		want := []string{"Trust", "Trust parent folder (/home/jjaggars/.worktrees/hypershell-gitops)", "Trust (this session only)", "Do not trust", "Do not trust (this session only)"}
		if !reflect.DeepEqual(labels, want) {
			t.Errorf("%s: options %q", c.file, labels)
		}
		// Answers are arrow presses from the highlighted row, then enter.
		keys, err := Keys(d, ActionSelect, 4)
		if err != nil {
			t.Fatal(err)
		}
		wantKeys := []string{"enter"}
		if n := 4 - c.sel; n > 0 {
			wantKeys = append(strings.Fields(strings.Repeat("down ", n)), "enter")
		}
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Errorf("%s: keys %v want %v", c.file, keys, wantKeys)
		}
		if k, _ := Keys(d, ActionCancel, 0); !reflect.DeepEqual(k, []string{"esc"}) {
			t.Errorf("%s: cancel %v", c.file, k)
		}
		if _, err := Keys(d, ActionSelect, 5); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%s: out of range accepted", c.file)
		}
	}
	// Resolve (what the server uses) agrees with Detect, with no transcript question.
	if d := Resolve(readFixture(t, "pi_trust_initial.txt"), "pi", nil); d == nil || d.Kind != KindPiSelect {
		t.Errorf("resolve: %+v", d)
	}
}

// Ordinary Pi output with arrows in it is not a dialog.
func TestPiArrowsAreNotADialog(t *testing.T) {
	for name, screen := range map[string]string{
		"status line":     " done\n\n ⚡auto ⏱ 74 tok in 0.6s → 128 tok/s\n",
		"arrow bullets":   "Plan:\n\n → step one\n → step two\n → step three\n\n ↑↓ is how you scroll\n",
		"pair, no footer": " Pick:\n\n → A\n   B\n\n> \n",
	} {
		if d := Detect(screen, "pi"); d != nil {
			t.Errorf("%s: detected %+v", name, d)
		}
	}
}
