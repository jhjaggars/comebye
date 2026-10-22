package dialog

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Activity is what a working agent's own status line says it is doing:
// Claude Code's spinner ("✻ Compacting conversation… (12s · ↓ 2.1k
// tokens)") or Pi's loader ("⠋ Compacting context... (esc to cancel)").
// The transcript cannot say this: compaction, retries and the current todo
// never reach the session file while they run.
type Activity struct {
	Text string `json:"text"`             // "Compacting conversation"
	Note string `json:"detail,omitempty"` // "↓ 2.1k tokens · thinking"
	// Generic is set for the placeholder verbs (Claude's random
	// "Shimmying…", Pi's "Working..."), which say nothing beyond "busy".
	Generic bool `json:"generic,omitempty"`
}

// activityScan is how many lines up from the bottom the status line may
// be: Claude Code draws the todo list, the input box and its status bar
// under it.
const activityScan = 40

var (
	// Claude Code: a spinner glyph, the text, "…", then an optional
	// parenthetical of elapsed time, token counts and key hints.
	claudeSpinner = regexp.MustCompile(`^\s*[·✢✳✶✻✽*]\s+(\S.*?)…\s*(?:\((.*)\))?\s*$`)
	// Pi: a braille spinner frame, the message, "..." and an optional
	// "(esc to cancel)".
	piSpinner = regexp.MustCompile(`^\s*[\x{2800}-\x{28FF}]\s+(\S.*?)(?:\.\.\.|…)\s*(?:\((.*)\))?\s*$`)
	// elapsed is a parenthetical part that only repeats the timer the app
	// already shows ("27s", "1m 5s").
	elapsed = regexp.MustCompile(`^(?:\d+h\s*)?(?:\d+m\s*)?(?:\d+s)?$`)
	// oneVerb is a lone "-ing" word: Claude's random spinner verbs.
	oneVerb = regexp.MustCompile(`^[A-Z][a-z]+ing$`)
)

// DetectActivity finds the agent's status line near the bottom of a visible
// screen. Returns nil when there is none (idle, or a harness it does not
// know).
func DetectActivity(screen, agentKind string) *Activity {
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	stop := max(0, len(lines)-activityScan)
	for i := len(lines) - 1; i >= stop; i-- {
		line := strings.TrimRight(lines[i], " ")
		re := claudeSpinner
		if agentKind == "pi" {
			re = piSpinner
		}
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		text := strings.TrimSpace(m[1])
		if text == "" || utf8.RuneCountInString(text) > 120 {
			continue
		}
		return &Activity{
			Text:    text,
			Note:    activityNote(m[2]),
			Generic: oneVerb.MatchString(text) || (agentKind == "pi" && text == "Working"),
		}
	}
	return nil
}

// activityNote keeps the informative parts of the parenthetical (token
// counts, "thinking") and drops the timer and key hints.
func activityNote(paren string) string {
	var keep []string
	for _, part := range strings.Split(paren, "·") {
		part = strings.TrimSpace(part)
		switch {
		case part == "", elapsed.MatchString(part),
			strings.Contains(part, "esc to"), strings.Contains(part, "ctrl+"), strings.Contains(part, "to cancel"):
			continue
		}
		keep = append(keep, part)
	}
	return strings.Join(keep, " · ")
}
