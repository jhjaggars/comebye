package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// answeredNothing makes the fixture's session one whose agent is gone and that
// never answered (the hub's proof alone decides), with no leftover to settle.
func (f *goneFix) answeredNothing(t *testing.T) {
	t.Helper()
	if _, _, err := f.s.ledger.Update(f.e.ID, func(e *ledger.Entry) {
		e.Mark.Ask = &ledger.Ask{State: ledger.AskNoAnswer, Reason: "the agent was gone"}
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *goneFix) status(t *testing.T) ledger.Status {
	t.Helper()
	e, ok := f.s.ledger.Get(f.e.ID)
	if !ok {
		t.Fatal("no entry")
	}
	return e.Status
}

func hasLocalBranch(t *testing.T, r landedRepo, b string) bool {
	t.Helper()
	return git(t, r.repo, "branch", "--list", b) != ""
}

// A session with nothing to lose is removed: the session, its worktree and its
// local branch, and it is listed with the sweep's reason for a day.
func TestSweepCleanRemovesASafeSession(t *testing.T) {
	f := newGoneFix(t, true, "")
	f.answeredNothing(t)
	now := time.Now()
	done := f.s.sweepCleanStep(ctx(), now, true)
	if len(done) != 1 || done[0].Kind != doneSession || done[0].Branch != "w" || done[0].Tip == "" {
		t.Fatalf("done = %+v", done)
	}
	if f.status(t) != ledger.Removed {
		t.Fatalf("status = %s", f.status(t))
	}
	if _, err := os.Stat(f.r.wt); err == nil {
		t.Fatal("the worktree is still there")
	}
	if hasLocalBranch(t, f.r, "w") {
		t.Fatal("the local branch is still there")
	}
	if recent := f.s.recentAutoRemovals(now); len(recent) != 1 || !strings.Contains(recent[0].Reason, "nothing to lose") || !recent[0].WorktreeRemoved {
		t.Fatalf("recent = %+v", recent)
	}
	// A second run does nothing.
	if again := f.s.sweepCleanStep(ctx(), now.Add(time.Hour), true); len(again) != 0 {
		t.Fatalf("second run = %+v", again)
	}
	if got := len(f.s.sweeps.get().Done); got != 1 {
		t.Fatalf("done recorded %d times", got)
	}
}

// A session that changed since it was marked is not removed: the loop looks
// again on the host right before removing.
func TestSweepCleanSkipsASessionThatChangedSinceMarked(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, f *goneFix){
		"new commit":      func(t *testing.T, f *goneFix) { f.r.edit(t, f.r.wt, 20, "new work after the mark") },
		"dirty tree":      func(t *testing.T, f *goneFix) { os.WriteFile(filepath.Join(f.r.wt, "new.txt"), []byte("x\n"), 0o644) },
		"unpushed commit": func(t *testing.T, f *goneFix) { f.r.edit(t, f.r.wt, 21, "only here") },
		"modified tracked": func(t *testing.T, f *goneFix) {
			os.WriteFile(filepath.Join(f.r.wt, "f.txt"), []byte("changed\n"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGoneFix(t, true, "")
			f.answeredNothing(t)
			change(t, f)
			if done := f.s.sweepCleanStep(ctx(), time.Now(), true); len(done) != 0 {
				t.Fatalf("removed: %+v", done)
			}
			if f.status(t) != ledger.ReadyToRemove {
				t.Fatalf("status = %s", f.status(t))
			}
			if _, err := os.Stat(f.r.wt); err != nil {
				t.Fatalf("worktree gone: %v", err)
			}
		})
	}
}

// Work that never reached main keeps its session.
func TestSweepCleanKeepsASessionWhoseWorkIsNotInMain(t *testing.T) {
	f := newGoneFix(t, false, "")
	f.answeredNothing(t)
	if done := f.s.sweepCleanStep(ctx(), time.Now(), true); len(done) != 0 || f.status(t) != ledger.ReadyToRemove {
		t.Fatalf("removed %+v, status %s", done, f.status(t))
	}
}

// A leftover the hub cannot settle (a process to stop) keeps the session in the
// review with no To do.
func TestSweepCleanKeepsAnUnsettledLeftover(t *testing.T) {
	f := newGoneFix(t, true, "Stop the dev server I started (pid 4242).")
	if done := f.s.sweepCleanStep(ctx(), time.Now(), true); len(done) != 0 || f.status(t) != ledger.ReadyToRemove {
		t.Fatalf("removed %+v, status %s", done, f.status(t))
	}
	if all, _ := sweepItemsOf(f.s); len(all) != 0 {
		t.Fatalf("a To do was filed: %+v", all)
	}
}

// A backup branch with one commit main lacks, or with an open PR, is never
// deleted, and its session stays.
func TestSweepLoopNeverDeletesABranchWithExtraWorkOrAnOpenPR(t *testing.T) {
	t.Run("one extra commit", func(t *testing.T) {
		f := newGoneFix(t, true, "")
		f.r.edit(t, f.r.wt, 12, "one more")
		git(t, f.r.wt, "push", "-q", "origin", "w")
		f.s.sweepLoopStep(ctx(), time.Now())
		if !remoteHas(t, f.r, "w") || f.status(t) != ledger.ReadyToRemove || len(f.s.sweeps.get().Done) != 0 {
			t.Fatalf("branch kept %v, status %s, done %+v", remoteHas(t, f.r, "w"), f.status(t), f.s.sweeps.get().Done)
		}
	})
	t.Run("open PR", func(t *testing.T) {
		f := newGoneFix(t, true, "")
		stubOpenPRs(t, []ghPR{{Number: 7}}, nil)
		f.s.sweepLoopStep(ctx(), time.Now())
		if !remoteHas(t, f.r, "w") || f.status(t) != ledger.ReadyToRemove || len(f.s.sweeps.get().Done) != 0 {
			t.Fatalf("branch kept %v, status %s, done %+v", remoteHas(t, f.r, "w"), f.status(t), f.s.sweeps.get().Done)
		}
	})
}

// With the setting off the loop only marks: nothing removed, nothing deleted,
// no To do.
func TestSweepCleanSettingOffOnlyMarks(t *testing.T) {
	f := newGoneFix(t, true, "")
	f.answeredNothing(t)
	f.setAuto(t, false)
	f.s.sweepLoopStep(ctx(), time.Now())
	if f.status(t) != ledger.ReadyToRemove || !remoteHas(t, f.r, "w") || len(f.s.sweeps.get().Done) != 0 {
		t.Fatalf("status %s done %+v", f.status(t), f.s.sweeps.get().Done)
	}
	if all, _ := sweepItemsOf(f.s); len(all) != 0 {
		t.Fatalf("a To do was filed: %+v", all)
	}
	// The old second switch is read and dropped.
	if code, body := putSweepSetting(t, f.s, `{"sweep_loop":{"enabled":true,"auto_delete_branches":true}}`); code != 200 || strings.Contains(body, "auto_delete_branches") {
		t.Fatalf("put = %d %s", code, body)
	}
}

// The scratch directory a removal deletes is the one item the loop counts as done.
func TestScratchItemIsOnlyTheSessionsOwnScratchPath(t *testing.T) {
	f := newGoneFix(t, true, "")
	root := t.TempDir()
	f.s.cfg.ScratchDir = root
	it := &removal{Host: f.s.fleet.LocalName(), Worktree: f.r.wt}
	dir := filepath.Join(root, "claude-"+itoa(os.Getuid()), strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(f.r.wt))
	for text, want := range map[string]bool{
		"Delete the scratch directory " + dir:                          true,
		"Remove " + dir + "/session1/scratchpad (my scratch dir)":      true,
		"Delete the scratch dir " + dir + " and /home/me/important":    false,
		"Delete " + filepath.Join(root, "claude-"+itoa(os.Getuid())):   false, // the whole root
		"Delete " + dir + "-other":                                     false, // a sibling
		"Keep the scratch directory " + dir:                            false, // no deletion verb
		"Delete the remote branch origin/w and the scratch dir " + dir: false,
		"Delete the scratch directory (no path given)":                 false,
	} {
		if got := f.s.scratchItem(text, it); got != want {
			t.Errorf("%q = %v, want %v", text, got, want)
		}
	}
	it.Host = "elsewhere"
	if f.s.scratchItem("Delete "+dir, it) {
		t.Error("another host's scratch is not the hub's to judge")
	}
}

// The wording sessions really used (slice-go-flakes, page-chat-s5, test-audit,
// review-hardening-1): only an item naming the session's own scratch path counts.
func TestScratchItemRealWording(t *testing.T) {
	f := newGoneFix(t, true, "")
	f.s.cfg.ScratchDir = "/tmp"
	own := func(name string) *removal {
		return &removal{Host: f.s.fleet.LocalName(), Worktree: "/home/jhjaggars/.worktrees/comebye/" + name}
	}
	for _, c := range []struct {
		name, text string
		want       bool
	}{
		{"ci-go-flakes", "Scratch files remain in `/tmp/claude-"+itoa(os.Getuid())+"/-home-jhjaggars--worktrees-comebye-ci-go-flakes/7faa4e22-1554-4231-a596-c00c9d286e3f/scratchpad/`: `server.base` and `server.new` (about 40 MB each, race test binaries). I can delete them once told to go ahead.", true},
		{"feat-page-chat-5-handback", "throwaway files from the live check under `/tmp/claude-"+itoa(os.Getuid())+"/-home-jhjaggars--worktrees-comebye-feat-page-chat-5-handback/c78d827b-3134-4f20-a808-31d6a310a439/scratchpad/` (state, code, bin, logs). They are scratch only, and I can delete them if told to go ahead.", true},
		{"audit-tests", "temp files: scratch logs and screenshots under /tmp/claude-"+itoa(os.Getuid())+"/ (CI logs, local test-run logs, page build files). I can delete them on a go-ahead; nothing depends on them.", false},
		{"review-hardening-batch-1", "delete three small leftover Claude session folders from my permission-mode proofs, under `/home/jhjaggars/.claude/projects/`, with names ending `scratchpad-cl-A`", false},
		{"audit-tests", "pid 1561652: a background wait-loop I started that never exited. I can stop it once told to go ahead.", false},
	} {
		if got := f.s.scratchItem(c.text, own(c.name)); got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, c.text[:40], got, c.want)
		}
	}
}
