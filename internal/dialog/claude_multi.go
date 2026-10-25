package dialog

import (
	"regexp"
	"strconv"
	"strings"
)

// Claude Code's multi-select AskUserQuestion, as captured from v2.1.310:
//
//	←  ☐ Investigate  ☐ Output  ✔ Submit  →      (tab bar, several questions)
//
//	What should be investigated?
//
//	❯ 1. [ ] A
//	         Option A
//	  2. [✔] B
//	  3. [ ] Type something                        (free-text row, always last)
//	     Next                                      ("Submit" on the last tab)
//	──────────────────────────────────────────
//	  4. Chat about this
//
//	Enter to select · Tab/Arrow keys to navigate · Esc to cancel
//
// Space toggles a checkbox, Enter on the commit row moves to the next tab.
// Committing with nothing ticked is allowed. Up/Down move through the rows in
// screen order, commit row and "Chat about this" included.
var (
	claudeNumRe    = regexp.MustCompile(`^(❯|\s)\s(\d{1,2})\.\s*(.*)$`)
	claudeCommitRe = regexp.MustCompile(`^(❯| )\s{2,}(Next|Submit)$`)
)

const chatLabel = "Chat about this"

// detectClaudeMulti recognizes the layout above, or returns nil.
func detectClaudeMulti(lines []string) *Dialog {
	commit := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if claudeCommitRe.MatchString(lines[i]) {
			commit = i
			break
		}
	}
	if commit < 0 {
		return nil
	}
	// The block of numbered rows and their descriptions right above it.
	start := commit
	for start > 0 {
		l := lines[start-1]
		if claudeNumRe.MatchString(l) || (strings.TrimSpace(l) != "" && indentOf(l) >= 5) {
			start--
			continue
		}
		break
	}
	type crow struct {
		active bool
		num    int
		text   string
		desc   []string
	}
	var rows []crow
	for _, l := range lines[start:commit] {
		if m := claudeNumRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			rows = append(rows, crow{active: m[1] == "❯", num: n, text: strings.TrimSpace(m[3])})
		} else if len(rows) > 0 {
			rows[len(rows)-1].desc = append(rows[len(rows)-1].desc, strings.TrimSpace(l))
		}
	}
	if len(rows) < 2 {
		return nil
	}
	for k := 1; k < len(rows); k++ {
		if rows[k].num != rows[k-1].num+1 {
			return nil
		}
	}
	d := &Dialog{Kind: KindClaudeQuestion, Source: "screen", Multi: true, Selected: -1, Keys: KeysArrows, CancelLabel: "Cancel", Answerable: true}
	active, hasCheck := 0, false
	for k, r := range rows {
		label, checked := splitCheckbox(r.text)
		o := Option{num: r.num, Checked: checked, Label: label, Description: strings.Join(r.desc, " ")}
		if k == len(rows)-1 { // the free-text row is always last
			o.Role, o.Label, o.Description = RoleOther, otherLabel, ""
			if checked == nil {
				o.Checked = boolp(false)
			}
		} else if checked != nil {
			hasCheck = true
		}
		if r.active {
			d.Selected, active = k, active+1
		}
		d.Options = append(d.Options, o)
	}
	if !hasCheck {
		return nil
	}
	cm := claudeCommitRe.FindStringSubmatch(lines[commit])
	d.Options = append(d.Options, Option{Label: cm[2], Role: RoleNext})
	if cm[1] == "❯" {
		d.Selected, active = len(d.Options)-1, active+1
	}
	// "Chat about this" sits below a rule.
	end := commit + 1
	for end < len(lines) && (strings.TrimSpace(lines[end]) == "" || isRule(lines[end])) {
		end++
	}
	if end < len(lines) {
		if m := claudeNumRe.FindStringSubmatch(lines[end]); m != nil && strings.TrimSpace(m[3]) == chatLabel {
			n, _ := strconv.Atoi(m[2])
			d.Options = append(d.Options, Option{Label: chatLabel, Role: RoleChat, num: n})
			if m[1] == "❯" {
				d.Selected, active = len(d.Options)-1, active+1
			}
		}
	}
	// Exactly one highlight, else the position is unknown.
	if active != 1 {
		d.Selected, d.Answerable = -1, false
	}
	d.Question, _ = nearestAbove(lines, start)
	// vim INSERT mode: a focused free-text row swallows Up/Down/Enter.
	if d.Selected >= 0 && d.Options[d.Selected].Role == RoleOther {
		for j := len(lines) - 1; j >= 0 && j >= len(lines)-4; j-- {
			if strings.Contains(lines[j], "-- INSERT --") {
				d.Insert = true
			}
		}
	}
	return d
}

// isOtherPlaceholder reports the free-text row's label: "Type something."
// in Pi, "Type something" in Claude Code.
func isOtherPlaceholder(label string) bool {
	return strings.TrimSuffix(strings.TrimSpace(label), ".") == strings.TrimSuffix(otherLabel, ".")
}
