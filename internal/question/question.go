// Package question decides whether an agent's last message asks the user
// something that is waiting on an answer. It is a heuristic over prose, so
// it is tuned against a labelled corpus (testdata/corpus.jsonl) and not by
// feel; the corpus test reports precision and recall.
//
// The rules, in order:
//
//  1. A final code fence is ignored: the question is above it.
//  2. The last block is the last paragraph, with the list that follows a
//     question ("Which one? / - A / - B") counted as part of it.
//  3. A block that is a quotation is not the agent asking.
//  4. A sentence ends in a question mark (not one inside code or a URL).
//  5. A question counts when it offers or asks permission ("should I",
//     "want me to"), asks the user to choose ("which", "A or B?") or is
//     addressed to the user ("you"); closers such as "Anything else?" and
//     "What would you like to work on next?" do not count.
package question

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Result is the verdict for one message.
type Result struct {
	Ask bool
	// Block is the paragraph that holds the question (cut to MaxBlock
	// bytes), the context for the item.
	Block string
	// Question is the question sentence itself (the last one), one line.
	Question string
}

// MaxBlock caps Result.Block.
const MaxBlock = 1500

var (
	fenceLine  = regexp.MustCompile("(?m)^\\s*(```|~~~)")
	listLine   = regexp.MustCompile(`^\s*([-*+•]|\d{1,2}[.)])\s+\S`)
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	urlRE      = regexp.MustCompile(`https?://\S+`)
	// A sentence boundary: ., !, ? or a newline followed by space.
	sentenceEnd = regexp.MustCompile(`[.!?:]["')\]*_]*(\s+|$)|\n`)

	askCue = regexp.MustCompile(`(?i)\b(should (i|we)|shall (i|we)|can (i|we)|may i|do you want|would you (like|prefer|rather)|want me to|want us to|like me to|ok(ay)? (to|if)|is (it|that) ok|(do|did) you (mean|prefer|intend|want)|which (one|of|do|should|would|client|option|approach|way|branch|environment|host)|which\b|what (should|do you|did you|would you|is your|are your)|where (should|do you)|how (should|would|do you)|or (should|would|do) (i|you)|let me know (which|whether|if you (want|prefer|would)))\b`)
	closer = regexp.MustCompile(`(?i)\b(anything else|any(thing)? other|any (more |other |further )?questions|need anything|what else|how can i help|what would you like to (work on|do|tackle|focus on) next|what('s| is) next|what next|something else|help with anything|else i can)\b`)
	you    = regexp.MustCompile(`(?i)\b(you|your|you're|you'd|you've|you'll)\b`)
	choice = regexp.MustCompile(`(?i)\bor\b`)
	// The agent says no answer is wanted.
	noAnswer = regexp.MustCompile(`(?i)(nothing (is )?(needed|required) from you|no (answer|action|reply|response|decision) (is )?(needed|required)|no need (to|for you to) (answer|reply|respond)|for later;|rhetorical)`)
)

// Classify reports whether text ends on a question for the user.
func Classify(text string) Result {
	text = stripFinalFence(strings.TrimSpace(text))
	block := lastBlock(text)
	if block == "" || quoted(block) || noAnswer.MatchString(block) {
		return Result{}
	}
	scan := inlineCode.ReplaceAllString(urlRE.ReplaceAllString(block, " "), " ")
	var qs []string
	for _, s := range sentences(scan) {
		if strings.HasSuffix(strings.TrimRight(strings.TrimSpace(s), `"')]*_`), "?") {
			qs = append(qs, strings.TrimSpace(s))
		}
	}
	if len(qs) == 0 {
		return Result{}
	}
	var live []string
	for _, q := range qs {
		if !closer.MatchString(q) {
			live = append(live, q)
		}
	}
	for i := len(live) - 1; i >= 0; i-- {
		q := live[i]
		if askCue.MatchString(q) || you.MatchString(q) || (i == len(live)-1 && choice.MatchString(q)) {
			return Result{Ask: true, Block: clip(block, MaxBlock), Question: oneLine(q, 240)}
		}
	}
	return Result{}
}

// stripFinalFence drops a code fence that closes the message.
func stripFinalFence(s string) string {
	for {
		s = strings.TrimSpace(s)
		if !strings.HasSuffix(s, "```") && !strings.HasSuffix(s, "~~~") {
			return s
		}
		locs := fenceLine.FindAllStringIndex(s, -1)
		if len(locs) < 2 || len(locs)%2 == 1 {
			return s // unbalanced: leave it
		}
		s = s[:locs[len(locs)-2][0]]
	}
}

// lastBlock is the last paragraph, joined with the paragraph before it
// when the last one is only a list (the options of a question).
func lastBlock(s string) string {
	paras := regexp.MustCompile(`\n[ \t]*\n`).Split(s, -1)
	var keep []string
	for _, p := range paras {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, strings.TrimSpace(p))
		}
	}
	if len(keep) == 0 {
		return ""
	}
	i := len(keep) - 1
	for steps := 0; i > 0 && isList(keep[i]) && steps < 2; steps++ {
		i--
	}
	return strings.Join(keep[i:], "\n\n")
}

func isList(p string) bool {
	for _, l := range strings.Split(p, "\n") {
		if strings.TrimSpace(l) != "" && !listLine.MatchString(l) {
			return false
		}
	}
	return true
}

func quoted(p string) bool {
	for _, l := range strings.Split(p, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, ">") {
			return false
		}
	}
	return true
}

func sentences(s string) []string {
	var out []string
	last := 0
	for _, m := range sentenceEnd.FindAllStringIndex(s, -1) {
		out = append(out, s[last:m[1]])
		last = m[1]
	}
	if last < len(s) {
		out = append(out, s[last:])
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, n)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) { // do not cut a rune
		s = s[:len(s)-1]
	}
	return s
}
