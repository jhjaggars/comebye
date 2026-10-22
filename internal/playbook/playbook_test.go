package playbook

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	gen, err := Render("")
	if err != nil || gen != Body() || !strings.HasPrefix(gen, "# Comebye coordinator playbook") {
		t.Fatalf("generic: %v", err)
	}
	// The playbook covers the workflows and the rules.
	for _, want := range []string{
		"comebye handoff", "comebye ledger", "comebye read", "comebye mark", "--reason", "comebye brief set",
		"Goal", "Context", "Constraints", "Definition of done", "branch name",
		"Never edit code", "Never close, remove or launch", "Never prompt yourself", "confirms in the app",
		"home agent", "concise",
	} {
		if !strings.Contains(gen, want) {
			t.Errorf("playbook lacks %q", want)
		}
	}

	pi, err := Render("pi")
	if err != nil {
		t.Fatal(err)
	}
	fm, rest, ok := strings.Cut(strings.TrimPrefix(pi, "---\n"), "\n---\n")
	if !strings.HasPrefix(pi, "---\n") || !ok {
		t.Fatalf("pi: no frontmatter:\n%s", pi[:200])
	}
	for _, want := range []string{"name: coordinator", "description: ", "tools: read, grep, find, ls, bash"} {
		if !strings.Contains(fm, want) {
			t.Errorf("pi frontmatter lacks %q:\n%s", want, fm)
		}
	}
	if strings.Contains(fm, "edit") || strings.Contains(fm, "write") || !strings.Contains(rest, "comebye handoff") {
		t.Errorf("pi definition:\n%s", pi)
	}

	cl, err := Render("Claude")
	if err != nil || !strings.HasPrefix(cl, "# Comebye coordinator (Claude Code)") || !strings.Contains(cl, "comebye presets claude") ||
		strings.Count(cl, "# Comebye coordinator") != 1 || !strings.Contains(cl, "comebye handoff") {
		t.Fatalf("claude: %v\n%s", err, cl)
	}
	if _, err := Render("emacs"); err == nil {
		t.Fatal("unknown harness accepted")
	}
}

func TestPresets(t *testing.T) {
	cl, err := Preset("claude")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Permissions struct{ Allow []string } `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(cl), &v); err != nil || len(v.Permissions.Allow) != 1 || v.Permissions.Allow[0] != "Bash(comebye:*)" {
		t.Fatalf("claude preset %q: %v", cl, err)
	}
	if !strings.Contains(PresetNote("claude"), "settings.json") || PresetNote("pi") != "" {
		t.Fatal("notes")
	}
	pi, err := Preset("pi")
	if err != nil || !strings.Contains(pi, "permission-gate") || !strings.Contains(pi, "judge-rules.md") {
		t.Fatalf("pi preset: %v\n%s", err, pi)
	}
	if g, err := Preset("generic"); err != nil || !strings.Contains(g, "comebye") {
		t.Fatalf("generic: %v", err)
	}
	if _, err := Preset("nope"); err == nil {
		t.Fatal("unknown preset accepted")
	}
}

func TestLaunchPieces(t *testing.T) {
	if got := strings.Join(AgentArgs("claude", "coordinator"), " "); got != "--allowedTools Bash(comebye:*)" {
		t.Fatalf("claude args %q", got)
	}
	if got := strings.Join(AgentArgs("pi", "coordinator-2"), " "); got != "--name coordinator-2 --no-skills --no-context-files --tools read,grep,find,ls,bash --auto" {
		t.Fatalf("pi args %q", got)
	}
	if AgentArgs("codex", "c") != nil {
		t.Fatal("codex args")
	}
	for kind, h := range map[string]string{"pi": "pi", "claude": "claude", "codex": "generic", "opencode": "generic"} {
		p := FirstPrompt(kind)
		if !strings.Contains(p, "comebye playbook --harness "+h) || len(p) > 300 {
			t.Errorf("first prompt for %s: %q", kind, p)
		}
	}
}
