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
		Board: []Lane{
			{Key: "running", Name: "Running", Count: 1, Items: []LaneItem{{Title: "Slice 1 worker", Status: "running", Since: now.Add(-3 * time.Hour)}}},
			{Key: "waiting", Name: "Waiting on others", Count: 0},
			{Key: "landed", Name: "Landed, recent", Count: 1, Items: []LaneItem{{Title: "Passkey slice 4", Status: "landed", Since: now.Add(-26 * time.Hour)}}},
		},
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
		Marker, "comebye playbook --harness claude", "MEMORY.md", "## Board", "Running (1)", "- Slice 1 worker [running] 3h", "Landed, recent (1)", "comebye board",
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
	for _, bad := range []string{"Briefing (current text)", "brief get", "Waiting on others"} {
		if strings.Contains(out, bad) {
			t.Errorf("digest has %q", bad)
		}
	}
	if strings.Contains(out, "<system-reminder>") {
		t.Error("no transcript text belongs in a digest")
	}
}

func TestBuildEmpty(t *testing.T) {
	out := Build(Input{Now: now})
	for _, want := range []string{Marker, "(none)", "(no state file)", "--harness generic"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty digest lacks %q", want)
		}
	}
}

// The digest never passes MaxBytes, however much is fed in, and keeps
// the end of the state file, not its start.
func TestBuildSizeCap(t *testing.T) {
	in := full()
	for i := 0; i < 400; i++ {
		in.Board[0].Items = append(in.Board[0].Items, LaneItem{Title: strings.Repeat("board title ", 40), Status: "running", Since: now})
	}
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

func TestHubDidSection(t *testing.T) {
	in := full()
	for i := 0; i < 12; i++ {
		kind := "park"
		if i%3 == 0 {
			kind = "landed"
		}
		in.Hub = append(in.Hub, HubEvent{At: now.Add(-time.Duration(i+1) * time.Minute), Kind: kind, Text: fmt.Sprintf("event number %d", i)})
	}
	out := Build(in)
	i := strings.Index(out, "## The hub handled since your last turn")
	if i < 0 {
		t.Fatalf("no section:\n%s", out)
	}
	sec := out[i:]
	if j := strings.Index(sec[3:], "\n## "); j >= 0 {
		sec = sec[:j+3]
	}
	for _, want := range []string{"12 things", "4 landed, 8 park", "- 1m ago [landed] event number 0", "event number 7", "`comebye log --since 24h` for the rest"} {
		if !strings.Contains(sec, want) {
			t.Errorf("section lacks %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "event number 8") {
		t.Errorf("more than 8 lines:\n%s", sec)
	}
	if len(sec) > hubBudget+200 {
		t.Errorf("section is %d bytes", len(sec))
	}
	// Nothing handled: no section.
	if strings.Contains(Build(full()), "The hub handled") {
		t.Error("an empty section is shown")
	}
}

func TestBuildScopedSaysWhoseWorkItIs(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	out := Build(Input{Now: now, Harness: "claude", Name: "proj-a", Scoped: true,
		Others: "- coordinator (main): 2 handoffs, 1 to-dos, 0 plans open",
		Plans:  []Plan{{ID: "pl1", Title: "Split the API", Size: 3, Done: 1}}})
	for _, want := range []string{"## Whose work this is", "You are proj-a, one of several coordinators", "yours alone", "- coordinator (main): 2 handoffs", "## Open plans", "`pl1` Split the API (1 of 3 done)"} {
		if !strings.Contains(out, want) {
			t.Errorf("digest lacks %q:\n%s", want, out)
		}
	}
	main := Build(Input{Now: now, Harness: "claude", Name: "coordinator", Scoped: true, Main: true})
	if !strings.Contains(main, "the main coordinator") || strings.Contains(main, "Open plans") {
		t.Errorf("main's digest:\n%s", main)
	}
	if plain := Build(Input{Now: now, Harness: "claude"}); strings.Contains(plain, "Whose work") {
		t.Errorf("an unscoped digest says whose work it is:\n%s", plain)
	}
}

func TestHubDidCountsRelaysPerAgent(t *testing.T) {
	in := full()
	in.Hub = []HubEvent{
		{At: now.Add(-time.Minute), Kind: "relay", Who: "sweep-rebased", Text: `relayed t1 → sweep-rebased ("ok 1"), closed`},
		{At: now.Add(-2 * time.Minute), Kind: "relay", Who: "sweep-rebased", Text: `relayed t2 → sweep-rebased ("ok 2"), closed`},
		{At: now.Add(-3 * time.Minute), Kind: "relay", Who: "alpha", Text: `relayed t3 → alpha ("yes"), closed`},
		{At: now.Add(-4 * time.Minute), Kind: "relay", Who: "beta", Text: `queued t4 for beta until it can take it (parked)`},
	}
	out := Build(in)
	for _, want := range []string{"- the hub relayed 2 answers to sweep-rebased", "- the hub relayed 1 answer to alpha"} {
		if !strings.Contains(out, want) {
			t.Errorf("digest lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "answer to beta") || strings.Contains(out, "answers to beta") {
		t.Errorf("a queued relay is counted as delivered:\n%s", out)
	}
}
