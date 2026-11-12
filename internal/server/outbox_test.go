package server

import (
	"strings"
	"testing"
	"time"
)

// A worker that files two explicit reports has a "finished" key per report
// (finished/<id>/1, /2) and the same line for both: the digest listed the
// handoff twice (hf43984137a46 in notice fe29c8). One line per handoff and
// event.
func TestOutboxOneLineForTheSameEntryAndText(t *testing.T) {
	o := openOutbox("")
	line := "handoff h1 (branch feat/x in proj on desk) finished with no decisions for the user."
	add := func(key, text string) bool {
		return o.add(outLine{Key: "finished/" + key, Kind: "finished", Entry: "h1", Text: text, At: time.Now()})
	}
	if !add("h1/1", line) {
		t.Fatal("first line not queued")
	}
	if add("h1/2", line) {
		t.Fatal("same entry and text queued under a second key")
	}
	if n := len(o.pending()); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
	// Another handoff's identical-looking line is its own.
	o.add(outLine{Key: "finished/h2/1", Kind: "finished", Entry: "h2", Text: strings.Replace(line, "h1", "h2", 1), At: time.Now()})
	if n := len(o.pending()); n != 2 {
		t.Fatalf("pending = %d, want 2", n)
	}
	// Once sent, a later round's report is a new line (its key is new).
	o.settle(o.pending())
	if !add("h1/3", line) {
		t.Fatal("a later report after the digest went out should queue a line")
	}
}

// An outbox file written before the fix may hold repeats: the notice and the
// digest text say each line once.
func TestOutboxTextDropsRepeatedLines(t *testing.T) {
	l := outLine{Key: "a", Text: "handoff h1 finished"}
	txt := outboxText([]outLine{l, {Key: "b", Text: l.Text}, {Key: "c", Text: "handoff h2 finished"}})
	if n := strings.Count(txt, "- handoff h1 finished"); n != 1 || !strings.Contains(txt, "h2") {
		t.Fatalf("text = %q", txt)
	}
	if one := outboxText([]outLine{l, {Key: "b", Text: l.Text}}); strings.Contains(one, "digest") {
		t.Fatalf("two identical lines should read as a single notice: %q", one)
	}
}
