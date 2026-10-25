// Package dialog recognizes the selection dialog a coding agent is showing
// (a permission prompt, a multiple-choice question, a yes/no confirm) and
// maps a tapped option to the keys that pick it.
//
// Everything here is a pure function of the agent's visible screen text
// (herdr pane.read source "visible", format "text"), the agent kind herdr
// reports ("pi", "claude", ...), and, when the agent has one pending, the
// structured question from its session transcript. The screen is the
// authority for *where the highlight is*; the transcript, when present, is
// the authority for the exact question and option wording.
package dialog

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Dialog kinds. The kind decides how a choice is turned into keys.
const (
	// KindPiGate is a Pi permission-gate extension prompt: "⚠️ Permission Required",
	// rows marked "❯ ", Allow once / Block, ↑/↓ clamp, Esc = Block.
	KindPiGate = "pi_gate"
	// KindPiSelect is Pi's built-in ctx.ui.select/confirm: rows marked "→ ",
	// footer "↑↓ navigate", ↑/↓ clamp, Esc cancels.
	KindPiSelect = "pi_select"
	// KindPiQuestion is the ask_user_question questionnaire (or any other
	// numbered "❯ 1." list in a Pi pane): ↑/↓ wrap, Enter, Space toggles in
	// multi-select, "n" opens a notes editor (so it is never sent).
	KindPiQuestion = "pi_question"
	// KindClaudePermission is Claude Code's tool permission prompt:
	// "❯ 1. Yes / 2. Yes, and don't ask again … / 3. No …" with digit
	// shortcuts.
	KindClaudePermission = "claude_permission"
	// KindClaudeQuestion is Claude Code's AskUserQuestion dialog (numbered
	// rows plus "Type something.", or checkboxes).
	KindClaudeQuestion = "claude_question"
	// KindSelect is a numbered "❯ 1." list from any other agent.
	KindSelect = "select"
	// KindConfirm is a generic line prompt ending in [y/N], (y/n), ….
	KindConfirm = "confirm"
	// KindAsk is a pending transcript question whose dialog could not be
	// found on screen: the options are known, the highlight is not, so it
	// cannot be answered by tapping.
	KindAsk = "ask"
)

// Key strategies.
const (
	KeysArrows = "arrows" // move the highlight with up/down, then enter/space
	KeysDigits = "digits" // press the option's number
	KeysYN     = "yn"     // type y or n, then enter
	KeysNone   = "none"
)

// Option roles. Plain options have no role.
const (
	RoleOther = "other" // the free-text "Type something." row
	RoleNext  = "next"  // the multi-select "Next" (commit) row
	RoleChat  = "chat"  // Claude's "Chat about this" row: never an answer
)

// Option is one row of a dialog.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Checked is set for multi-select rows.
	Checked *bool  `json:"checked,omitempty"`
	Role    string `json:"role,omitempty"`
	// num is the number printed in front of the row ("2." → 2), 0 if none.
	num int
}

// Dialog is a recognized, possibly answerable, dialog.
type Dialog struct {
	Kind   string `json:"kind"`
	Source string `json:"source"` // screen | transcript | transcript+screen
	// Agent is herdr's agent kind ("pi", "claude", ...), for wording hints.
	Agent string `json:"agent,omitempty"`
	// Title is the dialog's heading ("Permission required", a select title).
	Title string `json:"title,omitempty"`
	// Header is the short questionnaire chip ("Cache").
	Header string `json:"header,omitempty"`
	// Question is the line asked right above the options.
	Question string `json:"question,omitempty"`
	// Detail is context shown above the question (the command a permission
	// prompt is about), already dedented.
	Detail  string   `json:"detail,omitempty"`
	Options []Option `json:"options"`
	// Insert is set when the highlighted row is a free-text field in vim
	// INSERT mode: it swallows arrows and Enter until one Esc leaves it.
	Insert bool `json:"insert,omitempty"`
	// Selected is the highlighted option index, -1 when unknown.
	Selected int  `json:"selected"`
	Multi    bool `json:"multi,omitempty"`
	// Keys is the strategy used to answer (see Keys*).
	Keys string `json:"keys"`
	// Answerable reports whether tapping an option can be turned into keys.
	// False means: show the options, but answer with the key pad.
	Answerable bool `json:"answerable"`
	// CancelLabel names what Esc does here ("Block", "Cancel"); empty when
	// Esc is not a meaningful answer (line prompts).
	CancelLabel string `json:"cancel_label,omitempty"`
	// Questions is the number of questions in the pending transcript ask
	// (multi-question questionnaires answer one tab at a time).
	Questions int `json:"questions,omitempty"`
	// Fingerprint identifies the dialog's content (kind, text, options,
	// checkbox states), not the highlight position. An answer must quote it.
	Fingerprint string `json:"fingerprint"`
}

func (d *Dialog) finish() *Dialog {
	h := sha256.New()
	w := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	w(d.Kind)
	w(d.Title)
	w(d.Header)
	w(d.Question)
	w(d.Detail)
	for _, o := range d.Options {
		w(o.Role)
		w(o.Label)
		switch {
		case o.Checked == nil:
			w("")
		case *o.Checked:
			w("x")
		default:
			w("o")
		}
	}
	d.Fingerprint = hex.EncodeToString(h.Sum(nil))[:16]
	return d
}

// questionKind reports whether a kind is a questionnaire-like numbered list
// that a transcript ask can describe.
func questionKind(kind string) bool {
	switch kind {
	case KindPiQuestion, KindClaudeQuestion, KindSelect, KindClaudePermission:
		return true
	}
	return false
}

func boolp(b bool) *bool { return &b }

func normSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
