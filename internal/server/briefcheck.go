package server

import (
	"fmt"
	"regexp"
	"strings"
)

// The briefing check: a briefing's "Needs you" section may only list
// things that are active to-do items, each line carrying the item's id, so
// that the list the user reads and the To do panel cannot drift apart. It
// runs when a briefing is published (POST /api/brief), refuses a line with
// no live item behind it, and warns about open items the section leaves
// out. A briefing without that heading is not checked: its format is the
// user's.

var (
	briefHeadingRE  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	briefBoldHeadRE = regexp.MustCompile(`^\s{0,3}(?:\*\*|__)(.+?)(?:\*\*|__)\s*:?\s*$`)
	briefRuleRE     = regexp.MustCompile(`^\s{0,3}(-{3,}|\*{3,}|_{3,})\s*$`)
	briefNeedsRE    = regexp.MustCompile(`(?i)^needs\s+you\b.{0,30}$`)
	briefItemRE     = regexp.MustCompile(`^\s*(?:[-*+]|\d{1,3}[.)])\s+`)
	briefIDRE       = regexp.MustCompile(`\bt[0-9a-f]{10}\b`)
	// "Nothing open.", "None.", "No open questions", "All clear": a line that
	// says the section is empty.
	briefNothingRE = regexp.MustCompile(`(?i)^(nothing|none|nobody|no one|no open|no items|no questions|no decisions|n/a|all clear|empty)\b`)
)

// briefHeadingText returns the text of a heading line ("## Needs you",
// "**Needs you**") and whether the line is one.
func briefHeadingText(l string) (string, bool) {
	if m := briefHeadingRE.FindStringSubmatch(l); m != nil {
		return strings.Trim(m[1], "*_` "), true
	}
	if m := briefBoldHeadRE.FindStringSubmatch(l); m != nil && !strings.ContainsAny(m[1], "*_") {
		return strings.TrimSpace(m[1]), true
	}
	return "", false
}

// briefNeedsEntries returns the entries of the "Needs you" section: one
// per list item (with its wrapped lines) or per plain line. found is false
// when the briefing has no such heading.
func briefNeedsEntries(md string) (entries []string, found bool) {
	inSection, inCode := false, false
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			entries = append(entries, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, l := range strings.Split(strings.ReplaceAll(md, "\r", ""), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inCode = !inCode
			if inSection {
				flush()
			}
			continue
		}
		if inCode {
			continue
		}
		if h, ok := briefHeadingText(l); ok {
			if inSection {
				flush()
				inSection = false
			}
			if briefNeedsRE.MatchString(h) {
				inSection, found = true, true
			}
			continue
		}
		if !inSection {
			continue
		}
		if briefRuleRE.MatchString(l) {
			flush()
			inSection = false
			continue
		}
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			flush()
		case briefItemRE.MatchString(l):
			flush()
			cur = []string{t}
		case len(cur) > 0 && strings.HasPrefix(l, " "):
			cur = append(cur, t) // a wrapped line of the item above
		default:
			flush()
			cur = []string{t}
		}
	}
	flush()
	return entries, found
}

// checkBrief checks md against the items. fail is the refusal message ("" if
// the briefing passes); warns are notes to print after publishing.
func checkBrief(md string, items []Todo) (fail string, warns []string) {
	entries, found := briefNeedsEntries(md)
	if !found {
		return "", nil
	}
	byID := map[string]Todo{}
	for _, it := range items {
		byID[it.ID] = it
	}
	var open []Todo
	for _, it := range items {
		if it.State == TodoOpen {
			open = append(open, it)
		}
	}
	mentioned := map[string]bool{}
	var noID, stale []string
	for _, e := range entries {
		ids := briefIDRE.FindAllString(e, -1)
		short := oneLine(e, 70)
		if len(ids) == 0 {
			text := strings.TrimSpace(briefItemRE.ReplaceAllString(e, ""))
			text = strings.TrimSpace(strings.Trim(text, "*_`"))
			if len(items) == 0 && briefNothingRE.MatchString(text) {
				continue // "Nothing open." with nothing open
			}
			noID = append(noID, short)
			continue
		}
		live := false
		for _, id := range ids {
			if _, ok := byID[id]; ok {
				live, mentioned[id] = true, true
			}
		}
		if !live {
			stale = append(stale, short)
		}
	}
	if len(noID)+len(stale) > 0 {
		var sb strings.Builder
		sb.WriteString("the briefing was not published: its \"Needs you\" section must list only active to-do items, each line with the item's id (t plus 10 hex characters).")
		for _, l := range noID {
			fmt.Fprintf(&sb, "\n  no item id: %s", l)
		}
		for _, l := range stale {
			fmt.Fprintf(&sb, "\n  not an active item (done, dropped or unknown id): %s", l)
		}
		sb.WriteString("\nTo fix it: for each line that is a real question, run `comebye todo add \"the question\" --kind decision` and put the id it prints on the line; drop lines that are not for the user; run `comebye todo list` for the current ids.")
		if len(items) == 0 {
			sb.WriteString(" If nothing is open, write one line such as \"Nothing open.\".")
		}
		sb.WriteString(" Use --no-check to publish as is.")
		return sb.String(), nil
	}
	var missing []string
	for _, it := range open {
		if !mentioned[it.ID] {
			missing = append(missing, fmt.Sprintf("%s (%s)", it.ID, oneLine(it.Question, 60)))
		}
	}
	if len(missing) > 0 {
		warns = append(warns, fmt.Sprintf("%d open to-do item(s) are not in \"Needs you\": %s", len(missing), strings.Join(missing, "; ")))
	}
	return "", warns
}
