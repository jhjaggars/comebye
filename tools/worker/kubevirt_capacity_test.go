package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The capacity gate on a VM that exists already, Longhorn's room for the
// disks a create still has to make, and the namespace's LimitRange and
// ResourceQuota. Every cluster here is a fake kubectl; no VM is made.

func TestKubeGateDoesNotCountTheVMAgainstItself(t *testing.T) {
	c := kubeCfg(t) // 32Gi VM: 32.5 GiB with the overhead; node-a's floor is 12
	need := 32*gib + podOverheadBytes

	// node-a: 63 GiB, 8 GiB of other pods plus the VM's own pod. Counted
	// against itself it leaves 63-8-32.5-32.5 < 12; without, 22.5 >= 12.
	n := node("node-a.x", 63, 8)
	n.Requested += need
	n.OwnRequested = need
	n.Usage = 8*gib + need
	n.OwnUsage = need
	in := gateInput{Nodes: []kubeNode{n, node("node-b.x", 63, 0)}, UsageKnown: true, Pinned: "node-a.x"}
	g := kubeGate(c, in)
	if g.Chosen != "node-a.x" || g.Err() != nil {
		t.Fatalf("the resumed VM was refused on its own node: %q %v %+v", g.Chosen, g.Err(), g.Rows)
	}
	if g.Own != need {
		t.Errorf("own pod %d, want %d", g.Own, need)
	}
	// the old gate's answer, for the record: the VM's pod counted twice
	n2 := n
	n2.OwnRequested, n2.OwnUsage = 0, 0
	if g := kubeGate(c, gateInput{Nodes: []kubeNode{n2}, UsageKnown: true, Pinned: "node-a.x"}); g.Err() == nil {
		t.Error("with its own pod counted the VM still fit: the test proves nothing")
	}
	// measured use under the request: the VM's measured share comes off, not its request
	n3 := n
	n3.Usage, n3.OwnUsage = 30*gib, 4*gib // others use 26 GiB: 63-26-32.5 = 4.5 < 12
	if g := kubeGate(c, gateInput{Nodes: []kubeNode{n3}, UsageKnown: true, Pinned: "node-a.x"}); g.Err() == nil {
		t.Error("other pods' measured use was credited to the VM")
	}

	// pinned: the other node, roomier and preferred, is not looked at
	in.Nodes = []kubeNode{node("node-b.x", 200, 0), n}
	if g = kubeGate(c, in); g.Chosen != "node-a.x" {
		t.Errorf("pinned VM placed on %q", g.Chosen)
	}
	for _, r := range g.Rows {
		if r.Node == "node-b.x" && (r.OK || !strings.Contains(r.Reason, "pinned to node-a")) {
			t.Errorf("node-b: %+v", r)
		}
	}
	// pinned to a node that is cordoned: refused, with the reason, not moved
	nc := n
	nc.Blocked = "cordoned"
	in.Nodes = []kubeNode{node("node-b.x", 200, 0), nc}
	if g = kubeGate(c, in); g.Chosen != "" || !strings.Contains(g.Err().Error(), "node-a: cordoned") {
		t.Errorf("cordoned pin: %q %v", g.Chosen, g.Err())
	}
	// a stopped VM (no pod): its node must still have room for it
	ns := node("node-a.x", 63, 25) // 63-25-32.5 = 5.5 < 12
	if g = kubeGate(c, gateInput{Nodes: []kubeNode{ns}, UsageKnown: true, Pinned: "node-a.x"}); g.Err() == nil {
		t.Error("a stopped VM was let back onto a full node")
	}
}

func TestFitDisks(t *testing.T) {
	boot := diskNeed{What: "boot disk", Class: "fast", Bytes: 42 * gib, Longhorn: true, Selector: []string{"nvme"}}
	data := diskNeed{What: "data disk", Class: "local", Bytes: 64 * gib, Longhorn: true, Selector: []string{"sata"}}
	n := kubeNode{Name: "n", Longhorn: []lhDisk{
		{Name: "d-nvme", Tags: []string{"nvme"}, Room: 50 * gib},
		{Name: "d-sata", Tags: []string{"sata", "slow"}, Room: 70 * gib},
	}}
	if why := fitDisks(n, []diskNeed{boot, data}); why != "" {
		t.Errorf("fitting disks refused: %s", why)
	}
	// the data disk does not fit its tagged disk
	data.Bytes = 71 * gib
	if why := fitDisks(n, []diskNeed{boot, data}); !strings.Contains(why, "data disk needs 71.0 GiB") || !strings.Contains(why, "70.0 GiB schedulable") {
		t.Errorf("full sata disk: %q", why)
	}
	// both on one disk: each fits, the two together do not
	data = diskNeed{What: "data disk", Class: "fast", Bytes: 20 * gib, Longhorn: true, Selector: []string{"nvme"}}
	if why := fitDisks(n, []diskNeed{boot, data}); !strings.Contains(why, "data disk") {
		t.Errorf("42+20 GiB on a 50 GiB disk: %q", why)
	}
	// no disk with the tag
	if why := fitDisks(n, []diskNeed{{What: "data disk", Class: "x", Bytes: 1, Longhorn: true, Selector: []string{"ssd"}}}); !strings.Contains(why, "no schedulable Longhorn disk tagged ssd") {
		t.Errorf("missing tag: %q", why)
	}
	// a disk not on Longhorn is not checked here
	if why := fitDisks(kubeNode{}, []diskNeed{{What: "data disk", Bytes: 1 << 50}}); why != "" {
		t.Errorf("non-Longhorn disk checked: %q", why)
	}
	// a node with no Longhorn disks at all
	if why := fitDisks(kubeNode{Longhorn: []lhDisk{}}, []diskNeed{boot}); !strings.Contains(why, "no schedulable Longhorn disk tagged nvme") {
		t.Errorf("no disks: %q", why)
	}
}

// lhNodesJSON is Longhorn's node list: one node with a big nvme disk, a
// disabled disk and a full sata disk; one node that is not schedulable.
const lhNodesJSON = `{"items":[
 {"metadata":{"name":"node-a.x.cc"},
  "spec":{"allowScheduling":true,"disks":{
    "d-nvme":{"allowScheduling":true,"storageReserved":0,"tags":["nvme"]},
    "d-off":{"allowScheduling":false,"storageReserved":0,"tags":[]},
    "d-sata":{"allowScheduling":true,"storageReserved":107374182400,"tags":["sata"]}}},
  "status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"True"}],"diskStatus":{
    "d-nvme":{"storageAvailable":429496729600,"storageMaximum":644245094400,"storageScheduled":214748364800,"conditions":[{"type":"Schedulable","status":"True"}]},
    "d-off":{"storageAvailable":999999999999,"storageMaximum":999999999999,"storageScheduled":0,"conditions":[{"type":"Schedulable","status":"True"}]},
    "d-sata":{"storageAvailable":322122547200,"storageMaximum":429496729600,"storageScheduled":300647710720,"conditions":[{"type":"Schedulable","status":"True"}]}}}},
 {"metadata":{"name":"node-b.x.cc"},
  "spec":{"allowScheduling":true,"disks":{"d1":{"allowScheduling":true,"tags":["nvme"]}}},
  "status":{"conditions":[{"type":"Schedulable","status":"False"}],"diskStatus":{
    "d1":{"storageAvailable":999999999999,"storageMaximum":999999999999,"storageScheduled":0}}}}]}`

func TestLonghornDisksReads(t *testing.T) {
	w := goodWorld()
	w.longhorn = lhNodesJSON
	w.lhSettings = map[string]string{"storage-over-provisioning-percentage": "100", "storage-minimal-available-percentage": "25"}
	e, _, _, _ := kubeEnv(t, w)
	k := &kubeCLI{r: e.Run, cfg: e.Cfg}
	got, err := k.longhornDisks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := got["node-a.x.cc"]
	if len(a) != 2 || a[0].Name != "d-nvme" || a[1].Name != "d-sata" {
		t.Fatalf("node-a disks %+v", a)
	}
	// nvme: 600 GiB max, 200 scheduled -> 400 by over-provisioning; 400 available
	// less 25% of 600 -> 250. The smaller wins.
	if a[0].Room != 250*gib {
		t.Errorf("nvme room %s", fmtGiB(a[0].Room))
	}
	// sata: (400-100 reserved) - 280 scheduled = 20 GiB; 300 - 100 = 200 available.
	if a[1].Room != 20*gib {
		t.Errorf("sata room %s", fmtGiB(a[1].Room))
	}
	if b, ok := got["node-b.x.cc"]; !ok || len(b) != 0 {
		t.Errorf("an unschedulable Longhorn node offers %+v", b)
	}
	// more over-provisioning: the scheduled rule loosens, the available rule stays
	w.lhSettings["storage-over-provisioning-percentage"] = "200"
	got, _ = k.longhornDisks(context.Background())
	if r := got["node-a.x.cc"][1].Room; r != 200*gib {
		t.Errorf("sata room at 200%%: %s", fmtGiB(r))
	}
}

// vmJSON is a VM of ours pinned to node.
func vmJSON(name, node, status string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"uid":"u1","labels":{%q:%q}},"spec":{"runStrategy":"RerunOnFailure","template":{"spec":{"domain":{"cpu":{"cores":4},"resources":{"limits":{"memory":"32Gi"}}},"nodeSelector":{"kubernetes.io/hostname":%q}}}},"status":{"printableStatus":%q}}`,
		name, labelKey, labelVal, node, status)
}

// launcherPodsJSON is the cluster's pods: one other pod and the VM's own
// virt-launcher pod, both on node.
func launcherPodsJSON(node, vm, ns, other, own string) string {
	return fmt.Sprintf(`{"items":[
	 {"metadata":{"name":"other","namespace":"default"},"spec":{"nodeName":%q,"containers":[{"resources":{"requests":{"memory":%q}}}]}},
	 {"metadata":{"name":"virt-launcher-%s-abcde","namespace":%q,"labels":{"vm.kubevirt.io/name":%q}},"spec":{"nodeName":%q,"containers":[{"resources":{"requests":{"memory":%q}}}]}}]}`,
		node, other, vm, ns, vm, node, own)
}

func TestKubeVirtResumedDryRunPassesTheGate(t *testing.T) {
	w := goodWorld()
	w.nodes = nodesJSON([3]string{"node-a.x.cc", "63Gi", "1k"}, [3]string{"node-b.x.cc", "200Gi", "1k"})
	w.vm = vmJSON("w1", "node-a.x.cc", "Running")
	w.pods = launcherPodsJSON("node-a.x.cc", "w1", "comebye-workers", "8Gi", "33280Mi")
	w.metrics = `{"items":[{"metadata":{"name":"node-a.x.cc"},"usage":{"memory":"41Gi"}}]}`
	w.podMetrics = `{"containers":[{"name":"compute","usage":{"memory":"32Gi"}}]}`
	e, r, out, _ := kubeEnv(t, w)
	if err := e.create(context.Background(), "w1", createOpts{Version: "v0.3.1"}); err != nil {
		t.Fatalf("a resumed create was refused: %v\n%s", err, out())
	}
	o := out()
	for _, want := range []string{"-> node-a", "pinned to node-a", "the VM exists on node-a", "its own pod (32.5 GiB) is not counted"} {
		if !strings.Contains(o, want) {
			t.Errorf("plan lacks %q:\n%s", want, o)
		}
	}
	if !r.saw("get --raw /apis/metrics.k8s.io/v1beta1/namespaces/comebye-workers/pods/virt-launcher-w1-abcde") {
		t.Error("the VM pod's measured use was not read")
	}

	// kubeNodes keeps the own pod apart, and only for the named VM
	k := &kubeCLI{r: e.Run, cfg: e.Cfg}
	nodes, _, err := k.kubeNodes(context.Background(), "w1")
	if err != nil || nodes[0].OwnRequested != 33280*mib || nodes[0].OwnUsage != 32*gib || nodes[0].Requested != 8*gib+33280*mib {
		t.Errorf("node-a %+v %v", nodes[0], err)
	}
	nodes, _, _ = k.kubeNodes(context.Background(), "w2")
	if nodes[0].OwnRequested != 0 || nodes[0].OwnUsage != 0 {
		t.Errorf("another VM's pod credited: %+v", nodes[0])
	}
	// a pod of the same VM name in another namespace is not ours
	w.pods = launcherPodsJSON("node-a.x.cc", "w1", "elsewhere", "8Gi", "33280Mi")
	if nodes, _, _ = k.kubeNodes(context.Background(), "w1"); nodes[0].OwnRequested != 0 {
		t.Errorf("a pod outside the namespace credited: %+v", nodes[0])
	}
}

// diskWorld is a new VM whose two disks do not exist yet, on Longhorn classes.
func diskWorld() *kubeWorld {
	w := goodWorld()
	w.nodes = nodesJSON([3]string{"node-a.x.cc", "65471768Ki", "1k"})
	w.absent["datavolume/w1-boot"], w.absent["pvc/w1-boot"] = true, true
	w.absent["datavolume/w1-data"], w.absent["pvc/w1-data"] = true, true
	w.classes = map[string]string{"longhorn": `{"provisioner":"driver.longhorn.io","parameters":{"diskSelector":"nvme","numberOfReplicas":"3"}}`}
	w.longhorn = lhNodesJSON
	return w
}

func TestKubeVirtGateChecksLonghorn(t *testing.T) {
	ctx := context.Background()
	// the data disk on a class with a sata selector: node-a's sata disk has 20 GiB
	w := diskWorld()
	w.classes["scratch"] = `{"provisioner":"driver.longhorn.io","parameters":{"diskSelector":"sata","numberOfReplicas":"1","dataLocality":"strict-local"}}`
	e, _, out, _ := kubeEnv(t, w)
	e.Cfg.KubeDataStorageClass = "scratch"
	err := e.create(ctx, "w1", createOpts{Version: "v0.3.1"})
	if err == nil || !strings.Contains(err.Error(), "node-a: Longhorn: the data disk needs") || !strings.Contains(err.Error(), "tagged sata has 20.0 GiB schedulable") {
		t.Errorf("a data disk with no room passed: %v\n%s", err, out())
	}
	if !strings.Contains(out(), "data disk to make:") {
		t.Errorf("the plan does not name the disk it checked:\n%s", out())
	}

	// both on the nvme disk (250 GiB): fits
	w = diskWorld()
	e, _, out, _ = kubeEnv(t, w)
	if err := e.create(ctx, "w1", createOpts{Version: "v0.3.1"}); err != nil {
		t.Errorf("roomy nvme refused: %v\n%s", err, out())
	}
	// the data disk exists already (a resume): only the boot disk is counted
	w = diskWorld()
	w.classes["scratch"] = w.classes["longhorn"]
	delete(w.absent, "pvc/w1-data")
	w.longhorn = strings.Replace(lhNodesJSON, `"storageScheduled":214748364800`, `"storageScheduled":558345748480`, 1) // nvme: 600-520 = 80 GiB
	e, _, out, _ = kubeEnv(t, w)
	e.Cfg.DataDiskGB = 100
	if err := e.create(ctx, "w1", createOpts{Version: "v0.3.1"}); err != nil {
		t.Errorf("an existing data disk was counted again: %v\n%s", err, out())
	}
	if strings.Contains(out(), "data disk to make") {
		t.Errorf("the existing data disk is listed as to make:\n%s", out())
	}

	// Longhorn unreadable: a note, not a refusal
	w = diskWorld()
	w.longhorn = ""
	e, _, out, _ = kubeEnv(t, w)
	if err := e.create(ctx, "w1", createOpts{Version: "v0.3.1"}); err != nil || !strings.Contains(out(), "Longhorn could not be read") {
		t.Errorf("unreadable Longhorn: %v\n%s", err, out())
	}
	// a class that is not Longhorn's is not checked against Longhorn
	w = diskWorld()
	w.classes["longhorn"] = `{"provisioner":"rancher.io/local-path"}`
	w.longhorn = ""
	e, r, out, _ := kubeEnv(t, w)
	if err := e.create(ctx, "w1", createOpts{Version: "v0.3.1"}); err != nil || r.saw("nodes.longhorn.io") {
		t.Errorf("a local-path class: %v\n%s", err, out())
	}
}

func TestQuotaMisfits(t *testing.T) {
	c := kubeCfg(t) // 32Gi, vm_cpu 4, request 2
	disks := []diskNeed{{What: "boot disk", Bytes: 42 * gib}, {What: "data disk", Bytes: 160 * gib}}
	lr := `{"items":[{"metadata":{"name":"lr"},"spec":{"limits":[
	  {"type":"Container","max":{"cpu":"2","memory":"16Gi"}},
	  {"type":"PersistentVolumeClaim","max":{"storage":"150Gi"}}]}}]}`
	bad := strings.Join(quotaMisfits(c, false, false, disks, lr, ""), "\n")
	for _, want := range []string{"LimitRange lr allows a container at most 16.0 GiB of memory, the VM's pod needs 32.5 GiB", "at most 2 CPU, vm_cpu is 4", "a claim at most 150.0 GiB, the data disk claims 160.0 GiB"} {
		if !strings.Contains(bad, want) {
			t.Errorf("lacks %q:\n%s", want, bad)
		}
	}
	if strings.Contains(bad, "boot disk") {
		t.Errorf("the boot disk fits the claim limit:\n%s", bad)
	}
	// a running VM's pod was admitted already: only its disks are checked
	if bad := quotaMisfits(c, true, true, nil, lr, ""); len(bad) != 0 {
		t.Errorf("running VM: %v", bad)
	}

	q := `{"items":[{"metadata":{"name":"rq"},"status":{
	  "hard":{"requests.memory":"60Gi","limits.cpu":"24","requests.cpu":"12","pods":"12","count/virtualmachines.kubevirt.io":"2","requests.storage":"1Ti","persistentvolumeclaims":"16"},
	  "used":{"requests.memory":"30Gi","limits.cpu":"4015m","requests.cpu":"2005m","pods":"1","count/virtualmachines.kubevirt.io":"2","requests.storage":"900Gi","persistentvolumeclaims":"7"}}}]}`
	bad = strings.Join(quotaMisfits(c, false, false, disks, "", q), "\n")
	for _, want := range []string{
		"ResourceQuota rq has 30.0 GiB of requests.memory left, the VM needs 32.5 GiB",
		"ResourceQuota rq has 0 of count/virtualmachines.kubevirt.io left, the VM needs 1",
		"ResourceQuota rq has 124.0 GiB of requests.storage left, the VM needs 202.0 GiB",
	} {
		if !strings.Contains(bad, want) {
			t.Errorf("lacks %q:\n%s", want, bad)
		}
	}
	if strings.Contains(bad, "cpu") || strings.Contains(bad, "pods") {
		t.Errorf("fitting resources refused:\n%s", bad)
	}
	// the VM exists and runs, its disks exist: nothing new to count
	if bad := quotaMisfits(c, true, true, nil, "", q); len(bad) != 0 {
		t.Errorf("a running VM counted against the quota again: %v", bad)
	}
	// the VM exists but is stopped: its pod is new, its VM count is not
	bad = strings.Join(quotaMisfits(c, true, false, nil, "", q), "\n")
	if !strings.Contains(bad, "requests.memory") || strings.Contains(bad, "virtualmachines") {
		t.Errorf("stopped VM: %s", bad)
	}
}

func TestKubeVirtGateRefusesOnTheNamespaceQuota(t *testing.T) {
	w := goodWorld()
	w.quotas = `{"items":[{"metadata":{"name":"rq"},"status":{"hard":{"requests.memory":"40Gi"},"used":{"requests.memory":"30Gi"}}}]}`
	e, _, out, _ := kubeEnv(t, w)
	err := e.create(context.Background(), "w1", createOpts{Version: "v0.3.1"})
	if err == nil || !strings.Contains(err.Error(), "the namespace cannot take the VM: ResourceQuota rq has 10.0 GiB of requests.memory left") {
		t.Errorf("quota: %v\n%s", err, out())
	}
	if !strings.Contains(out(), "pinned to node <node chosen by the gate>") {
		t.Errorf("a node was chosen despite the quota:\n%s", out())
	}
}

// create runs the gate before it makes the data disk, so a VM that cannot fit
// leaves nothing behind.
func TestKubeVirtCreateGatesBeforeTheDataDisk(t *testing.T) {
	w := newLiveWorld()
	w.nodes = nodesJSON([3]string{"node-a.x.cc", "65471768Ki", "1k"})
	w.classes = map[string]string{"longhorn": `{"provisioner":"driver.longhorn.io","parameters":{"diskSelector":"ssd"}}`}
	w.longhorn = lhNodesJSON
	e, _ := liveEnv(t, w)
	err := e.create(context.Background(), "kv1", createOpts{Version: "v0.3.1"})
	if err == nil || !strings.Contains(err.Error(), "no schedulable Longhorn disk tagged ssd") {
		t.Fatalf("create: %v", err)
	}
	if len(w.applied) != 0 {
		t.Errorf("objects made before the gate refused: %v", w.applied)
	}
}
