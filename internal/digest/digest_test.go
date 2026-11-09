package digest

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func full() Input {
	in := Input{
		Now: now, Agent: "laharl/w21:p1", Harness: "claude",
		Brief:       &Brief{Markdown: "# Briefing\n\n- all well\n", By: "coordinator", At: now.Add(-30 * time.Minute)},
		LastReset:   now.Add(-26 * time.Hour),
		MemoryIndex: "/home/u/.claude/projects/p/memory/MEMORY.md",
		StatePath:   "/home/u/.local/state/comebye/coordinator/state.md",
	}
	for i := 0; i < 5; i++ {
		in.Todos = append(in.Todos, Todo{ID: fmt.Sprintf("t%010d", i), Kind: "decision", State: "open", Question: "Which key?", Waiting: "laharl/w9:p1", At: now.Add(-time.Hour)})
	}
	in.Handoffs = []Handoff{
		{ID: "h1", Repo: "comebye", Branch: "feat/a", Worker: "alpha", Status: "running", State: "agent working", Updated: now.Add(-time.Minute)},
		{ID: "h2", Repo: "comebye", Branch: "feat/b", Worker: "beta", Status: "landing", State: "approved; hub checking", Updated: now.Add(-time.Hour)},
	}
	in.Events = []Event{{At: now.Add(-2 * time.Hour), Text: "h3 finished"}, {At: now.Add(-time.Hour), Text: "h4 landed"}}
	in.StateTail = "## Notes\n- one\n- two\n"
	in.StateSize = len(in.StateTail)
	return in
}

func TestBuildSections(t *testing.T) {
	out := Build(full())
	for _, want := range []string{
		Marker, "comebye playbook --harness claude", "MEMORY.md", "## Briefing (current text)", "all well",
		"## Open to-dos", "t0000000000", "## Running and landing handoffs", "feat/a", "approved; hub checking",
		"## Ledger events since the last reset", "h4 landed", "## Your state file", "All of `/home/u/.local/state/comebye/coordinator/state.md`",
		"the previous fresh start was 26h ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest lacks %q:\n%s", want, out)
		}
	}
	// Newest handoff first.
	if strings.Index(out, "feat/a") > strings.Index(out, "feat/b") {
		t.Error("handoffs are not newest first")
	}
	if strings.Contains(out, "<system-reminder>") {
		t.Error("no transcript text belongs in a digest")
	}
}

func TestBuildEmpty(t *testing.T) {
	out := Build(Input{Now: now})
	for _, want := range []string{Marker, "No briefing has been published", "(none)", "(no state file)", "--harness generic"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty digest lacks %q", want)
		}
	}
}

// The digest never passes MaxBytes, however much is fed in, and keeps
// the end of the state file, not its start.
func TestBuildSizeCap(t *testing.T) {
	in := full()
	in.Brief.Markdown = strings.Repeat("a long briefing line that goes on and on\n", 5000)
	for i := 0; i < 400; i++ {
		in.Todos = append(in.Todos, Todo{ID: fmt.Sprintf("x%010d", i), Kind: "action", State: "open", Question: strings.Repeat("q ", 400), At: now})
		in.Handoffs = append(in.Handoffs, Handoff{ID: fmt.Sprintf("y%d", i), Repo: "r", Branch: "b", Status: "running", State: strings.Repeat("s", 500), Updated: now})
		in.Events = append(in.Events, Event{At: now, Text: strings.Repeat("e", 500)})
	}
	var st strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&st, "## Entry %04d\nnotes for entry %d that take some room on the line\n", i, i)
	}
	in.StateTail, in.StateSize = st.String(), 10*st.Len()
	out := Build(in)
	if len(out) > MaxBytes {
		t.Fatalf("digest is %d bytes, over %d", len(out), MaxBytes)
	}
	if !strings.Contains(out, "Entry 2999") || strings.Contains(out, "Entry 0000") {
		t.Fatal("the state section must be the file's tail")
	}
	if !strings.Contains(out, "cut to fit") {
		t.Error("a cut section says so")
	}
	// About 7k tokens at 3.5 bytes each.
	if tokens := len(out) * 10 / 35; tokens > 8000 {
		t.Fatalf("~%d tokens, over the 8k aim", tokens)
	}
}

func TestStateTailStartsOnALine(t *testing.T) {
	in := Input{Now: now, StatePath: "/s/state.md"}
	var st strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&st, "line %d is here\n", i)
	}
	in.StateTail, in.StateSize = st.String(), st.Len()+1000
	out := Build(in)
	i := strings.Index(out, "## Your state file")
	body := out[i:]
	// Skip the title and note lines; the first state line must be whole.
	lines := strings.Split(body, "\n")
	first := lines[3]
	if !strings.HasPrefix(first, "line ") || !strings.HasSuffix(first, " is here") {
		t.Fatalf("the tail starts mid-line: %q", first)
	}
	if !strings.Contains(lines[1], "The last") {
		t.Fatalf("a cut file says so: %q", lines[1])
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Hour: "0m", 5 * time.Minute: "5m", 3 * time.Hour: "3h", 60 * time.Hour: "2d"} {
		if got := Age(now.Add(-d), now); got != want {
			t.Errorf("Age(-%v) = %s, want %s", d, got, want)
		}
	}
}
