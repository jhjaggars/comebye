package server

import (
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestMakeSkeletonHTML(t *testing.T) {
	doc := `<!doctype html><html><head><title>Storage request quotas</title><style>p{color:red}</style>
<script>var lead = "script text is never a lead, even when it is long enough to be one";</script></head>
<body><h1>Storage request quotas</h1><p class="muted">Short label</p>
<div class="hero"><p>Research: adding a portable storage request to the sandbox template, and how Kubernetes quotas and limits would block it.</p></div>
<h2>Error reporting today (reproduced)</h2><p>Second paragraph that must not replace the lead of the page.</p>
<h2>Storage <em>request</em> without driver_config</h2><h3>Drafts</h3><h2>Four</h2><h2>Five is one too many</h2>
<table><tr><td>x</td></tr></table><pre>code</pre><svg><text>label inside an svg is not text</text></svg>
<pre class="mermaid">graph TD; A-->B</pre></body></html>`
	got := makeSkeleton("html", []byte(doc))
	want := pageSkeleton{V: skelVersion,
		Lead:  "Research: adding a portable storage request to the sandbox template, and how Kubernetes quotas and limits would block it.",
		Heads: []string{"Error reporting today (reproduced)", "Storage request without driver_config", "Drafts", "Four"},
		Flags: []string{"diagram", "table", "code"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestMakeSkeletonHTMLEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body string
		lead             string
		heads            []string
		flags            []string
	}{
		{"a fragment with no head or body", "html", `<h2>Only a heading</h2><p>A fragment still has a first paragraph worth showing here.</p>`,
			"A fragment still has a first paragraph worth showing here.", []string{"Only a heading"}, nil},
		{"broken markup is read as far as it goes", "html", `<body><p>Unclosed paragraph that is long enough to count as the page's lead <b>bold <i>never closed`,
			"Unclosed paragraph that is long enough to count as the page's lead bold never closed", nil, nil},
		{"text outside a paragraph is the fallback lead", "html", `<body><div>Only loose text in a div that is long enough to stand as a lead line.</div></body>`,
			"Only loose text in a div that is long enough to stand as a lead line.", nil, nil},
		{"an image and a canvas are flags", "html", `<body><img src="data:image/png;base64,AAAA"><canvas id=c></canvas></body>`, "", nil, []string{"chart", "image"}},
		{"an unclosed head does not hide the page", "html", `<head><title>T</title><body><p>The body began without the head ever being closed, so this counts.</p>`,
			"The body began without the head ever being closed, so this counts.", nil, nil},
		{"markup in text is text, not markup", "html", `<p>&lt;img src=x onerror=alert(1)&gt; is quoted here as text, which is long enough to lead.</p>`,
			"<img src=x onerror=alert(1)> is quoted here as text, which is long enough to lead.", nil, nil},
		{"empty", "html", ``, "", nil, nil},
		{"markdown", "md", "# Title\n\nThe first paragraph of the note, with a [link](http://x) and **bold** text.\nSecond line of it.\n\n## Findings\n\n| a | b |\n|---|---|\n\n```go\nx := 1\n## not a heading\n```\n\n### Next steps\n\n![shot](data:image/png;base64,AA)\n",
			"The first paragraph of the note, with a link and bold text. Second line of it.", []string{"Findings", "Next steps"}, []string{"image", "table", "code"}},
		{"markdown with mermaid", "md", "# T\n\n```mermaid\ngraph TD\n```\n", "", nil, []string{"diagram"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := makeSkeleton(tc.kind, []byte(tc.body))
			if got.Lead != tc.lead || !reflect.DeepEqual(got.Heads, tc.heads) || !reflect.DeepEqual(got.Flags, tc.flags) {
				t.Fatalf("got %+v\nwant lead %q heads %q flags %q", got, tc.lead, tc.heads, tc.flags)
			}
		})
	}
}

func TestMakeSkeletonIsBounded(t *testing.T) {
	// 5 MiB of one long paragraph and 100000 headings: the result stays small and the walk stays fast.
	big := "<p>" + strings.Repeat("word ", 1<<20) + "</p>" + strings.Repeat("<h2>h</h2>", 100000)
	got := makeSkeleton("html", []byte(big))
	if n := len([]rune(got.Lead)); n > skelLeadMax+1 || len(got.Heads) > skelHeads {
		t.Fatalf("lead %d runes, %d heads", n, len(got.Heads))
	}
	// Control characters and invalid UTF-8 never reach the app.
	got = makeSkeleton("html", []byte("<p>"+"bell\x07 and \x00 nul and bad \xff\xfe bytes, long enough to be a lead paragraph.</p>"))
	if strings.ContainsAny(got.Lead, "\x07\x00\xff") || got.Lead == "" {
		t.Fatalf("lead %q", got.Lead)
	}
}

func TestPagesPreviewAtPublishAndOnDemand(t *testing.T) {
	s, _, _ := todoServer(t)
	dir := s.cfg.StateDir
	body := "<title>Quotas</title><h1>Quotas</h1><p>Where the time goes when the Diff side panel opens, and what to cut first.</p><h2>Causes, ranked</h2><table></table>"
	w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": body, "key": "q"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", w.Code, w.Body)
	}
	p := pageOf(t, w.Body.Bytes())
	file := s.pages.previewPath(p.ID, 1)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the preview is not kept at publish: %v", err)
	}
	g := do(s, "GET", "/api/pages/"+p.ID+"/preview?v=1", userHdr, "")
	if g.Code != 200 || !strings.Contains(g.Body.String(), `"heads":["Causes, ranked"]`) || !strings.Contains(g.Body.String(), `"flags":["table"]`) {
		t.Fatalf("preview = %d %s", g.Code, g.Body)
	}
	if cc := g.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("a version's preview never changes: Cache-Control %q", cc)
	}
	// A republish is a new version with its own preview; the first is untouched.
	w = do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": strings.Replace(body, "Causes, ranked", "A different heading", 1), "key": "q"}))
	if w.Code != 200 {
		t.Fatalf("republish = %d %s", w.Code, w.Body)
	}
	if g = do(s, "GET", "/api/pages/"+p.ID+"/preview?v=2", userHdr, ""); !strings.Contains(g.Body.String(), "A different heading") {
		t.Fatalf("v2 preview = %s", g.Body)
	}
	if g = do(s, "GET", "/api/pages/"+p.ID+"/preview?v=1", userHdr, ""); !strings.Contains(g.Body.String(), "Causes, ranked") {
		t.Fatalf("v1 preview changed: %s", g.Body)
	}
	// A version published before previews existed gets one on the first ask, and keeps it.
	_ = os.Remove(file)
	if g = do(s, "GET", "/api/pages/"+p.ID+"/preview?v=1", userHdr, ""); g.Code != 200 || !strings.Contains(g.Body.String(), "Causes, ranked") {
		t.Fatalf("backfill = %d %s", g.Code, g.Body)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the backfilled preview is not kept: %v", err)
	}
	if g = do(s, "GET", "/api/pages/"+p.ID+"/preview?v=9", userHdr, ""); g.Code != http.StatusNotFound {
		t.Fatalf("unknown version = %d", g.Code)
	}
	if g = do(s, "GET", "/api/pages/pg_0000000000/preview", userHdr, ""); g.Code != http.StatusNotFound {
		t.Fatalf("unknown page = %d", g.Code)
	}
	// Deleting the page removes its directory, previews with it.
	if d := do(s, "DELETE", "/api/pages/"+p.ID, userHdr, ""); d.Code != 200 && d.Code != 204 {
		t.Fatalf("delete = %d %s", d.Code, d.Body)
	}
	if _, err := os.Stat(s.pages.previewPath(p.ID, 2)); !os.IsNotExist(err) {
		t.Fatalf("a deleted page keeps its preview: %v", err)
	}
	_ = dir
}

func TestPagesPreviewIsReadUnderTheBodyRule(t *testing.T) {
	s, _, _ := todoServer(t)
	w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "<p>Private words of one host's agent, long enough to be the lead line.</p>"}))
	p := pageOf(t, w.Body.Bytes())
	// Another host's agent may not read the page's words through its preview; the user (and the page's own host) may.
	fwd := map[string]string{"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgent: "w3:p1", HeaderAgentHost: "elsewhere"}
	if g := do(s, "GET", "/api/pages/"+p.ID+"/preview?v=1", fwd, ""); g.Code != 403 || strings.Contains(g.Body.String(), "Private words") {
		t.Fatalf("an agent of another host read the preview: %d %s", g.Code, g.Body)
	}
	if g := do(s, "GET", "/api/pages/"+p.ID+"/preview?v=1", userHdr, ""); g.Code != 200 || !strings.Contains(g.Body.String(), "Private words") {
		t.Fatalf("the user cannot read it: %d %s", g.Code, g.Body)
	}
	if g := do(s, "GET", "/api/pages/"+p.ID+"/preview?v=1", agentHdr("w2:p1"), ""); g.Code != 200 {
		t.Fatalf("the page's own host cannot read it: %d %s", g.Code, g.Body)
	}
}
