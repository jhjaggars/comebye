package push

import (
	"sync"
	"time"
)

// DefaultPresenceTTL is how long a "viewing pane P" report suppresses
// pushes for P unless refreshed. The client refreshes every 15s while
// visible and clears it on hide, so this only matters when the hide
// report is lost (the phone locked mid-request): it bounds how long a
// stale report can swallow notifications.
const DefaultPresenceTTL = 45 * time.Second

// maxPresenceClients bounds the presence table (one entry per open tab).
const maxPresenceClients = 256

type presenceEntry struct {
	pane    string
	visible bool
	expires time.Time
}

// Presence tracks which agent each open app instance is showing, so a
// notification for an agent the user is looking at right now is skipped.
// This has to happen on the server: iOS requires every push to display a
// notification, so the service worker cannot drop it.
type Presence struct {
	TTL time.Duration
	now func() time.Time

	mu      sync.Mutex
	clients map[string]presenceEntry
}

// NewPresence returns an empty tracker with DefaultPresenceTTL.
func NewPresence() *Presence {
	return &Presence{TTL: DefaultPresenceTTL, now: time.Now, clients: map[string]presenceEntry{}}
}

// Update records what client (a random per-page id) is showing. An empty
// pane or visible=false clears it.
func (p *Presence) Update(client, pane string, visible bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for id, e := range p.clients {
		if !now.Before(e.expires) {
			delete(p.clients, id)
		}
	}
	if pane == "" || !visible {
		delete(p.clients, client)
		return
	}
	if _, ok := p.clients[client]; !ok && len(p.clients) >= maxPresenceClients {
		return
	}
	p.clients[client] = presenceEntry{pane: pane, visible: true, expires: now.Add(p.TTL)}
}

// Viewing reports whether some client has pane open and visible.
func (p *Presence) Viewing(pane string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, e := range p.clients {
		if e.visible && e.pane == pane && now.Before(e.expires) {
			return true
		}
	}
	return false
}
