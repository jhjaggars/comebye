package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/release"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// relHubCounting is relHubWithPeer whose peer, kube, counts the hub's
// release-status calls (the fan-out relHosts makes).
func relHubCounting(t *testing.T, version string) (*Server, *atomic.Int32) {
	t.Helper()
	var asks atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(fleet.VersionHeader, version)
		switch {
		case r.URL.Path == "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: agents\ndata: {\"type\":\"agents\",\"version\":%q,\"agents\":[]}\n\n", version)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == "GET" && r.URL.Path == "/api/release/upgrade":
			asks.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"enabled":true,"version":%q}`, version)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peer.Close)
	store, err := fleet.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(nil, "desk", nil, fleet.Options{HubMode: true, Version: version, Logf: func(string, ...any) {}})
	fl.AttachStore(store, fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	tool := &fakeTool{check: okCheck(), build: release.Build{State: "success"}}
	s := newServer(t, Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken, StateDir: t.TempDir(), Version: version,
		Release: ReleaseConfig{Tool: tool, Upgrade: true, Runner: &fakeRunner{}}}, nil, nil)
	b, _ := json.Marshal(map[string]any{"name": "kube", "urls": []string{peer.URL}})
	if w := do(s, "POST", "/api/peers", userHdr, string(b)); w.Code != 201 {
		t.Fatalf("add peer = %d %s", w.Code, w.Body)
	}
	waitUntil(t, "peer up", func() bool {
		for _, h := range fl.Hosts() {
			if h.Name == "kube" && h.Status == fleet.StatusOK && h.Version == version {
				return true
			}
		}
		return false
	})
	return s, &asks
}

// due makes every step of the loop due on its next pass.
func (l *releaseLoop) due() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, st := range l.steps {
		st.next = time.Unix(1, 0)
	}
}

// One pass of the loop asks every peer once, however many of its steps
// need the fleet: the engine's tick of a live rollout and the sweep share
// the answer.
func TestReleaseLoopAsksThePeersOncePerTick(t *testing.T) {
	s, asks := relHubCounting(t, "v0.5.9")
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	if _, err := s.rollout.Start(it.ID, "v0.5.9", relSHA, "me", []RolloutStep{{Host: "kube", Wave: wavePeers, Phase: roPending}}); err != nil {
		t.Fatal(err)
	}
	if got := s.relLoop.every(); got != s.relLoop.live {
		t.Fatalf("clock while live = %v", got)
	}
	asks.Store(0)
	s.dropRelHosts()
	s.relLoop.due()
	s.relLoop.pass(context.Background())
	s.waitBackground()
	for _, name := range []string{"rollout", "auto", "sweep"} {
		if n := s.relLoop.step(name).runs; n != 1 {
			t.Fatalf("step %s ran %d times", name, n)
		}
	}
	// The engine saw kube current (the step is ok, the job done) and the
	// sweep closed the item: both read the fleet, from one fan-out.
	if j, _ := s.rollout.Get(); j.State != roDone {
		t.Fatalf("job %s %+v", j.State, j.Plan)
	}
	if got, _ := s.todos.get(it.ID); got.State != TodoDone {
		t.Fatalf("item %s", got.State)
	}
	if n := asks.Load(); n != 1 {
		t.Fatalf("the peer was asked %d times in one pass", n)
	}
	if s.relCache.asks != 1 {
		t.Fatalf("fan-outs = %d", s.relCache.asks)
	}
	if got := s.relLoop.every(); got != s.relLoop.idle {
		t.Fatalf("clock once done = %v", got)
	}
}

// The cached answer lives a short while and is dropped as soon as an
// upgrade starts: the next read asks the fleet again.
func TestRelHostsCacheIsDroppedAfterAnUpgrade(t *testing.T) {
	s, asks := relHubCounting(t, "v0.5.8")
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	ctx := context.Background()
	asks.Store(0)
	s.dropRelHosts()
	s.relHostsCached(ctx)
	h := s.relHostsCached(ctx)
	if n := asks.Load(); n != 1 {
		t.Fatalf("asked %d times within the cache's life", n)
	}
	h[0].Version = "changed by a caller" // a caller's copy is its own
	if c := s.relHostsCached(ctx); c[0].Version == h[0].Version {
		t.Fatalf("the cache kept a caller's change: %+v", c)
	}
	v := getReleases(t, s)
	asks.Store(0) // the Releases tab asks afresh, without the cache
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(v.CSRF), `{"todo":"`+it.ID+`","version":"v0.5.9"}`); w.Code != 202 {
		t.Fatalf("upgrade = %d %s", w.Code, w.Body)
	}
	s.relHostsCached(ctx)
	if n := asks.Load(); n != 1 {
		t.Fatalf("after the upgrade started the fleet was asked %d times, want 1", n)
	}
	// And it expires by itself.
	s.relCache.mu.Lock()
	s.relCache.at = s.relCache.at.Add(-s.relCache.ttl)
	s.relCache.mu.Unlock()
	s.relHostsCached(ctx)
	if n := asks.Load(); n != 2 {
		t.Fatalf("after its life asked %d times, want 2", n)
	}
}

// A host the automatic rollout skipped while it was stopped is upgraded by
// the loop's one catch-up path when it starts: the done job is reopened
// (no second job), from the engine's step, on the one clock.
func TestReleaseLoopCatchesUpAHostThatComesBack(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a", "wk1")
	ctx := context.Background()
	f.set("wk1", func(h *relHost) { h.Status, h.Stopped = "down", true })
	l := s.relLoop
	l.due()
	pass := func() { l.pass(ctx); s.waitBackground() }
	pass() // auto: starts the rollout; wk1 is planned skipped
	j, ok := s.rollout.Get()
	if !ok || !j.Auto || j.Todo != it.ID {
		t.Fatalf("no automatic job: %+v", j)
	}
	pass()
	f.finish("a", upgrade.RecOK, "v0.5.9")
	for range 3 {
		pass()
	}
	if j, _ := s.rollout.Get(); j.State != roDone || j.Plan[1].Phase != roSkipped || !j.Plan[1].CatchUp {
		t.Fatalf("job %s %+v", j.State, j.Plan)
	}
	// wk1 starts, on the old version. Within the minute nothing is asked.
	f.set("wk1", func(h *relHost) { h.Status, h.Stopped = "ok", false })
	s.rollout.lastCatchUp = f.clock()
	pass()
	if j, _ := s.rollout.Get(); j.State != roDone {
		t.Fatalf("caught up within the clock: %s", j.State)
	}
	f.advance(rolloutCatchUpEvery)
	pass()
	if j, _ := s.rollout.Get(); j.State != roRunning || j.Plan[1].Phase != roPending {
		t.Fatalf("not reopened: %s %+v", j.State, j.Plan[1])
	}
	pass()
	f.finish("wk1", upgrade.RecOK, "v0.5.9")
	for range 3 {
		pass()
	}
	if j, _ := s.rollout.Get(); j.State != roDone || j.Plan[1].Phase != roOK {
		t.Fatalf("wk1 not caught up: %s %+v", j.State, j.Plan[1])
	}
	if strings.Join(f.starts, ",") != "a,wk1" || autoStarts(s) != 1 {
		t.Fatalf("starts %v, rollout.start entries %d", f.starts, autoStarts(s))
	}
	caught := 0
	for _, ev := range s.ledger.AuditLog(it.ID) {
		if ev.Action == "rollout.catchup" && ev.Host == "wk1" {
			caught++
		}
	}
	if caught != 1 {
		t.Fatalf("rollout.catchup for wk1 = %d", caught)
	}
}

// The steps keep their own cadence on the shared clock: the engine every
// pass, the automatic rollout every minute, the sweep every 30 s.
func TestReleaseLoopStepsKeepTheirCadence(t *testing.T) {
	s, _, _, _, _ := rolloutRouteRig(t, "a")
	l := s.relLoop
	now := time.Now()
	l.now = func() time.Time { return now }
	for range 20 { // ten minutes of idle 30 s passes
		l.pass(context.Background())
		s.waitBackground()
		now = now.Add(releaseIdleTick)
	}
	got := fmt.Sprintf("rollout=%d auto=%d sweep=%d", l.step("rollout").runs, l.step("auto").runs, l.step("sweep").runs)
	if got != "rollout=20 auto=9 sweep=19" {
		t.Fatal(got)
	}
}
