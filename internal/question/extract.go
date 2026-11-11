package question

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Extract finds every question or decision an agent's last message leaves
// for the user, one Item each, with the agent's own wording, the choices it
// offered and the option it recommends. Classify answers "is there a
// question"; Extract answers "which ones, and what were the choices".
//
// The shapes it reads, in order of confidence:
//
//  1. A "Decisions for the user" / "Questions" heading (any level, or a bold
//     line): every top-level numbered or bulleted item under it is one item,
//     and so is a question asked in a paragraph after the list.
//  2. A numbered or bulleted list at the end of the message in which at
//     least half the items (and two or more) ask something.
//  3. The last paragraph (Classify's block): one item per question
//     sentence; a list below the question is its options.
//
// Options are a nested numbered, lettered or bulleted list under an item
// (or the list under a lone question); "Recommend: N", "My recommendation:
// ok N" or "(recommended)" on an option is the recommendation. They are
// returned as fields and left out of Detail.
type Item struct {
	// Question is the question, one line.
	Question string
	// Detail is the agent's wording around it (the rest of the bullet, or
	// the paragraph), Markdown, without the option list.
	Detail    string
	Options   []Option
	Recommend []int
}

// Option is one choice.
type Option struct{ Label, Detail string }

// Extraction is what Extract found.
type Extraction struct {
	Items []Item
	// Lead is what the agent said just before the questions (the last
	// paragraphs above the list or heading), cut short; "" when the
	// questions are the message.
	Lead string
}

const (
	// MaxItems caps one extraction.
	MaxItems = 6
	// leadMax caps Extraction.Lead.
	leadMax     = 600
	questionMax = 240
)

var (
	sectionName = regexp.MustCompile(`(?i)^(decisions?( (for|needed from|from) (the )?(user|you))?|decisions? (i need|to make|needed)|(open |my |a few )?questions?( (for|to) (the )?(user|you))?|questions? (i have|before i (go on|continue|proceed))|needs? your (input|decisions?)|for you to decide)$`)
	markerRE    = regexp.MustCompile(`^( {0,1})(?:[-*+•]|\d{1,2}[.)])\s+(.*\S.*)$`)
	nestedRE    = regexp.MustCompile(`^(?:\s{2,}|\t)(.*\S.*)$`)
	optRE       = regexp.MustCompile(`^(?:\d{1,2}[.)]|[a-zA-Z][.)]|[-*+•])\s+(.*\S.*)$`)
	recRE       = regexp.MustCompile(`(?i)^(?:my\s+)?recommend(?:ation|ed)?\s*[:=-]?\s*(?:is\s+)?(?:ok\s+|option\s+)?(\d+(?:\s*(?:\+|,|&|and)\s*\d+)*)\b.*$`)
	recMarkRE   = regexp.MustCompile(`(?i)\s*[(\[]\s*recommended\s*[)\]]\s*$`)
	noneRE      = regexp.MustCompile(`(?i)^(none|nothing|no decisions?|no questions?|n/a)\b[\s.!]*$`)
	headingRE   = regexp.MustCompile(`^#{1,6}\s`)
	leadOrRE    = regexp.MustCompile(`^(?i:or|and|also|plus|one more thing:?)\s+`)
	boldLabelRE = regexp.MustCompile(`^\*\*([^*]+)\*\*\s*[-–—:]?\s*(.*)$`)
	numRE       = regexp.MustCompile(`\d+`)
)

type rawItem struct {
	head string
	body []string
}

type line struct {
	text  string
	fence bool // inside (or a boundary of) a code fence
}

// Extract reads text; see Item.
func Extract(text string) Extraction {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, " "), "\r", "")
	text = stripFinalFence(strings.TrimSpace(text))
	if text == "" {
		return Extraction{}
	}
	lines := splitLines(text)
	// 1. A named section.
	if h := lastHeading(lines); h >= 0 {
		end := len(lines)
		for i := h + 1; i < len(lines); i++ {
			if !lines[i].fence && headingRE.MatchString(lines[i].text) {
				end = i
				break
			}
		}
		if ex, ok := fromRegion(lines, h+1, end, true); ok {
			ex.Lead = leadOf(lines[:h])
			return ex
		}
		if sectionIsNone(lines[h+1 : end]) {
			return Extraction{}
		}
	}
	// 2. A list of questions at the end.
	if ex, ok := fromRegion(lines, 0, len(lines), false); ok {
		return ex
	}
	// 3. The last paragraph.
	return fromBlock(text)
}

func splitLines(text string) []line {
	var out []line
	fence := false
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			out = append(out, line{l, true})
			fence = !fence
			continue
		}
		out = append(out, line{l, fence})
	}
	return out
}

// plain strips Markdown emphasis and heading marks from a heading line.
func plain(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "#*_:. \t"))
}

func lastHeading(lines []line) int {
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if l.fence || len(strings.TrimSpace(l.text)) > 60 {
			continue
		}
		if p := plain(l.text); p != "" && sectionName.MatchString(p) {
			return i
		}
	}
	return -1
}

func sectionIsNone(ls []line) bool {
	var parts []string
	for _, l := range ls {
		if t := strings.TrimSpace(l.text); t != "" {
			parts = append(parts, t)
		}
	}
	return len(parts) == 0 || noneRE.MatchString(plain(strings.Join(parts, " ")))
}

// leadOf is the last two paragraphs of ls, cut short.
func leadOf(ls []line) string {
	var paras []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			paras = append(paras, strings.TrimSpace(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	for _, l := range ls {
		if strings.TrimSpace(l.text) == "" {
			flush()
			continue
		}
		cur = append(cur, l.text)
	}
	flush()
	if len(paras) > 2 {
		paras = paras[len(paras)-2:]
	}
	s := strings.Join(paras, "\n\n")
	if r := []rune(s); len(r) > leadMax {
		s = "…" + string(r[len(r)-leadMax:])
	}
	return s
}

// fromRegion reads lines[from:to] as a list of questions. headed: the
// region is under a named section, so every item counts and any question
// sentence will do; otherwise the last list must look like questions.
func fromRegion(lines []line, from, to int, headed bool) (Extraction, bool) {
	type run struct {
		items      []*rawItem
		start, end int // line span
	}
	var runs []run
	var cur *run
	blank := false
	closeRun := func(end int) {
		if cur != nil {
			cur.end = end
			runs = append(runs, *cur)
			cur = nil
		}
	}
	for i := from; i < to; i++ {
		l := lines[i]
		t := strings.TrimSpace(l.text)
		if t == "" {
			blank = true
			continue
		}
		if !l.fence {
			if m := markerRE.FindStringSubmatch(l.text); m != nil {
				if cur == nil {
					cur = &run{start: i}
				}
				cur.items = append(cur.items, &rawItem{head: strings.TrimSpace(m[2])})
				blank = false
				continue
			}
		}
		if cur != nil {
			last := cur.items[len(cur.items)-1]
			nested := nestedRE.MatchString(l.text)
			switch {
			case nested || (!blank && !headingRE.MatchString(l.text)):
				// Nested under the item, or a lazy continuation of it.
				if nested {
					last.body = append(last.body, l.text)
				} else {
					last.head += " " + t
				}
				blank = false
				continue
			default:
				closeRun(i)
			}
		}
		blank = false
	}
	closeRun(to)
	if len(runs) == 0 {
		return Extraction{}, false
	}
	var chosen []run
	if headed {
		chosen = runs
	} else {
		r := runs[len(runs)-1]
		asking := 0
		for _, it := range r.items {
			if hasQuestion(it.head) {
				asking++
			}
		}
		if len(r.items) < 2 || asking < 2 || asking*2 < len(r.items) {
			return Extraction{}, false
		}
		chosen = []run{r}
	}
	var ex Extraction
	seen := map[string]bool{}
	add := func(it Item) {
		k := Key(it.Question)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		ex.Items = append(ex.Items, it)
	}
	for _, r := range chosen {
		for _, ri := range r.items {
			add(buildItem(ri))
		}
	}
	// The recommendation of a list of one, written below it.
	last := chosen[len(chosen)-1]
	if len(chosen) == 1 && len(ex.Items) == 1 && ex.Items[0].Recommend == nil {
		for _, l := range lines[last.end:to] {
			if m := recRE.FindStringSubmatch(plain2(l.text)); m != nil {
				ex.Items[0].Recommend = recNumbers(m[1], len(ex.Items[0].Options))
			}
		}
	}
	// Questions asked in the paragraphs after the list.
	for _, p := range paragraphs(lines[last.end:to]) {
		if isRecLine(p) || quoted(p) {
			continue
		}
		for _, q := range questionsIn(p, headed) {
			add(Item{Question: oneLine(leadOrRE.ReplaceAllString(q, ""), questionMax), Detail: p})
		}
	}
	if len(ex.Items) == 0 {
		return Extraction{}, false
	}
	if !headed {
		ex.Lead = leadOf(lines[:last.start])
	}
	capItems(&ex)
	return ex, true
}

func capItems(ex *Extraction) {
	if len(ex.Items) > MaxItems {
		n := len(ex.Items) - MaxItems
		ex.Items = ex.Items[:MaxItems]
		last := &ex.Items[MaxItems-1]
		last.Detail = strings.TrimSpace(last.Detail + "\n\n" + strconv.Itoa(n) + " more question(s) in the message were not filed: read the message.")
	}
}

func paragraphs(ls []line) []string {
	var out, cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	for _, l := range ls {
		if strings.TrimSpace(l.text) == "" {
			flush()
		} else {
			cur = append(cur, l.text)
		}
	}
	flush()
	return out
}

func hasQuestion(s string) bool {
	scan := inlineCode.ReplaceAllString(urlRE.ReplaceAllString(s, " "), " ")
	return strings.Contains(scan, "?")
}

// plain2 drops emphasis marks around a line so "**Recommend:** 2" reads as
// "Recommend: 2".
func plain2(s string) string {
	s = strings.NewReplacer("**", "", "__", "").Replace(strings.TrimSpace(s))
	return strings.Trim(s, "_* ")
}

func isRecLine(p string) bool {
	return recRE.MatchString(plain2(p))
}

func recNumbers(s string, n int) []int {
	var out []int
	for _, f := range numRE.FindAllString(s, -1) {
		v, _ := strconv.Atoi(f)
		if v >= 1 && v <= n {
			out = append(out, v)
		}
	}
	return out
}

// buildItem makes an Item of one top-level list item.
func buildItem(ri *rawItem) Item {
	head := strings.NewReplacer("**", "", "__", "").Replace(strings.TrimSpace(ri.head))
	var opts []Option
	var rec []int
	var notes []string
	fenceOpen := false
	for _, l := range ri.body {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenceOpen = !fenceOpen
			notes = append(notes, t)
			continue
		}
		if fenceOpen {
			notes = append(notes, l)
			continue
		}
		if m := recRE.FindStringSubmatch(plain2(t)); m != nil {
			rec = append(rec, numRE2(m[1])...)
			continue
		}
		if m := optRE.FindStringSubmatch(t); m != nil {
			opts = append(opts, splitOption(m[1]))
			continue
		}
		notes = append(notes, t)
	}
	if len(opts) < 2 || len(opts) > 9 {
		// Not a choice (or too many to be one): keep the lines as text.
		for _, l := range ri.body {
			if optRE.MatchString(strings.TrimSpace(l)) {
				notes = append(notes, strings.TrimSpace(l))
			}
		}
		opts, rec = nil, nil
	}
	for i := range opts {
		if m := recMarkRE.FindStringIndex(opts[i].Label + " " + opts[i].Detail); m != nil {
			rec = append(rec, i+1)
		}
		opts[i].Label = strings.TrimSpace(recMarkRE.ReplaceAllString(opts[i].Label, ""))
		opts[i].Detail = strings.TrimSpace(recMarkRE.ReplaceAllString(opts[i].Detail, ""))
	}
	q, rest := firstSentences(head)
	it := Item{Question: q}
	var detail []string
	if rest != "" {
		detail = append(detail, rest)
	}
	detail = append(detail, notes...)
	it.Detail = strings.TrimSpace(strings.Join(detail, "\n"))
	if len(opts) >= 2 {
		it.Options = opts
		it.Recommend = dedupInts(filterRange(rec, len(opts)))
	}
	return it
}

func numRE2(s string) []int {
	var out []int
	for _, f := range numRE.FindAllString(s, -1) {
		v, _ := strconv.Atoi(f)
		out = append(out, v)
	}
	return out
}

func filterRange(in []int, n int) []int {
	var out []int
	for _, v := range in {
		if v >= 1 && v <= n {
			out = append(out, v)
		}
	}
	return out
}

func dedupInts(in []int) []int {
	var out []int
	seen := map[int]bool{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// firstSentences returns the head as the question (up to questionMax bytes,
// ending at a sentence) and what did not fit.
func firstSentences(head string) (q, rest string) {
	head = strings.Join(strings.Fields(head), " ")
	if len(head) <= questionMax {
		return head, ""
	}
	cut := 0
	for _, m := range sentenceEnd.FindAllStringIndex(head, -1) {
		if m[1] > questionMax {
			break
		}
		cut = m[1]
	}
	if cut == 0 {
		return clip(head, questionMax) + "…", head
	}
	return strings.TrimSpace(head[:cut]), strings.TrimSpace(head[cut:])
}

// splitOption makes a label and detail of "**Label** - detail", "Label -
// detail", "Label: detail" or "Label".
func splitOption(s string) Option {
	s = strings.TrimSpace(s)
	if m := boldLabelRE.FindStringSubmatch(s); m != nil {
		return Option{Label: strings.TrimSpace(m[1]), Detail: strings.TrimSpace(m[2])}
	}
	s = strings.NewReplacer("**", "", "__", "").Replace(s)
	for _, sep := range []string{" - ", " – ", " — ", ": "} {
		if a, b, ok := strings.Cut(s, sep); ok && strings.TrimSpace(a) != "" {
			return Option{Label: strings.TrimSpace(a), Detail: strings.TrimSpace(b)}
		}
	}
	return Option{Label: s}
}

// ---- the last paragraph ----

// questionsIn is every sentence of p that asks the user something. relaxed
// accepts any question that is not a closer (under a "Questions" heading
// the agent has said these are questions).
func questionsIn(p string, relaxed bool) []string {
	scan := inlineCode.ReplaceAllString(urlRE.ReplaceAllString(p, " "), " ")
	var live []string
	for _, s := range sentences(scan) {
		s = strings.TrimSpace(s)
		if strings.HasSuffix(strings.TrimRight(s, `"')]*_`), "?") && !closer.MatchString(s) {
			live = append(live, s)
		}
	}
	var out []string
	for i, q := range live {
		if relaxed || askCue.MatchString(q) || you.MatchString(q) || (i == len(live)-1 && choice.MatchString(q)) {
			out = append(out, q)
		}
	}
	return out
}

func fromBlock(text string) Extraction {
	block := lastBlock(text)
	if block == "" || quoted(block) || noAnswer.MatchString(block) {
		return Extraction{}
	}
	qs := questionsIn(block, false)
	if len(qs) == 0 {
		return Extraction{}
	}
	var opts []Option
	var rec []int
	var prose []string
	for _, p := range paragraphsOf(block) {
		switch {
		case isRecLine(p):
			if m := recRE.FindStringSubmatch(plain2(p)); m != nil {
				rec = append(rec, numRE2(m[1])...)
			}
		case isList(p) && len(opts) == 0:
			var o []Option
			for _, l := range strings.Split(p, "\n") {
				if m := optRE.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
					o = append(o, splitOption(m[1]))
				}
			}
			// A list of questions is not the choices to one.
			asking := 0
			for _, x := range o {
				if hasQuestion(x.Label + x.Detail) {
					asking++
				}
			}
			if len(o) >= 2 && len(o) <= 9 && asking == 0 {
				opts = o
			} else {
				prose = append(prose, p)
			}
		default:
			prose = append(prose, p)
		}
	}
	detail := strings.TrimSpace(strings.Join(prose, "\n\n"))
	var ex Extraction
	for _, q := range qs {
		ex.Items = append(ex.Items, Item{Question: oneLine(q, questionMax), Detail: detail})
	}
	if len(opts) > 0 {
		last := &ex.Items[len(ex.Items)-1]
		for i := range opts {
			if recMarkRE.MatchString(opts[i].Label + " " + opts[i].Detail) {
				rec = append(rec, i+1)
			}
			opts[i].Label = strings.TrimSpace(recMarkRE.ReplaceAllString(opts[i].Label, ""))
			opts[i].Detail = strings.TrimSpace(recMarkRE.ReplaceAllString(opts[i].Detail, ""))
		}
		last.Options = opts
		last.Recommend = dedupInts(filterRange(rec, len(opts)))
	}
	capItems(&ex)
	return ex
}

func paragraphsOf(block string) []string {
	var out []string
	for _, p := range regexp.MustCompile(`\n[ \t]*\n`).Split(block, -1) {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

// Key is a question reduced to lowercase letters and digits, the identity
// two wordings of the same question share.
func Key(q string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(q) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
