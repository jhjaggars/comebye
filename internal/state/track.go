package state

import (
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// AgentKey is the key an agent is tracked, throttled and presence-matched
// by: its host-qualified ID when it has one, else its pane ID.
func AgentKey(a *herdr.AgentInfo) string {
	if a.ID != "" {
		return a.ID
	}
	return a.PaneID
}

// QualifiedID builds the host-qualified agent ID "<host>/<pane>".
func QualifiedID(host, pane string) string {
	if host == "" {
		return pane
	}
	return host + "/" + pane
}

// Transition is an agent whose status changed between two observations.
type Transition struct {
	Agent  herdr.AgentInfo
	Before herdr.AgentStatus
}

// Tracker detects status transitions per agent (keyed by AgentKey), the
// logic that decides when to notify. It is shared by the local state
// manager and the hub's peer streams. The first observation, and any agent
// seen for the first time, only sets a baseline, so a (re)connect never
// replays transitions that happened while nobody was watching.
//
// A Tracker is not safe for concurrent use; callers hold their own lock.
type Tracker struct {
	last  map[string]herdr.AgentStatus
	since map[string]time.Time
	have  bool
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{last: map[string]herdr.AgentStatus{}, since: map[string]time.Time{}}
}

// Observe ingests an authoritative agent list and returns the transitions
// since the previous one. Agents missing from the list are forgotten.
func (t *Tracker) Observe(agents []herdr.AgentInfo, now time.Time) []Transition {
	var out []Transition
	seen := make(map[string]bool, len(agents))
	for i := range agents {
		a := &agents[i]
		k := AgentKey(a)
		seen[k] = true
		before, had := t.last[k]
		switch {
		case !t.have || !had:
			t.last[k] = a.Status // baseline, no notification
			t.since[k] = now
		case before != a.Status:
			t.last[k] = a.Status
			t.since[k] = now
			out = append(out, Transition{Agent: *a, Before: before})
		}
	}
	for k := range t.last {
		if !seen[k] {
			delete(t.last, k)
			delete(t.since, k)
		}
	}
	t.have = true
	return out
}

// Update applies one agent's new status between full observations (the
// event fast path). changed is true for a transition of a known agent.
func (t *Tracker) Update(key string, status herdr.AgentStatus, now time.Time) (before herdr.AgentStatus, changed bool) {
	if !t.have {
		return "", false
	}
	prev, had := t.last[key]
	t.last[key] = status
	if !had {
		t.since[key] = now
		return "", false
	}
	if prev == status {
		return prev, false
	}
	t.since[key] = now
	return prev, true
}

// Since is when the agent entered its current status, as far as this
// tracker has seen; zero if unknown.
func (t *Tracker) Since(key string) time.Time { return t.since[key] }

// Reset forgets everything, so the next observation is a fresh baseline
// (used when a peer disconnects).
func (t *Tracker) Reset() {
	t.last = map[string]herdr.AgentStatus{}
	t.since = map[string]time.Time{}
	t.have = false
}
