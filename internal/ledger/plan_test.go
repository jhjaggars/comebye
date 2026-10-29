package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func TestPlanSettles(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Now()
	p, _ := s.AddPlan(Plan{Title: "add foo", Size: 2, ReplyTo: &Ref{AgentID: "desk/w1:p1"}})
	a, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, PlanID: p.ID})
	if got := s.EvalPlans(now); len(got) != 0 {
		t.Fatalf("settled with one of two members: %+v", got)
	}
	b, _ := s.Add(Entry{AgentID: "desk/w9:p2", Status: Running, PlanID: p.ID})
	both := func(sa, sb herdr.AgentStatus) []herdr.AgentInfo {
		return []herdr.AgentInfo{agent(a.AgentID, sa), agent(b.AgentID, sb)}
	}
	s.Observe(both(herdr.StatusWorking, herdr.StatusWorking), now)
	s.Observe(both(herdr.StatusIdle, herdr.StatusWorking), now)
	if got := s.EvalPlans(now); len(got) != 0 {
		t.Fatalf("settled with a member working: %+v", got)
	}
	s.Observe(both(herdr.StatusIdle, herdr.StatusIdle), now)
	got := s.EvalPlans(now)
	if len(got) != 1 || got[0].Outcome != PlanDone || got[0].Callback == nil || got[0].Callback.State != CallbackPending {
		t.Fatalf("settled = %+v", got)
	}
	// Once: a second look settles nothing new, and the callback is findable.
	if got := s.EvalPlans(now); len(got) != 0 {
		t.Fatalf("settled twice: %+v", got)
	}
	pc := s.PendingPlanCallbacks()
	if len(pc) != 1 {
		t.Fatalf("pending = %+v", pc)
	}
	s.PlanCallbackDone(p.ID, pc[0].Callback.At, CallbackDelivered, "desk/w1:p1", now)
	if len(s.PendingPlanCallbacks()) != 0 {
		t.Fatal("still pending")
	}

	// A member back at work reopens it; it settles (and tells) again.
	s.Observe(both(herdr.StatusIdle, herdr.StatusWorking), now)
	if got := s.EvalPlans(now); len(got) != 0 {
		t.Fatalf("settled while reopened: %+v", got)
	}
	if q, _ := s.GetPlan(p.ID); q.Outcome != "" || q.Notified {
		t.Fatalf("not reopened: %+v", q)
	}
	s.Observe(both(herdr.StatusIdle, herdr.StatusIdle), now)
	if got := s.EvalPlans(now); len(got) != 1 {
		t.Fatalf("not settled again: %+v", got)
	}
	// A finished member that is removed later still counts as finished.
	s.Removed(a.AgentID, nil, now)
	if got := s.EvalPlans(now); len(got) != 0 {
		t.Fatalf("removal of a finished member changed the plan: %+v", got)
	}
	if q, _ := s.GetPlan(p.ID); q.Outcome != PlanDone {
		t.Fatalf("plan = %+v", q)
	}
}

func TestPlanPartial(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Now()
	p, _ := s.AddPlan(Plan{Title: "x", Size: 2})
	ok, _ := s.Add(Entry{AgentID: "desk/w9:p1", Status: Running, PlanID: p.ID})
	s.Add(Entry{Status: Failed, Error: "no room", PlanID: p.ID})
	if got := s.EvalPlans(now); len(got) != 0 {
		t.Fatalf("settled with a member pending: %+v", got)
	}
	s.Observe([]herdr.AgentInfo{agent(ok.AgentID, herdr.StatusWorking)}, now)
	s.Observe([]herdr.AgentInfo{agent(ok.AgentID, herdr.StatusIdle)}, now)
	got := s.EvalPlans(now)
	if len(got) != 1 || got[0].Outcome != PlanPartial || got[0].Callback != nil {
		t.Fatalf("settled = %+v", got)
	}
	// A worker removed before it ever finished is a failed member.
	if MemberOutcome(Entry{Status: Removed}) != MemberFailed || MemberOutcome(Entry{Status: ReadyToRemove}) != MemberPending {
		t.Fatal("member outcomes")
	}
}
