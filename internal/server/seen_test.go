package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func seenAgent(st herdr.AgentStatus, since time.Time) herdr.AgentInfo {
	return rAgent("w9:p1", "feat", st, since)
}

func shown(s *Server, a herdr.AgentInfo) herdr.AgentStatus {
	return s.slimAgents([]herdr.AgentInfo{a})[0].Status
}

func TestSeenDoneBecomesIdleUntilNextTurn(t *testing.T) {
	s, _, _ := todoServer(t)
	t0 := autoT0
	done := seenAgent(herdr.StatusDone, t0)
	if got := shown(s, done); got != herdr.StatusDone {
		t.Fatalf("before seen: %s", got)
	}
	st := s.seenStore()
	if !st.mark(&done, t0.Add(time.Minute)) {
		t.Fatal("mark reported no change")
	}
	if got := shown(s, done); got != herdr.StatusIdle {
		t.Fatalf("after seen: %s, want idle", got)
	}
	if got := shown(s, seenAgent(herdr.StatusWorking, t0.Add(2*time.Minute))); got != herdr.StatusWorking {
		t.Fatalf("working: %s", got)
	}
	if got := shown(s, seenAgent(herdr.StatusDone, t0.Add(3*time.Minute))); got != herdr.StatusDone {
		t.Fatalf("a new turn finished: %s, want done again", got)
	}
	// a working or idle agent is never marked
	w := seenAgent(herdr.StatusWorking, t0)
	if st.mark(&w, t0) {
		t.Fatal("marked a working agent")
	}
}

func TestSeenSurvivesRestart(t *testing.T) {
	s, _, dir := todoServer(t)
	_ = dir
	if s.cfg.StateDir == "" {
		t.Skip("no state dir")
	}
	t0 := autoT0
	done := seenAgent(herdr.StatusDone, t0)
	s.seenStore().mark(&done, t0)

	// a restarted hub: reads the file, and sees the same agent with a
	// baseline StatusSince at the restart.
	s2 := &Server{cfg: s.cfg}
	again := seenAgent(herdr.StatusDone, t0.Add(time.Hour))
	if got := s2.seenStore().m[seenKey(&done)]; got.Since.IsZero() {
		t.Fatal("record not persisted")
	}
	list := []herdr.AgentInfo{again}
	s2.seenStore().apply(list)
	if list[0].Status != herdr.StatusIdle {
		t.Fatalf("after restart: %s, want idle", list[0].Status)
	}
	// and a later turn is still caught
	later := []herdr.AgentInfo{seenAgent(herdr.StatusDone, t0.Add(2*time.Hour))}
	s2.seenStore().apply(later)
	if later[0].Status != herdr.StatusDone {
		t.Fatalf("new turn after restart: %s", later[0].Status)
	}
}

func TestSeenInstanceIsolated(t *testing.T) {
	s, _, _ := todoServer(t)
	a := seenAgent(herdr.StatusDone, autoT0)
	a.Instance = "i1"
	s.seenStore().mark(&a, autoT0)
	b := a
	b.Instance = "i2"
	if got := shown(s, b); got != herdr.StatusDone {
		t.Fatalf("another incarnation: %s", got)
	}
}

func TestSeenRouteUserOnly(t *testing.T) {
	s, _, _ := coordServer(t)
	for _, p := range []string{"/api/agents/w2:p1/seen", "/api/hosts/desk/agents/w2:p1/seen"} {
		if w := do(s, "POST", p, agentHdr("w2:p1"), `{}`); w.Code != http.StatusForbidden {
			t.Errorf("agent POST %s = %d, want 403", p, w.Code)
		}
		if w := do(s, "POST", p, userHdr, `{}`); w.Code != http.StatusOK {
			t.Errorf("user POST %s = %d %s", p, w.Code, w.Body)
		}
		if w := do(s, "POST", p[:len(p)-len("w2:p1/seen")]+"nope/seen", userHdr, `{}`); w.Code != http.StatusNotFound {
			t.Errorf("unknown agent = %d", w.Code)
		}
	}
}
