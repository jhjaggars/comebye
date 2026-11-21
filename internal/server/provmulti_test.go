package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/provision"
)

// A provisioner with two worker configs: AWS (whose login has expired) and
// KubeVirt. The failing one must not hide the other's hosts.

func kvStatusDoc(state string) string {
	return `{"v":1,"provider":"kubevirt","workers":[{"name":"kv1","state":"` + state + `","type":"4 vCPU, 32Gi","managed":true,"launched_at":null,"last_stop_at":null,"created_at":"` +
		time.Now().Add(-5*time.Hour).UTC().Format(time.RFC3339) + `","caps":{"budget_cap_usd":null,"destroy_after_hours":null,"launch_price_usd_hour":null}}]}`
}

func twoConfigHub(t *testing.T) (*Server, *provPeer, *fakeWorkerDir, *fakeWorkerDir) {
	t.Helper()
	s, prov, awsW, _ := hubSetup(t)
	kvW := newFakeWorker(t)
	kvW.set(t, "status.json", kvStatusDoc("STOPPED"))
	kvW.set(t, "start.flip", kvStatusDoc("RUNNING"))
	awsW.set(t, "status.fail", "") // "Your session has expired" on stderr in real life
	pc := &prov.peer.cfg.Provision
	pc.Tool.Name, pc.Tool.Provider, pc.Tool.Real = "aws", "aws", true
	pc.Tool.Config = "/etc/aws.conf"
	pc.Extra = []*provision.Tool{{Bin: kvW.bin, Config: "/etc/kubevirt.conf", Name: "kubevirt", Provider: "kubevirt", Real: true}}
	return s, prov, awsW, kvW
}

func TestTwoConfigsAFailingOneDoesNotHideTheOthersStoppedHost(t *testing.T) {
	s, _, awsW, _ := twoConfigHub(t)
	var h hostsResp
	if code := getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h); code != 200 {
		t.Fatalf("hosts = %d", code)
	}
	if len(h.Hosts) != 1 || h.Hosts[0].Name != "kv1" || h.Hosts[0].State != "stopped" || h.Hosts[0].Provider != "kubevirt" {
		t.Fatalf("hosts: %+v", h.Hosts)
	}
	if len(h.Nodes) != 1 || !strings.HasPrefix(h.Nodes[0].Stale, "aws: ") || strings.Contains(h.Nodes[0].Stale, "kubevirt") {
		t.Errorf("the error is shown for aws only: %+v", h.Nodes)
	}
	if awsW.runs("status") == 0 {
		t.Errorf("aws was never asked: %v", awsW.argv())
	}
}

func TestTwoConfigsStartRunsTheOwnersBinaryAndConfig(t *testing.T) {
	s, prov, awsW, kvW := twoConfigHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	hubCall := map[string]string{"Tailscale-User-Login": "me@example.com", fleet.HeaderHubCall: "1"}
	w := do(prov.peer, "POST", "/api/provisioner/host/start", hubCall, `{"name":"kv1"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	waitUntil(t, "kv1 start ran", func() bool { return kvW.runs("start kv1") == 1 })
	if !strings.Contains(strings.Join(kvW.argv(), " "), "-config /etc/kubevirt.conf") {
		t.Errorf("kubevirt argv: %v", kvW.argv())
	}
	if awsW.runs("start") != 0 {
		t.Errorf("the aws tool was asked to start kv1: %v", awsW.argv())
	}
	if h, _ := prov.peer.phosts.Get("kv1"); h.Owner != "kubevirt" || !h.Adopted {
		t.Errorf("record: %+v", h)
	}
}

func TestTwoConfigsPricesAndAddHostUseTheCloudsConfig(t *testing.T) {
	_, prov, awsW, kvW := twoConfigHub(t)
	hubCall := map[string]string{"Tailscale-User-Login": "me@example.com", fleet.HeaderHubCall: "1"}
	if w := do(prov.peer, "GET", "/api/provisioner/catalog?kind=aws&region=us-east-1", hubCall, ""); w.Code != 200 {
		t.Fatalf("catalog = %d %s", w.Code, w.Body)
	}
	if awsW.runs("catalog") == 0 || kvW.runs("catalog") != 0 {
		t.Errorf("catalog ran on aws=%d kubevirt=%d", awsW.runs("catalog"), kvW.runs("catalog"))
	}
	if w := do(prov.peer, "GET", "/api/provisioner/preflight", hubCall, ""); w.Code != 200 {
		t.Fatalf("preflight = %d %s", w.Code, w.Body)
	}
	if kvW.runs("preflight") != 0 {
		t.Errorf("preflight asked the kubevirt config")
	}
}

func TestAHostWhoseConfigIsGoneIsNotActedOnByAnotherConfig(t *testing.T) {
	s, prov, _, kvW := twoConfigHub(t)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	prov.peer.cfg.Provision.Extra = nil // the kubevirt config is taken off
	hubCall := map[string]string{"Tailscale-User-Login": "me@example.com", fleet.HeaderHubCall: "1"}
	if w := do(prov.peer, "POST", "/api/provisioner/host/start", hubCall, `{"name":"kv1"}`); w.Code == http.StatusAccepted {
		t.Errorf("start went ahead through another config: %d", w.Code)
	}
	if kvW.runs("start") != 0 {
		t.Errorf("ran %v", kvW.argv())
	}
}

func TestOneConfigIsUnchanged(t *testing.T) {
	w := newFakeWorker(t)
	s := provPeerServer(t, w)
	w.set(t, "status.json", statusDoc("kv1", "STOPPED", ""))
	if ok := s.observeHosts(t.Context()); len(ok) != 1 {
		t.Fatalf("read: %v", ok)
	}
	if h, _ := s.phosts.Get("kv1"); h.State != "STOPPED" {
		t.Fatalf("%+v", h)
	}
	s.cfg.Provision.Tool.Real = true
	hubCall := map[string]string{"Tailscale-User-Login": "me@example.com", fleet.HeaderHubCall: "1"}
	if rec := do(s, "POST", "/api/provisioner/host/start", hubCall, `{"name":"kv1"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "start", func() bool { return w.runs("start kv1") == 1 })
	var got peerHostsAnswer
	if code := getJSON(t, s, "/api/provisioner/hosts", hubCall, &got); code != 200 || got.Stale != "" {
		t.Errorf("hosts = %d stale %q", code, got.Stale)
	}
}
