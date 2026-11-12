package server

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

func hubText(h hubCheck) string {
	var b strings.Builder
	for _, i := range h.Items {
		b.WriteString(i.Kind + "/" + i.State + ": " + i.Text + "\n")
	}
	return b.String()
}

func TestHubCheckFlagsContradictions(t *testing.T) {
	e := ledger.Entry{ID: "h1", FinalReport: "## Verification\nPushed to origin. CI green on the head. Worktree clean. Fixed in abcdef1.\n"}
	c := &worktreeChecks{
		Exists: true, Dirty: true, DirtyFiles: 2, BaseUsed: "origin/main", Unmerged: 1,
		Verified: []commitFact{{SHA: "abcdef1"}},
		Detail:   &worktreeDetail{Unpushed: 3, Upstream: "origin/x"},
		CI:       &ledger.CI{State: ledger.CIFailure, SHA: "0123456789abc"},
	}
	h := hubCheckOf(e, c, time.Now())
	if h.Contradictions != 4 {
		t.Fatalf("contradictions = %d, want 4:\n%s", h.Contradictions, hubText(h))
	}
	for _, want := range []string{"commit/bad", "pushed/bad", "ci/bad", "worktree/bad"} {
		if !strings.Contains(hubText(h), want) {
			t.Errorf("missing %s:\n%s", want, hubText(h))
		}
	}
}

func TestHubCheckAgreeingReport(t *testing.T) {
	e := ledger.Entry{ID: "h2", FinalReport: "Done. Not pushed yet; CI not run. Tests pass."}
	c := &worktreeChecks{
		Exists: true, BaseUsed: "origin/main", Unmerged: 1,
		Detail: &worktreeDetail{Unpushed: 1},
		CI:     &ledger.CI{State: ledger.CINone, SHA: "0123456789abc"},
	}
	h := hubCheckOf(e, c, time.Now())
	if h.Contradictions != 0 {
		t.Fatalf("flagged an honest report:\n%s", hubText(h))
	}
	if !strings.Contains(hubText(h), "pushed/warn") || !strings.Contains(hubText(h), "worktree/ok") {
		t.Fatalf("facts missing:\n%s", hubText(h))
	}
	if n := hubCheckOf(ledger.Entry{}, c, time.Now()); n.Note == "" {
		t.Fatal("no report should say so")
	}
}

func hcGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestVerifyCommits(t *testing.T) {
	dir := t.TempDir()
	hcGit(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(dir+"/a", []byte("a"), 0o600)
	hcGit(t, dir, "add", "a")
	hcGit(t, dir, "commit", "-qm", "one")
	onMain := hcGit(t, dir, "rev-parse", "HEAD")
	hcGit(t, dir, "checkout", "-qb", "feat")
	os.WriteFile(dir+"/b", []byte("b"), 0o600)
	hcGit(t, dir, "add", "b")
	hcGit(t, dir, "commit", "-qm", "two")
	onFeat := hcGit(t, dir, "rev-parse", "HEAD")

	got := verifyCommits(context.Background(), dir, "feat", "main", []string{onMain[:9], onFeat[:9], "deadbee1234"})
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if f := got[0]; !f.Exists || !f.InBase || !f.OnBranch {
		t.Errorf("main commit: %+v", f)
	}
	if f := got[1]; !f.Exists || f.InBase || !f.OnBranch {
		t.Errorf("branch commit: %+v", f)
	}
	if f := got[2]; f.Exists {
		t.Errorf("made-up sha exists: %+v", f)
	}
}

func TestClaimedPushedFalsePositives(t *testing.T) {
	for _, no := range []string{
		"Nothing is pushed.", "Nothing has been pushed yet.", "None of it is pushed.",
		"The branch isn't pushed.", "Not pushed (the user lands it).", "Will be pushed after review.",
		"Needs to be pushed by the coordinator.", "I have not pushed.", "Push it once approved; pushed: no",
	} {
		if claimed(no, pushedRe, notPushedRe) {
			t.Errorf("read as a pushed claim: %q", no)
		}
	}
	for _, yes := range []string{"Pushed to origin.", "All commits pushed.", "Branch pushed: feat/x"} {
		if !claimed(yes, pushedRe, notPushedRe) {
			t.Errorf("missed a pushed claim: %q", yes)
		}
	}
}

func TestHubCheckLandedClaim(t *testing.T) {
	c := &worktreeChecks{Exists: true, BaseUsed: "origin/main", Unmerged: 2, Detail: &worktreeDetail{Unpushed: 0, Upstream: "origin/x"}}
	h := hubCheckOf(ledger.Entry{ID: "l1", FinalReport: "All done and landed on main."}, c, time.Now())
	if !strings.Contains(hubText(h), "landed/bad") || h.Contradictions != 1 {
		t.Fatalf("landed claim with 2 commits not in the base not flagged:\n%s", hubText(h))
	}
	for _, honest := range []string{"Do not land. Ready to land when you say.", "Not landed yet.", "File a To do: Land feat/x?", "Will be merged by the coordinator."} {
		h := hubCheckOf(ledger.Entry{ID: "l2", FinalReport: honest}, c, time.Now())
		if h.Contradictions != 0 {
			t.Errorf("flagged %q:\n%s", honest, hubText(h))
		}
	}
	c.Unmerged = 0
	if h := hubCheckOf(ledger.Entry{ID: "l3", FinalReport: "landed"}, c, time.Now()); h.Contradictions != 0 {
		t.Fatalf("flagged a true landed claim:\n%s", hubText(h))
	}
}
