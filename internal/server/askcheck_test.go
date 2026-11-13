package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// stubProbes stands in for gh, the process table and k3d.
func stubProbes(t *testing.T, pr map[int]*probeResult, alive map[int]bool, clusters []string) {
	t.Helper()
	oPR, oPID, oK3D := probeGHPR, probePIDAlive, probeK3D
	t.Cleanup(func() { probeGHPR, probePIDAlive, probeK3D = oPR, oPID, oK3D })
	probeGHPR = func(ctx context.Context, repo string, n int) (*probeResult, error) {
		if r, ok := pr[n]; ok {
			return r, nil
		}
		return nil, context.DeadlineExceeded
	}
	probePIDAlive = func(pid int) bool { return alive[pid] }
	probeK3D = func(ctx context.Context, name string) (bool, error) {
		for _, c := range clusters {
			if c == name {
				return true, nil
			}
		}
		return false, nil
	}
}

func agoT(d time.Duration) *time.Time { t := time.Now().UTC().Add(-d); return &t }

// markUnsafe marks desk/w2:p1 with an UNSAFE answer given `age` ago.
func markUnsafe(t *testing.T, s *Server, age time.Duration, items ...ledger.AskItem) ledger.Mark {
	t.Helper()
	at := time.Now().UTC().Add(-age)
	m := ledger.Mark{AgentID: "desk/w2:p1", Host: "desk", PaneID: "w2:p1", Name: "home", By: "coordinator", Reason: "both PRs merged", At: at.Add(-time.Minute),
		Ask: &ledger.Ask{State: ledger.AskUnsafe, Reason: "see items", Items: items, At: at, AskedAt: &at}}
	if _, err := s.ledger.SetMark(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRecheckResolutionPerType(t *testing.T) {
	s, _, _ := coordServer(t)
	s.probeTTL = 0
	stubProbes(t, map[int]*probeResult{
		343: {State: "merged", At: agoT(5 * time.Hour)},
		344: {State: "open"},
		345: {State: "closed", At: agoT(time.Hour)},
		346: {State: "unknown", Detail: "gh: not logged in"},
	}, map[int]bool{100: true, 200: false}, []string{"dev"})

	done, _, _ := s.todos.add(Todo{Question: "q", Kind: "decision", State: TodoOpen, CreatedBy: "w"})
	if _, err := s.todos.close(done.ID, TodoDone, "me", ""); err != nil {
		t.Fatal(err)
	}
	open, _, _ := s.todos.add(Todo{Question: "q2", Kind: "decision", State: TodoOpen, CreatedBy: "w"})

	it := &removal{ID: "desk/w2:p1", Host: "desk", Worktree: "/wt", Branch: "feat/x", Base: "main"}
	check := func(item ledger.AskItem, state, now string) *time.Time {
		t.Helper()
		st, n, at := s.recheck(context.Background(), it, item)
		if st != state || !strings.Contains(n, now) {
			t.Errorf("%+v = %s %q; want %s ~%q", item, st, n, state, now)
		}
		return at
	}
	// PRs: merged and closed resolve (with when), open does not, gh trouble is unchecked.
	if at := check(ledger.AskItem{Type: "pr", Repo: "o/r", Number: 343}, itemResolved, "merged 5 h ago"); at == nil {
		t.Error("a merged PR has no time")
	}
	check(ledger.AskItem{Type: "pr", Repo: "o/r", Number: 344}, itemOpen, "still open")
	check(ledger.AskItem{Type: "pr", Repo: "o/r", Number: 345}, itemResolved, "closed without merging")
	check(ledger.AskItem{Type: "pr", Repo: "o/r", Number: 346}, itemUnchecked, "not logged in")
	check(ledger.AskItem{Type: "pr", Repo: "o/r", Number: 999}, itemUnchecked, "can't check")
	check(ledger.AskItem{Type: "pr", Number: 5}, itemUnchecked, "names no repository")
	check(ledger.AskItem{Type: "pr", Repo: "o/r; rm -rf", Number: 5}, itemUnchecked, "not a repository")
	// To-dos come from the hub's own store.
	if at := check(ledger.AskItem{Type: "todo", ID: done.ID}, itemResolved, "is done"); at == nil {
		t.Error("a closed to-do has no time")
	}
	check(ledger.AskItem{Type: "todo", ID: open.ID}, itemOpen, "still open")
	check(ledger.AskItem{Type: "todo", ID: "t0000000000"}, itemUnchecked, "not in this hub")
	// Resources: a pid and a k3d cluster are looked at; anything else is not.
	check(ledger.AskItem{Type: "resource", Kind: "pid", Name: "200"}, itemResolved, "no longer running")
	check(ledger.AskItem{Type: "resource", Kind: "pid", Name: "100"}, itemOpen, "still running")
	check(ledger.AskItem{Type: "resource", Kind: "pid", Name: "abc"}, itemUnchecked, "not a pid")
	check(ledger.AskItem{Type: "resource", Kind: "k3d-cluster", Name: "gone"}, itemResolved, "is gone")
	check(ledger.AskItem{Type: "resource", Kind: "k3d-cluster", Name: "dev"}, itemOpen, "still there")
	check(ledger.AskItem{Type: "resource", Kind: "container", Name: "pg"}, itemUnchecked, "does not look for a container")
	check(ledger.AskItem{Type: "resource", Kind: "pid", Name: "100", Host: "nowhere"}, itemUnchecked, "not in this hub's fleet")
	check(ledger.AskItem{Type: "other", Text: "a loop"}, itemUnchecked, "free text")
}

func TestRecheckCommits(t *testing.T) {
	s, _, _ := coordServer(t)
	it := func(c *worktreeChecks) *removal {
		return &removal{ID: "desk/w2:p1", Host: "desk", Worktree: "/wt", Branch: "feat/x", Base: "main", Checks: c}
	}
	item := ledger.AskItem{Type: "commits", Branch: "feat/x", Remote: "origin"}
	for _, c := range []struct {
		name  string
		c     worktreeChecks
		state string
		now   string
	}{
		{"merged", worktreeChecks{Exists: true, Branch: "feat/x", Unmerged: 0, BaseUsed: "origin/main"}, itemResolved, "all in origin/main"},
		{"squashed", worktreeChecks{Exists: true, Branch: "feat/x", Unmerged: 2, Equivalent: "merged as PR #3", BaseUsed: "main"}, itemResolved, "merged as PR #3"},
		{"pushed", worktreeChecks{Exists: true, Branch: "feat/x", Unmerged: 2, BaseUsed: "main", Detail: &worktreeDetail{Unpushed: 0, Upstream: "origin/feat/x"}}, itemResolved, "all on a remote"},
		{"unpushed", worktreeChecks{Exists: true, Branch: "feat/x", Unmerged: 2, BaseUsed: "main", Detail: &worktreeDetail{Unpushed: 1}}, itemOpen, "1 on no remote"},
		{"unknown", worktreeChecks{Exists: true, Branch: "feat/x", Unmerged: -1, Error: "bad ref"}, itemUnchecked, "bad ref"},
		{"no worktree", worktreeChecks{Exists: false}, itemUnchecked, "gone"},
	} {
		st, now, _ := s.recheck(context.Background(), it(&c.c), item)
		if st != c.state || !strings.Contains(now, c.now) {
			t.Errorf("%s: %s %q; want %s ~%q", c.name, st, now, c.state, c.now)
		}
	}
}

func TestRecheckPRThroughAPeer(t *testing.T) {
	peer := &workerPeer{probe: `{"state":"merged","at":"` + time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339) + `"}`}
	ps := httptest.NewServer(peer)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir()}, nil, nil)
	s.probeTTL = 0
	// The local gh must not be asked: the session is on kube.
	stubProbes(t, nil, nil, nil)
	probeGHPR = func(context.Context, string, int) (*probeResult, error) {
		t.Error("gh ran on the hub")
		return nil, nil
	}
	it := &removal{ID: "kube/wS:p9", Host: "kube"}
	st, now, at := s.recheck(context.Background(), it, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 343})
	if st != itemResolved || !strings.Contains(now, "merged 3 h ago") || at == nil {
		t.Fatalf("peer PR = %s %q %v", st, now, at)
	}
	got := peer.saw("GET /api/probe")
	if len(got) != 1 || got[0] != "GET /api/probe?kind=pr&number=343&repo=o%2Fr" {
		t.Fatalf("peer saw %v", got)
	}
	// A pid on the peer is the peer's to look at.
	peer.mu.Lock()
	peer.probe = `{"state":"dead"}`
	peer.mu.Unlock()
	if st, _, _ := s.recheck(context.Background(), it, ledger.AskItem{Type: "resource", Kind: "pid", Name: "77"}); st != itemResolved {
		t.Errorf("peer pid = %s", st)
	}
	// A peer that cannot answer leaves the item unchecked, not resolved.
	peer.mu.Lock()
	peer.probe = ""
	peer.mu.Unlock()
	if st, now, _ := s.recheck(context.Background(), it, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 344}); st != itemUnchecked || !strings.Contains(now, "can't check") {
		t.Errorf("peer down = %s %q", st, now)
	}
}

func TestProbeValidatesAndIsUserOnly(t *testing.T) {
	s, _, _ := coordServer(t)
	stubProbes(t, nil, map[int]bool{4242: true}, nil)
	if w := do(s, "GET", "/api/probe?kind=pid&pid=4242", userHdr, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"alive"`) {
		t.Fatalf("pid = %d %s", w.Code, w.Body)
	}
	for _, q := range []string{"kind=pr&repo=o/r;x&number=1", "kind=pr&repo=o/r&number=0", "kind=pid&pid=1", "kind=pid&pid=x", "kind=k3d&name=-bad", "kind=sh"} {
		if w := do(s, "GET", "/api/probe?"+q, userHdr, ""); w.Code != 400 {
			t.Errorf("%s = %d", q, w.Code)
		}
	}
	if w := do(s, "GET", "/api/probe?kind=pid&pid=4242", agentHdr("w1:p1"), ""); w.Code < 400 {
		t.Errorf("agent probed: %d", w.Code)
	}
}

func TestReviewTurnsAResolvedUnsafeIntoANote(t *testing.T) {
	s, _, _ := coordServer(t)
	s.probeTTL = 0
	stubProbes(t, map[int]*probeResult{343: {State: "merged", At: agoT(5 * time.Hour)}, 344: {State: "open"}}, nil, nil)
	td, _, _ := s.todos.add(Todo{Question: "q", Kind: "decision", State: TodoOpen, CreatedBy: "w"})
	if _, err := s.todos.close(td.ID, TodoDone, "me", ""); err != nil {
		t.Fatal(err)
	}

	// Every item has resolved: a note, and the session counts as safe.
	markUnsafe(t, s, 6*time.Hour,
		ledger.AskItem{Type: "pr", Repo: "o/r", Number: 343}, ledger.AskItem{Type: "todo", ID: td.ID})
	it := reviewOf(t, s, "desk/w2:p1")
	notes := strings.Join(it.Notes, "|")
	if !it.Safe || len(it.Warnings) != 0 ||
		!strings.Contains(notes, "the session said UNSAFE at ") || !strings.Contains(notes, "(6 h ago), but PR o/r#343 merged 5 h ago and to-do "+td.ID+" is done") {
		t.Fatalf("resolved: %+v", it)
	}
	if ar := it.AskReview; ar == nil || !ar.Resolved || !ar.Event || len(ar.Items) != 2 || ar.Items[0].State != itemResolved {
		t.Fatalf("ask_review = %+v", ar)
	}

	// One PR still open and one note nobody can check: each its own warning.
	markUnsafe(t, s, 6*time.Hour,
		ledger.AskItem{Type: "pr", Repo: "o/r", Number: 343}, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 344, Text: "watching it"},
		ledger.AskItem{Type: "other", Text: "a loop I run"})
	it = reviewOf(t, s, "desk/w2:p1")
	if it.Safe || len(it.Warnings) != 2 ||
		!strings.Contains(it.Warnings[0], "the session says UNSAFE: PR o/r#344 is still open (watching it) (answered 6 h ago)") ||
		!strings.Contains(it.Warnings[1], "the session says UNSAFE: a loop I run (answered 6 h ago)") ||
		!strings.Contains(strings.Join(it.Notes, "|"), "about PR o/r#343, but it merged 5 h ago") {
		t.Fatalf("unresolved: %+v", it)
	}

	// An answer recorded before items existed is one "other" item: it stays a warning.
	m := markUnsafe(t, s, time.Hour)
	if _, err := s.ledger.SetAsk(m.AgentID, m.At, ledger.Ask{State: ledger.AskUnsafe, Reason: "old style reason", At: m.Ask.At}); err != nil {
		t.Fatal(err)
	}
	it = reviewOf(t, s, "desk/w2:p1")
	if it.Safe || len(it.Warnings) != 1 || !strings.Contains(it.Warnings[0], "the session says UNSAFE: old style reason (answered 1 h ago)") {
		t.Fatalf("legacy: %+v", it)
	}
}

func TestLedgerShowsTheRecheckedAnswers(t *testing.T) {
	s, _, _ := coordServer(t)
	s.probeTTL = 0
	stubProbes(t, map[int]*probeResult{343: {State: "merged", At: agoT(time.Hour)}}, nil, nil)
	markUnsafe(t, s, 2*time.Hour, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 343})
	w := do(s, "GET", "/api/ledger", userHdr, "")
	var l struct {
		Asks map[string]*askReview `json:"asks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	ar := l.Asks["desk/w2:p1"]
	if ar == nil || !ar.Resolved || len(ar.Items) != 1 || ar.Items[0].State != itemResolved || !strings.Contains(ar.Items[0].Now, "merged 1 h ago") || ar.AnsweredAt.IsZero() {
		t.Fatalf("asks = %s", w.Body)
	}
}

func TestProbeCache(t *testing.T) {
	s, _, _ := coordServer(t)
	s.probeTTL = time.Hour
	n := 0
	stubProbes(t, nil, nil, nil)
	probeGHPR = func(ctx context.Context, repo string, num int) (*probeResult, error) {
		n++
		return &probeResult{State: "open"}, nil
	}
	it := &removal{ID: "desk/w2:p1", Host: "desk"}
	for i := 0; i < 3; i++ {
		s.recheck(context.Background(), it, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 1})
	}
	if n != 1 {
		t.Fatalf("gh ran %d times", n)
	}
	s.recheck(context.Background(), it, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 2})
	if n != 2 {
		t.Fatalf("another PR shares the cache: %d", n)
	}
}

// ---- asking again ----

func lastPrompt(t *testing.T, fh *fakeLaunchHerdr) string {
	t.Helper()
	fh.mu.Lock()
	defer fh.mu.Unlock()
	var text string
	for _, c := range fh.calls {
		if c.Method == "agent.prompt" {
			text, _ = c.Params["text"].(string)
		}
	}
	return text
}

func TestTheReasonReachesTheSession(t *testing.T) {
	s, fh := askServer(t, "SAFE")
	if w := do(s, "POST", "/api/marks", agentHdr("w1:p1"), `{"agent_id":"desk/w2:p1","reason":"both PRs merged at 14:02 (jhjaggars/homelab#343, #344)"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	answered(t, s, "desk/w2:p1")
	p := lastPrompt(t, fh)
	if !strings.Contains(p, "```\nboth PRs merged at 14:02 (jhjaggars/homelab#343, #344)\n```") || !strings.Contains(p, "gh pr view") {
		t.Fatalf("prompt = %s", p)
	}
}

func TestAnUnsafeReplyIsRecordedWithItsItems(t *testing.T) {
	s, _ := askServer(t, "UNSAFE\n- pr o/r#343 still open\n- todo t0123456789 for the user")
	if w := do(s, "POST", "/api/marks", userHdr, `{"agent_id":"desk/w2:p1","reason":"done"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	a := answered(t, s, "desk/w2:p1")
	if a.State != ledger.AskUnsafe || len(a.Items) != 2 || a.Items[0].Repo != "o/r" || a.Items[0].Number != 343 || a.Items[1].ID != "t0123456789" {
		t.Fatalf("ask = %+v", a)
	}
}

func TestReaskKeepsTheStandingAnswerUntilANewOne(t *testing.T) {
	s, fh := askServer(t, "SAFE")
	s.probeTTL = 0
	m := markUnsafe(t, s, 6*time.Hour, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 344})
	stubProbes(t, map[int]*probeResult{344: {State: "open"}}, nil, nil)
	oldAt := m.Ask.At

	if w := do(s, "POST", "/api/marks/reask", userHdr, `{"agent_id":"desk/w2:p1","item":1}`); w.Code != 200 {
		t.Fatalf("reask = %d %s", w.Code, w.Body)
	}
	// The focus of the item the person clicked is in the prompt.
	waitFor(t, func() bool { return promptCount(fh) > 0 })
	if p := lastPrompt(t, fh); !strings.Contains(p, "Look again at this in particular: PR o/r#344") {
		t.Fatalf("prompt = %s", p)
	}
	waitFor(t, func() bool {
		a := markOf(t, s, "desk/w2:p1").Ask
		return a.State == ledger.AskSafe && a.ReaskedAt == nil
	})
	if a := markOf(t, s, "desk/w2:p1").Ask; !a.At.After(oldAt) || len(a.Items) != 0 || a.Reasks != 0 {
		t.Fatalf("after = %+v", a)
	}

	// No answer to a re-ask: the old answer stays, with a note.
	s2, _ := askServer(t, "")
	markUnsafe(t, s2, 6*time.Hour, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 344})
	if w := do(s2, "POST", "/api/marks/reask", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := do(s2, "POST", "/api/marks/reask", userHdr, `{"agent_id":"desk/w2:p1"}`); w.Code != 409 || !strings.Contains(w.Body.String(), `"asking"`) {
		t.Fatalf("second reask = %d %s", w.Code, w.Body)
	}
	waitFor(t, func() bool { a := markOf(t, s2, "desk/w2:p1").Ask; return a.ReaskedAt == nil && a.ReaskNote != "" })
	if a := markOf(t, s2, "desk/w2:p1").Ask; a.State != ledger.AskUnsafe || len(a.Items) != 1 || !strings.Contains(a.ReaskNote, "no answer") {
		t.Fatalf("no answer: %+v", a)
	}
	// Unknown and unmarked sessions are refused.
	if w := do(s2, "POST", "/api/marks/reask", userHdr, `{"agent_id":"desk/nobody"}`); w.Code != 404 {
		t.Errorf("unmarked = %d", w.Code)
	}
}

func TestAutoReaskTriggersAndRateLimit(t *testing.T) {
	s, fh := askServer(t, "UNSAFE\n- pr o/r#344 still open")
	s.probeTTL = 0
	ctx := context.Background()
	now := time.Now()
	pending := ledger.AskItem{Type: "pr", Repo: "o/r", Number: 344}

	// Nothing has moved and the answer is young: no re-ask.
	stubProbes(t, map[int]*probeResult{344: {State: "open"}}, nil, nil)
	markUnsafe(t, s, 30*time.Minute, pending)
	if got := s.reaskStep(ctx, now); len(got) != 0 {
		t.Fatalf("young answer re-asked: %v", got)
	}

	// Older than the cap and still unresolved: re-asked, and counted.
	markUnsafe(t, s, 13*time.Hour, pending)
	if got := s.reaskStep(ctx, now); len(got) != 1 || got[0] != "desk/w2:p1" {
		t.Fatalf("stale answer not re-asked: %v", got)
	}
	waitFor(t, func() bool {
		a := markOf(t, s, "desk/w2:p1").Ask
		return a.ReaskedAt == nil && a.Reasks == 1 && a.At.After(now.Add(-time.Minute))
	})
	if promptCount(fh) != 1 {
		t.Fatalf("prompts = %d", promptCount(fh))
	}
	// The new answer is fresh, so there is no loop; and had it been stale,
	// the gap since the last ask holds the next one back.
	if got := s.reaskStep(ctx, now); len(got) != 0 {
		t.Fatalf("re-asked a fresh answer: %v", got)
	}
	markUnsafe(t, s, 13*time.Hour, pending)
	s.ledger.UpdateAsk("desk/w2:p1", markOf(t, s, "desk/w2:p1").At, func(a *ledger.Ask) bool {
		t := now.Add(-5 * time.Minute)
		a.AskedAt = &t // asked 5 minutes ago
		return true
	})
	if got := s.reaskStep(ctx, now); len(got) != 0 {
		t.Fatalf("rate limit broken: %v", got)
	}
	// Past the gap it asks again, until reaskMax.
	for i := 1; i <= s.reaskMax; i++ {
		markUnsafe(t, s, 13*time.Hour, pending)
		s.ledger.UpdateAsk("desk/w2:p1", markOf(t, s, "desk/w2:p1").At, func(a *ledger.Ask) bool { a.Reasks = i - 1; return true })
		if got := s.reaskStep(ctx, now); len(got) != 1 {
			t.Fatalf("ask %d: %v", i, got)
		}
		waitFor(t, func() bool { return markOf(t, s, "desk/w2:p1").Ask.ReaskedAt == nil })
	}
	markUnsafe(t, s, 13*time.Hour, pending)
	s.ledger.UpdateAsk("desk/w2:p1", markOf(t, s, "desk/w2:p1").At, func(a *ledger.Ask) bool { a.Reasks = s.reaskMax; return true })
	if got := s.reaskStep(ctx, now); len(got) != 0 {
		t.Fatalf("past the max: %v", got)
	}
}

func TestAutoReaskOnAnEvent(t *testing.T) {
	s, _ := askServer(t, "SAFE")
	s.probeTTL = 0
	ctx := context.Background()
	// The PR merged 1 h ago, after the 3 h old answer: that is an event,
	// even though every item has resolved and the answer is young.
	stubProbes(t, map[int]*probeResult{343: {State: "merged", At: agoT(time.Hour)}}, nil, nil)
	markUnsafe(t, s, 3*time.Hour, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 343})
	if got := s.reaskStep(ctx, time.Now()); len(got) != 1 {
		t.Fatalf("event did not re-ask: %v", got)
	}
	waitFor(t, func() bool {
		a := markOf(t, s, "desk/w2:p1").Ask
		return a.State == ledger.AskSafe && a.ReaskedAt == nil
	})

	// A PR that merged before the answer was given is no event.
	stubProbes(t, map[int]*probeResult{343: {State: "merged", At: agoT(5 * time.Hour)}}, nil, nil)
	markUnsafe(t, s, 3*time.Hour, ledger.AskItem{Type: "pr", Repo: "o/r", Number: 343})
	if got := s.reaskStep(ctx, time.Now()); len(got) != 0 {
		t.Fatalf("an old merge re-asked: %v", got)
	}
	// Neither is a SAFE answer re-asked, nor a working session.
	markUnsafe(t, s, 20*time.Hour, ledger.AskItem{Type: "other", Text: "x"})
	s.ledger.UpdateAsk("desk/w2:p1", markOf(t, s, "desk/w2:p1").At, func(a *ledger.Ask) bool { a.State = ledger.AskSafe; return true })
	if got := s.reaskStep(ctx, time.Now()); len(got) != 0 {
		t.Fatalf("a SAFE answer re-asked: %v", got)
	}
}
