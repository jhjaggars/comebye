package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// instRig is a hub with a ledger and a to-do list in front of a fake peer.
type instRig struct {
	*peerRig
	s *Server
}

func newInstRig(t *testing.T, peersDir, stateDir string) *instRig {
	t.Helper()
	peer := httptest.NewServer(&hubPeer{})
	t.Cleanup(peer.Close)
	store, err := fleet.OpenStore(peersDir)
	if err != nil {
		t.Fatal(err)
	}
	opt := fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}}
	peers := fleet.LoadDynamic(store, nil, "desk", opt, t.Logf)
	fl := fleet.New(nil, "desk", peers, fleet.Options{Version: "v1.0.0", HubMode: true, Logf: func(string, ...any) {}})
	fl.AttachStore(store, opt)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: "tok", Version: "v1.0.0", StateDir: stateDir}, nil, nil)
	if s.ledger == nil || s.todos == nil {
		t.Fatal("no ledger")
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &instRig{&peerRig{t: t, dir: peersDir, fl: fl, srv: srv, peer: peer, stop: cancel}, s}
}

func (r *instRig) addPeer(name string) string {
	r.t.Helper()
	code, body := r.req("POST", "/api/peers", `{"name":"`+name+`","urls":["`+r.peer.URL+`"]}`, nil)
	if code != http.StatusCreated {
		r.t.Fatalf("add: %d %s", code, body)
	}
	var info struct{ Instance string }
	json.Unmarshal([]byte(body), &info)
	if info.Instance == "" {
		r.t.Fatalf("no instance: %s", body)
	}
	return info.Instance
}

func (r *instRig) entry(id string) ledger.Entry {
	e, _ := r.s.ledger.Get(id)
	return e
}

func TestReplacingAPeerLosesItsUnfinishedEntries(t *testing.T) {
	r := newInstRig(t, t.TempDir(), t.TempDir())
	u1 := r.addPeer("kube")
	waitUntil(t, "peer agents", func() bool { return r.agentsContain("kube/wS:p1") })
	if _, body := r.req("GET", "/api/agents", "", nil); !strings.Contains(body, `"instance":"`+u1+`"`) {
		t.Fatalf("agents carry no instance: %s", body)
	}
	run, _ := r.s.ledger.Add(ledger.Entry{Host: "kube", HostInstance: r.s.hostInstance("kube"), AgentID: "kube/wS:p1", Branch: "a", Status: ledger.Running})
	done, _ := r.s.ledger.Add(ledger.Entry{Host: "kube", HostInstance: u1, AgentID: "kube/wS:p2", Branch: "b", Status: ledger.Finished})
	local, _ := r.s.ledger.Add(ledger.Entry{Host: "desk", AgentID: "desk/w1:p1", Branch: "c", Status: ledger.Running})

	// worker create tells the hub the cloud's id: a new incarnation.
	if code, body := r.req("PUT", "/api/peers/kube/instance", `{"instance":"i-0abc123"}`, nil); code != 200 || !strings.Contains(body, `"instance":"i-0abc123"`) {
		t.Fatalf("set instance: %d %s", code, body)
	}
	if e := r.entry(run.ID); e.Status != ledger.Lost || !strings.Contains(e.Error, "replaced") {
		t.Fatalf("running entry = %v %q", e.Status, e.Error)
	}
	if r.entry(done.ID).Status != ledger.Finished || r.entry(local.ID).Status != ledger.Running {
		t.Fatal("a reported entry or another host's entry changed")
	}
	// Same value again: nothing else is lost.
	if code, _ := r.req("PUT", "/api/peers/kube/instance", `{"instance":"i-0abc123"}`, nil); code != 200 {
		t.Fatal(code)
	}

	// An entry made now follows the new instance; removing the peer loses it.
	next, _ := r.s.ledger.Add(ledger.Entry{Host: "kube", HostInstance: r.s.hostInstance("kube"), AgentID: "kube/wS:p1", Branch: "d", Status: ledger.Running})
	if next.HostInstance != "i-0abc123" {
		t.Fatalf("instance = %q", next.HostInstance)
	}
	if code, _ := r.req("DELETE", "/api/peers/kube", "", nil); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if e := r.entry(next.ID); e.Status != ledger.Lost || !strings.Contains(e.Error, "removed") {
		t.Fatalf("after removal = %v %q", e.Status, e.Error)
	}
	// The name comes back as a different machine: a different instance.
	if u3 := r.addPeer("kube"); u3 == u1 || u3 == "i-0abc123" {
		t.Fatalf("re-added peer kept instance %s", u3)
	}
}

func TestHandoffRecordsTheHostInstance(t *testing.T) {
	r := newInstRig(t, t.TempDir(), t.TempDir())
	u := r.addPeer("kube")
	// The queued-handoff path stores a Waiting entry; starting it goes
	// through put(), which stamps the instance. Exercise the stamp directly.
	if got := r.s.hostInstance("kube"); got != u {
		t.Fatalf("hostInstance = %q, want %q", got, u)
	}
	if r.s.hostInstance("desk") != "" || r.s.hostInstance("nope") != "" {
		t.Fatal("this host and unknown hosts have no instance")
	}
}

// Only the user sets or changes an instance: not an agent, not a tagged node.
func TestInstanceIsTheUsersAlone(t *testing.T) {
	r := newInstRig(t, t.TempDir(), t.TempDir())
	u := r.addPeer("kube")
	code, body := r.req("PUT", "/api/peers/kube/instance", `{"instance":"i-evil"}`, agentHdr("w1:p1"))
	if code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("agent role: %d %s", code, body)
	}
	if r.s.hostInstance("kube") != u {
		t.Fatal("an agent changed the instance")
	}
	// A tagged worker, through the peer listener.
	tr := newCapRig(t, true)
	tr.who.setHolder(tr.node, "tag:worker")
	if code, body := tr.peer("PUT", "/api/peers/vm1/instance", `{"instance":"i-evil"}`); code != http.StatusForbidden {
		t.Fatalf("tagged PUT: %d %s", code, body)
	}
	// Announcing with an instance of its own: ignored, the hub makes one.
	code, body = tr.peer("POST", "/api/peers/announce", `{"name":"vm1","url":"`+tr.workerURL()+`","instance":"i-evil"}`)
	if code != http.StatusCreated && code != http.StatusBadRequest {
		t.Fatalf("announce: %d %s", code, body)
	}
	if code == http.StatusCreated {
		var info struct{ Instance string }
		json.Unmarshal([]byte(body), &info)
		if info.Instance == "" || info.Instance == "i-evil" {
			t.Fatalf("announced instance = %q", info.Instance)
		}
	}
	if tr.fl.Instances()["vm1"] == "i-evil" {
		t.Fatal("a worker named its own instance")
	}
	// Changing it for a peer that is not there, a static one, or with junk.
	if code, _ := r.req("PUT", "/api/peers/nope/instance", `{"instance":"i-1"}`, nil); code != http.StatusNotFound {
		t.Fatalf("unknown peer: %d", code)
	}
	if code, _ := r.req("PUT", "/api/peers/kube/instance", `{"instance":"has space"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("bad instance: %d", code)
	}
}

// Entries from before instances get their host's current one at start; a
// file of the old shape loads unchanged.
func TestStartBackfillsOldEntries(t *testing.T) {
	peers, state := t.TempDir(), t.TempDir()
	r := newInstRig(t, peers, state)
	u := r.addPeer("kube")
	r.stop()
	old := `{"version":1,"entries":[` +
		`{"id":"h1","host":"kube","agent_id":"kube/wS:p1","branch":"a","brief":"x","status":"running","created_at":"2026-01-01T00:00:00Z"},` +
		`{"id":"h2","host":"gone","agent_id":"gone/w1:p1","branch":"b","brief":"y","status":"running","created_at":"2026-01-01T00:00:00Z"},` +
		`{"id":"h3","host":"kube","agent_id":"kube/w9:p1","branch":"c","brief":"z","status":"removed","created_at":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(state, "ledger.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := newInstRig(t, peers, state)
	if e := r2.entry("h1"); e.HostInstance != u || e.Status != ledger.Running {
		t.Fatalf("h1 = %+v", e)
	}
	if r2.entry("h2").HostInstance != "" || r2.entry("h3").HostInstance != "" {
		t.Fatal("stamped an entry whose host is not a peer, or a finished one")
	}
}

func TestTodoWaitingAgentGone(t *testing.T) {
	r := newInstRig(t, t.TempDir(), t.TempDir())
	u := r.addPeer("kube")
	code, body := r.req("POST", "/api/todos", `{"question":"may I?","kind":"decision","waiting":"kube/wS:p1"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	var out struct{ Todo Todo }
	json.Unmarshal([]byte(body), &out)
	if out.Todo.Waiting == nil || out.Todo.Waiting.Instance != u {
		t.Fatalf("waiting = %+v", out.Todo.Waiting)
	}
	// A request cannot choose the instance.
	_, body = r.req("POST", "/api/todos", `{"question":"another","kind":"decision","waiting":"kube/wS:p1","instance":"i-evil","waiting_instance":"i-evil"}`, nil)
	if strings.Contains(body, "i-evil") {
		t.Fatalf("instance taken from the request: %s", body)
	}
	_, body = r.req("GET", "/api/todos", "", nil)
	if strings.Contains(body, `"gone":true`) {
		t.Fatalf("gone too early: %s", body)
	}
	r.req("PUT", "/api/peers/kube/instance", `{"instance":"i-0abc123"}`, nil)
	for _, p := range []string{"/api/todos", "/api/todos/" + out.Todo.ID} {
		if _, body = r.req("GET", p, "", nil); !strings.Contains(body, `"gone":true`) {
			t.Fatalf("%s: not marked gone: %s", p, body)
		}
	}
	// What is stored does not say "gone": it is worked out when served.
	if it, _ := r.s.todos.get(out.Todo.ID); it.Waiting.Gone {
		t.Fatal("gone was stored")
	}
}

// An item stored before instances has no instance and is never "gone".
func TestOldTodoFileLoads(t *testing.T) {
	state := t.TempDir()
	old := `{"version":1,"items":[{"id":"t1","question":"q","kind":"decision","state":"open","waiting":{"id":"kube/wS:p1","name":"w"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`
	os.WriteFile(filepath.Join(state, "todo.json"), []byte(old), 0o600)
	r := newInstRig(t, t.TempDir(), state)
	r.addPeer("kube")
	_, body := r.req("GET", "/api/todos", "", nil)
	if !strings.Contains(body, `"id":"t1"`) || strings.Contains(body, `"gone"`) {
		t.Fatalf("%s", body)
	}
}
