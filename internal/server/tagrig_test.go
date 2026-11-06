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

// tagRig is a hub (its handler on a test server; serve() calls it as Serve
// does for a tagged node holding the capability), and a fake worker node at the
// address a self-registration announces. The fake WhoIs reports the
// worker's node as the loopback address, so the announced URL (the fake
// worker's own) is "on the caller's node".
type tagRig struct {
	t      *testing.T
	fl     *fleet.Fleet
	main   *httptest.Server
	worker *httptest.Server
	who    *fakeWhoIs
	node   string
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
		main: httptest.NewServer(s.Handler()),
	}
	t.Cleanup(r.main.Close)
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

func TestAnnounceThenPeerAndAgentsAppear(t *testing.T) {
	r := newCapRig(t, true)
	r.who.setHolder(r.node, "tag:worker")
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
	// Once registered, the node is a peer on through Serve too.
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
	r.who.setHolder("other.t.ts.net", "tag:worker")
	if code, body := r.peer("POST", "/api/peers/announce", `{"name":"vm1","url":"https://other.t.ts.net"}`); code != http.StatusConflict || !strings.Contains(body, "another node") {
		t.Fatalf("taken name: %d %s", code, body)
	}
	// The announcer cannot remove anyone: DELETE is the user's alone.
	r.who.setHolder(r.node, "tag:worker")
	if code, _ := r.peer("DELETE", "/api/peers/vm1", ""); code != http.StatusForbidden {
		t.Fatalf("self-unregister: %d", code)
	}
}

func TestAnnounceRefusals(t *testing.T) {
	// Off by default.
	off := newCapRig(t, false)
	off.who.setHolder("w1.t.ts.net", "tag:worker")
	if code, body := off.peer("POST", "/api/peers/announce", `{"name":"w1"}`); code != http.StatusForbidden || !strings.Contains(body, "self_register_off") {
		t.Fatalf("off: %d %s", code, body)
	}
	if _, b := off.call(off.main, "GET", "/api/peers", "", map[string]string{"Tailscale-User-Login": "me@example.com"}); strings.Contains(b, "w1") {
		t.Fatalf("registered while off: %s", b)
	}

	r := newCapRig(t, true)
	// Untagged / wrongly tagged nodes cannot announce.
	r.who.setHolder("laptop.t.ts.net")
	if code, body := r.peer("POST", "/api/peers/announce", `{"name":"laptop"}`); code != http.StatusForbidden || !strings.Contains(body, "cap_not_held") {
		t.Fatalf("untagged: %d %s", code, body)
	}
	// A worker cannot register someone else's URL, however it is spelled.
	r.who.setHolder("w1.t.ts.net", "tag:worker")
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
