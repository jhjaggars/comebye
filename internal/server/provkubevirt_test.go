package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/provision"
)

// ---- the KubeVirt kind: create and destroy from the in-cluster provisioner ----

const kubeSpec = `{"kind":"kubevirt","name":"kv2","idle_minutes":120,"destroy_after_hours":72}`

// kubeHub is a hub with one provisioner peer whose only worker config is a
// kubevirt one (the in-cluster pod). real turns -provision-create on.
func kubeHub(t *testing.T, kvState string, real bool) (*Server, *provPeer, *fakeWorkerDir, map[string]string) {
	t.Helper()
	s, prov, wk, hdr := hubSetup(t)
	tool := prov.peer.cfg.Provision.Tool
	tool.Real, tool.Provider, tool.Name = real, provision.KindKubeVirt, "kubevirt"
	pf := `{"v":1,"checked_at":"2026-11-20T12:00:00Z",
	 "aws":{"state":"missing","note":"aws is not installed"},"gcp":{"state":"missing"},
	 "tailnet":{"state":"ok","note":"OAuth client accepted for tag:worker"},
	 "openshell":{"state":"missing"},"ssh":{"state":"ok","note":"ssh key present"},
	 "kubevirt":{"state":"` + kvState + `","note":"in-cluster credentials; cluster answers; KubeVirt and CDI Deployed; namespace comebye-workers; 2 node(s) may take a VM"}}`
	wk.set(t, "preflight.json", pf)
	ev := ""
	for i, st := range provision.Stages {
		ev += `{"v":1,"seq":` + string(rune('1'+i)) + `,"stage":"` + st + `","step":"` + string(rune('1'+i)) + `/9","state":"ok"}` + "\n"
	}
	wk.set(t, "create.events", ev)
	wk.set(t, "create.stderr", "")
	wk.set(t, "status.json", `{"v":1,"provider":"kubevirt","workers":[{"name":"kv2","state":"RUNNING","type":"","managed":true,"launched_at":"`+time.Now().Add(-30*time.Minute).UTC().Format(time.RFC3339)+`","last_stop_at":null,"caps":{}}]}`)
	tick(s)
	do(s, "POST", "/api/provision/refresh", hdr, `{"what":"nodes"}`)
	return s, prov, wk, hdr
}

func TestANodeWithAKubeVirtConfigOffersAKubeVirtVM(t *testing.T) {
	s, _, _, _ := kubeHub(t, "ok", true)
	var n nodesResp2
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	got := map[string]kindView{}
	for _, k := range n.Nodes[0].Kinds {
		got[k.Kind] = k
	}
	if !got["kubevirt"].OK || n.Nodes[0].Preflight.KubeVirt == nil || !strings.Contains(n.Nodes[0].Preflight.KubeVirt.Note, "in-cluster credentials") {
		t.Errorf("kinds: %+v %+v", got, n.Nodes[0].Preflight.KubeVirt)
	}
	if got["aws"].OK {
		t.Errorf("the in-cluster node offers AWS without a login: %+v", got["aws"])
	}
	// a node with no kubevirt config does not list the kind at all
	s2, _, _, _ := sandboxHub(t, "ok")
	getJSON(t, s2, "/api/provision/nodes", userHdr, &n)
	for _, k := range n.Nodes[0].Kinds {
		if k.Kind == "kubevirt" {
			t.Errorf("a node without a kubevirt config offers it: %+v", k)
		}
	}
	// a cluster that does not answer is shown, with why, and not offered
	s3, _, _, _ := kubeHub(t, "error", true)
	getJSON(t, s3, "/api/provision/nodes", userHdr, &n)
	for _, k := range n.Nodes[0].Kinds {
		if k.Kind == "kubevirt" && (k.OK || !strings.Contains(k.Why, "kubevirt error")) {
			t.Errorf("an unreachable cluster is offered: %+v", k)
		}
	}
}

// A dry-run plan with the in-cluster config: priced as nothing, the tool is
// handed three fields, and no create runs.
func TestAKubeVirtPlanIsADryRunThroughTheInClusterConfig(t *testing.T) {
	s, _, wk, hdr := kubeHub(t, "ok", false)
	wk.set(t, "create.events", plannedEvents())
	rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+kubeSpec+`}`)
	var pl struct {
		OK     bool            `json:"ok"`
		DryRun bool            `json:"dry_run"`
		Ready  bool            `json:"ready"`
		Cost   *provision.Cost `json:"cost"`
	}
	json.Unmarshal(rec.Body.Bytes(), &pl)
	if rec.Code != 200 || !pl.OK || !pl.DryRun || !pl.Ready || pl.Cost == nil || pl.Cost.USDHour != 0 || !strings.Contains(pl.Cost.Basis, "KubeVirt") {
		t.Fatalf("plan = %d %s", rec.Code, rec.Body)
	}
	if in := string(mustRead(t, wk.dir+"/spec.in")); in != `{"destroy_after_hours":72,"idle_minutes":120,"kind":"kubevirt","name":"kv2"}` {
		t.Errorf("the tool read: %s", in)
	}
	for _, l := range wk.argvLines() {
		if strings.Contains(l, " create ") && !strings.Contains(l, "--dry-run") {
			t.Errorf("a real create from a plan: %s", l)
		}
	}
	// a request that names a machine, a disk or a price is refused: this kind has none
	for _, extra := range []string{`,"type":"t3.large"`, `,"budget_cap_usd":10`, `,"data_disk_gb":50`, `,"spot":true`} {
		bad := strings.Replace(kubeSpec, `"kind":"kubevirt"`, `"kind":"kubevirt"`+extra, 1)
		if rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+bad+`}`); rec.Code != 422 {
			t.Errorf("%s = %d %s", extra, rec.Code, rec.Body)
		}
	}
}

// Create is off until -provision-create: the job runs the tool's dry run and
// nothing real, whatever the hub asks.
func TestKubeVirtCreateIsRefusedWithoutTheFlag(t *testing.T) {
	s, _, wk, hdr := kubeHub(t, "ok", false)
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+kubeSpec+`,"key":"kube-key-0001","ack":true}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 202 || !v.Dry {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, v.ID, jobOK)
	for _, l := range wk.argvLines() {
		if strings.Contains(l, " create ") && !strings.Contains(l, "--dry-run") {
			t.Errorf("a real create without -provision-create: %s", l)
		}
	}
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	for _, x := range h.Hosts {
		if x.Spend.USD != 0 || x.State == "destroying" {
			t.Errorf("a dry run left a record: %+v", x)
		}
	}
}

// The host-count cap holds; the cost cap is unchanged for the priced kinds and
// does not stop an unpriced KubeVirt VM (the quota and the host cap hold that).
func TestKubeVirtCreateIsHeldToTheHostCapAndTheCostCapIsUnchanged(t *testing.T) {
	s, prov, wk, hdr := kubeHub(t, "ok", true)
	prov.peer.cfg.Provision.Tool.MaxHosts = 1
	prov.peer.cfg.Provision.Tool.MaxUSDHour = 0.0001 // far under any priced VM
	// the cost cap still refuses a priced VM...
	wk.set(t, "catalog.json", string(mustRead(t, provGolden)))
	if rec := do(s, "POST", "/api/provision/plan", hdr, `{"spec":`+okSpec+`,"region":"us-east-1"}`); !strings.Contains(rec.Body.String(), "over_ceiling") {
		t.Errorf("a priced VM was not held to the cost cap: %d %s", rec.Code, rec.Body)
	}
	// ...but a KubeVirt VM has no price to hold
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+kubeSpec+`,"key":"kube-key-0002","ack":true}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 202 || v.Dry {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, v.ID, jobOK)
	creates := wk.runs("create --events")
	rec = do(s, "POST", "/api/provision", hdr, `{"spec":`+strings.Replace(kubeSpec, "kv2", "kv3", 1)+`,"key":"kube-key-0003","ack":true}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "too_many_hosts") {
		t.Fatalf("a second host over the cap = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("create --events") != creates {
		t.Errorf("the tool ran for a refused host: %v", wk.argvLines())
	}
}

func TestKubeVirtNeedsItsClusterCheckNotAnAWSLogin(t *testing.T) {
	s, _, wk, hdr := kubeHub(t, "error", true)
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+kubeSpec+`,"key":"kube-key-0004","ack":true}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "kubevirt error") {
		t.Errorf("create with a cluster that does not answer = %d %s", rec.Code, rec.Body)
	}
	if wk.runs("create --events") != 0 {
		t.Error("the tool ran")
	}
}

// Destroy goes through the same job; the data disk is kept unless asked.
func TestKubeVirtDestroyKeepsTheDataDiskUnlessAsked(t *testing.T) {
	s, _, wk, hdr := kubeHub(t, "ok", true)
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+kubeSpec+`,"key":"kube-key-0005","ack":true}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	hubWait(t, s, v.ID, jobOK)
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	if len(h.Hosts) != 1 || h.Hosts[0].Kind != "kubevirt" || h.Hosts[0].Mode != "sandbox" || h.Hosts[0].State != "running" {
		t.Fatalf("host: %+v", h.Hosts)
	}
	rec = do(s, "POST", "/api/provision/hosts/kv2/destroy", hdr, `{"confirm":"kv2"}`)
	var j hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != 202 {
		t.Fatalf("destroy = %d %s", rec.Code, rec.Body)
	}
	hubWait(t, s, j.ID, jobOK)
	argv := strings.Join(wk.argvLines(), "\n")
	if !strings.Contains(argv, "destroy kv2 --yes --events") || strings.Contains(argv, "--delete-data-disk") {
		t.Errorf("argv: %s", argv)
	}
	h = hostsResp{}
	getJSON(t, s, "/api/provision/hosts", userHdr, &h)
	if h.Hosts[0].State != "destroyed" || !h.Hosts[0].DataKept {
		t.Errorf("the kept data disk is not shown: %+v", h.Hosts[0])
	}
}

// Nothing a tool prints about a token, a key or the OAuth client reaches a
// job's journal, even from a KubeVirt create.
func TestKubeVirtJobOutputCarriesNoTokenKeyOrOAuthValue(t *testing.T) {
	s, _, wk, hdr := kubeHub(t, "ok", true)
	wk.set(t, "create.events", `{"v":1,"seq":1,"stage":"tailnet","step":"7/9","state":"running","msg":"minted tskey-auth-kABC123CNTRL-SECRETSECRET with client tskey-client-kXYZ789CNTRL-OAUTHSECRET; Bearer eyJhbGciOi.payload.sig"}`+"\n"+
		`{"v":1,"seq":2,"stage":"instance","step":"4/9","state":"ok","msg":"client_secret=OAUTHVALUE token=SATOKEN123"}`+"\n")
	wk.set(t, "create.stderr", "using secret=hunter2 and password=hunter3; key=PRIVATEKEYBODY\nAuthorization: Bearer abc.def.ghi\n")
	rec := do(s, "POST", "/api/provision", hdr, `{"spec":`+kubeSpec+`,"key":"kube-key-0006","ack":true}`)
	var v hubJobResp
	json.Unmarshal(rec.Body.Bytes(), &v)
	done := hubWait(t, s, v.ID, jobOK)
	b, _ := json.Marshal(done)
	all := string(b)
	// and what the peer answers to the hub, before the hub copies it
	peer := do(s, "GET", "/api/provision/jobs/"+v.ID, userHdr, "")
	all += peer.Body.String()
	for _, bad := range []string{"tskey-", "SECRETSECRET", "OAUTHSECRET", "OAUTHVALUE", "SATOKEN123", "hunter2", "hunter3", "PRIVATEKEYBODY", "eyJhbGciOi", "abc.def.ghi"} {
		if strings.Contains(all, bad) {
			t.Errorf("%q reached the job output: %s", bad, all)
		}
	}
}
