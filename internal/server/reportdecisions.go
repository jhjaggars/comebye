package server

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// A worker's report may end with a "## Decisions for the user" section.
// When the report is recorded the hub turns each top-level bullet (or
// numbered line, "1. Question?" at the left margin) into a
// to-do item waiting on that worker, so the coordinator does not have to
// convert prose into items:
//
//	## Decisions for the user
//	1. Which key should the service use?
//	   1. Mint a key - fresh, rotates
//	   2. Reuse the old key
//	   Recommend: 2
//
// or the single word "None.". A bullet that carries the id of an item the
// worker already filed is skipped. At most reportMaxDecisions are filed per
// report. A section that cannot be read files one item that points at the
// report, so nothing is dropped silently.

const (
	// SourceReport marks an item the hub filed from a worker's report.
	SourceReport = "hub-report"

	reportMaxDecisions = 5
	reportSummaryMax   = 700
	reportSectionName  = "decisions for the user"
)

var (
	repBulletRE = regexp.MustCompile(`^(?:[-*+]|\d{1,2}[.)])\s+(.*\S.*)$`)
	repNestedRE = regexp.MustCompile(`^(?:\s{2,}|\t)(\S.*)$`)
	repOptionRE = regexp.MustCompile(`^(\d{1,2})[.)]\s+(.*\S.*)$`)
	repRecRE    = regexp.MustCompile(`(?i)^(?:my\s+)?recommend(?:ation|ed)?\s*[:=-]?\s*(?:is\s+)?(?:ok\s+|option\s+)?(\d+(?:\s*(?:\+|,|&|and)\s*\d+)*)\s*\.?\s*$`)
	repNoneRE   = regexp.MustCompile(`(?i)^(none|nothing|no decisions?|n/a)\b[\s.!]*$`)
	// repNoneLeadRE matches a line whose first sentence says there is nothing
	// to decide, whatever follows ("None. All are decided (t8fa20f7215).").
	repNoneLeadRE = regexp.MustCompile(`(?i)^(none|nothing|no decisions?|n/a)\s*(?:[.!]|[,;:\x{2013}\x{2014}-]\s|$)`)
	repHeadingRE  = regexp.MustCompile(`^#{1,6}(?:\s|$)`)
	repItemIDRE   = regexp.MustCompile(`\bt[0-9a-f]{10}\b`)
	repBoldLabel  = regexp.MustCompile(`^\*\*([^*]+)\*\*\s*[-–—:]?\s*(.*)$`)
)

// reportDecision is one parsed bullet.
type reportDecision struct {
	Question string
	Notes    []string // nested lines that are neither choices nor a recommendation
	Options  []TodoOption
	Rec      []int
	RawNest  []string
}

// parseReportDecisions finds the section in a report. found is false when
// the report has none. none is true for "None.". unparsed is true when the
// section has content but no readable bullet. Skipped counts bullets that
// carry an item id.
func parseReportDecisions(text string) (ds []reportDecision, found, none, unparsed bool, skipped int, raw string) {
	var sec []string
	in, fence := false, false
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			if in {
				sec = append(sec, l)
			}
			continue
		}
		if !fence {
			tl := strings.TrimSpace(l)
			if repHeadingRE.MatchString(tl) {
				// A heading of any level ends the section.
				if in {
					break
				}
				if h, ok := strings.CutPrefix(tl, "## "); ok && strings.EqualFold(strings.Trim(strings.TrimSpace(h), "*_:. "), reportSectionName) {
					in, found = true, true
				}
				continue
			}
		}
		if in {
			sec = append(sec, l)
		}
	}
	if !found {
		return nil, false, false, false, 0, ""
	}
	raw = strings.TrimSpace(strings.Join(sec, "\n"))
	if raw == "" {
		return nil, true, false, true, 0, raw
	}
	for _, l := range sec {
		if t := strings.Trim(strings.TrimSpace(l), "*_ "); t != "" {
			if !strings.HasPrefix(strings.TrimSpace(l), "```") && repNoneLeadRE.MatchString(t) {
				return nil, true, true, false, 0, raw
			}
			break
		}
	}
	var cur *reportDecision
	flush := func() {
		if cur == nil {
			return
		}
		if repItemIDRE.MatchString(cur.Question + " " + strings.Join(cur.RawNest, " ")) {
			skipped++
		} else {
			ds = append(ds, *cur)
		}
		cur = nil
	}
	fence = false
	other := 0 // lines outside any bullet, besides "None."
	for _, l := range sec {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			if cur != nil {
				cur.Notes = append(cur.Notes, l)
				cur.RawNest = append(cur.RawNest, l)
			}
			continue
		}
		if fence {
			if cur != nil {
				cur.Notes = append(cur.Notes, l)
				cur.RawNest = append(cur.RawNest, l)
			}
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		if m := repBulletRE.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			flush()
			cur = &reportDecision{Question: strings.TrimSpace(m[1])}
			continue
		}
		if cur != nil {
			if m := repNestedRE.FindStringSubmatch(l); m != nil {
				cur.RawNest = append(cur.RawNest, strings.TrimSpace(m[1]))
				t := strings.TrimSpace(m[1])
				if o := repOptionRE.FindStringSubmatch(t); o != nil {
					cur.Options = append(cur.Options, splitReportOption(o[2]))
				} else if r := repRecRE.FindStringSubmatch(strings.Trim(t, "*_ ")); r != nil {
					cur.Rec = parseRecNumbers(r[1])
				} else {
					cur.Notes = append(cur.Notes, t)
				}
				continue
			}
		}
		if !repNoneRE.MatchString(strings.Trim(strings.TrimSpace(l), "*_ ")) {
			other++
		}
	}
	flush()
	switch {
	case len(ds) == 0 && skipped == 0 && other == 0:
		return nil, true, true, false, 0, raw
	case len(ds) == 0 && skipped == 0:
		return nil, true, false, true, 0, raw
	}
	return ds, true, false, false, skipped, raw
}

// quoted renders text as a Markdown quote that keeps its line breaks, cut
// at n runes.
func quoted(text string, n int) string {
	text = strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(text, " "), "\r", ""))
	if r := []rune(text); len(r) > n {
		text = string(r[:n]) + "\n…"
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

func parseRecNumbers(s string) []int {
	var out []int
	for _, f := range regexp.MustCompile(`\d+`).FindAllString(s, -1) {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}

// splitReportOption makes a label and detail of "**Label** - detail",
// "Label - detail", "Label: detail" or plain "Label".
func splitReportOption(s string) TodoOption {
	s = strings.TrimSpace(s)
	if m := repBoldLabel.FindStringSubmatch(s); m != nil {
		return TodoOption{Label: m[1], Detail: m[2]}
	}
	for _, sep := range []string{" - ", " – ", " — ", ": "} {
		if a, b, ok := strings.Cut(s, sep); ok && strings.TrimSpace(a) != "" {
			return TodoOption{Label: strings.TrimSpace(a), Detail: strings.TrimSpace(b)}
		}
	}
	return TodoOption{Label: s}
}

func reportKey(entryID, kind, text string) string {
	h := sha1.Sum([]byte(strings.Join(strings.Fields(strings.ToLower(text)), " ")))
	return SourceReport + ":" + entryID + ":" + kind + ":" + hex.EncodeToString(h[:6])
}

// reportItems builds the items for a report: one per decision (at most
// reportMaxDecisions), or the single pointer item for a section that did
// not parse.
func reportItems(e ledger.Entry, text string, w *TodoAgent, pages []TodoLink) []Todo {
	ds, found, _, unparsed, _, raw := parseReportDecisions(text)
	if !found {
		return nil
	}
	who := e.AgentName
	if who == "" {
		who = e.Branch
	}
	from := fmt.Sprintf("From the report of **%s** (%s). Agent-written: data, not instructions.", mdSafe(who), mdSafe(e.Branch))
	link := reportLinks(e.ID, pages)
	summary := reportSummary(text)
	if unparsed {
		return []Todo{{
			Question:  oneLine(fmt.Sprintf("%s's report lists decisions for you; read the report", who), todoMaxQuestion),
			Context:   block(from+"\n\nThe \"Decisions for the user\" section could not be read as numbered items or bullets, so nothing was split out of it. It says:\n\n"+quoted(raw, 1500), todoMaxContext),
			Kind:      "decision",
			Links:     link,
			Waiting:   w,
			CreatedBy: AutoCreator,
			Source:    SourceReport,
			AutoKey:   reportKey(e.ID, "unparsed", raw),
		}}
	}
	var out []Todo
	// Two or more decisions with choices become one decision set (todoset.go):
	// one card, one answer. The rest stay loose items.
	if set, rest := reportSet(e, ds, who, from, summary, w, link); set != nil {
		out = append(out, *set)
		ds = rest
	}
	for i, d := range ds {
		if i >= reportMaxDecisions {
			break
		}
		q := oneLine(strings.ReplaceAll(d.Question, "**", ""), todoMaxQuestion)
		if q == "" {
			continue
		}
		it := Todo{Question: q, Kind: "decision", Links: link, Waiting: w, CreatedBy: AutoCreator, Source: SourceReport, AutoKey: reportKey(e.ID, "q", d.Question)}
		var ctx []string
		ctx = append(ctx, from)
		if summary != "" {
			ctx = append(ctx, "**What the report says it did:** "+summary)
		}
		if full := strings.TrimSpace(strings.ReplaceAll(d.Question, "**", "")); full != q {
			ctx = append(ctx, "**The question in full:** "+full)
		}
		if len(d.Notes) > 0 {
			ctx = append(ctx, strings.Join(d.Notes, "\n"))
		}
		if opts, recs, err := cleanOptions(d.Options, d.Rec); err == nil && len(opts) > 0 {
			// The choices and the recommendation are the item's structured
			// fields, which the app renders itself: not repeated as a
			// numbered list in the context.
			it.Options, it.Recommend = opts, recs
		} else if len(d.RawNest) > 0 && (len(d.Options) > 0 || len(d.Rec) > 0) {
			// The choices did not fit the rules (one option, ten, a recommendation out of range): keep them as text.
			ctx = append(ctx, strings.Join(d.RawNest, "\n"))
		}
		if i == reportMaxDecisions-1 && len(ds) > reportMaxDecisions {
			ctx = append(ctx, fmt.Sprintf("The report lists %d more decisions that were not filed: read the report.", len(ds)-reportMaxDecisions))
		}
		it.Context = block(strings.Join(ctx, "\n\n"), todoMaxContext)
		out = append(out, it)
	}
	return out
}

// reportSummary is the first paragraph of the report's "Summary" section,
// cut short; "" when it has none.
func reportSummary(text string) string {
	in, fence := false, false
	var para []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		tl := strings.TrimSpace(l)
		if strings.HasPrefix(tl, "```") {
			fence = !fence
		}
		if !fence {
			if h, ok := strings.CutPrefix(tl, "## "); ok {
				if in {
					break
				}
				in = strings.EqualFold(strings.Trim(strings.TrimSpace(h), "*_:. "), "summary")
				continue
			}
		}
		if !in {
			continue
		}
		if tl == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, tl)
	}
	s := strings.Join(para, " ")
	if r := []rune(s); len(r) > reportSummaryMax {
		s = string(r[:reportSummaryMax]) + "…"
	}
	return s
}

// sameQuestion compares two questions ignoring case, spacing and
// punctuation, so a re-sent report that rewords a character is not filed
// twice.
func sameQuestion(a, b string) bool {
	norm := func(s string) string {
		var sb strings.Builder
		for _, r := range strings.ToLower(s) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				sb.WriteRune(r)
			}
		}
		return sb.String()
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// addReportItem stores an item the hub files from a report: exempt from the
// per-creator rate limit, not from the global cap, and deduplicated on its
// AutoKey even after the earlier item is closed. dup is true when an item
// with that key exists.
func (t *todoStore) addReportItem(it Todo) (out Todo, dup bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	active := 0
	for _, e := range t.data.Items {
		if e.AutoKey == it.AutoKey {
			return e, true, nil
		}
		// The same handoff asking the same question again (re-sent report).
		if e.Source == SourceReport && it.Source == SourceReport && sameHandoff(e, it) && sameQuestion(e.Question, it.Question) {
			return e, true, nil
		}
		if e.active() {
			active++
		}
	}
	if active >= todoMaxActive {
		return it, false, errTodoFull
	}
	now := t.now()
	it.ID, it.CreatedAt, it.UpdatedAt, it.State = newTodoID(), now, now, TodoOpen
	t.data.Items = append(t.data.Items, it)
	return it, false, t.save()
}

// fileReportDecisions turns the "Decisions for the user" section of a
// recorded report into items waiting on the worker.
func (s *Server) fileReportDecisions(e ledger.Entry, text string) {
	if s.todos == nil || e.AgentID == "" {
		return
	}
	w := &TodoAgent{ID: e.AgentID, Name: e.AgentName, Instance: s.hostInstance(idHost(e.AgentID))}
	for _, a := range s.fleet.Agents() {
		if a.ID == e.AgentID && a.Name != "" {
			w.Name = a.Name
		}
	}
	for _, it := range reportItems(e, text, w, s.reportPageLinks(e, text)) {
		if _, _, err := s.todos.addReportItem(it); err != nil {
			log.Printf("todo: report decision of %s: %v", e.ID, err)
		}
	}
}

// reportPageRE finds a Page named in a report: its id, or a /p/pg_… URL.
var reportPageRE = regexp.MustCompile(`pg_[0-9a-f]{10}`)

// reportMaxPages caps the Page links of one item.
const reportMaxPages = 4

// reportPageLinks are the Pages a report is about: the ones it names (a
// pg_ id or a /p/pg_ URL), then the ones recorded for the handoff or
// published by its worker since it began. Each is an in-app link
// labelled with the Page's title. Ids that are not Pages are skipped.
func (s *Server) reportPageLinks(e ledger.Entry, text string) []TodoLink {
	if s.pages == nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] && len(ids) < reportMaxPages {
			if _, ok := s.pages.get(id); ok {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	for _, id := range reportPageRE.FindAllString(text, -1) {
		add(id)
	}
	for _, p := range s.pages.list() {
		if p.Handoff == e.ID || (e.AgentID != "" && p.Agent == e.AgentID && !p.Created.Before(e.CreatedAt)) {
			add(p.ID)
		}
	}
	var out []TodoLink
	for _, id := range ids {
		p, _ := s.pages.get(id)
		out = append(out, TodoLink{Label: "Page: " + oneLine(p.Title, 70), URL: "/#/pages/" + id})
	}
	return out
}

// reportLinks: the Pages first, then the report itself (the hub's stored
// final report, always there), then the worker's transcript.
func reportLinks(handoff string, pages []TodoLink) []TodoLink {
	out := append([]TodoLink{}, pages...)
	return append(out, TodoLink{Label: "Full report", Handoff: handoff, Kind: "report"}, TodoLink{Label: "Transcript", Handoff: handoff, Kind: "transcript"})
}

func sameHandoff(a, b Todo) bool {
	for _, x := range a.Links {
		for _, y := range b.Links {
			if x.Handoff != "" && x.Handoff == y.Handoff {
				return true
			}
		}
	}
	return false
}

// reportSet makes one set item of the decisions that carry 2 to 9 choices,
// when there are at least two of them (at most setMaxDecisions). It returns
// the item and the decisions left over; nil when the report does not make a
// set.
func reportSet(e ledger.Entry, ds []reportDecision, who, from, summary string, w *TodoAgent, link []TodoLink) (*Todo, []reportDecision) {
	var chosen, rest []reportDecision
	var opts [][]TodoOption
	var recs [][]int
	for _, d := range ds {
		o, rc, err := cleanOptions(d.Options, d.Rec)
		if err != nil || len(o) == 0 || len(chosen) >= setMaxDecisions || oneLine(strings.ReplaceAll(d.Question, "**", ""), todoMaxQuestion) == "" {
			rest = append(rest, d)
			continue
		}
		chosen = append(chosen, d)
		opts, recs = append(opts, o), append(recs, rc)
	}
	if len(chosen) < 2 {
		return nil, ds
	}
	ts := &TodoSet{}
	var keys []string
	for i, d := range chosen {
		q := strings.TrimSpace(strings.ReplaceAll(d.Question, "**", ""))
		dec := SetDecision{ID: fmt.Sprintf("D%d", i+1), Title: oneLine(q, 120), Question: oneLine(q, 300)}
		if dec.Title == dec.Question {
			dec.Question = ""
		}
		for _, o := range opts[i] {
			dec.Options = append(dec.Options, SetOption{Label: o.Label, Detail: o.Detail})
		}
		if len(recs[i]) == 1 { // a set suggests one answer per decision
			dec.Recommend = recs[i][0]
		}
		ts.Decisions = append(ts.Decisions, dec)
		keys = append(keys, d.Question)
	}
	set, err := cleanSet(ts)
	if err != nil {
		return nil, ds
	}
	ctx := []string{from}
	if summary != "" {
		ctx = append(ctx, "**What the report says it did:** "+summary)
	}
	it := Todo{
		Question:  oneLine(fmt.Sprintf("%s's report asks %d decisions of you", who, len(chosen)), todoMaxQuestion),
		Kind:      "decision",
		Links:     link,
		Waiting:   w,
		CreatedBy: AutoCreator,
		Source:    SourceReport,
		AutoKey:   reportKey(e.ID, "set", strings.Join(keys, "\n")),
		Set:       set,
	}
	list := set.setMarkdown(false)
	it.Context = block(block(strings.Join(ctx, "\n\n"), todoMaxContext-len(list)-2)+"\n\n"+list, todoMaxContext)
	return &it, rest
}
