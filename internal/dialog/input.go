package dialog

import (
	"regexp"
	"strings"
	"unicode"
)

// Input is what an agent's prompt box holds: whether the user has begun
// typing a message there. It is read from the screen, because neither
// herdr nor the agents say (herdr reports no input or cursor state and no
// keystroke times).
type Input struct {
	// Known is false when the screen is not one this package can read the
	// box from: an agent kind it does not know, or no box on screen. Busy
	// and Text mean nothing then.
	Known bool
	// Busy is true when the box holds anything the user (or a half-sent
	// message) put there: text, a pasted-text chip, a ! command.
	Busy bool
	// Text is the start of what the box holds, for messages and logs.
	Text string
}

// DetectInput reads the prompt box from a screen. screen should be the
// visible screen *with* its ANSI styling when it can be had: Claude Code
// draws its placeholder ("Try "fix typecheck errors"") and prompt
// suggestions dimmed, which is the only way to tell them from typed text.
// A plain-text screen works too (the known placeholder shape is then
// recognised by its words).
//
// Claude Code: the box is the lines between the last two rules, the first
// starting with the ❯ prompt (a pink ! in bash mode). Pi: the editor is the
// lines between the last two rules, with no prompt glyph. The cursor, which
// both draw as a reverse-video blank, never counts as text. Vim mode makes
// no difference: normal and insert mode show the same text.
func DetectInput(screen, agentKind string) Input {
	if agentKind == "opencode" {
		return detectOpencodeInput(screen)
	}
	if agentKind != "claude" && agentKind != "pi" {
		return Input{}
	}
	raw := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	plain := make([]string, len(raw))
	live := make([]string, len(raw))
	styled := false
	for i, l := range raw {
		var s bool
		plain[i], live[i], s = splitStyled(l)
		styled = styled || s
	}
	b, a := -1, -1
	for i := len(plain) - 1; i >= 0; i-- {
		if !isInputRule(plain[i]) {
			continue
		}
		if b < 0 {
			b = i
		} else {
			a = i
			break
		}
	}
	if a < 0 || b-a < 2 {
		return Input{}
	}
	box := live[a+1 : b]
	if agentKind == "claude" {
		first := strings.TrimLeftFunc(plain[a+1], unicode.IsSpace)
		switch {
		case strings.HasPrefix(first, "❯"):
			box = append([]string{strings.TrimPrefix(strings.TrimLeftFunc(box[0], unicode.IsSpace), "❯")}, box[1:]...)
		case strings.HasPrefix(first, "!"):
			// Bash mode: the user pressed ! and is typing a command.
			return Input{Known: true, Busy: true, Text: clip(strings.TrimSpace(strings.TrimPrefix(first, "!")))}
		default:
			return Input{}
		}
	}
	text := strings.TrimSpace(strings.Join(box, "\n"))
	if !styled && claudePlaceholder.MatchString(text) && agentKind == "claude" {
		text = ""
	}
	return Input{Known: true, Busy: text != "", Text: clip(text)}
}

// claudePlaceholder is the empty box's hint in a screen without styling.
var claudePlaceholder = regexp.MustCompile(`^Try "[^"\n]*"$`)

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

// isInputRule reports a full-width rule of ─ that bounds the prompt box.
func isInputRule(l string) bool {
	t := strings.TrimSpace(l)
	n := 0
	for _, r := range t {
		if r == '─' || r == '━' {
			n++
		}
	}
	return n >= 10 && n*10 >= len([]rune(t))*9
}

// splitStyled removes ANSI escapes from one screen line. plain keeps every
// character; live leaves out the dimmed ones (SGR 2, until 22 or a reset).
// styled reports whether the line carried any escape at all.
func splitStyled(l string) (plain, live string, styled bool) {
	var p, v strings.Builder
	dim := false
	rs := []rune(l)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r != 0x1b {
			p.WriteRune(r)
			if !dim {
				v.WriteRune(r)
			}
			continue
		}
		styled = true
		if i+1 >= len(rs) {
			break
		}
		switch rs[i+1] {
		case '[': // CSI: parameters, then a final byte
			j := i + 2
			for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				fs := strings.Split(string(rs[i+2:j]), ";")
				for k := 0; k < len(fs); k++ {
					switch fs[k] {
					case "", "0", "22":
						dim = false
					case "2":
						dim = true
					case "38", "48", "58": // a colour: skip its 5;n or 2;r;g;b operands
						if k+1 < len(fs) && fs[k+1] == "5" {
							k += 2
						} else if k+1 < len(fs) && fs[k+1] == "2" {
							k += 4
						}
					}
				}
			}
			i = j
		case ']': // OSC: up to BEL or ESC \
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
	return p.String(), v.String(), styled
}

// detectOpencodeInput reads OpenCode's prompt box: the "┃" lines just above
// the mode line ("Build · Big Pickle OpenCode"), which sits above the "╹▀▀▀"
// rule that closes the box. A blank box is blank (no placeholder to tell
// apart), so the styling is not needed.
func detectOpencodeInput(screen string) Input {
	raw := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	plain := make([]string, len(raw))
	for i, l := range raw {
		plain[i], _, _ = splitStyled(l)
	}
	border := func(l string) (string, bool) {
		return strings.CutPrefix(strings.TrimLeftFunc(l, unicode.IsSpace), "┃")
	}
	mode := -1
	for i := len(plain) - 2; i >= 0; i-- {
		body, ok := border(plain[i])
		if ok && opencodeModeLine.MatchString(body) &&
			strings.HasPrefix(strings.TrimLeftFunc(plain[i+1], unicode.IsSpace), "╹▀▀") {
			mode = i
			break
		}
	}
	if mode < 0 {
		return Input{}
	}
	var lines []string
	for i := mode - 1; i >= 0; i-- {
		body, ok := border(plain[i])
		if !ok {
			break
		}
		// The sidebar shares these rows: it starts after a wide gap.
		if len(body)-len(strings.TrimLeft(body, " ")) >= 12 {
			body = ""
		} else if loc := wideGap.FindStringIndex(body); loc != nil {
			body = body[:loc[0]]
		}
		lines = append([]string{body}, lines...)
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if opencodePlaceholder.MatchString(text) {
		text = "" // a new session's hint, drawn grey, not text anyone typed
	}
	return Input{Known: true, Busy: text != "", Text: clip(text)}
}

// opencodePlaceholder is the empty box's hint in a new session
// (Ask anything… "What is the tech stack of this project?").
var opencodePlaceholder = regexp.MustCompile(`^Ask anything… "[^"\n]*"$`)

// opencodeModeLine is the box's last line: the agent mode, the model and
// "OpenCode", with the sidebar's text possibly after a wide gap.
var opencodeModeLine = regexp.MustCompile(`\S+ OpenCode(\s{3,}.*)?$`)

var wideGap = regexp.MustCompile(`\s{8,}`)
