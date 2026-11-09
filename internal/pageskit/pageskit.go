// Package pageskit is what an agent needs to make a good Comebye page: the
// comebye-pages skill (how to write a page, `comebye pages guide`), the
// starters (`comebye pages new`) and installing the skill where claude and
// pi look for skills (`comebye skill install`).
//
// The starters are the Comebye page template (template.html, the theme
// contract: tokens from web/themes.js and the comebye-theme listener) with a
// filled-in example of each type spliced in. There is one template, so there
// is one theme contract; pageskit_test checks it against web/themes.js.
package pageskit

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SkillName is the skill's directory name and its frontmatter name.
const SkillName = "comebye-pages"

//go:embed SKILL.md
var skill string

//go:embed template.html
var template string

//go:embed starters/*.html
var starters embed.FS

// Guide is the skill: SKILL.md with its frontmatter.
func Guide() string { return skill }

// Types are the starter types, the default first.
var Types = []string{"doc", "mockup", "deck", "dashboard"}

// Anchors in template.html that Starter splices at.
const (
	tmplComment  = "<!--\n  Comebye page template."
	tmplStyleEnd = "</style>"
	tmplBody     = `<div class="wrap">`
	tmplListener = "<script>\n/* Theme from the Comebye app."
	bodyMarker   = "\n<!-- body -->\n"
)

// Starter returns the starter page of a type with its title filled in.
func Starter(kind, title string) (string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = Types[0]
	}
	b, err := starters.ReadFile("starters/" + kind + ".html")
	if err != nil {
		return "", fmt.Errorf("no starter %q (want one of %s)", kind, strings.Join(Types, ", "))
	}
	css, body, ok := strings.Cut(string(b), bodyMarker)
	if !ok {
		return "", fmt.Errorf("starter %s: no body marker", kind)
	}
	if strings.TrimSpace(title) == "" {
		title = "Untitled page"
	}
	t := html.EscapeString(strings.TrimSpace(title))

	page := template
	// The template's comment documents its placeholders for its own builder;
	// a starter says what it is instead.
	c0 := strings.Index(page, tmplComment)
	c1 := strings.Index(page, "-->\n<style>")
	s := strings.LastIndex(page, tmplStyleEnd)
	w := strings.Index(page, tmplBody)
	l := strings.Index(page, tmplListener)
	if c0 < 0 || c1 < c0 || s < c1 || w < s || l < w {
		return "", errors.New("the page template changed shape: update pageskit.Starter")
	}
	note := "<!--\n  Comebye page starter: " + kind + " (comebye pages new). The token blocks at the top of\n" +
		"  <style> and the theme script at the end are the Comebye page template:\n" +
		"  keep both, and colour everything with var(--token). Replace the example\n" +
		"  content. The rules: comebye pages guide.\n"
	var out strings.Builder
	out.WriteString(page[:c0])
	out.WriteString(note)
	out.WriteString(page[c1:s])
	out.WriteString("\n/* ---- " + kind + " ---- */\n")
	out.WriteString(strings.TrimRight(css, "\n") + "\n")
	out.WriteString(page[s:w])
	out.WriteString(strings.TrimRight(body, "\n") + "\n")
	out.WriteString(page[l:])
	return strings.ReplaceAll(out.String(), "{{TITLE}}", t), nil
}

// ---- installing the skill ----

// Target is where one harness reads user skills, relative to $HOME.
type Target struct{ Name, Dir string }

// Targets: claude reads ~/.claude/skills, pi ~/.pi/agent/skills.
var Targets = []Target{{"claude", ".claude/skills"}, {"pi", ".pi/agent/skills"}}

// recordName is the file beside SKILL.md that holds the SHA-256 of the
// SKILL.md comebye last wrote there, so an install can tell a file it wrote
// (safe to update) from one the user edited (left alone).
const recordName = ".comebye-installed"

// States a target can be in.
const (
	Missing   = "missing"   // no SKILL.md
	Current   = "current"   // this binary's skill
	Outdated  = "outdated"  // an older skill comebye wrote, unedited
	Edited    = "edited"    // not what comebye wrote: the user's
	Linked    = "symlink"   // SKILL.md or its directory is a symlink: managed elsewhere
	Installed = "installed" // written now, there was none
	Updated   = "updated"   // replaced an outdated one
	Replaced  = "replaced"  // replaced an edited one (--force); the old one is in SKILL.md.bak
)

// Result is one target's state, before (Status) or after (Install).
type Result struct {
	Target string `json:"target"`
	Path   string `json:"path"`
	State  string `json:"state"`
	Backup string `json:"backup,omitempty"`
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func pick(names []string) ([]Target, error) {
	if len(names) == 0 {
		return Targets, nil
	}
	var out []Target
	for _, n := range names {
		found := false
		for _, t := range Targets {
			if t.Name == n {
				out, found = append(out, t), true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown target %q (want claude or pi)", n)
		}
	}
	return out, nil
}

func state(home string, t Target) (Result, error) {
	dir := filepath.Join(home, t.Dir, SkillName)
	path := filepath.Join(dir, "SKILL.md")
	r := Result{Target: t.Name, Path: path}
	for _, p := range []string{dir, path} {
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			r.State = Missing
			return r, nil
		case err != nil:
			return r, err
		case fi.Mode()&fs.ModeSymlink != 0:
			r.State = Linked
			return r, nil
		}
	}
	have, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	rec, _ := os.ReadFile(filepath.Join(dir, recordName))
	switch h := sum(have); {
	case string(have) == skill:
		r.State = Current
	case strings.TrimSpace(string(rec)) == h:
		r.State = Outdated
	default:
		r.State = Edited
	}
	return r, nil
}

// Status says what each target holds. names picks targets (none: all).
func Status(home string, names []string) ([]Result, error) {
	if home == "" {
		return nil, errors.New("HOME is not set")
	}
	ts, err := pick(names)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, t := range ts {
		r, err := state(home, t)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Install writes the skill to each target. A missing or outdated skill is
// written; a current one is left as it is (installing twice changes
// nothing); an edited one is left alone unless force, which keeps it as
// SKILL.md.bak; a symlinked one is always left alone.
func Install(home string, names []string, force bool) ([]Result, error) {
	rs, err := Status(home, names)
	if err != nil {
		return rs, err
	}
	for i, r := range rs {
		switch r.State {
		case Current, Linked:
			continue
		case Edited:
			if !force {
				continue
			}
			bak := r.Path + ".bak"
			old, err := os.ReadFile(r.Path)
			if err != nil {
				return rs, err
			}
			if err := writeFile(bak, old); err != nil {
				return rs, err
			}
			rs[i].Backup, rs[i].State = bak, Replaced
		case Outdated:
			rs[i].State = Updated
		case Missing:
			rs[i].State = Installed
		}
		dir := filepath.Dir(r.Path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return rs, err
		}
		if err := writeFile(r.Path, []byte(skill)); err != nil {
			return rs, err
		}
		if err := writeFile(filepath.Join(dir, recordName), []byte(sum([]byte(skill))+"\n")); err != nil {
			return rs, err
		}
	}
	return rs, nil
}

// writeFile replaces path atomically (a temp file renamed over it), 0644.
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".skill-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
