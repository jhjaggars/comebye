package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// deliveredSetup is a hub with a worker w9:p1 (desk/w9:p1) whose worktree
// holds an unpushed docs commit and an uncommitted notes file, and a
// coordinator pane w1:p1. The worker has published a page.
func deliveredSetup(t *testing.T) (s *Server, repo, wt, page string) {
	s, _, repo, wt, page = deliveredSetupFH(t)
	return
}

func deliveredSetupFH(t *testing.T) (s *Server, fh *fakeLaunchHerdr, repo, wt, page string) {
	t.Helper()
	s0, fh, repo, wt := removalSetup(t)
	os.MkdirAll(wt+"/docs", 0o755)
	os.WriteFile(wt+"/docs/findings.md", []byte("findings\n"), 0o644)
	git(t, wt, "add", "docs/findings.md")
	git(t, wt, "commit", "-q", "-m", "docs: findings")
	os.WriteFile(wt+"/scratch.md", []byte("scratch\n"), 0o644)
	os.WriteFile(wt+"/more-notes.md", []byte("more\n"), 0o644)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "coordinator", PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: herdr.StatusIdle, CWD: repo})
	})
	m := managerWith(t, s0.client, "desk", 2)
	s = New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), AgentToken: testToken,
		LaunchRoots: []string{filepath.Dir(repo)}, WorktreeRoot: filepath.Dir(filepath.Dir(wt))}, s0.client, m)
	w := do(s, "POST", "/api/pages", agentHdr("w9:p1"), pubJSON(map[string]any{"title": "Findings", "body": "# findings\ntext"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", w.Code, w.Body)
	}
	return s, fh, repo, wt, pageOf(t, w.Body.Bytes()).ID
}

func deliver(s *Server, hdr map[string]string, agent, notCaptured string, pages ...string) *struct {
	Code int
	Body string
} {
	b, _ := json.Marshal(map[string]any{"agent_id": agent, "pages": pages, "not_captured": strings.Split(notCaptured, "|")})
	if notCaptured == "" {
		b, _ = json.Marshal(map[string]any{"agent_id": agent, "pages": pages})
	}
	w := do(s, "POST", "/api/deliveries", hdr, string(b))
	return &struct {
		Code int
		Body string
	}{w.Code, w.Body.String()}
}

func mustMark(t *testing.T, s *Server, body string) {
	t.Helper()
	if w := do(s, "POST", "/api/marks", userHdr, body); w.Code != 200 {
		t.Fatalf("mark = %d %s", w.Code, w.Body)
	}
}

func TestDeliveredDirtyAndUnpushedAreNotesAndSafe(t *testing.T) {
	s, _, _, page := deliveredSetup(t)
	// Before the delivery the same mark is full of warnings.
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"report published","discard":true,"no_ask":true}`)
	it := reviewOf(t, s, "desk/w9:p1")
	if it.Safe || len(it.Warnings) != 2 || it.DeliveredDiscard || !it.Discardable {
		t.Fatalf("before: safe=%v warnings=%v", it.Safe, it.Warnings)
	}
	// The agent records its page; the review now lists it as captured.
	if r := deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page); r.Code != 200 {
		t.Fatalf("deliver = %d %s", r.Code, r.Body)
	}
	it = reviewOf(t, s, "desk/w9:p1")
	if !it.Safe || len(it.Warnings) != 0 || !it.DeliveredDiscard || !it.Discardable || it.Delivered == nil || !it.Delivered.Holds {
		t.Fatalf("after: safe=%v warnings=%v delivered=%+v", it.Safe, it.Warnings, it.Delivered)
	}
	d := it.Delivered
	if d.Files != 2 || d.Commits != 1 || len(d.FileList) != 2 || len(d.CommitList) != 1 || !strings.Contains(d.CommitList[0], "docs: findings") ||
		len(d.Pages) != 1 || d.Pages[0].ID != page || d.Pages[0].Title != "Findings" {
		t.Fatalf("delivered = %+v", d)
	}
	var note string
	for _, n := range it.Notes {
		if strings.HasPrefix(n, "captured in") {
			note = n
		}
	}
	for _, want := range []string{"\u201cFindings\u201d", "discarding drops 2 uncommitted files", "scratch.md", "1 unmerged commit", "docs: findings"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
}

func TestDeliveredNeedsADiscardMark(t *testing.T) {
	s, _, _, page := deliveredSetup(t)
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","no_ask":true}`)
	if it := reviewOf(t, s, "desk/w9:p1"); it.Safe || it.DeliveredDiscard || it.Delivered != nil {
		t.Fatalf("a plain mark must not count the pages: safe=%v %+v", it.Safe, it.Delivered)
	}
}

func TestDeliveredUnsafeAfterIsAWarning(t *testing.T) {
	s, _, _, page := deliveredSetup(t)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	m := markOf(t, s, "desk/w9:p1")
	// The session answers UNSAFE after its pages were recorded.
	if ok, err := s.ledger.SetAsk("desk/w9:p1", m.At, ledger.Ask{State: ledger.AskUnsafe, Reason: "there is code in src/", At: time.Now().Add(time.Minute)}); err != nil || !ok {
		t.Fatalf("SetAsk = %v %v", ok, err)
	}
	it := reviewOf(t, s, "desk/w9:p1")
	if it.Safe || it.DeliveredDiscard || it.Delivered == nil || it.Delivered.Holds {
		t.Fatalf("safe=%v delivered=%+v", it.Safe, it.Delivered)
	}
	all := strings.Join(it.Warnings, "\n")
	for _, want := range []string{"the session says UNSAFE: there is code in src/", "uncommitted changes (2 files)", "1 commits not in"} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings lack %q:\n%s", want, all)
		}
	}
}

func TestDeliveredUnsafeBeforeIsANote(t *testing.T) {
	s, _, _, page := deliveredSetup(t)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	m := markOf(t, s, "desk/w9:p1")
	s.ledger.SetAsk("desk/w9:p1", m.At, ledger.Ask{State: ledger.AskUnsafe, Reason: "notes not saved yet", At: time.Now().Add(-time.Minute)})
	if it := reviewOf(t, s, "desk/w9:p1"); it.Safe {
		t.Fatal("UNSAFE with no pages must stay a warning")
	}
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	it := reviewOf(t, s, "desk/w9:p1")
	if !it.Safe || !it.DeliveredDiscard || !strings.Contains(strings.Join(it.Notes, "\n"), "the session said UNSAFE before its pages were recorded: notes not saved yet") {
		t.Fatalf("safe=%v warnings=%v notes=%v", it.Safe, it.Warnings, it.Notes)
	}
}

func TestDeliveredStillAskingIsNotSafe(t *testing.T) {
	s, _, _, page := deliveredSetup(t)
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	m := markOf(t, s, "desk/w9:p1")
	s.ledger.SetAsk("desk/w9:p1", m.At, ledger.Ask{State: ledger.AskAsking, At: time.Now()})
	if it := reviewOf(t, s, "desk/w9:p1"); it.Safe || it.DeliveredDiscard {
		t.Fatalf("an unanswered session must not make it safe: %v", it.Warnings)
	}
}

func TestDeliveredFlaggedCodeIsAWarning(t *testing.T) {
	s, _, _, page := deliveredSetup(t)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	if r := deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "src/parser.go has a fix the page only describes", page); r.Code != 200 {
		t.Fatalf("deliver = %d %s", r.Code, r.Body)
	}
	it := reviewOf(t, s, "desk/w9:p1")
	if it.Safe || it.DeliveredDiscard || it.Delivered == nil || it.Delivered.Holds {
		t.Fatalf("safe=%v delivered=%+v", it.Safe, it.Delivered)
	}
	all := strings.Join(it.Warnings, "\n")
	if !strings.Contains(all, "not captured in a page: src/parser.go has a fix") || !strings.Contains(all, "uncommitted changes") {
		t.Fatalf("warnings = %s", all)
	}
}

func TestDeliveredInProgressGitOpStaysAWarning(t *testing.T) {
	s, repo, wt, page := deliveredSetup(t)
	os.Remove(wt + "/scratch.md")
	os.Remove(wt + "/more-notes.md")
	os.WriteFile(repo+"/docs.txt", []byte("main\n"), 0o644)
	os.WriteFile(wt+"/docs.txt", []byte("branch\n"), 0o644)
	git(t, wt, "add", "docs.txt")
	git(t, wt, "commit", "-q", "-m", "conflicting")
	git(t, repo, "add", "docs.txt")
	git(t, repo, "commit", "-q", "-m", "main moves")
	exec.Command("git", "-C", wt, "rebase", "main").Run() // stops on the conflict
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	it := reviewOf(t, s, "desk/w9:p1")
	if it.Safe || it.DeliveredDiscard || !strings.Contains(strings.Join(it.Warnings, "\n"), "rebase is in progress") {
		t.Fatalf("safe=%v warnings=%v", it.Safe, it.Warnings)
	}
}

func TestDeliveredWorkingAgentIsAWarning(t *testing.T) {
	s, fh, _, _, page := deliveredSetupFH(t)
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	fh.set(func(f *fakeLaunchHerdr) {
		for i := range f.agents {
			if f.agents[i].PaneID == "w9:p1" {
				f.agents[i].Status = herdr.StatusWorking
			}
		}
	})
	s.manager.Refresh()
	waitFor(t, func() bool {
		a := s.liveAgent("desk/w9:p1")
		return a != nil && a.Status == herdr.StatusWorking
	})
	it := reviewOf(t, s, "desk/w9:p1")
	if it.Safe || !strings.Contains(strings.Join(it.Warnings, "\n"), "closing it interrupts it") {
		t.Fatalf("safe=%v warnings=%v", it.Safe, it.Warnings)
	}
}

func TestDeliverValidatesPageOwnership(t *testing.T) {
	s, _, _, mine := deliveredSetup(t)
	// A page by another agent, one by the user, one by a coordinator.
	other := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"title": "Other", "body": "x"})).Body.Bytes()).ID
	if r := deliver(s, userHdr, "desk/w9:p1", "", mine, other); r.Code != 400 || !strings.Contains(r.Body, "page_not_theirs") || !strings.Contains(r.Body, other) {
		t.Fatalf("another agent's page = %d %s", r.Code, r.Body)
	}
	if r := deliver(s, userHdr, "desk/w9:p1", "", "pg_0123456789"); r.Code != 400 || !strings.Contains(r.Body, "page_not_found") {
		t.Fatalf("unknown page = %d %s", r.Code, r.Body)
	}
	if r := deliver(s, userHdr, "desk/nobody", "", mine); r.Code != 404 {
		t.Fatalf("unknown agent = %d %s", r.Code, r.Body)
	}
	if r := deliver(s, userHdr, "desk/w9:p1", ""); r.Code != 400 {
		t.Fatalf("no pages = %d", r.Code)
	}
	if _, ok := s.ledger.DeliveryOf("desk/w9:p1"); ok {
		t.Fatal("a refused delivery was recorded")
	}
	// An agent records only its own; a plain agent cannot vouch for another.
	if r := deliver(s, agentHdr("w2:p1"), "desk/w9:p1", "", mine); r.Code != 403 {
		t.Fatalf("another agent = %d %s", r.Code, r.Body)
	}
	if r := deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", mine); r.Code != 200 {
		t.Fatalf("own = %d %s", r.Code, r.Body)
	}
	// Recording again adds pages and replaces what is not captured.
	own2 := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w9:p1"), pubJSON(map[string]any{"title": "Second", "body": "y"})).Body.Bytes()).ID
	if r := deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "code in src/|data in out/", own2); r.Code != 200 {
		t.Fatal(r.Body)
	}
	d, _ := s.ledger.DeliveryOf("desk/w9:p1")
	if len(d.Pages) != 2 || len(d.NotCaptured) != 2 {
		t.Fatalf("delivery = %+v", d)
	}
	if r := deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", mine); r.Code != 200 {
		t.Fatal(r.Body)
	}
	if d, _ = s.ledger.DeliveryOf("desk/w9:p1"); len(d.Pages) != 2 || len(d.NotCaptured) != 0 {
		t.Fatalf("delivery after clearing = %+v", d)
	}
}

func TestDeliverHandoffPagesByUserOrCoordinator(t *testing.T) {
	s, _, _ := coordServer(t)
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: "/r", RepoName: "r", Branch: "w", AgentID: "desk/w2:p1", PaneID: "w2:p1", AgentName: "home", Status: ledger.Finished})
	if err != nil {
		t.Fatal(err)
	}
	pub := func(hdr map[string]string, handoff string) string {
		w := do(s, "POST", "/api/pages", hdr, pubJSON(map[string]any{"body": "x", "handoff": handoff}))
		if w.Code != http.StatusCreated {
			t.Fatalf("publish = %d %s", w.Code, w.Body)
		}
		return pageOf(t, w.Body.Bytes()).ID
	}
	byUser := pub(userHdr, e.ID)
	byStranger := pub(agentHdr("w1:p1"), e.ID) // not yet a recorded coordinator
	elsewhere := pub(userHdr, "h-other")
	if r := deliver(s, userHdr, "desk/w2:p1", "", byUser); r.Code != 200 {
		t.Fatalf("user's page for the handoff = %d %s", r.Code, r.Body)
	}
	if r := deliver(s, userHdr, "desk/w2:p1", "", byStranger); r.Code != 400 || !strings.Contains(r.Body, "neither the agent nor a coordinator") {
		t.Fatalf("a stranger's page = %d %s", r.Code, r.Body)
	}
	if r := deliver(s, userHdr, "desk/w2:p1", "", elsewhere); r.Code != 400 {
		t.Fatalf("another handoff's page = %d %s", r.Code, r.Body)
	}
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	if r := deliver(s, agentHdr("w1:p1"), "desk/w2:p1", "", byStranger); r.Code != 200 {
		t.Fatalf("a coordinator's page for the handoff, recorded by the coordinator = %d %s", r.Code, r.Body)
	}
}

func TestDeliveryNotKeptForAnotherIncarnation(t *testing.T) {
	s, _, _, page := deliveredSetup(t)
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	// The pane id is reused by another agent: the record is not its own.
	d, _ := s.ledger.DeliveryOf("desk/w9:p1")
	d.Name = "someone-else"
	if _, err := s.ledger.SetDelivery(ledger.Delivery{AgentID: d.AgentID, Name: "someone-else", Pages: d.Pages}); err != nil {
		t.Fatal(err)
	}
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	if it := reviewOf(t, s, "desk/w9:p1"); it.Safe || it.Delivered != nil {
		t.Fatalf("safe=%v delivered=%+v", it.Safe, it.Delivered)
	}
}

func TestDeliveredDiscardConfirmWithTipAndLedgerRecord(t *testing.T) {
	s, repo, wt, page := deliveredSetup(t)
	entry, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: repo, RepoName: "proj", Branch: "w", Base: "main", AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "w", WorkspaceID: "w9", Worktree: wt, Status: ledger.Finished})
	if err != nil {
		t.Fatal(err)
	}
	deliver(s, agentHdr("w9:p1"), "desk/w9:p1", "", page)
	mustMark(t, s, `{"agent_id":"desk/w9:p1","reason":"done","discard":true,"no_ask":true}`)
	it := reviewOf(t, s, "desk/w9:p1")
	if !it.DeliveredDiscard || it.Checks == nil || it.Checks.Tip == "" {
		t.Fatalf("review = %+v", it)
	}
	// Without the tip the worktree is kept: a plain removal never drops work.
	// With a stale tip nothing is removed either.
	w := do(s, "POST", "/api/removals/confirm", userHdr, `{"ids":["desk/w9:p1"],"discard":{"desk/w9:p1":"0123abc"}}`)
	if !strings.Contains(w.Body.String(), "discard_changed") {
		t.Fatalf("stale tip = %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(wt + "/scratch.md"); err != nil {
		t.Fatal("a refused discard removed the notes")
	}
	// An agent cannot confirm.
	if w := do(s, "POST", "/api/removals/confirm", agentHdr("w1:p1"), `{"ids":["desk/w9:p1"],"discard":{"desk/w9:p1":"`+it.Checks.Tip+`"}}`); w.Code != http.StatusForbidden {
		t.Fatalf("agent confirm = %d", w.Code)
	}
	w = do(s, "POST", "/api/removals/confirm", userHdr, `{"ids":["desk/w9:p1"],"discard":{"desk/w9:p1":"`+it.Checks.Tip+`"}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"discarded":true`) {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) || git(t, repo, "branch", "--list", "w") != "" {
		t.Fatal("the worktree or the branch is still there")
	}
	if _, ok := s.ledger.DeliveryOf("desk/w9:p1"); ok {
		t.Fatal("the delivery record outlived the removal")
	}
	e, _ := s.ledger.Get(entry.ID)
	var rec struct {
		Lost lostWork `json:"lost"`
	}
	if e.Status != ledger.Removed || json.Unmarshal(e.Removal, &rec) != nil {
		t.Fatalf("entry = %+v", e)
	}
	l := rec.Lost
	if !l.Captured || l.Files != 2 || l.Commits != 1 || len(l.Pages) != 1 || l.Pages[0].ID != page || l.Branch != "w" || l.Tip != it.Checks.Tip ||
		len(l.FileList) != 2 || !strings.Contains(l.CommitList[0], "docs: findings") {
		t.Fatalf("lost = %+v (%s)", l, e.Removal)
	}
}

func TestLostWorkInTheLedgerRecord(t *testing.T) {
	raw := withLost(json.RawMessage(`{"closed":"pane","discarded":true}`), &lostWork{Captured: true, Pages: []deliveredPage{{ID: "pg_0123456789", Title: "T"}}, Files: 2, FileList: []string{"?? scratch.md"}, Commits: 1, CommitList: []string{"abc docs"}})
	var m struct {
		Closed string   `json:"closed"`
		Lost   lostWork `json:"lost"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Closed != "pane" || !m.Lost.Captured || m.Lost.Files != 2 || m.Lost.Commits != 1 || m.Lost.Pages[0].ID != "pg_0123456789" {
		t.Fatalf("record = %s (%v)", raw, err)
	}
}
