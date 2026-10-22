package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Git, for workers: comebye runs a few read-only git commands (rev-parse,
// status, rev-list, cherry, symbolic-ref) in checkouts inside the launch
// roots or the worktree root, and one write: `git worktree remove` without
// --force, which git itself refuses for a dirty worktree. Branches are never
// deleted.

// gitTimeout bounds one git command.
const gitTimeout = 20 * time.Second

// runGit runs git in dir and returns its trimmed stdout. Prompts and
// optional locks are off, so it never waits on a terminal or takes the
// index lock away from an agent working in the checkout.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// branchRe is a conservative subset of git's branch names: ASCII letters,
// digits, '.', '_', '-' and '/', starting with a letter or digit. validBranch
// adds git's other rules (check-ref-format) on top.
var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)

// validBranch reports whether b is a branch name workers may use.
func validBranch(b string) bool {
	if !branchRe.MatchString(b) || b == "HEAD" || strings.Contains(b, "..") || strings.Contains(b, "//") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") {
		return false
	}
	for _, part := range strings.Split(b, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") || strings.HasPrefix(part, "-") {
			return false
		}
	}
	return true
}

// gitRefFormatOK asks git itself (check-ref-format), for the rules the
// regexp does not spell out.
func gitRefFormatOK(ctx context.Context, b string) bool {
	_, err := runGit(ctx, os.TempDir(), "check-ref-format", "refs/heads/"+b)
	return err == nil
}

// branchDirName is a branch's worktree directory name: slashes become '-'
// (the ~/.worktrees/<repo>/<branch> convention).
func branchDirName(b string) string { return strings.ReplaceAll(b, "/", "-") }

// gitDirs returns a checkout's top level, git dir and common dir, all
// absolute with symlinks resolved.
func gitDirs(ctx context.Context, dir string) (top, gitDir, common string, err error) {
	out, err := runGit(ctx, dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		return "", "", "", errors.New("unexpected git rev-parse output")
	}
	for i, l := range lines {
		if p, err := filepath.EvalSymlinks(l); err == nil {
			lines[i] = p
		}
	}
	return lines[0], lines[1], lines[2], nil
}

// primaryCheckout reports whether dir is the top level of a repository's
// primary (not linked) checkout.
func primaryCheckout(ctx context.Context, dir string) bool {
	top, gitDir, common, err := gitDirs(ctx, dir)
	return err == nil && top == dir && gitDir == common
}

// refExists reports whether ref names a commit in the repository at dir.
func refExists(ctx context.Context, dir, ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return false
	}
	_, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	return err == nil
}

// branchExists reports whether the local branch b exists.
func branchExists(ctx context.Context, dir, b string) bool {
	_, err := runGit(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+b)
	return err == nil
}

// defaultBranch is the repository's default branch: origin's HEAD when
// known, else main or master if present, else the primary checkout's
// current branch.
func defaultBranch(ctx context.Context, dir string) string {
	if out, err := runGit(ctx, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(out, "origin/"); ok && branchExists(ctx, dir, b) {
			return b
		}
	}
	for _, b := range []string{"main", "master"} {
		if branchExists(ctx, dir, b) {
			return b
		}
	}
	_, _, common, err := gitDirs(ctx, dir)
	if err == nil {
		if out, err := runGit(ctx, filepath.Dir(common), "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			return out
		}
	}
	return ""
}

// defaultBranchFast reads a primary checkout's default branch from its .git
// directory without running git (for listing many repositories): origin's
// HEAD, else main or master, else HEAD's branch.
func defaultBranchFast(repo string) string {
	gd := filepath.Join(repo, ".git")
	readRef := func(name string) string {
		b, err := os.ReadFile(filepath.Join(gd, name))
		if err != nil {
			return ""
		}
		s, _ := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: ")
		return s
	}
	packed, _ := os.ReadFile(filepath.Join(gd, "packed-refs"))
	has := func(ref string) bool {
		if _, err := os.Stat(filepath.Join(gd, ref)); err == nil {
			return true
		}
		return bytes.Contains(packed, []byte(" "+ref+"\n"))
	}
	if r, ok := strings.CutPrefix(readRef("refs/remotes/origin/HEAD"), "refs/remotes/origin/"); ok && has("refs/heads/"+r) {
		return r
	}
	for _, b := range []string{"main", "master"} {
		if has("refs/heads/" + b) {
			return b
		}
	}
	if r, ok := strings.CutPrefix(readRef("HEAD"), "refs/heads/"); ok {
		return r
	}
	return ""
}

// worktreeChecks are the safety checks before a worker's worktree is
// removed: it must be clean and have nothing its base lacks.
type worktreeChecks struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Primary: the path is a repository's primary checkout, which is never
	// removed.
	Primary bool   `json:"primary"`
	Branch  string `json:"branch,omitempty"`
	Dirty   bool   `json:"dirty"`
	// DirtyFiles counts `git status --porcelain` lines (untracked included).
	DirtyFiles int `json:"dirty_files"`
	// Unmerged counts the branch's commits with no equivalent in the base
	// (git cherry), -1 when unknown.
	Unmerged int    `json:"unmerged_commits"`
	BaseUsed string `json:"base_used,omitempty"`
	Repo     string `json:"repo,omitempty"` // the primary checkout
	Error    string `json:"error,omitempty"`
}

// Removable reports whether the worktree may be removed.
func (c worktreeChecks) Removable() (bool, string) {
	switch {
	case !c.Exists:
		return false, "the worktree does not exist"
	case c.Primary:
		return false, "it is the repository's primary checkout"
	case c.Error != "":
		return false, "the checks failed: " + c.Error
	case c.Dirty:
		return false, fmt.Sprintf("it has uncommitted changes (%d files)", c.DirtyFiles)
	case c.Unmerged != 0:
		if c.Unmerged < 0 {
			return false, "could not tell whether its branch is merged"
		}
		return false, fmt.Sprintf("its branch has %d commits not in %s", c.Unmerged, c.BaseUsed)
	}
	return true, ""
}

// checkWorktree runs the checks for the checkout at path (resolved), on
// branch (default: its current branch) against base (default: the
// repository's default branch; origin's copy of the base counts too, so a
// branch merged upstream but not pulled yet is merged).
func checkWorktree(ctx context.Context, path, branch, base string) worktreeChecks {
	c := worktreeChecks{Path: path, Unmerged: -1}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return c
	}
	top, gitDir, common, err := gitDirs(ctx, path)
	if err != nil || top != path {
		return c
	}
	c.Exists = true
	c.Primary = gitDir == common
	c.Repo = filepath.Dir(common)
	head, _ := runGit(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branch == "" {
		branch = head
	}
	c.Branch = branch
	out, err := runGit(ctx, path, "status", "--porcelain")
	if err != nil {
		c.Error = err.Error()
		return c
	}
	if out != "" {
		c.Dirty = true
		c.DirtyFiles = strings.Count(out, "\n") + 1
	}
	tip := branch
	if tip == "" || !refExists(ctx, path, "refs/heads/"+tip) {
		tip = "HEAD"
	} else {
		tip = "refs/heads/" + tip
	}
	if base == "" || !refExists(ctx, path, base) {
		base = defaultBranch(ctx, path)
	}
	if base == "" || !refExists(ctx, path, base) {
		c.Error = "no base branch to compare with"
		return c
	}
	c.BaseUsed = base
	for _, b := range []string{base, "refs/remotes/origin/" + base} {
		if b != base && !refExists(ctx, path, b) {
			continue
		}
		out, err := runGit(ctx, path, "cherry", "--end-of-options", b, tip)
		if err != nil {
			continue
		}
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "+") {
				n++
			}
		}
		if c.Unmerged < 0 || n < c.Unmerged {
			c.Unmerged = n
		}
	}
	return c
}

// removeWorktree removes a linked worktree with git (no --force: git
// refuses a dirty one too). The branch stays.
func removeWorktree(ctx context.Context, repo, path string) error {
	_, err := runGit(ctx, repo, "worktree", "remove", "--end-of-options", path)
	return err
}

// itoa is strconv.Itoa, for messages.
func itoa(n int) string { return strconv.Itoa(n) }
