package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func kubeCfg(t *testing.T) *Config {
	t.Helper()
	c := testCfg(t)
	c.Provider = "kubevirt"
	c.Project, c.Zone, c.SSHSourceCIDR = "", "", ""
	c.finish()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKubeVirtConfig(t *testing.T) {
	c := kubeCfg(t)
	if c.VMCPU != 4 || c.VMCPURequest != 2 || c.VMMemory != "32Gi" || c.BootDiskGB != 40 || c.DataDiskGB != 150 || c.VMImageDV == "" {
		t.Errorf("defaults: %+v", c)
	}
	bad := map[string]func(*Config){
		"both images":      func(c *Config) { c.VMImageURL = "https://x.example/a.qcow2" },
		"no image":         func(c *Config) { c.VMImageDV = "" },
		"http image":       func(c *Config) { c.VMImageDV, c.VMImageURL = "", "http://x.example/a.qcow2" },
		"memory units":     func(c *Config) { c.VMMemory = "32G" },
		"memory tiny":      func(c *Config) { c.VMMemory = "1Gi" },
		"request over cpu": func(c *Config) { c.VMCPURequest = 8 },
		"no cpu":           func(c *Config) { c.VMCPU = 0 },
		"ns shell":         func(c *Config) { c.KubeNamespace = "a;b" },
		"ctx shell":        func(c *Config) { c.KubeContext = "a b" },
		"node shell":       func(c *Config) { c.KubeNodePrefer = []string{"a;b"} },
		"prefer and avoid": func(c *Config) { c.KubeNodePrefer, c.KubeNodeAvoid = []string{"x"}, []string{"x"} },
		"floor zero":       func(c *Config) { c.KubeFloorGiB = 0 },
		"llm without pi":   func(c *Config) { c.LLMGatewayURL, c.Agents = "https://l.t.ts.net", []string{"claude"} },
	}
	for name, mut := range bad {
		cc := *c
		mut(&cc)
		if cc.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// ssh_source_cidr, project and zone do not apply.
	cc := *c
	cc.SSHSourceCIDR, cc.Project, cc.Zone = "", "", ""
	if err := cc.Validate(); err != nil {
		t.Errorf("gcp settings required: %v", err)
	}
}

func TestKubeVirtConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config")
	body := "provider=kubevirt\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=https://h.t.ts.net\n" +
		"kube_context=homelab\nkube_namespace=ws\nkube_node_prefer=etna,rozalin\nkube_node_avoid=adell\nkube_floor_gib=16\nkube_node_floor=etna=6,rozalin=9\nvm_memory=24Gi\nvm_cpu=6\nvm_cpu_request=3\ndata_disk_gb=200\n"
	os.WriteFile(p, []byte(body), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.KubeContext != "homelab" || c.KubeNamespace != "ws" || c.VMCPU != 6 || c.DataDiskGB != 200 || c.BootDiskGB != 40 ||
		len(c.KubeNodePrefer) != 2 || c.floorBytes("etna.x.cc") != 6*gib || c.floorBytes("rozalin") != 9*gib || c.floorBytes("other") != 16*gib {
		t.Errorf("parsed wrong: %+v", c)
	}
	// the default zetta floor is replaced, not merged, once a floor is given
	if c.floorBytes("zetta") != 16*gib {
		t.Errorf("default floors leaked into a configured set")
	}
	os.WriteFile(p, []byte(body+"kube_node_floor=oops\n"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Error("bad kube_node_floor accepted")
	}
	// a gcp config is unaffected
	if g := testCfg(t); g.Provider != "gcp" || g.BootDiskGB != 20 {
		t.Errorf("gcp defaults changed: %+v", g)
	}
}

func TestParseQty(t *testing.T) {
	for in, want := range map[string]int64{"131810636Ki": 131810636 << 10, "32Gi": 32 << 30, "512Mi": 512 << 20, "1G": 1e9, "100": 100, "500m": 0, "1500m": 1} {
		if got, err := parseQty(in); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Gi", "x", "1.2.3"} {
		if _, err := parseQty(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// node is a kubeNode with memory in GiB.
func node(name string, alloc, used int64) kubeNode {
	return kubeNode{Name: name, Alloc: alloc * gib, Requested: used * gib, KVM: true, Ready: true}
}

func TestKubeGate(t *testing.T) {
	c := kubeCfg(t) // 32Gi VM, prefer rozalin,zetta, floor 12 (zetta 8), avoid adell,etna
	need := 32*gib + podOverheadBytes

	// rozalin has room: chosen first even though etna has more.
	g := kubeGate(c, []kubeNode{node("etna.x", 94, 20), node("rozalin.x", 63, 8), node("zetta.x", 63, 21), node("adell.x", 126, 94)}, true)
	if g.Chosen != "rozalin.x" || g.Err() != nil || g.Need != need {
		t.Fatalf("chose %q (%v)", g.Chosen, g.Err())
	}
	for _, r := range g.Rows {
		if (r.Node == "etna.x" || r.Node == "adell.x") && (r.OK || !strings.Contains(r.Reason, "avoid")) {
			t.Errorf("%s not refused as avoided: %+v", r.Node, r)
		}
	}

	// the floor is exact: 63 - used - 32.5 must be >= 12 on rozalin.
	roz := func(used int64) gateResult {
		return kubeGate(c, []kubeNode{{Name: "rozalin", Alloc: 63 * gib, Requested: used, KVM: true, Ready: true}}, true)
	}
	edge := 63*gib - need - 12*gib
	if roz(edge).Chosen == "" {
		t.Error("exactly at the floor was refused")
	}
	if roz(edge+1).Chosen != "" {
		t.Error("one byte under the floor was accepted")
	}

	// rozalin too full: fall back to zetta, whose floor is 8.
	g = kubeGate(c, []kubeNode{node("rozalin", 63, 30), node("zetta", 63, 22)}, true)
	if g.Chosen != "zetta" { // 63-22-32.5 = 8.5 >= 8, but < 12
		t.Errorf("zetta fallback with the 8 GiB floor: chose %q", g.Chosen)
	}
	// ... and the same free memory on rozalin (floor 12) is refused.
	g = kubeGate(c, []kubeNode{node("rozalin", 63, 22)}, true)
	if g.Chosen != "" || g.Err() == nil || !strings.Contains(g.Err().Error(), "under the 12.0 GiB floor") {
		t.Errorf("rozalin at 8.5 GiB after: %q %v", g.Chosen, g.Err())
	}
	// zetta under its own floor is refused.
	if g = kubeGate(c, []kubeNode{node("zetta", 63, 23)}, true); g.Chosen != "" {
		t.Errorf("zetta at 7.5 GiB after was accepted")
	}

	// measured use beats the requests
	n := node("rozalin", 63, 5)
	n.Usage = 50 * gib
	if g = kubeGate(c, []kubeNode{n}, true); g.Chosen != "" {
		t.Error("measured use ignored")
	}

	// other reasons
	for name, mut := range map[string]func(*kubeNode){
		"no kvm":   func(n *kubeNode) { n.KVM = false },
		"not rdy":  func(n *kubeNode) { n.Ready = false },
		"cordoned": func(n *kubeNode) { n.Blocked = "cordoned" },
	} {
		n := node("rozalin", 63, 1)
		mut(&n)
		if g = kubeGate(c, []kubeNode{n}, true); g.Chosen != "" || g.Err() == nil {
			t.Errorf("%s: node accepted", name)
		}
	}
	// a node the config does not list is never chosen while prefer is set
	if g = kubeGate(c, []kubeNode{node("other", 500, 0)}, true); g.Chosen != "" {
		t.Error("unlisted node chosen")
	}
	// with no prefer list, any node but the avoided: most free first
	c2 := *c
	c2.KubeNodePrefer = nil
	g = kubeGate(&c2, []kubeNode{node("a", 63, 30), node("b", 63, 2), node("etna", 99, 0)}, true)
	if g.Chosen != "b" {
		t.Errorf("no prefer list: chose %q", g.Chosen)
	}
	if kubeGate(c, nil, true).Err() == nil {
		t.Error("an empty cluster was not refused")
	}
}

// ---- a fake cluster ----

func nodesJSON(rows ...[3]string) string { // name, memory, kvm
	var items []string
	for _, r := range rows {
		items = append(items, fmt.Sprintf(`{"metadata":{"name":%q},"spec":{},"status":{"allocatable":{"memory":%q,"devices.kubevirt.io/kvm":%q},"conditions":[{"type":"Ready","status":"True"}]}}`, r[0], r[1], r[2]))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func podsJSON(node string, mem ...string) string {
	var cs []string
	for _, m := range mem {
		cs = append(cs, fmt.Sprintf(`{"resources":{"requests":{"memory":%q,"cpu":"1"}}}`, m))
	}
	return fmt.Sprintf(`{"items":[{"spec":{"nodeName":%q,"containers":[%s]}},{"spec":{"containers":[{"resources":{"requests":{"memory":"900Gi"}}}]}}]}`, node, strings.Join(cs, ","))
}

type kubeWorld struct {
	absent   map[string]bool // "kind/name" the cluster does not have
	denied   map[string]bool // verbs that fail (not NotFound)
	kv, cdi  string
	nodes    string
	pods     string
	metrics  string // "" = the API is absent
	noServer bool
}

func (w *kubeWorld) fake() *fakeRunner {
	return &fakeRunner{handle: func(c call, f *fakeRunner) (string, string, error) {
		if c.name != "kubectl" {
			return "", "", fmt.Errorf("unexpected %s", c.line())
		}
		if len(c.args) >= 2 && c.args[0] == "--context" {
			c.args = c.args[2:]
		}
		line := strings.Join(c.args, " ")
		switch {
		case w.noServer:
			return "", "Unable to connect to the server", errors.New("exit 1")
		case strings.HasPrefix(line, "get --raw /version"):
			return `{"major":"1"}`, "", nil
		case strings.HasPrefix(line, "get kubevirt"):
			return w.kv, "", nil
		case strings.HasPrefix(line, "get cdi"):
			return w.cdi, "", nil
		case strings.HasPrefix(line, "get nodes"):
			return w.nodes, "", nil
		case strings.HasPrefix(line, "get pods"):
			return w.pods, "", nil
		case strings.HasPrefix(line, "get --raw /apis/metrics"):
			if w.metrics == "" {
				return "", "the server could not find the requested resource", errors.New("exit 1")
			}
			return w.metrics, "", nil
		case strings.Contains(line, " get ") || strings.HasPrefix(line, "get "):
			// get [-n NS] KIND NAME -o name
			f := strings.Fields(line)
			kind, name := "", ""
			for i := 0; i < len(f); i++ {
				if f[i] == "get" {
					a := f[i+1:]
					for len(a) > 0 && a[0] == "-n" {
						a = a[2:]
					}
					kind, name = a[0], a[1]
					break
				}
			}
			if w.denied[kind] {
				return "", "Error from server (Forbidden)", errors.New("exit 1")
			}
			if w.absent[kind+"/"+name] {
				return "", fmt.Sprintf("Error from server (NotFound): %s %q not found", kind, name), errors.New("exit 1")
			}
			return kind + "/" + name, "", nil
		}
		return "", "", fmt.Errorf("unexpected %s", line)
	}}
}

func goodWorld() *kubeWorld {
	return &kubeWorld{
		absent: map[string]bool{}, denied: map[string]bool{}, kv: "Deployed", cdi: "Deployed",
		nodes:   nodesJSON([3]string{"rozalin.x.cc", "65471768Ki", "1k"}, [3]string{"zetta.x.cc", "65450024Ki", "1k"}, [3]string{"etna.x.cc", "98518240Ki", "1k"}),
		pods:    podsJSON("rozalin.x.cc", "3Gi", "512Mi"),
		metrics: `{"items":[{"metadata":{"name":"rozalin.x.cc"},"usage":{"memory":"10000000Ki"}}]}`,
	}
}

func kubeEnv(t *testing.T, w *kubeWorld) (*env, *fakeRunner, func() string, func() string) {
	t.Helper()
	c := kubeCfg(t)
	r := w.fake()
	e, out, errb := newEnv(t, c, &fakeWorld{hubVersion: "v0.3.1"}, r)
	e.DryRun = true
	return e, r, out.String, errb.String
}

func withPath(t *testing.T, missing string) {
	t.Helper()
	old := lookPath
	lookPath = func(b string) (string, error) {
		if b == missing {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + b, nil
	}
	t.Cleanup(func() { lookPath = old })
}

func TestKubeNodesReads(t *testing.T) {
	e, _, _, _ := kubeEnv(t, goodWorld())
	k := &kubeCLI{r: e.Run, cfg: e.Cfg}
	nodes, usage, err := k.kubeNodes(context.Background())
	if err != nil || !usage || len(nodes) != 3 {
		t.Fatalf("%v %v %v", nodes, usage, err)
	}
	roz := nodes[0]
	if roz.Name != "rozalin.x.cc" || roz.Requested != 3*gib+512*mib || roz.Usage != 10000000<<10 || !roz.KVM || !roz.Ready {
		t.Errorf("rozalin: %+v", roz)
	}
	// a pod without a node (Pending) is not counted
	if nodes[1].Requested != 0 {
		t.Errorf("unscheduled pod counted on zetta: %+v", nodes[1])
	}
}

func TestPreflightKubeVirt(t *testing.T) {
	withPath(t, "")
	run := func(w *kubeWorld) pfCheck {
		e, _, _, _ := kubeEnv(t, w)
		return e.pfKubeVirt(context.Background())
	}
	if c := run(goodWorld()); c.State != "ok" || !strings.Contains(c.Note, "2 node(s)") {
		t.Errorf("good world: %+v", c)
	}
	w := goodWorld()
	w.noServer = true
	if c := run(w); c.State != "error" || !strings.Contains(c.Note, "did not answer") {
		t.Errorf("unreachable: %+v", c)
	}
	w = goodWorld()
	w.kv = ""
	if c := run(w); c.State != "error" || !strings.Contains(c.Note, "KubeVirt is not Deployed") {
		t.Errorf("no kubevirt: %+v", c)
	}
	w = goodWorld()
	w.cdi = "Deploying"
	if c := run(w); c.State != "error" || !strings.Contains(c.Note, "CDI is not Deployed") {
		t.Errorf("cdi: %+v", c)
	}
	w = goodWorld()
	w.absent["namespace/comebye-workers"] = true
	if c := run(w); c.State != "missing" || !strings.Contains(c.Note, "namespace comebye-workers") {
		t.Errorf("no namespace: %+v", c)
	}
	w = goodWorld()
	w.absent["serviceaccount/comebye-worker-tool"] = true
	w.absent["datavolume/debian-13-golden"] = true
	if c := run(w); c.State != "missing" || !strings.Contains(c.Note, "service account comebye-worker-tool") || !strings.Contains(c.Note, "image DataVolume debian-13-golden") {
		t.Errorf("no sa/image: %+v", c)
	}
	w = goodWorld()
	w.denied["namespace"] = true
	if c := run(w); c.State != "error" {
		t.Errorf("forbidden: %+v", c)
	}
	w = goodWorld()
	w.nodes = nodesJSON([3]string{"rozalin", "65471768Ki", "0"}, [3]string{"etna", "98518240Ki", "1k"})
	if c := run(w); c.State != "error" || !strings.Contains(c.Note, "no node with KVM") {
		t.Errorf("no kvm node: %+v", c)
	}
	for _, b := range []string{"virtctl", "kubectl"} {
		withPath(t, b)
		if c := run(goodWorld()); c.State != "missing" || !strings.Contains(c.Note, b) {
			t.Errorf("%s missing: %+v", b, c)
		}
	}
}

func TestPreflightOutputKubeVirtOnly(t *testing.T) {
	withPath(t, "")
	e, r, out, _ := kubeEnv(t, goodWorld())
	e.Cfg.Provider = "gcp"
	e.Cfg.TSOAuthFile = filepath.Join(t.TempDir(), "none")
	r.handle = func(c call, f *fakeRunner) (string, string, error) { return "", "", errors.New("exit 1") }
	if err := e.preflight(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out(), `"kubevirt"`) || r.saw("kubectl") {
		t.Errorf("a gcp node ran the kubevirt check: %s", out())
	}
}

func TestPreflightJSONKubeVirt(t *testing.T) {
	withPath(t, "")
	e, _, out, _ := kubeEnv(t, goodWorld())
	e.Cfg.TSOAuthFile = filepath.Join(t.TempDir(), "none")
	if err := e.preflight(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	var d struct {
		KubeVirt struct{ State, Note string } `json:"kubevirt"`
	}
	if err := json.Unmarshal([]byte(out()), &d); err != nil || d.KubeVirt.State != "ok" {
		t.Errorf("%v %s", err, out())
	}
}

// ---- renderers and the dry run ----

func goldenJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, _ := json.MarshalIndent(v, "", "  ")
	goldenCheck(t, "kubevirt/"+name, append(b, '\n'))
}

func TestKubeVirtRenderGolden(t *testing.T) {
	c := kubeCfg(t)
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItestkeytestkeytestkeytestkeytestkeytestkey comebye-worker"
	priv := "-----BEGIN OPENSSH PRIVATE KEY-----\nFAKEFAKEFAKE\n-----END OPENSSH PRIVATE KEY-----\n"
	hostPub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIhostkeyhostkeyhostkeyhostkeyhostkeyhost"
	goldenJSON(t, "vm.json", renderVM(c, "w1", "rozalin.x.cc"))
	goldenJSON(t, "datavolume.json", renderDataDV(c, "w1"))
	goldenJSON(t, "secret.json", renderSecret(c, "w1", pub, priv, hostPub))
	c.VMImageDV, c.VMImageURL = "", "https://cloud.debian.org/images/cloud/trixie/20261101-1/debian-13-genericcloud-amd64-20261101-1.qcow2"
	goldenJSON(t, "vm-url.json", renderVM(c, "w1", "zetta.x.cc"))
}

func TestKubeVirtVMInvariants(t *testing.T) {
	c := kubeCfg(t)
	vm := renderVM(c, "w1", "rozalin")
	b, _ := json.Marshal(vm)
	s := string(b)
	for _, want := range []string{`"managed-by":"comebye-worker"`, `"runStrategy":"RerunOnFailure"`, `"kubernetes.io/hostname":"rozalin"`,
		`"requests":{"cpu":"2","memory":"32Gi"}`, `"limits":{"cpu":"4","memory":"32Gi"}`, `"serial":"comebye-data"`} {
		if !strings.Contains(s, want) {
			t.Errorf("VM lacks %s", want)
		}
	}
	// no tailnet key, token or secret in the user-data
	ud := renderCloudInit(c, "w1", "ssh-ed25519 AAAA k", "PRIV\n", "ssh-ed25519 BBBB")
	for _, bad := range []string{"tskey", "authkey", "auth-key", "token", "COMEBYE_HUB"} {
		if strings.Contains(strings.ToLower(ud), strings.ToLower(bad)) {
			t.Errorf("user-data mentions %q", bad)
		}
	}
}

func TestKubeVirtDryRun(t *testing.T) {
	e, r, out, _ := kubeEnv(t, goodWorld())
	if err := e.create(context.Background(), "w1", createOpts{Version: "v0.3.1"}); err != nil {
		t.Fatalf("%v\n%s", err, out())
	}
	o := out()
	for _, want := range []string{"4 vCPU, 32Gi", "-> rozalin", "avoided", "DataVolume w1-data", "Secret w1-cloudinit", "VirtualMachine w1", "RerunOnFailure", "virtctl port-forward --stdio=true vmi/w1.comebye-workers 22", `"kubernetes.io/hostname": "rozalin.x.cc"`} {
		if !strings.Contains(o, want) {
			t.Errorf("plan lacks %q:\n%s", want, o)
		}
	}
	for _, c := range r.calls {
		if c.name != "kubectl" || !(c.args[0] == "get" || (len(c.args) > 2 && c.args[2] == "get")) {
			t.Errorf("the dry run ran %s", c.line())
		}
	}
	for _, bad := range []string{"BEGIN OPENSSH PRIVATE KEY", secretKey} {
		if strings.Contains(o, bad) {
			t.Errorf("plan prints %q", bad)
		}
	}
}

func TestKubeVirtDryRunRefusedByGate(t *testing.T) {
	w := goodWorld()
	w.pods = podsJSON("rozalin.x.cc", "40Gi")
	w.nodes = nodesJSON([3]string{"rozalin.x.cc", "65471768Ki", "1k"})
	w.metrics = ""
	e, _, out, _ := kubeEnv(t, w)
	err := e.create(context.Background(), "w1", createOpts{})
	if err == nil || !strings.Contains(err.Error(), "capacity gate") || !strings.Contains(out(), "REFUSED") {
		t.Errorf("err %v\n%s", err, out())
	}
	if !strings.Contains(out(), "metrics API did not answer") {
		t.Errorf("no note about missing metrics:\n%s", out())
	}
	w.noServer = true
	e, _, _, _ = kubeEnv(t, w)
	if err := e.create(context.Background(), "w1", createOpts{}); err == nil {
		t.Error("an unreachable cluster did not fail the dry run")
	}
}

// liveWorld is a tiny stateful cluster behind a fake kubectl: objects by
// "kind/name" with their JSON, so apply, get, patch and delete behave.
type liveWorld struct {
	*kubeWorld
	objs    map[string]map[string]any
	applied []string // "kind/name" in order
	stdins  []string
	vmState string // printableStatus once a VM exists
	vmiGone bool
	patches []string
	denyDel bool
	fail    bool // every kubectl call fails (not NotFound)
}

func newLiveWorld() *liveWorld {
	return &liveWorld{kubeWorld: goodWorld(), objs: map[string]map[string]any{}, vmState: "Running"}
}

func kindOf(k string) string {
	switch k = strings.ToLower(k); k {
	case "virtualmachine", "vm":
		return "vm"
	case "virtualmachineinstance":
		return "vmi"
	}
	return k
}

func (w *liveWorld) fake() *fakeRunner {
	base := w.kubeWorld.fake()
	return &fakeRunner{handle: func(c call, f *fakeRunner) (string, string, error) {
		if c.name == "ssh-keygen" {
			path := c.args[len(c.args)-1]
			os.WriteFile(path, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nFAKEHOSTKEY\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600)
			os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpinnedpinnedpinnedpinnedpinnedpinnedpinned\n"), 0o644)
			return "", "", nil
		}
		if c.name != "kubectl" {
			return "", "", fmt.Errorf("unexpected %s", c.line())
		}
		if w.fail {
			return "", "Unable to connect to the server", errors.New("exit 1")
		}
		args := c.args
		if len(args) >= 2 && args[0] == "--context" {
			args = args[2:]
		}
		if len(args) >= 2 && args[0] == "-n" {
			args = args[2:]
		}
		if len(args) >= 3 && args[1] == "-n" {
			args = append([]string{args[0]}, args[3:]...)
		}
		nf := func(kind, name string) (string, string, error) {
			return "", fmt.Sprintf("Error from server (NotFound): %s %q not found", kind, name), errors.New("exit 1")
		}
		switch args[0] {
		case "apply":
			var m map[string]any
			if err := json.Unmarshal([]byte(c.stdin), &m); err != nil {
				return "", "bad manifest", err
			}
			k := kindOf(m["kind"].(string)) + "/" + m["metadata"].(map[string]any)["name"].(string)
			w.objs[k] = m
			w.applied = append(w.applied, k)
			w.stdins = append(w.stdins, c.stdin)
			return k + " configured", "", nil
		case "patch":
			w.patches = append(w.patches, args[len(args)-1])
			return "patched", "", nil
		case "delete":
			if w.denyDel {
				return "", "Forbidden", errors.New("exit 1")
			}
			k := kindOf(args[1]) + "/" + args[2]
			delete(w.objs, k)
			if args[1] == "vm" {
				delete(w.objs, "vmi/"+args[2])
			}
			w.patches = append(w.patches, "delete "+k)
			return "deleted", "", nil
		case "get":
			if len(args) >= 3 && (args[1] == "vm" || args[1] == "vmi" || args[1] == "secret" || args[1] == "datavolume" || args[1] == "pvc") && args[2] != "-l" {
				k := args[1] + "/" + args[2]
				o, ok := w.objs[k]
				if args[1] == "vm" && ok {
					o["status"] = map[string]any{"printableStatus": w.vmState}
				}
				if args[1] == "vmi" {
					if !ok && w.vmState == "Running" && w.objs["vm/"+args[2]] != nil {
						o, ok = map[string]any{"status": map[string]any{"nodeName": "rozalin.x.cc", "interfaces": []any{map[string]any{"ipAddress": "10.42.1.9"}}}}, true
					}
					if w.vmiGone {
						ok = false
					}
				}
				if !ok {
					return nf(args[1], args[2])
				}
				line := strings.Join(args, " ")
				switch {
				case strings.Contains(line, "managed-by}"):
					return labelOf(o), "", nil
				case strings.Contains(line, "host-key}"):
					return o["metadata"].(map[string]any)["annotations"].(map[string]any)[hostKeyAnno].(string), "", nil
				case strings.Contains(line, "-o name"):
					return k, "", nil
				}
				b, _ := json.Marshal(o)
				return string(b), "", nil
			}
			if len(args) >= 2 && args[1] == "vm" { // list with -l
				var items []string
				for k, o := range w.objs {
					if strings.HasPrefix(k, "vm/") {
						o["status"] = map[string]any{"printableStatus": w.vmState}
						b, _ := json.Marshal(o)
						items = append(items, string(b))
					}
				}
				return `{"items":[` + strings.Join(items, ",") + `]}`, "", nil
			}
		}
		return base.handle(c, f)
	}}
}

func labelOf(o map[string]any) string {
	l, _ := o["metadata"].(map[string]any)["labels"].(map[string]any)
	s, _ := l[labelKey].(string)
	return s
}

func liveEnv(t *testing.T, w *liveWorld) (*env, *fakeRunner) {
	t.Helper()
	c := kubeCfg(t)
	r := w.fake()
	e, _, _ := newEnv(t, c, &fakeWorld{hubVersion: "v0.3.1"}, r)
	e.prov().(*kubevirtProv).wait = func(int) {}
	return e, r
}

const fakePub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItestkeytestkeytestkeytestkeytestkeytestkey comebye-worker"

func TestKubeVirtCreatePinsHostKey(t *testing.T) {
	w := newLiveWorld()
	e, r := liveEnv(t, w)
	p := e.prov().(*kubevirtProv)
	ctx := context.Background()
	if err := p.EnsureDataDisk(ctx, "kv1", func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if err := p.Create(ctx, "kv1", fakePub); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.applied, ",") != "datavolume/kv1-data,secret/kv1-cloudinit,vm/kv1" {
		t.Errorf("applied %v", w.applied)
	}
	// the pinned key is the one in the Secret's user-data, and HostKey returns exactly it
	k, err := p.HostKey(ctx, "kv1", nil)
	if err != nil || !strings.HasPrefix(k, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpinned") || strings.Contains(k, " comebye") {
		t.Fatalf("HostKey %q %v", k, err)
	}
	ud := w.stdins[1]
	if !strings.Contains(ud, "FAKEHOSTKEY") || !strings.Contains(ud, strings.Fields(k)[1]) {
		t.Error("the Secret does not carry the private key and the pinned public key")
	}
	// the node is the gate's choice, and the secret was never in argv
	if !strings.Contains(w.stdins[2], `"kubernetes.io/hostname":"rozalin.x.cc"`) {
		t.Errorf("VM not pinned to the gate's node: %s", w.stdins[2])
	}
	for _, c := range r.calls {
		if strings.Contains(c.line(), "FAKEHOSTKEY") {
			t.Errorf("a key reached argv: %s", c.line())
		}
	}
	if !strings.Contains(w.stdins[2], `"dnsPolicy":"None"`) || !strings.Contains(w.stdins[2], "1.1.1.1") {
		t.Error("the guest has no public resolvers")
	}
}

func TestKubeVirtCreateRefusals(t *testing.T) {
	w := newLiveWorld()
	w.objs["secret/kv1-cloudinit"] = map[string]any{"metadata": map[string]any{"name": "kv1-cloudinit"}}
	e, _ := liveEnv(t, w)
	p := e.prov().(*kubevirtProv)
	if err := p.Create(context.Background(), "kv1", fakePub); err == nil || !strings.Contains(err.Error(), "without") || len(w.applied) != 0 {
		t.Errorf("an unlabelled Secret was overwritten: %v %v", err, w.applied)
	}
	// the gate refuses
	w = newLiveWorld()
	w.pods = podsJSON("rozalin.x.cc", "40Gi")
	w.nodes = nodesJSON([3]string{"rozalin.x.cc", "65471768Ki", "1k"})
	e, _ = liveEnv(t, w)
	if err := e.prov().Create(context.Background(), "kv1", fakePub); err == nil || !strings.Contains(err.Error(), "capacity gate") || len(w.applied) != 0 {
		t.Errorf("create went past the gate: %v %v", err, w.applied)
	}
	// an unlabelled data disk is not adopted
	w = newLiveWorld()
	w.objs["datavolume/kv1-data"] = map[string]any{"metadata": map[string]any{"name": "kv1-data"}}
	e, _ = liveEnv(t, w)
	if err := e.prov().EnsureDataDisk(context.Background(), "kv1", func(string, ...any) {}); err == nil {
		t.Error("adopted a foreign DataVolume")
	}
}

func TestKubeVirtDescribe(t *testing.T) {
	w := newLiveWorld()
	e, _ := liveEnv(t, w)
	p := e.prov()
	ctx := context.Background()
	if i, err := p.Describe(ctx, "kv1"); i != nil || err != nil {
		t.Fatalf("absent VM: %v %v", i, err)
	}
	p.EnsureDataDisk(ctx, "kv1", func(string, ...any) {})
	p.Create(ctx, "kv1", fakePub)
	i, err := p.Describe(ctx, "kv1")
	if err != nil || i == nil || i.State != stateRunning || i.IP != "10.42.1.9" || !i.Managed || i.Type != "0 vCPU, " && !strings.Contains(i.Type, "32Gi") {
		t.Fatalf("%+v %v", i, err)
	}
	w.vmState = "Stopped"
	if i, _ := p.Describe(ctx, "kv1"); i.State != stateStopped || i.IP != "" {
		t.Errorf("%+v", i)
	}
	w.vmState = "Provisioning"
	if i, _ := p.Describe(ctx, "kv1"); i.State != "PROVISIONING" {
		t.Errorf("%+v", i)
	}
	// a kubectl failure other than NotFound is an error, not "no such VM"
	w.fail = true
	if i, err := p.Describe(ctx, "kv1"); err == nil {
		t.Errorf("an unreachable cluster read as %v", i)
	}
}

func TestKubeVirtStopStart(t *testing.T) {
	w := newLiveWorld()
	e, _ := liveEnv(t, w)
	p := e.prov().(*kubevirtProv)
	ctx := context.Background()
	p.Create(ctx, "kv1", fakePub)
	w.vmState = "Stopped"
	w.vmiGone = true
	if err := p.Stop(ctx, "kv1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx, "kv1"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.patches, "|") != `{"spec":{"runStrategy":"Halted"}}|{"spec":{"runStrategy":"Halted"}}|{"spec":{"runStrategy":"RerunOnFailure"}}` {
		t.Errorf("patches %v", w.patches)
	}
	// a VM that never reaches Stopped is an error, not a hang
	w.vmState, w.vmiGone = "Running", false
	if err := p.Stop(ctx, "kv1"); err == nil {
		t.Error("Stop returned while the VM runs")
	}
	// an unlabelled VM is left alone
	delete(w.objs["vm/kv1"]["metadata"].(map[string]any), "labels")
	w.patches = nil
	if err := p.Stop(ctx, "kv1"); err == nil || len(w.patches) != 0 {
		t.Errorf("stopped a foreign VM: %v %v", err, w.patches)
	}
}

func TestKubeVirtDelete(t *testing.T) {
	w := newLiveWorld()
	e, _ := liveEnv(t, w)
	p := e.prov()
	ctx := context.Background()
	p.EnsureDataDisk(ctx, "kv1", func(string, ...any) {})
	p.Create(ctx, "kv1", fakePub)
	if err := p.Delete(ctx, "kv1"); err != nil {
		t.Fatal(err)
	}
	if w.objs["vm/kv1"] != nil || w.objs["secret/kv1-cloudinit"] != nil {
		t.Errorf("left behind: %v", w.objs)
	}
	if w.objs["datavolume/kv1-data"] == nil {
		t.Error("Delete removed the data disk")
	}
	// absent VM: nothing to guard, nothing deleted
	if err := p.Delete(ctx, "kv1"); err == nil {
		t.Error("Delete of an absent VM succeeded silently") // destroy checks Describe first
	}
	if err := p.DeleteDataDisk(ctx, "kv1"); err != nil || w.objs["datavolume/kv1-data"] != nil {
		t.Errorf("%v %v", err, w.objs)
	}
	// a foreign VM is not deleted
	w.objs["vm/other"] = map[string]any{"metadata": map[string]any{"name": "other"}}
	if err := p.Delete(ctx, "other"); err == nil || w.objs["vm/other"] == nil {
		t.Errorf("deleted a foreign VM: %v", err)
	}
	w.objs["datavolume/foreign-data"] = map[string]any{"metadata": map[string]any{"name": "foreign-data"}}
	if err := p.DeleteDataDisk(ctx, "foreign"); err == nil || w.objs["datavolume/foreign-data"] == nil {
		t.Errorf("deleted a foreign disk: %v", err)
	}
}

func TestKubeVirtSSHConfig(t *testing.T) {
	c := kubeCfg(t)
	c.KubeContext = "homelab"
	s := sshConfig(c, "kv1", "kv1", "/k/known_hosts")
	for _, want := range []string{"HostName kv1", "StrictHostKeyChecking yes", "ProxyCommand virtctl --context homelab port-forward --stdio=true vmi/kv1.comebye-workers 22", "UserKnownHostsFile /k/known_hosts"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh_config lacks %q:\n%s", want, s)
		}
	}
	c.Provider = "gcp"
	if strings.Contains(sshConfig(c, "kv1", "1.2.3.4", "/k"), "ProxyCommand") {
		t.Error("a non-kubevirt config got a ProxyCommand")
	}
}

func TestKubeVirtConnectPins(t *testing.T) {
	w := newLiveWorld()
	e, r := liveEnv(t, w)
	e.prov().Create(context.Background(), "kv1", fakePub)
	// ssh answers
	base := r.handle
	r.handle = func(c call, f *fakeRunner) (string, string, error) {
		if c.name == "ssh" {
			return "", "", nil
		}
		return base(c, f)
	}
	if err := e.connect(context.Background(), "kv1"); err != nil {
		t.Fatal(err)
	}
	kh, _ := os.ReadFile(filepath.Join(e.Cfg.WorkerDir("kv1"), "known_hosts"))
	if !strings.HasPrefix(string(kh), "kv1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpinned") {
		t.Errorf("known_hosts %q", kh)
	}
	cfg, _ := os.ReadFile(filepath.Join(e.Cfg.WorkerDir("kv1"), "ssh_config"))
	if !strings.Contains(string(cfg), "ProxyCommand virtctl port-forward") {
		t.Errorf("ssh_config %s", cfg)
	}
}

func TestKubeVirtInstancesAndList(t *testing.T) {
	w := newLiveWorld()
	e, _ := liveEnv(t, w)
	e.prov().Create(context.Background(), "kv1", fakePub)
	is, err := e.prov().Instances(context.Background())
	if err != nil || len(is) != 1 || is[0].Name != "kv1" || !is[0].Managed {
		t.Errorf("%+v %v", is, err)
	}
}
