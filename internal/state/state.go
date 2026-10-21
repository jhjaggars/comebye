// Package state maintains an in-memory view of herdr's agents, keeps the
// event subscription aligned with the live pane set, diffs status changes,
// sends notifications (Web Push and optional ntfy), and broadcasts updates to SSE clients.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
)

// Manager owns the agent view and the event stream.
type Manager struct {
	client   *herdr.Client
	dispatch *Dispatcher
	stream   *herdr.Stream
	debounce time.Duration

	mu     sync.Mutex
	host   string // stamped on every agent as Host (and the ID prefix)
	agents []herdr.AgentInfo
	// tracker diffs statuses so notifications fire only on transitions
	// into blocked/done, and records when each agent entered its current
	// status (the fallback for AgentInfo.LastActive).
	tracker *Tracker

	// refreshPending coalesces event-driven refreshes.
	refreshTimer *time.Timer
	subRebuild   bool

	hub *Hub
}

// New wires up a Manager and performs the initial snapshot + subscription.
func New(client *herdr.Client, notifier *Notifier) *Manager {
	m := &Manager{
		client:   client,
		dispatch: &Dispatcher{Ntfy: notifier},
		debounce: 400 * time.Millisecond,
		tracker:  NewTracker(),
		hub:      NewHub(),
	}
	m.stream = herdr.NewStream(client, m.onEvent, m.onResync)
	return m
}

// SetPush enables Web Push delivery and foreground suppression (which
// also applies to ntfy). Call before Run.
func (m *Manager) SetPush(p Pusher, v Viewer) {
	m.dispatch.Push = p
	m.dispatch.Viewer = v
}

// Dispatcher returns the notification dispatcher, so the hub can share its
// channels, throttling and presence for peer agents.
func (m *Manager) Dispatcher() *Dispatcher { return m.dispatch }

// SetHost names this instance's host: every agent gets Host = name and
// ID = name + "/" + pane_id. Call before Run.
func (m *Manager) SetHost(name string) {
	m.mu.Lock()
	m.host = name
	m.mu.Unlock()
}

// Host returns the name set with SetHost.
func (m *Manager) Host() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.host
}

// Hub returns the SSE broadcast hub.
func (m *Manager) Hub() *Hub { return m.hub }

// Run starts the stream and blocks until ctx is done.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.refresh(ctx); err != nil {
		return fmt.Errorf("initial snapshot: %w", err)
	}
	m.resubscribe(ctx)

	// Safety net: even a healthy event stream can miss edge cases, and
	// herdr status classification is heuristic. Poll every 30s regardless.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.stream.Stop()
			return nil
		case <-ticker.C:
			m.scheduleRefresh()
		}
	}
}

func (m *Manager) onEvent(ev herdr.PushEvent) {
	switch ev.Event {
	case "pane.agent_status_changed":
		// Fast path: the event carries the new status; apply it immediately.
		var d herdr.AgentStatusChangedData
		if err := json.Unmarshal(ev.Data, &d); err == nil && d.PaneID != "" {
			m.applyStatus(d.PaneID, herdr.AgentStatus(d.Status))
		}
		// Also fall through to a debounced authoritative refresh, since the
		// pane may host a different agent now.
		m.scheduleRefresh()
	case "pane.created", "pane.closed", "pane.exited", "pane.moved", "pane.agent_detected", "pane.updated":
		// Pane set or contents changed: refresh state and rebuild the
		// per-pane status subscriptions once the refresh settles.
		m.mu.Lock()
		m.subRebuild = true
		m.mu.Unlock()
		m.scheduleRefresh()
	default:
		m.scheduleRefresh()
	}
}

func (m *Manager) onResync(reason string) {
	log.Printf("herdr stream resync: %s", reason)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.refresh(ctx); err != nil {
		log.Printf("refresh after resync: %v", err)
	}
	m.resubscribe(ctx)
	m.hub.Broadcast(Event{Type: "resync", Reason: reason})
}

// scheduleRefresh coalesces bursts of events (a busy pane can emit many
// pane.updated events per second) into one agent.list call.
func (m *Manager) scheduleRefresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refreshTimer != nil {
		m.refreshTimer.Reset(m.debounce)
		return
	}
	m.refreshTimer = time.AfterFunc(m.debounce, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.refresh(ctx); err != nil {
			log.Printf("scheduled refresh: %v", err)
		}
		m.mu.Lock()
		rebuild := m.subRebuild
		m.subRebuild = false
		m.refreshTimer = nil
		m.mu.Unlock()
		if rebuild {
			m.resubscribe(ctx)
		}
	})
}

// refresh re-reads agent.list, enriches agents with workspace worktree
// info from the session snapshot, applies status transitions (with
// notifications), and broadcasts the new list to SSE clients.
func (m *Manager) refresh(ctx context.Context) error {
	agents, err := m.client.ListAgents(ctx)
	if err != nil {
		return err
	}
	worktrees, wtErr := m.client.WorkspaceWorktrees(ctx)
	if wtErr != nil {
		// Non-fatal: the UI falls back to the cwd-derived label.
		log.Printf("workspace worktrees: %v", wtErr)
		worktrees = nil
	}
	for i := range agents {
		if wt, ok := worktrees[agents[i].WorkspaceID]; ok {
			agents[i].Worktree = wt
		}
	}
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].PaneID != agents[j].PaneID {
			return agents[i].PaneID < agents[j].PaneID
		}
		return agents[i].Name < agents[j].Name
	})

	m.mu.Lock()
	for i := range agents {
		agents[i].Host = m.host
		agents[i].ID = QualifiedID(m.host, agents[i].PaneID)
	}
	transitions := m.tracker.Observe(agents, time.Now())
	for i := range agents {
		agents[i].LastActive = lastActive(&agents[i], m.tracker.Since(AgentKey(&agents[i])))
	}
	m.agents = agents
	snapshot := agents
	m.mu.Unlock()

	// Notify outside the lock: ntfy posts can take seconds.
	for _, t := range transitions {
		m.notifyTransition(&t.Agent)
	}
	m.hub.Broadcast(Event{Type: "agents", Agents: snapshot})
	return nil
}

// lastActive prefers the agent's session file mtime, which survives server
// restarts, and falls back to when we first saw the current status.
func lastActive(a *herdr.AgentInfo, since time.Time) *time.Time {
	if s := a.AgentSession; s != nil && s.Kind == "path" && s.Value != "" {
		if fi, err := os.Stat(s.Value); err == nil {
			t := fi.ModTime()
			return &t
		}
	}
	if since.IsZero() {
		return nil
	}
	return &since
}

// applyStatus optimistically patches the cached status for one pane and
// pushes it out without waiting for the debounced refresh.
func (m *Manager) applyStatus(paneID string, status herdr.AgentStatus) {
	m.mu.Lock()
	// Copy-on-write: a snapshot handed out by Agents or a broadcast must
	// not change under its reader.
	agents := append([]herdr.AgentInfo(nil), m.agents...)
	var notify *herdr.AgentInfo
	found := false
	for i := range agents {
		a := &agents[i]
		if a.PaneID != paneID {
			continue
		}
		found = true
		a.Status = status
		key := AgentKey(a)
		if _, changed := m.tracker.Update(key, status, time.Now()); changed {
			a.LastActive = lastActive(a, m.tracker.Since(key))
			copyOfAgent := *a
			notify = &copyOfAgent
		}
		break
	}
	if found {
		m.agents = agents
	}
	m.mu.Unlock()
	if !found {
		return
	}

	// Notify outside the lock: ntfy posts can take seconds.
	if notify != nil {
		m.notifyTransition(notify)
	}
	m.hub.Broadcast(Event{Type: "agents", Agents: agents})
}

func (m *Manager) notifyTransition(a *herdr.AgentInfo) {
	ref := LocalRef(a)
	m.dispatch.Observe(a, ref)
	if !m.dispatch.Wants(a, ref) {
		return // skip resolving the session path for nothing
	}
	m.dispatch.Notify(a, ref, localLatest(a, m.Agents()))
}

// resubscribe rebuilds the subscription set from the current agent list:
// global pane events plus per-pane agent_status_changed for every live pane.
// herdr fixes the subscription set per connection, so a changed pane set
// requires a fresh connection (Stream.Start handles the swap).
func (m *Manager) resubscribe(ctx context.Context) {
	agents, err := m.client.ListAgents(ctx)
	if err != nil {
		log.Printf("resubscribe: list agents: %v", err)
		return
	}
	seen := map[string]bool{}
	subs := herdr.GlobalSubscriptions()
	for _, a := range agents {
		if !seen[a.PaneID] {
			seen[a.PaneID] = true
			subs = append(subs, herdr.StatusSubscription(a.PaneID))
		}
	}
	m.stream.Start(subs)
}

// Agents returns the current cached agent list.
func (m *Manager) Agents() []herdr.AgentInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]herdr.AgentInfo(nil), m.agents...)
}

// Event is a message broadcast to SSE clients.
type Event struct {
	Type   string            `json:"type"` // "agents" | "resync"
	Reason string            `json:"reason,omitempty"`
	Agents []herdr.AgentInfo `json:"agents,omitempty"`
	// Hosts is the per-host status (this instance first) on "agents"
	// events sent to browsers.
	Hosts []HostStatus `json:"hosts,omitempty"`
	// Version is the serving instance's app version, stamped on "agents"
	// events as they are written to a browser.
	Version string `json:"version,omitempty"`
}

// HostStatus is one host's health as seen by this instance.
type HostStatus struct {
	Name string `json:"name"`
	// Local marks this instance's own herdr; the others are peers.
	Local bool `json:"local,omitempty"`
	// Status is "ok", "connecting" (not reached yet) or "down".
	Status string `json:"status"`
	// Error is the last connection error of a peer that is not ok.
	Error string `json:"error,omitempty"`
	// LastSeen is when the peer last sent anything (event or ping).
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Agents   int        `json:"agents"`
}

// Hub fans out Events to connected SSE clients.
type Hub struct {
	mu      sync.Mutex
	clients map[chan Event]struct{}
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{clients: map[chan Event]struct{}{}}
}

// Subscribe registers a client and returns its channel plus a cancel func.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Broadcast delivers ev to every client, dropping it for clients that are
// too slow to keep up (the phone client refetches on reconnect).
func (h *Hub) Broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- ev:
		default:
			// Slow consumer: drop rather than block. If this is the only
			// symptom the client sees, its EventSource reconnect will
			// recover state via the initial agents event.
		}
	}
}
