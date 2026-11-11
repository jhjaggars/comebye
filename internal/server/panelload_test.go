package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func loadFile(t *testing.T, s *Server, dir, section, file string) (int, panelFile, map[string]any) {
	t.Helper()
	status, out := s.panelRun(context.Background(), "diff", url.Values{"dir": {dir}, "section": {section}, "file": {file}})
	var f panelFile
	if status == 200 {
		b, _ := json.Marshal(out["file"])
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
	}
	return status, f, out
}

// A change set over the total cap: the files that did not fit say so (Skip
// "total", with their size), hand-written files win the budget over generated
// ones, and each skipped file can be fetched on its own.
func TestPanelTotalCapSkipsAndLoads(t *testing.T) {
	_, wt := panelHome(t)
	const akia = "AKIAABCDEFGHIJKLMNOP"
	body := strings.Repeat("0123456789abcdef0123456789abcdef\n", 1000) // ~33 KB
	// Generated files sort first; they must not eat the budget.
	for i := 0; i < 40; i++ {
		put(t, filepath.Join(wt, "vendor", "lib", fmt.Sprintf("g%02d.go", i)), body)
	}
	for i := 0; i < 12; i++ {
		put(t, filepath.Join(wt, "src", fmt.Sprintf("h%02d.go", i)), body)
	}
	put(t, filepath.Join(wt, "src", "zlast.go"), "package src\n\nconst key = \""+akia+"\"\nvar ok = 1\n")
	gitIn(t, wt, "add", "vendor", "src")
	s := panelServer(t)
	d := runDiff(t, s, wt, "")

	if f := sectionFile(d, "staged", "src/zlast.go"); f == nil || f.TooBig || !strings.Contains(f.Diff, "var ok = 1") {
		t.Fatalf("a small hand-written file shows although it is listed last: %+v", f)
	}
	skippedGen, skippedHand := 0, 0
	for _, f := range d.Sections[1].Files {
		if f.Skip == "total" {
			if !f.TooBig || f.Diff != "" || f.Bytes < 30000 || f.Lines < 1000 {
				t.Errorf("a skipped file keeps its size and line count: %+v", f)
			}
			if f.Generated {
				skippedGen++
			} else {
				skippedHand++
			}
		}
	}
	if skippedGen == 0 || skippedHand != 0 {
		t.Errorf("the generated files give way: generated skipped %d, hand-written skipped %d", skippedGen, skippedHand)
	}
	if !strings.Contains(d.Note, "not loaded") || !strings.Contains(d.Note, "768 KB") {
		t.Errorf("the note says the change set is large: %q", d.Note)
	}
	// Fetch a skipped file on its own.
	var path string
	for _, f := range d.Sections[1].Files {
		if f.Skip == "total" {
			path = f.Path
			break
		}
	}
	if st, f, _ := loadFile(t, s, wt, "staged", path); st != 200 || f.TooBig || !strings.HasPrefix(f.Diff, "@@") || f.Add != 1000 {
		t.Errorf("load %s: %d %+v", path, st, f.Skip)
	}
}

// The fetch redacts, withholds secret files, and takes a bigger cap than the
// panel's own, for tracked, committed and untracked files.
func TestPanelLoadFile(t *testing.T) {
	_, wt := panelHome(t)
	const akia = "AKIAABCDEFGHIJKLMNOP"
	big := strings.Repeat("filler line that is fairly long here\n", 3000) // ~110 KB: over the per-file cap
	put(t, filepath.Join(wt, "big.go"), "package big\n\nconst key = \""+akia+"\"\n"+big)
	put(t, filepath.Join(wt, "a.go"), "package a\n\nconst key = \""+akia+"\"\n"+big)
	put(t, filepath.Join(wt, ".env"), "TOKEN=abcdefghijklmnop\n")
	gitIn(t, wt, "add", "-f", ".env")
	s := panelServer(t)
	d := runDiff(t, s, wt, "")
	for _, c := range []struct{ sec, path string }{{"untracked", "big.go"}, {"unstaged", "a.go"}} {
		f := sectionFile(d, c.sec, c.path)
		if f == nil || !f.TooBig || f.Skip != "size" || f.Diff != "" {
			t.Fatalf("%s is over the per-file cap: %+v", c.path, f)
		}
		st, g, _ := loadFile(t, s, wt, c.sec, c.path)
		if st != 200 || g.TooBig || !strings.Contains(g.Diff, "filler line") || strings.Contains(g.Diff, akia) || g.Redacted == 0 {
			t.Errorf("%s loads redacted: %d tooBig=%v redacted=%d", c.path, st, g.TooBig, g.Redacted)
		}
	}
	if st, g, _ := loadFile(t, s, wt, "staged", ".env"); st != 200 || g.Withheld == "" || g.Diff != "" {
		t.Errorf("a secret file stays withheld: %d %+v", st, g)
	}
	if st, g, _ := loadFile(t, s, wt, "commits", "c.go"); st != 200 || !strings.Contains(g.Diff, "+package c") {
		t.Errorf("a committed file loads: %d %+v", st, g)
	}
	// Over the one-off cap too: still a placeholder, now saying so.
	put(t, filepath.Join(wt, "huge.txt"), strings.Repeat("0123456789abcdef0123456789abcdef\n", 40000))
	if st, g, _ := loadFile(t, s, wt, "untracked", "huge.txt"); st != 200 || !g.TooBig || g.Skip != "load" || g.Diff != "" {
		t.Errorf("over the one-off cap: %d %+v", st, g)
	}
	for _, c := range []struct {
		sec, path string
		want      int
	}{
		{"unstaged", "../etc/passwd", 400}, {"unstaged", "/etc/passwd", 400}, {"unstaged", "", 0}, {"other", "a.go", 400},
		{"unstaged", "b.go", 404}, {"untracked", "a.go", 404}, {"untracked", "nothere.txt", 404},
	} {
		if c.path == "" {
			continue
		}
		if st, _, _ := loadFile(t, s, wt, c.sec, c.path); st != c.want {
			t.Errorf("%s %q: status %d, want %d", c.sec, c.path, st, c.want)
		}
	}
}

func TestGeneratedPath(t *testing.T) {
	for p, want := range map[string]bool{
		"api/v1/zz_generated.deepcopy.go":                           true,
		"api/v1/zz_generated.featuregated-crd-manifests/x/Foo.yaml": true,
		"vendor/github.com/x/y.go":                                  true,
		"a/vendor/b.go":                                             true,
		"pkg/x.pb.go":                                               true,
		"docs/content/reference/aggregated-docs.md":                 true,
		"go.sum":                            true,
		"cmd/main.go":                       false,
		"api/hypershift/v1beta1/powervs.go": false,
		"docs/vendoring.md":                 false,
	} {
		if got := generatedPath(p); got != want {
			t.Errorf("generatedPath(%q) = %v", p, got)
		}
	}
}
