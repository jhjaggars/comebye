package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/provision"
	"github.com/jhjaggars/comebye/internal/state"
)

// herdrlessProvServer is the in-cluster provisioner: a peer with no ledger and
// no herdr (-hub-only -ledger=false -hub=...), holding one kubevirt config.
func herdrlessProvServer(t *testing.T, w *fakeWorkerDir) *Server {
	t.Helper()
	client := herdr.NewClient("")
	m := state.New(client, &state.Notifier{})
	f := fleet.New(m, "provisioner", nil, fleet.Options{NoLocalAgents: true, Version: "v-test"})
	tool := &provision.Tool{Bin: w.bin, Config: "/etc/comebye-provisioner/config", Name: "kubevirt", Provider: "kubevirt", Real: true}
	return newServer(t, Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "provisioner", StateDir: t.TempDir(),
		NoLedger: true, HubOnly: true, AgentToken: testToken, Fleet: f, Version: "v-test",
		Provision: ProvisionConfig{Tool: tool}}, client, m)
}

// The herdr-less peer answers the hub's provisioner calls, says it hosts no
// agents, reports 0 agents (a plain answer, not an error), and every agent
// route says "no herdr here".
func TestHerdrlessPeerAnswersProvisionerRoutesAndReportsNoAgents(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "status.json", kvStatusDoc("STOPPED"))
	s := herdrlessProvServer(t, w)
	rec := do(s, "GET", "/api/provisioner/preflight", hubCall, "")
	if rec.Code != 200 || rec.Header().Get(fleet.NoAgentsHeader) != "1" {
		t.Fatalf("preflight = %d %s (no-agents header %q)", rec.Code, rec.Body, rec.Header().Get(fleet.NoAgentsHeader))
	}
	rec = do(s, "GET", "/api/provisioner/hosts?refresh=1", hubCall, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"kv1"`) {
		t.Fatalf("hosts = %d %s", rec.Code, rec.Body)
	}
	// 0 agents, not an error
	var al struct {
		Agents []herdr.AgentInfo  `json:"agents"`
		Hosts  []state.HostStatus `json:"hosts"`
	}
	if code := getJSON(t, s, "/api/agents", userHdr, &al); code != 200 || len(al.Agents) != 0 || len(al.Hosts) != 1 || !al.Hosts[0].NoAgents {
		t.Fatalf("/api/agents = %d %+v", code, al)
	}
	for _, h := range s.fleet.Hosts() {
		if h.Local && (!h.NoAgents || h.Agents != 0 || h.Status != fleet.StatusOK) {
			t.Errorf("own host: %+v", h)
		}
	}
	// an agent route answers clearly, as 409 no_herdr
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/agents/w1:p1/prompt", `{"text":"hi"}`},
	} {
		rec := do(s, c.method, c.path, userHdr, c.body)
		t.Logf("%s %s = %d %s", c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "no_herdr") {
			t.Errorf("%s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if code, body := getHealth(s, "/healthz", "10.42.0.7:8743"); code != 200 || body["hub_only"] != true {
		t.Errorf("healthz = %d %v", code, body)
	}
}

// A hub shows such a peer as a host with no agents by design, not a broken one.
func TestHubShowsAHerdrlessProvisionerAsHostingNoAgents(t *testing.T) {
	w := newFakeWorker(t)
	w.set(t, "status.json", kvStatusDoc("STOPPED"))
	kube := newProvPeer(t, w)
	kube.peer = herdrlessProvServer(t, w)
	s := provHub(t, "", map[string]*provPeer{"kube": kube})
	var got *state.HostStatus
	waitUntil(t, "kube hosts no agents", func() bool {
		for _, h := range s.fleet.Hosts() {
			if h.Name == "kube" && h.NoAgents {
				c := h
				got = &c
				return true
			}
		}
		return false
	})
	if got.Agents != 0 || got.Status != fleet.StatusOK {
		t.Errorf("host: %+v", got)
	}
}

// twoProvisionerHub: "prov" is laharl's (aws, demo1 stopped), "kube" the
// in-cluster one (kubevirt, kv1 stopped); the default is laharl's.
func twoProvisionerHub(t *testing.T) (s *Server, awsW, kvW *fakeWorkerDir, hdr map[string]string, kube *provPeer) {
	t.Helper()
	s, _, awsW, hdr, _ = stoppedHubW(t, nil)
	kvW = newFakeWorker(t)
	kvW.set(t, "status.json", kvStatusDoc("STOPPED"))
	kvW.set(t, "start.flip", kvStatusDoc("RUNNING"))
	kube = newProvPeer(t, kvW)
	kube.peer = herdrlessProvServer(t, kvW)
	kube.peer.cfg.Provision.Tool.Real = true
	add := func(name, url string) {
		b, _ := json.Marshal(map[string]any{"name": name, "urls": []string{url}})
		if w := do(s, "POST", "/api/peers", userHdr, string(b)); w.Code != 201 {
			t.Fatalf("add %s = %d %s", name, w.Code, w.Body)
		}
		t.Cleanup(func() { do(s, "DELETE", "/api/peers/"+name, userHdr, "") })
	}
	add("kube", kube.srv.URL)
	// kv1 itself: a worker peer, which comes up once started
	kv1 := newProvPeer(t, newFakeWorker(t))
	add("kv1", kv1.srv.URL)
	waitUntil(t, "kube up", func() bool { return s.peerStatus("kube") == fleet.StatusOK })
	return
}

// With two provisioners, a start goes to the one whose book lists the host,
// and the other is never asked to run anything.
func TestStartPicksTheProvisionerThatOwnsTheHost(t *testing.T) {
	s, awsW, kvW, hdr, kube := twoProvisionerHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	byName := map[string]string{}
	for _, x := range h.Hosts {
		byName[x.Name] = x.Via + "/" + x.State
	}
	if byName["demo1"] != "prov/stopped" || byName["kv1"] != "kube/stopped" {
		t.Fatalf("hosts: %v", byName)
	}
	rec := do(s, "POST", "/api/provision/hosts/kv1/start", hdr, `{}`)
	var a wakeResp
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != http.StatusAccepted || a.Wake.Via != "kube" {
		t.Fatalf("start kv1 = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "kv1 start ran", func() bool { return kvW.runs("start kv1") == 1 })
	if awsW.runs("start") != 0 {
		t.Errorf("laharl's tool was asked: %v", awsW.argv())
	}
	if kube.asked("/api/provisioner/host/start") != 1 {
		t.Errorf("kube asked %d times", kube.asked("/api/provisioner/host/start"))
	}
	// an explicit via that is not the owner finds nothing to start
	rec = do(s, "POST", "/api/provision/hosts/demo1/start", hdr, `{"via":"kube"}`)
	if rec.Code == http.StatusAccepted {
		t.Errorf("start of demo1 via kube = %d %s", rec.Code, rec.Body)
	}
	if kvW.runs("start demo1") != 0 {
		t.Errorf("kube was told to start laharl's host: %v", kvW.argv())
	}
}

// Laharl is the default provisioner and is gone: nothing blocks the in-cluster
// one. Its stopped worker is found, offered, started, and the wake goes up.
func TestWakeThroughTheInClusterPeerWithLaharlAbsent(t *testing.T) {
	s, _, kvW, hdr, _ := twoProvisionerHub(t)
	// laharl goes away: its peer is removed (the default stays named)
	if rec := do(s, "DELETE", "/api/peers/prov", userHdr, ""); rec.Code >= 300 {
		t.Fatalf("remove prov = %d %s", rec.Code, rec.Body)
	}
	var h hostsResp
	if code := getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h); code != 200 {
		t.Fatalf("hosts = %d", code)
	}
	if len(h.Hosts) != 1 || h.Hosts[0].Name != "kv1" || h.Hosts[0].State != "stopped" || h.Hosts[0].Via != "kube" {
		t.Fatalf("hosts: %+v", h.Hosts)
	}
	rec := do(s, "POST", "/api/provision/hosts/kv1/start", hdr, `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	waitWake(t, s, "kv1", wakeUp)
	if kvW.runs("start kv1") != 1 {
		t.Errorf("argv: %v", kvW.argv())
	}
}
