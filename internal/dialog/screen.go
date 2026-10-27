package dialog

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Screen parsing.
//
// Every recognized dialog is a vertical list of rows sharing one marker
// column: the highlighted row starts with a marker ("❯ " or "→ ") and the
// others with two spaces at the same column. Only the bottom-most marker
// line on the screen is considered: dialogs render at the bottom, and an
// older dialog still visible higher up (already answered, now scrollback)
// must never be mistaken for the live one. A bare "❯ " input line (Claude
// Code's prompt box) is never a dialog: lists need at least two rows plus
// a shape-specific anchor (numbers, a footer, or a heading).

const (
	markerChevron = '❯'
	markerArrow   = '→'
	// contextAbove bounds how far above the list headings are searched.
	contextAbove = 40
	// maxDetailLines caps the context shown above a permission question.
	maxDetailLines = 20
)

var (
	numberedRe  = regexp.MustCompile(`^(\d{1,2})\.(?:\s+(.*))?$`)
	countdownRe = regexp.MustCompile(`(?m) \(\d+s\)$`)
	confirmRe   = regexp.MustCompile(`(?i)(\[\s*y\s*/\s*n\s*\]|\(\s*y\s*/\s*n\s*\)|\[\s*yes\s*/\s*no\s*\]|\(\s*yes\s*/\s*no\s*\))\s*[:?]?\s*$`)
)

// row is one parsed list row.
type row struct {
	line   int // screen line index
	active bool
	num    int    // printed number, 0 if none
	text   string // label text after the number
	desc   []string
}

// Detect finds the dialog on a visible screen. agentKind is herdr's agent
// kind ("pi", "claude", ...); it picks between shapes that look alike and
// decides which shortcut keys are safe. Returns nil when nothing is
// recognized.
func Detect(screen, agentKind string) *Dialog {
	lines := normalizeScreen(screen)
	if agentKind == "claude" {
		if d := detectClaudeMulti(lines); d != nil {
			return d.finish()
		}
	}
	// The lower of the two marker kinds wins; the other is stale.
	ci, cc := lastMarker(lines, markerChevron)
	ai, ac := lastMarker(lines, markerArrow)
	if ci >= 0 && ci >= ai {
		if d := detectChevron(lines, ci, cc, agentKind); d != nil {
			return d.finish()
		}
	} else if ai >= 0 {
		if d := detectPiSelect(lines, ai, ac); d != nil {
			return d.finish()
		}
	}
	if agentKind != "pi" { // never synthesize y/n in Pi dialogs
		if d := detectConfirm(lines); d != nil {
			return d.finish()
		}
	}
	return nil
}

// normalizeScreen splits the screen into lines, drops trailing blanks, and
// blanks out box borders ("│ … │") so column positions are preserved.
func normalizeScreen(screen string) []string {
	raw := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	out := make([]string, len(raw))
	for i, l := range raw {
		l = strings.ReplaceAll(l, "\t", "    ")
		l = strings.TrimRight(l, "  ")
		trimmed := strings.TrimLeft(l, " ")
		if strings.HasPrefix(trimmed, "│") {
			l = strings.Replace(l, "│", " ", 1)
			if strings.HasSuffix(l, "│") {
				l = strings.TrimSuffix(l, "│")
			}
			l = strings.TrimRight(l, " ")
		}
		out[i] = l
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// lastMarker returns the index and rune column of the bottom-most line whose
// first non-space character is marker, followed by a space or nothing.
func lastMarker(lines []string, marker rune) (int, int) {
	for i := len(lines) - 1; i >= 0; i-- {
		r := []rune(lines[i])
		c := 0
		for c < len(r) && r[c] == ' ' {
			c++
		}
		// A bare marker (an empty prompt box) counts too: it is the bottom
		// marker, so anything above it is stale.
		if c < len(r) && r[c] == marker && (c+1 == len(r) || r[c+1] == ' ') {
			return i, c
		}
	}
	return -1, -1
}

// rowAt parses line as a list row whose marker column is c.
func rowAt(line string, c int, marker rune) (active bool, text string, ok bool) {
	r := []rune(line)
	if len(r) <= c+2 || r[c+2] == ' ' {
		return false, "", false
	}
	for _, ch := range r[:c] {
		if ch != ' ' {
			return false, "", false
		}
	}
	switch {
	case r[c] == marker && r[c+1] == ' ':
		active = true
	case r[c] == ' ' && r[c+1] == ' ':
	default:
		return false, "", false
	}
	return active, strings.TrimSpace(string(r[c+2:])), true
}

// indentOf returns the rune column of the first non-space character, or -1
// for a blank line.
func indentOf(line string) int {
	for i, ch := range []rune(line) {
		if ch != ' ' {
			return i
		}
	}
	return -1
}

// commitRowLabels lists the unnumbered commit-row labels a numbered list
// may end with. pi questionnaires commit with "Submit" (single question)
// or "Next" (every tab of a multi-question ask); other agents only
// "Next". Scoped so a generic numbered list ending in "Submit" (some
// other agent) is not mistaken for a commit row.
func commitRowLabels(agentKind string) []string {
	if agentKind == "pi" {
		return []string{"Next", "Submit"}
	}
	return []string{"Next"}
}

func isCommitLabel(text, agentKind string) bool {
	for _, l := range commitRowLabels(agentKind) {
		if text == l {
			return true
		}
	}
	return false
}

// rowHasCheckbox reports whether a numbered row's label carries a checkbox
// prefix ("1. [ ] Apple", with or without the number). A bare "[ ]" (the
// focused free-text row, label empty) counts too.
func rowHasCheckbox(text string) bool {
	if m := numberedRe.FindStringSubmatch(text); m != nil {
		text = m[2]
	}
	_, checked := splitCheckbox(strings.TrimSpace(text))
	return checked != nil
}

// collectRows gathers the list around the marker line i. With numbered,
// rows must read "N. label" (or be the bare commit row) and description
// lines are attached to the row above; without, any non-row line ends the
// list. Blank lines end the list.
//
// Descriptions are normally indented deeper than the row text, but pi's
// multi-select layout puts them at the same column (the "[ ] " checkbox
// prefix shifts the label, not the description), so a line at the row-text
// column counts as a description when the row it touches has a checkbox.
func collectRows(lines []string, i, c int, marker rune, numbered bool, commit []string) (rows []row, start, end int) {
	isRow := func(l string) (bool, string, bool) {
		active, text, ok := rowAt(l, c, marker)
		if !ok {
			return false, "", false
		}
		if numbered && !numberedRe.MatchString(text) && !slices.Contains(commit, text) {
			return false, "", false
		}
		return active, text, true
	}
	// isDesc reports a description line next to the row rowText: deeper
	// than the row text, or at its column under a checkbox row.
	isDesc := func(l, rowText string) bool {
		if !numbered {
			return false
		}
		in := indentOf(l)
		if in > c+2 {
			return true
		}
		return in == c+2 && rowHasCheckbox(rowText)
	}
	// lastRow is the closest row at or below start: the row a line above
	// start would describe.
	lastRow := ""
	if _, text, ok := isRow(lines[i]); ok {
		lastRow = text
	}
	start = i
	for start > 0 {
		l := lines[start-1]
		_, text, ok := isRow(l)
		if ok {
			lastRow = text
		}
		if ok || isDesc(l, lastRow) {
			start--
			continue
		}
		break
	}
	end = start
	for end < len(lines) {
		l := lines[end]
		if active, text, ok := isRow(l); ok {
			rw := row{line: end, active: active, text: text}
			if m := numberedRe.FindStringSubmatch(text); numbered && m != nil {
				rw.num, _ = strconv.Atoi(m[1])
				rw.text = strings.TrimSpace(m[2])
			}
			rows = append(rows, rw)
		} else if isDesc(l, descTarget(rows)) {
			if len(rows) > 0 {
				rows[len(rows)-1].desc = append(rows[len(rows)-1].desc, strings.TrimSpace(l))
			}
		} else {
			break
		}
		end++
	}
	if len(rows) > 0 {
		start = rows[0].line // orphan description-like lines above belong to the heading
	}
	return rows, start, end
}

// descTarget is the row a non-row line in the downward walk would attach
// to: the last collected row, or "" before the first one.
func descTarget(rows []row) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[len(rows)-1].text
}

// activeCount returns how many rows are highlighted and the index of the
// last one.
func activeIndex(rows []row) (int, int) {
	n, idx := 0, -1
	for i, r := range rows {
		if r.active {
			n++
			idx = i
		}
	}
	return n, idx
}

// window returns the trimmed lines in [from, to) clamped to the screen.
func window(lines []string, from, to int) []string {
	if from < 0 {
		from = 0
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from >= to {
		return nil
	}
	return lines[from:to]
}

func containsAny(lines []string, subs ...string) bool {
	for _, l := range lines {
		ll := strings.ToLower(l)
		for _, s := range subs {
			if strings.Contains(ll, strings.ToLower(s)) {
				return true
			}
		}
	}
	return false
}

// isRule reports a horizontal rule or box edge line.
func isRule(l string) bool {
	t := strings.TrimSpace(l)
	if len([]rune(t)) < 3 {
		return false
	}
	n := 0
	for _, ch := range t {
		switch ch {
		case '─', '━', '═', '╭', '╮', '╰', '╯', '┌', '┐', '└', '┘', '-', '▔', '▁':
			n++
		}
	}
	return n*10 >= len([]rune(t))*8
}

// nearestAbove returns the closest non-blank line above index i and its
// index, or "", -1.
func nearestAbove(lines []string, i int) (string, int) {
	for j := i - 1; j >= 0 && j >= i-4; j-- {
		if t := strings.TrimSpace(lines[j]); t != "" {
			if isRule(t) {
				return "", -1
			}
			return t, j
		}
	}
	return "", -1
}

// dedent trims blank edges and the common indentation of a block.
func dedent(block []string) string {
	for len(block) > 0 && strings.TrimSpace(block[0]) == "" {
		block = block[1:]
	}
	for len(block) > 0 && strings.TrimSpace(block[len(block)-1]) == "" {
		block = block[:len(block)-1]
	}
	if len(block) == 0 {
		return ""
	}
	minIn := -1
	for _, l := range block {
		if in := indentOf(l); in >= 0 && (minIn < 0 || in < minIn) {
			minIn = in
		}
	}
	out := make([]string, len(block))
	for i, l := range block {
		r := []rune(l)
		if len(r) >= minIn {
			out[i] = string(r[minIn:])
		}
	}
	return strings.Join(out, "\n")
}

// detailAbove collects up to maxDetailLines of context above line i, stopping
// at a rule/box edge or a run of blank lines.
func detailAbove(lines []string, i int, skip func(string) bool) string {
	var block []string
	blanks := 0
	for j := i - 1; j >= 0 && len(block) < maxDetailLines; j-- {
		l := lines[j]
		if isRule(l) {
			break
		}
		if strings.TrimSpace(l) == "" {
			blanks++
			if blanks >= 2 {
				break
			}
		} else {
			blanks = 0
		}
		if skip != nil && skip(l) {
			continue
		}
		block = append([]string{l}, block...)
	}
	return dedent(block)
}

// detectChevron handles every "❯ " list: Pi's permission gate (unnumbered,
// with its footer/heading) and numbered lists (Pi questionnaire, Claude
// Code permission and AskUserQuestion, other agents).
func detectChevron(lines []string, i, c int, agentKind string) *Dialog {
	_, text, _ := rowAt(lines[i], c, markerChevron)
	if numberedRe.MatchString(text) {
		return detectNumbered(lines, i, c, agentKind)
	}
	// The highlight may sit on the commit row ("❯ Submit"): the list is
	// still the numbered one above it.
	if isCommitLabel(text, agentKind) {
		if d := detectNumbered(lines, i, c, agentKind); d != nil {
			return d
		}
	}
	return detectPiGate(lines, i, c)
}

func detectPiGate(lines []string, i, c int) *Dialog {
	rows, start, end := collectRows(lines, i, c, markerChevron, false, nil)
	if len(rows) < 2 {
		return nil
	}
	if n, _ := activeIndex(rows); n != 1 {
		return nil
	}
	below := window(lines, end, end+4)
	header := -1
	for j := start - 1; j >= 0 && j >= start-contextAbove; j-- {
		if strings.Contains(strings.ToLower(lines[j]), "permission required") {
			header = j
			break
		}
	}
	if header < 0 && !containsAny(below, "esc block", "enter select") {
		return nil // a bare "❯" line pair, e.g. a prompt box: not a dialog
	}
	d := &Dialog{Kind: KindPiGate, Source: "screen", Title: "Permission required", Keys: KeysArrows, Answerable: true, CancelLabel: "Block"}
	_, d.Selected = activeIndex(rows)
	for _, r := range rows {
		d.Options = append(d.Options, Option{Label: r.text})
	}
	if header >= 0 {
		var block []string
		for _, l := range lines[header+1 : start] {
			t := strings.TrimSpace(l)
			// Transient lines: the speed-bump countdown disappears on the
			// first key, and the scroll indicator changes with scrolling.
			if strings.HasPrefix(t, "⏱") || strings.Contains(t, "···") {
				continue
			}
			block = append(block, l)
		}
		d.Detail = dedent(block)
	} else {
		d.Detail = detailAbove(lines, start, nil)
	}
	return d
}

// checkbox prefixes used by questionnaire multi-select rows.
var checkboxes = []struct {
	prefix  string
	checked bool
}{
	{"[ ] ", false}, {"[✔] ", true}, {"[✓] ", true}, {"[x] ", true}, {"[X] ", true},
	{"☐ ", false}, {"☒ ", true}, {"☑ ", true}, {"◯ ", false}, {"◉ ", true},
}

func splitCheckbox(label string) (string, *bool) {
	for _, cb := range checkboxes {
		if strings.HasPrefix(label, cb.prefix) {
			return strings.TrimSpace(strings.TrimPrefix(label, cb.prefix)), boolp(cb.checked)
		}
		// A bare checkbox ("[ ]") is the focused free-text row: the label
		// is gone, the box remains.
		if label == strings.TrimSpace(cb.prefix) {
			return "", boolp(cb.checked)
		}
	}
	return label, nil
}

const otherLabel = "Type something."

func detectNumbered(lines []string, i, c int, agentKind string) *Dialog {
	rows, start, end := collectRows(lines, i, c, markerChevron, true, commitRowLabels(agentKind))
	if len(rows) < 2 {
		return nil
	}
	nActive, sel := activeIndex(rows)
	if nActive != 1 {
		return nil
	}
	// Numbers must count up by one (a scrolled list may start above 1).
	prev := 0
	hasNext := false
	for k, r := range rows {
		if r.num == 0 {
			if !isCommitLabel(r.text, agentKind) || k != len(rows)-1 {
				return nil
			}
			hasNext = true
			continue
		}
		if prev != 0 && r.num != prev+1 {
			return nil
		}
		prev = r.num
	}
	d := &Dialog{Source: "screen", Selected: sel, Answerable: true, Keys: KeysArrows, CancelLabel: "Cancel"}
	hasOther, hasCheck, allCheck := false, false, true
	for _, r := range rows {
		o := Option{num: r.num}
		switch {
		case r.num == 0:
			o.Label, o.Role = r.text, RoleNext
		default:
			label := strings.TrimSuffix(r.text, " ✔") // previously confirmed answer mark
			label, o.Checked = splitCheckbox(label)
			if o.Checked != nil {
				hasCheck = true
			} else if !isOtherPlaceholder(label) {
				allCheck = false
			}
			if isOtherPlaceholder(label) {
				o.Role = RoleOther
				hasOther = true
			}
			o.Label = label
			o.Description = strings.Join(r.desc, " ")
		}
		d.Options = append(d.Options, o)
	}
	question, qi := nearestAbove(lines, start)
	d.Question = question
	below := window(lines, end, end+6)
	above := window(lines, start-contextAbove, start)
	tabBar := containsAny(above, "☐", "✔ Submit", "☒")
	switch agentKind {
	case "pi":
		d.Kind = KindPiQuestion
		if hasCheck {
			d.Multi = true
			// Toggling needs the commit row and a checkbox on every option.
			d.Answerable = hasNext && allCheck
		}
	case "claude":
		if hasOther || hasCheck || hasNext || tabBar || containsAny(below, "Tab/Arrow keys", "Enter to select") {
			d.Kind = KindClaudeQuestion
			if hasCheck || hasNext {
				// Claude's multi-select commit flow is not pinned down; use the pad.
				d.Multi, d.Answerable = true, false
			}
		} else {
			d.Kind = KindClaudePermission
			d.Keys = KeysDigits
			d.CancelLabel = "Esc"
			// Permission option labels wrap rather than carrying descriptions.
			for k := range d.Options {
				if d.Options[k].Description != "" {
					d.Options[k].Label += " " + d.Options[k].Description
					d.Options[k].Description = ""
				}
			}
			if qi >= 0 {
				d.Detail = detailAbove(lines, qi, nil)
			}
		}
	default:
		d.Kind = KindSelect
		if hasCheck || hasNext {
			d.Multi, d.Answerable = true, false
		}
	}
	return d
}

// detectPiSelect handles Pi's built-in selector: a "→ " list framed by rules
// with a "↑↓ navigate" footer.
func detectPiSelect(lines []string, i, c int) *Dialog {
	rows, start, end := collectRows(lines, i, c, markerArrow, false, nil)
	if len(rows) < 2 {
		return nil
	}
	if n, _ := activeIndex(rows); n != 1 {
		return nil
	}
	if !containsAny(window(lines, end, end+3), "navigate") {
		return nil
	}
	d := &Dialog{Kind: KindPiSelect, Source: "screen", Keys: KeysArrows, Answerable: true, CancelLabel: "Cancel"}
	_, d.Selected = activeIndex(rows)
	for _, r := range rows {
		d.Options = append(d.Options, Option{Label: r.text})
	}
	// The title sits between the top border and the list.
	top := start - 1
	for top >= 0 && top >= start-contextAbove && !isRule(lines[top]) {
		top--
	}
	title := dedent(window(lines, top+1, start))
	// A countdown suffix " (12s)" changes every second; keep it out of the
	// fingerprint.
	title = countdownRe.ReplaceAllString(title, "")
	if strings.Contains(title, "\n") {
		first, rest, _ := strings.Cut(title, "\n")
		d.Title, d.Detail = strings.TrimSpace(first), strings.TrimSpace(rest)
	} else {
		d.Title = title
	}
	return d
}

// detectConfirm recognizes a line prompt ending in [y/N] on the last line.
func detectConfirm(lines []string) *Dialog {
	if len(lines) == 0 {
		return nil
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if !confirmRe.MatchString(last) {
		return nil
	}
	return &Dialog{
		Kind: KindConfirm, Source: "screen", Question: last, Keys: KeysYN, Answerable: true, Selected: -1,
		Options: []Option{{Label: "Yes"}, {Label: "No"}},
	}
}
