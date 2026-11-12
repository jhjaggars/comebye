package dialog

import (
	"regexp"
	"strings"
)

// OpenCode's dialogs.
//
// Permission prompt (a tool wants something outside its allow list):
//
//	┃  △ Permission required
//	┃    ← Access external directory /var/tmp
//	┃  Patterns
//	┃  - /var/tmp/*
//	┃   Allow once   Allow always   Reject         ctrl+f fullscreen ⇆ select enter confirm
//
// The buttons sit on one row; the selected one is drawn with a different
// background, which only the styled (ANSI) screen shows. ←/→ move the
// selection (and wrap), Enter presses it, Esc rejects. "Allow always" opens
// a second prompt ("△ Always allow", Confirm / Cancel) where Esc goes back.
//
// Question (the question tool): a numbered list in the same bordered panel,
// the question above it, "↑↓ select  enter submit  esc dismiss" below, the
// selected row again told by its background.
//
// The text screen alone recognises both (so the agent counts as blocked and
// the options are listed); only the styled screen says which row is
// selected, and with it the dialog can be answered.

const (
	// KindOpencodePermission is OpenCode's permission prompt (and the
	// Confirm / Cancel prompt "Allow always" opens): one row of buttons,
	// ←/→ then Enter, Esc rejects.
	KindOpencodePermission = "opencode_permission"
	// KindOpencodeQuestion is OpenCode's question tool: a numbered list,
	// ↑/↓ then Enter, Esc dismisses.
	KindOpencodeQuestion = "opencode_question"
)

var (
	opencodePermButtons = []string{"Allow once", "Allow always", "Reject"}
	opencodeConfirmBtns = []string{"Confirm", "Cancel"}
	opencodeQuestionFt  = regexp.MustCompile(`↑↓ select\s+enter submit`)
	multiSpace          = regexp.MustCompile(`\s{4,}`)
	opencodeNumbered    = regexp.MustCompile(`^(\d{1,2})\.\s+(.+)$`)
)

// cell is one screen cell: its rune and the background it is drawn on.
type cell struct {
	r  rune
	bg string
}

// styledCells parses one line of ANSI text into cells, tracking the
// background only. A plain line has an empty background everywhere.
func styledCells(l string) []cell {
	var out []cell
	bg := ""
	rs := []rune(strings.TrimRight(l, "\r"))
	for i := 0; i < len(rs); i++ {
		if rs[i] != 0x1b {
			out = append(out, cell{rs[i], bg})
			continue
		}
		if i+1 >= len(rs) {
			break
		}
		switch rs[i+1] {
		case '[':
			j := i + 2
			for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				fs := strings.Split(string(rs[i+2:j]), ";")
				for k := 0; k < len(fs); k++ {
					switch fs[k] {
					case "", "0", "49":
						bg = ""
					case "38", "58":
						if k+1 < len(fs) && fs[k+1] == "5" {
							k += 2
						} else if k+1 < len(fs) && fs[k+1] == "2" {
							k += 4
						}
					case "48":
						switch {
						case k+1 < len(fs) && fs[k+1] == "5" && k+2 < len(fs):
							bg = strings.Join(fs[k:k+3], ";")
							k += 2
						case k+1 < len(fs) && fs[k+1] == "2" && k+4 < len(fs):
							bg = strings.Join(fs[k:k+5], ";")
							k += 4
						}
					}
				}
			}
			i = j
		case ']':
			j := i + 2
			for j < len(rs) && rs[j] != 0x07 && !(rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\') {
				j++
			}
			if j < len(rs) && rs[j] == 0x1b {
				j++
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// opencodeLine is one screen line: its text with the panel's border and
// the right-hand sidebar left out, and the cells it was read from.
type opencodeLine struct {
	text  string // trimmed content
	body  string // the text after the border, untrimmed
	cells []cell
	start int // cell index where text starts
}

// opencodeLines reads a screen (styled or plain) into lines. Each line's
// text starts after the "┃" border when it has one; lines without the
// border keep their whole text, so they can still end a panel.
func opencodeLines(screen string) []opencodeLine {
	raw := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	out := make([]opencodeLine, len(raw))
	for i, l := range raw {
		cs := styledCells(l)
		var sb strings.Builder
		for _, c := range cs {
			sb.WriteRune(c.r)
		}
		full := sb.String()
		start := 0
		if rest, ok := strings.CutPrefix(strings.TrimLeft(full, " "), "┃"); ok {
			start = len([]rune(full)) - len([]rune(rest))
		}
		body := string([]rune(full)[start:])
		out[i] = opencodeLine{cells: cs, start: start, body: body, text: strings.TrimSpace(body)}
	}
	return out
}

// content cuts the sidebar and footer hints off a line's text: they sit
// after a wide gap.
func (l opencodeLine) content() string {
	// Nothing near the border: what follows is the sidebar's.
	if len(l.body)-len(strings.TrimLeft(l.body, " ")) >= 12 {
		return ""
	}
	t := l.text
	if loc := multiSpace.FindStringIndex(t); loc != nil {
		t = t[:loc[0]]
	}
	return strings.TrimSpace(t)
}

// selectedAmong returns which of the labels, found in the line at the given
// rune columns, is drawn on a background unlike the gap after the last one.
// -1 when the screen has no styling or nothing stands out.
func selectedAmong(l opencodeLine, cols []int, widths []int) int {
	n := len(cols)
	if n == 0 {
		return -1
	}
	at := func(c int) (string, bool) {
		if c < 0 || c >= len(l.cells) {
			return "", false
		}
		return l.cells[c].bg, true
	}
	ref, ok := at(cols[n-1] + widths[n-1] + 2)
	if !ok {
		return -1
	}
	sel := -1
	for k := range cols {
		bg, ok := at(cols[k])
		if !ok {
			return -1
		}
		if bg != ref {
			if sel >= 0 {
				return -1 // two stand out: not a highlight
			}
			sel = k
		}
	}
	return sel
}

func detectOpencode(screen string) *Dialog {
	lines := opencodeLines(screen)
	if d := detectOpencodePermission(lines); d != nil {
		return d
	}
	return detectOpencodeQuestion(lines)
}

func detectOpencodePermission(lines []opencodeLine) *Dialog {
	btn, labels := -1, []string(nil)
	for i := len(lines) - 1; i >= 0 && btn < 0; i-- {
		toks := strings.Split(strings.TrimSpace(lines[i].text), "  ")
		var run []string
		for _, t := range toks {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			run = append(run, t)
		}
		for _, set := range [][]string{opencodePermButtons, opencodeConfirmBtns} {
			if len(run) >= len(set) && equalStrings(run[:len(set)], set) {
				btn, labels = i, set
			}
		}
	}
	if btn < 0 {
		return nil
	}
	// The title: a "△" line above the buttons.
	title := -1
	for i := btn - 1; i >= 0 && i >= btn-30; i-- {
		if strings.HasPrefix(lines[i].text, "△") {
			title = i
			break
		}
	}
	if title < 0 {
		return nil
	}
	d := &Dialog{
		Kind: KindOpencodePermission, Source: "screen",
		Title:    strings.TrimSpace(strings.TrimPrefix(lines[title].content(), "△")),
		Selected: -1, Keys: KeysArrows,
	}
	var detail []string
	for i := title + 1; i < btn; i++ {
		if c := lines[i].content(); c != "" {
			detail = append(detail, c)
		}
	}
	if len(detail) > 0 {
		d.Question = strings.TrimSpace(strings.TrimPrefix(detail[0], "←"))
		d.Detail = strings.Join(detail[1:], "\n")
	}
	for _, l := range labels {
		d.Options = append(d.Options, Option{Label: l})
	}
	if labels[0] == "Confirm" {
		d.CancelLabel = "Back"
	} else {
		d.CancelLabel = "Reject"
	}
	// Where the buttons are on the line, to tell the selected one.
	runes := []rune(lines[btn].cells2string())
	var cols, widths []int
	from := lines[btn].start
	for _, l := range labels {
		w := len([]rune(l))
		idx := indexRunes(runes, []rune(l), from)
		if idx < 0 {
			cols = nil
			break
		}
		cols, widths = append(cols, idx), append(widths, w)
		from = idx + w
	}
	if cols != nil {
		if s := selectedAmong(lines[btn], cols, widths); s >= 0 {
			d.Selected, d.Answerable = s, true
		}
	}
	return d
}

func detectOpencodeQuestion(lines []opencodeLine) *Dialog {
	foot := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if opencodeQuestionFt.MatchString(lines[i].text) {
			foot = i
			break
		}
	}
	if foot < 0 {
		return nil
	}
	type orow struct {
		line int
		num  int
		text string
	}
	var rows []orow
	for i := foot - 1; i >= 0 && i >= foot-60; i-- {
		m := opencodeNumbered.FindStringSubmatch(lines[i].content())
		if m == nil {
			continue
		}
		n := 0
		for _, ch := range m[1] {
			n = n*10 + int(ch-'0')
		}
		rows = append([]orow{{i, n, m[2]}}, rows...)
		if n == 1 {
			break
		}
	}
	if len(rows) < 2 || rows[0].num != 1 {
		return nil
	}
	for k, r := range rows {
		if r.num != k+1 {
			return nil
		}
	}
	// The question: the last non-blank line above the first row.
	q := ""
	for i := rows[0].line - 1; i >= 0 && i >= rows[0].line-6; i-- {
		if c := lines[i].content(); c != "" {
			q = c
			break
		}
	}
	d := &Dialog{
		Kind: KindOpencodeQuestion, Source: "screen", Question: q, Selected: -1, Keys: KeysArrows,
		CancelLabel: "Dismiss",
	}
	var cols, widths []int
	for _, r := range rows {
		o := Option{Label: r.text, num: r.num}
		// The description is the indented line under the row.
		for j := r.line + 1; j < foot && j <= r.line+1; j++ {
			c := lines[j].content()
			if c != "" && !opencodeNumbered.MatchString(c) {
				o.Description = c
			}
		}
		if strings.EqualFold(r.text, "Type your own answer") {
			o.Role = RoleOther
		}
		d.Options = append(d.Options, o)
		runes := []rune(lines[r.line].cells2string())
		c := indexRunes(runes, []rune(r.text), lines[r.line].start)
		cols, widths = append(cols, c), append(widths, len([]rune(r.text)))
	}
	// Selected: the row whose label is on a different background from the
	// gap after the labels. Rows are on separate lines, so compare each
	// with its own line's trailing filler.
	sel, ok := -1, true
	for k, r := range rows {
		l := lines[r.line]
		if cols[k] < 0 || cols[k]+widths[k]+2 >= len(l.cells) {
			ok = false
			break
		}
		if l.cells[cols[k]].bg != l.cells[cols[k]+widths[k]+2].bg {
			if sel >= 0 {
				ok = false
				break
			}
			sel = k
		}
	}
	if ok && sel >= 0 {
		d.Selected, d.Answerable = sel, true
	}
	return d
}

func (l opencodeLine) cells2string() string {
	var sb strings.Builder
	for _, c := range l.cells {
		sb.WriteRune(c.r)
	}
	return sb.String()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func indexRunes(hay, needle []rune, from int) int {
	for i := max(from, 0); i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}
