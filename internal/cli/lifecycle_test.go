package cli

import (
	"os"
	"strings"
	"testing"
)

// The push and report rules are stated to workers twice: in the handoff
// template (internal/server) and in the land prompt. docs/lifecycle.md is
// the canonical statement; this keeps the two prompts saying the same.
func TestTemplateAndLandPromptAgree(t *testing.T) {
	src, err := os.ReadFile("../server/handoff.go")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := string(src)
	land := landPrompt("main", false, "", 0)
	for _, rule := range []struct{ template, land string }{
		{"fast-forward pushes only", "Push as a fast-forward"},
		{"never force-push", "Never force-push"},
		{"never rewrite pushed history", "never rewrite pushed history"},
		{"plain commits", "as plain commits"},
		{"Verified live / Unit tests only / Not verified", "Verified live / Unit tests only / Not verified"},
		{"comebye report -", "comebye report -"},
	} {
		if !strings.Contains(tmpl, rule.template) {
			t.Errorf("the handoff template lacks %q", rule.template)
		}
		if !strings.Contains(land, rule.land) {
			t.Errorf("the land prompt lacks %q", rule.land)
		}
	}
	doc, err := os.ReadFile("../../docs/lifecycle.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Verified live", "Unit tests only", "Not verified", "fast-forward", "never force", "plain"} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("docs/lifecycle.md lacks %q", want)
		}
	}
}

func TestLandPromptWaitsWhenQueued(t *testing.T) {
	up := landPrompt("main", false, "", 0)
	if strings.Contains(up, "QUEUED") || !strings.Contains(up, "You hold the landing slot") {
		t.Fatalf("first in line should land now: %q", up)
	}
	q := landPrompt("main", false, "", 2)
	for _, want := range []string{"QUEUED: 2 landing(s) are ahead", "You're up", "Do NOT push", "moved, rebase again"} {
		if !strings.Contains(q, want) {
			t.Errorf("queued prompt lacks %q", want)
		}
	}
}
