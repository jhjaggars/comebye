package dialog

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jhjaggars/herdr-phone/internal/transcript"
)

// All screens below are synthetic, written from the components' render code
// (a Pi permission-gate extension's prompt UI, Pi's ExtensionSelectorComponent,
// rpiv-ask-user-question's WrappingSelect / MultiSelectView) and Claude Code's
// documented prompt layouts. None is a capture of a real session.

const transcriptNoise = `● Ran the tests
  ok   github.com/example/thing  0.4s
  → next I will push the branch
`

const piGate = transcriptNoise + `
  ⚠️ Permission Required

  Disposition: ask
  Source: classifier
  Rule: bash.git.push
  Likely intent (best effort): push the branch to origin

    git push origin feature

  ⏱ Auto-allow in 5s if idle · any input cancels

  ❯ Allow once
    Block

  ↑↓ choose  •  enter select  •  esc block

`

const piGateTrust = `
  ⚠️ Permission Required

  Disposition: ask
  Source: judge
  Rule: read.outside-cwd
  Likely intent (best effort): read a config file
  Session trust candidate (read-only path access only): /etc/app

    cat /etc/app/config.toml

    Allow once
  ❯ Trust /etc/app for this session (read-only)
    Block

  ↑↓ choose  •  enter select  •  esc block
`

const piSelect = transcriptNoise + `
────────────────────────────────────────

 Plan approved. What next? (12s)

   Execute the plan
 → Refine the plan
   Abort

 ↑↓ navigate  enter select  esc cancel

────────────────────────────────────────
`

const piQuestion = transcriptNoise + `
 ☐ Cache  ✔ Submit
 Which caching strategy should the service use?

   1. LRU
      bounded memory, evicts the least recently used entry
 ❯ 2. TTL
      entries expire after a fixed time
   3. Type something.

 Enter to select · ↑/↓ to navigate · n to add notes · Esc to cancel
`

const piQuestionMulti = `
 Which features should ship?

 ❯ 1. [ ] Auth
      login and sessions
   2. [✔] Logging
   3. [ ] Metrics
   4. [ ] Type something.
   Next

 Space to toggle · Enter to select · Esc to cancel
`

const claudePermissionBoxed = `● Bash(git status)
╭──────────────────────────────────────────────────────────────╮
│ Bash command                                                 │
│                                                              │
│   git push origin feature                                    │
│   Push the feature branch                                    │
│                                                              │
│ Do you want to proceed?                                      │
│ ❯ 1. Yes                                                     │
│   2. Yes, and don't ask again for git push commands in       │
│      /home/u/code/repo                                       │
│   3. No, and tell Claude what to do differently (esc)        │
╰──────────────────────────────────────────────────────────────╯
`

const claudePermissionPlain = `● Update(main.go)
────────────────────────────────────────────────────────────────
 Edit file
 main.go
 - fmt.Println("hi")
 + fmt.Println("hello")

 Do you want to make this edit to main.go?
   1. Yes
   2. Yes, allow all edits during this session (shift+tab)
 ❯ 3. No, and tell Claude what to do differently (esc)

 Esc to cancel · Tab to amend
`

const claudeQuestion = `
←  ☐ Cache  ✔ Submit  →

Which caching strategy should the service use?

❯ 1. LRU
     bounded memory
  2. TTL
     entries expire
  3. Type something.

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`

const yesNo = `Reading package lists... Done
The following packages will be upgraded: curl
Do you want to continue? [Y/n]`

const claudeIdle = `● Done. The plan is committed.

✻ Worked for 7s

────────────────────────────────────────
❯ commit the plan changes
────────────────────────────────────────
   Opus | repo | main
  -- INSERT -- ⏵⏵ auto mode on
`

// An answered permission prompt still visible above the prompt box.
const claudeStale = `
 Do you want to proceed?
 ❯ 1. Yes
   2. No

● Bash(git push) pushed.
────────────────────────────────────────
❯
────────────────────────────────────────
`

func labels(d *Dialog) []string {
	var out []string
	for _, o := range d.Options {
		out = append(out, o.Label)
	}
	return out
}

func TestDetectShapes(t *testing.T) {
	cases := []struct {
		name, screen, agent string
		kind                string
		labels              []string
		selected            int
		keys                string
		answerable, multi   bool
		question, title     string
		detailHas           string
	}{
		{"pi gate", piGate, "pi", KindPiGate, []string{"Allow once", "Block"}, 0, KeysArrows, true, false, "", "Permission required", "git push origin feature"},
		{"pi gate trust", piGateTrust, "pi", KindPiGate, []string{"Allow once", "Trust /etc/app for this session (read-only)", "Block"}, 1, KeysArrows, true, false, "", "Permission required", "cat /etc/app/config.toml"},
		{"pi select", piSelect, "pi", KindPiSelect, []string{"Execute the plan", "Refine the plan", "Abort"}, 1, KeysArrows, true, false, "", "Plan approved. What next?", ""},
		{"pi question", piQuestion, "pi", KindPiQuestion, []string{"LRU", "TTL", "Type something."}, 1, KeysArrows, true, false, "Which caching strategy should the service use?", "", ""},
		{"pi question multi", piQuestionMulti, "pi", KindPiQuestion, []string{"Auth", "Logging", "Metrics", "Type something.", "Next"}, 0, KeysArrows, true, true, "Which features should ship?", "", ""},
		{"claude permission boxed", claudePermissionBoxed, "claude", KindClaudePermission, []string{"Yes", "Yes, and don't ask again for git push commands in /home/u/code/repo", "No, and tell Claude what to do differently (esc)"}, 0, KeysDigits, true, false, "Do you want to proceed?", "", "git push origin feature"},
		{"claude permission plain", claudePermissionPlain, "claude", KindClaudePermission, []string{"Yes", "Yes, allow all edits during this session (shift+tab)", "No, and tell Claude what to do differently (esc)"}, 2, KeysDigits, true, false, "Do you want to make this edit to main.go?", "", "+ fmt.Println(\"hello\")"},
		{"claude question", claudeQuestion, "claude", KindClaudeQuestion, []string{"LRU", "TTL", "Type something."}, 0, KeysArrows, true, false, "Which caching strategy should the service use?", "", ""},
		{"numbered list other agent", claudePermissionPlain, "codex", KindSelect, []string{"Yes", "Yes, allow all edits during this session (shift+tab)", "No, and tell Claude what to do differently (esc)"}, 2, KeysArrows, true, false, "Do you want to make this edit to main.go?", "", ""},
		{"y/n confirm", yesNo, "codex", KindConfirm, []string{"Yes", "No"}, -1, KeysYN, true, false, "Do you want to continue? [Y/n]", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Detect(c.screen, c.agent)
			if d == nil {
				t.Fatal("no dialog detected")
			}
			if d.Kind != c.kind || d.Selected != c.selected || d.Keys != c.keys || d.Answerable != c.answerable || d.Multi != c.multi {
				t.Errorf("got kind=%s selected=%d keys=%s answerable=%v multi=%v", d.Kind, d.Selected, d.Keys, d.Answerable, d.Multi)
			}
			if got := labels(d); !reflect.DeepEqual(got, c.labels) {
				t.Errorf("labels = %q, want %q", got, c.labels)
			}
			if d.Question != c.question || d.Title != c.title {
				t.Errorf("question=%q title=%q", d.Question, d.Title)
			}
			if c.detailHas != "" && !strings.Contains(d.Detail, c.detailHas) {
				t.Errorf("detail %q lacks %q", d.Detail, c.detailHas)
			}
			if len(d.Fingerprint) != 16 {
				t.Errorf("fingerprint %q", d.Fingerprint)
			}
		})
	}
}

func TestDetectDetails(t *testing.T) {
	d := Detect(piGate, "pi")
	if strings.Contains(d.Detail, "Auto-allow") {
		t.Errorf("countdown line leaked into detail: %q", d.Detail)
	}
	if !strings.HasPrefix(d.Detail, "Disposition: ask") {
		t.Errorf("detail not dedented: %q", d.Detail)
	}
	m := Detect(piQuestionMulti, "pi")
	want := []bool{false, true, false, false}
	for i, w := range want {
		if c := m.Options[i].Checked; c == nil || *c != w {
			t.Errorf("option %d checked = %v, want %v", i, c, w)
		}
	}
	if m.Options[3].Role != RoleOther || m.Options[4].Role != RoleNext || m.Options[0].Description != "login and sessions" {
		t.Errorf("roles/desc: %+v", m.Options)
	}
	q := Detect(claudeQuestion, "claude")
	if q.Options[0].Description != "bounded memory" {
		t.Errorf("description: %+v", q.Options[0])
	}
}

func TestDetectNegatives(t *testing.T) {
	cases := map[string]struct{ screen, agent string }{
		"claude idle prompt box":       {claudeIdle, "claude"},
		"stale dialog above prompt":    {claudeStale, "claude"},
		"arrow in transcript text":     {transcriptNoise + "\n→ also this\n  and that\n", "pi"},
		"empty":                        {"", "pi"},
		"y/n never for pi":             {yesNo, "pi"},
		"y/n not on last line":         {"Continue? [y/N]\nok\n", "codex"},
		"two highlighted rows":         {"Q?\n❯ 1. a\n❯ 2. b\n", "claude"},
		"numbers skip":                 {"Q?\n❯ 1. a\n  3. b\n", "claude"},
		"gate rows without any anchor": {"text\n  ❯ Allow once\n    Block\n", "pi"},
		"select without footer":        {" title\n → a\n   b\n", "pi"},
	}
	for name, c := range cases {
		if d := Detect(c.screen, c.agent); d != nil {
			t.Errorf("%s: unexpected dialog %+v", name, d)
		}
	}
}

func TestFingerprint(t *testing.T) {
	a := Detect(piGate, "pi")
	// Moving the highlight or the countdown disappearing does not change it.
	moved := strings.Replace(strings.Replace(piGate, "  ❯ Allow once\n    Block", "    Allow once\n  ❯ Block", 1), "  ⏱ Auto-allow in 5s if idle · any input cancels\n", "", 1)
	b := Detect(moved, "pi")
	if b.Selected != 1 || a.Fingerprint != b.Fingerprint {
		t.Errorf("highlight move changed fingerprint: %s vs %s (sel %d)", a.Fingerprint, b.Fingerprint, b.Selected)
	}
	// A different command does.
	c := Detect(strings.Replace(piGate, "git push origin feature", "git push --force origin feature", 1), "pi")
	if c.Fingerprint == a.Fingerprint {
		t.Error("different command, same fingerprint")
	}
	// Ticking a checkbox does.
	m1 := Detect(piQuestionMulti, "pi")
	m2 := Detect(strings.Replace(piQuestionMulti, "[ ] Auth", "[✔] Auth", 1), "pi")
	if m1.Fingerprint == m2.Fingerprint {
		t.Error("checkbox change kept the fingerprint")
	}
	// The select countdown ticking does not.
	s1 := Detect(piSelect, "pi")
	s2 := Detect(strings.Replace(piSelect, "(12s)", "(11s)", 1), "pi")
	if s1.Fingerprint != s2.Fingerprint {
		t.Error("countdown changed the fingerprint")
	}
}

func cacheAsk(multi bool) *transcript.Ask {
	return &transcript.Ask{ToolCallID: "q1", Questions: []transcript.AskQuestion{{
		Question: "Which caching strategy should the service use?", Header: "Cache", MultiSelect: multi,
		Options: []transcript.AskOption{
			{Label: "LRU", Description: "bounded memory, evicts the least recently used entry"},
			{Label: "TTL", Description: "entries expire after a fixed time"},
		},
	}}}
}

func TestResolveWithAsk(t *testing.T) {
	d := Resolve(piQuestion, "pi", cacheAsk(false))
	if d.Source != "transcript+screen" || d.Header != "Cache" || d.Selected != 1 || !d.Answerable || d.Questions != 1 {
		t.Fatalf("merged: %+v", d)
	}
	if got := labels(d); !reflect.DeepEqual(got, []string{"LRU", "TTL", "Type something."}) {
		t.Errorf("labels %q", got)
	}
	// Clipped labels on a narrow screen still match.
	ask := cacheAsk(false)
	ask.Questions[0].Options[0].Label = "LRU with a long explanation that the terminal clips"
	clipped := strings.Replace(piQuestion, "1. LRU", "1. LRU with a long expl…", 1)
	if d := Resolve(clipped, "pi", ask); d.Source != "transcript+screen" || d.Options[0].Label != ask.Questions[0].Options[0].Label {
		t.Errorf("clipped: %+v", d)
	}
	// The focused free-text row renders the draft instead of its label.
	drafting := strings.Replace(strings.Replace(piQuestion, " ❯ 2. TTL", "   2. TTL", 1), "   3. Type something.", " ❯ 3. use redis", 1)
	if d := Resolve(drafting, "pi", cacheAsk(false)); d.Source != "transcript+screen" || d.Selected != 2 {
		t.Errorf("drafting: %+v", d)
	}
	// A different list on screen (e.g. the Submit review) wins, unmerged.
	review := "\n Review your answers\n\n ❯ 1. Submit answers\n   2. Cancel\n"
	if d := Resolve(review, "pi", cacheAsk(false)); d.Source != "screen" || d.Options[0].Label != "Submit answers" {
		t.Errorf("review: %+v", d)
	}
	// No dialog on screen: read-only from the transcript.
	d = Resolve(claudeIdle, "pi", cacheAsk(false))
	if d.Kind != KindAsk || d.Answerable || d.Selected != -1 || len(d.Options) != 2 {
		t.Errorf("transcript-only: %+v", d)
	}
	if _, err := Keys(d, ActionSelect, 0); !errors.Is(err, ErrBadAnswer) {
		t.Error("transcript-only dialog must not be answerable")
	}
	if k, err := Keys(d, ActionCancel, 0); err != nil || !reflect.DeepEqual(k, []string{"esc"}) {
		t.Errorf("transcript-only cancel: %v %v", k, err)
	}
	// A permission gate on screen while an ask is pending wins.
	if d := Resolve(piGate, "pi", cacheAsk(false)); d.Kind != KindPiGate {
		t.Errorf("gate: %+v", d)
	}
	// Multi-select merge keeps checkbox states and adds Next.
	multiScreen := `
 Which caching strategy should the service use?

   1. [✔] LRU
 ❯ 2. [ ] TTL
   3. [ ] Type something.
   Next
`
	d = Resolve(multiScreen, "pi", cacheAsk(true))
	if !d.Multi || !d.Answerable || d.Selected != 1 || len(d.Options) != 4 || d.Options[3].Role != RoleNext || !*d.Options[0].Checked || *d.Options[1].Checked {
		t.Errorf("multi merge: %+v", d)
	}
}

func TestKeys(t *testing.T) {
	gate := Detect(piGate, "pi")                         // selected 0 of 2
	trust := Detect(piGateTrust, "pi")                   // selected 1 of 3
	sel := Detect(piSelect, "pi")                        // selected 1 of 3
	q := Detect(piQuestion, "pi")                        // selected 1 of 3 (LRU, TTL, Type something.)
	multi := Detect(piQuestionMulti, "pi")               // selected 0; Next at 4
	perm := Detect(claudePermissionPlain, "claude")      // digits, selected 2
	other := Detect(claudePermissionPlain, "codex")      // arrows, selected 2
	cq := Detect(claudeQuestion, "claude")               // arrows, selected 0
	yn := Detect(yesNo, "codex")                         // y/n
	permBoxed := Detect(claudePermissionBoxed, "claude") // digits
	cases := []struct {
		name   string
		d      *Dialog
		action string
		index  int
		want   []string
		bad    bool
	}{
		{"gate stay", gate, ActionSelect, 0, []string{"enter"}, false},
		{"gate down", gate, ActionSelect, 1, []string{"down", "enter"}, false},
		{"trust up", trust, ActionSelect, 0, []string{"up", "enter"}, false},
		{"trust down", trust, ActionSelect, 2, []string{"down", "enter"}, false},
		{"gate cancel", gate, ActionCancel, 0, []string{"esc"}, false},
		{"select up", sel, ActionSelect, 0, []string{"up", "enter"}, false},
		{"select down", sel, ActionSelect, 2, []string{"down", "enter"}, false},
		{"question up", q, ActionSelect, 0, []string{"up", "enter"}, false},
		{"question type something refused", q, ActionSelect, 2, nil, true},
		{"question out of range", q, ActionSelect, 3, nil, true},
		{"question negative", q, ActionSelect, -1, nil, true},
		{"question toggle on single", q, ActionToggle, 0, nil, true},
		{"multi toggle here", multi, ActionToggle, 0, []string{"space"}, false},
		{"multi toggle third", multi, ActionToggle, 2, []string{"down", "down", "space"}, false},
		{"multi submit", multi, ActionSubmit, 0, []string{"down", "down", "down", "down", "enter"}, false},
		{"multi select refused", multi, ActionSelect, 1, nil, true},
		{"multi toggle other refused", multi, ActionToggle, 3, nil, true},
		{"multi toggle next refused", multi, ActionToggle, 4, nil, true},
		{"claude digit 1", perm, ActionSelect, 0, []string{"1"}, false},
		{"claude digit 2 boxed", permBoxed, ActionSelect, 1, []string{"2"}, false},
		{"claude digit 3 (highlighted)", perm, ActionSelect, 2, []string{"3"}, false},
		{"claude cancel", perm, ActionCancel, 0, []string{"esc"}, false},
		{"other agent arrows", other, ActionSelect, 0, []string{"up", "up", "enter"}, false},
		{"claude question arrows", cq, ActionSelect, 1, []string{"down", "enter"}, false},
		{"yes", yn, ActionSelect, 0, []string{"y", "enter"}, false},
		{"no", yn, ActionSelect, 1, []string{"n", "enter"}, false},
		{"y/n has no cancel", yn, ActionCancel, 0, nil, true},
		{"unknown action", gate, "explode", 0, nil, true},
		{"nil dialog", nil, ActionSelect, 0, nil, true},
	}
	for _, c := range cases {
		got, err := Keys(c.d, c.action, c.index)
		if c.bad {
			if !errors.Is(err, ErrBadAnswer) {
				t.Errorf("%s: want ErrBadAnswer, got %v %v", c.name, got, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v %v, want %v", c.name, got, err, c.want)
		}
	}
	// Arrow fallback for a digits dialog (used when herdr rejects a digit).
	if k, _ := ArrowKeys(perm, ActionSelect, 0); !reflect.DeepEqual(k, []string{"up", "up", "enter"}) {
		t.Errorf("arrow fallback: %v", k)
	}
	// Unknown highlight: arrows cannot be computed.
	q2 := *q
	q2.Selected = -1
	if _, err := Keys(&q2, ActionSelect, 0); !errors.Is(err, ErrBadAnswer) {
		t.Error("unknown highlight must refuse")
	}
}

// Pi dialogs never get y or n: in the questionnaire, n opens the notes editor.
func TestNoYNForPi(t *testing.T) {
	for _, screen := range []string{piGate, piGateTrust, piSelect, piQuestion, piQuestionMulti, yesNo, claudePermissionPlain} {
		d := Resolve(screen, "pi", cacheAsk(false))
		if d == nil {
			continue
		}
		for i := range d.Options {
			for _, action := range []string{ActionSelect, ActionToggle, ActionSubmit, ActionCancel} {
				keys, _ := Keys(d, action, i)
				for _, k := range keys {
					if k == "n" || k == "y" {
						t.Fatalf("%s %s %d sent %q to a Pi dialog", d.Kind, action, i, k)
					}
				}
			}
		}
	}
}
