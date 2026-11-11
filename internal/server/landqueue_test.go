package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// qWorker adds a worker that reported, ready to be landed.
func qWorker(t *testing.T, s *Server, pane, repo, base string) ledger.Entry {
	t.Helper()
	id := "desk/" + pane
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", Repo: repo, RepoName: "proj", Branch: pane, Base: base, AgentID: id, PaneID: pane, AgentName: pane, Status: ledger.Running, ReplyTo: &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ledger.Report(id, "## Summary\nwork", time.Now().Add(-time.Hour)); !ok {
		t.Fatal("report refused")
	}
	return e
}

func queueResp(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := do(s, method, path, agentHdr("w1:p1"), body)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func pqHas(s *Server, agent, substr string) bool {
	for _, p := range s.pq.list(agent, true) {
		if strings.Contains(p.Text, substr) {
			return true
		}
	}
	return false
}

func TestLandingQueueOverHTTP(t *testing.T) {
	s, _, _ := coordServer(t)
	qWorker(t, s, "w2:p1", "/r/proj", "main")
	qWorker(t, s, "w3:p1", "/r/proj", "main")
	qWorker(t, s, "w4:p1", "/r/proj", "main")
	other := qWorker(t, s, "w5:p1", "/r/other", "main")

	_, out := queueResp(t, s, "POST", "/api/landings", `{"agent_id":"desk/w2:p1","note":"ok"}`)
	if q, _ := out["queue"].(map[string]any); q == nil || q["turn"] != true || q["ahead"] != float64(0) {
		t.Fatalf("first landing is up at once: %v", out)
	}
	_, out = queueResp(t, s, "POST", "/api/landings", `{"agent_id":"desk/w3:p1","note":"ok"}`)
	if q, _ := out["queue"].(map[string]any); q == nil || q["turn"] != false || q["ahead"] != float64(1) || q["position"] != float64(2) {
		t.Fatalf("second waits: %v", out)
	}
	queueResp(t, s, "POST", "/api/landings", `{"agent_id":"desk/w4:p1","note":"ok"}`)
	// Another repo has its own queue.
	_, out = queueResp(t, s, "POST", "/api/landings", `{"agent_id":"`+other.AgentID+`","note":"ok"}`)
	if q, _ := out["queue"].(map[string]any); q == nil || q["turn"] != true {
		t.Fatalf("other repo: %v", out)
	}
	if pqHas(s, "desk/w3:p1", "You're up") {
		t.Fatal("w3 must not be told it is up yet")
	}

	code, out := queueResp(t, s, "GET", "/api/landings/queue", "")
	qs, _ := out["queues"].([]any)
	if code != 200 || len(qs) != 2 || out["turn_timeout_seconds"] != float64(45*60) {
		t.Fatalf("queue list = %d %v", code, out)
	}

	// Reorder: w4 to second (a no-op), then skip w3 to the back.
	if code, _ := queueResp(t, s, "POST", "/api/landings/queue", `{"agent_id":"desk/w4:p1","action":"move","to":2}`); code != 200 {
		t.Fatalf("move = %d", code)
	}
	if code, _ := queueResp(t, s, "POST", "/api/landings/queue", `{"agent_id":"desk/w2:p1","action":"move","to":3}`); code != 409 {
		t.Fatalf("the holder cannot be moved: %d", code)
	}
	if code, _ := queueResp(t, s, "POST", "/api/landings/queue", `{"agent_id":"desk/w1:p1","action":"skip"}`); code != 409 {
		t.Fatalf("self/not queued = %d", code)
	}

	// The holder lands (the hub's check found the commits on the base).
	h, _ := s.ledger.ByAgent("desk/w2:p1")
	s.ledger.SetLandCheck(h.ID, h.FinishedAt, ledger.LandCheck{OK: true, At: time.Now()})
	s.advanceLandQueues()
	if !pqHas(s, "desk/w4:p1", "You're up") {
		t.Fatalf("w4 (moved ahead of w3) should be told it is up: %+v", s.pq.list("desk/w4:p1", true))
	}
	if pqHas(s, "desk/w3:p1", "You're up") {
		t.Fatal("w3 is still waiting")
	}
	// Skipping the holder tells it to stop and passes the turn.
	if code, out := queueResp(t, s, "POST", "/api/landings/queue", `{"agent_id":"desk/w4:p1","action":"skip"}`); code != 200 || out["skipped_turn"] != true {
		t.Fatalf("skip = %d %v", code, out)
	}
	if !pqHas(s, "desk/w4:p1", "skipped your turn") || !pqHas(s, "desk/w3:p1", "You're up") {
		t.Fatal("skip: w4 told to stop and w3 told it is up")
	}
}

func TestLandingQueueTurnTimeoutTellsCoordinator(t *testing.T) {
	s, _, _ := coordServer(t)
	s.landTurn = time.Minute
	qWorker(t, s, "w2:p1", "/r/proj", "main")
	qWorker(t, s, "w3:p1", "/r/proj", "main")
	queueResp(t, s, "POST", "/api/landings", `{"agent_id":"desk/w2:p1","note":"ok"}`)
	queueResp(t, s, "POST", "/api/landings", `{"agent_id":"desk/w3:p1","note":"ok"}`)
	// Age the holder's turn.
	e, _ := s.ledger.ByAgent("desk/w2:p1")
	past := time.Now().Add(-2 * time.Minute)
	if _, err := s.ledger.SetTurnAtForTest(e.ID, past); err != nil {
		t.Fatal(err)
	}
	s.advanceLandQueues()
	if !pqHas(s, "desk/w2:p1", "ran out") || !pqHas(s, "desk/w1:p1", "released the slot") || !pqHas(s, "desk/w3:p1", "You're up") {
		t.Fatalf("timeout notices: w2 %v, coordinator %v, w3 %v", s.pq.list("desk/w2:p1", true), s.pq.list("desk/w1:p1", true), s.pq.list("desk/w3:p1", true))
	}
}
