package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShowDeclaresAndClears(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/show"] = answer{body: `{"ok":true,"path":"/h/doc.md","section":"3. Rollout"}`}
	if code := run(e, "show", "docs/plan.md", "--section", "3. Rollout"); code != 0 {
		t.Fatalf("show: %d %s", code, errb)
	}
	var b map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/show").body), &b)
	abs, _ := filepath.Abs("docs/plan.md")
	if b["agent_id"] != "laharl/w1:p1" || b["path"] != abs || b["section"] != "3. Rollout" {
		t.Fatalf("body = %v", b)
	}
	if !strings.Contains(out.String(), "now opens /h/doc.md") {
		t.Fatalf("stdout = %q", out)
	}
	// A section the file lacks is a warning, not an error.
	out.Reset()
	f.answers["POST /api/show"] = answer{body: `{"ok":true,"path":"/h/doc.md","warning":"no heading matches \"x\""}`}
	if code := run(e, "show", os.DevNull+".md", "--section", "x"); code != 0 || !strings.Contains(out.String(), "Warning: no heading matches") {
		t.Fatalf("warning: %d %q", code, out)
	}
	// Clear.
	out.Reset()
	f.answers["POST /api/show"] = answer{body: `{"ok":true,"cleared":true}`}
	if code := run(e, "show", "--clear"); code != 0 {
		t.Fatalf("clear: %d", code)
	}
	b = map[string]any{}
	json.Unmarshal([]byte(f.find("POST", "/api/show").body), &b)
	if b["clear"] != true || b["path"] != nil {
		t.Fatalf("clear body = %v", b)
	}
	// Bad use.
	for _, args := range [][]string{{"show"}, {"show", "a.md", "b.md"}, {"show", "--clear", "a.md"}} {
		if code := run(e, args...); code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
}
