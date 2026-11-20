// Package buildout finds and deletes the build output inside a git worktree:
// the ignored directories a compiler or package manager regenerates (Cargo's
// target/, node_modules/ and the like). Parking or removing a worktree clears
// them so an idle checkout stops costing tens of gigabytes.
//
// The rule is deliberately conservative. A directory counts only when
//
//   - it is a real directory (a symlink is never followed or deleted through),
//   - git ignores it and tracks nothing inside it, and
//   - it carries a valid CACHEDIR.TAG, or is on the short known list:
//     target/ beside a Cargo.toml, node_modules/, .venv/, dist/ and build/,
//     and bin/ beside a go.mod.
//
// Code, commits and untracked source are never touched.
package buildout

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// cacheTagSig is the signature line of a CACHEDIR.TAG (bford.info/cachedir).
const cacheTagSig = "Signature: 8a477f597d28d172789f06886806bc55"

// maxDepth bounds how deep the search for build directories goes.
const maxDepth = 6

// Dir is one build-output directory.
type Dir struct {
	Path  string `json:"path"` // absolute
	Rel   string `json:"rel"`  // relative to the worktree
	Bytes int64  `json:"bytes"`
}

func hasCacheTag(dir string) bool {
	b := make([]byte, len(cacheTagSig))
	f, err := os.Open(filepath.Join(dir, "CACHEDIR.TAG"))
	if err != nil {
		return false
	}
	defer f.Close()
	n, _ := f.Read(b)
	return string(b[:n]) == cacheTagSig
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// known reports whether name (a directory at dir) is on the known list.
func known(dir, name string) bool {
	switch name {
	case "node_modules", ".venv", "dist", "build":
		return true
	case "target":
		return exists(filepath.Join(filepath.Dir(dir), "Cargo.toml"))
	case "bin":
		return exists(filepath.Join(filepath.Dir(dir), "go.mod"))
	}
	return false
}

func git(ctx context.Context, wt string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", wt}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode(), nil
		}
		return "", -1, err
	}
	return string(out), 0, nil
}

// ignoredAndUntracked reports whether git ignores rel and tracks nothing in it.
func ignoredAndUntracked(ctx context.Context, wt, rel string) bool {
	// Without --no-index, check-ignore does not report a tracked path as ignored.
	if _, code, err := git(ctx, wt, "check-ignore", "-q", "--", rel+"/"); err != nil || code != 0 {
		return false
	}
	out, code, err := git(ctx, wt, "ls-files", "--", rel)
	return err == nil && code == 0 && strings.TrimSpace(out) == ""
}

// Size is the byte size of a tree, without following symlinks.
func Size(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// Find lists the build-output directories of the worktree at wt, with sizes.
func Find(ctx context.Context, wt string) ([]Dir, error) {
	if fi, err := os.Stat(filepath.Join(wt, ".git")); err != nil || fi == nil {
		return nil, fmt.Errorf("%s is not a git worktree", wt)
	}
	var out []Dir
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if ctx.Err() != nil {
				return
			}
			name := e.Name()
			if name == ".git" || e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
				continue // a symlink is never followed
			}
			p := filepath.Join(dir, name)
			if hasCacheTag(p) || known(p, name) {
				rel, err := filepath.Rel(wt, p)
				if err == nil && ignoredAndUntracked(ctx, wt, filepath.ToSlash(rel)) {
					out = append(out, Dir{Path: p, Rel: filepath.ToSlash(rel), Bytes: Size(p)})
					continue // everything inside goes with it
				}
			}
			if depth < maxDepth {
				walk(p, depth+1)
			}
		}
	}
	walk(wt, 0)
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, ctx.Err()
}

// Total is the summed size of dirs.
func Total(dirs []Dir) int64 {
	var n int64
	for _, d := range dirs {
		n += d.Bytes
	}
	return n
}

// Clear deletes the build output of the worktree at wt and returns what it
// freed. RemoveAll deletes a symlink itself, never what it points at.
func Clear(ctx context.Context, wt string) (freed int64, cleared []Dir, err error) {
	dirs, err := Find(ctx, wt)
	if err != nil {
		return 0, nil, err
	}
	var firstErr error
	for _, d := range dirs {
		if e := os.RemoveAll(d.Path); e != nil {
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		freed += d.Bytes
		cleared = append(cleared, d)
	}
	return freed, cleared, firstErr
}

// Human formats a byte count: "22 GB", "310 MB".
func Human(n int64) string {
	const k = 1000.0
	f := float64(n)
	switch {
	case f >= k*k*k:
		return fmt.Sprintf("%.3g GB", f/(k*k*k))
	case f >= k*k:
		return fmt.Sprintf("%.3g MB", f/(k*k))
	case f >= k:
		return fmt.Sprintf("%.3g kB", f/k)
	}
	return fmt.Sprintf("%d B", n)
}
