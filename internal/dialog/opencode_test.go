package dialog

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func labelsOf(d *Dialog) []string {
	var out []string
	for _, o := range d.Options {
		out = append(out, o.Label)
	}
	return out
}

// Real screens of OpenCode 1.19.5 (captured from a live session): the text
// alone shows the buttons but not the selection; the styled screen does.
func TestOpencodePermission(t *testing.T) {
	for _, c := range []struct {
		name string
		sel  int
	}{{"opencode_perm_once", 0}, {"opencode_perm_always", 1}, {"opencode_perm_reject_hl", 0}} {
		text := Detect(readFixture(t, c.name+".txt"), "opencode")
		if text == nil {
			t.Fatalf("%s: text screen not recognised", c.name)
		}
		if text.Kind != KindOpencodePermission || text.Title != "Permission required" || text.Answerable || text.Selected != -1 {
			t.Errorf("%s (text): %+v", c.name, text)
		}
		if got, want := labelsOf(text), []string{"Allow once", "Allow always", "Reject"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: options %q", c.name, got)
		}
		if !strings.HasPrefix(text.Question, "Access external directory /") || !strings.Contains(text.Detail, "Patterns") {
			t.Errorf("%s: question %q detail %q", c.name, text.Question, text.Detail)
		}
		d := Detect(readFixture(t, c.name+".ansi"), "opencode")
		if d == nil || d.Selected != c.sel || !d.Answerable || d.Keys != KeysArrows || d.CancelLabel != "Reject" {
			t.Fatalf("%s (ansi): %+v", c.name, d)
		}
		if d.Fingerprint != text.Fingerprint {
			t.Errorf("%s: fingerprint differs between the text and the styled screen", c.name)
		}
	}
}

func TestOpencodePermissionKeys(t *testing.T) {
	d := Detect(readFixture(t, "opencode_perm_always.ansi"), "opencode") // selected: Allow always
	for _, c := range []struct {
		idx  int
		want []string
	}{{0, []string{"left", "enter"}}, {1, []string{"enter"}}, {2, []string{"right", "enter"}}} {
		got, err := Keys(d, ActionSelect, c.idx)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("select %d: %v %v, want %v", c.idx, got, err, c.want)
		}
	}
	if k, _ := Keys(d, ActionCancel, 0); !reflect.DeepEqual(k, []string{"esc"}) {
		t.Errorf("cancel: %v", k)
	}
	// Without the styling nothing is known about the selection: no keys.
	plain := Detect(readFixture(t, "opencode_perm_always.txt"), "opencode")
	if _, err := Keys(plain, ActionSelect, 0); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("a text-only screen must not be answerable: %v", err)
	}
	// The same screen is not a dialog for other agents' parsers.
	if x := Detect(readFixture(t, "opencode_perm_once.txt"), "claude"); x != nil && x.Kind == KindOpencodePermission {
		t.Errorf("claude: %+v", x)
	}
}

func TestOpencodeQuestion(t *testing.T) {
	text := Detect(readFixture(t, "opencode_question.txt"), "opencode")
	if text == nil || text.Kind != KindOpencodeQuestion || text.Question != "Which colour?" {
		t.Fatalf("text: %+v", text)
	}
	if got, want := labelsOf(text), []string{"Red", "Green", "Blue", "Type your own answer"}; !reflect.DeepEqual(got, want) {
		t.Errorf("options %q", got)
	}
	if text.Options[0].Description != "Red" || text.Options[3].Role != RoleOther || text.Answerable {
		t.Errorf("%+v", text.Options)
	}
	d := Detect(readFixture(t, "opencode_question.ansi"), "opencode")
	if d == nil || d.Selected != 0 || !d.Answerable || d.Fingerprint != text.Fingerprint {
		t.Fatalf("ansi: %+v", d)
	}
	keys, err := Keys(d, ActionSelect, 2)
	if want := []string{"down", "down", "enter"}; err != nil || !reflect.DeepEqual(keys, want) {
		t.Errorf("keys %v %v", keys, err)
	}
	if _, err := Keys(d, ActionSelect, 3); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("free text must be refused: %v", err)
	}
}

// The sidebar next to the panel is not part of the prompt.
func TestOpencodePermissionIgnoresSidebar(t *testing.T) {
	d := Detect(readFixture(t, "opencode_perm_once.txt"), "opencode")
	if strings.Contains(d.Detail, "scratchpad") || strings.Contains(d.Detail, "worktrees") || strings.Contains(d.Question, "Context") {
		t.Errorf("sidebar leaked: question %q detail %q", d.Question, d.Detail)
	}
}

// "Allow always" opens a second prompt: Confirm / Cancel, where Esc goes back.
func TestOpencodeAlwaysConfirm(t *testing.T) {
	d := Detect(readFixture(t, "opencode_always_confirm.ansi"), "opencode")
	if d == nil || d.Kind != KindOpencodePermission || d.Title != "Always allow" || d.Selected != 0 || !d.Answerable || d.CancelLabel != "Back" {
		t.Fatalf("%+v", d)
	}
	if got, want := labelsOf(d), []string{"Confirm", "Cancel"}; !reflect.DeepEqual(got, want) {
		t.Errorf("options %q", got)
	}
	if k, err := Keys(d, ActionSelect, 1); err != nil || !reflect.DeepEqual(k, []string{"right", "enter"}) {
		t.Errorf("cancel button: %v %v", k, err)
	}
}
