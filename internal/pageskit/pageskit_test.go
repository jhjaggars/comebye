package pageskit

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestGuideIsASkill(t *testing.T) {
	g := Guide()
	if !strings.HasPrefix(g, "---\nname: "+SkillName+"\ndescription: ") {
		t.Fatalf("not Agent Skills frontmatter:\n%s", g[:min(len(g), 200)])
	}
	end := strings.Index(g[4:], "\n---\n")
	if end < 0 {
		t.Fatal("frontmatter not closed")
	}
	for _, want := range []string{
		"comebye pages new", "comebye-theme", "--key <your-name>-", // starters, the theme contract, keys prefixed with the agent's name
		"390", "cdnjs.cloudflare.com", "5 MiB", "execCommand", // a page's copy button
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	for _, typ := range Types {
		if !strings.Contains(g, "`"+typ+"`") {
			t.Errorf("guide does not describe the %s starter", typ)
		}
	}
}

// tokenBlock parses "--name:value;" pairs out of a CSS block.
func tokenBlock(t *testing.T, css string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, x := range regexp.MustCompile(`--([a-z0-9-]+):\s*([^;]+);`).FindAllStringSubmatch(css, -1) {
		if x[1] == "mono" || x[1] == "sans" {
			continue
		}
		m[x[1]] = strings.TrimSpace(x[2])
	}
	return m
}

// themeVars parses `const NAME = { ... };` out of web/themes.js.
func themeVars(t *testing.T, js, name string) map[string]string {
	t.Helper()
	i := strings.Index(js, "const "+name+" = {")
	if i < 0 {
		t.Fatalf("themes.js: no %s", name)
	}
	j := strings.Index(js[i:], "};")
	m := map[string]string{}
	for _, x := range regexp.MustCompile(`"?([a-z0-9-]+)"?:\s*"([^"]+)"`).FindAllStringSubmatch(js[i:i+j], -1) {
		m[x[1]] = x[2]
	}
	return m
}

// The template is the theme contract: its fallback colours must be the
// app's Comebye Light and Dark, and its listener must accept every token.
func TestTemplateMatchesTheApp(t *testing.T) {
	js, err := os.ReadFile("../../web/themes.js")
	if err != nil {
		t.Fatal(err)
	}
	style := template[strings.Index(template, "<style>"):strings.Index(template, "</style>")]
	dm := strings.Index(style, "@media (prefers-color-scheme: dark)")
	if dm < 0 {
		t.Fatal("template: no dark block")
	}
	light, dark := tokenBlock(t, style[:dm]), tokenBlock(t, style[dm:strings.Index(style[dm:], "\n}\n")+dm])
	for name, got := range map[string]map[string]string{"comebyeLight": light, "comebyeDark": dark} {
		want := themeVars(t, string(js), name)
		if len(want) < 20 {
			t.Fatalf("themes.js %s: parsed only %d tokens", name, len(want))
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s --%s: template %q, themes.js %q", name, k, got[k], v)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("%s: template has --%s, themes.js does not", name, k)
			}
		}
	}
	m := regexp.MustCompile(`var KNOWN = \[([^\]]+)\]`).FindStringSubmatch(template)
	if m == nil {
		t.Fatal("template: no KNOWN list in the listener")
	}
	var known, tokens []string
	for _, q := range regexp.MustCompile(`"([a-z0-9-]+)"`).FindAllStringSubmatch(m[1], -1) {
		known = append(known, q[1])
	}
	for k := range light {
		tokens = append(tokens, k)
	}
	sort.Strings(known)
	sort.Strings(tokens)
	if strings.Join(known, " ") != strings.Join(tokens, " ") {
		t.Errorf("the listener's KNOWN list and the template's tokens differ:\n%v\n%v", known, tokens)
	}
}

var (
	hexColour   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	funcColour  = regexp.MustCompile(`\b(rgba?|hsla?)\(`)
	externalURL = regexp.MustCompile(`https?://[^\s"'<>)]+`)
	forbidden   = regexp.MustCompile(`localStorage|sessionStorage|document\.cookie|\bfetch\(|XMLHttpRequest|\balert\(|\bconfirm\(|\bprompt\(|window\.open|<link\b|@import|<iframe\b|<form\b`)
)

func TestStarters(t *testing.T) {
	listener := template[strings.Index(template, tmplListener):]
	tokens := template[strings.Index(template, ":root {"):strings.Index(template, "* { box-sizing")]
	for _, typ := range Types {
		page, err := Starter(typ, `Drill <1> & "two"`)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if !strings.Contains(page, "<title>Drill &lt;1&gt; &amp; &#34;two&#34;</title>") {
			t.Errorf("%s: title not filled in and escaped", typ)
		}
		if strings.Contains(page, "{{") {
			t.Errorf("%s: a placeholder is left: %s", typ, page[strings.Index(page, "{{"):][:30])
		}
		// One contract: the template's token blocks and listener, verbatim, once.
		if !strings.Contains(page, tokens) || !strings.HasSuffix(page, listener) || strings.Count(page, `addEventListener("message"`) != 1 {
			t.Errorf("%s: the template's token blocks or theme listener are not there verbatim", typ)
		}
		if strings.Count(page, "<title>") != 1 || strings.Count(page, "<html") != 1 || !strings.Contains(page, `name="viewport"`) {
			t.Errorf("%s: not one well-formed document", typ)
		}
		// The type's own CSS and markup: colours only as tokens, nothing the sandbox blocks, no network.
		src, _ := starters.ReadFile("starters/" + typ + ".html")
		if c := hexColour.FindString(string(src)); c != "" {
			t.Errorf("%s: a literal colour %s (use a var(--token))", typ, c)
		}
		if funcColour.MatchString(string(src)) {
			t.Errorf("%s: a literal rgb()/hsl() colour (use a var(--token))", typ)
		}
		if u := externalURL.FindString(string(src)); u != "" && !strings.HasPrefix(u, "http://www.w3.org/") {
			t.Errorf("%s: loads %s (pages have no network)", typ, u)
		}
		if f := forbidden.FindString(string(src)); f != "" {
			t.Errorf("%s: uses %s, which the page sandbox blocks", typ, f)
		}
	}
	if p, err := Starter("", ""); err != nil || !strings.Contains(p, "<title>Untitled page</title>") || !strings.Contains(p, "starter: doc") {
		t.Errorf("default starter: %v", err)
	}
	if _, err := Starter("slides", "x"); err == nil || !strings.Contains(err.Error(), "doc, mockup, deck, dashboard") {
		t.Errorf("unknown type: %v", err)
	}
	if _, err := Starter("../SKILL", "x"); err == nil {
		t.Error("a path as a type was accepted")
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func states(rs []Result) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Target+"="+r.State)
	}
	return strings.Join(s, " ")
}

func TestInstallBothPathsAndIdempotent(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude", "skills", "comebye-pages", "SKILL.md")
	pi := filepath.Join(home, ".pi", "agent", "skills", "comebye-pages", "SKILL.md")

	rs, err := Status(home, nil)
	if err != nil || states(rs) != "claude=missing pi=missing" {
		t.Fatalf("status before: %s %v", states(rs), err)
	}
	rs, err = Install(home, nil, false)
	if err != nil || states(rs) != "claude=installed pi=installed" || rs[0].Path != claude || rs[1].Path != pi {
		t.Fatalf("install: %+v %v", rs, err)
	}
	for _, p := range []string{claude, pi} {
		if read(t, p) != Guide() {
			t.Errorf("%s is not the skill", p)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v", p, fi.Mode())
		}
	}
	fi1, _ := os.Stat(claude)
	// Twice changes nothing.
	rs, err = Install(home, nil, false)
	if err != nil || states(rs) != "claude=current pi=current" {
		t.Fatalf("second install: %s %v", states(rs), err)
	}
	if fi2, _ := os.Stat(claude); !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Error("a current skill was rewritten")
	}
	// Nothing else was written under HOME.
	var files []string
	filepath.Walk(home, func(p string, fi os.FileInfo, _ error) error {
		if !fi.IsDir() {
			r, _ := filepath.Rel(home, p)
			files = append(files, r)
		}
		return nil
	})
	sort.Strings(files)
	want := ".claude/skills/comebye-pages/.comebye-installed .claude/skills/comebye-pages/SKILL.md .pi/agent/skills/comebye-pages/.comebye-installed .pi/agent/skills/comebye-pages/SKILL.md"
	if strings.Join(files, " ") != want {
		t.Errorf("files under HOME: %v", files)
	}
}

func TestInstallOneTarget(t *testing.T) {
	home := t.TempDir()
	rs, err := Install(home, []string{"pi"}, false)
	if err != nil || states(rs) != "pi=installed" {
		t.Fatalf("%s %v", states(rs), err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Error("--pi touched ~/.claude")
	}
	if _, err := Install(home, []string{"codex"}, false); err == nil {
		t.Error("unknown target accepted")
	}
	if _, err := Install("", nil, false); err == nil {
		t.Error("empty HOME accepted")
	}
}

func TestInstallUpdatesOnlyWhatItWrote(t *testing.T) {
	home := t.TempDir()
	if _, err := Install(home, nil, false); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(home, ".claude", "skills", "comebye-pages", "SKILL.md")
	pi := filepath.Join(home, ".pi", "agent", "skills", "comebye-pages", "SKILL.md")

	// An older skill comebye wrote (its record matches): updated.
	old := "---\nname: comebye-pages\ndescription: old\n---\nold text\n"
	os.WriteFile(pi, []byte(old), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(pi), recordName), []byte(sum([]byte(old))+"\n"), 0o644)
	// The user's own edit: never clobbered.
	edited := Guide() + "\nMy own rule: always add a chart.\n"
	os.WriteFile(claude, []byte(edited), 0o644)

	rs, _ := Status(home, nil)
	if states(rs) != "claude=edited pi=outdated" {
		t.Fatalf("status: %s", states(rs))
	}
	rs, err := Install(home, nil, false)
	if err != nil || states(rs) != "claude=edited pi=updated" {
		t.Fatalf("install: %s %v", states(rs), err)
	}
	if read(t, claude) != edited {
		t.Fatal("an edited skill was replaced without --force")
	}
	if read(t, pi) != Guide() {
		t.Fatal("an outdated skill was not updated")
	}
	// A SKILL.md with no record that differs is the user's too.
	os.Remove(filepath.Join(filepath.Dir(pi), recordName))
	os.WriteFile(pi, []byte(old), 0o644)
	if rs, _ := Install(home, []string{"pi"}, false); states(rs) != "pi=edited" || read(t, pi) != old {
		t.Fatalf("no record: %s", states(rs))
	}
	// --force replaces it and keeps the user's copy.
	rs, err = Install(home, []string{"claude"}, true)
	if err != nil || states(rs) != "claude=replaced" || rs[0].Backup != claude+".bak" {
		t.Fatalf("force: %+v %v", rs, err)
	}
	if read(t, claude) != Guide() || read(t, claude+".bak") != edited {
		t.Fatal("force did not replace and back up")
	}
	if rs, _ := Install(home, []string{"claude"}, true); states(rs) != "claude=current" {
		t.Fatalf("force on a current skill: %s", states(rs))
	}
}

func TestInstallLeavesSymlinksAlone(t *testing.T) {
	home, dots := t.TempDir(), t.TempDir()
	// A dotfiles repo owns the skill directory...
	mine := filepath.Join(dots, "comebye-pages")
	os.MkdirAll(mine, 0o755)
	os.WriteFile(filepath.Join(mine, "SKILL.md"), []byte("mine"), 0o644)
	os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755)
	os.Symlink(mine, filepath.Join(home, ".claude", "skills", "comebye-pages"))
	// ...or just the file.
	pdir := filepath.Join(home, ".pi", "agent", "skills", "comebye-pages")
	os.MkdirAll(pdir, 0o755)
	os.Symlink(filepath.Join(mine, "SKILL.md"), filepath.Join(pdir, "SKILL.md"))

	rs, err := Install(home, nil, true)
	if err != nil || states(rs) != "claude=symlink pi=symlink" {
		t.Fatalf("%s %v", states(rs), err)
	}
	if read(t, filepath.Join(mine, "SKILL.md")) != "mine" {
		t.Fatal("wrote through a symlink")
	}
	if _, err := os.Stat(filepath.Join(mine, recordName)); !os.IsNotExist(err) {
		t.Fatal("wrote a record into the linked directory")
	}
}

// The page template shares the app's design tokens: the non-colour scales
// (space, radius, type, hit size, elevation, ring) must be the same numbers as
// web/index.html's :root, and the template's own rules must use them.
func TestTemplateSharesTheAppScales(t *testing.T) {
	app := read(t, "../../web/index.html")
	root := app[strings.Index(app, ":root {"):]
	root = root[:strings.Index(root, "\n    }")]
	scale := regexp.MustCompile(`--((?:sp|r|fs|hit)-[a-z0-9]+|elev-\d|ring-off):\s*([^;]+);`)
	want := map[string]string{}
	for _, m := range scale.FindAllStringSubmatch(root, -1) {
		want[m[1]] = strings.TrimSpace(m[2])
	}
	if len(want) < 35 {
		t.Fatalf("parsed only %d scale tokens from web/index.html", len(want))
	}
	got := map[string]string{}
	for _, m := range scale.FindAllStringSubmatch(template, -1) {
		got[m[1]] = strings.TrimSpace(m[2])
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("--%s: template %q, app %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("template has --%s, the app does not", k)
		}
	}
	if !strings.Contains(template, "--ring: 2px solid var(--focus)") || !strings.Contains(template, ":focus-visible { outline: var(--ring)") {
		t.Error("template: no shared focus ring")
	}
	// The template's own rules write tokens, not numbers that are on the scale or within 1px of a step
	// (a device frame's 28px radius or a 28px hero figure is its own size, so it is fine).
	nearStep := func(kind string, px float64) bool {
		steps := map[string][]float64{"border-radius": {2, 4, 6, 8, 10, 12, 14, 16}, "font-size": {11, 12, 13, 14, 15, 16, 18, 20, 22, 24}}[kind]
		for _, st := range steps {
			if px-st <= 1 && st-px <= 1 {
				return true
			}
		}
		return false
	}
	css := template[strings.Index(template, "* { box-sizing"):strings.Index(template, "</style>")]
	lit := regexp.MustCompile(`(border-radius|font-size):\s*(\d+(?:\.\d+)?)px`)
	for _, m := range lit.FindAllStringSubmatch(css, -1) {
		if px, _ := strconv.ParseFloat(m[2], 64); !nearStep(m[1], px) {
			continue
		}
		t.Errorf("template: %s: %spx is a literal; use the scale token", m[1], m[2])
	}
	// Every starter's own CSS too.
	for _, typ := range Types {
		src, _ := starters.ReadFile("starters/" + typ + ".html")
		own := string(src)[:strings.Index(string(src), "<!-- body -->")]
		for _, m := range lit.FindAllStringSubmatch(own, -1) {
			if px, _ := strconv.ParseFloat(m[2], 64); !nearStep(m[1], px) {
				continue
			}
			t.Errorf("starter %s: %s: %spx is a literal; use the scale token", typ, m[1], m[2])
		}
	}
}
