package modkit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude answers the few claude calls modkit makes and records them.
type fakeClaude struct {
	calls   []string
	markets []marketJSON
	plugins []pluginJSON
}

func (f *fakeClaude) Run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch strings.Join(args[:min(len(args), 3)], " ") {
	case "plugin marketplace list":
		b, _ := json.Marshal(f.markets)
		return string(b), nil
	case "plugin list --json":
		b, _ := json.Marshal(f.plugins)
		return string(b), nil
	case "plugin marketplace add":
		f.markets = append(f.markets, marketJSON{Name: Marketplace, Path: args[3]})
	case "plugin install " + args[min(2, len(args)-1)]:
		f.plugins = append(f.plugins, pluginJSON{ID: args[2]})
	}
	return "", nil
}

func (f *fakeClaude) did(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func opts(t *testing.T, c Claude) Options {
	return Options{Dir: filepath.Join(t.TempDir(), "share", "comebye", "mods"), Claude: c}
}

func TestEmbeddedFiles(t *testing.T) {
	fs, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range fs {
		have[f.Rel] = true
		if strings.Contains(f.Rel, ".test.") {
			t.Errorf("a test file is shipped: %s", f.Rel)
		}
	}
	for _, want := range []string{
		".claude-plugin/marketplace.json",
		"comebye-hub/.claude-plugin/plugin.json", "comebye-hub/hooks/hooks.json", "comebye-hub/hooks/register.tsx",
		"comebye-coord-window/.claude-plugin/plugin.json", "comebye-coord-window/hooks/register.tsx",
	} {
		if !have[want] {
			t.Errorf("missing %s", want)
		}
	}
}

func TestInstallIdempotentAndRegisters(t *testing.T) {
	c := &fakeClaude{}
	o := opts(t, c)
	r, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Marketplace != "registered" || len(r.Installed) != 1 || r.Installed[0] != Hub {
		t.Fatalf("report = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "comebye-hub", "hooks", "register.tsx")); err != nil {
		t.Fatal(err)
	}
	// The early-access mod is not installed unless asked.
	if c.did("plugin install comebye-coord-window") != 0 {
		t.Fatalf("coord-window installed by default: %v", c.calls)
	}
	// Again: nothing is rewritten, added or installed twice.
	n := len(c.calls)
	r, err = Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Files {
		if f.State != Current {
			t.Errorf("%s: %s on a second install", f.Rel, f.State)
		}
	}
	for _, call := range c.calls[n:] {
		if strings.Contains(call, " add ") || strings.Contains(call, "install") {
			t.Errorf("second install ran %q", call)
		}
	}
	// --with adds the coordinator window.
	o.With = []string{"coord-window"}
	r, err = Install(o)
	if err != nil || len(r.Installed) != 2 {
		t.Fatalf("with: %v %+v", err, r)
	}
	o.With = []string{"nope"}
	if _, err := Install(o); err == nil {
		t.Fatal("unknown mod accepted")
	}
}

func TestInstallKeepsAnEditedCopyAndUpdatesAnOldOne(t *testing.T) {
	c := &fakeClaude{}
	o := opts(t, c)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	band := filepath.Join(o.Dir, "comebye-hub", "hooks", "band-model.ts")
	guard := filepath.Join(o.Dir, "comebye-hub", "hooks", "guard-rules.ts")
	// An older comebye's file (recorded, unedited) is updated; the user's edit is kept.
	old := []byte("// an older comebye wrote this\n")
	os.WriteFile(band, old, 0o644)
	rec := readRecord(o.Dir)
	rec.Files["comebye-hub/hooks/band-model.ts"] = sum(old)
	writeRecord(o.Dir, rec)
	os.WriteFile(guard, []byte("// mine\n"), 0o644)

	r, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{}
	for _, f := range r.Files {
		st[f.Rel] = f.State
	}
	if st["comebye-hub/hooks/band-model.ts"] != "updated" || st["comebye-hub/hooks/guard-rules.ts"] != Edited {
		t.Fatalf("states = %v", st)
	}
	if b, _ := os.ReadFile(guard); string(b) != "// mine\n" {
		t.Fatalf("an edited file was overwritten: %q", b)
	}
	if b, _ := os.ReadFile(band); string(b) == string(old) {
		t.Fatal("an outdated file was not updated")
	}
	// Status reports it; --force replaces it and keeps a copy.
	s, _ := Status(Options{Dir: o.Dir, NoClaude: true})
	for _, f := range s.Files {
		if f.Rel == "comebye-hub/hooks/guard-rules.ts" && f.State != Edited {
			t.Fatalf("status = %s", f.State)
		}
	}
	o.Force = true
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(guard + ".bak"); string(b) != "// mine\n" {
		t.Fatalf("backup = %q", b)
	}
}

func TestUninstallTouchesOnlyItsOwn(t *testing.T) {
	c := &fakeClaude{
		markets: []marketJSON{{Name: "someone-elses", Path: "/x"}},
		plugins: []pluginJSON{{ID: "other@someone-elses"}, {ID: "lint@comebye-lint"}},
	}
	o := opts(t, c)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(o.Dir, "my-notes.txt")
	os.WriteFile(extra, []byte("keep"), 0o644)
	edited := filepath.Join(o.Dir, "comebye-hub", "hooks", "argv.ts")
	os.WriteFile(edited, []byte("// mine"), 0o644)

	c.calls = nil
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	for _, call := range c.calls {
		if strings.Contains(call, "uninstall") && !strings.HasSuffix(call, "@"+Marketplace) {
			t.Errorf("uninstalled another plugin: %q", call)
		}
		if strings.Contains(call, "marketplace remove") && !strings.HasSuffix(call, " "+Marketplace) {
			t.Errorf("removed another marketplace: %q", call)
		}
	}
	if c.did("plugin uninstall comebye-hub@comebye-mods") != 1 || c.did("plugin marketplace remove comebye-mods") != 1 {
		t.Fatalf("calls = %v", c.calls)
	}
	for _, p := range []string{extra, edited} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "comebye-hub", "hooks", "register.tsx")); err == nil {
		t.Error("register.tsx stayed")
	}
}

func TestUninstallEmptiesTheFolder(t *testing.T) {
	o := opts(t, &fakeClaude{})
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.Dir); !os.IsNotExist(err) {
		t.Fatalf("the folder stayed: %v", err)
	}
}

func TestRegisteredElsewhereIsNotTakenOver(t *testing.T) {
	c := &fakeClaude{markets: []marketJSON{{Name: Marketplace, Path: "/elsewhere"}}}
	if _, err := Install(opts(t, c)); err == nil {
		t.Fatal("took over a marketplace that points elsewhere")
	}
}

// ---- against the real claude, in a temporary HOME ----

func claudePath(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
	}
}

func TestClaudePluginValidate(t *testing.T) {
	claudePath(t)
	o := Options{Dir: filepath.Join(t.TempDir(), "mods"), NoClaude: true}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for _, p := range []string{"", Hub, CoordWindow} {
		cmd := exec.Command("claude", "plugin", "validate", filepath.Join(o.Dir, p))
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("validate %q: %v\n%s", p, err, out)
		}
	}
}

func TestRealClaudeRoundTripInATempHome(t *testing.T) {
	claudePath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	o := Options{Dir: filepath.Join(home, "mods")}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	r, err := Install(o) // again
	if err != nil {
		t.Fatal(err)
	}
	if r.Marketplace != "registered" || len(r.Installed) != 1 || r.Installed[0] != Hub {
		t.Fatalf("report = %+v", r)
	}
	s, err := Status(o)
	if err != nil || s.Marketplace != "registered" {
		t.Fatalf("status = %+v %v", s, err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	s, _ = Status(Options{Dir: o.Dir})
	if s.Marketplace != "missing" || len(s.Installed) != 0 {
		t.Fatalf("after uninstall: %+v", s)
	}
}

// The mods' own tests (mods/*/hooks/*.test.tsx) run on the engine itself.
func TestClaudePluginTest(t *testing.T) {
	claudePath(t)
	for _, p := range Plugins {
		cmd := exec.Command("claude", "plugin", "test", filepath.Join("mods", p))
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", p, err, out)
		}
	}
}
