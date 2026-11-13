package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/ledger"
)

func TestParseReportDecisions(t *testing.T) {
	const head = "## Summary\nx\n## Decisions for the user\n"
	cases := []struct {
		name                  string
		text                  string
		found, none, unparsed bool
		n, skipped            int
		q0                    string
		opts, rec             int
	}{
		{"no section", "## Summary\nx\n## Follow-ups\nNone.\n", false, false, false, 0, 0, "", 0, 0},
		{"None.", head + "None.\n", true, true, false, 0, 0, "", 0, 0},
		{"bold none", head + "**None.**\n## Follow-ups\n", true, true, false, 0, 0, "", 0, 0},
		{"one bullet", head + "- May I delete the old key?\n", true, false, false, 1, 0, "May I delete the old key?", 0, 0},
		{"nested options", head + "- Which key?\n  1. Mint a key - fresh\n  2. **Reuse** the old one\n  Recommend: 2\n- Second?\n", true, false, false, 2, 0, "Which key?", 2, 1},
		{"recommend multi", head + "- Which?\n  1. a\n  2. b\n  3. c\n  My recommendation: ok 1+3\n", true, false, false, 1, 0, "Which?", 3, 2},
		{"carries an id", head + "- Already filed t0123456789\n- New one?\n", true, false, false, 1, 1, "New one?", 0, 0},
		{"only ids", head + "- t0123456789 is filed\n", true, false, false, 0, 1, "", 0, 0},
		{"prose", head + "I think you should look at the key and decide.\n", true, false, true, 0, 0, "", 0, 0},
		{"empty", head, true, false, true, 0, 0, "", 0, 0},
		{"ends at next heading", head + "- A?\n## Follow-ups\n- not a decision\n", true, false, false, 1, 0, "A?", 0, 0},
		{"fenced heading ignored", "```\n## Decisions for the user\n- fake?\n```\n## Summary\nx\n", false, false, false, 0, 0, "", 0, 0},
		{"numbered top level", head + "1. Which key?\n   1. Mint a key\n   2. Reuse it\n   My recommendation: ok 2\n2. Second?\n   1. a\n   2. b\n   My recommendation: ok 1\n", true, false, false, 2, 0, "Which key?", 2, 1},
		// Real reports: research-rolling-rollout, and the tracking probe.
		{"None. then prose", head + "None. All are decided (t8fa20f7215 and the earlier answers).\n", true, true, false, 0, 0, "", 0, 0},
		{"None then sub-heading list", head + "None.\n\n### Answers\n\n1. The probe is read-only.\n2. It files nothing.\n3. Third.\n4. Fourth.\n5. Fifth.\n", true, true, false, 0, 0, "", 0, 0},
		{"sub-heading ends section", head + "- A?\n### Notes\n- not a decision\n", true, false, false, 1, 0, "A?", 0, 0},
		{"case and colon", "## decisions for the user:\n- A?\n", true, false, false, 1, 0, "A?", 0, 0},
	}
	for _, c := range cases {
		ds, found, none, unparsed, skipped, _ := parseReportDecisions(c.text)
		if found != c.found || none != c.none || unparsed != c.unparsed || len(ds) != c.n || skipped != c.skipped {
			t.Errorf("%s: found=%v none=%v unparsed=%v n=%d skipped=%d", c.name, found, none, unparsed, len(ds), skipped)
			continue
		}
		if c.n > 0 && (ds[0].Question != c.q0 || len(ds[0].Options) != c.opts || len(ds[0].Rec) != c.rec) {
			t.Errorf("%s: first = %+v", c.name, ds[0])
		}
	}
}

func decisionsServer(t *testing.T) (*Server, ledger.Entry) {
	t.Helper()
	s, _, _ := todoServer(t)
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "feat/x", AgentID: "desk/w9:p1", PaneID: "w9:p1",
		AgentName: "feat-x", Status: ledger.Running, ReplyTo: &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, e
}

func report(t *testing.T, s *Server, text string) int {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"text": text})
	return do(s, "POST", "/api/report", agentHdr("w9:p1"), string(b)).Code
}

func activeItems(s *Server) []Todo { return s.todos.list("all") }

func TestReportFilesDecisions(t *testing.T) {
	s, e := decisionsServer(t)
	const tail = "## Summary\nx\n## Decisions for the user\n"
	text := tail + "- Which key should the service use?\n  1. Mint a key - fresh, rotates\n  2. Reuse the old key\n  Recommend: 2\n- Delete the old branch?\n- Already asked (t0123456789)\n"
	if c := report(t, s, text); c != http.StatusOK {
		t.Fatalf("report = %d", c)
	}
	items := activeItems(s)
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	it := items[0]
	if it.Question != "Which key should the service use?" || it.Source != SourceReport || it.CreatedBy != AutoCreator || it.Kind != "decision" ||
		it.Waiting == nil || it.Waiting.ID != "desk/w9:p1" || len(it.Links) != 2 || it.Links[0].Handoff != e.ID || it.Links[0].Label != "Full report" ||
		len(it.Options) != 2 || it.Options[0].Label != "Mint a key" || it.Options[0].Detail != "fresh, rotates" || len(it.Recommend) != 1 || it.Recommend[0] != 2 ||
		strings.Contains(it.Context, "1. **Mint a key**") || strings.Contains(it.Context, "My recommendation") ||
		!strings.Contains(it.Context, "What the report says it did:** x") {
		t.Fatalf("item = %+v", it)
	}
	if items[1].Question != "Delete the old branch?" || len(items[1].Options) != 0 {
		t.Fatalf("second = %+v", items[1])
	}
	// The same report again files nothing; a changed bullet files one.
	report(t, s, text)
	if n := len(activeItems(s)); n != 2 {
		t.Fatalf("after repeat: %d items", n)
	}
	report(t, s, text+"- A brand new question?\n")
	if n := len(activeItems(s)); n != 3 {
		t.Fatalf("after change: %d items", n)
	}
	// A bullet already filed stays deduped after the user closed it.
	if _, err := s.todos.close(items[1].ID, TodoDropped, "me", "moot"); err != nil {
		t.Fatal(err)
	}
	report(t, s, text)
	if n := len(activeItems(s)); n != 3 {
		t.Fatalf("after drop and repeat: %d items", n)
	}
}

func TestReportDecisionsNoneFilesNothing(t *testing.T) {
	s, _ := decisionsServer(t)
	for _, text := range []string{"## Summary\nx\n## Decisions for the user\nNone.\n", "## Summary\nx\n"} {
		if c := report(t, s, text); c != http.StatusOK {
			t.Fatalf("report = %d", c)
		}
	}
	if n := len(activeItems(s)); n != 0 {
		t.Fatalf("%d items", n)
	}
}

func TestReportDecisionsCapAndFallback(t *testing.T) {
	s, _ := decisionsServer(t)
	var sb strings.Builder
	sb.WriteString("## Decisions for the user\n")
	for i := 0; i < 8; i++ {
		sb.WriteString("- Question number " + string(rune('A'+i)) + "?\n")
	}
	report(t, s, sb.String())
	items := activeItems(s)
	if len(items) != reportMaxDecisions {
		t.Fatalf("%d items, want %d", len(items), reportMaxDecisions)
	}
	if last := items[len(items)-1]; !strings.Contains(last.Context, "3 more decisions") {
		t.Fatalf("overflow not mentioned: %q", last.Context)
	}
	// A section that does not parse files one pointer item, once.
	s2, e2 := decisionsServer(t)
	prose := "## Summary\nx\n## Decisions for the user\nYou should look at the key and decide.\n"
	report(t, s2, prose)
	report(t, s2, prose)
	items = activeItems(s2)
	if len(items) != 1 || !strings.Contains(items[0].Question, "read the report") || items[0].Links[0].Handoff != e2.ID ||
		!strings.Contains(items[0].Context, "look at the key") {
		t.Fatalf("fallback = %+v", items)
	}
	// Choices that break the rules (one option) are kept as text, not dropped.
	s3, _ := decisionsServer(t)
	report(t, s3, "## Decisions for the user\n- Pick?\n  1. only one\n  Recommend: 1\n")
	items = activeItems(s3)
	if len(items) != 1 || len(items[0].Options) != 0 || !strings.Contains(items[0].Context, "1. only one") {
		t.Fatalf("bad options = %+v", items)
	}
}

// A worker whose handoff is landed or marked ready to remove can report
// again (it was refused as "not a handoff worker"), and its decisions are
// filed.
func TestReportAfterLandedOrMarkedRoute(t *testing.T) {
	for _, st := range []ledger.Status{ledger.Landed, ledger.ReadyToRemove} {
		s, _, _ := coordServer(t)
		e, _ := s.ledger.Add(ledger.Entry{Host: "desk", Branch: "feat/x", AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "feat-x", Status: st, FinalReport: "old", Mark: &ledger.Mark{AgentID: "desk/w9:p1"}})
		if c := report(t, s, "## Summary\nagain\n## Decisions for the user\n- One more?\n"); c != http.StatusOK {
			t.Fatalf("%s: report = %d", st, c)
		}
		g, _ := s.ledger.Get(e.ID)
		if g.Status != ledger.Finished || !strings.Contains(g.FinalReport, "again") || len(s.todos.list("")) != 1 {
			t.Fatalf("%s: entry %+v, items %d", st, g, len(s.todos.list("")))
		}
	}
}

// hubK8sSection paraphrases the section hub-k8s's report filed as a
// fallback: numbered decisions, indented numbered options, a recommendation.
const hubK8sSection = `## Summary
x

## Decisions for the user
1. Recall index refresh after the cutover (step M)?
   1. Give the hub its own archive key and an hourly index container in the pod.
   2. Keep the VM's index job and copy the index over on a schedule.
   3. Accept a stale index at first and decide later.
   My recommendation: ok 1
2. May I run the read-only preparation on the old VM and the rehearsal on another port?
   1. Yes, both.
   2. Preparation only.
   3. Not yet.
   My recommendation: ok 1
3. When should the cutover window be, and what is the rollback deadline?
   1. A quiet slot you name; rollback by copy-back within 4 hours.
   2. Same slot, but a stricter 1-hour deadline.
   My recommendation: ok 1
`

// Three decisions with choices are one decision set, not three loose items.
func TestReportFilesNumberedDecisions(t *testing.T) {
	s, _ := decisionsServer(t)
	report(t, s, hubK8sSection)
	items := activeItems(s)
	if len(items) != 1 || items[0].Set == nil || len(items[0].Set.Decisions) != 3 {
		t.Fatalf("want one set of 3: %+v", items)
	}
	it := items[0]
	if it.Source != SourceReport || it.Waiting == nil || len(it.Links) != 2 || !strings.Contains(it.Question, "3 decisions") {
		t.Fatalf("set item = %+v", it)
	}
	d := it.Set.Decisions[0]
	if d.ID != "D1" || d.Title != "Recall index refresh after the cutover (step M)?" || len(d.Options) != 3 || d.Recommend != 1 {
		t.Fatalf("D1 = %+v", d)
	}
	if !strings.Contains(it.Context, "What the report says it did:** x") || !strings.Contains(it.Context, "D2 May I run") {
		t.Fatalf("context = %q", it.Context)
	}
	// Re-sent, it is not filed again, even after the first is closed.
	report(t, s, hubK8sSection)
	s.todos.close(it.ID, TodoDropped, "me", "moot")
	report(t, s, hubK8sSection)
	if n := len(s.todos.list("all")); n != 1 {
		t.Fatalf("re-sent report filed %d items", n)
	}
}

// Decisions without choices stay loose items next to the set; a single
// decision with choices stays a plain item.
func TestReportSetAlongsideLooseItems(t *testing.T) {
	s, _ := decisionsServer(t)
	report(t, s, "## Decisions for the user\n- Open question with no choices?\n- First pick?\n  1. a\n  2. b\n  Recommend: 1+2\n- Second pick?\n  1. c\n  2. d\n")
	items := activeItems(s)
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	var set, loose *Todo
	for i := range items {
		if items[i].Set != nil {
			set = &items[i]
		} else {
			loose = &items[i]
		}
	}
	if set == nil || loose == nil || loose.Question != "Open question with no choices?" || len(set.Set.Decisions) != 2 {
		t.Fatalf("set %+v loose %+v", set, loose)
	}
	if set.Set.Decisions[0].Recommend != 0 {
		t.Fatalf("a combined recommendation is not one suggestion: %+v", set.Set.Decisions[0])
	}
	s2, _ := decisionsServer(t)
	report(t, s2, "## Decisions for the user\n- Only pick?\n  1. a\n  2. b\n- Another?\n")
	for _, it := range activeItems(s2) {
		if it.Set != nil {
			t.Fatalf("one decision with choices made a set: %+v", it)
		}
	}
}

func TestReportFallbackKeepsLines(t *testing.T) {
	s, _ := decisionsServer(t)
	report(t, s, "## Decisions for the user\nFirst thought.\n\nSecond thought.\n  indented\n")
	items := activeItems(s)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	c := items[0].Context
	if strings.Contains(c, "```") || !strings.Contains(c, "> First thought.\n>\n> Second thought.\n>   indented") {
		t.Fatalf("fallback lost its lines: %q", c)
	}
}

// The to-do carries the report's summary and the decision's own text, keeps
// the choices structured only, and a re-sent report with a reworded
// punctuation does not file the question again.
func TestReportDecisionContextAndDedupe(t *testing.T) {
	s, e := decisionsServer(t)
	text := "## Summary\nI moved the cache behind a flag.\nIt is off by default.\n\nMore detail.\n## Decisions for the user\n" +
		"1. Should the flag default to on?\n   The flag costs 3 ms per request when on.\n   1. Yes - faster reads\n   2. No - safer\n   Recommend: 2\n"
	report(t, s, text)
	items := activeItems(s)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	c := items[0].Context
	for _, want := range []string{"I moved the cache behind a flag. It is off by default.", "The flag costs 3 ms per request when on."} {
		if !strings.Contains(c, want) {
			t.Errorf("context lacks %q: %q", want, c)
		}
	}
	if strings.Contains(c, "More detail") || strings.Contains(c, "1. ") || strings.Contains(c, "recommendation") {
		t.Errorf("context = %q", c)
	}
	if len(items[0].Options) != 2 || items[0].Options[1].Detail != "safer" || items[0].Recommend[0] != 2 || items[0].Links[0].Handoff != e.ID {
		t.Errorf("item = %+v", items[0])
	}
	report(t, s, strings.Replace(text, "default to on?", "default to ON ?", 1))
	if n := len(activeItems(s)); n != 1 {
		t.Fatalf("re-sent report filed %d items", n)
	}
}

// A report that names a Page makes a set whose first link is that Page; a
// Page published by the worker is linked too; "Full report" opens the
// stored report; ids that are not Pages are left out.
func TestReportLinksPagesFirst(t *testing.T) {
	s, e := decisionsServer(t)
	s.pages = newPageStore(t.TempDir())
	named, _, err := s.pages.publish(pageIn{Kind: "md", Title: "Review to-do UX", Body: []byte("# a")}, pageWho{user: true, by: "me"})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.pages.publish(pageIn{Kind: "md", Title: "Worker demo", Body: []byte("# b"), Handoff: e.ID}, pageWho{user: true, by: "me"})
	if err != nil {
		t.Fatal(err)
	}
	text := "## Summary\nMockup at http://hub/p/" + named.ID + " and pg_0000000000 (gone).\n## Decisions for the user\n- First pick?\n  1. a\n  2. b\n- Second pick?\n  1. c\n  2. d\n"
	report(t, s, text)
	items := activeItems(s)
	if len(items) != 1 || items[0].Set == nil {
		t.Fatalf("items = %+v", items)
	}
	l := items[0].Links
	if len(l) != 4 || l[0].Label != "Page: Review to-do UX" || l[0].URL != "/#/pages/"+named.ID ||
		l[1].Label != "Page: Worker demo" || l[1].URL != "/#/pages/"+other.ID ||
		l[2].Label != "Full report" || l[2].Kind != "report" || l[2].Handoff != e.ID ||
		l[3].Label != "Transcript" || l[3].Kind != "transcript" {
		t.Fatalf("links = %+v", l)
	}
	// A single "from report" item gets the same links.
	s2, e2 := decisionsServer(t)
	s2.pages = s.pages
	report(t, s2, "## Decisions for the user\n- Only question? see "+named.ID+"\n")
	it := activeItems(s2)[0]
	if len(it.Links) < 3 || it.Links[0].Label != "Page: Review to-do UX" || it.Links[len(it.Links)-2].Handoff != e2.ID {
		t.Fatalf("single item links = %+v", it.Links)
	}
}
