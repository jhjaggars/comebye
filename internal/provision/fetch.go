package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FetchTimeout bounds one fetch-and-push: an ssh fetch through the
// provider's transport and a push to origin.
const FetchTimeout = 4 * time.Minute

// FetchReq asks the tool to bring a branch back from a worker that cannot
// push (`worker fetch NAME BRANCH --repo REPO --remote-repo R --push`).
type FetchReq struct {
	Name       string // the worker
	Branch     string
	Repo       string // the local checkout on this node (absolute)
	RemoteRepo string // the repository's name under /data/code on the worker
}

// FetchResult is what `fetch --json` prints.
type FetchResult struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Tip    string `json:"tip"`
	Local  string `json:"local"`
	Pushed string `json:"pushed"`
}

var (
	branchRe   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,199}$`)
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)
	shaRe      = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// ValidBranch is a plain work branch name: no option, refspec or default
// branch. The tool checks it again (git check-ref-format).
func ValidBranch(b string) bool {
	return branchRe.MatchString(b) && !strings.Contains(b, "..") && !strings.HasSuffix(b, "/") &&
		!strings.HasSuffix(b, ".lock") && b != "main" && b != "master" && !strings.HasPrefix(b, "refs/")
}

// ValidRepoName is a repository directory name (no path).
func ValidRepoName(s string) bool { return repoNameRe.MatchString(s) && !strings.Contains(s, "..") }

// Fetch runs the fetch and push. The arguments are validated here and
// passed one argv element each.
func (t *Tool) Fetch(ctx context.Context, q FetchReq) (FetchResult, error) {
	switch {
	case !ValidName(q.Name):
		return FetchResult{}, errors.New("not a worker name")
	case !ValidBranch(q.Branch):
		return FetchResult{}, errors.New("not a work branch name")
	case !filepath.IsAbs(q.Repo) || filepath.Clean(q.Repo) != q.Repo:
		return FetchResult{}, errors.New("the local repository must be an absolute path")
	case !ValidRepoName(q.RemoteRepo):
		return FetchResult{}, errors.New("not a repository name")
	}
	out, err := t.run(ctx, "fetch", FetchTimeout, "fetch", q.Name, q.Branch, "--repo", q.Repo, "--remote-repo", q.RemoteRepo, "--push", "--json")
	if err != nil {
		return FetchResult{}, err
	}
	var r FetchResult
	if json.Unmarshal(out, &r) != nil || !shaRe.MatchString(r.Tip) || r.Name != q.Name || r.Branch != q.Branch {
		return FetchResult{}, fmt.Errorf("the worker tool's fetch answer is not a result: %s", Text(lastLine(string(out)), 120))
	}
	return r, nil
}
