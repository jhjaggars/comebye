package state

import (
	"strings"
	"sync"
	"time"
)

// The "changed" event tells a browser that one store it shows has new data,
// so the screen that shows it fetches it again instead of polling. The event
// carries only the kind, never the data: the browser reads the data through
// the same API as before, with the same access checks.
//
// Kinds: ledger (handoffs, marks, roles, archived), todos, pages, parked,
// releases, usage, transcript:<agent id> and job:<id> (a provision job or a
// sign-in).
const (
	ChangedLedger   = "ledger"
	ChangedTodos    = "todos"
	ChangedPages    = "pages"
	ChangedParked   = "parked"
	ChangedReleases = "releases"
	ChangedUsage    = "usage"
	// ChangedTranscriptPrefix + an agent's qualified id ("host/pane").
	ChangedTranscriptPrefix = "transcript:"
	// ChangedJobPrefix + a job's id.
	ChangedJobPrefix = "job:"
)

// ChangedKinds are the fixed kinds (the prefixed ones carry an id).
var ChangedKinds = []string{ChangedLedger, ChangedTodos, ChangedPages, ChangedParked, ChangedReleases, ChangedUsage}

// ValidChangedKind reports whether kind is one the browser knows, with a
// short, printable id for the prefixed kinds.
func ValidChangedKind(kind string) bool {
	for _, k := range ChangedKinds {
		if kind == k {
			return true
		}
	}
	for _, p := range []string{ChangedTranscriptPrefix, ChangedJobPrefix} {
		if id, ok := strings.CutPrefix(kind, p); ok {
			return id != "" && len(id) <= 160 && printable(id)
		}
	}
	return false
}

func printable(s string) bool {
	for _, r := range s {
		if r <= ' ' || r == 0x7f || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// ChangedDebounce is the shortest gap between two "changed" events of one
// kind: a burst of writes (a ledger pass that saves five times) is one
// event, sent at once, plus one at the end of the burst.
const ChangedDebounce = 250 * time.Millisecond

// Changes fans "changed" events out to the browsers' streams. It is apart
// from the agents hub so that the hub's own loops, which follow agent
// updates, never lose an agents event to a burst of these.
type Changes struct {
	hub    *Hub
	window time.Duration
	mu     sync.Mutex
	last   map[string]time.Time // when each kind last went out
	queued map[string]bool      // a trailing event is due for the kind
}

// NewChanges makes an empty fan-out with the default debounce.
func NewChanges() *Changes {
	return &Changes{hub: NewHub(), window: ChangedDebounce, last: map[string]time.Time{}, queued: map[string]bool{}}
}

// SetDebounce changes the debounce window (tests).
func (c *Changes) SetDebounce(d time.Duration) {
	c.mu.Lock()
	c.window = d
	c.mu.Unlock()
}

// Subscribe registers a stream; see Hub.Subscribe.
func (c *Changes) Subscribe() (<-chan Event, func()) { return c.hub.Subscribe() }

// Changed says kind has new data. The first call of a burst is sent at
// once; later calls within the window are folded into one trailing event.
// It never blocks and is safe from any goroutine, with any lock held.
func (c *Changes) Changed(kind string) {
	if c == nil || !ValidChangedKind(kind) {
		return
	}
	c.mu.Lock()
	if c.queued[kind] {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	gap := now.Sub(c.last[kind])
	if gap >= c.window {
		c.last[kind] = now
		c.pruneLocked(now)
		c.mu.Unlock()
		c.send(kind)
		return
	}
	c.queued[kind] = true
	wait := c.window - gap
	c.mu.Unlock()
	time.AfterFunc(wait, func() {
		c.mu.Lock()
		delete(c.queued, kind)
		c.last[kind] = time.Now()
		c.mu.Unlock()
		c.send(kind)
	})
}

// pruneLocked forgets kinds that went out long ago, so the per-agent and
// per-job kinds do not pile up.
func (c *Changes) pruneLocked(now time.Time) {
	if len(c.last) < 256 {
		return
	}
	for k, t := range c.last {
		if now.Sub(t) >= c.window && !c.queued[k] {
			delete(c.last, k)
		}
	}
}

func (c *Changes) send(kind string) {
	c.hub.Broadcast(Event{Type: "changed", Kind: kind})
}
