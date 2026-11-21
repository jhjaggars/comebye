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
//   - each of those commits has exactly one commit on origin/<default> since
//     that merge base with the same author (email), the same author date (with
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
	mb, err := runGit(ctx, dir, "merge-base", "--end-of-options", base, tip)
	if err != nil || mb == "" {
		return false
	}
	cherry, err := runGit(ctx, dir, "cherry", "--end-of-options", base, tip, mb)
	if err != nil {
		return false
	}
	var plus []string
	for _, l := range strings.Split(cherry, "\n") {
		if sha, ok := strings.CutPrefix(l, "+ "); ok {
			plus = append(plus, strings.TrimSpace(sha))
		}
	}
	if len(plus) == 0 || len(plus) > maxSuperseded {
		return false
	}
	mine, ok := commitIDs(ctx, dir, append([]string{"--no-walk=unsorted", "--end-of-options"}, plus...)...)
	if !ok || len(mine) != len(plus) {
		return false
	}
	landed, ok := commitIDs(ctx, dir, "--no-merges", "-n", strconv.Itoa(maxSupersededWindow), "--end-of-options", mb+".."+base)
	if !ok {
		return false
	}
	byKey := map[string][]commitID{}
	for _, c := range landed {
		if c.key != "" {
			byKey[c.key] = append(byKey[c.key], c)
		}
	}
	used := map[string]bool{}
	var pairs []landedCommit
	for _, c := range mine {
		got := byKey[c.key]
		if c.key == "" || len(got) != 1 || used[got[0].sha] || c.ct > got[0].ct {
			return false
		}
		m := got[0]
		out, err := runGit(ctx, dir, "range-diff", "--no-color", c.sha+"^!", m.sha+"^!")
		if err != nil || !rangeDiffPairRe.MatchString(out) {
			return false
		}
		used[m.sha] = true
		pairs = append(pairs, landedCommit{SHA: c.sha, As: m.sha})
	}
	var parts []string
	for _, p := range pairs[:min(len(pairs), 3)] {
		parts = append(parts, short(p.SHA)+" was rebased as "+short(p.As))
	}
	if len(pairs) > len(parts) {
		parts = append(parts, fmt.Sprintf("%d more", len(pairs)-len(parts)))
	}
	r.Superseded = pairs
	r.Proof = fmt.Sprintf("it is superseded by its rebase on %s: %s (same author, author date and subject, no newer than the land; git range-diff pairs them)",
		r.Ref, strings.Join(parts, ", "))
	return true
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
