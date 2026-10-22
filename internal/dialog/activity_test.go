package dialog

import (
	"reflect"
	"testing"
)

func TestDetectActivity(t *testing.T) {
	claudeBottom := "\n────────────\n❯ \n────────────\n  Opus 5.5 | main ✓\n  -- INSERT -- ⏵⏵ auto mode on\n"
	cases := []struct {
		name, screen, kind string
		want               *Activity
	}{
		{"claude random verb", "● Reading the pane\n\n· Shimmying… (27s · ↓ 2.1k tokens)\n" + claudeBottom, "claude",
			&Activity{Text: "Shimmying", Note: "↓ 2.1k tokens", Generic: true}},
		{"claude compaction", "✻ Compacting conversation… (1m 5s · esc to interrupt)\n" + claudeBottom, "claude",
			&Activity{Text: "Compacting conversation"}},
		{"claude todo with thinking", "✶ Running the test suite… (12s · ↑ 300 tokens · thinking)\n  ⎿  ☐ Fix the parser\n" + claudeBottom, "claude",
			&Activity{Text: "Running the test suite", Note: "↑ 300 tokens · thinking"}},
		{"claude no parenthetical", "✢ Compacting conversation…\n" + claudeBottom, "claude",
			&Activity{Text: "Compacting conversation"}},
		{"claude idle", "● Done.\n" + claudeBottom, "claude", nil},
		{"pi working", "some output\n⠋ Working... (esc to interrupt)\n\n> \n", "pi",
			&Activity{Text: "Working", Generic: true}},
		{"pi compaction", "⠙ Compacting context... (esc to cancel)\n> \n", "pi",
			&Activity{Text: "Compacting context"}},
		{"pi ignores claude glyphs", "· Shimmying… (2s)\n", "pi", nil},
		{"too far up", "✻ Compacting conversation… (3s)\n" + repeatLines(activityScan+1), "claude", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectActivity(c.screen, c.kind); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func repeatLines(n int) string {
	s := ""
	for range n {
		s += "x\n"
	}
	return s
}
