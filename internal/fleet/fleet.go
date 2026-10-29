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

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
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
	// ProbeEvery is how often the hub health-checks each peer with
	// GET /api/whoami and refreshes the failure class of a down peer
	// (default 3m).
	ProbeEvery time.Duration
	// ProbeTimeout bounds one health probe (default 10s).
	ProbeTimeout time.Duration
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
	if o.ProbeEvery <= 0 {
		o.ProbeEvery = 3 * time.Minute
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 10 * time.Second
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
const userAgent = "comebye-hub/1"

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
		p := &peer{f: f, name: cfg.Name, addrs: cfg.URLs, status: StatusConnecting, tracker: state.NewTracker()}
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
	go f.probeLoop(ctx)
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
	// addrs are the peer's addresses, tried in order by attempt.
	addrs []*url.URL

	mu        sync.Mutex
	status    string
	lastErr   string
	class     string // failure class while down: dns, tcp, tls, http
	since     time.Time
	addr      string // the address that last answered
	preferred int    // index into addrs of the last address that answered
	lastSeen  time.Time
	agents    []herdr.AgentInfo
	tracker   *state.Tracker
	checked   bool // the peer's push setting was checked
	capped    bool // the last snapshot was cut to maxPeerAgents (logged once)
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
	hs := state.HostStatus{Name: p.name, Status: p.status, Agents: len(p.agents), Class: p.class, Addr: p.addr}
	if p.status != StatusOK {
		hs.Error = p.lastErr
	}
	if !p.since.IsZero() {
		t := p.since
		hs.Since = &t
	}
	if !p.lastSeen.IsZero() {
		t := p.lastSeen
		hs.LastSeen = &t
	}
	return hs
}

// endpointOn builds a URL on base from fixed path segments; each
// segment is escaped, so nothing can climb out of /api/.
func endpointOn(base *url.URL, segments ...string) *url.URL {
	u := *base
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	u.RawPath = b.String()
	u.Path, _ = url.PathUnescape(u.RawPath)
	return &u
}

// attempt runs fn against each of the peer's addresses - the one that
// last answered first, then the rest in configured order - until one
// returns an HTTP response. A request that fails before a response
// (DNS, TCP, TLS) moves on to the next address, so a peer configured
// with a name plus a tailnet IP survives a rename. It returns the
// response, the host of the address that answered (or the last one
// tried), and the last transport error.
func (p *peer) attempt(fn func(base *url.URL) (*http.Response, error)) (*http.Response, string, error) {
	if len(p.addrs) == 0 {
		return nil, "", errors.New("no addresses configured")
	}
	p.mu.Lock()
	order := make([]int, 0, len(p.addrs))
	order = append(order, p.preferred)
	for i := range p.addrs {
		if i != p.preferred {
			order = append(order, i)
		}
	}
	p.mu.Unlock()
	var err error
	host := ""
	for _, i := range order {
		resp, ferr := fn(p.addrs[i])
		host = p.addrs[i].Host
		if ferr == nil {
			p.mu.Lock()
			first := p.preferred != i
			p.preferred, p.addr = i, host
			p.mu.Unlock()
			if first {
				p.f.opts.Logf("peer %s: using %s (address %d of %d)", p.name, host, i+1, len(p.addrs))
			}
			return resp, host, nil
		}
		err = ferr
	}
	return nil, host, err
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
	resp, _, err := p.attempt(func(base *url.URL) (*http.Response, error) {
		u := endpointOn(base, "api", "events")
		u.RawQuery = "full=1"
		req, err := http.NewRequestWithContext(sctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", userAgent)
		return o.Client.Do(req)
	})
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
		return false, fmt.Errorf("not an event stream (Content-Type %q): is this a comebye URL?", ct)
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

// statusMessage explains a non-2xx answer from a peer.
func statusMessage(code int) string {
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Sprintf("HTTP %d: the peer rejected the hub; add the hub device owner's Tailscale login to the peer's -ts-allow-login", code)
	}
	return fmt.Sprintf("HTTP %d from peer", code)
}

// statusError wraps a peer's status code for classification as http.
func statusError(code int) error { return peerHTTPError{code} }

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
	now := time.Now()
	p.status, p.lastErr, p.lastSeen = StatusOK, "", now
	if changed {
		p.since, p.class = now, ""
	}
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
	if !wasOK {
		p.since, p.class = now, ""
	}
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
// it was down are not notified). The error is classified (dns, tcp, tls,
// http) so the shown reason names the likely fix; the down-since time is
// kept across reconnect attempts.
func (p *peer) markDown(err error) {
	msg := "unreachable"
	class := ""
	if err != nil {
		class, msg = classifyError(err, "")
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	p.mu.Lock()
	changed := p.status != StatusDown || p.lastErr != msg
	wasDown := p.status == StatusDown
	now := time.Now()
	p.status, p.lastErr, p.agents = StatusDown, msg, nil
	p.class = class
	if !wasDown {
		p.since = now
	}
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
	body, code, err := p.get(ctx, []string{"api", "push", "key"}, nil, 64<<10)
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

// get fetches a small JSON resource from the peer, trying each of its
// addresses in order.
func (p *peer) get(ctx context.Context, segments []string, query url.Values, limit int64) ([]byte, int, error) {
	resp, _, err := p.attempt(func(base *url.URL) (*http.Response, error) {
		u := endpointOn(base, segments...)
		if len(query) > 0 {
			u.RawQuery = query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		return p.f.opts.Client.Do(req)
	})
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
	body, code, err := p.get(ctx, []string{"api", "agents", pane, "transcript"}, url.Values{"limit": {"20"}}, 2<<20)
	if err != nil || code != http.StatusOK {
		return nil
	}
	var t transcript.Transcript
	if err := json.Unmarshal(body, &t); err != nil {
		return nil
	}
	return transcript.LatestFromMessages(t.Messages)
}
