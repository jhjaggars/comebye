package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckBrief(t *testing.T) {
	open := Todo{ID: "t0123456789", Question: "ship it?", State: TodoOpen}
	answered := Todo{ID: "tabcdef0123", Question: "relay me", State: TodoAnswered}
	items := []Todo{open, answered}
	cases := []struct {
		name  string
		md    string
		items []Todo
		fail  string // substring of the refusal; "" passes
		warn  string // substring of a warning
	}{
		{"no heading", "## proj\n**Finished:** x\n", items, "", ""},
		{"line with id", "## Needs you\n- ship it? t0123456789\n- relay me (tabcdef0123)\n", items, "", ""},
		{"line without id", "## Needs you\n- ship it? t0123456789\n- what about the key?\n", items, "no item id: - what about the key?", ""},
		{"unknown id", "## Needs you\n- ship it? t9999999999\n", items, "not an active item", ""},
		{"closed id", "## Needs you\n- old t0123456789\n", nil, "not an active item", ""}, // only active items are passed in
		{"nothing open, none open", "## Needs you\nNothing open.\n", nil, "", ""},
		{"bulleted none", "## Needs you\n- None.\n", nil, "", ""},
		{"bold none", "## Needs you\n**Nothing open.**\n", nil, "", ""},
		{"nothing but items open", "## Needs you\nNothing open.\n", items, "no item id: Nothing open.", ""},
		{"empty section", "## Needs you\n\n## Next\nx\n", items, "", "not in \"Needs you\""},
		{"bold heading", "**Needs you**\n- q t0123456789\n\n**Next**\nplain text\n", items, "", ""},
		{"wrapped bullet", "## Needs you\n- ship it? the long\n  question t0123456789\n- relay t0123456789 tabcdef0123\n", items, "", ""},
		{"section ends at next heading", "## Needs you\n- q t0123456789\n## Next\n- a line with no id\n", items, "", ""},
		{"code fence ignored", "## Needs you\n- q t0123456789\n```\nnot checked\n```\n", items, "", ""},
		{"case and colon", "### needs you:\n- q t0123456789\n", items, "", ""},
		{"warn missing open", "## Needs you\n- relay t0123456789\n", []Todo{open, {ID: "tffffffffff", Question: "other one", State: TodoOpen}}, "", "tffffffffff"},
		{"answered not warned", "## Needs you\n- q t0123456789\n", items, "", ""},
		{"rule ends section", "## Needs you\n- q t0123456789\n---\nfooter text\n", items, "", ""},
		{"text id in word", "## Needs you\n- fat0123456789x\n", items, "no item id", ""},
	}
	for _, c := range cases {
		fail, warns := checkBrief(c.md, c.items)
		switch {
		case c.fail == "" && fail != "":
			t.Errorf("%s: refused: %s", c.name, fail)
		case c.fail != "" && !strings.Contains(fail, c.fail):
			t.Errorf("%s: refusal = %q, want it to contain %q", c.name, fail, c.fail)
		}
		if c.warn != "" && !strings.Contains(strings.Join(warns, "|"), c.warn) {
			t.Errorf("%s: warnings = %v, want %q", c.name, warns, c.warn)
		}
		if c.warn == "" && len(warns) > 0 && c.fail == "" && c.name != "empty section" {
			t.Errorf("%s: unexpected warnings %v", c.name, warns)
		}
	}
}

func TestBriefSetChecksNeedsYou(t *testing.T) {
	s, _, _ := todoServer(t)
	w := do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"ship it?"}`)
	id := todoOf(t, w.Body.Bytes()).ID
	post := func(md string, noCheck bool) (int, string) {
		b, _ := json.Marshal(map[string]any{"markdown": md, "no_check": noCheck})
		w := do(s, "POST", "/api/brief", agentHdr("w1:p1"), string(b))
		return w.Code, w.Body.String()
	}
	if code, body := post("## Needs you\n- ship it?\n", false); code != 422 || !strings.Contains(body, "needs_you_unlinked") || !strings.Contains(body, "comebye todo add") {
		t.Fatalf("unlinked = %d %s", code, body)
	}
	// Nothing was published.
	if g := do(s, "GET", "/api/brief", userHdr, ""); !strings.Contains(g.Body.String(), `"brief":null`) {
		t.Fatalf("published anyway: %s", g.Body)
	}
	if code, body := post("## Needs you\n- ship it? "+id+"\n", false); code != 200 || strings.Contains(body, "warnings") {
		t.Fatalf("linked = %d %s", code, body)
	}
	if code, body := post("## Needs you\nNothing open.\n", false); code != 422 {
		t.Fatalf("nothing while open = %d %s", code, body)
	}
	// An open item left out is a warning, not a refusal.
	if code, body := post("## Needs you\n- relay tabcdef0123\n", true); code != 200 {
		t.Fatalf("no_check = %d %s", code, body)
	}
	w = do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"second?"}`)
	id2 := todoOf(t, w.Body.Bytes()).ID
	if code, body := post("## Needs you\n- ship it? "+id+"\n", false); code != 200 || !strings.Contains(body, `"warnings"`) || !strings.Contains(body, id2) {
		t.Fatalf("warn = %d %s", code, body)
	}
	if code, _ := post("## proj\nno heading\n", false); code != 200 {
		t.Fatalf("no heading = %d", code)
	}
}
