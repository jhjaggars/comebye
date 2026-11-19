package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPageHasScript(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"inline", `<p>hi</p><script>alert(1)</script>`, true},
		{"src", `<script src="https://cdn.example/x.js"></script>`, true},
		{"upper and attrs", `<SCRIPT type="module">x</SCRIPT>`, true},
		{"self-closed", `<script/>`, true},
		{"noscript only", `<noscript>turn on JavaScript</noscript><p>hi</p>`, false},
		{"prose", `<p>This script is for the demo; a script tag is how it works.</p>`, false},
		{"escaped", `<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>`, false},
		{"commented out", `<!-- <script>x</script> --><p>hi</p>`, false},
		{"scripts tag name", `<scripts>x</scripts>`, false},
		{"after a comment", `<!-- c --><script>x</script>`, true},
	}
	for _, c := range cases {
		if got := pageHasScript([]byte(c.body)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPagesScriptFlagAndSearch(t *testing.T) {
	s, _, _ := todoServer(t)
	pub := func(m map[string]any) Page {
		w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(m))
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("publish = %d %s", w.Code, w.Body)
		}
		return pageOf(t, w.Body.Bytes())
	}
	chart := pub(map[string]any{"body": `<title>Chart demo</title><p>A chart of leases.</p><script>draw()</script>`})
	ext := pub(map[string]any{"body": `<title>Ext</title><p>nothing</p><script src="x.js"></script>`})
	plain := pub(map[string]any{"body": `<title>Plain</title><p>A chart with no code. <noscript>script off</noscript></p>`})
	md := pub(map[string]any{"body": "# Notes\n\nA chart and <script>x</script> in markdown.", "kind": "md"})
	for _, c := range []struct {
		p    Page
		want bool
	}{{chart, true}, {ext, true}, {plain, false}, {md, false}} {
		if c.p.Script != c.want {
			t.Errorf("publish response %s: script=%v, want %v", c.p.Title, c.p.Script, c.want)
		}
	}

	type resp struct {
		Pages   []Page      `json:"pages"`
		Matches []PageMatch `json:"matches"`
	}
	get := func(url string) (int, resp, string) {
		w := do(s, "GET", url, userHdr, "")
		var r resp
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r, w.Body.String()
	}
	// The API field: true for flagged pages, absent for the rest.
	_, r, raw := get("/api/pages")
	flag := map[string]bool{}
	for _, p := range r.Pages {
		flag[p.ID] = p.Script
	}
	if len(r.Pages) != 4 || !flag[chart.ID] || !flag[ext.ID] || flag[plain.ID] || flag[md.ID] {
		t.Fatalf("list flags %v in %s", flag, raw)
	}
	var generic struct {
		Pages []map[string]any `json:"pages"`
	}
	json.Unmarshal([]byte(raw), &generic)
	n := 0
	for _, p := range generic.Pages {
		if v, ok := p["script"]; ok {
			if v != true {
				t.Errorf("script field = %v", v)
			}
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d pages carry script:true, want 2", n)
	}
	if w := do(s, "GET", "/api/pages/"+chart.ID, userHdr, ""); !strings.Contains(w.Body.String(), `"script":true`) {
		t.Errorf("get: %s", w.Body)
	}

	// has:script alone: the flagged pages, no body scan (no matches).
	code, r, _ := get("/api/pages?q=has:script")
	if code != 200 || len(r.Pages) != 2 || len(r.Matches) != 0 {
		t.Fatalf("has:script: %d pages %d matches %d", code, len(r.Pages), len(r.Matches))
	}
	for _, p := range r.Pages {
		if !p.Script {
			t.Errorf("unflagged page %s listed", p.Title)
		}
	}
	// With words: flagged pages whose text holds them; the plain page and the md page have "chart" too.
	code, r, _ = get("/api/pages?q=has:script+chart")
	if code != 200 || len(r.Pages) != 1 || r.Pages[0].ID != chart.ID || len(r.Matches) != 1 || r.Matches[0].ID != chart.ID {
		t.Fatalf("has:script chart: %d %+v", code, r)
	}
	// Script text is not page text, as before: the word is only in the code.
	code, r, _ = get("/api/pages?q=draw+has:SCRIPT")
	if code != 200 || len(r.Pages) != 0 {
		t.Fatalf("has:script draw: %d %+v", code, r)
	}
	// Words alone are unchanged.
	code, r, _ = get("/api/pages?q=chart")
	if code != 200 || len(r.Pages) != 3 {
		t.Fatalf("chart: %d pages %d", code, len(r.Pages))
	}
	// An agent may not search.
	if w := do(s, "GET", "/api/pages?q=has:script", agentHdr("w2:p1"), ""); w.Code != http.StatusForbidden {
		t.Errorf("agent has:script = %d", w.Code)
	}
}

// A page kept before the flag existed gets it when the store opens, once.
func TestPagesScriptBackfill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pages")
	st := newPageStore(dir)
	for _, c := range []struct{ key, body string }{{"a", `<p>x</p><script>1</script>`}, {"b", `<p>x</p>`}} {
		if _, _, err := st.publish(pageIn{Key: c.key, Kind: "html", Body: []byte(c.body)}, pageWho{user: true, by: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	// Make the index look as an older hub wrote it: no flag on any version.
	b, _ := os.ReadFile(filepath.Join(dir, "pages.json"))
	var f pagesFile
	json.Unmarshal(b, &f)
	for i := range f.Pages {
		for j := range f.Pages[i].History {
			f.Pages[i].History[j].Script = nil
		}
	}
	old, _ := json.Marshal(f)
	os.WriteFile(filepath.Join(dir, "pages.json"), old, 0o600)

	st2, err := openPageStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range st2.list() {
		got[p.Key] = p.Script
	}
	if !got["a"] || got["b"] {
		t.Fatalf("backfilled flags %v", got)
	}
	saved, _ := os.ReadFile(filepath.Join(dir, "pages.json"))
	var f2 pagesFile
	json.Unmarshal(saved, &f2)
	for _, p := range f2.Pages {
		if p.History[0].Script == nil {
			t.Errorf("%s: backfill not saved", p.Key)
		}
	}
}
