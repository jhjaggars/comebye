package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLatestOfLastTextAndPending(t *testing.T) {
	path := writeSession(t,
		`{"type":"session","version":3,"id":"s1"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix it"}]}}`,
		`{"type":"message","id":"a1","timestamp":"T1","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"a.go"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"tc1","content":[{"type":"text","text":"..."}]}}`,
		`{"type":"message","id":"a2","timestamp":"T2","message":{"role":"assistant","content":[{"type":"text","text":"I will run the tests."},{"type":"toolCall","id":"tc2","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
		`{"type":"custom","customType":"permission-gate-state","data":{}}`,
	)
	l, err := LatestOf(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Text != "I will run the tests." || l.Time != "T2" {
		t.Fatalf("text = %q time = %q", l.Text, l.Time)
	}
	if len(l.Pending) != 1 || l.Pending[0].Tool != "bash" || l.Pending[0].Args != "go test ./..." {
		t.Fatalf("pending = %+v", l.Pending)
	}
}

func TestLatestOfResolvedAndToolOnlyTurns(t *testing.T) {
	path := writeSession(t,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"Should I also update the docs?"}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"ls"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"tc1","content":[{"type":"text","text":"x"}]}}`,
		`{"type":"message","id":"a3","message":{"role":"assistant","content":[{"type":"text","text":"   "}]}}`,
	)
	l, err := LatestOf(path)
	if err != nil {
		t.Fatal(err)
	}
	// A tool-only or blank turn does not erase the last real text.
	if l.Text != "Should I also update the docs?" {
		t.Fatalf("text = %q", l.Text)
	}
	if len(l.Pending) != 0 {
		t.Fatalf("pending = %+v", l.Pending)
	}
}

func TestLatestOfTailWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	filler := `{"type":"custom","customType":"pad","data":"` + strings.Repeat("x", 1000) + `"}` + "\n"
	var b strings.Builder
	b.WriteString(`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n")
	for b.Len() < tailWindow+5000 {
		b.WriteString(filler)
	}
	b.WriteString(`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LatestOf(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Text != "new" {
		t.Fatalf("text = %q", l.Text)
	}
}

func TestLatestOfMissing(t *testing.T) {
	if _, err := LatestOf(filepath.Join(t.TempDir(), "nope.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

func TestExcerpt(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello\n\n  world", 50, "hello world"},
		{"héllo wörld", 5, "héllo…"},
		{"abc", 0, ""},
		{"abc def", 4, "abc…"},
	}
	for _, c := range cases {
		if got := Excerpt(c.in, c.n); got != c.want {
			t.Errorf("Excerpt(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestLatestFromMessagesMatchesLatestOf(t *testing.T) {
	path := writeSession(t,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix it"}]}}`,
		`{"type":"message","id":"a1","timestamp":"T1","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"a.go"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"tc1","durationMs":3,"content":[{"type":"text","text":"..."}]}}`,
		`{"type":"message","id":"a2","timestamp":"T2","message":{"role":"assistant","content":[{"type":"text","text":"I will run the tests."},{"type":"toolCall","id":"tc2","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
	)
	tr, err := Read(path, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	l := LatestFromMessages(tr.Messages)
	if l.Text != "I will run the tests." || l.Time != "T2" || len(l.Pending) != 1 || l.Pending[0].Tool != "bash" {
		t.Fatalf("latest = %+v", l)
	}
	// A new user turn leaves nothing pending.
	l = LatestFromMessages(append(tr.Messages, Message{Kind: "user", Text: "go"}))
	if len(l.Pending) != 0 || l.Text != "I will run the tests." {
		t.Fatalf("after user turn: %+v", l)
	}
}
