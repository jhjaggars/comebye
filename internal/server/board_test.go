package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

func getBoard(t *testing.T, s *Server, hdr map[string]string) BoardView {
	t.Helper()
	w := do(s, "GET", "/api/board", hdr, "")
	if w.Code != 200 {
		t.Fatalf("board = %d %s", w.Code, w.Body)
	}
	var v BoardView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return v
}

func laneOf(t *testing.T, v BoardView, key string) BoardLane {
	t.Helper()
	for _, l := range v.Lanes {
		if l.Key == key {
			return l
		}
	}
	t.Fatalf("no lane %s", key)
	return BoardLane{}
}

func laneIDs(l BoardLane) string {
	var ids []string
	for _, it := range l.Items {
		ids = append(ids, it.ID)
	}
	return strings.Join(ids, ",")
}

func TestBoardLanesFromRecords(t *testing.T) {
	s, _, _ := todoServer(t)
	now := time.Now()
	old := now.Add(-5 * 24 * time.Hour)
	add := func(e ledger.Entry) string {
		t.Helper()
		e.Host, e.Repo, e.RepoName, e.Branch = "desk", "/r/comebye", "comebye", "feat/"+e.AgentName
		got, err := s.ledger.Add(e)
		if err != nil {
			t.Fatal(err)
		}
		return got.ID
	}
	fin := now.Add(-time.Hour)
	run := add(ledger.Entry{AgentName: "w-run", Status: ledger.Running, Brief: "build the thing"})
	wait := add(ledger.Entry{AgentName: "w-wait", Status: ledger.Waiting, After: []string{run}, Brief: "later"})
	rtl := add(ledger.Entry{AgentName: "w-ready", Status: ledger.Finished, FinishedAt: &fin, Brief: "done, not landed"})
	research := add(ledger.Entry{AgentName: "w-res", Status: ledger.Finished, Purpose: "research", FinishedAt: &fin})
	lnd := add(ledger.Entry{AgentName: "w-landing", Status: ledger.Landing, Approval: &ledger.Approval{At: fin}})
	ld := add(ledger.Entry{AgentName: "w-landed", Status: ledger.Landed, FinishedAt: &fin})
	ldOld := add(ledger.Entry{AgentName: "w-landed-old", Status: ledger.Landed, FinishedAt: &old})
	rm := add(ledger.Entry{AgentName: "w-rtr-landed", Status: ledger.ReadyToRemove, MarkedFrom: ledger.Landed, FinishedAt: &fin})
	rm2 := add(ledger.Entry{AgentName: "w-rtr-never", Status: ledger.ReadyToRemove, MarkedFrom: ledger.Finished, FinishedAt: &fin})
	_ = []string{ldOld, rm2}

	q, _, err := s.todos.add(Todo{Question: "ship it?", Kind: "decision", CreatedBy: "agent", Waiting: &TodoAgent{ID: "desk/w2:p1"}})
	if err != nil {
		t.Fatal(err)
	}
	done, _, _ := s.todos.add(Todo{Question: "old", Kind: "decision", CreatedBy: "agent"})
	s.todos.mu.Lock()
	for i := range s.todos.data.Items {
		if s.todos.data.Items[i].ID == done.ID {
			s.todos.data.Items[i].State = TodoDone
		}
	}
	s.todos.mu.Unlock()
	rel, _, _ := s.todos.add(Todo{Question: "release v1.2.3", Kind: "release", CreatedBy: "agent", Release: &TodoRelease{Version: "v1.2.3", Commit: "abc1234", State: relTagged}})
	dis, _, _ := s.todos.add(Todo{Question: "release v1.0.0", Kind: "release", CreatedBy: "agent", Release: &TodoRelease{Version: "v1.0.0", Commit: "abc1234", State: relTagged, Dismissed: true}})

	v := getBoard(t, s, userHdr)
	if len(v.Lanes) != 6 {
		t.Fatalf("lanes = %d", len(v.Lanes))
	}
	check := func(key string, count int, ids ...string) {
		t.Helper()
		l := laneOf(t, v, key)
		if l.Count != count || l.Rule == "" {
			t.Errorf("%s: count %d rule %q, want %d", key, l.Count, l.Rule, count)
		}
		for _, id := range ids {
			if !strings.Contains(laneIDs(l), id) {
				t.Errorf("%s lacks %s: %s", key, id, laneIDs(l))
			}
		}
	}
	check(laneNeeds, 1, q.ID)
	check(laneRunning, 1, run)
	check(laneWaiting, 2, wait, rel.ID)
	check(laneLanding, 2, rtl, lnd)
	check(laneReports, 1, research)
	check(laneLanded, 2, ld, rm)
	for _, l := range v.Lanes {
		if strings.Contains(laneIDs(l), dis.ID) || strings.Contains(laneIDs(l), done.ID) {
			t.Errorf("dismissed or done item shown in %s", l.Key)
		}
	}
	if v.Loose.Reports != 0 || v.Loose.ReadyToRemove != 1 {
		t.Errorf("loose = %+v", v.Loose)
	}
	if it := laneOf(t, v, laneNeeds).Items[0]; it.Project != "Not tied to a project" || it.Kind != "decision" {
		t.Errorf("needs item = %+v", it)
	}
	if it := laneOf(t, v, laneWaiting).Items; !strings.Contains(it[0].Detail+it[1].Detail, "queued behind "+run+"") {
		t.Errorf("waiting detail = %+v", it)
	}
	if v.Headline != "" || v.BriefAt != nil {
		t.Errorf("no briefing yet, got %q", v.Headline)
	}
}

func TestBoardNotesAndHeadlineFromTheBriefing(t *testing.T) {
	s, _, _ := todoServer(t)
	// Waiting behind a step that has not reported. With no step to wait for, the board's own ledger pass would release the
	// entry (releaseQueued starts it in a goroutine) and it could be a running handoff by the time the board is read.
	step, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "comebye", Branch: "feat/step", AgentName: "step", Status: ledger.Running, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "comebye", Branch: "feat/x", AgentName: "oc-pr-4025", Status: ledger.Waiting, After: []string{step.ID}, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	q, _, _ := s.todos.add(Todo{Question: "may I?", Kind: "decision", CreatedBy: "agent"})
	md := "**Tue 20:04. v0.5.20 tagged. Next: you press Bump hub.**\n\n## Needs you\n- " + q.ID + ": the rollout is blocked on this.\n\n## Notes\n- **" + e.ID + "**: waits for the RFC, on purpose.\n- `oc-pr-4025` — same, by name\n- tdeadbeef00: an id that is gone\n- plain bullet: not a note\n"
	if err := s.brief.set(Brief{Markdown: md, By: "coord", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	v := getBoard(t, s, agentHdr("w1:p1")) // the coordinator reads it too
	if v.Headline != "Tue 20:04. v0.5.20 tagged. Next: you press Bump hub." || v.BriefBy != "coord" {
		t.Errorf("headline = %q by %q", v.Headline, v.BriefBy)
	}
	n := laneOf(t, v, laneNeeds).Items[0]
	if n.ID != q.ID || n.Why != "the rollout is blocked on this." {
		t.Errorf("why = %+v", n)
	}
	w := laneOf(t, v, laneWaiting).Items[0]
	if w.ID != e.ID || w.Note != "waits for the RFC, on purpose." {
		t.Errorf("note = %+v", w)
	}
	if len(v.UnkeyedNotes) != 1 || !strings.HasPrefix(v.UnkeyedNotes[0], "tdeadbeef00") {
		t.Errorf("unkeyed = %v", v.UnkeyedNotes)
	}
}

func TestBoardNoteByName(t *testing.T) {
	s, _, _ := todoServer(t)
	s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "comebye", Branch: "b", AgentName: "oc-pr-4025", Status: ledger.Running, Brief: "b"})
	s.brief.set(Brief{Markdown: "head\n\n- oc-pr-4025: parked on the RFC\n", By: "c", At: time.Now()})
	v := getBoard(t, s, userHdr)
	if it := laneOf(t, v, laneRunning).Items[0]; it.Note != "parked on the RFC" {
		t.Errorf("note by name = %+v", it)
	}
}

func TestBoardIsReadOnlyAndAuthed(t *testing.T) {
	s, _, _ := todoServer(t)
	if w := do(s, "GET", "/api/board", nil, ""); w.Code == 200 {
		t.Fatalf("anonymous board = %d", w.Code)
	}
	if w := do(s, "POST", "/api/board", userHdr, "{}"); w.Code != 405 && w.Code != 404 {
		t.Fatalf("POST board = %d", w.Code)
	}
}

func TestBoardDialogBlockedIsNeedsYou(t *testing.T) {
	fh, client := startFakeLaunchHerdr(t)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{
			{Name: "coordinator", PaneID: "w1:p1", Agent: "claude", Status: herdr.StatusIdle},
			{Name: "home", PaneID: "w2:p1", Agent: "pi", Status: herdr.StatusBlocked},
		}
	})
	m := managerWith(t, client, "desk", 2)
	s := newServer(t, Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), AgentToken: testToken}, client, m)
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "comebye", Branch: "b", AgentName: "home", AgentID: "desk/w2:p1", PaneID: "w2:p1", Status: ledger.Running, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v := getBoard(t, s, userHdr)
		if n := laneOf(t, v, laneNeeds); n.Count == 1 && n.Items[0].ID == e.ID && n.Items[0].Type == "dialog" {
			if laneOf(t, v, laneRunning).Count != 0 {
				t.Fatal("also in running")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not in Needs you: %+v", v.Lanes)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A lane longer than the cap keeps the newest items, whatever order the
// ledger holds them in, and its count still says how many there are.
func TestBoardCapKeepsNewest(t *testing.T) {
	s, _, _ := todoServer(t)
	now := time.Now()
	n := boardMaxItems + 9
	var newest, oldest string
	for i := 0; i < n; i++ { // added oldest first, so the newest come last in the ledger
		fin := now.Add(-time.Duration(n-i) * time.Hour)
		got, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: "/r/c", RepoName: "comebye", Branch: fmt.Sprintf("feat/b%d", i), AgentName: fmt.Sprintf("w%d", i), Status: ledger.Finished, FinishedAt: &fin})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = got.ID
		}
		newest = got.ID
	}
	l := laneOf(t, getBoard(t, s, userHdr), laneLanding)
	if l.Count != n || len(l.Items) != boardMaxItems {
		t.Fatalf("count %d items %d, want %d and %d", l.Count, len(l.Items), n, boardMaxItems)
	}
	if l.Items[0].ID != newest || strings.Contains(laneIDs(l), oldest) {
		t.Errorf("newest first %v, oldest shown %v", l.Items[0].ID == newest, strings.Contains(laneIDs(l), oldest))
	}
}

// A finished research handoff is a card in Reports to read, with its report
// flag, purpose and the Pages published for it; a marked one is not.
func TestBoardReportsLane(t *testing.T) {
	s, _, _ := todoServer(t)
	fin := time.Now().Add(-time.Hour)
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: "/r/c", RepoName: "comebye", Branch: "r/etcd", AgentName: "etcd-op", Status: ledger.Finished, Purpose: "research", FinishedAt: &fin, FinalReport: "# findings"})
	if err != nil {
		t.Fatal(err)
	}
	s.pages.mu.Lock()
	s.pages.data.Pages = append(s.pages.data.Pages, Page{ID: "pg_etcd", Title: "etcd lifecycle", Handoff: e.ID})
	s.pages.mu.Unlock()
	l := laneOf(t, getBoard(t, s, userHdr), laneReports)
	if l.Count != 1 || len(l.Items) != 1 {
		t.Fatalf("reports lane = %+v", l)
	}
	it := l.Items[0]
	if it.ID != e.ID || !it.Report || it.Kind != "research" || len(it.Pages) != 1 || it.Pages[0].ID != "pg_etcd" {
		t.Errorf("item = %+v", it)
	}
}
