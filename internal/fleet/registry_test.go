package fleet

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// registryFleet is a running hub-mode fleet with a store; loopback http
// addresses are admitted (test mode), so httptest peers can be added.
func registryFleet(t *testing.T, version string, static ...Peer) (*Fleet, *Store) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := fastOpts(nil)
	opts.HubMode = true
	opts.Version = version
	f := startFleet(t, opts, static...)
	f.opts.HubMode = true
	f.AttachStore(store, AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}})
	return f, store
}

func TestAddPeerLive(t *testing.T) {
	f, store := registryFleet(t, "v1.0.0")
	if !f.IsHub() || f.HasPeers() {
		t.Fatal("hub mode with no peers")
	}
	fp := newFakePeer(t, agent("wS:p1", "remote", herdr.StatusIdle))
	info, err := f.Add("w1", []string{fp.srv.URL}, "me@example.com", "tag:worker")
	if err != nil {
		t.Fatal(err)
	}
	if info.Source != SourceDynamic || info.AddedBy != "me@example.com" || info.Tag != "tag:worker" || info.AddedAt == nil {
		t.Fatalf("info = %+v", info)
	}
	// Streamed at once, with no restart: the agent shows up merged.
	waitFor(t, "peer agent", func() bool {
		for _, a := range f.Agents() {
			if a.ID == "w1/wS:p1" {
				return true
			}
		}
		return false
	})
	if hostByName(f, "w1").Status != StatusOK {
		t.Fatalf("host = %+v", hostByName(f, "w1"))
	}
	if len(store.List()) != 1 {
		t.Fatal("not persisted")
	}
	// The health probe ran immediately too (not after ProbeEvery = 5m).
	waitFor(t, "first probe", func() bool { return len(fp.requestsTo("/api/whoami")) > 0 })
}

func TestAddPeerRefusals(t *testing.T) {
	static := newFakePeer(t)
	f, _ := registryFleet(t, "", Peer{Name: "stat", URLs: []*url.URL{static.url()}})
	fp := newFakePeer(t)
	if _, err := f.Add("w1", []string{fp.srv.URL}, "me", ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		urls []string
		want error
		sub  string
	}{
		{"w1", []string{"https://w1b.t.ts.net"}, ErrExists, ""},                // not an upsert
		{"stat", []string{"https://s.t.ts.net"}, ErrExists, "-peers"},          // static clash says so
		{"desk", []string{"https://d.t.ts.net"}, ErrLocalName, ""},             // local host
		{"Bad_Name", []string{"https://b.t.ts.net"}, InvalidError{}, "DNS"},    // host-name rules
		{"x", []string{"https://evil.example.com"}, InvalidError{}, "tailnet"}, // not a tailnet host
		{"x", []string{"http://x.t.ts.net"}, InvalidError{}, "https"},
		{"x", nil, InvalidError{}, "address"},
		{"x", []string{fp.srv.URL}, InvalidError{}, "already belongs"}, // an address is one peer's
		{"x", []string{"https://x.t.ts.net"}, InvalidError{}, "tag"},   // placeholder, replaced below
	}
	for _, c := range cases[:len(cases)-1] {
		_, err := f.Add(c.name, c.urls, "me", "")
		var inv InvalidError
		switch {
		case err == nil:
			t.Errorf("%s %v: accepted", c.name, c.urls)
		case c.want == (InvalidError{}) && !errors.As(err, &inv):
			t.Errorf("%s: %v is not InvalidError", c.name, err)
		case c.want != (InvalidError{}) && !errors.Is(err, c.want):
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		case c.sub != "" && !strings.Contains(err.Error(), c.sub):
			t.Errorf("%s: %q lacks %q", c.name, err, c.sub)
		}
	}
	if _, err := f.Add("x", []string{"https://x.t.ts.net"}, "me", "two words"); err == nil {
		t.Error("a bad tag was accepted")
	}
}

func TestNotHubAndStaticRemoval(t *testing.T) {
	// No hub mode, no peers: refused.
	plain := New(nil, "desk", nil, fastOpts(nil))
	store, _ := OpenStore(t.TempDir())
	plain.AttachStore(store, AddrOptions{})
	if _, err := plain.Add("w", []string{"https://w.t.ts.net"}, "me", ""); !errors.Is(err, ErrNotHub) {
		t.Fatalf("add on a non-hub: %v", err)
	}
	if err := plain.Remove("w", "me"); !errors.Is(err, ErrNotHub) {
		t.Fatalf("remove on a non-hub: %v", err)
	}
	// A static peer is listed as static and cannot be removed.
	sp := newFakePeer(t)
	f, _ := registryFleet(t, "", Peer{Name: "stat", URLs: []*url.URL{sp.url()}})
	if err := f.Remove("stat", "me"); !errors.Is(err, ErrStaticPeer) || !strings.Contains(err.Error(), "-peers") {
		t.Fatalf("static removal: %v", err)
	}
	if err := f.Remove("nope", "me"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown removal: %v", err)
	}
	l := f.List()
	if len(l) != 1 || l[0].Source != SourceStatic {
		t.Fatalf("list = %+v", l)
	}
}

func TestRemovePeerCleanly(t *testing.T) {
	f, store := registryFleet(t, "")
	// A peer whose per-agent endpoint hangs until the hub gives up.
	var inFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("event: agents\ndata: {\"type\":\"agents\",\"agents\":[{\"pane_id\":\"wS:p1\",\"agent_status\":\"idle\"}]}\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/api/agents/wS:p1/transcript":
			inFlight.Add(1)
			<-r.Context().Done()
		default:
			w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()
	// Settle the goroutine count (httptest keep-alives) before measuring.
	runtime.GC()
	before := runtime.NumGoroutine()

	if _, err := f.Add("w1", []string{srv.URL}, "me", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "agents", func() bool { return len(f.Agents()) == 1 })

	res := make(chan error, 1)
	go func() {
		_, _, err := f.CallAgent(context.Background(), "w1", "wS:p1", "transcript", nil)
		res <- err
	}()
	waitFor(t, "in-flight forward", func() bool { return inFlight.Load() == 1 })

	if err := f.Remove("w1", "me"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-res:
		if err == nil || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("in-flight forward: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the in-flight forward was not cancelled by removal")
	}
	if len(f.Agents()) != 0 || f.HasPeer("w1") || len(f.Hosts()) != 1 || len(store.List()) != 0 {
		t.Fatalf("left behind: agents %d hosts %v store %v", len(f.Agents()), f.Hosts(), store.List())
	}
	if _, _, err := f.CallAgent(context.Background(), "w1", "wS:p1", "transcript", nil); err == nil {
		t.Fatal("a removed peer must be unknown")
	}
	// Nothing leaks: stream, probe and forward goroutines all end.
	f.opts.Client.CloseIdleConnections()
	srv.CloseClientConnections()
	waitFor(t, "goroutines to end", func() bool { return runtime.NumGoroutine() <= before+1 })
}

func TestVersionVisibility(t *testing.T) {
	f, _ := registryFleet(t, "v2.0.0")
	same := newFakePeer(t)
	same.version = "v2.0.0"
	other := newFakePeer(t)
	other.version = "v1.9.0"
	old := newFakePeer(t) // sends no header, like a peer from before it existed
	for name, fp := range map[string]*fakePeer{"same": same, "other": other, "old": old} {
		if _, err := f.Add(name, []string{fp.srv.URL}, "me", ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "all up", func() bool {
		for _, n := range []string{"same", "other", "old"} {
			if hostByName(f, n).Status != StatusOK {
				return false
			}
		}
		return true
	})
	if h := hostByName(f, "desk"); h.Version != "v2.0.0" || h.VersionMismatch {
		t.Fatalf("local = %+v", h)
	}
	if h := hostByName(f, "same"); h.Version != "v2.0.0" || h.VersionMismatch {
		t.Fatalf("match = %+v", h)
	}
	if h := hostByName(f, "other"); h.Version != "v1.9.0" || !h.VersionMismatch {
		t.Fatalf("mismatch = %+v", h)
	}
	// Unknown is not a mismatch: it reads as absent, and callers say "unknown".
	if h := hostByName(f, "old"); h.Version != "" || h.VersionMismatch {
		t.Fatalf("unknown = %+v", h)
	}
	for _, i := range f.List() {
		if i.Name == "other" && (i.Version != "v1.9.0" || !i.VersionMismatch) {
			t.Fatalf("list = %+v", i)
		}
	}
	// A deploy on the peer is noticed on its next answer.
	other.mu.Lock()
	other.version = "v2.0.0"
	other.mu.Unlock()
	f.lookup("other").probe(context.Background())
	if h := hostByName(f, "other"); h.VersionMismatch || h.Version != "v2.0.0" {
		t.Fatalf("after upgrade = %+v", h)
	}
}

func TestCleanVersion(t *testing.T) {
	if cleanVersion(" v1 ") != "v1" || cleanVersion("a\nb") != "" || len(cleanVersion(strings.Repeat("x", 500))) != 64 {
		t.Fatal("cleanVersion")
	}
}

func TestPeerInstances(t *testing.T) {
	static := newFakePeer(t)
	f, store := registryFleet(t, "v1.0.0", Peer{Name: "stat", URLs: []*url.URL{static.url()}})
	var ended [][3]string
	f.OnInstanceEnd(func(host, in, why string) { ended = append(ended, [3]string{host, in, why}) })

	// The peer claims an instance of its own in its agent list: ignored.
	forged := agent("wS:p1", "remote", herdr.StatusIdle)
	forged.Instance = "i-forged"
	fp := newFakePeer(t, forged)
	info, err := f.Add("w1", []string{fp.srv.URL}, "me", "")
	if err != nil || info.Instance == "" || len(info.Instance) != 36 {
		t.Fatalf("add = %+v %v", info, err)
	}
	if rec, _ := store.Get("w1"); rec.Instance != info.Instance {
		t.Fatalf("not stored: %+v", rec)
	}
	waitFor(t, "stamped agent", func() bool {
		for _, a := range f.Agents() {
			if a.ID == "w1/wS:p1" {
				return a.Instance == info.Instance
			}
		}
		return false
	})
	if got := f.Instances(); got["w1"] != info.Instance || len(got) != 1 {
		t.Fatalf("instances = %v (a static peer has none)", got)
	}

	// The cloud's id replaces it; the agents are re-stamped and the old
	// incarnation ends.
	if _, err := f.SetInstance("w1", "bad id!", "me"); !errors.As(err, new(InvalidError)) {
		t.Fatalf("bad instance: %v", err)
	}
	if _, err := f.SetInstance("stat", "i-1", "me"); !errors.Is(err, ErrStaticPeer) {
		t.Fatalf("static: %v", err)
	}
	if _, err := f.SetInstance("nope", "i-1", "me"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := f.SetInstance("w1", "i-0abc123", "me"); err != nil {
		t.Fatal(err)
	}
	if len(ended) != 1 || ended[0][0] != "w1" || ended[0][1] != info.Instance || !strings.Contains(ended[0][2], "i-0abc123") {
		t.Fatalf("ended = %v", ended)
	}
	for _, a := range f.Agents() {
		if a.ID == "w1/wS:p1" && a.Instance != "i-0abc123" {
			t.Fatalf("agent still stamped %q", a.Instance)
		}
	}
	if rec, _ := store.Get("w1"); rec.Instance != "i-0abc123" {
		t.Fatalf("not stored: %+v", rec)
	}
	// The same value again is not a new incarnation.
	if _, err := f.SetInstance("w1", "i-0abc123", "me"); err != nil || len(ended) != 1 {
		t.Fatalf("same instance: %v ended=%v", err, ended)
	}

	// Removing the peer ends it; adding the name again is a new one.
	if err := f.Remove("w1", "me"); err != nil {
		t.Fatal(err)
	}
	if len(ended) != 2 || ended[1][1] != "i-0abc123" {
		t.Fatalf("ended = %v", ended)
	}
	again, err := f.Add("w1", []string{fp.srv.URL}, "me", "")
	if err != nil || again.Instance == "" || again.Instance == "i-0abc123" || again.Instance == info.Instance {
		t.Fatalf("re-added = %+v %v", again, err)
	}
}

// A peers.json from before instances: each stored peer gets one, saved.
func TestLoadDynamicGivesLegacyPeersAnInstance(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	if err := store.Add(Record{Name: "w1", URLs: []string{"https://w1.ts.net"}, AddedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	opt := AddrOptions{Domains: []string{".ts.net"}}
	ps := LoadDynamic(store, nil, "desk", opt, t.Logf)
	if len(ps) != 1 || ps[0].Instance == "" {
		t.Fatalf("peers = %+v", ps)
	}
	again, _ := OpenStore(dir)
	if rec, _ := again.Get("w1"); rec.Instance != ps[0].Instance {
		t.Fatalf("not saved: %+v", rec)
	}
	if ps2 := LoadDynamic(again, nil, "desk", opt, t.Logf); ps2[0].Instance != ps[0].Instance {
		t.Fatal("changes on every start")
	}
}
