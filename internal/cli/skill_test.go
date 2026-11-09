package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/pageskit"
)

// withHome points the env's HOME at a temp dir (never the real one).
func withHome(t *testing.T, e *Env) string {
	t.Helper()
	home := t.TempDir()
	get := e.Getenv
	e.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return get(k)
	}
	return home
}

func TestPagesGuideAndNewNeedNoHub(t *testing.T) {
	f, e, out, errb := newFake(t)
	if code := run(e, "pages", "guide"); code != 0 || out.String() != pageskit.Guide() {
		t.Fatalf("guide: %d %q", code, errb)
	}
	out.Reset()
	if code := run(e, "pages", "new", "--type", "dashboard", "--title", "Fleet <load>"); code != 0 {
		t.Fatalf("new: %d %s", code, errb)
	}
	want, _ := pageskit.Starter("dashboard", "Fleet <load>")
	if out.String() != want || !strings.Contains(out.String(), "<title>Fleet &lt;load&gt;</title>") {
		t.Fatal("new did not print the dashboard starter")
	}
	out.Reset()
	if code := run(e, "pages", "new"); code != 0 || !strings.Contains(out.String(), "starter: doc") {
		t.Fatalf("new without a type: %d", code)
	}
	for _, args := range [][]string{{"pages", "new", "--type", "slides"}, {"pages", "new", "x.html"}, {"pages", "guide", "extra"}} {
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("guide/new called the hub: %v", f.reqs)
	}
}

func TestSkillInstallAndStatus(t *testing.T) {
	_, e, out, errb := newFake(t)
	home := withHome(t, e)
	claude := filepath.Join(home, ".claude", "skills", "comebye-pages", "SKILL.md")
	pi := filepath.Join(home, ".pi", "agent", "skills", "comebye-pages", "SKILL.md")

	if code := run(e, "skill", "status"); code != 0 || !strings.Contains(out.String(), "claude  missing") || !strings.Contains(errb.String(), "comebye skill install") {
		t.Fatalf("status before: %d %q %q", code, out, errb)
	}
	out.Reset()
	if code := run(e, "skill", "install"); code != 0 {
		t.Fatalf("install: %d %s", code, errb)
	}
	if !strings.Contains(out.String(), "claude  installed  "+claude) || !strings.Contains(out.String(), "pi      installed  "+pi) {
		t.Fatalf("install output:\n%s", out)
	}
	for _, p := range []string{claude, pi} {
		if b, _ := os.ReadFile(p); string(b) != pageskit.Guide() {
			t.Fatalf("%s is not the skill", p)
		}
	}
	// Again: nothing changes.
	out.Reset()
	if code := run(e, "skill", "install", "--json"); code != 0 {
		t.Fatalf("again: %d", code)
	}
	var r struct {
		Skill   string            `json:"skill"`
		Targets []pageskit.Result `json:"targets"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Skill != "comebye-pages" || len(r.Targets) != 2 || r.Targets[0].State != "current" || r.Targets[1].State != "current" {
		t.Fatalf("again: %s %v", out, err)
	}
	// An edit of yours stays, with a note saying how to replace it.
	os.WriteFile(claude, []byte("mine\n"), 0o644)
	out.Reset()
	errb.Reset()
	if code := run(e, "skill", "install", "--claude"); code != 0 || !strings.Contains(out.String(), "claude  edited") || !strings.Contains(errb.String(), "--force") {
		t.Fatalf("edited: %d %q %q", code, out, errb)
	}
	if b, _ := os.ReadFile(claude); string(b) != "mine\n" {
		t.Fatal("an edited skill was replaced")
	}
	errb.Reset()
	if code := run(e, "skill", "install", "--claude", "--force"); code != 0 || !strings.Contains(errb.String(), claude+".bak") {
		t.Fatalf("force: %d %q", code, errb)
	}
	if b, _ := os.ReadFile(claude + ".bak"); string(b) != "mine\n" {
		t.Fatal("force lost the user's copy")
	}
}

func TestSkillMistakes(t *testing.T) {
	_, e, _, errb := newFake(t)
	// No HOME: refuse rather than guess (the test env has none).
	if code := run(e, "skill", "install"); code != 1 || !strings.Contains(errb.String(), "HOME is not set") {
		t.Fatalf("no HOME: %d %q", code, errb)
	}
	withHome(t, e)
	for _, args := range [][]string{{"skill"}, {"skill", "remove"}, {"skill", "status", "--force"}, {"skill", "install", "extra"}} {
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
