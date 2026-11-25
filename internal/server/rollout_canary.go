package server

import (
	"fmt"
	"strings"
	"time"
)

// The canary and its soak (issue #141, slice 2).
//
// Every rollout, automatic or tapped, upgrades one peer first, alone: the
// canary. Once it is back on the version it soaks: the hub watches it for a
// while (10 minutes by default) before any other host is touched. The soak
// fails on
//
//   - a restart: the host's comebye process started again (its
//     upgradeStatus.Started changed), which is how a crash loop shows when
//     systemd's Restart=on-failure brings it back within seconds;
//   - a failed health check: the host went offline, or could not be asked;
//   - a version that went back: it no longer runs the release (its smoke
//     check put comebye.prev back, or someone did);
//   - a new upgrade run on it that failed.
//
// A failed soak stops the rollout where it is: nothing else is touched, the
// canary keeps whatever it runs (its own fallback when its smoke check put
// it back), and the hub files one To do saying what failed (alarmSoak).
//
// The canary is a hub setting (settings.json "rollout"): by default the
// -release-canary peer, else the release host (-release-host), which tags
// and runs the hub bump: proving it first also proves the machine that runs
// the last wave. "none" turns the canary off. A tapped rollout can skip the
// soak, for an urgent fix: at the confirm (skip_soak), or while it soaks
// (POST /api/release/rollout/skip-soak). An automatic rollout always soaks.

const (
	// rolloutSoakDefault is the soak when the setting says nothing.
	rolloutSoakDefault = 10 * time.Minute
	rolloutSoakMaxMin  = 120
	// canaryNone in the setting turns the canary off.
	canaryNone = "none"
)

// rolloutSettings is settings.json's "rollout".
type rolloutSettings struct {
	// Canary is the peer upgraded first: "" is the default (-release-canary,
	// else the release host), "none" is no canary (and no soak).
	Canary string `json:"canary,omitempty"`
	// SoakMinutes is how long the canary soaks: nil is 10 minutes.
	SoakMinutes *int `json:"soak_minutes,omitempty"`
}

func normalizeRolloutSettings(r *rolloutSettings) *rolloutSettings {
	if r == nil {
		return nil
	}
	c := copyRolloutSettings(r)
	c.Canary = strings.TrimSpace(c.Canary)
	if strings.EqualFold(c.Canary, canaryNone) {
		c.Canary = canaryNone
	}
	if c.SoakMinutes != nil {
		n := min(max(*c.SoakMinutes, 1), rolloutSoakMaxMin)
		c.SoakMinutes = &n
	}
	if c.Canary == "" && c.SoakMinutes == nil {
		return nil
	}
	return c
}

func copyRolloutSettings(r *rolloutSettings) *rolloutSettings {
	if r == nil {
		return nil
	}
	c := *r
	if r.SoakMinutes != nil {
		n := *r.SoakMinutes
		c.SoakMinutes = &n
	}
	return &c
}

// checkRolloutSettings is the PUT's check: "" when the setting is fine.
func checkRolloutSettings(r *rolloutSettings) string {
	if r.SoakMinutes != nil && (*r.SoakMinutes < 1 || *r.SoakMinutes > rolloutSoakMaxMin) {
		return fmt.Sprintf("rollout.soak_minutes: want 1 to %d", rolloutSoakMaxMin)
	}
	c := strings.TrimSpace(r.Canary)
	if len(c) > 64 || strings.ContainsAny(c, " \t\r\n/\\\"'<>") {
		return "rollout.canary: want a host name, or \"none\""
	}
	return ""
}

// rolloutSettingsView is the setting in force, for the Settings screen.
type rolloutSettingsView struct {
	Hub bool `json:"hub"` // this host runs the rollouts
	// Canary is the canary in force ("" none); CanaryFrom says why:
	// "setting", "flag" (-release-canary), "release host" or "none".
	Canary      string `json:"canary"`
	CanaryFrom  string `json:"canary_from"`
	SoakMinutes int    `json:"soak_minutes"`
	// SoakSet: the soak comes from the setting (else the default).
	SoakSet bool `json:"soak_set,omitempty"`
}

func (s *Server) rolloutSettingsView() rolloutSettingsView {
	c, from := s.rolloutCanary()
	v := rolloutSettingsView{Hub: s.rollout != nil, Canary: c, CanaryFrom: from, SoakMinutes: int(rolloutSoakDefault / time.Minute)}
	if r := s.settings.get().Rollout; r != nil && r.SoakMinutes != nil {
		v.SoakMinutes, v.SoakSet = *r.SoakMinutes, true
	}
	return v
}

// rolloutCanary is the canary in force, and where it comes from.
func (s *Server) rolloutCanary() (string, string) {
	if r := s.settings.get().Rollout; r != nil && r.Canary != "" {
		if r.Canary == canaryNone {
			return "", "none"
		}
		return r.Canary, "setting"
	}
	if c := s.cfg.Release.Canary; c != "" {
		return c, "flag"
	}
	if h := s.cfg.Release.Host; h != "" {
		return h, "release host"
	}
	return "", "none"
}

// canaryName is rolloutCanary's name alone.
func (s *Server) canaryName() string {
	c, _ := s.rolloutCanary()
	return c
}

// rolloutSoakSetting is the soak the setting asks for; 0 when it says
// nothing, and the engine then uses its own default (10 minutes, shorter
// under -release-simulate and -test-mode).
func (s *Server) rolloutSoakSetting() time.Duration {
	if r := s.settings.get().Rollout; r != nil && r.SoakMinutes != nil {
		return time.Duration(*r.SoakMinutes) * time.Minute
	}
	return 0
}

// noSoak makes a plan's canary an ordinary first peer: the tapped
// rollout's skip_soak. The order stays as it was.
func noSoak(plan []RolloutStep) []RolloutStep {
	for i := range plan {
		if plan[i].Wave == waveCanary {
			plan[i].Wave = wavePeers
		}
	}
	return plan
}

// soakDuration is the soak a canary starting to soak now gets.
func (e *rolloutEngine) soakDuration() time.Duration {
	if e.soakFor != nil {
		if d := e.soakFor(); d > 0 {
			return d
		}
	}
	return e.soak
}

// SkipSoak ends the soak of a tapped rollout now: the canary's step is done
// and the others follow. An automatic rollout always soaks.
func (e *rolloutEngine) SkipSoak(by string) (Rollout, error) {
	return e.control(by, func(j *Rollout) (string, string, error) {
		if j.State != roRunning || j.Auto {
			return "", "", errRolloutState
		}
		i := inFlight(*j)
		if i < 0 || j.Plan[i].Phase != roSoaking {
			return "", "", errRolloutState
		}
		st := &j.Plan[i]
		now := e.now()
		left := ""
		if st.SoakUntil != nil {
			left = " with " + st.SoakUntil.Sub(now).Round(time.Second).String() + " left"
		}
		t := now
		st.Phase, st.Done, st.Detail, st.SoakUntil = roOK, &t, "soak skipped by "+oneLine(by, 60)+left, nil
		j.SoakSkipped = true
		j.Note = "soak of " + st.Host + " skipped"
		if j.PauseRequested && current(*j) >= 0 {
			j.State, j.PauseRequested, j.Note = roPaused, false, "paused after "+st.Host
		}
		return "rollout.soak-skip", st.Host + left, nil
	})
}

// alarmSoak files the one To do for a canary whose soak failed. A run that
// failed or rolled back (its own record) is filed by alarmUpgrade with its
// output; at most one item is open per release and host either way.
func (s *Server) alarmSoak(j Rollout, st RolloutStep) {
	if s.todos == nil || j.Todo == "" {
		return
	}
	rel, ok := s.todos.get(j.Todo)
	if !ok || rel.Release == nil || rel.Release.Version != j.Version {
		return
	}
	what := "The canary soak of " + mdSafe(st.Host) + " (the rollout stopped before any other host)"
	s.rolloutFailed(rel, st.Host, fmt.Sprintf("%s:soak:%d", j.ID, st.Attempt), what, "", st.Detail, st.Output)
}
