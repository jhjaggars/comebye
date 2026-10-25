package dialog

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/transcript"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func optLabels(d *Dialog) []string {
	var out []string
	for _, o := range d.Options {
		out = append(out, o.Label)
	}
	return out
}

func TestClaudeMultiDetect(t *testing.T) {
	cases := []struct {
		file     string
		selected int
		labels   []string
		checked  []bool // per option, first four
		question string
	}{
		{"claude_multi_a.txt", 0, []string{"A", "B", "C", "D", "Type something.", "Next", "Chat about this"}, []bool{false, false, false, false}, "What should be investigated?"},
		{"claude_multi_checked_on_c.txt", 2, []string{"A", "B", "C", "D", "Type something.", "Next", "Chat about this"}, []bool{true, false, false, false}, "What should be investigated?"},
		{"claude_multi_on_next.txt", 5, []string{"A", "B", "C", "D", "Type something.", "Next", "Chat about this"}, []bool{true, false, false, false}, "What should be investigated?"},
		{"claude_multi_on_chat.txt", 6, []string{"A", "B", "C", "D", "Type something.", "Next", "Chat about this"}, []bool{true, false, false, false}, "What should be investigated?"},
		{"claude_multi_last_tab.txt", 0, []string{"X", "Y", "Z", "Type something.", "Submit", "Chat about this"}, []bool{false, false, false}, "What output do you want?"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			d := Detect(fixture(t, c.file), "claude")
			if d == nil {
				t.Fatal("not detected")
			}
			if d.Kind != KindClaudeQuestion || !d.Multi || !d.Answerable || d.Insert {
				t.Errorf("kind=%s multi=%v answerable=%v insert=%v", d.Kind, d.Multi, d.Answerable, d.Insert)
			}
			if d.Selected != c.selected || d.Question != c.question || !reflect.DeepEqual(optLabels(d), c.labels) {
				t.Errorf("selected=%d question=%q labels=%q", d.Selected, d.Question, optLabels(d))
			}
			for k, want := range c.checked {
				if got := d.Options[k].Checked; got == nil || *got != want {
					t.Errorf("option %d checked=%v, want %v", k, got, want)
				}
			}
			if d.Options[len(c.labels)-2].Role != RoleNext || d.Options[len(c.labels)-1].Role != RoleChat || d.Options[len(c.labels)-3].Role != RoleOther {
				t.Errorf("roles: %+v", d.Options)
			}
		})
	}
}

func TestClaudeMultiReviewIsSingleSelect(t *testing.T) {
	d := Detect(fixture(t, "claude_multi_review.txt"), "claude")
	if d == nil || d.Multi || !d.Answerable || d.Options[0].Label != "Submit answers" {
		t.Errorf("review: %+v", d)
	}
}

func TestClaudeMultiKeys(t *testing.T) {
	onC := Detect(fixture(t, "claude_multi_checked_on_c.txt"), "claude") // highlight on C (2)
	onA := Detect(fixture(t, "claude_multi_a.txt"), "claude")
	cases := []struct {
		name   string
		d      *Dialog
		action string
		index  int
		want   []string
		bad    bool
	}{
		{"toggle here", onC, ActionToggle, 2, []string{"space"}, false},
		{"toggle A from C", onC, ActionToggle, 0, []string{"up", "up", "space"}, false},
		{"toggle D from C", onC, ActionToggle, 3, []string{"down", "space"}, false},
		{"submit with nothing ticked", onA, ActionSubmit, 0, []string{"down", "down", "down", "down", "down", "enter"}, false},
		{"submit from C", onC, ActionSubmit, 0, []string{"down", "down", "down", "enter"}, false},
		{"toggle other refused", onA, ActionToggle, 4, nil, true},
		{"toggle chat refused", onA, ActionToggle, 6, nil, true},
		{"toggle next refused", onA, ActionToggle, 5, nil, true},
		{"select refused", onA, ActionSelect, 0, nil, true},
		{"reset without insert refused", onA, ActionReset, 0, nil, true},
		{"cancel", onA, ActionCancel, 0, []string{"esc"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Keys(c.d, c.action, c.index)
			if (err != nil) != c.bad || !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, %v; want %v (bad=%v)", got, err, c.want, c.bad)
			}
		})
	}
}

// The incident: the free-text row focused in vim INSERT mode swallows
// arrows and Enter. Only a single Esc is safe, and only then.
func TestClaudeMultiInsertMode(t *testing.T) {
	screen := strings.Replace(fixture(t, "claude_multi_a.txt"), "❯ 1. [ ] A", "  1. [ ] A", 1)
	screen = strings.Replace(screen, "  5. [ ] Type something", "❯ 5. [ ] We just handled a multi select box", 1)
	screen += "\n-- INSERT --\n"
	d := Detect(screen, "claude")
	if d == nil || d.Selected != 4 || !d.Insert || d.Options[4].Role != RoleOther || d.Options[4].Label != "Type something." {
		t.Fatalf("insert: %+v", d)
	}
	for _, a := range []string{ActionToggle, ActionSubmit} {
		if keys, err := Keys(d, a, 0); err == nil {
			t.Errorf("%s allowed in INSERT mode: %v", a, keys)
		}
	}
	if keys, err := Keys(d, ActionReset, 0); err != nil || !reflect.DeepEqual(keys, []string{"esc"}) {
		t.Errorf("reset: %v %v", keys, err)
	}
}

func TestClaudeMultiMergesAsk(t *testing.T) {
	ask := &transcript.Ask{Questions: []transcript.AskQuestion{{
		Header: "Investigate", Question: "What should be investigated?", MultiSelect: true,
		Options: []transcript.AskOption{{Label: "A"}, {Label: "B"}, {Label: "C"}, {Label: "D"}},
	}}}
	for file, sel := range map[string]int{"claude_multi_a.txt": 0, "claude_multi_on_next.txt": 5, "claude_multi_on_chat.txt": 6} {
		d := Resolve(fixture(t, file), "claude", ask)
		if d == nil || d.Source != "transcript+screen" || !d.Answerable || d.Selected != sel || len(d.Options) != 7 || d.Options[6].Role != RoleChat {
			t.Errorf("%s: %+v", file, d)
		}
	}
}
