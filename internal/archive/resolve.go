package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Where did a commit end up? A session records a commit's SHA when it makes it, but
// landing rewrites commits: a rebase, a reworded message, a squash merge. So each
// commit is asked, in order:
//
//  1. Is it an ancestor of main?
//  2. Is a commit on main the same change? Its patch-id is computed here from git, at
//     index time, with no context lines, so that a rebase over other work still
//     matches. Nothing is stored in the repository.
//  3. With --gh, does GitHub name a PR for it, and is the PR merged? That is how a
//     squash merge is found.
//
// Anything else is "unknown": it may be unmerged, or squashed with no PR, and git
// cannot tell. It is never guessed. "notinmain" is only said when GitHub names a PR
// that is open or closed without merging.

// resolution is what the git pass learned about one commit.
type resolution struct {
	state, how string // landed | notinmain | unknown | unchecked
	landedAs   string // the commit on main that holds the change, when it is another SHA
	pr         string // owner/repo#N of the PR that carried it, when GitHub named one
	patchID    string
}

// patchWindow bounds the patch-id scan of a repository's main: its newest commits, no
// more. One scan of 1,200 commits of a large repository (hypershift) takes about 3 s.
const patchWindow = 3000

// ghCommitLookups bounds the commit-to-PR requests of one pass.
const ghCommitLookups = 300

// resolve answers for one commit of a session.
func (g *gitCtx) resolve(host, cwd, repo string, l *Link, sha string) resolution {
	if !g.enabled {
		return resolution{state: "unchecked", how: "git not consulted"}
	}
	if k, ok := g.known[sha]; ok {
		return resolution{state: k.state, how: k.how}
	}
	dir := g.repoDir(host, cwd, repo, l)
	if dir == "" {
		return resolution{state: "unchecked", how: "no local checkout"}
	}
	ri := g.info(dir)
	if !ri.ok {
		return resolution{state: "unchecked", how: "no main branch in " + baseName(dir)}
	}
	r := resolution{}
	full, _, _ := run(dir, 10*time.Second, "git", "rev-parse", "--verify", "-q", sha+"^{commit}")
	have := full != ""
	if !have {
		full = sha
	}
	if have {
		if _, code, _ := run(dir, 10*time.Second, "git", "merge-base", "--is-ancestor", full, ri.base); code == 0 {
			r.state, r.how = "landed", "git: ancestor of "+ri.base
			return r
		}
		if pid := commitPatchID(dir, full); pid != "" {
			r.patchID = pid
			if c := ri.patchOn(g, pid, full); c != "" {
				r.state, r.how, r.landedAs = "landed", "git: the same change is "+c[:min(8, len(c))]+" on "+ri.base+" (patch-id)", c
				return r
			}
		}
	}
	gh := ""
	if g.gh {
		if pr, state, how, as := g.prFor(dir, ri, full); pr != "" {
			r.pr = pr
			if state != "" {
				r.state, r.how, r.landedAs = state, how, as
				return r
			}
			gh = how
		} else {
			gh = how
		}
	}
	r.state = "unknown"
	switch {
	case !have && !g.gh:
		r.how = "git: the commit is not in " + baseName(dir) + " and no PR lookup (--gh) was asked"
	case !have:
		r.how = "git: the commit is not in " + baseName(dir)
	default:
		r.how = "git: not an ancestor of " + ri.base + " and no commit on it has the same patch-id"
		if !g.gh {
			r.how += "; no PR lookup (--gh) was asked"
		}
	}
	if gh != "" {
		r.how += "; " + gh
	}
	return r
}

func baseName(dir string) string {
	if i := strings.LastIndexByte(dir, '/'); i >= 0 && i < len(dir)-1 {
		return dir[i+1:]
	}
	return dir
}

var multiSpace = regexp.MustCompile(`\s+`)

// commitPatchID is the patch-id of one commit, "" for a merge or an empty change.
func commitPatchID(dir, sha string) string {
	if out, code, _ := run(dir, 10*time.Second, "git", "rev-list", "--parents", "-n", "1", sha); code != 0 || len(multiSpace.Split(out, -1)) > 2 {
		return ""
	}
	out, err := pipeline(dir, 20*time.Second, []string{"git", "show", "-U0", "--no-color", "--format=", sha}, []string{"git", "patch-id", "--stable"})
	if err != nil {
		return ""
	}
	if f := strings.Fields(out); len(f) > 0 {
		return f[0]
	}
	return ""
}

// patchOn finds the commit on main with this patch-id, other than the commit
// itself. The first use scans main's newest commits once, from the oldest commit
// the pass will ask about (see prime).
func (ri *repoInfo) patchOn(g *gitCtx, pid, self string) string {
	if !ri.scanned {
		ri.scanned = true
		ri.pids = map[string]string{}
		args := []string{"git", "log", "-p", "-U0", "--no-color", "--no-merges", "-n", strconv.Itoa(patchWindow)}
		if t := g.since[ri.dir]; t > 0 {
			args = append(args, "--since="+strconv.FormatInt(t-86400, 10))
		}
		args = append(args, ri.base)
		out, err := pipeline(ri.dir, 5*time.Minute, args, []string{"git", "patch-id", "--stable"})
		if err != nil && g.errs != nil {
			*g.errs = append(*g.errs, "patch-id scan failed in "+baseName(ri.dir)+": "+err.Error())
		}
		for _, ln := range strings.Split(out, "\n") {
			if f := strings.Fields(ln); len(f) == 2 {
				if _, dup := ri.pids[f[0]]; !dup { // the newest has it first
					ri.pids[f[0]] = f[1]
				}
			}
		}
	}
	if c := ri.pids[pid]; c != "" && c != self {
		return c
	}
	return ""
}

// pipeline runs a | b in dir and returns b's output.
func pipeline(dir string, timeout time.Duration, a, b []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c1 := exec.CommandContext(ctx, a[0], a[1:]...)
	c2 := exec.CommandContext(ctx, b[0], b[1:]...)
	for _, c := range []*exec.Cmd{c1, c2} {
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	pr, err := c1.StdoutPipe()
	if err != nil {
		return "", err
	}
	c2.Stdin = pr
	var out bytes.Buffer
	c2.Stdout = &out
	if err := c1.Start(); err != nil {
		return "", err
	}
	if err := c2.Start(); err != nil {
		_ = c1.Process.Kill()
		_ = c1.Wait()
		return "", err
	}
	err2 := c2.Wait()
	err1 := c1.Wait()
	if err1 != nil {
		return out.String(), err1
	}
	return out.String(), err2
}

// prime finds, for each repository, the oldest commit this pass will ask about, so
// that the patch-id scan of main reaches back that far and no further.
func (g *gitCtx) prime(commits []map[string]any, byLedger map[string]*Link) {
	if !g.enabled {
		return
	}
	byDir := map[string][]string{}
	seen := map[string]bool{}
	for _, r := range commits {
		sha := sget(r, "sha")
		if _, ok := g.known[sha]; ok || seen[sha] {
			continue
		}
		seen[sha] = true
		var link *Link
		if l := sget(r, "ledger_id"); l != "" {
			link = byLedger[l]
		}
		if dir := g.repoDir(sget(r, "host"), sget(r, "cwd"), sget(r, "repo"), link); dir != "" {
			byDir[dir] = append(byDir[dir], sha)
		}
	}
	g.since = map[string]int64{}
	for dir, shas := range byDir {
		out, err := runStdin(dir, 20*time.Second, strings.Join(shas, "\n")+"\n", "git", "cat-file", "--batch-check")
		if err != nil {
			continue
		}
		var have []string
		for _, ln := range strings.Split(out, "\n") {
			if f := strings.Fields(ln); len(f) == 3 && f[1] == "commit" {
				have = append(have, f[0])
			}
		}
		if len(have) == 0 {
			continue
		}
		out, err = runStdin(dir, 30*time.Second, strings.Join(have, "\n")+"\n", "git", "log", "--no-walk", "--stdin", "--format=%ct")
		if err != nil {
			continue
		}
		for _, ln := range strings.Fields(out) {
			if t, err := strconv.ParseInt(ln, 10, 64); err == nil && (g.since[dir] == 0 || t < g.since[dir]) {
				g.since[dir] = t
			}
		}
	}
}

func runStdin(dir string, timeout time.Duration, in, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	return string(out), err
}

// ----- GitHub's commit-to-PR lookup -----

type ghPull struct {
	Number   int    `json:"number"`
	State    string `json:"state"`
	MergedAt string `json:"merged_at"`
	MergeSHA string `json:"merge_commit_sha"`
	Base     struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

var remoteRE = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// remoteSlugs are the owner/repo of the checkout's origin and upstream, in that order.
func (ri *repoInfo) remoteSlugs() []string {
	if ri.slugsDone {
		return ri.slugs
	}
	ri.slugsDone = true
	for _, name := range []string{"origin", "upstream"} {
		if u, code, _ := run(ri.dir, 10*time.Second, "git", "remote", "get-url", name); code == 0 {
			if m := remoteRE.FindStringSubmatch(u); m != nil {
				ri.slugs = append(ri.slugs, m[1])
			}
		}
	}
	return ri.slugs
}

// prFor asks GitHub which PRs contain the commit. It returns the PR it settled on
// ("owner/repo#N"), and when that settles the commit's state, the state. how is
// what to tell the reader either way.
func (g *gitCtx) prFor(dir string, ri *repoInfo, sha string) (pr, state, how, landedAs string) {
	slugs := ri.remoteSlugs()
	if len(slugs) == 0 {
		return "", "", "no GitHub remote to ask about a PR", ""
	}
	mainBranch := strings.TrimPrefix(ri.base, "origin/")
	asked := false
	var open *ghPull
	var openSlug string
	var elsewhere *ghPull
	var elsewhereSlug string
	missing := 0
	for _, slug := range slugs {
		if g.ghCalls >= ghCommitLookups {
			return "", "", "the limit of PR lookups for one pass was reached", ""
		}
		g.ghCalls++
		out, code, _ := run(dir, 30*time.Second, "gh", "api", "-H", "Accept: application/vnd.github+json", "repos/"+slug+"/commits/"+sha+"/pulls")
		if code != 0 {
			if strings.Contains(out, "No commit found") { // a 422: GitHub never had the commit
				missing++
			}
			continue
		}
		asked = true
		var ps []ghPull
		if json.Unmarshal([]byte(out), &ps) != nil {
			continue
		}
		for i := range ps {
			p := &ps[i]
			// The lookup on a fork also answers with the parent's PRs, so the PR's own
			// repository is the base's, not the remote that was asked.
			prRepo := slug
			if p.Base.Repo.FullName != "" {
				prRepo = p.Base.Repo.FullName
			}
			ref := prRepo + "#" + strconv.Itoa(p.Number)
			switch {
			case p.MergedAt != "" && p.Base.Ref == mainBranch:
				as := p.MergeSHA
				if as == sha {
					as = ""
				}
				return ref, "landed", fmt.Sprintf("github: PR %s merged into %s", ref, mainBranch), as
			case p.MergedAt != "" && elsewhere == nil:
				elsewhere, elsewhereSlug = p, prRepo
			case p.MergedAt == "" && open == nil:
				open, openSlug = p, prRepo
			}
		}
	}
	switch {
	case open != nil:
		verb := "is open"
		if open.State == "closed" {
			verb = "was closed without merging"
		}
		ref := openSlug + "#" + strconv.Itoa(open.Number)
		return ref, "notinmain", fmt.Sprintf("github: PR %s %s", ref, verb), ""
	case elsewhere != nil:
		ref := elsewhereSlug + "#" + strconv.Itoa(elsewhere.Number)
		return ref, "", fmt.Sprintf("github: PR %s merged into %s, not %s", ref, elsewhere.Base.Ref, mainBranch), ""
	case !asked && missing > 0:
		return "", "", "GitHub has no such commit (it was never pushed there)", ""
	case !asked:
		return "", "", "GitHub could not be asked about a PR", ""
	}
	return "", "", "GitHub names no PR for it", ""
}
