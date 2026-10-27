package dialog

import (
	"errors"
	"strconv"
	"strings"

	"github.com/jhjaggars/comebye/internal/transcript"
)

// Resolve combines the screen and a pending transcript question (ask may be
// nil) into the dialog to present.
//
// Priority: the transcript ask supplies the exact question and options when
// the questionnaire on screen matches it; the screen supplies the highlight.
// Whatever the screen shows wins when it is a different dialog (for example
// a multi-question questionnaire's Submit review). An ask with no matching
// dialog on screen is returned read-only (KindAsk, not answerable).
//
// Any transcript parser that fills transcript.Ask (Pi's ask_user_question,
// Claude Code's AskUserQuestion) plugs in here unchanged.
func Resolve(screen, agentKind string, ask *transcript.Ask) *Dialog {
	d := resolve(screen, agentKind, ask)
	if d != nil {
		d.Agent = agentKind
	}
	return d
}

func resolve(screen, agentKind string, ask *transcript.Ask) *Dialog {
	sd := Detect(screen, agentKind)
	if ask == nil || len(ask.Questions) == 0 {
		return sd
	}
	if sd != nil {
		if questionKind(sd.Kind) {
			for _, q := range ask.Questions {
				if m := mergeAsk(sd, q); m != nil {
					m.Questions = len(ask.Questions)
					return m.finish()
				}
			}
		}
		return sd
	}
	q := ask.Questions[0]
	d := &Dialog{
		Kind: KindAsk, Source: "transcript", Header: q.Header, Question: q.Question,
		Multi: q.MultiSelect, Selected: -1, Keys: KeysNone, Questions: len(ask.Questions), CancelLabel: "Cancel",
	}
	for _, o := range q.Options {
		d.Options = append(d.Options, Option{Label: o.Label, Description: o.Description})
	}
	return d.finish()
}

// labelsMatch compares a screen label (possibly clipped with "…") with the
// authored one.
func labelsMatch(screen, authored string) bool {
	s, a := normSpace(screen), normSpace(authored)
	if s == a {
		return true
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "…"))
	return len(s) >= 3 && strings.HasPrefix(a, s)
}

// mergeAsk overlays question q onto a numbered screen dialog when every
// visible numbered row matches q's options (by number), returning nil
// otherwise.
func mergeAsk(sd *Dialog, q transcript.AskQuestion) *Dialog {
	n := len(q.Options)
	matched := 0
	activeNum, activeNext, activeChat := 0, false, false
	checked := map[int]*bool{}
	for k, o := range sd.Options {
		if k == sd.Selected {
			activeNum, activeNext = o.num, o.Role == RoleNext
			activeChat = o.Role == RoleChat
		}
		switch {
		case o.Role == RoleNext || o.Role == RoleChat:
			continue
		case o.num >= 1 && o.num <= n:
			if !labelsMatch(o.Label, q.Options[o.num-1].Label) {
				return nil
			}
			matched++
			checked[o.num] = o.Checked
		case o.num == n+1:
			// The free-text row: its label reads "Type something.", vanishes
			// when focused, or shows the draft once something is typed.
		default:
			return nil
		}
	}
	if matched == 0 {
		return nil
	}
	d := *sd
	d.Source = "transcript+screen"
	d.Question = q.Question
	d.Header = q.Header
	d.Multi = q.MultiSelect || sd.Multi
	d.Options = nil
	for k, o := range q.Options {
		opt := Option{Label: o.Label, Description: o.Description, num: k + 1}
		if d.Multi {
			if c := checked[k+1]; c != nil {
				opt.Checked = c
			} else {
				opt.Checked = boolp(false) // not visible: assume unticked
			}
		}
		d.Options = append(d.Options, opt)
	}
	d.Options = append(d.Options, Option{Label: otherLabel, Role: RoleOther, num: n + 1})
	if d.Multi {
		// Keep the screen's commit row label ("Next", or "Submit" on the last tab).
		next := Option{Label: "Next", Role: RoleNext}
		for _, o := range sd.Options {
			if o.Role == RoleNext {
				next.Label = o.Label
			}
		}
		d.Options = append(d.Options, next)
	}
	hasChat := false
	for _, o := range sd.Options {
		if o.Role == RoleChat {
			hasChat = true
			d.Options = append(d.Options, o)
		}
	}
	switch {
	case activeChat && hasChat:
		d.Selected = len(d.Options) - 1
	case activeNext && d.Multi:
		d.Selected = len(d.Options) - 1 - btoi(hasChat)
	case activeNum >= 1 && activeNum <= n+1:
		d.Selected = activeNum - 1
	default:
		d.Selected = -1
		d.Answerable = false
	}
	if d.Multi && d.Kind != KindPiQuestion && !sd.Answerable {
		d.Answerable = false
	}
	if d.Multi && d.Kind == KindPiQuestion && d.Selected >= 0 {
		d.Answerable = true
	}
	return &d
}

// Answer actions.
const (
	ActionSelect = "select" // pick option (single-select)
	ActionToggle = "toggle" // flip a multi-select checkbox
	ActionSubmit = "submit" // commit a multi-select question (its Next row)
	ActionCancel = "cancel" // Esc
	ActionReset  = "reset"  // one Esc to leave a focused text field, only then
)

// ErrBadAnswer is returned (wrapped) for answers that cannot be keyed.
var ErrBadAnswer = errors.New("bad answer")

func bad(msg string) error { return errors.Join(ErrBadAnswer, errors.New(msg)) }

// maxMoves bounds the arrow presses one answer may send.
const maxMoves = 40

// Keys computes the key presses that perform action on d, starting from the
// highlight d.Selected. It never returns y/n for Pi dialogs and never
// anything beyond the moves plus one confirming key.
func Keys(d *Dialog, action string, index int) ([]string, error) {
	if d == nil {
		return nil, bad("no dialog")
	}
	if action == ActionReset {
		// Leaving INSERT mode is the only thing an Esc may do here: once the
		// field is not focused, Esc cancels the whole question.
		if !d.Insert {
			return nil, bad("no text field is focused; nothing to reset")
		}
		return []string{"esc"}, nil
	}
	if action == ActionCancel {
		if d.CancelLabel == "" {
			return nil, bad("this dialog has no cancel")
		}
		return []string{"esc"}, nil
	}
	if d.Insert {
		return nil, bad("a text field is focused and swallows the keys: reset the dialog first")
	}
	if !d.Answerable {
		return nil, bad("this dialog can only be answered with the key pad")
	}
	switch action {
	case ActionSelect, ActionToggle, ActionSubmit:
	default:
		return nil, bad("unknown action " + strconv.Quote(action))
	}
	if action == ActionSubmit {
		index = -1
		for k, o := range d.Options {
			if o.Role == RoleNext {
				index = k
			}
		}
		if !d.Multi || index < 0 {
			return nil, bad("nothing to submit")
		}
	}
	if index < 0 || index >= len(d.Options) {
		return nil, bad("option out of range")
	}
	o := d.Options[index]
	if o.Role == RoleOther {
		return nil, bad("free-text answers are not supported; use the key pad")
	}
	if o.Role == RoleChat {
		return nil, bad("\"Chat about this\" is not an answer")
	}
	if d.Multi != (action != ActionSelect) {
		if d.Multi {
			return nil, bad("multi-select: toggle options, then submit")
		}
		return nil, bad("single-select: select an option")
	}
	if action == ActionToggle && o.Role == RoleNext {
		return nil, bad("use submit to commit")
	}

	switch d.Keys {
	case KeysYN:
		if strings.HasPrefix(d.Kind, "pi_") {
			return nil, bad("y/n is never sent to Pi dialogs")
		}
		if index == 0 {
			return []string{"y", "enter"}, nil
		}
		return []string{"n", "enter"}, nil
	case KeysDigits:
		if o.num >= 1 && o.num <= 9 {
			return []string{strconv.Itoa(o.num)}, nil
		}
		// No usable digit: fall through to arrows.
	}
	return ArrowKeys(d, action, index)
}

// ArrowKeys is the arrow-key path of Keys: move from d.Selected to index,
// then Enter (select/submit) or Space (toggle). Moves never wrap, so the
// result is correct for both clamping and wrapping lists.
func ArrowKeys(d *Dialog, action string, index int) ([]string, error) {
	if d.Selected < 0 || d.Selected >= len(d.Options) {
		return nil, bad("the highlighted option is unknown; use the key pad")
	}
	delta := index - d.Selected
	key := "down"
	if delta < 0 {
		key, delta = "up", -delta
	}
	if delta > maxMoves {
		return nil, bad("option too far from the highlight")
	}
	keys := make([]string, 0, delta+1)
	for range delta {
		keys = append(keys, key)
	}
	if action == ActionToggle {
		return append(keys, "space"), nil
	}
	return append(keys, "enter"), nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
