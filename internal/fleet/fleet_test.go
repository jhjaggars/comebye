package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/state"
)

// fakePeer is a minimal comebye instance: an SSE agents stream with
// pings, and the per-agent endpoints, recording every request.
type fakePeer struct {
	srv *httptest.Server

	mu       sync.Mutex
	agents   []herdr.AgentInfo
	hosts    []state.HostStatus
	silent   bool // accept event streams but never write
	pushOn   bool
	streams  map[chan string]struct{}
	requests []recorded
}

type recorded struct {
	method, path, query, body string
	header                    http.Header
}

func newFakePeer(t *testing.T, agents ...herdr.AgentInfo) *fakePeer {
	t.Helper()
	fp := &fakePeer{agents: agents, streams: map[chan string]struct{}{}}
	fp.srv = httptest.NewServer(http.HandlerFunc(fp.serve))
	t.Cleanup(fp.srv.Close)
	return fp
}

func (fp *fakePeer) url() *url.URL {
	u, _ := url.Parse(fp.srv.URL)
	return u
}

func (fp *fakePeer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	fp.mu.Lock()
	fp.requests = append(fp.requests, recorded{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Clone()})
	fp.mu.Unlock()
	path := r.URL.EscapedPath()
	switch {
	case path == "/api/events":
		fp.events(w, r)
	case path == "/api/push/key":
		fp.mu.Lock()
		on := fp.pushOn
		fp.mu.Unlock()
		fmt.Fprintf(w, `{"enabled":%v}`, on)
	case strings.HasSuffix(path, "/transcript"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"messages":[{"id":"a1","kind":"assistant","text":"Tests pass. Ship it?"},{"id":"t1","kind":"tool","tool":"bash","args":"git push"}],"total":2,"more":false}`)
	case strings.HasSuffix(path, "/dialog"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"dialog":{"kind":"pi_gate","fingerprint":"abc"}}`)
	case strings.HasSuffix(path, "/answer"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"code":"dialog_changed","dialog":null}`)
	case strings.HasSuffix(path, "/keys"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	case strings.HasSuffix(path, "/prompt"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"code":"agent_blocked","message":"agent is waiting for input"}`)
	case path == "/api/agents/big/output":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"text":%q}`, strings.Repeat("x", 4096))
	case path == "/api/agents/denied/output":
		w.WriteHeader(http.StatusUnauthorized)
	case path == "/api/agents/moved/output":
		w.Header().Set("Location", "http://169.254.169.254/")
		w.WriteHeader(http.StatusFound)
	case strings.HasSuffix(path, "/output"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"text":"screen"}`)
	case path == "/api/launch/options":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"enabled":true,"kinds":["pi"]}`)
	case path == "/api/launch/dirs":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q,"entries":[]}`, r.URL.Query().Get("path"))
	case path == "/api/launch":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"code":"agent_name_taken","message":"an agent named x is already running"}`)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func (fp *fakePeer) snapshot() string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	b, _ := json.Marshal(state.Event{Type: "agents", Agents: fp.agents, Hosts: fp.hosts})
	return "event: agents\ndata: " + string(b) + "\n\n"
}

func (fp *fakePeer) events(w http.ResponseWriter, r *http.Request) {
	fp.mu.Lock()
	silent := fp.silent
	fp.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	if silent {
		<-r.Context().Done()
		return
	}
	ch := make(chan string, 16)
	fp.mu.Lock()
	fp.streams[ch] = struct{}{}
	fp.mu.Unlock()
	defer func() {
		fp.mu.Lock()
		delete(fp.streams, ch)
		fp.mu.Unlock()
	}()
	io.WriteString(w, fp.snapshot())
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return // dropped by the test
			}
			io.WriteString(w, msg)
			w.(http.Flusher).Flush()
		}
	}
}

// send writes msg to every open stream.
func (fp *fakePeer) send(msg string) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for ch := range fp.streams {
		ch <- msg
	}
}

func (fp *fakePeer) setAgents(agents ...herdr.AgentInfo) {
	fp.mu.Lock()
	fp.agents = agents
	fp.mu.Unlock()
	fp.send(fp.snapshot())
}

// dropStreams closes every open stream and makes new ones silent.
func (fp *fakePeer) goSilent(on bool) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.silent = on
	if on {
		for ch := range fp.streams {
			close(ch)
			delete(fp.streams, ch)
		}
	}
}

func (fp *fakePeer) requestsTo(suffix string) []recorded {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	var out []recorded
	for _, r := range fp.requests {
		if strings.HasSuffix(r.path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func agent(pane, name string, status herdr.AgentStatus) herdr.AgentInfo {
	return herdr.AgentInfo{PaneID: pane, Name: name, Status: status, CWD: "/home/u/code/repo",
		AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: "/peer/only/" + pane + ".jsonl"}}
}

func fastOpts(d *state.Dispatcher) Options {
	return Options{
		Dispatcher:     d,
		ConnectTimeout: 300 * time.Millisecond,
		StaleAfter:     400 * time.Millisecond,
		MinBackoff:     50 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		MaxResponse:    1024,
		Logf:           func(string, ...any) {},
	}
}

func startFleet(t *testing.T, opts Options, peers ...Peer) *Fleet {
	t.Helper()
	f := New(nil, "desk", peers, opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	return f
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hostByName(f *Fleet, name string) state.HostStatus {
	for _, h := range f.Hosts() {
		if h.Name == name {
			return h
		}
	}
	return state.HostStatus{}
}

func TestAggregatesPeerAgents(t *testing.T) {
	fp := newFakePeer(t, agent("wS:p1", "fixer", herdr.StatusWorking))
	// The peer is itself a hub reporting another host's agent: not re-aggregated.
	other := agent("wX:p9", "elsewhere", herdr.StatusIdle)
	other.Host = "third"
	own := agent("wS:p2", "mine", herdr.StatusIdle)
	own.Host, own.ID = "kube-self", "kube-self/wS:p2"
	fp.agents = append(fp.agents, own, other)
	fp.hosts = []state.HostStatus{{Name: "kube-self", Local: true, Status: "ok"}, {Name: "third", Status: "ok"}}

	f := startFleet(t, fastOpts(nil), Peer{Name: "kube", URL: fp.url()})
	events, cancel := f.Hub().Subscribe()
	defer cancel()
	waitFor(t, "peer agents", func() bool { return len(f.Agents()) == 2 })
	got := f.Agents()
	if got[0].ID != "kube/wS:p1" || got[0].Host != "kube" || got[1].ID != "kube/wS:p2" || got[1].Host != "kube" {
		t.Fatalf("agents = %+v", got)
	}
	hosts := f.Hosts()
	if len(hosts) != 2 || !hosts[0].Local || hosts[0].Name != "desk" || hosts[1].Status != StatusOK || hosts[1].Agents != 2 || hosts[1].LastSeen == nil {
		t.Fatalf("hosts = %+v", hosts)
	}
	// The merged list is broadcast with host status.
	select {
	case ev := <-events:
		if ev.Type != "agents" || len(ev.Hosts) != 2 {
			t.Fatalf("broadcast = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no broadcast")
	}
	// The request carried no browser identity and asked for SSE.
	r := fp.requestsTo("/api/events")[0]
	if r.header.Get("Accept") != "text/event-stream" || r.header.Get("User-Agent") != userAgent {
		t.Fatalf("event stream request headers = %v", r.header)
	}
}

func TestPeerDownAndUp(t *testing.T) {
	fp := newFakePeer(t, agent("wS:p1", "fixer", herdr.StatusWorking))
	f := startFleet(t, fastOpts(nil), Peer{Name: "kube", URL: fp.url()})
	waitFor(t, "connected", func() bool { return hostByName(f, "kube").Status == StatusOK })

	// Pings keep a quiet stream alive past StaleAfter.
	for range 6 {
		time.Sleep(100 * time.Millisecond)
		fp.send("event: ping\ndata: {}\n\n")
	}
	if h := hostByName(f, "kube"); h.Status != StatusOK {
		t.Fatalf("pinged peer marked %s: %s", h.Status, h.Error)
	}

	// Missed pings: the stream goes silent, the peer is marked down and
	// its agents disappear, with the last-seen time kept.
	fp.goSilent(true)
	waitFor(t, "down", func() bool { return hostByName(f, "kube").Status == StatusDown })
	h := hostByName(f, "kube")
	if len(f.Agents()) != 0 || h.LastSeen == nil || h.Error == "" {
		t.Fatalf("down peer: agents=%d host=%+v", len(f.Agents()), h)
	}

	// It comes back on reconnect.
	fp.goSilent(false)
	waitFor(t, "back up", func() bool { return hostByName(f, "kube").Status == StatusOK && len(f.Agents()) == 1 })
}

func TestUnreachableAndHangingPeersAreIsolated(t *testing.T) {
	good := newFakePeer(t, agent("wS:p1", "fixer", herdr.StatusIdle))
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // accepts, never answers
	}))
	defer hang.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL, _ := url.Parse(closed.URL)
	closed.Close()
	hangURL, _ := url.Parse(hang.URL)

	f := startFleet(t, fastOpts(nil),
		Peer{Name: "hang", URL: hangURL},
		Peer{Name: "gone", URL: closedURL},
		Peer{Name: "good", URL: good.url()})
	waitFor(t, "good peer", func() bool { return hostByName(f, "good").Status == StatusOK })
	waitFor(t, "hanging peer down", func() bool { return hostByName(f, "hang").Status == StatusDown })
	waitFor(t, "closed peer down", func() bool { return hostByName(f, "gone").Status == StatusDown })
	if !strings.Contains(hostByName(f, "hang").Error, "no response") {
		t.Errorf("hang error = %q", hostByName(f, "hang").Error)
	}

	start := time.Now()
	if a := f.Agents(); len(a) != 1 || a[0].Host != "good" {
		t.Fatalf("agents = %+v", a)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Agents blocked on a peer")
	}

	// Proxying to the hanging peer times out without affecting the others.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("GET", "/x", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	f.Proxy(w, r, "hang", "wS:p1", "output")
	if w.Code != http.StatusGatewayTimeout || !strings.Contains(w.Body.String(), "host_timeout") {
		t.Fatalf("hang proxy = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	f.Proxy(w, httptest.NewRequest("GET", "/x", nil), "gone", "wS:p1", "output")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "host_unreachable") {
		t.Fatalf("gone proxy = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	f.Proxy(w, httptest.NewRequest("GET", "/x", nil), "good", "wS:p1", "output")
	if w.Code != http.StatusOK {
		t.Fatalf("good proxy = %d %s", w.Code, w.Body)
	}
}

func TestProxy(t *testing.T) {
	fp := newFakePeer(t)
	f := New(nil, "desk", []Peer{{Name: "kube", URL: fp.url()}}, fastOpts(nil))

	call := func(method, target, op, query, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/x?"+query, strings.NewReader(body))
		// Browser headers that must never reach the peer.
		r.Header.Set("Cookie", "secret=1")
		r.Header.Set("Authorization", "Bearer x")
		r.Header.Set("Tailscale-User-Login", "phone@example.com")
		r.Header.Set("Origin", "https://hub.ts.net")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Forwarded-For", "1.2.3.4")
		w := httptest.NewRecorder()
		f.Proxy(w, r, "kube", target, op)
		return w
	}

	// Keys: forwarded as JSON, response passed through.
	w := call("POST", "wS:p1", "keys", "", `{"keys":["esc"]}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"ok":true}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("keys = %d %q %v", w.Code, w.Body, w.Header())
	}
	reqs := fp.requestsTo("/keys")
	if len(reqs) != 1 {
		t.Fatalf("peer got %d keys requests", len(reqs))
	}
	k := reqs[0]
	if k.method != "POST" || k.path != "/api/agents/wS:p1/keys" || k.body != `{"keys":["esc"]}` || k.header.Get("Content-Type") != "application/json" {
		t.Fatalf("forwarded keys = %+v", k)
	}
	for _, h := range []string{"Cookie", "Authorization", "Tailscale-User-Login", "Origin", "Sec-Fetch-Site", "X-Forwarded-For"} {
		if k.header.Get(h) != "" {
			t.Errorf("browser header %s was forwarded", h)
		}
	}

	// Error passthrough: status and JSON body (agent_blocked -> 409).
	w = call("POST", "wS:p1", "prompt", "", `{"text":"hi"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "agent_blocked") {
		t.Fatalf("prompt = %d %s", w.Code, w.Body)
	}

	// Only whitelisted query parameters pass.
	call("GET", "wS:p1", "transcript", "limit=5&skip=2&evil=1&url=http://x", "")
	if q := fp.requestsTo("/transcript")[0].query; q != "limit=5&skip=2" {
		t.Fatalf("transcript query = %q", q)
	}

	// The target stays one path segment; nothing escapes /api/agents/.
	call("GET", "a/../../push/test", "output", "", "")
	if p := fp.requestsTo("/output"); len(p) != 1 || p[0].path != "/api/agents/a%2F..%2F..%2Fpush%2Ftest/output" {
		t.Fatalf("output path = %+v", p)
	}
	for _, target := range []string{"..", ".", ""} {
		if w := call("GET", target, "output", "", ""); w.Code != http.StatusBadRequest {
			t.Errorf("target %q: %d", target, w.Code)
		}
	}
	// Unknown operations, wrong methods and unknown hosts are refused locally.
	if w := call("GET", "wS:p1", "events", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("op events: %d", w.Code)
	}
	if w := call("GET", "wS:p1", "keys", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET keys: %d", w.Code)
	}
	w = httptest.NewRecorder()
	f.Proxy(w, httptest.NewRequest("GET", "/x", nil), "nope", "wS:p1", "output")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_host") {
		t.Errorf("unknown host: %d %s", w.Code, w.Body)
	}
	if w := call("POST", "wS:p1", "keys", "", `not json`); w.Code != http.StatusBadRequest {
		t.Errorf("non-JSON body: %d", w.Code)
	}
	if n := len(fp.requestsTo("/keys")); n != 1 {
		t.Errorf("refused calls reached the peer: %d keys requests", n)
	}

	// Response size cap, peer auth failures and redirects.
	if w := call("GET", "big", "output", "", ""); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "too_large") {
		t.Errorf("big: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "denied", "output", "", ""); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "peer_unauthorized") {
		t.Errorf("denied: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "moved", "output", "", ""); w.Code != http.StatusBadGateway || w.Header().Get("Location") != "" {
		t.Errorf("redirect: %d %v", w.Code, w.Header())
	}
	for _, r := range fp.requestsTo("") {
		if !strings.HasPrefix(r.path, "/api/agents/") {
			t.Errorf("peer got a request outside /api/agents/: %s", r.path)
		}
	}
}

func TestProxyHost(t *testing.T) {
	fp := newFakePeer(t)
	opts := fastOpts(nil)
	opts.MaxResponse = 8 << 20
	f := New(nil, "desk", []Peer{{Name: "kube", URL: fp.url()}}, opts)
	call := func(method, op, query, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/x?"+query, strings.NewReader(body))
		r.Header.Set("Tailscale-User-Login", "phone@example.com")
		r.Header.Set("Cookie", "secret=1")
		r.Header.Set("Origin", "https://hub.ts.net")
		w := httptest.NewRecorder()
		f.ProxyHost(w, r, "kube", op)
		return w
	}
	if w := call("GET", "launch-options", "path=/etc", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("options = %d %s", w.Code, w.Body)
	}
	if o := fp.requestsTo("/launch/options"); len(o) != 1 || o[0].path != "/api/launch/options" || o[0].query != "" {
		t.Fatalf("forwarded options = %+v", o)
	}
	// Only ?path= passes to the directory listing.
	if w := call("GET", "launch-dirs", "path=/home/me/code&evil=1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"/home/me/code"`) {
		t.Fatalf("dirs = %d %s", w.Code, w.Body)
	}
	if d := fp.requestsTo("/launch/dirs"); len(d) != 1 || d[0].query != "path=%2Fhome%2Fme%2Fcode" {
		t.Fatalf("forwarded dirs = %+v", d)
	}
	// The launch body is forwarded as is, without browser headers; the
	// peer's error passes through.
	body := `{"dir":"/home/me/code/x","kind":"pi","prompt":"` + strings.Repeat("p", 1<<20) + `"}`
	if w := call("POST", "launch", "", body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "agent_name_taken") {
		t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	l := fp.requestsTo("/api/launch")
	if len(l) != 1 || l[0].method != "POST" || l[0].body != body || l[0].header.Get("Content-Type") != "application/json" {
		t.Fatalf("forwarded launch = %d requests", len(l))
	}
	for _, h := range []string{"Cookie", "Tailscale-User-Login", "Origin"} {
		if l[0].header.Get(h) != "" {
			t.Errorf("browser header %s was forwarded", h)
		}
	}
	// Body cap, wrong methods, unknown ops and hosts are refused locally.
	if w := call("POST", "launch", "", `{"prompt":"`+strings.Repeat("p", 2<<20)+`"}`); w.Code != http.StatusBadRequest {
		t.Errorf("oversized launch: %d", w.Code)
	}
	if w := call("GET", "launch", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET launch: %d", w.Code)
	}
	if w := call("POST", "launch-dirs", "", `{}`); w.Code != http.StatusNotFound {
		t.Errorf("POST launch-dirs: %d", w.Code)
	}
	if w := call("GET", "output", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("per-agent op on the host proxy: %d", w.Code)
	}
	w := httptest.NewRecorder()
	f.ProxyHost(w, httptest.NewRequest("GET", "/x", nil), "nope", "launch-options")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_host") {
		t.Errorf("unknown host: %d %s", w.Code, w.Body)
	}
	if n := len(fp.requestsTo("")); n != 3 {
		t.Errorf("refused calls reached the peer: %d requests", n)
	}
}

func TestProxyDialogAndAnswer(t *testing.T) {
	fp := newFakePeer(t)
	f := New(nil, "desk", []Peer{{Name: "kube", URL: fp.url()}}, fastOpts(nil))
	call := func(method, op, query, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/x?"+query, strings.NewReader(body))
		r.Header.Set("Tailscale-User-Login", "phone@example.com")
		w := httptest.NewRecorder()
		f.Proxy(w, r, "kube", "wS:p1", op)
		return w
	}
	if w := call("GET", "dialog", "evil=1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "pi_gate") {
		t.Fatalf("dialog = %d %s", w.Code, w.Body)
	}
	if d := fp.requestsTo("/dialog"); len(d) != 1 || d[0].path != "/api/agents/wS:p1/dialog" || d[0].query != "" {
		t.Fatalf("forwarded dialog = %+v", d)
	}
	body := `{"action":"select","index":1,"fingerprint":"abc"}`
	// The peer's 409 dialog_changed (with the fresh dialog) passes through.
	if w := call("POST", "answer", "", body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "dialog_changed") {
		t.Fatalf("answer = %d %s", w.Code, w.Body)
	}
	a := fp.requestsTo("/answer")
	if len(a) != 1 || a[0].method != "POST" || a[0].body != body || a[0].header.Get("Tailscale-User-Login") != "" {
		t.Fatalf("forwarded answer = %+v", a)
	}
	if w := call("GET", "answer", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET answer: %d", w.Code)
	}
	if w := call("POST", "dialog", "", "{}"); w.Code != http.StatusNotFound {
		t.Errorf("POST dialog: %d", w.Code)
	}
}

func TestPromptTimeout(t *testing.T) {
	if d := promptTimeout([]byte(`{"text":"x"}`)); d != 30*time.Second {
		t.Errorf("no wait: %v", d)
	}
	if d := promptTimeout([]byte(`{"wait":true,"timeout_ms":60000}`)); d != 95*time.Second {
		t.Errorf("wait 60s: %v", d)
	}
	if d := promptTimeout([]byte(`{"wait":true,"timeout_ms":99999999999}`)); d > 31*time.Minute {
		t.Errorf("capped: %v", d)
	}
}

// fakePusher records Web Push notifications.
type fakePusher struct {
	mu   sync.Mutex
	keys []string
	msgs []push.Message
}

func (p *fakePusher) Active() bool      { return true }
func (p *fakePusher) ExcerptChars() int { return 200 }
func (p *fakePusher) Notify(key string, msg push.Message, _ push.Options) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, key)
	p.msgs = append(p.msgs, msg)
}
func (p *fakePusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.msgs)
}

type viewer map[string]bool

func (v viewer) Viewing(key string) bool     { return v[key] }
func (v viewer) LastViewed(string) time.Time { return time.Time{} }

func TestPeerNotifications(t *testing.T) {
	var ntfyMu sync.Mutex
	ntfyPosts := 0
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyMu.Lock()
		ntfyPosts++
		ntfyMu.Unlock()
	}))
	defer ntfy.Close()
	ntfyCount := func() int { ntfyMu.Lock(); defer ntfyMu.Unlock(); return ntfyPosts }

	fp := newFakePeer(t,
		agent("wS:p1", "fixer", herdr.StatusBlocked), // blocked at connect: baseline, no notification
		agent("wS:p2", "watched", herdr.StatusWorking))
	pusher := &fakePusher{}
	d := &state.Dispatcher{
		Ntfy:     &state.Notifier{BaseURL: ntfy.URL, Topic: "t", MinInterval: time.Hour},
		Push:     pusher,
		Viewer:   viewer{"kube/wS:p2": true}, // the phone has this one open
		ShowHost: true,
		HoldFor:  -1, // count every transition; collation is tested in state
	}
	f := startFleet(t, fastOpts(d), Peer{Name: "kube", URL: fp.url()})
	waitFor(t, "connected", func() bool { return len(f.Agents()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if pusher.count() != 0 {
		t.Fatalf("baseline notified: %+v", pusher.msgs)
	}

	// working -> done for the watched agent: suppressed by presence.
	// blocked -> working -> blocked for fixer: notifies once with the
	// excerpt fetched from the peer's transcript.
	fp.setAgents(agent("wS:p1", "fixer", herdr.StatusWorking), agent("wS:p2", "watched", herdr.StatusDone))
	fp.setAgents(agent("wS:p1", "fixer", herdr.StatusBlocked), agent("wS:p2", "watched", herdr.StatusDone))
	waitFor(t, "push", func() bool { return pusher.count() == 1 })
	pusher.mu.Lock()
	key, msg := pusher.keys[0], pusher.msgs[0]
	pusher.mu.Unlock()
	if key != "kube/wS:p1/blocked" || msg.URL != "/#/agent/kube/wS:p1" || msg.Tag != "kube/wS:p1" ||
		msg.Title != "fixer needs input" || msg.Body != "kube · repo\nwants to run bash: git push" {
		t.Fatalf("push = %q %+v", key, msg)
	}
	if n := len(fp.requestsTo("/api/agents/wS:p1/transcript")); n != 1 {
		t.Fatalf("excerpt fetches = %d", n)
	}
	waitFor(t, "ntfy", func() bool { return ntfyCount() == 1 })

	// Same transition again within the throttle interval: ntfy throttles
	// (the fake pusher does not throttle; push.Service does, by the same key).
	fp.setAgents(agent("wS:p1", "fixer", herdr.StatusWorking), agent("wS:p2", "watched", herdr.StatusDone))
	fp.setAgents(agent("wS:p1", "fixer", herdr.StatusBlocked), agent("wS:p2", "watched", herdr.StatusDone))
	waitFor(t, "second push", func() bool { return pusher.count() == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := ntfyCount(); n != 1 {
		t.Fatalf("ntfy posts = %d, want 1 (throttled)", n)
	}

	// A reconnect re-baselines: a status that changed while the peer was
	// down is not notified.
	fp.goSilent(true)
	waitFor(t, "down", func() bool { return hostByName(f, "kube").Status == StatusDown })
	fp.mu.Lock()
	fp.agents = []herdr.AgentInfo{agent("wS:p1", "fixer", herdr.StatusDone), agent("wS:p2", "watched", herdr.StatusDone)}
	fp.mu.Unlock()
	fp.goSilent(false)
	waitFor(t, "up", func() bool { return hostByName(f, "kube").Status == StatusOK })
	time.Sleep(100 * time.Millisecond)
	if n := pusher.count(); n != 2 {
		t.Fatalf("pushes after reconnect = %d, want 2", n)
	}
}

func TestPeerPushWarning(t *testing.T) {
	fp := newFakePeer(t, agent("wS:p1", "fixer", herdr.StatusIdle))
	fp.pushOn = true
	var mu sync.Mutex
	var logs []string
	opts := fastOpts(nil)
	opts.Logf = func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	startFleet(t, opts, Peer{Name: "kube", URL: fp.url()})
	waitFor(t, "warning", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, "-push=false") {
				return true
			}
		}
		return false
	})
}
