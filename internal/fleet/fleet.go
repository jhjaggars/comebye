package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
	"github.com/jhjaggars/herdr-phone/internal/state"
	"github.com/jhjaggars/herdr-phone/internal/transcript"
)

// Host status values reported in state.HostStatus.
const (
	StatusOK         = "ok"
	StatusConnecting = "connecting"
	StatusDown       = "down"
)

// Options tunes a Fleet. Zero values pick the defaults.
type Options struct {
	// Dispatcher sends notifications for peer agents. Default: the local
	// manager's, so throttling and presence are shared with local agents.
	Dispatcher *state.Dispatcher
	// Client makes the requests to peers. Default: no redirects, no
	// proxy from the environment.
	Client *http.Client
	// ConnectTimeout bounds how long a peer may take to answer the event
	// stream request (default 10s).
	ConnectTimeout time.Duration
	// StaleAfter marks a peer down when its stream has been silent this
	// long. Peers send a ping every 15s (default 45s, three missed pings).
	StaleAfter time.Duration
	// MinBackoff and MaxBackoff bound the reconnect delay (1s, 30s).
	MinBackoff, MaxBackoff time.Duration
	// MaxResponse caps a proxied response body (default 8 MiB).
	MaxResponse int64
	// MaxEvent caps one event-stream message from a peer (default 8 MiB).
	MaxEvent int
	// ExcerptConcurrency bounds concurrent transcript fetches for
	// notification excerpts (default 4); beyond it a notification goes out
	// without an excerpt.
	ExcerptConcurrency int
	// Logf logs (default log.Printf).
	Logf func(format string, args ...any)
}

func (o *Options) defaults() {
	if o.Client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil // tailnet names are reached directly
		o.Client = &http.Client{
			Transport: tr,
			// Never follow redirects: a peer must not be able to point
			// the hub somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 10 * time.Second
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 45 * time.Second
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = max(30*time.Second, o.MinBackoff)
	}
	if o.MaxResponse <= 0 {
		o.MaxResponse = 8 << 20
	}
	if o.MaxEvent <= 0 {
		o.MaxEvent = 8 << 20
	}
	if o.ExcerptConcurrency <= 0 {
		o.ExcerptConcurrency = 4
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
}

// userAgent identifies hub requests in peer logs.
const userAgent = "herdr-phone-hub/1"

// Fleet is the merged view of the local host and its peers. With no peers
// it is a thin pass-through over the local state manager.
type Fleet struct {
	local     *state.Manager
	localName string
	opts      Options
	peers     []*peer
	byName    map[string]*peer
	hub       *state.Hub
	bmu       sync.Mutex // serializes broadcasts so snapshots go out in order
	excerpts  chan struct{}
}

// New builds a fleet over local (may be nil in tests) named localName.
// It sets the manager's host name, so call it before the manager runs.
func New(local *state.Manager, localName string, peers []Peer, opts Options) *Fleet {
	if opts.Dispatcher == nil && local != nil {
		opts.Dispatcher = local.Dispatcher()
	}
	opts.defaults()
	f := &Fleet{
		local:     local,
		localName: localName,
		opts:      opts,
		byName:    map[string]*peer{},
		hub:       state.NewHub(),
		excerpts:  make(chan struct{}, opts.ExcerptConcurrency),
	}
	if local != nil {
		local.SetHost(localName)
	}
	for _, cfg := range peers {
		p := &peer{f: f, name: cfg.Name, base: cfg.URL, status: StatusConnecting, tracker: state.NewTracker()}
		f.peers = append(f.peers, p)
		f.byName[cfg.Name] = p
	}
	return f
}

// LocalName is this instance's host name.
func (f *Fleet) LocalName() string { return f.localName }

// HasPeers reports whether this instance is a hub.
func (f *Fleet) HasPeers() bool { return len(f.peers) > 0 }

// HasPeer reports whether name is a configured peer.
func (f *Fleet) HasPeer(name string) bool { return f.byName[name] != nil }

// Hub is the broadcast hub for browser SSE clients: merged "agents"
// snapshots (with host status) and the local manager's "resync" events.
func (f *Fleet) Hub() *state.Hub { return f.hub }

// Agents is the merged agent list: local agents first, then each peer's
// last known list. It never touches the network.
func (f *Fleet) Agents() []herdr.AgentInfo {
	var out []herdr.AgentInfo
	if f.local != nil {
		out = f.local.Agents()
	}
	for _, p := range f.peers {
		p.mu.Lock()
		out = append(out, p.agents...)
		p.mu.Unlock()
	}
	if out == nil {
		out = []herdr.AgentInfo{}
	}
	return out
}

// Hosts reports every host's status, the local one first.
func (f *Fleet) Hosts() []state.HostStatus {
	n := 0
	if f.local != nil {
		n = len(f.local.Agents())
	}
	out := []state.HostStatus{{Name: f.localName, Local: true, Status: StatusOK, Agents: n}}
	for _, p := range f.peers {
		out = append(out, p.hostStatus())
	}
	return out
}

// Snapshot is the "agents" event sent to browsers.
func (f *Fleet) Snapshot() state.Event {
	return state.Event{Type: "agents", Agents: f.Agents(), Hosts: f.Hosts()}
}

func (f *Fleet) broadcast() {
	f.bmu.Lock()
	defer f.bmu.Unlock()
	f.hub.Broadcast(f.Snapshot())
}

// Run relays local updates and keeps every peer subscription alive until
// ctx is done. A peer that is down or hangs only affects its own agents.
func (f *Fleet) Run(ctx context.Context) {
	var events <-chan state.Event
	if f.local != nil {
		ch, cancel := f.local.Hub().Subscribe()
		defer cancel()
		events = ch
	}
	for _, p := range f.peers {
		go p.run(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Type == "agents" {
				f.broadcast()
			} else {
				f.hub.Broadcast(ev)
			}
		}
	}
}

// peer is the hub's connection to one peer instance.
type peer struct {
	f    *Fleet
	name string
	base *url.URL

	mu       sync.Mutex
	status   string
	lastErr  string
	lastSeen time.Time
	agents   []herdr.AgentInfo
	tracker  *state.Tracker
	checked  bool // the peer's push setting was checked
	capped   bool // the last snapshot was cut to maxPeerAgents (logged once)
}

// maxPeerAgents caps the agents taken from one peer snapshot. A real host
// runs a handful; the cap bounds what a misbehaving peer can make the hub
// track, broadcast and notify for.
const maxPeerAgents = 256

// maxPaneID bounds a peer agent's pane id (herdr's look like "wS:p1").
const maxPaneID = 128

// validStatus reports whether s is one of herdr's agent statuses.
func validStatus(s herdr.AgentStatus) bool {
	switch s {
	case herdr.StatusIdle, herdr.StatusWorking, herdr.StatusBlocked, herdr.StatusDone, herdr.StatusUnknown:
		return true
	}
	return false
}

func (p *peer) hostStatus() state.HostStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	hs := state.HostStatus{Name: p.name, Status: p.status, Agents: len(p.agents)}
	if p.status != StatusOK {
		hs.Error = p.lastErr
	}
	if !p.lastSeen.IsZero() {
		t := p.lastSeen
		hs.LastSeen = &t
	}
	return hs
}

// endpoint builds a URL on the peer from fixed path segments; each
// segment is escaped, so nothing can climb out of /api/.
func (p *peer) endpoint(segments ...string) *url.URL {
	u := *p.base
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	u.RawPath = b.String()
	u.Path, _ = url.PathUnescape(u.RawPath)
	return &u
}

var errStale = errors.New("stream went silent (missed pings)")

// run keeps the event subscription alive, reconnecting with backoff.
func (p *peer) run(ctx context.Context) {
	o := &p.f.opts
	backoff := o.MinBackoff
	for {
		start := time.Now()
		delivered, err := p.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := backoff
		if delivered && !errors.Is(err, errStale) && time.Since(start) > 5*time.Second {
			// A healthy stream ended (peer restart, network blip): retry
			// right away and keep its agents until a reconnect fails.
			backoff = o.MinBackoff
			wait = min(200*time.Millisecond, o.MinBackoff)
		} else {
			p.markDown(err)
			backoff = min(backoff*2, o.MaxBackoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// stream runs one event-stream connection. delivered reports whether any
// event arrived.
func (p *peer) stream(ctx context.Context) (delivered bool, err error) {
	o := &p.f.opts
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// One watchdog covers both phases: no response headers within
	// ConnectTimeout, then no event or ping within StaleAfter.
	var fired atomic.Bool
	watchdog := time.AfterFunc(o.ConnectTimeout, func() { fired.Store(true); cancel() })
	defer watchdog.Stop()

	// full=1: herdr's full agent records (browsers get a slim view), which
	// the hub needs for notification excerpts.
	u := p.endpoint("api", "events")
	u.RawQuery = "full=1"
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	resp, err := o.Client.Do(req)
	if err != nil {
		if fired.Load() {
			return false, fmt.Errorf("no response within %s", o.ConnectTimeout)
		}
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, statusError(resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return false, fmt.Errorf("not an event stream (Content-Type %q): is this a herdr-phone URL?", ct)
	}
	watchdog.Reset(o.StaleAfter)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), o.MaxEvent)
	var name string
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if name != "" || data.Len() > 0 {
				watchdog.Reset(o.StaleAfter)
				p.handle(name, data.String())
				delivered = true
			}
			name = ""
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			if data.Len()+len(value) > o.MaxEvent {
				return delivered, errors.New("event too large")
			}
			data.WriteString(value)
		}
	}
	if fired.Load() {
		return delivered, fmt.Errorf("%w: nothing for %s", errStale, o.StaleAfter)
	}
	if err := sc.Err(); err != nil {
		return delivered, err
	}
	return delivered, errors.New("stream closed by peer")
}

// statusError explains a non-200 answer from a peer.
func statusError(code int) error {
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Errorf("HTTP %d: the peer rejected the hub; add the hub device owner's Tailscale login to the peer's -ts-allow-login", code)
	}
	return fmt.Errorf("HTTP %d from peer", code)
}

// handle applies one event from the peer's stream.
func (p *peer) handle(name, data string) {
	if name != "agents" {
		p.seen() // ping, resync, or anything newer: the peer is alive
		return
	}
	var ev state.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		p.f.opts.Logf("peer %s: bad agents event: %v", p.name, err)
		p.seen()
		return
	}
	p.setAgents(ev.Agents, ev.Hosts)
}

// seen records liveness without new agent data.
func (p *peer) seen() {
	p.mu.Lock()
	changed := p.status != StatusOK
	p.status, p.lastErr, p.lastSeen = StatusOK, "", time.Now()
	p.mu.Unlock()
	if changed {
		p.f.opts.Logf("peer %s: connected", p.name)
		p.f.broadcast()
	}
}

// setAgents replaces the peer's agents with a fresh snapshot and sends
// notifications for transitions.
func (p *peer) setAgents(agents []herdr.AgentInfo, hosts []state.HostStatus) {
	// Only the peer's own agents: if the peer is itself a hub, its list
	// includes other hosts, which must not be re-aggregated here.
	own := ""
	for _, h := range hosts {
		if h.Local {
			own = h.Name
		}
	}
	kept := make([]herdr.AgentInfo, 0, min(len(agents), maxPeerAgents))
	dropped := false
	for _, a := range agents {
		if own != "" && a.Host != "" && a.Host != own {
			continue
		}
		if a.PaneID == "" || len(a.PaneID) > maxPaneID {
			continue
		}
		if len(kept) == maxPeerAgents {
			dropped = true
			break
		}
		// The browser and the dispatcher key off the status: only
		// herdr's values get through.
		if !validStatus(a.Status) {
			a.Status = herdr.StatusUnknown
		}
		a.Host = p.name
		a.ID = state.QualifiedID(p.name, a.PaneID)
		kept = append(kept, a)
	}
	now := time.Now()
	p.mu.Lock()
	wasOK := p.status == StatusOK
	transitions := p.tracker.Observe(kept, now)
	p.agents = kept
	p.status, p.lastErr, p.lastSeen = StatusOK, "", now
	check := !p.checked
	p.checked = true
	logCap := dropped && !p.capped
	p.capped = dropped
	p.mu.Unlock()
	if logCap {
		p.f.opts.Logf("peer %s: more than %d agents, ignoring the rest", p.name, maxPeerAgents)
	}
	if !wasOK {
		p.f.opts.Logf("peer %s: connected (%d agents)", p.name, len(kept))
	}
	if check {
		go p.checkPush()
	}
	p.f.broadcast()
	for _, t := range transitions {
		p.f.notify(p, t.Agent)
	}
}

// markDown drops the peer's agents and forgets its statuses, so the next
// snapshot after reconnecting is a fresh baseline (transitions missed while
// it was down are not notified).
func (p *peer) markDown(err error) {
	msg := "unreachable"
	if err != nil {
		msg = err.Error()
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	p.mu.Lock()
	changed := p.status != StatusDown || p.lastErr != msg
	wasDown := p.status == StatusDown
	p.status, p.lastErr, p.agents = StatusDown, msg, nil
	p.tracker.Reset()
	p.mu.Unlock()
	if !wasDown {
		p.f.opts.Logf("peer %s: down: %s", p.name, msg)
	}
	if changed {
		p.f.broadcast()
	}
}

// checkPush warns when a peer has Web Push on: a browser that subscribed
// on the peer's own URL would get every notification twice (from the peer
// and from the hub).
func (p *peer) checkPush() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, code, err := p.get(ctx, p.endpoint("api", "push", "key"), 64<<10)
	if err != nil || code != http.StatusOK {
		return
	}
	var k struct {
		Enabled bool `json:"enabled"`
	}
	if json.Unmarshal(body, &k) == nil && k.Enabled {
		p.f.opts.Logf("peer %s: Web Push is enabled there; run peers with -push=false (or never subscribe on the peer's URL), or a phone subscribed on both gets duplicate notifications", p.name)
	}
}

// get fetches a small JSON resource from the peer.
func (p *peer) get(ctx context.Context, u *url.URL, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.f.opts.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(body)) > limit {
		return nil, resp.StatusCode, errors.New("response too large")
	}
	return body, resp.StatusCode, nil
}

// notify hands a peer agent's transition to the shared dispatcher. The
// excerpt comes from the peer's transcript endpoint, fetched off the
// stream goroutine and bounded in time, size and concurrency.
func (f *Fleet) notify(p *peer, a herdr.AgentInfo) {
	d := f.opts.Dispatcher
	ref := state.PeerRef(&a)
	d.Observe(&a, ref)
	if d == nil || !d.Wants(&a, ref) {
		return
	}
	var latest state.LatestFunc
	if s := a.AgentSession; s != nil && s.Kind == "path" && s.Value != "" {
		pane := a.PaneID
		latest = func(ctx context.Context) *transcript.Latest {
			select {
			case f.excerpts <- struct{}{}:
				defer func() { <-f.excerpts }()
			default:
				return nil // too many in flight: notify without an excerpt
			}
			return p.fetchLatest(ctx, pane)
		}
	}
	go d.Notify(&a, ref, latest)
}

// fetchLatest reads the tail of a peer agent's transcript.
func (p *peer) fetchLatest(ctx context.Context, pane string) *transcript.Latest {
	u := p.endpoint("api", "agents", pane, "transcript")
	u.RawQuery = "limit=20"
	body, code, err := p.get(ctx, u, 2<<20)
	if err != nil || code != http.StatusOK {
		return nil
	}
	var t transcript.Transcript
	if err := json.Unmarshal(body, &t); err != nil {
		return nil
	}
	return transcript.LatestFromMessages(t.Messages)
}
