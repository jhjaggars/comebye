package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A stale remote branch, provably in main. A finished session often leaves one
// item "left to finish": delete the backup branch it pushed before a rebase
// (origin/<its own branch>). When every commit of that branch is already in
// origin's default branch, deleting it loses nothing, and the hub can say so
// from git, not from what an agent said:
//
//   - the branch is the session's own (its name on the ledger entry), never the
//     default branch and never anyone else's;
//   - after a fetch, `git cherry origin/<default> <tip>` shows no "+" line and
//     the branch has no merge commit (or its tree is identical to a commit on
//     origin/<default>);
//     origin/<default>), or it is superseded by its own rebase (supersededOn:
//     each commit git cherry cannot match was rebased as exactly one commit on
//     origin/<default>, which git range-diff pairs with it);
//   - gh finds no open pull request from it (gh failing is "unknown": no proof).
//
// Before deleting, the host keeps the tip under refs/comebye/backup/<branch>
// in the repository, so the branch can be pushed back from there.
//
// The check and the deletion run on the session's host, where the repository and
// its git credentials are. The deletion re-runs the whole check and pushes with
// a lease on the tip it verified, so a branch that moved is never deleted.

// staleBranchBody asks a host about one remote branch (Delete: and removes it
// when the proof holds).
type staleBranchBody struct {
	Path   string `json:"path"` // the session's worktree (may be gone)
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch"` // the remote branch
	Own    string `json:"own"`    // the session's own branch: Branch must be it
	Delete bool   `json:"delete,omitempty"`
}

// Stale branch states.
const (
	staleInMain    = "in_main"     // every commit is in the default branch, no open PR
	staleAbsent    = "absent"      // origin no longer has it
	staleNotInMain = "not_in_main" // it has commits the default branch lacks
	staleOpenPR    = "open_pr"     // a pull request from it is open
	staleUnknown   = "unknown"     // could not tell: no proof
	staleRefused   = "refused"     // not a branch the loop may touch
)

// staleBranchResult is what a host found.
type staleBranchResult struct {
	State string `json:"state"`
	Tip   string `json:"tip,omitempty"`
	Ref   string `json:"ref,omitempty"`   // "origin/main"
	Proof string `json:"proof,omitempty"` // how it is in main
	Ahead int    `json:"ahead,omitempty"` // commits the default branch lacks (not_in_main)
	PR    int    `json:"pr,omitempty"`
	Why   string `json:"why,omitempty"`
	// Superseded lists, for a branch proven by its rebase (supersededOn),
	// each of its commits and the commit on the default branch it was rebased as.
	Superseded []landedCommit `json:"superseded,omitempty"`
	// Deleted: this call removed it from origin.
	Deleted bool `json:"deleted,omitempty"`
	// Backup is the local ref on the host that holds the deleted tip
	// (refs/comebye/backup/<branch>), set when Deleted.
	Backup string `json:"backup,omitempty"`
}

// ghOpenPRs lists the open pull requests whose head branch is branch. A
// variable so tests can stand in for gh.
var ghOpenPRs = func(ctx context.Context, dir, branch string) ([]ghPR, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--state", "open", "--head", branch, "--limit", "10", "--json", "number,url,headRefOid")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal(out.Bytes(), &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

var protectedBranches = map[string]bool{"main": true, "master": true, "develop": true, "trunk": true, "HEAD": true}

var shaOnlyRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkStaleBranch decides, in dir (a checkout of the repository), whether the
// remote branch may be deleted without losing anything. del: delete it when so.
func checkStaleBranch(ctx context.Context, dir, branch, own string, del bool) staleBranchResult {
	refuse := func(why string) staleBranchResult { return staleBranchResult{State: staleRefused, Why: why} }
	unknown := func(why string) staleBranchResult { return staleBranchResult{State: staleUnknown, Why: why} }
	switch {
	case !validBranch(branch):
		return refuse("not a branch name")
	case protectedBranches[branch]:
		return refuse("a default branch is never deleted")
	case own == "" || branch != own:
		if del || own == "" {
			return refuse("not the session's own branch")
		}
		// Somebody else's branch is never deleted, but whether origin
		// still has it is a fact the hub may look at.
		return remotePresence(ctx, dir, branch)
	}
	res := fetchOrigin(ctx, dir)
	switch {
	case res.NoRepo:
		return unknown("the repository has no origin")
	case !res.Fetched:
		return unknown("origin could not be fetched")
	}
	def := defaultBranch(ctx, dir)
	if def == "" || !refExists(ctx, dir, "refs/remotes/origin/"+def) {
		return unknown("origin has no default branch to compare with")
	}
	if branch == def {
		return refuse("a default branch is never deleted")
	}
	base := "refs/remotes/origin/" + def
	out, err := runGit(ctx, dir, "ls-remote", "--heads", "--exit-code", "origin", "refs/heads/"+branch)
	switch {
	case err != nil && strings.Contains(err.Error(), "exit status 2"):
		return staleBranchResult{State: staleAbsent, Why: "origin does not have it"}
	case err != nil:
		return unknown("could not ask origin: " + clip(err.Error(), 120))
	}
	tip, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if !shaOnlyRe.MatchString(tip) {
		return unknown("origin's answer was not a commit")
	}
	r := staleBranchResult{Tip: tip, Ref: "origin/" + def}
	if _, err := runGit(ctx, dir, "cat-file", "-e", tip+"^{commit}"); err != nil {
		return unknown("its tip " + short(tip) + " is not in the local copy of the repository")
	}
	cherry, err := runGit(ctx, dir, "cherry", "--end-of-options", base, tip)
	if err != nil {
		return unknown("could not compare with " + r.Ref)
	}
	for _, l := range strings.Split(cherry, "\n") {
		if strings.HasPrefix(l, "+ ") {
			r.Ahead++
		}
	}
	merges, err := runGit(ctx, dir, "rev-list", "--merges", "--count", base+".."+tip)
	if err != nil {
		return unknown("could not compare with " + r.Ref)
	}
	switch {
	case r.Ahead == 0 && strings.TrimSpace(merges) == "0":
		r.Proof = "every commit has an equal in " + r.Ref + " (git cherry)"
	case treeOnBase(ctx, dir, base, tip):
		r.Proof = "its tree is identical to a commit on " + r.Ref
	case strings.TrimSpace(merges) == "0" && supersededOn(ctx, dir, base, tip, &r):
		// r.Proof and r.Superseded say which commits replaced it.
	default:
		r.State = staleNotInMain
		r.Why = fmt.Sprintf("%d %s not in %s", max(r.Ahead, 1), plural(max(r.Ahead, 1), "commit is", "commits are"), r.Ref)
		return r
	}
	prs, err := ghOpenPRs(ctx, dir, branch)
	switch {
	case err != nil:
		r.State, r.Why, r.Proof = staleUnknown, "could not ask GitHub whether a pull request is open: "+clip(err.Error(), 120), ""
		return r
	case len(prs) > 0:
		r.State, r.PR, r.Why, r.Proof = staleOpenPR, prs[0].Number, fmt.Sprintf("pull request #%d from it is open", prs[0].Number), ""
		return r
	}
	r.State = staleInMain
	if del {
		// A local ref keeps the commits on this host first (git gc never
		// collects them), so the branch can be pushed back from it.
		bk, err := backupRef(ctx, dir, branch, tip)
		if err != nil {
			r.State, r.Why = staleUnknown, "no backup ref could be kept, so the branch was not deleted: "+clip(err.Error(), 160)
			return r
		}
		r.Backup = bk
		// A lease on the tip just checked: a branch that moved since is kept.
		if _, err := runGit(ctx, dir, "push", "--no-verify", "--force-with-lease=refs/heads/"+branch+":"+tip, "origin", ":refs/heads/"+branch); err != nil {
			r.State, r.Why = staleUnknown, "the delete was refused: "+clip(err.Error(), 160)
			return r
		}
		r.Deleted = true
	}
	return r
}

// remotePresence says whether origin still has branch, deleting nothing:
// absent, or refused with the tip it is at.
func remotePresence(ctx context.Context, dir, branch string) staleBranchResult {
	out, err := runGit(ctx, dir, "ls-remote", "--heads", "--exit-code", "origin", "refs/heads/"+branch)
	switch {
	case err != nil && strings.Contains(err.Error(), "exit status 2"):
		return staleBranchResult{State: staleAbsent, Why: "origin does not have it"}
	case err != nil:
		return staleBranchResult{State: staleUnknown, Why: "could not ask origin: " + clip(err.Error(), 120)}
	}
	tip, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if !shaOnlyRe.MatchString(tip) {
		return staleBranchResult{State: staleUnknown, Why: "origin's answer was not a commit"}
	}
	return staleBranchResult{State: staleRefused, Tip: tip, Why: "not the session's own branch"}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// treeOnBase: the tip's tree is the tree of one of the last commits of base.
func treeOnBase(ctx context.Context, dir, base, tip string) bool {
	tree, err := runGit(ctx, dir, "rev-parse", tip+"^{tree}")
	if err != nil || tree == "" {
		return false
	}
	trees, err := runGit(ctx, dir, "log", "--format=%T", "-n", "2000", base)
	if err != nil {
		return false
	}
	for _, t := range strings.Fields(trees) {
		if t == tree {
			return true
		}
	}
	return false
}

// staleBranchLocal runs the check on this host.
func (s *Server) staleBranchLocal(ctx context.Context, b staleBranchBody) (*staleBranchResult, *launchError) {
	roots := s.checkRoots()
	if len(roots) == 0 {
		return nil, errLaunchDisabled
	}
	if !validBranch(b.Branch) || !validBranch(b.Own) {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_branch", message: "invalid branch"}
	}
	dir := ""
	if p, lerr := resolveLaunchDir(b.Path, roots); lerr == nil {
		dir = p
	} else if lerr.code == "not_found" {
		repo, _, why := goneRepo(ctx, b.Repo, filepath.Clean(b.Path), roots)
		if repo == "" {
			return &staleBranchResult{State: staleUnknown, Why: "the repository is gone: " + why}, nil
		}
		dir = repo
	} else {
		return nil, lerr
	}
	r := checkStaleBranch(ctx, dir, b.Branch, b.Own, b.Delete)
	return &r, nil
}

// handleStaleBranch is POST /api/stale-branch: the hub's check (and, with
// delete, removal) of a session's own remote branch on this host.
func (s *Server) handleStaleBranch(w http.ResponseWriter, r *http.Request) {
	var b staleBranchBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, lerr := s.staleBranchLocal(ctx, b)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

// staleBranchOn asks (or deletes on) host.
func (s *Server) staleBranchOn(ctx context.Context, host string, b staleBranchBody) (*staleBranchResult, *launchError) {
	if host == s.fleet.LocalName() {
		return s.staleBranchLocal(ctx, b)
	}
	var res staleBranchResult
	if e := s.callHost(ctx, host, "stale-branch", nil, b, &res); e != nil {
		return nil, e
	}
	return &res, nil
}

var (
	remoteBranchWordRe = regexp.MustCompile(`(?i)\b(remote|origin|upstream|backup|github)\b`)
	branchishRe        = regexp.MustCompile(`[A-Za-z0-9][\w.-]*/[\w./-]*[\w]`)
)

// remoteBranchItem reads a leftover's words for "delete the remote copy of the
// session's own branch": a deletion verb, the word "branch", the remote named
// (origin/<own>, or "remote"/"backup"), the session's own branch name, and no
// other branch. ok is false for anything else, which stays for the session or
// the user. The hub then checks the branch itself; the words only say what to look at.
func remoteBranchItem(text, own string) (branch string, ok bool) {
	t := strings.TrimSpace(text)
	if own == "" || t == "" || !strings.Contains(strings.ToLower(t), "branch") || !deleteVerbRe.MatchString(t) || !remoteBranchWordRe.MatchString(t) {
		return "", false
	}
	if !strings.Contains(t, own) {
		return "", false
	}
	t2 := strings.ReplaceAll(strings.ReplaceAll(t, "`", ""), "origin/"+own, "")
	t2 = strings.ReplaceAll(t2, own, "")
	for b := range protectedBranches {
		t2 = strings.ReplaceAll(t2, "origin/"+b, "")
	}
	// Another branch named beside it (a/b) means the item is about more than this.
	for _, tok := range branchishRe.FindAllString(t2, -1) {
		if strings.HasPrefix(tok, "origin/") || strings.Contains(tok, "/") {
			return "", false
		}
	}
	return own, true
}
