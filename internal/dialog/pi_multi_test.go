package dialog

import (
	"reflect"
	"testing"

	"github.com/jhjaggars/comebye/internal/transcript"
)

// Pi's ask_user_question multi-select, as captured from pi 1.4.0 (fixtures
// recorded from a live agent; see docs/investigations/pi-multiselect-dialog.md):
//
//	❯ 1. [ ] Apple
//	  keeps the doctor away        (description at the row-text column)
//	  2. [ ] Banana
//	  yellow
//	  3. [ ] Cherry
//	  red
//	  4. [ ] Type something.
//	  Submit                        ("Next" on every tab of a multi-question ask)
//
// Enter to select · ↑/↓ to navigate (wrapping) · Space to toggle · Esc to
// cancel. The free-text row is inline: focused it renders as a bare
// "❯ 4. [ ]", typed text as "❯ 4. [✔] hello", and arrows keep working from
// the typed state (no vim INSERT mode, unlike Claude).

func fruitAsk() *transcript.Ask {
	return &transcript.Ask{Questions: []transcript.AskQuestion{{
		Header: "Fruit", Question: "Which fruits do you like?", MultiSelect: true,
		Options: []transcript.AskOption{
			{Label: "Apple", Description: "keeps the doctor away"},
			{Label: "Banana", Description: "yellow"},
			{Label: "Cherry", Description: "red"},
		},
	}}}
}

func TestPiMultiDetect(t *testing.T) {
	cases := []struct {
		file     string
		selected int
		labels   []string
		checked  map[int]bool
		question string
		// otherRole: the free-text row shows its "Type something." label.
		// Focused or typed, only the merged transcript dialog knows the role.
		otherRole bool
	}{
		{"pi_multi_initial.txt", 0, []string{"Apple", "Banana", "Cherry", "Type something.", "Submit"}, nil, "Which fruits do you like?", true},
		{"pi_multi_toggled.txt", 0, []string{"Apple", "Banana", "Cherry", "Type something.", "Submit"}, map[int]bool{0: true}, "Which fruits do you like?", true},
		// The focused free-text row loses its label; the typed draft shows in it.
		{"pi_multi_on_other.txt", 3, []string{"Apple", "Banana", "Cherry", "", "Submit"}, map[int]bool{0: true, 3: false}, "Which fruits do you like?", false},
		{"pi_multi_other_typed.txt", 3, []string{"Apple", "Banana", "Cherry", "hello", "Submit"}, map[int]bool{0: true, 3: true}, "Which fruits do you like?", false},
		{"pi_multi_on_submit.txt", 4, []string{"Apple", "Banana", "Cherry", "hello", "Submit"}, map[int]bool{0: true, 3: true}, "Which fruits do you like?", false},
		// A multi-question ask commits with "Next" until its last tab.
		{"pi_multi_two_questions.txt", 0, []string{"Small", "Medium", "Large", "Type something.", "Next"}, nil, "What size?", true},
		{"pi_multi_two_q_on_next.txt", 4, []string{"Small", "Medium", "Large", "Type something.", "Next"}, map[int]bool{0: true}, "What size?", true},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			d := Detect(fixture(t, c.file), "pi")
			if d == nil {
				t.Fatal("not detected")
			}
			if d.Kind != KindPiQuestion || !d.Multi || !d.Answerable || d.Insert {
				t.Errorf("kind=%s multi=%v answerable=%v insert=%v", d.Kind, d.Multi, d.Answerable, d.Insert)
			}
			if d.Selected != c.selected || d.Question != c.question || !reflect.DeepEqual(optLabels(d), c.labels) {
				t.Errorf("selected=%d question=%q labels=%q", d.Selected, d.Question, optLabels(d))
			}
			if d.Options[len(c.labels)-1].Role != RoleNext {
				t.Errorf("commit row role=%q", d.Options[len(c.labels)-1].Role)
			}
			if role := d.Options[len(c.labels)-2].Role; (role == RoleOther) != c.otherRole {
				t.Errorf("free-text row role=%q, otherRole=%v", role, c.otherRole)
			}
			if c.checked == nil {
				c.checked = map[int]bool{}
			}
			for k := 0; k < len(c.labels)-2; k++ {
				want, ok := c.checked[k]
				got := d.Options[k].Checked
				if !ok && got == nil {
					continue
				}
				if got == nil || *got != want {
					t.Errorf("option %d checked=%v, want %v", k, got, want)
				}
			}
			if d.Keys != KeysArrows || d.CancelLabel != "Cancel" {
				t.Errorf("keys=%s cancel=%q", d.Keys, d.CancelLabel)
			}
		})
	}
}

func TestPiMultiNoDialogAfterAnswer(t *testing.T) {
	for _, f := range []string{"pi_multi_answered.txt", "pi_multi_cancelled.txt"} {
		if d := Detect(fixture(t, f), "pi"); d != nil {
			t.Errorf("%s: detected %s, want nil", f, d.Kind)
		}
	}
}

// The single-select layout (deeper descriptions, no commit row) and the
// single-select tab of a multi-question ask must stay single-select.
func TestPiSingleStillSelects(t *testing.T) {
	for _, c := range []struct {
		file   string
		labels []string
	}{
		{"pi_single_initial.txt", []string{"Red", "Green", "Blue", "Type something."}},
		{"pi_multi_two_q_tab2.txt", []string{"Slow", "Fast", "Type something."}},
	} {
		t.Run(c.file, func(t *testing.T) {
			d := Detect(fixture(t, c.file), "pi")
			if d == nil {
				t.Fatal("not detected")
			}
			if d.Kind != KindPiQuestion || d.Multi || !d.Answerable {
				t.Errorf("kind=%s multi=%v answerable=%v", d.Kind, d.Multi, d.Answerable)
			}
			if !reflect.DeepEqual(optLabels(d), c.labels) {
				t.Errorf("labels=%q", optLabels(d))
			}
			if keys, err := Keys(d, ActionSelect, 1); err != nil || !reflect.DeepEqual(keys, []string{"down", "enter"}) {
				t.Errorf("select(1)=%v err=%v", keys, err)
			}
		})
	}
}

func TestPiMultiKeys(t *testing.T) {
	d := Detect(fixture(t, "pi_multi_initial.txt"), "pi")
	if d == nil {
		t.Fatal("not detected")
	}
	cases := []struct {
		action string
		index  int
		keys   []string
	}{
		{ActionToggle, 0, []string{"space"}},
		{ActionToggle, 2, []string{"down", "down", "space"}},
		{ActionSubmit, 0, []string{"down", "down", "down", "down", "enter"}},
		{ActionCancel, 0, []string{"esc"}},
	}
	for _, c := range cases {
		keys, err := Keys(d, c.action, c.index)
		if err != nil || !reflect.DeepEqual(keys, c.keys) {
			t.Errorf("Keys(%s,%d)=%v err=%v, want %v", c.action, c.index, keys, err, c.keys)
		}
	}
	// Moves never wrap, so they are correct for pi's wrapping list too.
	onSubmit := Detect(fixture(t, "pi_multi_on_submit.txt"), "pi")
	if keys, err := Keys(onSubmit, ActionSubmit, 0); err != nil || !reflect.DeepEqual(keys, []string{"enter"}) {
		t.Errorf("submit from the Submit row=%v err=%v", keys, err)
	}
	if _, err := Keys(d, ActionSelect, 0); err == nil {
		t.Error("select on a multi-select must be refused")
	}
	if _, err := Keys(d, ActionToggle, len(d.Options)-1); err == nil {
		t.Error("toggling the commit row must be refused")
	}
}

func TestPiMultiMergesAsk(t *testing.T) {
	fruit := fruitAsk()
	for _, c := range []struct {
		file     string
		selected int
		checked  map[int]bool
	}{
		{"pi_multi_initial.txt", 0, nil},
		{"pi_multi_toggled.txt", 0, map[int]bool{0: true}},
		{"pi_multi_on_other.txt", 3, map[int]bool{0: true}},
		{"pi_multi_other_typed.txt", 3, map[int]bool{0: true}},
		{"pi_multi_on_submit.txt", 4, map[int]bool{0: true}},
	} {
		t.Run(c.file, func(t *testing.T) {
			d := Resolve(fixture(t, c.file), "pi", fruit)
			if d == nil {
				t.Fatal("not resolved")
			}
			if d.Kind != KindPiQuestion || d.Source != "transcript+screen" || !d.Multi || !d.Answerable {
				t.Errorf("kind=%s source=%s multi=%v answerable=%v", d.Kind, d.Source, d.Multi, d.Answerable)
			}
			if d.Header != "Fruit" || d.Question != "Which fruits do you like?" || d.Selected != c.selected || d.Questions != 1 {
				t.Errorf("header=%q question=%q selected=%d questions=%d", d.Header, d.Question, d.Selected, d.Questions)
			}
			want := []string{"Apple", "Banana", "Cherry", "Type something.", "Submit"}
			if !reflect.DeepEqual(optLabels(d), want) {
				t.Errorf("labels=%q", optLabels(d))
			}
			if d.Options[3].Role != RoleOther || d.Options[4].Role != RoleNext {
				t.Errorf("roles=%q %q", d.Options[3].Role, d.Options[4].Role)
			}
			if c.checked == nil {
				c.checked = map[int]bool{}
			}
			for k := 0; k < 3; k++ {
				got := d.Options[k].Checked
				if want, ok := c.checked[k]; !ok {
					if got == nil || *got {
						t.Errorf("option %d checked=%v, want false", k, got)
					}
				} else if got == nil || *got != want {
					t.Errorf("option %d checked=%v, want %v", k, got, want)
				}
			}
		})
	}
}

// A multi-question ask merges tab 1 with its "Next" commit row and reports
// the question count; its single-select tab 2 stays a plain select.
func TestPiMultiQuestionAsk(t *testing.T) {
	ask := &transcript.Ask{Questions: []transcript.AskQuestion{
		{Header: "Size", Question: "What size?", MultiSelect: true, Options: []transcript.AskOption{{Label: "Small"}, {Label: "Medium"}, {Label: "Large"}}},
		{Header: "Speed", Question: "How fast?", Options: []transcript.AskOption{{Label: "Slow"}, {Label: "Fast"}}},
	}}
	d := Resolve(fixture(t, "pi_multi_two_questions.txt"), "pi", ask)
	if d == nil {
		t.Fatal("not resolved")
	}
	if d.Kind != KindPiQuestion || !d.Multi || !d.Answerable || d.Questions != 2 || d.Selected != 0 {
		t.Errorf("kind=%s multi=%v answerable=%v questions=%d selected=%d", d.Kind, d.Multi, d.Answerable, d.Questions, d.Selected)
	}
	want := []string{"Small", "Medium", "Large", "Type something.", "Next"}
	if !reflect.DeepEqual(optLabels(d), want) {
		t.Errorf("labels=%q", optLabels(d))
	}
	d2 := Resolve(fixture(t, "pi_multi_two_q_tab2.txt"), "pi", ask)
	if d2 == nil {
		t.Fatal("tab 2 not resolved")
	}
	// The visible single-select tab merges with the ask's second question.
	if d2.Source != "transcript+screen" || d2.Multi || !d2.Answerable || d2.Questions != 2 {
		t.Errorf("tab 2: source=%s multi=%v answerable=%v questions=%d", d2.Source, d2.Multi, d2.Answerable, d2.Questions)
	}
	if d2.Header != "Speed" || d2.Question != "How fast?" {
		t.Errorf("tab 2: header=%q question=%q", d2.Header, d2.Question)
	}
	if !reflect.DeepEqual(optLabels(d2), []string{"Slow", "Fast", "Type something."}) {
		t.Errorf("tab 2 labels=%q", optLabels(d2))
	}
}
