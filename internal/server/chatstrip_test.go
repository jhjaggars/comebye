package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// chatAgent is a chat helper of page as the fleet would show it: idle for d,
// in its scratch directory, with the hub's role and record for it.
func chatAgent(t *testing.T, s *Server, page string, d time.Duration, now time.Time) herdr.AgentInfo {
	t.Helper()
	since := now.Add(-d)
	a := herdr.AgentInfo{ID: "desk/w30:p1", Host: "desk", PaneID: "w30:p1", Name: "chat-drill", Agent: "claude", Status: herdr.StatusIdle,
		StatusSince: &since, CWD: filepath.Join(s.chatBase(), page+"-1")}
	if err := s.roles.set(a.ID, Role{Role: RoleChat, Name: a.Name, Page: page, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.chats.set(page, ChatRec{Agent: a.ID, Host: "desk", Name: a.Name, Version: 2, At: now}); err != nil {
		t.Fatal(err)
	}
	return a
}

// A chat helper has no worktree, so the guards that need one must not keep it
// from being parked once it has been idle long enough.
func TestIdleChatHelperIsSuggestedAndAutoParked(t *testing.T) {
	s, _, page, _ := chatHub(t)
	s.initParkMode(24*time.Hour, ParkModeAuto)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var parked []string
	var checked int
	s.parkSug.auto = true
	s.parkSug.now = func() time.Time { return now }
	s.parkSug.parkOne = func(_ context.Context, a herdr.AgentInfo) (string, error) {
		parked = append(parked, a.Name)
		return "pk_" + a.Name, nil
	}
	s.parkSug.checkClean = func(context.Context, herdr.AgentInfo) (bool, bool) { checked++; return false, false }

	young := chatAgent(t, s, page, 5*time.Hour, now)
	s.parkSuggestStep(context.Background(), []herdr.AgentInfo{young}, now)
	s.autoParkStep(context.Background(), []herdr.AgentInfo{young}, now) // the janitor's stage, after the suggestion
	if len(parked) != 0 {
		t.Fatalf("parked a chat idle for 5h: %v", parked)
	}
	// Idle for a day: suggested after the hour setting, parked after a day.
	old := chatAgent(t, s, page, 30*time.Hour, now)
	s.parkSuggestStep(context.Background(), []herdr.AgentInfo{old}, now)
	s.autoParkStep(context.Background(), []herdr.AgentInfo{old}, now) // the janitor's stage, after the suggestion
	if len(parked) != 1 || parked[0] != "chat-drill" {
		t.Fatalf("parked %v, want the idle chat", parked)
	}
	if checked != 0 {
		t.Errorf("asked for a worktree check of a chat helper %d times", checked)
	}

	// A suggestion in suggest mode: the hint is set, nothing parked.
	s.parkSug.auto = false
	parked = nil
	s.parkSuggestStep(context.Background(), []herdr.AgentInfo{old}, now)
	s.autoParkStep(context.Background(), []herdr.AgentInfo{old}, now) // the janitor's stage, after the suggestion
	cp := []herdr.AgentInfo{old}
	s.stampPark(cp)
	if cp[0].ParkHint == nil || len(parked) != 0 {
		t.Errorf("suggest mode: hint=%v parked=%v", cp[0].ParkHint, parked)
	}

	// The other guards still hold: an open question of its own, a queued prompt.
	s.parkSug.auto = true
	if _, _, err := s.todos.add(Todo{Question: "Which one?", Kind: "decision", CreatedBy: old.Name, CreatedByID: old.ID, Source: "agent"}); err != nil {
		t.Fatal(err)
	}
	s.parkSug.m = map[string]*parkSugState{}
	s.parkSuggestStep(context.Background(), []herdr.AgentInfo{old}, now)
	s.autoParkStep(context.Background(), []herdr.AgentInfo{old}, now) // the janitor's stage, after the suggestion
	if len(parked) != 0 {
		t.Errorf("parked a chat with an open question: %v", parked)
	}
}

// Without the role, or outside <state>/chats, an agent without a worktree is
// still no candidate: only the hub's own chat helpers are let through.
func TestParkGuardLetsOnlyChatHelpersWithoutAWorktree(t *testing.T) {
	s, _, page, _ := chatHub(t)
	s.initParkMode(24*time.Hour, ParkModeAuto)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	a := chatAgent(t, s, page, 30*time.Hour, now)
	if why := s.parkBlock(a, 24*time.Hour, now, nil); why != "" {
		t.Fatalf("chat helper blocked: %s", why)
	}
	elsewhere := a
	elsewhere.CWD = "/home/u/code/chats/" + page + "-1"
	if why := s.parkBlock(elsewhere, 24*time.Hour, now, nil); why == "" {
		t.Error("a directory that is not <state>/chats-like let an agent through")
	}
	renamed := a
	renamed.Name = "someone-else"
	if why := s.parkBlock(renamed, 24*time.Hour, now, nil); why == "" {
		t.Error("a pane that now runs another agent kept the chat exemption")
	}
	plain := a
	plain.ID, plain.PaneID = "desk/w31:p1", "w31:p1"
	if why := s.parkBlock(plain, 24*time.Hour, now, nil); why == "" {
		t.Error("an agent without the chat role was exempted")
	}
}

// The agents list marks chat helpers with the Page, its title and the version.
func TestAgentsListCarriesTheChatMarker(t *testing.T) {
	s, _, page, _ := chatHub(t)
	now := time.Now()
	a := chatAgent(t, s, page, time.Hour, now)
	other := herdr.AgentInfo{ID: "desk/w31:p1", Host: "desk", PaneID: "w31:p1", Name: "worker", Agent: "claude"}
	out := s.slimAgents([]herdr.AgentInfo{a, other})
	if out[0].Chat == nil || out[0].Chat.Page != page || out[0].Chat.Version != 2 || out[0].Chat.Title == "" {
		t.Fatalf("chat marker: %+v", out[0].Chat)
	}
	if out[1].Chat != nil {
		t.Errorf("a worker is marked as a chat: %+v", out[1].Chat)
	}
	// The pane now runs another agent: no mark.
	a.Name = "reused"
	if m := s.slimAgents([]herdr.AgentInfo{a})[0].Chat; m != nil {
		t.Errorf("marker kept after the pane was reused: %+v", m)
	}
}
