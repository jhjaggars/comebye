package question

import (
	"reflect"
	"strings"
	"testing"
)

// A reconstruction of the design thread that ended a turn with five
// numbered decisions and a trailing question: the user saw only the last.
const provenance = "I put three mockups on the Pages screen. They share one data model.\n\n" +
	"## Decisions for the user\n\n" +
	"1. **Where should the provenance live?** In the artifact header, or as a footer line?\n" +
	"   1. Header chip - always visible, costs a row\n" +
	"   2. Footer line - quieter, easy to miss\n" +
	"   Recommend: 1\n" +
	"2. **Should the chip show the full commit SHA or seven characters?**\n" +
	"3. **Do we keep provenance for artifacts published before this change?** They have no record.\n" +
	"   1. Backfill from git log\n" +
	"   2. Show \"unknown\"\n" +
	"   My recommendation: ok 2\n" +
	"4. Which hosts get the badge: only remote ones, or every host?\n" +
	"5. Should clicking the chip open the source artifact in a new tab?\n\n" +
	"Or should I change any of the mockups first?"

func TestExtractProvenanceThread(t *testing.T) {
	ex := Extract(provenance)
	if len(ex.Items) != 6 {
		t.Fatalf("want 6 items, got %d: %+v", len(ex.Items), ex.Items)
	}
	if ex.Lead != "I put three mockups on the Pages screen. They share one data model." {
		t.Fatalf("lead = %q", ex.Lead)
	}
	a := ex.Items[0]
	if a.Question != "Where should the provenance live? In the artifact header, or as a footer line?" {
		t.Fatalf("question = %q", a.Question)
	}
	if want := []Option{{"Header chip", "always visible, costs a row"}, {"Footer line", "quieter, easy to miss"}}; !reflect.DeepEqual(a.Options, want) {
		t.Fatalf("options = %+v", a.Options)
	}
	if !reflect.DeepEqual(a.Recommend, []int{1}) || strings.Contains(a.Detail, "Recommend") || strings.Contains(a.Detail, "Header chip") {
		t.Fatalf("recommend %v, detail %q: the choices belong in the fields, not the text", a.Recommend, a.Detail)
	}
	c := ex.Items[2]
	if !reflect.DeepEqual(c.Recommend, []int{2}) || len(c.Options) != 2 || c.Options[1].Label != `Show "unknown"` {
		t.Fatalf("item 3: %+v", c)
	}
	if !strings.Contains(c.Question, "keep provenance") || !strings.Contains(c.Question, "They have no record") {
		t.Fatalf("item 3 question = %q", c.Question)
	}
	if got := ex.Items[5].Question; got != "Should I change any of the mockups first?" {
		t.Fatalf("tail question = %q", got)
	}
}

func TestExtractSingleQuestion(t *testing.T) {
	ex := Extract("The level test needs a baseline. Should I record it from the current build, or from the last release tag?\n\n1. Current build - fastest\n2. Last release tag - clean\n\nRecommend: 2")
	if len(ex.Items) != 1 {
		t.Fatalf("%+v", ex.Items)
	}
	it := ex.Items[0]
	if it.Question != "Should I record it from the current build, or from the last release tag?" {
		t.Fatalf("question = %q", it.Question)
	}
	if len(it.Options) != 2 || it.Options[1].Label != "Last release tag" || !reflect.DeepEqual(it.Recommend, []int{2}) {
		t.Fatalf("%+v", it)
	}
	if !strings.Contains(it.Detail, "needs a baseline") {
		t.Fatalf("the agent's wording is the context, got %q", it.Detail)
	}
	if got := Extract("All done. Want me to commit it?").Items; len(got) != 1 {
		t.Fatalf("plain question: %+v", got)
	}
}

func TestExtractShapes(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       int
	}{
		{"several question marks", "Done with the edits. Should I commit? Should I also push the branch?", 2},
		{"numbered, no heading", "Before I go on:\n\n1. Should the cache be per user?\n2. Do you want the TTL configurable?\n\nI'll start on the parser.", 2},
		{"questions heading, closing line", "Built.\n\n**Questions**\n\n- Which region is staging in?\n- Can I delete the old migrations?\n\nTake your time.", 2},
		{"decisions then a next-steps section", "## Decisions for the user\n\n1. Keep the CSV format?\n2. Which compression, gzip or zstd?\n\n## Next steps\n\nWaiting.", 2},
		{"none", "## Decisions for the user\n\nNone.", 0},
		{"a list of steps", "I did:\n\n1. Ran it\n2. Fixed it\n\nDone.", 0},
		{"a list with one rhetorical item", "Results:\n\n1. Build passes\n2. Lint passes\n3. Why did it fail? A stale cache\n\nFixed.", 0},
		{"a quote", "> Should I delete it?", 0},
		{"a closer", "Anything else?", 0},
		{"empty", "  ", 0},
	} {
		if got := len(Extract(c.text).Items); got != c.want {
			t.Errorf("%s: want %d items, got %d: %+v", c.name, c.want, got, Extract(c.text).Items)
		}
	}
}

func TestExtractRecommendMarkAndCap(t *testing.T) {
	ex := Extract("## Decisions for the user\n\n1. Which key?\n   a) Mint one (recommended)\n   b) Reuse the old one")
	if len(ex.Items) != 1 || !reflect.DeepEqual(ex.Items[0].Recommend, []int{1}) || ex.Items[0].Options[0].Label != "Mint one" {
		t.Fatalf("%+v", ex.Items)
	}
	var b strings.Builder
	b.WriteString("Questions:\n\n")
	for i := 0; i < 9; i++ {
		b.WriteString("- Should I do thing " + string(rune('a'+i)) + "?\n")
	}
	ex = Extract(b.String())
	if len(ex.Items) != MaxItems || !strings.Contains(ex.Items[MaxItems-1].Detail, "3 more") {
		t.Fatalf("%d items, last detail %q", len(ex.Items), ex.Items[len(ex.Items)-1].Detail)
	}
}

func TestKey(t *testing.T) {
	if Key("Should I commit?") != Key("  should i COMMIT ?! ") || Key("a") == Key("b") {
		t.Fatal("Key")
	}
}
