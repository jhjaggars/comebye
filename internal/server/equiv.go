package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A squash-merged (or rebased-and-merged) branch has commits `git cherry`
// cannot match to the base: the squash is one commit with a different
// patch. Two checks recognise it anyway, so the sweep does not send a
// merged branch to "needs a look":
//
//   - content: every file the branch changed has, in the base, exactly
//     the branch tip's content (works offline, no PR needed);
//   - PR: a merged pull request whose head commit is the branch tip
//     (`gh pr list --state merged --head B`; skipped without gh).
//
// Either way nothing of the branch is missing from the base.

// maxEquivFiles bounds the files one content check passes to git.
const maxEquivFiles = 1500

// contentInBase reports how many files the branch (tip) changed since its
// merge base with baseRef, and whether every one has the tip's content in
// baseRef (a deletion counts when baseRef lacks the file too).
func contentInBase(ctx context.Context, dir, tip, baseRef string) (files int, ok bool) {
	mb, err := runGit(ctx, dir, "merge-base", "--end-of-options", baseRef, tip)
	if err != nil || mb == "" {
		return 0, false
	}
	out, err := runGit(ctx, dir, "diff", "--name-only", "--no-renames", "-z", mb, tip, "--")
	if err != nil {
		return 0, false
	}
	var names []string
	for _, n := range strings.Split(out, "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 || len(names) > maxEquivFiles {
		return len(names), false
	}
	args := append([]string{"diff", "--quiet", "--no-renames", baseRef, tip, "--"}, names...)
	if _, err := runGit(ctx, dir, args...); err != nil {
		return len(names), false
	}
	return len(names), true
}

// ghPR is a pull request as `gh pr list` reports it.
type ghPR struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	HeadRefOid string `json:"headRefOid"`
}

// ghMergedPRs lists the merged pull requests whose head branch is branch.
// A variable so tests can stand in for gh.
var ghMergedPRs = func(ctx context.Context, dir, branch string) ([]ghPR, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--state", "merged", "--head", branch, "--limit", "10", "--json", "number,url,headRefOid")
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

// mergedPRAtTip finds a merged PR whose head is exactly tip.
func mergedPRAtTip(ctx context.Context, dir, branch, tip string) *ghPR {
	if branch == "" || tip == "" {
		return nil
	}
	prs, err := ghMergedPRs(ctx, dir, branch)
	if err != nil {
		return nil
	}
	for i := range prs {
		if strings.EqualFold(prs[i].HeadRefOid, tip) {
			return &prs[i]
		}
	}
	return nil
}

// equivalence says why a branch with commits unknown to the base has
// nothing the base lacks ("" when it cannot be shown).
func equivalence(ctx context.Context, dir, branch, tip, baseRef, baseName string) string {
	if tip == "" || baseRef == "" {
		return ""
	}
	if n, ok := contentInBase(ctx, dir, tip, baseRef); ok {
		return fmt.Sprintf("its changes to %d %s are identical in %s (squash-merged or rebased)", n, plural(n, "file", "files"), baseName)
	}
	if pr := mergedPRAtTip(ctx, dir, branch, tip); pr != nil {
		return fmt.Sprintf("merged as PR #%d (its head is this branch's tip)", pr.Number)
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
