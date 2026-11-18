package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

func eventsServer(t *testing.T) (*Server, *fakeLaunchHerdr, herdr.AgentInfo) {
	t.Helper()
	s, fh, _ := coordServer(t)
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	s.events = openEvents("")
	return s, fh, cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
}

func allEvents(s *Server, kinds ...string) []Event { return s.events.query(time.Time{}, kinds, 0) }

// landedFor makes a finished handoff of the coordinator landed, with ci
// and, optionally, its landing report still to come.
func landedFor(t *testing.T, s *Server, coord herdr.AgentInfo, ci *ledger.CI, awaitReport bool) ledger.Entry {
	t.Helper()
	// The coordinator is not passed to cbFinished: the observer's own
	// deliverCallbacks pass races the report capture, and with the coordinator
	// idle in the fleet it would send the "finished" notice before this test
	// makes the entry landed. Without it the requester is not in the fleet, so
	// nothing is sent until the test's own deliverAll.
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"})
	if _, _, err := s.ledger.Update(e.ID, func(x *ledger.Entry) {
		x.Status = ledger.Landed
		x.AwaitLandReport = awaitReport
		x.LandCheck = &ledger.LandCheck{OK: true, State: ledger.LandLanded, Base: "origin/main", CI: ci, At: time.Now()}
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func deliverAll(t *testing.T, s *Server, coord herdr.AgentInfo, id string) {
	t.Helper()
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { g, _ := s.ledger.Get(id); return g.Callback.State == ledger.CallbackDelivered })
}

// Landed work (row 2: CI green, CI still running) is an event: the
// coordinator's pane gets nothing, and the callback settles as logged.
func TestLandedNoticeIsAnEventNotAMessage(t *testing.T) {
	for name, ci := range map[string]*ledger.CI{
		"CI green":   {State: ledger.CISuccess},
		"CI running": {State: ledger.CIRunning},
		"no CI":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			s, fh, coord := eventsServer(t)
			e := landedFor(t, s, coord, ci, false)
			deliverAll(t, s, coord, e.ID)
			if n := promptCount(fh); n != 0 {
				t.Fatalf("a landed notice reached the coordinator's pane: %d", n)
			}
			if g, _ := s.ledger.Get(e.ID); g.Callback.To != "log" {
				t.Fatalf("callback = %+v", g.Callback)
			}
			evs := allEvents(s, "landed")
			if len(evs) != 1 || !strings.Contains(evs[0].Text, e.ID) || !strings.Contains(evs[0].Text, "landed on origin/main") || strings.Contains(evs[0].Text, "IGNORE") {
				t.Fatalf("events = %+v", evs)
			}
		})
	}
}

// Row 4: landed before the worker filed its landing report is the same event.
func TestLandedBeforeReportIsAnEvent(t *testing.T) {
	s, fh, coord := eventsServer(t)
	e := landedFor(t, s, coord, &ledger.CI{State: ledger.CISuccess}, true)
	deliverAll(t, s, coord, e.ID)
	if n := promptCount(fh); n != 0 {
		t.Fatalf("prompts = %d", n)
	}
	if evs := allEvents(s, "landed"); len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
}

// Landing or CI failing, and a finished report (with or without
// decisions), still reach the coordinator as messages.
func TestJudgementNoticesStayMessages(t *testing.T) {
	t.Run("CI failed", func(t *testing.T) {
		s, fh, coord := eventsServer(t)
		e := landedFor(t, s, coord, &ledger.CI{State: ledger.CIFailure}, false)
		deliverAll(t, s, coord, e.ID)
		if n := promptCount(fh); n != 1 {
			t.Fatalf("prompts = %d", n)
		}
		if text, _ := fh.call("agent.prompt").Params["text"].(string); !strings.Contains(text, "CI FAILED") {
			t.Fatalf("text = %q", text)
		}
		if evs := allEvents(s); len(evs) != 0 {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("landing failed", func(t *testing.T) {
		s, fh, coord := eventsServer(t)
		e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"})
		if _, _, err := s.ledger.Update(e.ID, func(x *ledger.Entry) {
			x.LandCheck = &ledger.LandCheck{State: ledger.LandFailed, Reason: "conflict", At: time.Now().Add(time.Minute)}
		}); err != nil {
			t.Fatal(err)
		}
		deliverAll(t, s, coord, e.ID)
		if n := promptCount(fh); n != 1 {
			t.Fatalf("prompts = %d", n)
		}
		if evs := allEvents(s); len(evs) != 0 {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("finished report", func(t *testing.T) {
		s, fh, coord := eventsServer(t)
		e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"})
		deliverAll(t, s, coord, e.ID)
		if n := promptCount(fh); n != 1 {
			t.Fatalf("a finished report must stay a message: %d prompts", n)
		}
		if text, _ := fh.call("agent.prompt").Params["text"].(string); !strings.Contains(text, "finished") || !strings.Contains(text, "comebye read "+e.ID+" --report") {
			t.Fatalf("text = %q", text)
		}
		if evs := allEvents(s); len(evs) != 0 {
			t.Fatalf("events = %+v", evs)
		}
	})
}

func TestRoutineLineClassification(t *testing.T) {
	base := ledger.Entry{ID: "h1", Branch: "b", RepoName: "r", Host: "desk"}
	landed := base
	landed.Status, landed.LandCheck = ledger.Landed, &ledger.LandCheck{OK: true, Base: "origin/main", CI: &ledger.CI{State: ledger.CISuccess}}
	if k, _, l, ok := routineLine(landed); !ok || k != evLanded || !strings.Contains(l, "CI is green") {
		t.Fatalf("landed: %v %q", ok, l)
	}
	failedCI := landed
	failedCI.LandCheck = &ledger.LandCheck{OK: true, CI: &ledger.CI{State: ledger.CIFailure}}
	if _, _, _, ok := routineLine(failedCI); ok {
		t.Fatal("landed with failed CI is not routine")
	}
	lf := base
	lf.Status, lf.LandCheck = ledger.Finished, &ledger.LandCheck{State: ledger.LandFailed}
	lf.FinalReport = "## Summary\nx"
	if _, _, _, ok := routineLine(lf); ok {
		t.Fatal("a landing failure is not routine")
	}
	fin := base
	fin.Status, fin.FinalReport = ledger.Finished, "## Summary\nx\n## Decisions for the user\nNone.\n"
	if _, _, _, ok := routineLine(fin); ok {
		t.Fatal("a finished report needs the coordinator's judgement: not routine")
	}
}

// Rows 13 and 14: a parked or removed agent is an event, not a line queued
// for a digest. (Row 14's removal is covered in autoremove_test.go, the
// cleanup in autoclean_test.go.)
func TestParkedAgentIsAnEvent(t *testing.T) {
	s, fh, _ := eventsServer(t)
	s.queueLine(evPark, "pk1", "pk1", "home was parked automatically (idle for a day, clean worktree); resume it from Recently closed.")
	evs := allEvents(s, "park")
	if len(evs) != 1 || evs[0].Subject != "pk1" || !strings.Contains(evs[0].Text, "home was parked") {
		t.Fatalf("events = %+v", evs)
	}
	if n := promptCount(fh); n != 0 {
		t.Fatalf("prompts = %d", n)
	}
	// Once-only by key.
	s.queueLine(evPark, "pk1", "pk1", "again")
	if n := len(allEvents(s, "park")); n != 1 {
		t.Fatalf("events = %d", n)
	}
}

// Row 12: a prompt held for typing is logged once, not sent to its sender.
func TestHeldPromptIsAnEvent(t *testing.T) {
	s, fh, _ := eventsServer(t)
	s.pq = newPromptQueue("")
	it := QueuedPrompt{ID: "q1", Name: "home", FromID: "desk/w1:p1"}
	s.pq.data.Items = []QueuedPrompt{it}
	s.sendHeldNotice(nil, it, time.Now())
	evs := allEvents(s, "held")
	if len(evs) != 1 || evs[0].Subject != "q1" || !strings.Contains(evs[0].Text, "queued prompt q1 to home is still held") {
		t.Fatalf("events = %+v", evs)
	}
	if !s.pq.data.Items[0].HeldNoticed {
		t.Fatal("the held notice is not marked")
	}
	if n := promptCount(fh); n != 0 {
		t.Fatalf("the sender was messaged: %d", n)
	}
}

// The event store: filters, newest first, limit, a restart, pruning.
func TestEventStoreQueryAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	st := openEvents(path)
	base := time.Now().UTC().Add(-3 * time.Hour)
	st.add(Event{At: base, Kind: "park", Text: "a"})
	st.add(Event{At: base.Add(time.Hour), Kind: "landed", Key: "k", Text: "b"})
	if st.add(Event{At: base.Add(90 * time.Minute), Kind: "landed", Key: "k", Text: "dup"}) {
		t.Fatal("a repeated key was added")
	}
	st.add(Event{At: base.Add(2 * time.Hour), Kind: "park", Text: "c"})
	got := st.query(base.Add(30*time.Minute), nil, 0)
	if len(got) != 2 || got[0].Text != "c" || got[1].Text != "b" {
		t.Fatalf("since: %+v", got)
	}
	if got := st.query(time.Time{}, []string{"park"}, 0); len(got) != 2 || got[0].Text != "c" {
		t.Fatalf("kind: %+v", got)
	}
	if got := st.query(time.Time{}, nil, 1); len(got) != 1 || got[0].Text != "c" {
		t.Fatalf("limit: %+v", got)
	}
	re := openEvents(path)
	if got := re.query(time.Time{}, nil, 0); len(got) != 3 {
		t.Fatalf("after restart: %+v", got)
	}
	// Ids are distinct.
	ids := map[string]bool{}
	for _, e := range re.query(time.Time{}, nil, 0) {
		ids[e.ID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
	old := openEvents("")
	old.add(Event{At: time.Now().Add(-15 * 24 * time.Hour), Kind: "park", Text: "old"})
	if got := old.query(time.Time{}, nil, 0); len(got) != 0 {
		t.Fatalf("an event past its keep time stayed: %+v", got)
	}
}

// An outbox file an older hub left is turned into events once, then removed.
func TestOutboxIsMigratedToEvents(t *testing.T) {
	s, _, _ := eventsServer(t)
	dir := t.TempDir()
	b, _ := json.Marshal(map[string]any{"lines": []map[string]any{{"key": "landed/h1", "kind": "landed", "entry": "h1", "text": "handoff h1 landed", "at": time.Now()}}})
	if err := os.WriteFile(outboxPath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.migrateOutbox(dir)
	if evs := allEvents(s, "landed"); len(evs) != 1 || evs[0].Subject != "h1" {
		t.Fatalf("events = %+v", evs)
	}
	if _, err := os.Stat(outboxPath(dir)); err == nil {
		t.Fatal("the outbox file stayed")
	}
}

// GET /api/log: since, kind, limit; and the board's Hub did lane.
func TestLogAPIAndBoardLane(t *testing.T) {
	s, _, _ := eventsServer(t)
	now := time.Now().UTC()
	for i, k := range []string{"park", "landed", "park", "autoremove", "cleanup", "park", "landed"} {
		s.events.add(Event{At: now.Add(-time.Duration(7-i) * time.Minute), Kind: k, Subject: "s" + k, Text: "line " + string(rune('a'+i))})
	}
	s.events.add(Event{At: now.Add(-30 * time.Hour), Kind: "park", Text: "yesterday"})

	var out struct {
		Events []Event `json:"events"`
	}
	getJSON := func(path string, v any) {
		t.Helper()
		w := do(s, "GET", path, userHdr, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
			t.Fatal(err)
		}
	}
	getJSON("/api/log", &out)
	if len(out.Events) != 7 || out.Events[0].Text != "line g" {
		t.Fatalf("default 24h: %d %+v", len(out.Events), out.Events)
	}
	getJSON("/api/log?since=48h", &out)
	if len(out.Events) != 8 {
		t.Fatalf("48h: %d", len(out.Events))
	}
	getJSON("/api/log?kind=park,cleanup", &out)
	if len(out.Events) != 4 {
		t.Fatalf("kinds: %+v", out.Events)
	}
	getJSON("/api/log?limit=2", &out)
	if len(out.Events) != 2 {
		t.Fatalf("limit: %+v", out.Events)
	}
	if w := do(s, "GET", "/api/log?since=soon", userHdr, ""); w.Code != 400 {
		t.Fatalf("bad since: %d", w.Code)
	}

	// The board lane: the count of the day, the newest five.
	var bv struct {
		Lanes []BoardLane `json:"lanes"`
	}
	getJSON("/api/board", &bv)
	var lane *BoardLane
	for i := range bv.Lanes {
		if bv.Lanes[i].Key == "hub_did" {
			lane = &bv.Lanes[i]
		}
	}
	if lane == nil || lane.Name != "Hub did" || lane.Count != 7 || len(lane.Items) != 5 || lane.Items[0].Title != "line g" || lane.Items[0].Kind != "landed" {
		t.Fatalf("lane = %+v", lane)
	}
}

// With nothing to show, the lane is not there.
func TestBoardLaneHiddenAtZero(t *testing.T) {
	s, _, _ := eventsServer(t)
	s.events.add(Event{At: time.Now().Add(-30 * time.Hour), Kind: "park", Text: "yesterday"})
	var bv struct {
		Lanes []BoardLane `json:"lanes"`
	}
	w := do(s, "GET", "/api/board", userHdr, "")
	if err := json.Unmarshal(w.Body.Bytes(), &bv); err != nil {
		t.Fatal(err)
	}
	for _, l := range bv.Lanes {
		if l.Key == "hub_did" {
			t.Fatalf("lane shown with 0 events: %+v", l)
		}
	}
}
