// Package digest builds the coordinator's fresh-start digest: a compact
// Markdown summary of what a coordinator needs to carry on after its
// context was cleared (the hub's `/clear` plus a SessionStart hook that
// injects this text). It holds pointers and one-line states, never
// transcript text: the full records stay where they are (the ledger, the
// to-do list, the coordinator's own state file and memory) and the digest
// says how to read them.
package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Marker opens every digest. A session whose transcript holds it received
// the digest (the hub checks that after a reset).
const Marker = "comebye-digest:v1"

// Size limits. A token is about 3.5 bytes of this text, so MaxBytes is
// about 7k tokens.
const (
	MaxBytes = 24 << 10
	// StateTailBytes is how much of the coordinator's state file is kept.
	StateTailBytes = 8 << 10

	briefBudget   = 5 << 10
	todoBudget    = 4 << 10
	handoffBudget = 4 << 10
	eventBudget   = 3 << 10
)

// Brief is the published briefing.
type Brief struct {
	Markdown string
	By       string
	At       time.Time
}

// Todo is an open (or answered, not yet relayed) to-do item.
type Todo struct {
	ID       string
	Kind     string
	State    string
	Question string
	Waiting  string // the agent waiting on it
	Answer   string // a short note that an answer is in, "" when none
	At       time.Time
}

// Handoff is a running or landing handoff.
type Handoff struct {
	ID      string
	Repo    string
	Branch  string
	Worker  string
	Host    string
	Status  string
	State   string // a one-line state: agent working, landing verdict, report waiting
	Updated time.Time
}

// Event is one thing that happened in the ledger since the last reset.
type Event struct {
	At   time.Time
	Text string
}

// Input is everything a digest is made of. The caller gathers it; Build
// only shapes it, so it is testable without a hub.
type Input struct {
	Now      time.Time
	Agent    string // the coordinator's id
	Harness  string // playbook harness: claude, pi, generic
	Brief    *Brief
	Todos    []Todo
	Handoffs []Handoff
	Events   []Event
	// LastReset is when the coordinator was last reset (zero: never).
	LastReset time.Time
	// StateTail is the end of the coordinator's state file; StatePath is
	// its path and StateSize the file's full size in bytes.
	StateTail string
	StatePath string
	StateSize int
	// MemoryIndex is the path of the memory index the coordinator loads
	// ("" when there is none).
	MemoryIndex string
}

// Build renders the digest, at most MaxBytes.
func Build(in Input) string {
	if in.Harness == "" {
		in.Harness = "generic"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- %s -->\n# Fresh start: your context was cleared\n\n", Marker)
	fmt.Fprintf(&b, "The Comebye hub cleared your context%s (it had grown large). You are still the same coordinator%s, in the same pane, with the same role. This digest is what you need to carry on; it holds pointers and one-line states, not transcripts. Read the full record where it says to.\n\n",
		since(in.LastReset, in.Now), who(in.Agent))

	b.WriteString("## Start here\n")
	fmt.Fprintf(&b, "- Playbook: run `comebye playbook --harness %s` and follow it.\n", in.Harness)
	b.WriteString("- Then `comebye whoami`, `comebye todo list`, `comebye ledger --active`, and `comebye brief get`.\n")
	if in.MemoryIndex != "" {
		fmt.Fprintf(&b, "- Memory index: `%s` (loaded with your context; read the entries it points to when they matter).\n", in.MemoryIndex)
	}
	b.WriteString("\n")

	b.WriteString(section("## Briefing (current text)", briefing(in), briefBudget))
	b.WriteString(section("## Open to-dos", todos(in), todoBudget))
	b.WriteString(section("## Running and landing handoffs", handoffs(in), handoffBudget))
	b.WriteString(section("## Ledger events since the last reset", events(in), eventBudget))

	head := b.String()
	// The state tail takes what is left, up to its own limit.
	room := MaxBytes - len(head) - 512
	if room > StateTailBytes {
		room = StateTailBytes
	}
	b.WriteString(stateSection(in, room))
	out := b.String()
	if len(out) > MaxBytes { // cannot happen with the budgets; a backstop
		out = cutUTF8(out, MaxBytes-40) + "\n\n(digest cut at its size limit)\n"
	}
	return out
}

func who(agent string) string {
	if agent == "" {
		return ""
	}
	return " (" + agent + ")"
}

func since(last, now time.Time) string {
	if last.IsZero() {
		return ""
	}
	return " (the previous fresh start was " + Age(last, now) + " ago)"
}

// Age prints how long ago t was, coarsely: 5m, 3h, 2d.
func Age(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func section(title, body string, budget int) string {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		body = "(none)"
	}
	if len(body) > budget {
		body = cutLines(body, budget) + "\n… (cut to fit; the full list is one command away)"
	}
	return title + "\n" + body + "\n\n"
}

func briefing(in Input) string {
	b := in.Brief
	if b == nil || strings.TrimSpace(b.Markdown) == "" {
		return "No briefing has been published. Publish one with `comebye brief set`."
	}
	return fmt.Sprintf("Published %s ago by %s:\n\n%s", Age(b.At, in.Now), orDash(b.By), strings.TrimSpace(b.Markdown))
}

func todos(in Input) string {
	var out []string
	for _, t := range in.Todos {
		line := fmt.Sprintf("- `%s` [%s, %s, %s ago] %s", t.ID, t.Kind, t.State, Age(t.At, in.Now), oneLine(t.Question, 160))
		if t.Waiting != "" {
			line += " (waiting: " + t.Waiting + ")"
		}
		if t.Answer != "" {
			line += " — " + t.Answer
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func handoffs(in Input) string {
	hs := append([]Handoff(nil), in.Handoffs...)
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].Updated.After(hs[j].Updated) })
	var out []string
	for _, h := range hs {
		line := fmt.Sprintf("- `%s` %s %s", h.ID, orDash(h.Repo), orDash(h.Branch))
		if h.Worker != "" {
			line += " — worker " + h.Worker
		}
		line += " — " + h.Status
		if h.State != "" {
			line += ": " + oneLine(h.State, 140)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func events(in Input) string {
	es := append([]Event(nil), in.Events...)
	sort.SliceStable(es, func(i, j int) bool { return es[i].At.After(es[j].At) })
	var out []string
	for _, e := range es {
		out = append(out, fmt.Sprintf("- %s ago: %s", Age(e.At, in.Now), oneLine(e.Text, 160)))
	}
	return strings.Join(out, "\n")
}

// stateSection renders the tail of the coordinator's state file within
// room bytes, starting at a line boundary (a heading when there is one).
func stateSection(in Input, room int) string {
	title := "## Your state file (the tail)"
	if in.StatePath == "" || strings.TrimSpace(in.StateTail) == "" {
		if in.StatePath != "" {
			return title + "\n" + "`" + in.StatePath + "` is empty or missing.\n"
		}
		return title + "\n(no state file)\n"
	}
	tail := in.StateTail
	if room < 512 {
		room = 512
	}
	cut := len(tail) < in.StateSize
	if len(tail) > room {
		tail, cut = tail[len(tail)-room:], true
	}
	if cut {
		tail = fromLine(tail)
	}
	note := fmt.Sprintf("The last %d of %d bytes of `%s` (read the whole file when you need an older entry).", len(tail), max(in.StateSize, len(tail)), in.StatePath)
	if !cut {
		note = fmt.Sprintf("All of `%s` (%d bytes).", in.StatePath, len(tail))
	}
	return title + "\n" + note + "\n\n" + strings.TrimRight(tail, "\n") + "\n"
}

// fromLine drops the first, probably partial, line, preferring to start at
// the next heading when one is near.
func fromLine(s string) string {
	s = strings.ToValidUTF8(s, "")
	if i := strings.Index(s, "\n## "); i >= 0 && i < len(s)/2 {
		return s[i+1:]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = cutUTF8(s, n-1) + "…"
	}
	return s
}

// cutLines keeps whole lines up to n bytes.
func cutLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = cutUTF8(s, n)
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return s
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
