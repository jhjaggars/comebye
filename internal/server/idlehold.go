package server

import (
	"log/slog"
	"time"

	"github.com/jhjaggars/comebye/internal/idle"
)

// inFlightHold is how long work that has reached this host, and that no
// agent has picked up yet, keeps the host from stopping as idle: a prompt
// typed into an idle pane (a handoff, a queued prompt, a nudge), a launch
// or a resume. The agent is "working" within seconds when it takes the
// work, and that keeps the host up from then on; the hold covers the gap
// and an agent CLI that is slow to start.
const inFlightHold = 15 * time.Minute

// holdIdle keeps this host from stopping as idle until d from now. The VM's
// idle-check.sh and `comebye idle` both honour it (internal/idle/hold.go).
// Best effort: a host that cannot write a hold still works, it may only
// stop sooner.
func (s *Server) holdIdle(name string, d time.Duration, reason string) {
	if s.cfg.StateDir == "" {
		return
	}
	if err := idle.SetHold(idle.HoldsDir(s.cfg.StateDir), name, time.Now().Add(d), reason); err != nil {
		slog.Warn("idle hold", "name", name, "err", err)
	}
}

func (s *Server) clearIdleHold(name string) {
	if s.cfg.StateDir == "" {
		return
	}
	_ = idle.ClearHold(idle.HoldsDir(s.cfg.StateDir), name)
}
