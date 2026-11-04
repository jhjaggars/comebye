package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/tsauth"
)

// fakeWhoIs answers WhoIs with whatever the test set (the connection's
// real source is loopback in tests). addr records the remote address.
type fakeWhoIs struct {
	mu   sync.Mutex
	id   tsauth.Identity
	err  error
	addr string
}

func (f *fakeWhoIs) set(node string, tags ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id, f.err = tsauth.Identity{Node: node, Tags: tags}, nil
}

func (f *fakeWhoIs) WhoIs(_ context.Context, addr string) (tsauth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addr = addr
	return f.id, f.err
}

// tagRig is a hub with a peer listener (the PeerHandler on its own test
// server) next to its normal handler, and a fake worker node at the
// address a self-registration announces. The fake WhoIs reports the
// worker's node as the loopback address, so the announced URL (the fake
// worker's own) is "on the caller's node".
type tagRig struct {
	t      *testing.T
	fl     *fleet.Fleet
	main   *httptest.Server
	peers  *httptest.Server
	worker *httptest.Server
	who    *fakeWhoIs
	node   string
}

func newTagRig(t *testing.T, selfRegister bool) *tagRig {
	t.Helper()
	return newRigWith(t, selfRegister, func(c *Config) { c.AllowTags = []string{"tag:worker"} })
}

func newRigWith(t *testing.T, selfRegister bool, set func(*Config)) *tagRig {
	t.Helper()
	worker := httptest.NewServer(&hubPeer{})
	t.Cleanup(worker.Close)
	store, err := fleet.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opt := fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}}
	var logs []string
	var lmu sync.Mutex
	fl := fleet.New(nil, "desk", nil, fleet.Options{HubMode: true, Logf: func(f string, a ...any) {
		lmu.Lock()
		logs = append(logs, f)
		lmu.Unlock()
	}})
	fl.AttachStore(store, opt)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	who := &fakeWhoIs{}
	cfg := Config{
		AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: "tok",
		WhoIs: who, SelfRegister: selfRegister,
	}
	set(&cfg)
	s := New(cfg, nil, nil)
	r := &tagRig{t: t, fl: fl, who: who, worker: worker,
		main:  httptest.NewServer(s.Handler()),
		peers: httptest.NewServer(s.PeerHandler()),
	}
	t.Cleanup(r.main.Close)
	t.Cleanup(r.peers.Close)
	u, _ := net.ResolveTCPAddr("tcp", strings.TrimPrefix(worker.URL, "http://"))
	r.node = u.IP.String() // 127.0.0.1: the "node" whose address is the fake worker
	return r
}

func (r *tagRig) workerURL() string { return r.worker.URL }

func (r *tagRig) call(srv *httptest.Server, method, path, body string, hdr map[string]string) (int, string) {
	r.t.Helper()
	q, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if body != "" {
		q.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		q.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(q)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *tagRig) peer(method, path, body string) (int, string) {
	return r.call(r.peers, method, path, body, nil)
}

func TestPeerListenerIdentityAndTagAllowlist(t *testing.T) {
	r := newTagRig(t, true)
	// tailscaled does not know the address (or the lookup failed): 401.
	r.who.err = tsauth.ErrUnknown
	if code, _ := r.peer("GET", "/api/whoami", ""); code != http.StatusUnauthorized {
		t.Fatalf("unknown address: %d", code)
	}
	// A user-owned node (no tags) and a node with another tag: 403.
	r.who.set("laptop.t.ts.net")
	if code, body := r.peer("GET", "/api/whoami", ""); code != http.StatusForbidden || !strings.Contains(body, "tag_not_allowed") {
		t.Fatalf("untagged: %d %s", code, body)
	}
	r.who.set("db.t.ts.net", "tag:db")
	if code, body := r.peer("GET", "/api/whoami", ""); code != http.StatusForbidden || !strings.Contains(body, "tag_not_allowed") {
		t.Fatalf("other tag: %d %s", code, body)
	}
	// The right tag but not a registered peer: 403, whatever it claims.
	r.who.set("w1.t.ts.net", "tag:other", "tag:worker")
	code, body := r.peer("GET", "/api/whoami", "")
	if code != http.StatusForbidden || !strings.Contains(body, "not_a_peer") {
		t.Fatalf("unregistered: %d %s", code, body)
	}
	// The WhoIs lookup used the connection's remote address.
	if !strings.HasPrefix(r.who.addr, "127.0.0.1:") {
		t.Fatalf("whois addr = %q", r.who.addr)
	}
}

func TestPeerListenerRoleIsAgentOnly(t *testing.T) {
	r := newTagRig(t, false)
	if code, body := r.call(r.main, "POST", "/api/peers", `{"name":"w1","urls":["https://w1.t.ts.net"],"tag":"tag:worker"}`, map[string]string{"Tailscale-User-Login": "me@example.com"}); code != http.StatusCreated {
		t.Fatalf("user adds the peer: %d %s", code, body)
	}
	r.who.set("w1.t.ts.net", "tag:worker")

	// Agent-role routes work, as the forwarded agent of the peer's name,
	// whatever host the request claims to be from.
	code, body := r.call(r.peers, "GET", "/api/whoami", "", map[string]string{HeaderAgent: "w:1", HeaderAgentHost: "someone-else"})
	var who struct{ Role, Agent, Host string }
	json.Unmarshal([]byte(body), &who)
	if code != 200 || who.Role != "agent" || who.Agent != "w1/w:1" {
		t.Fatalf("whoami: %d %s", code, body)
	}
	if code, _ := r.peer("GET", "/api/hosts", ""); code != 200 {
		t.Fatalf("agent route: %d", code)
	}
	// User-only routes are refused, for every kind of route the user has.
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/launch", `{}`},
		{"GET", "/api/peers", ""},
		{"POST", "/api/peers", `{"name":"x","urls":["https://x.t.ts.net"]}`},
		{"DELETE", "/api/peers/w1", ""},
		{"POST", "/api/removals/confirm", `{}`},
		{"GET", "/api/events", ""},
	} {
		if code, body := r.peer(c.m, c.p, c.b); code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("%s %s: %d %s", c.m, c.p, code, body)
		}
	}
	// Nothing but /api is served (no UI).
	if code, _ := r.peer("GET", "/", ""); code != http.StatusNotFound {
		t.Errorf("UI on the peer listener: %d", code)
	}
	// Forged identity headers change nothing: a login is not the user
	// role here, and the local agent token is not honored.
	for _, h := range []map[string]string{
		{"Tailscale-User-Login": "me@example.com"},
		{HeaderAgentToken: "tok"},
		{"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1"},
	} {
		if code, body := r.call(r.peers, "POST", "/api/launch", `{}`, h); code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("forged %v: %d %s", h, code, body)
		}
	}
	// And the other way round: the peer listener's WhoIs path is not
	// reachable on the Serve handler (no tagged context there).
	if code, _ := r.call(r.main, "GET", "/api/whoami", "", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous on main: %d", code)
	}
}

func TestPeerListenerPeerTagMustStillMatch(t *testing.T) {
	r := newTagRig(t, false)
	r.call(r.main, "POST", "/api/peers", `{"name":"w1","urls":["https://w1.t.ts.net"],"tag":"tag:vm"}`, map[string]string{"Tailscale-User-Login": "me@example.com"})
	r.who.set("w1.t.ts.net", "tag:worker") // allowed tag, but not the tag the peer was registered with
	if code, body := r.peer("GET", "/api/whoami", ""); code != http.StatusForbidden || !strings.Contains(body, "tag_mismatch") {
		t.Fatalf("%d %s", code, body)
	}
	r.who.set("w1.t.ts.net", "tag:worker", "tag:vm")
	if code, _ := r.peer("GET", "/api/whoami", ""); code != 200 {
		t.Fatalf("both tags: %d", code)
	}
}

func TestAnnounceThenPeerAndAgentsAppear(t *testing.T) {
	r := newTagRig(t, true)
	r.who.set(r.node, "tag:worker")
	code, body := r.peer("POST", "/api/peers/announce", `{"name":"vm1","url":"`+r.workerURL()+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("announce: %d %s", code, body)
	}
	var info struct {
		Name, Source, Tag string
		AddedBy           string `json:"added_by"`
	}
	json.Unmarshal([]byte(body), &info)
	if info.Name != "vm1" || info.Source != "dynamic" || info.Tag != "tag:worker" || info.AddedBy != "tag:worker@"+r.node {
		t.Fatalf("record = %s", body)
	}
	// The peer is listed (for the user) and its agents appear in the hub.
	_, list := r.call(r.main, "GET", "/api/peers", "", map[string]string{"Tailscale-User-Login": "me@example.com"})
	if !strings.Contains(list, `"name":"vm1"`) {
		t.Fatalf("list: %s", list)
	}
	waitUntil(t, "announced peer's agents", func() bool {
		_, b := r.call(r.main, "GET", "/api/agents", "", map[string]string{"Tailscale-User-Login": "me@example.com"})
		return strings.Contains(b, `"id":"vm1/wS:p1"`)
	})
	// Once registered, the node is a peer on the peer listener too.
	if code, _ := r.peer("GET", "/api/whoami", ""); code != 200 {
		t.Fatalf("announced node as a peer: %d", code)
	}
	// Re-announcing is idempotent: 200, and no second record.
	if code, body := r.peer("POST", "/api/peers/announce", `{"name":"vm1","url":"`+r.workerURL()+`"}`); code != http.StatusOK {
		t.Fatalf("re-announce: %d %s", code, body)
	}
	if n := strings.Count(func() string {
		_, b := r.call(r.main, "GET", "/api/peers", "", map[string]string{"Tailscale-User-Login": "me@example.com"})
		return b
	}(), `"name":"vm1"`); n != 1 {
		t.Fatalf("%d records after re-announce", n)
	}
	// A node cannot take a second name, nor a taken name from another node.
	if code, body := r.peer("POST", "/api/peers/announce", `{"name":"vm2","url":"`+r.workerURL()+`"}`); code != http.StatusConflict || !strings.Contains(body, "already registered") {
		t.Fatalf("second name: %d %s", code, body)
	}
	r.who.set("other.t.ts.net", "tag:worker")
	if code, body := r.peer("POST", "/api/peers/announce", `{"name":"vm1","url":"https://other.t.ts.net"}`); code != http.StatusConflict || !strings.Contains(body, "another node") {
		t.Fatalf("taken name: %d %s", code, body)
	}
	// The announcer cannot remove anyone: DELETE is the user's alone.
	r.who.set(r.node, "tag:worker")
	if code, _ := r.peer("DELETE", "/api/peers/vm1", ""); code != http.StatusForbidden {
		t.Fatalf("self-unregister: %d", code)
	}
}

func TestAnnounceRefusals(t *testing.T) {
	// Off by default.
	off := newTagRig(t, false)
	off.who.set("w1.t.ts.net", "tag:worker")
	if code, body := off.peer("POST", "/api/peers/announce", `{"name":"w1"}`); code != http.StatusForbidden || !strings.Contains(body, "self_register_off") {
		t.Fatalf("off: %d %s", code, body)
	}
	if _, b := off.call(off.main, "GET", "/api/peers", "", map[string]string{"Tailscale-User-Login": "me@example.com"}); strings.Contains(b, "w1") {
		t.Fatalf("registered while off: %s", b)
	}

	r := newTagRig(t, true)
	// Untagged / wrongly tagged nodes cannot announce.
	r.who.set("laptop.t.ts.net")
	if code, body := r.peer("POST", "/api/peers/announce", `{"name":"laptop"}`); code != http.StatusForbidden || !strings.Contains(body, "tag_not_allowed") {
		t.Fatalf("untagged: %d %s", code, body)
	}
	// A worker cannot register someone else's URL, however it is spelled.
	r.who.set("w1.t.ts.net", "tag:worker")
	for _, u := range []string{"https://w2.t.ts.net", "https://w1.t.ts.net.evil.example.com", "https://evil.example.com", "https://w2.t.ts.net@w1.t.ts.net"} {
		if code, body := r.peer("POST", "/api/peers/announce", `{"name":"w1","url":"`+u+`"}`); code != http.StatusBadRequest {
			t.Errorf("url %s: %d %s", u, code, body)
		}
	}
	// A bad name.
	if code, _ := r.peer("POST", "/api/peers/announce", `{"name":"Bad Name!"}`); code != http.StatusBadRequest {
		t.Errorf("bad name: %d", code)
	}
	// Announce is not answered on the Serve side: not even for the user.
	if code, body := r.call(r.main, "POST", "/api/peers/announce", `{"name":"w1"}`, map[string]string{"Tailscale-User-Login": "me@example.com"}); code != http.StatusForbidden || !strings.Contains(body, "announce_tagged_only") {
		t.Errorf("announce on Serve: %d %s", code, body)
	}
	// Nothing got registered by the refusals.
	if _, b := r.call(r.main, "GET", "/api/peers", "", map[string]string{"Tailscale-User-Login": "me@example.com"}); strings.Contains(b, `"name":"w`) {
		t.Fatalf("refusals registered a peer: %s", b)
	}
	// With no name or url, the node's own are used.
	if code, body := r.peer("POST", "/api/peers/announce", `{}`); code != http.StatusCreated || !strings.Contains(body, `"name":"w1"`) || !strings.Contains(body, `https://w1.t.ts.net`) {
		t.Fatalf("defaults: %d %s", code, body)
	}
}

func TestServePeerListenerRefusesNonTailnetAddresses(t *testing.T) {
	r := newTagRig(t, false)
	s := New(Config{AllowLogins: []string{"me@example.com"}, Fleet: r.fl, AllowTags: []string{"tag:worker"}, WhoIs: r.who}, nil, nil)
	for _, addr := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ServePeerListener(ln); err == nil || !strings.Contains(err.Error(), "tailnet") {
			t.Errorf("%s: %v", addr, err)
		}
		ln.Close()
	}
}
