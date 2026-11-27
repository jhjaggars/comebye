package server

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A superseded pre-rebase branch. A worker that rebases before it lands often
// pushes its branch first as a backup, then lands the rebased commits. The
// backup on origin keeps the pre-rebase commits. When the rebase had to resolve
// a conflict, their patches changed, so `git cherry` cannot match them to main
// and the tree check cannot either (main has moved on). The branch is still
// nothing but the work that landed, and git can show it:
//
//   - the branch has no merge commit since its merge base with origin/<default>,
//     and few commits (maxSuperseded) git cherry cannot match;
//   - each of those commits has exactly one commit among the recent ones of
//     origin/<default> (maxSupersededWindow) with the same author (email), the same author date (with
//     its zone) and the same subject. A rebase keeps all three and nothing else
//     does by chance; two branch commits never share one landed commit;
//   - each pre-rebase commit is no newer than its landed commit (committer
//     date): nothing on the branch came after the land;
//   - `git range-diff` pairs the two (= or !): the patch is the same change,
//     perhaps adjusted to the newer main, and not something else under the same
//     subject.
//
// What this does not prove is that the adjustments lost nothing. That is why a
// deletion first keeps the tip under refs/comebye/backup/<branch> on the
// session's host (backupRef): the remote copy goes, the commits do not.

// maxSuperseded bounds the pre-rebase commits paired one by one.
const maxSuperseded = 20

// maxSupersededWindow bounds how many base commits are searched for the landed ones.
const maxSupersededWindow = 2000

// rangeDiffPairRe is range-diff's line for a pair of single commits: "1: abc = 1: def"
// or "1: abc ! 1: def".
var rangeDiffPairRe = regexp.MustCompile(`(?m)^\s*1:\s+[0-9a-f]+\s+([=!])\s+1:\s+[0-9a-f]+\s`)

type commitID struct {
	sha, key string // key: author email, author date, subject
	ct       int64  // committer date
}

// commitIDs reads the identity of each commit git log prints for args.
func commitIDs(ctx context.Context, dir string, args ...string) ([]commitID, bool) {
	out, err := runGit(ctx, dir, append([]string{"log", "--date=raw", "--format=%H%x1f%ae%x1f%ad%x1f%ct%x1f%s"}, args...)...)
	if err != nil {
		return nil, false
	}
	var ids []commitID
	for _, l := range strings.Split(out, "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "\x1f")
		if len(f) != 5 {
			return nil, false
		}
		ct, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			return nil, false
		}
		key := ""
		if f[1] != "" && strings.TrimSpace(f[4]) != "" {
			key = f[1] + "\x1f" + f[2] + "\x1f" + f[4]
		}
		ids = append(ids, commitID{sha: f[0], key: key, ct: ct})
	}
	return ids, true
}

// supersededOn reports whether every commit of tip that git cherry cannot match
// to base was rebased as a commit on base (see above), and fills r's Proof and
// Superseded when so.
func supersededOn(ctx context.Context, dir, base, tip string, r *staleBranchResult) bool {
	pairs, ok := rebasedOnto(ctx, dir, base, tip)
	if !ok {
		return false
	}
	r.Superseded = pairs
	r.Proof = fmt.Sprintf("it is superseded by its rebase on %s: %s (same author, author date and subject, no newer than the land; git range-diff pairs them)",
		r.Ref, rebasedText(pairs))
	return true
}

// rebasedText names the pairs: "abc1234 was rebased as def5678, …".
func rebasedText(pairs []landedCommit) string {
	var parts []string
	for _, p := range pairs[:min(len(pairs), 3)] {
		parts = append(parts, short(p.SHA)+" was rebased as "+short(p.As))
	}
	if len(pairs) > len(parts) {
		parts = append(parts, fmt.Sprintf("%d more", len(pairs)-len(parts)))
	}
	return strings.Join(parts, ", ")
}

// rebasedOnto pairs each commit of tip that git cherry cannot match to base
// with the one commit on base it was rebased as (the rule above); ok is false
// when any of them has no such commit, or there are none.
func rebasedOnto(ctx context.Context, dir, base, tip string) (pairs []landedCommit, ok bool) {
	mb, err := runGit(ctx, dir, "merge-base", "--end-of-options", base, tip)
	if err != nil || mb == "" {
		return nil, false
	}
	cherry, err := runGit(ctx, dir, "cherry", "--end-of-options", base, tip, mb)
	if err != nil {
		return nil, false
	}
	var plus []string
	for _, l := range strings.Split(cherry, "\n") {
		if sha, ok := strings.CutPrefix(l, "+ "); ok {
			plus = append(plus, strings.TrimSpace(sha))
		}
	}
	if len(plus) == 0 || len(plus) > maxSuperseded {
		return nil, false
	}
	mine, ok := commitIDs(ctx, dir, append([]string{"--no-walk=unsorted", "--end-of-options"}, plus...)...)
	if !ok || len(mine) != len(plus) {
		return nil, false
	}
	// The recent commits of base, not only those since the merge base: a
	// branch that merged base after the land has its merge base past them.
	landed, ok := commitIDs(ctx, dir, "--no-merges", "-n", strconv.Itoa(maxSupersededWindow), "--end-of-options", base)
	if !ok {
		return nil, false
	}
	byKey := map[string][]commitID{}
	for _, c := range landed {
		if c.key != "" {
			byKey[c.key] = append(byKey[c.key], c)
		}
	}
	used := map[string]bool{}
	for _, c := range mine {
		got := byKey[c.key]
		if c.key == "" || len(got) != 1 || used[got[0].sha] || c.ct > got[0].ct {
			return nil, false
		}
		m := got[0]
		out, err := runGit(ctx, dir, "range-diff", "--no-color", c.sha+"^!", m.sha+"^!")
		if err != nil || !rangeDiffPairRe.MatchString(out) {
			return nil, false
		}
		used[m.sha] = true
		pairs = append(pairs, landedCommit{SHA: c.sha, As: m.sha})
	}
	return pairs, true
}

// maxMergesChecked bounds the merge commits cleanMerges looks at.
const maxMergesChecked = 20

// cleanMerges: every merge commit of tip that base lacks is a plain merge, with
// nothing in it that its parents did not have (git show --remerge-diff prints
// nothing: no conflict resolved by hand, no change slipped in). Such a merge
// carries no work of its own. No merge at all is clean too.
func cleanMerges(ctx context.Context, dir, base, tip string) bool {
	out, err := runGit(ctx, dir, "rev-list", "--merges", "--end-of-options", base+".."+tip)
	if err != nil {
		return false
	}
	merges := strings.Fields(out)
	if len(merges) > maxMergesChecked {
		return false
	}
	for _, m := range merges {
		d, err := runGit(ctx, dir, "show", "--no-color", "--remerge-diff", "--format=", "--end-of-options", m)
		if err != nil || strings.TrimSpace(d) != "" {
			return false
		}
	}
	return true
}

// rebasedProof says how a session's own commits that git cherry cannot match
// to base are in it anyway: each was rebased onto base (rebasedOnto) and every
// merge on the branch is clean. "" when that is not shown.
func rebasedProof(ctx context.Context, dir, base, baseName, tip string) string {
	if tip == "" || base == "" {
		return ""
	}
	pairs, ok := rebasedOnto(ctx, dir, base, tip)
	if !ok || !cleanMerges(ctx, dir, base, tip) {
		return ""
	}
	return fmt.Sprintf("%s %s rebased onto %s: %s (same author, author date and subject; git range-diff pairs them)",
		itoa(len(pairs)), plural(len(pairs), "commit was", "commits were"), baseName, rebasedText(pairs))
}

// backupRefPrefix holds the tips of remote branches the hub deleted.
const backupRefPrefix = "refs/comebye/backup/"

// backupRef keeps tip under refs/comebye/backup/<branch> in dir's repository
// (shared by all its worktrees) and returns the ref. A ref already there at tip
// is kept; one at another commit is never moved: the tip goes under
// <branch>-<short tip> instead, and an error is returned when that is taken too.
func backupRef(ctx context.Context, dir, branch, tip string) (string, error) {
	for _, ref := range []string{backupRefPrefix + branch, backupRefPrefix + branch + "-" + tip[:12]} {
		cur, _ := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
		switch cur = strings.TrimSpace(cur); {
		case cur == tip:
			return ref, nil
		case cur != "":
			continue
		}
		// The empty old value: create it only if it does not exist.
		if _, err := runGit(ctx, dir, "update-ref", "-m", "comebye: backup of origin/"+branch+" before deleting it", ref, tip, ""); err != nil {
			return "", err
		}
		return ref, nil
	}
	return "", fmt.Errorf("%s already holds other commits", backupRefPrefix+branch)
}

// restoreFromBackupCmd puts a deleted remote branch back from its backup ref
// (run in the repository on the session's host).
func restoreFromBackupCmd(ref, branch string) string {
	return "git push origin " + ref + ":refs/heads/" + branch
}
