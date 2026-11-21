package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/idle"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// R2: a prompt typed into an idle pane (a handoff) holds the host up until
// an agent picks it up; a refused prompt holds nothing.
func TestPromptSetsAnIdleHold(t *testing.T) {
	s, fh, _ := coordServer(t)
	if s.cfg.StateDir == "" {
		t.Fatal("the rig has no state dir")
	}
	dir := idle.HoldsDir(s.cfg.StateDir)
	showScreen(fh, "w2:p1", claudePermScreen)
	if w := do(s, "POST", "/api/agents/home/prompt", agentHdr("w1:p1"), `{"text":"hello"}`); w.Code != http.StatusConflict {
		t.Fatalf("guarded prompt = %d", w.Code)
	}
	if hs := idle.ActiveHolds(dir, time.Now()); len(hs) != 0 {
		t.Fatalf("a refused prompt set a hold: %+v", hs)
	}
	showScreen(fh, "w2:p1", "")
	if w := do(s, "POST", "/api/agents/home/prompt", agentHdr("w1:p1"), `{"text":"hello"}`); w.Code != 200 {
		t.Fatalf("prompt = %d %s", w.Code, w.Body)
	}
	hs := idle.ActiveHolds(dir, time.Now())
	if len(hs) != 1 || hs[0].Name != "prompt" || !strings.Contains(hs[0].Reason, "home") {
		t.Fatalf("holds = %+v", hs)
	}
	if d := time.Until(hs[0].Until); d < inFlightHold-time.Minute || d > inFlightHold {
		t.Fatalf("hold for %v", d)
	}
}

// An upgrade holds the host for as long as its running record is believed.
func TestUpgradeSetsAnIdleHold(t *testing.T) {
	s, run := peerRelServer(t)
	if w := do(s, "POST", "/api/release/upgrade", hubCall, `{"version":"v0.5.9"}`); w.Code != 202 || run.count() != 1 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	hs := idle.ActiveHolds(idle.HoldsDir(s.cfg.StateDir), time.Now())
	if len(hs) != 1 || hs[0].Name != "upgrade" || hs[0].Reason != "upgrade to v0.5.9" {
		t.Fatalf("holds = %+v", hs)
	}
	if d := time.Until(hs[0].Until); d < upgrade.StaleAfter-time.Minute || d > upgrade.StaleAfter {
		t.Fatalf("hold for %v", d)
	}
}
