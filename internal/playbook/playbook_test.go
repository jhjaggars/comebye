package playbook

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		"comebye handoff", "comebye ledger", "comebye read", "comebye mark", "--reason", "## Intent", "## Spec", "out of scope", "definition of done", "branch name",
		"Never edit code", "Never close, remove or launch", "Never prompt yourself", "confirm in the app",
		"home agent", "concise", "comebye todo add", "comebye todo list", "todo answer", "docs/lifecycle.md", "--note", "landed", "Never land on your own",
	} {
		if !strings.Contains(gen, want) {
			t.Errorf("playbook lacks %q", want)
		}
	}

	for _, bad := range []string{"briefing", "comebye brief ", "Needs you"} {
		if strings.Contains(gen, bad) {
			t.Errorf("playbook still mentions %q", bad)
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
	if got := strings.Join(AgentArgs("claude", "coordinator", ""), " "); got != "--allowedTools Bash(comebye:*)" {
		t.Fatalf("claude args %q", got)
	}
	if got := strings.Join(AgentArgs("pi", "coordinator-2", ""), " "); got != "--name coordinator-2 --no-skills --no-context-files --tools read,grep,find,ls,bash" {
		t.Fatalf("pi args %q", got)
	}
	if AgentArgs("codex", "c", "") != nil {
		t.Fatal("codex args")
	}
	for kind, h := range map[string]string{"pi": "pi", "claude": "claude", "codex": "generic", "opencode": "generic"} {
		p := FirstPrompt(kind, "")
		if !strings.Contains(p, "comebye playbook --harness "+h) || len(p) > 300 {
			t.Errorf("first prompt for %s: %q", kind, p)
		}
	}
}

func TestCoordinatorDirPieces(t *testing.T) {
	dir := t.TempDir()
	// An empty directory: today's arguments and prompt.
	if got := strings.Join(AgentArgs("pi", "c", dir), " "); got != "--name c --no-skills --no-context-files --tools read,grep,find,ls,bash" {
		t.Fatalf("pi args, empty dir: %q", got)
	}
	if p := FirstPrompt("claude", dir); !strings.Contains(p, "comebye playbook --harness claude") {
		t.Fatalf("first prompt, empty dir: %q", p)
	}
	// AGENTS.md and skills/: pi is handed both next to --no-skills.
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "skills"), 0o755)
	want := "--name c --no-skills --no-context-files --tools read,grep,find,ls,bash --append-system-prompt " + dir + "/AGENTS.md --skill " + dir + "/skills"
	if got := strings.Join(AgentArgs("pi", "c", dir), " "); got != want {
		t.Fatalf("pi args = %q\nwant      %q", got, want)
	}
	// Only the skills folder: only --skill.
	os.Remove(filepath.Join(dir, "AGENTS.md"))
	if got := strings.Join(AgentArgs("pi", "c", dir), " "); !strings.HasSuffix(got, "--skill "+dir+"/skills") || strings.Contains(got, "--append-system-prompt") {
		t.Fatalf("pi args without AGENTS.md = %q", got)
	}
	// Claude's arguments do not depend on the directory (it starts there).
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644)
	if got := strings.Join(AgentArgs("claude", "c", dir), " "); got != "--allowedTools Bash(comebye:*)" {
		t.Fatalf("claude args = %q", got)
	}
	for _, kind := range []string{"pi", "claude", "codex"} {
		p := FirstPrompt(kind, dir)
		if !strings.HasPrefix(p, "Read AGENTS.md, then run `comebye whoami`, `comebye todo list`, `comebye ledger --active`") {
			t.Errorf("first prompt for %s: %q", kind, p)
		}
	}
}
