package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// fetch brings a branch back from a worker that has no GitHub login. The
// worker commits locally; this runs on the host that holds the push
// credentials, fetches the branch over the provisioner's own ssh config
// (strict host key, our key only) into the matching local repository, and
// with --push sends it to origin.
//
// It never fetches into or pushes to the default branch, refuses a worker
// this provisioner did not make, and refuses to move a local or remote tip
// that is not an ancestor of the worker's unless --force is given; even
// then the move is leased on the tip it saw (update-ref with the old value,
// push --force-with-lease), so a tip that moved in between is not lost.

// workerCodeRoot is where a worker's repositories live (COMEBYE_LAUNCH_ROOTS
// in the bootstrap).
const workerCodeRoot = "/data/code"

type fetchOpts struct {
	Repo       string // local repository (default: the current directory)
	RemoteRepo string // repository name under /data/code (default: the local repo's name)
	Push       bool
	Force      bool
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// fetchStageRef is where the worker's tip is fetched first, so it can be
// compared before any real branch moves.
func fetchStageRef(name, branch string) string {
	return "refs/comebye-fetch/" + name + "/" + branch
}

func (e *env) fetch(ctx context.Context, name, branch string, o fetchOpts) error {
	if err := checkName(name); err != nil {
		return err
	}
	cfgPath := filepath.Join(e.Cfg.WorkerDir(name), "ssh_config")
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("%s is not a worker this provisioner knows (no %s)", name, cfgPath)
	}
	if err := checkBranch(ctx, e, branch); err != nil {
		return err
	}
	g := gitIn{e: e, dir: o.Repo}
	if g.dir == "" {
		g.dir = "."
	}
	top, err := g.out(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("%s is not a git repository: %w", g.dir, err)
	}
	g.dir = top // the common dir: every worktree's branches live there
	remoteRepo := o.RemoteRepo
	if remoteRepo == "" {
		remoteRepo = repoNameOf(top)
	}
	if !repoNameRe.MatchString(remoteRepo) || strings.Contains(remoteRepo, "..") {
		return fmt.Errorf("repository name %q: want letters, digits, '.', '_' or '-'", remoteRepo)
	}
	for _, d := range defaultBranches(ctx, g) {
		if branch == d {
			return fmt.Errorf("%s is the default branch: fetch and push a work branch, never the default one", branch)
		}
	}

	// 1. The worker's tip, into a staging ref of our own.
	url := name + ":" + workerCodeRoot + "/" + remoteRepo
	ref := "refs/heads/" + branch
	stage := fetchStageRef(name, branch)
	sshCmd := "ssh -F " + shellQuote(cfgPath) + " -o BatchMode=yes"
	// GIT_SSH_COMMAND, not core.sshCommand: an inherited GIT_SSH_COMMAND
	// would beat the config and reach the worker some other way.
	fa := []string{"GIT_SSH_COMMAND=" + sshCmd, "git", "-C", g.dir, "fetch", "--no-tags", "--no-write-fetch-head", "--", url, "+" + ref + ":" + stage}
	if _, _, err := e.Run.Run(ctx, "env", fa, nil); err != nil {
		return fmt.Errorf("fetch %s from %s: %w", branch, url, err)
	}
	defer g.out(context.WithoutCancel(ctx), "update-ref", "-d", stage)
	tip, err := g.out(ctx, "rev-parse", "--verify", stage+"^{commit}")
	if err != nil {
		return err
	}
	e.printf("%s %s is %s\n", name, branch, short(tip))

	// 2. The local branch: create it, fast-forward it, or (with --force)
	// overwrite it leased on what it was.
	local, _ := g.out(ctx, "rev-parse", "--verify", "--quiet", ref)
	switch {
	case local == tip:
		e.printf("local %s is already there\n", branch)
	default:
		if wt := checkedOutIn(ctx, g, ref); wt != "" {
			return fmt.Errorf("%s is checked out in %s: moving it would leave that worktree behind; switch it away first", branch, wt)
		}
		if local != "" && !g.ancestor(ctx, local, tip) {
			if !o.Force {
				return fmt.Errorf("local %s (%s) is not an ancestor of the worker's (%s): it would be overwritten; --force replaces it", branch, short(local), short(tip))
			}
			e.printf("local %s %s is not an ancestor: replacing it (--force)\n", branch, short(local))
		}
		old := local
		if old == "" {
			old = strings.Repeat("0", len(tip)) // must not exist yet
		}
		if _, err := g.out(ctx, "update-ref", "-m", "comebye-worker fetch "+name, ref, tip, old); err != nil {
			return fmt.Errorf("update %s: %w", ref, err)
		}
		if local == "" {
			e.printf("created local %s at %s\n", branch, short(tip))
		} else {
			e.printf("moved local %s %s -> %s\n", branch, short(local), short(tip))
		}
	}
	if !o.Push {
		e.printf("not pushed (--push sends it to origin)\n")
		return nil
	}

	// 3. origin: a fast-forward, or a leased overwrite with --force.
	ls, err := g.out(ctx, "ls-remote", "--heads", "origin", ref)
	if err != nil {
		return fmt.Errorf("read origin's %s: %w", branch, err)
	}
	remote := ""
	if f := strings.Fields(ls); len(f) >= 2 && f[1] == ref {
		remote = f[0]
	}
	if remote == tip {
		e.printf("origin %s is already there\n", branch)
		return nil
	}
	args := []string{"push", "--no-verify"}
	if remote != "" && !o.Force {
		if !g.has(ctx, remote) {
			if _, err := g.out(ctx, "fetch", "--no-tags", "--no-write-fetch-head", "origin", ref); err != nil {
				return fmt.Errorf("fetch origin's %s to compare: %w", branch, err)
			}
		}
		if !g.ancestor(ctx, remote, tip) {
			return fmt.Errorf("origin %s (%s) is not an ancestor of the worker's (%s): pushing would overwrite it; --force replaces it", branch, short(remote), short(tip))
		}
	}
	// The lease is what ls-remote saw (empty: the branch must not exist), so
	// a push that raced ours is never overwritten.
	args = append(args, "--force-with-lease="+ref+":"+remote, "origin", tip+":"+ref)
	if _, err := g.out(ctx, args...); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	e.printf("pushed %s to origin (%s)\n", branch, short(tip))
	return nil
}

// checkBranch refuses a branch name git would not accept, or one that
// could be read as an option or a refspec.
func checkBranch(ctx context.Context, e *env, b string) error {
	if b == "" || strings.HasPrefix(b, "-") || strings.ContainsAny(b, ":+^~?*[\\ \t\n") || strings.HasPrefix(b, "refs/") {
		return fmt.Errorf("branch %q: want a plain branch name", b)
	}
	if _, _, err := e.Run.Run(ctx, "git", []string{"check-ref-format", "--branch", b}, nil); err != nil {
		return fmt.Errorf("branch %q is not a valid git branch name", b)
	}
	return nil
}

// defaultBranches is origin's HEAD, and main and master whatever it says.
func defaultBranches(ctx context.Context, g gitIn) []string {
	d := []string{"main", "master"}
	if h, err := g.out(ctx, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		d = append(d, strings.TrimPrefix(h, "refs/remotes/origin/"))
	}
	if ls, err := g.out(ctx, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		for _, l := range strings.Split(ls, "\n") {
			if f := strings.Fields(l); len(f) >= 3 && f[0] == "ref:" && f[2] == "HEAD" {
				d = append(d, strings.TrimPrefix(f[1], "refs/heads/"))
			}
		}
	}
	return d
}

// checkedOutIn is the worktree that has ref checked out, if any.
func checkedOutIn(ctx context.Context, g gitIn, ref string) string {
	out, err := g.out(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	wt := ""
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			wt = p
		}
		if l == "branch "+ref {
			return wt
		}
	}
	return ""
}

// repoNameOf is the repository's name from its common git dir:
// /x/comebye/.git and /x/comebye.git are both "comebye".
func repoNameOf(commonDir string) string {
	d := filepath.Clean(commonDir)
	if filepath.Base(d) == ".git" {
		d = filepath.Dir(d)
	}
	return strings.TrimSuffix(filepath.Base(d), ".git")
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// gitIn runs git in one repository.
type gitIn struct {
	e   *env
	dir string
}

func (g gitIn) out(ctx context.Context, args ...string) (string, error) {
	out, _, err := g.e.Run.Run(ctx, "git", append([]string{"-C", g.dir}, args...), nil)
	return strings.TrimSpace(out), err
}

func (g gitIn) ancestor(ctx context.Context, a, b string) bool {
	_, err := g.out(ctx, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

func (g gitIn) has(ctx context.Context, sha string) bool {
	_, err := g.out(ctx, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}
