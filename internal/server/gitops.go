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

// Git, for workers: comebye runs read-only git commands (rev-parse,
// status, rev-list, cherry, symbolic-ref, log, diff) in checkouts inside
// the launch roots or the worktree root, and writes only on a confirmed
// removal: `git worktree remove` without --force, which git itself refuses
// for a dirty worktree. Only a removal the user confirmed as a discard
// aborts an in-progress rebase or merge, removes the worktree with --force
// and deletes its branch.

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
	// Tip is the commit the checks looked at (the branch, else HEAD); a
	// discard is refused if it moved since the user reviewed it.
	Tip string `json:"tip,omitempty"`
	// InProgress names an unfinished git operation in the worktree:
	// rebase, merge, cherry-pick, revert or bisect.
	InProgress string `json:"in_progress,omitempty"`
	Error      string `json:"error,omitempty"`
	// Detail is filled in for an inspection (checks?detail=1).
	Detail *worktreeDetail `json:"detail,omitempty"`
}

// worktreeDetail is what a coordinator inspects before proposing to keep
// or discard a worker's work.
type worktreeDetail struct {
	// Status is `git status --porcelain`, at most maxDetailLines lines.
	Status []string `json:"status"`
	// Commits are the branch's commits with no equivalent in the base
	// (git cherry -v), oldest first, at most maxDetailLines.
	Commits []commitInfo `json:"commits"`
	// DiffStat is `git diff --stat` of the branch against its merge base
	// with the base branch.
	DiffStat string `json:"diff_stat,omitempty"`
	// Upstream is the branch's upstream ("" when none); Unpushed counts
	// the branch's commits that are neither in the base nor on any
	// remote-tracking branch: work that exists only here (-1 when unknown).
	Upstream string `json:"upstream,omitempty"`
	Unpushed int    `json:"unpushed"`
	// Diff (checks?diff=1): the branch's committed changes against the
	// merge base, then the uncommitted changes (git diff HEAD), each
	// capped at maxDiffBytes.
	Diff          string `json:"diff,omitempty"`
	WorkDiff      string `json:"work_diff,omitempty"`
	DiffTruncated bool   `json:"diff_truncated,omitempty"`
}

type commitInfo struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

const (
	maxDetailLines = 100
	maxDiffBytes   = 128 << 10
)

// Removable reports whether the worktree may be removed.
func (c worktreeChecks) Removable() (bool, string) {
	switch {
	case !c.Exists:
		return false, "the worktree does not exist"
	case c.Primary:
		return false, "it is the repository's primary checkout"
	case c.Error != "":
		return false, "the checks failed: " + c.Error
	case c.InProgress != "":
		return false, "a " + c.InProgress + " is in progress in it"
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
	c.InProgress = inProgress(gitDir)
	head, _ := runGit(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if head == "" && c.InProgress == "rebase" {
		head = rebasingBranch(gitDir)
	}
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
	c.Tip, _ = runGit(ctx, path, "rev-parse", "--verify", "--quiet", "--end-of-options", tip+"^{commit}")
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

// inProgress names the unfinished operation in a git dir, "" when none.
func inProgress(gitDir string) string {
	for _, op := range []struct{ file, name string }{
		{"rebase-merge", "rebase"}, {"rebase-apply", "rebase"}, {"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"}, {"REVERT_HEAD", "revert"}, {"BISECT_LOG", "bisect"},
	} {
		if _, err := os.Stat(filepath.Join(gitDir, op.file)); err == nil {
			return op.name
		}
	}
	return ""
}

// rebasingBranch is the branch a rebase in progress is rewriting (HEAD
// is detached meanwhile), "" when unknown.
func rebasingBranch(gitDir string) string {
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if b, err := os.ReadFile(filepath.Join(gitDir, d, "head-name")); err == nil {
			if name, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "refs/heads/"); ok && validBranch(name) {
				return name
			}
		}
	}
	return ""
}

// inspectWorktree adds the detail of an inspection to checks that found
// a worktree: its uncommitted files, unmerged commits, diff stat and
// push state, and with diff the diffs themselves.
func inspectWorktree(ctx context.Context, c *worktreeChecks, diff bool) {
	if !c.Exists || c.Tip == "" {
		return
	}
	d := &worktreeDetail{Status: []string{}, Commits: []commitInfo{}, Unpushed: -1}
	c.Detail = d
	if out, err := runGit(ctx, c.Path, "status", "--porcelain"); err == nil && out != "" {
		d.Status = firstLines(out, maxDetailLines)
	}
	tip := c.Tip
	if c.BaseUsed != "" {
		if out, err := runGit(ctx, c.Path, "cherry", "-v", "--end-of-options", c.BaseUsed, tip); err == nil {
			for _, l := range strings.Split(out, "\n") {
				rest, ok := strings.CutPrefix(l, "+ ")
				if !ok {
					continue
				}
				sha, subject, _ := strings.Cut(rest, " ")
				if len(d.Commits) < maxDetailLines {
					d.Commits = append(d.Commits, commitInfo{SHA: shortSHA(sha), Subject: subject})
				}
			}
		}
		if mb, err := runGit(ctx, c.Path, "merge-base", "--end-of-options", c.BaseUsed, tip); err == nil {
			if out, err := runGit(ctx, c.Path, "diff", "--stat", "--end-of-options", mb, tip); err == nil {
				d.DiffStat, _ = capText(out, 16<<10)
			}
			if diff {
				var cut bool
				if out, err := runGit(ctx, c.Path, "diff", "--end-of-options", mb, tip); err == nil {
					d.Diff, cut = capText(out, maxDiffBytes)
					d.DiffTruncated = d.DiffTruncated || cut
				}
			}
		}
	}
	if diff {
		var cut bool
		if out, err := runGit(ctx, c.Path, "diff", "HEAD"); err == nil {
			d.WorkDiff, cut = capText(out, maxDiffBytes)
			d.DiffTruncated = d.DiffTruncated || cut
		}
	}
	if c.Branch != "" {
		d.Upstream, _ = runGit(ctx, c.Path, "for-each-ref", "--format=%(upstream:short)", "refs/heads/"+c.Branch)
	}
	// The branch's own commits (not in the base) that no remote has.
	notIn := []string{"rev-list", "--count", tip, "--not", "--remotes"}
	if c.BaseUsed != "" {
		notIn = append(notIn, c.BaseUsed)
	}
	if out, err := runGit(ctx, c.Path, notIn...); err == nil {
		if n, err := strconv.Atoi(out); err == nil {
			d.Unpushed = n
		}
	}
}

func firstLines(s string, n int) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… and %d more", len(lines)-n))
	}
	return lines
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// capText cuts s to at most n bytes at a line boundary, reporting whether
// it did.
func capText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i+1]
	}
	return s, true
}

// removeWorktree removes a linked worktree with git (no --force: git
// refuses a dirty one too). The branch stays.
func removeWorktree(ctx context.Context, repo, path string) error {
	_, err := runGit(ctx, repo, "worktree", "remove", "--end-of-options", path)
	return err
}

// discardWorktree throws a worker's work away, for a removal the user
// confirmed as a discard: it aborts an unfinished rebase, merge,
// cherry-pick or revert, removes the worktree with its uncommitted changes
// (--force) and deletes its branch (-D, merged or not). The caller has
// checked that the worktree is not a primary checkout and is not in use,
// and that branch is not the repository's default branch.
func discardWorktree(ctx context.Context, c worktreeChecks, branch string) (branchDeleted bool, err error) {
	switch c.InProgress {
	case "rebase", "merge", "cherry-pick", "revert":
		if _, err := runGit(ctx, c.Path, c.InProgress, "--abort"); err != nil {
			return false, err
		}
	case "bisect":
		if _, err := runGit(ctx, c.Path, "bisect", "reset"); err != nil {
			return false, err
		}
	}
	if _, err := runGit(ctx, c.Repo, "worktree", "remove", "--force", "--end-of-options", c.Path); err != nil {
		return false, err
	}
	if branch == "" || !branchExists(ctx, c.Repo, branch) {
		return false, nil
	}
	if _, err := runGit(ctx, c.Repo, "branch", "-D", branch); err != nil {
		return false, fmt.Errorf("the worktree was removed, but %w", err)
	}
	return true, nil
}

// itoa is strconv.Itoa, for messages.
func itoa(n int) string { return strconv.Itoa(n) }
