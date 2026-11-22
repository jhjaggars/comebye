package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

func TestParseWhatIsLeft(t *testing.T) {
	// NOTHING LEFT is the old SAFE; replies in the old format still parse.
	for _, in := range []string{"NOTHING LEFT", "**Nothing left**", "Nothing is left.", "nothing left — all merged", "SAFE", "NOTHING LEFT\n- pr o/r#1"} {
		if state, _, items := parseReply(in); state != ledger.AskSafe || len(items) != 0 {
			t.Errorf("%q = %s %v; want safe, no items", in, state, items)
		}
	}
	if _, reason := parseAnswer("NOTHING LEFT: all merged"); reason != "all merged" {
		t.Errorf("reason = %q", reason)
	}
	// "Nothing" alone is not an answer.
	if state, _, _ := parseReply("Nothing much happened here"); state != ledger.AskUnclear {
		t.Errorf("state = %s", state)
	}
	state, reason, items := parseReply("LEFT\n" +
		"- decision t0123456789 waiting on the user: which key to use\n" +
		"- cleanup delete branch feat/x and its worktree (me, once told to go ahead)\n" +
		"- cleanup stop the dev server I started (pid 4242)\n" +
		"- pr o/r#7 open, waiting on review")
	if state != ledger.AskUnsafe {
		t.Fatalf("state = %s", state)
	}
	want := []ledger.AskItem{
		{Type: "decision", ID: "t0123456789", Text: "waiting on the user: which key to use"},
		{Type: "cleanup", Text: "delete branch feat/x and its worktree (me, once told to go ahead)"},
		{Type: "cleanup", Text: "stop the dev server I started (pid 4242)"},
		{Type: "pr", Repo: "o/r", Number: 7, Text: "open, waiting on review"},
	}
	if len(items) != len(want) {
		t.Fatalf("items = %+v", items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("item %d = %+v; want %+v", i, items[i], want[i])
		}
	}
	if !strings.Contains(reason, "decision (to-do t0123456789)") {
		t.Errorf("reason = %q", reason)
	}
	// A decision without a to-do id keeps its words; an empty cleanup is no item.
	_, _, items = parseReply("LEFT\n- decision which cluster to use\n- cleanup\n- **cleanup** drop the scratch branch")
	if len(items) != 2 || items[0].Type != "decision" || items[0].ID != "" || items[0].Text != "which cluster to use" || items[1].Type != "cleanup" || items[1].Text != "drop the scratch branch" {
		t.Errorf("items = %+v", items)
	}
	// The old UNSAFE still lists its items.
	if state, _, items := parseReply("UNSAFE\n- cleanup close my PR"); state != ledger.AskUnsafe || len(items) != 1 || items[0].Type != "cleanup" {
		t.Errorf("old format: %s %+v", state, items)
	}
}

func TestAskPromptAsksWhatIsLeft(t *testing.T) {
	p := askPrompt("landed", "coordinator", "")
	for _, want := range []string{"What is left to finish this work?", "who each item waits on", "waiting on the user", "NOTHING LEFT, or LEFT",
		"- decision TODO_ID", "- cleanup <", "do not do the cleanup now", "without asking, delete your own scratch", "Not scratch"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestRecheckDecisionAndCleanup(t *testing.T) {
	s, _, _ := coordServer(t)
	done, _, _ := s.todos.add(Todo{Question: "q", Kind: "decision", State: TodoOpen, CreatedBy: "w"})
	if _, err := s.todos.close(done.ID, TodoDone, "me", ""); err != nil {
		t.Fatal(err)
	}
	open, _, _ := s.todos.add(Todo{Question: "q2", Kind: "decision", State: TodoOpen, CreatedBy: "w"})
	it := &removal{ID: "desk/w2:p1", Host: "desk"}
	check := func(item ledger.AskItem, state, now string) {
		t.Helper()
		if st, n, _ := s.recheck(context.Background(), it, item); st != state || !strings.Contains(n, now) {
			t.Errorf("%+v = %s %q; want %s ~%q", item, st, n, state, now)
		}
	}
	check(ledger.AskItem{Type: "decision", ID: done.ID}, itemResolved, "is done")
	check(ledger.AskItem{Type: "decision", ID: open.ID}, itemOpen, "still open")
	check(ledger.AskItem{Type: "decision", ID: "t0000000000"}, itemUnchecked, "not in this hub")
	check(ledger.AskItem{Type: "decision", Text: "which cluster"}, itemUnchecked, "names no to-do")
	check(ledger.AskItem{Type: "cleanup", Text: "delete branch"}, itemUnchecked, "can do this itself")
	if got := itemLabel(ledger.AskItem{Type: "decision", ID: "t0000000000"}); got != "decision (to-do t0000000000)" {
		t.Errorf("label = %q", got)
	}
}

// A mark whose session lists only cleanup shows only_cleanup; with a decision
// beside it, it does not.
func TestReviewOnlyCleanup(t *testing.T) {
	s, _ := askServer(t, "")
	cl := ledger.AskItem{Type: "cleanup", Text: "delete branch feat/x"}
	markUnsafe(t, s, time.Minute, cl, ledger.AskItem{Type: "cleanup", Text: "close my PR"})
	ar := reviewOf(t, s, "desk/w2:p1").AskReview
	if ar == nil || !ar.OnlyCleanup || ar.Resolved {
		t.Fatalf("review = %+v", ar)
	}
	markUnsafe(t, s, time.Minute, cl, ledger.AskItem{Type: "decision", ID: "t0123456789"})
	if ar := reviewOf(t, s, "desk/w2:p1").AskReview; ar == nil || ar.OnlyCleanup {
		t.Fatalf("a decision beside cleanup: %+v", ar)
	}
}

// replies lets a test change what the session "says" between asks.
func replyServer(t *testing.T) (*Server, *fakeLaunchHerdr, func(string)) {
	t.Helper()
	s, fh, _ := coordServer(t)
	s.askPoll, s.askWait = 10*time.Millisecond, 400*time.Millisecond
	t.Cleanup(s.asking.Wait)
	var mu sync.Mutex
	text, stamp := "Done: the tests pass.", "2026-10-23T16:00:00Z"
	s.latest = func(ctx context.Context, host, pane string) *transcript.Latest {
		mu.Lock()
		defer mu.Unlock()
		return &transcript.Latest{Text: text, Time: stamp}
	}
	return s, fh, func(reply string) {
		mu.Lock()
		defer mu.Unlock()
		text, stamp = reply, time.Now().UTC().Format(time.RFC3339Nano)
	}
}

func TestLetItCleanUpSendsExactlyTheListedCleanupThenAsksAgain(t *testing.T) {
	s, fh, say := replyServer(t)
	markUnsafe(t, s, time.Minute, ledger.AskItem{Type: "cleanup", Text: "delete branch feat/x"}, ledger.AskItem{Type: "cleanup", Text: "close my PR"})
	w := do(s, "POST", "/api/marks/cleanup", userHdr, `{"agent_id":"desk/w2:p1"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	waitFor(t, func() bool { return promptCount(fh) == 1 })
	p := lastPrompt(t, fh)
	for _, want := range []string{"Do exactly this, nothing else", "1. delete branch feat/x", "2. close my PR", "report"} {
		if !strings.Contains(p, want) {
			t.Errorf("go-ahead lacks %q:\n%s", want, p)
		}
	}
	if m := markOf(t, s, "desk/w2:p1"); m.Ask.CleaningAt == nil {
		t.Fatal("not cleaning up")
	}
	// While it cleans up, nothing else may ask it, and a second go-ahead is refused.
	if w := do(s, "POST", "/api/marks/cleanup", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != http.StatusConflict {
		t.Fatalf("second go-ahead: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/marks/reask", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != http.StatusConflict {
		t.Fatalf("reask while cleaning: %d %s", w.Code, w.Body)
	}
	// It reports; the hub asks what is left, and its answer replaces the old one.
	say("Deleted the branch and closed the PR.")
	waitFor(t, func() bool { return promptCount(fh) == 2 })
	if !strings.Contains(lastPrompt(t, fh), "What is left to finish this work?") {
		t.Fatalf("not asked again: %s", lastPrompt(t, fh))
	}
	say("NOTHING LEFT")
	waitFor(t, func() bool {
		a := markOf(t, s, "desk/w2:p1").Ask
		return a.State == ledger.AskSafe && a.ReaskedAt == nil && a.CleaningAt == nil
	})
}

func TestLetItCleanUpRefusals(t *testing.T) {
	s, fh, _ := replyServer(t)
	// Not marked.
	if w := do(s, "POST", "/api/marks/cleanup", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 404 {
		t.Fatalf("unmarked: %d", w.Code)
	}
	// A decision is left: only the user can finish it.
	markUnsafe(t, s, time.Minute, ledger.AskItem{Type: "cleanup", Text: "x"}, ledger.AskItem{Type: "decision", ID: "t0123456789"})
	if w := do(s, "POST", "/api/marks/cleanup", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "not_cleanup") {
		t.Fatalf("decision beside cleanup: %d %s", w.Code, w.Body)
	}
	// Nothing listed.
	markUnsafe(t, s, time.Minute, ledger.AskItem{Type: "other", Text: "a loop"})
	if w := do(s, "POST", "/api/marks/cleanup", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 409 {
		t.Fatalf("other: %d", w.Code)
	}
	if promptCount(fh) != 0 {
		t.Fatalf("prompts = %d", promptCount(fh))
	}
}

// An auto-marked landed session is asked what is left; "NOTHING LEFT" is the
// answer that lets the hub remove it by itself.
func TestAutoCleanupAsksWhatIsLeftAndNothingLeftIsSafe(t *testing.T) {
	s, _, e := landedGreen(t)
	s.latest = func(ctx context.Context, host, pane string) *transcript.Latest {
		return &transcript.Latest{Text: "NOTHING LEFT", Time: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	s.autoCleanOne(e)
	waitFor(t, func() bool {
		g, _ := s.ledger.Get(e.ID)
		return g.Mark != nil && g.Mark.Ask != nil && g.Mark.Ask.State == ledger.AskSafe
	})
}

// A real answer (coord-dir-s2): everything done, "the one loose end is the
// local branch, which I cannot delete from its own worktree". That is not
// work left: removing the session removes the worktree, and the branch goes
// with it once its commits are in the base.
func TestDeletingTheOwnLocalBranchIsNothingLeft(t *testing.T) {
	const reply = "LEFT\n- cleanup the local branch feat/coord-dir-s2-dir, which I cannot delete from its own worktree: git branch -d feat/coord-dir-s2-dir"
	state, _, items := parseReply(reply)
	if state != ledger.AskUnsafe || len(items) != 1 || items[0].Type != ledger.ItemCleanup {
		t.Fatalf("parsed %s %+v", state, items)
	}
	s, _, _ := coordServer(t)
	merged := &worktreeChecks{Exists: true, Branch: "feat/coord-dir-s2-dir", BaseUsed: "origin/main"}
	it := &removal{ID: "desk/w2:p1", Host: "desk", Worktree: "/wt", Branch: "feat/coord-dir-s2-dir", Base: "main", Checks: merged}
	if st, now, _ := s.recheck(context.Background(), it, items[0]); st != itemResolved || !strings.Contains(now, "goes with the worktree") {
		t.Fatalf("merged branch: %s %q", st, now)
	}
	// The same words typed "other" resolve too, and the review counts the answer as done.
	other := ledger.AskItem{Type: "other", Text: "git branch -d feat/coord-dir-s2-dir is all that is left to do"}
	if st, _, _ := s.recheck(context.Background(), it, other); st != itemResolved {
		t.Fatalf("other: %s", st)
	}
	// Commits not in the base: the branch is real work, so it stays open.
	it.Checks = &worktreeChecks{Exists: true, Branch: "feat/coord-dir-s2-dir", BaseUsed: "origin/main", Unmerged: 2, Detail: &worktreeDetail{Unpushed: 2}}
	if st, _, _ := s.recheck(context.Background(), it, items[0]); st == itemResolved {
		t.Fatal("an unmerged branch resolved")
	}
	// Other branches, remote branches and other chores are not it.
	it.Checks = merged
	for _, text := range []string{"delete branch other/thing", "delete the remote branch feat/coord-dir-s2-dir on origin", "close my PR", "stop the dev server"} {
		if ownBranchDeletion(text, it.Branch) {
			t.Errorf("%q counted as the own branch", text)
		}
	}
	if !ownBranchDeletion("delete my local branch", it.Branch) {
		t.Error("an unnamed own branch")
	}
	// And the prompt tells sessions not to list it.
	if p := askPrompt("", "", ""); !strings.Contains(p, "Do not list your own local branch") {
		t.Errorf("prompt:\n%s", p)
	}
}
