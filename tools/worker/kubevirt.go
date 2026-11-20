package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The kubevirt provider: a worker is a KubeVirt VM on a cluster that kubectl
// and virtctl already reach. The fixed pieces (namespace, quota, network
// policy, the golden image, a scoped service account) are applied once from
// the homelab repo; this tool makes the per-worker objects.
//
// This file holds the config, the pure renderers of the objects a create
// applies, the dry-run plan, the preflight check and the capacity gate;
// kubevirt_live.go has the methods that change the cluster.
//
// The tool never reads, copies or prints the kubeconfig: it runs kubectl with
// at most a --context name, and nothing it prints comes from a Secret.

const (
	kubectlBin = "kubectl"
	virtctlBin = "virtctl"

	// podOverheadBytes is what KubeVirt adds to the guest memory in the
	// virt-launcher pod's request (qemu, the compute container, page tables).
	// The gate counts it so a VM that fits on paper still fits as a pod.
	podOverheadBytes = int64(512) << 20

	gib = int64(1) << 30
	mib = int64(1) << 20

	// dataDiskSerial is the serial of the data disk: the guest sees it as
	// /dev/disk/by-id/virtio-comebye-data whichever order the disks come up in.
	dataDiskSerial = "comebye-data"

	hostKeyAnno = "comebye-worker/host-key"
)

// guestDNS are the resolvers the guest uses: the network policy denies cluster DNS.
var guestDNS = []any{"1.1.1.1", "9.9.9.9"}

var (
	kubeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	kubeNodeRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	kubeCtxRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,200}$`)
	vmMemRe    = regexp.MustCompile(`^([1-9][0-9]{0,4})(Gi|Mi)$`)
	imageURLRe = regexp.MustCompile(`^https://[A-Za-z0-9._~/:-]+\.(qcow2|img)$`)
)

// vmMemoryBytes is vm_memory in bytes (validated by Validate).
func (c *Config) vmMemoryBytes() int64 {
	m := vmMemRe.FindStringSubmatch(c.VMMemory)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	if m[2] == "Gi" {
		return n * gib
	}
	return n * mib
}

func (c *Config) validateKubeVirt(chk func(bool, string, ...any) error) error {
	for _, e := range []error{
		chk(c.KubeContext == "" || kubeCtxRe.MatchString(c.KubeContext), "bad kube_context %q", c.KubeContext),
		chk(kubeNameRe.MatchString(c.KubeNamespace), "bad kube_namespace %q", c.KubeNamespace),
		chk(kubeNameRe.MatchString(c.KubeServiceAcct), "bad kube_service_account %q", c.KubeServiceAcct),
		chk(kubeNameRe.MatchString(c.KubeStorageClass), "bad kube_storage_class %q", c.KubeStorageClass),
		chk((c.VMImageDV == "") != (c.VMImageURL == ""), "set exactly one of vm_image_dv (a DataVolume in the namespace) and vm_image_url (a qcow2 to import)"),
		chk(c.VMImageDV == "" || kubeNameRe.MatchString(c.VMImageDV), "bad vm_image_dv %q", c.VMImageDV),
		chk(c.VMImageURL == "" || imageURLRe.MatchString(c.VMImageURL), "bad vm_image_url %q (an https URL of a .qcow2 or .img)", c.VMImageURL),
		chk(c.VMCPU >= 1 && c.VMCPU <= 32, "vm_cpu must be 1..32"),
		chk(c.VMCPURequest >= 1 && c.VMCPURequest <= c.VMCPU, "vm_cpu_request must be 1..vm_cpu (it is the scheduler's share; vm_cpu is the limit)"),
		chk(vmMemRe.MatchString(c.VMMemory) && c.vmMemoryBytes() >= 2*gib && c.vmMemoryBytes() <= 256*gib, "vm_memory %q must be like 32Gi (2Gi..256Gi)", c.VMMemory),
		chk(c.KubeFloorGiB >= 1 && c.KubeFloorGiB <= 512, "kube_floor_gib must be 1..512"),
		chk(c.LLMGatewayURL == "" || c.hasAgent("pi"), "llm_gateway_url needs pi in agents"),
	} {
		if e != nil {
			return e
		}
	}
	for _, list := range [][]string{c.KubeNodePrefer, c.KubeNodeAvoid} {
		for _, n := range list {
			if !kubeNodeRe.MatchString(n) {
				return fmt.Errorf("bad node name %q in kube_node_prefer or kube_node_avoid", n)
			}
		}
	}
	for n, g := range c.KubeNodeFloors {
		if !kubeNodeRe.MatchString(n) || g < 1 || g > 512 {
			return fmt.Errorf("kube_node_floor %s=%d: want a node name and 1..512 GiB", n, g)
		}
	}
	for _, p := range c.KubeNodePrefer {
		if nodeIn(c.KubeNodeAvoid, p) {
			return fmt.Errorf("node %q is in both kube_node_prefer and kube_node_avoid", p)
		}
	}
	return nil
}

// ---- the kubectl runner ----

type kubeCLI struct {
	r   Runner
	cfg *Config
}

// kubectl runs kubectl with the configured context. Secrets never travel
// here; stdin carries a manifest only when a later slice applies one.
func (k *kubeCLI) kubectl(ctx context.Context, args ...string) (string, string, error) {
	return k.kubectlIn(ctx, nil, args...)
}

// kubectlIn is kubectl with stdin: a manifest for `apply -f -`. A Secret
// travels this way, never in argv.
func (k *kubeCLI) kubectlIn(ctx context.Context, stdin []byte, args ...string) (string, string, error) {
	var a []string
	if k.cfg.KubeContext != "" {
		a = append(a, "--context", k.cfg.KubeContext)
	}
	return k.r.Run(ctx, kubectlBin, append(a, args...), stdin)
}

func (k *kubeCLI) ns(args ...string) []string {
	return append([]string{"-n", k.cfg.KubeNamespace}, args...)
}

// exists reports whether `kubectl get ARGS -o name` finds an object: a
// NotFound is false, nil; anything else is an error.
func (k *kubeCLI) exists(ctx context.Context, args ...string) (bool, error) {
	_, errOut, err := k.kubectl(ctx, append(append([]string{"get"}, args...), "-o", "name")...)
	if err == nil {
		return true, nil
	}
	if strings.Contains(errOut+err.Error(), "NotFound") || strings.Contains(errOut, "not found") {
		return false, nil
	}
	return false, err
}

// ---- quantities and nodes ----

// parseQty reads a Kubernetes quantity as a number of units (bytes for
// memory). Only the forms a node or pod reports are understood.
func parseQty(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := map[string]float64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
		"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "m": 1e-3, "": 1}
	i := len(s)
	for i > 0 && !(s[i-1] >= '0' && s[i-1] <= '9') && s[i-1] != '.' {
		i--
	}
	m, ok := mult[s[i:]]
	f, err := strconv.ParseFloat(s[:i], 64)
	if !ok || err != nil || i == 0 {
		return 0, fmt.Errorf("unreadable quantity %q", s)
	}
	return int64(f * m), nil
}

// kubeNode is what the gate needs to know of a node.
type kubeNode struct {
	Name      string
	Alloc     int64 // allocatable memory
	Requested int64 // sum of the memory requests of its live pods
	Usage     int64 // measured use, 0 when the metrics API does not say
	KVM       bool  // exposes devices.kubevirt.io/kvm
	Ready     bool
	Blocked   string // why no pod can go here (cordoned, tainted), or ""
}

func shortNode(n string) string {
	s, _, _ := strings.Cut(n, ".")
	return s
}

func nodeIn(list []string, n string) bool {
	for _, l := range list {
		if l == n || l == shortNode(n) {
			return true
		}
	}
	return false
}

func nodeIndex(list []string, n string) int {
	for i, l := range list {
		if l == n || l == shortNode(n) {
			return i
		}
	}
	return -1
}

// kubeNodes reads the nodes, their pods' memory requests and, when the
// metrics API answers, their use. All reads.
func (k *kubeCLI) kubeNodes(ctx context.Context) (nodes []kubeNode, usageKnown bool, err error) {
	out, _, err := k.kubectl(ctx, "get", "nodes", "-o", "json")
	if err != nil {
		return nil, false, fmt.Errorf("could not list the nodes: %w", err)
	}
	var nl struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				Unschedulable bool `json:"unschedulable"`
				Taints        []struct{ Key, Effect string }
			} `json:"spec"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
				Conditions  []struct{ Type, Status string }
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &nl); err != nil {
		return nil, false, fmt.Errorf("the node list was not JSON: %w", err)
	}
	byName := map[string]*kubeNode{}
	for _, it := range nl.Items {
		n := kubeNode{Name: it.Metadata.Name}
		n.Alloc, err = parseQty(it.Status.Allocatable["memory"])
		if err != nil {
			return nil, false, fmt.Errorf("node %s: %w", n.Name, err)
		}
		if kvm, _ := parseQty(it.Status.Allocatable["devices.kubevirt.io/kvm"]); kvm > 0 {
			n.KVM = true
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				n.Ready = true
			}
		}
		switch {
		case it.Spec.Unschedulable:
			n.Blocked = "cordoned"
		default:
			for _, t := range it.Spec.Taints {
				if t.Effect == "NoSchedule" || t.Effect == "NoExecute" {
					n.Blocked = "tainted " + t.Key
				}
			}
		}
		nodes = append(nodes, n)
	}
	for i := range nodes {
		byName[nodes[i].Name] = &nodes[i]
	}

	out, _, err = k.kubectl(ctx, "get", "pods", "-A", "-o", "json", "--field-selector=status.phase!=Succeeded,status.phase!=Failed")
	if err != nil {
		return nil, false, fmt.Errorf("could not list the pods: %w", err)
	}
	var pl struct {
		Items []struct {
			Spec struct {
				NodeName   string `json:"nodeName"`
				Containers []struct {
					Resources struct{ Requests map[string]string } `json:"resources"`
				} `json:"containers"`
				InitContainers []struct {
					Resources struct{ Requests map[string]string } `json:"resources"`
				} `json:"initContainers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pl); err != nil {
		return nil, false, fmt.Errorf("the pod list was not JSON: %w", err)
	}
	for _, p := range pl.Items {
		n := byName[p.Spec.NodeName]
		if n == nil {
			continue
		}
		var sum, maxInit int64
		for _, c := range p.Spec.Containers {
			q, _ := parseQty(c.Resources.Requests["memory"])
			sum += q
		}
		for _, c := range p.Spec.InitContainers {
			if q, _ := parseQty(c.Resources.Requests["memory"]); q > maxInit {
				maxInit = q
			}
		}
		if maxInit > sum {
			sum = maxInit
		}
		n.Requested += sum
	}

	// Requests understate a node whose pods run past them, so the measured use
	// counts too when the metrics API is there.
	if out, _, merr := k.kubectl(ctx, "get", "--raw", "/apis/metrics.k8s.io/v1beta1/nodes"); merr == nil {
		var ml struct {
			Items []struct {
				Metadata struct{ Name string } `json:"metadata"`
				Usage    map[string]string     `json:"usage"`
			} `json:"items"`
		}
		if json.Unmarshal([]byte(out), &ml) == nil && len(ml.Items) > 0 {
			usageKnown = true
			for _, it := range ml.Items {
				if n := byName[it.Metadata.Name]; n != nil {
					n.Usage, _ = parseQty(it.Usage["memory"])
				}
			}
		}
	}
	return nodes, usageKnown, nil
}

// ---- the capacity gate ----

// gateRow is one node's line in the gate: why it was or was not chosen.
type gateRow struct {
	Node       string
	Free       int64 // allocatable minus the larger of requested and measured use
	After      int64 // free once the VM is placed
	Floor      int64
	OK         bool
	Reason     string // set when !OK
	Candidate  bool   // the config allows the VM here at all
	Preference int    // position in kube_node_prefer, -1 when unlisted
}

type gateResult struct {
	Rows       []gateRow
	Chosen     string // "" when refused
	Need       int64  // guest memory plus the pod overhead
	UsageKnown bool
}

func (c *Config) floorBytes(node string) int64 {
	if g, ok := c.KubeNodeFloors[node]; ok {
		return int64(g) * gib
	}
	if g, ok := c.KubeNodeFloors[shortNode(node)]; ok {
		return int64(g) * gib
	}
	return int64(c.KubeFloorGiB) * gib
}

// kubeGate decides where the VM may go: only a node the config allows, with
// KVM, schedulable, and left with at least its floor of free memory after
// the VM (guest memory plus the pod overhead). Pure: it reads nothing.
func kubeGate(c *Config, nodes []kubeNode, usageKnown bool) gateResult {
	g := gateResult{Need: c.vmMemoryBytes() + podOverheadBytes, UsageKnown: usageKnown}
	for _, n := range nodes {
		r := gateRow{Node: n.Name, Floor: c.floorBytes(n.Name), Preference: nodeIndex(c.KubeNodePrefer, n.Name)}
		used := n.Requested
		if n.Usage > used {
			used = n.Usage
		}
		r.Free = n.Alloc - used
		r.After = r.Free - g.Need
		switch {
		case nodeIn(c.KubeNodeAvoid, n.Name):
			r.Reason = "avoided (kube_node_avoid)"
		case len(c.KubeNodePrefer) > 0 && r.Preference < 0:
			r.Reason = "not in kube_node_prefer"
		case !n.KVM:
			r.Candidate, r.Reason = true, "no devices.kubevirt.io/kvm"
		case !n.Ready:
			r.Candidate, r.Reason = true, "not Ready"
		case n.Blocked != "":
			r.Candidate, r.Reason = true, n.Blocked
		case r.After < r.Floor:
			r.Candidate, r.Reason = true, fmt.Sprintf("would leave %s free, under the %s floor", fmtGiB(r.After), fmtGiB(r.Floor))
		default:
			r.Candidate, r.OK = true, true
		}
		g.Rows = append(g.Rows, r)
	}
	sort.SliceStable(g.Rows, func(i, j int) bool {
		a, b := g.Rows[i], g.Rows[j]
		if (a.Preference >= 0) != (b.Preference >= 0) {
			return a.Preference >= 0
		}
		if a.Preference != b.Preference {
			return a.Preference < b.Preference
		}
		return a.Free > b.Free
	})
	for _, r := range g.Rows {
		if r.OK {
			g.Chosen = r.Node
			break
		}
	}
	return g
}

func fmtGiB(b int64) string {
	if b < 0 {
		return "-" + fmtGiB(-b)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
}

// Err is the refusal, naming each node's reason, or nil when a node was chosen.
func (g gateResult) Err() error {
	if g.Chosen != "" {
		return nil
	}
	var parts []string
	for _, r := range g.Rows {
		if r.Candidate || r.Preference >= 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", shortNode(r.Node), r.Reason))
		}
	}
	if len(parts) == 0 {
		parts = []string{"no node is allowed by kube_node_prefer / kube_node_avoid"}
	}
	return fmt.Errorf("capacity gate: no node can take the VM (%s memory plus %s overhead): %s", fmtGiB(g.Need-podOverheadBytes), fmtGiB(podOverheadBytes), strings.Join(parts, "; "))
}

func (g gateResult) print(w logf) {
	for _, r := range g.Rows {
		mark, why := "  ", r.Reason
		if r.OK {
			mark, why = "ok", fmt.Sprintf("free %s, %s after, floor %s", fmtGiB(r.Free), fmtGiB(r.After), fmtGiB(r.Floor))
		}
		if r.Node == g.Chosen {
			mark = "->"
		}
		w("     %s %-10s %s\n", mark, shortNode(r.Node), why)
	}
	if !g.UsageKnown {
		w("     (the metrics API did not answer: free memory counts the pods' requests only)\n")
	}
}

// ---- the provider ----

type kubevirtProv struct {
	k       *kubeCLI
	cfg     *Config
	planErr error // set by Plan when the gate refused
	wait    func(secs int)
}

func newKubeVirt(r Runner, c *Config) *kubevirtProv {
	return &kubevirtProv{k: &kubeCLI{r: r, cfg: c}, cfg: c, wait: func(s int) { time.Sleep(time.Duration(s) * time.Second) }}
}

func (p *kubevirtProv) Kind() string { return "kubevirt" }

// Gate reads the cluster and decides the node. It changes nothing.
func (p *kubevirtProv) Gate(ctx context.Context) (gateResult, error) {
	nodes, usage, err := p.k.kubeNodes(ctx)
	if err != nil {
		return gateResult{}, err
	}
	return kubeGate(p.cfg, nodes, usage), nil
}

// PlanErr is the gate's refusal found by Plan, for a dry run to fail on.
func (p *kubevirtProv) PlanErr() error { return p.planErr }

// Plan prints what create would apply. It reads the cluster (get only) for
// the capacity gate; it creates nothing and prints nothing from a Secret.
func (p *kubevirtProv) Plan(name, pub string, w logf) {
	c := p.cfg
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctxName := c.KubeContext
	if ctxName == "" {
		ctxName = "<the current kubectl context>"
	}
	w("   kubectl context %s, namespace %s; every object below carries %s=%s and the tool touches nothing without it\n", ctxName, c.KubeNamespace, labelKey, labelVal)
	w("   capacity gate (read-only: kubectl get nodes, pods, and the metrics API):\n")
	node := "<node chosen by the gate>"
	g, err := p.Gate(ctx)
	switch {
	case err != nil:
		w("     could not read the cluster: %v\n", err)
		p.planErr = fmt.Errorf("capacity gate: %w", err)
	case g.Err() != nil:
		g.print(w)
		w("     REFUSED: %v\n", g.Err())
		p.planErr = g.Err()
	default:
		g.print(w)
		node = g.Chosen
	}
	w("   1. DataVolume %s: the data disk, %d GiB on %s, a standalone object (the VM does not own it; destroy keeps it unless --delete-data-disk)\n", dataDVName(name), c.DataDiskGB, c.KubeStorageClass)
	w("   2. Secret %s: cloud-config with the %s user and YOUR PUBLIC ssh key, this worker's ssh host key (made here, pinned in known_hosts before the VM exists), the data disk mount.\n      No tailnet key, hub URL secret or token goes in it; the Tailscale key travels over ssh stdin later.\n", cloudInitName(name), c.SSHUser)
	w("   3. VirtualMachine %s: %d vCPU (CPU request %d, limit %d), %s memory (request = limit), %d GiB boot disk cloned from %s,\n      pinned to node %s, runStrategy RerunOnFailure (an idle poweroff leaves it stopped), pod network only, the guest gets no service account token\n",
		name, c.VMCPU, c.VMCPURequest, c.VMCPU, c.VMMemory, c.BootDiskGB, p.imageSource(), shortNode(node))
	w("   4. wait for the VMI, then ssh through `%s port-forward -n %s --stdio=true vmi/%s 22` (StrictHostKeyChecking yes, the pinned key)\n", virtctlBin, c.KubeNamespace, name)
	w("   the objects, as kubectl would apply them (the host key is a placeholder):\n")
	for _, m := range []any{
		renderDataDV(c, name),
		renderSecret(c, name, pub, "<ed25519 private host key: generated at create, never printed>", "<ed25519 public host key>"),
		renderVM(c, name, node),
	} {
		b, _ := json.MarshalIndent(m, "", "  ")
		for _, l := range strings.Split(string(b), "\n") {
			w("      %s\n", l)
		}
	}
}

func (p *kubevirtProv) imageSource() string {
	if p.cfg.VMImageDV != "" {
		return "DataVolume " + p.cfg.VMImageDV
	}
	return p.cfg.VMImageURL
}

// ---- pure renderers ----

// networkData is the guest's network config: DHCP on whatever the NIC is
// called, with no MAC in it.
const networkData = "version: 2\nethernets:\n  nic:\n    match:\n      name: \"en*\"\n    dhcp4: true\n"

// guestMAC is a locally administered MAC derived from the worker's name, so
// it is the same at every start.
func guestMAC(name string) string {
	h := sha256.Sum256([]byte("comebye-worker/" + name))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}

func dataDVName(name string) string    { return name + "-data" }
func bootDVName(name string) string    { return name + "-boot" }
func cloudInitName(name string) string { return name + "-cloudinit" }

func kubeLabels(name string) map[string]any {
	return map[string]any{labelKey: labelVal, "comebye-worker/name": name}
}

func gibStr(n int) string { return strconv.Itoa(n) + "Gi" }

// renderDataDV is the data disk: blank, standalone, labelled.
func renderDataDV(c *Config, name string) map[string]any {
	return map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{"name": dataDVName(name), "namespace": c.KubeNamespace, "labels": kubeLabels(name),
			// keep the DataVolume object after the import, so it stays findable (and applies are idempotent)
			"annotations": map[string]any{"cdi.kubevirt.io/storage.deleteAfterCompletion": "false"}},
		"spec": map[string]any{
			"source":  map[string]any{"blank": map[string]any{}},
			"storage": storageSpec(c, c.DataDiskGB),
		},
	}
}

func storageSpec(c *Config, gb int) map[string]any {
	return map[string]any{
		"accessModes":      []any{"ReadWriteOnce"},
		"storageClassName": c.KubeStorageClass,
		"resources":        map[string]any{"requests": map[string]any{"storage": gibStr(gb)}},
	}
}

// renderCloudInit is the user-data: the worker user with the public key, the
// pinned host key, the data disk. Nothing secret but the host key's private
// half, which is why it lives in a Secret and is pinned on this side.
func renderCloudInit(c *Config, name, pub, hostPriv, hostPub string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#cloud-config\nhostname: %s\nmanage_etc_hosts: true\nusers:\n  - name: %s\n    shell: /bin/bash\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    lock_passwd: true\n    ssh_authorized_keys:\n      - %s\n", name, c.SSHUser, pub)
	b.WriteString("ssh_keys:\n  ed25519_private: |\n")
	for _, l := range strings.Split(strings.TrimRight(hostPriv, "\n"), "\n") {
		fmt.Fprintf(&b, "    %s\n", l)
	}
	fmt.Fprintf(&b, "  ed25519_public: %s\n", hostPub)
	fmt.Fprintf(&b, `ssh_deletekeys: true
ssh_genkeytypes: [ed25519]
disk_setup:
  /dev/disk/by-id/virtio-%[1]s:
    table_type: none
    layout: false
    overwrite: false
fs_setup:
  - label: comebye-data
    filesystem: ext4
    device: /dev/disk/by-id/virtio-%[1]s
    partition: none
    overwrite: false
mounts:
  - [/dev/disk/by-id/virtio-%[1]s, /data, ext4, "defaults,nofail", "0", "2"]
runcmd:
  - [mkdir, -p, /data]
  - [chown, "%[2]s:%[2]s", /data]
`, dataDiskSerial, c.SSHUser)
	return b.String()
}

func renderSecret(c *Config, name, pub, hostPriv, hostPub string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		// The public half of the pinned host key rides on the Secret so a later
		// connect (after a stop, or from another shell) pins the same key.
		"metadata":   map[string]any{"name": cloudInitName(name), "namespace": c.KubeNamespace, "labels": kubeLabels(name), "annotations": map[string]any{hostKeyAnno: hostPub}},
		"stringData": map[string]any{"userdata": renderCloudInit(c, name, pub, hostPriv, hostPub), "networkdata": networkData},
	}
}

// renderVM is the VirtualMachine, pinned to node. The boot disk is a
// dataVolumeTemplate (owned by the VM, gone with it); the data disk is the
// standalone DataVolume.
func renderVM(c *Config, name, node string) map[string]any {
	var src map[string]any
	if c.VMImageDV != "" {
		src = map[string]any{"pvc": map[string]any{"namespace": c.KubeNamespace, "name": c.VMImageDV}}
	} else {
		src = map[string]any{"http": map[string]any{"url": c.VMImageURL}}
	}
	mem := c.VMMemory
	disk := func(n string, extra map[string]any) map[string]any {
		d := map[string]any{"name": n, "disk": map[string]any{"bus": "virtio"}}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	return map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": name, "namespace": c.KubeNamespace, "labels": kubeLabels(name)},
		"spec": map[string]any{
			"runStrategy": "RerunOnFailure",
			"dataVolumeTemplates": []any{map[string]any{
				"metadata": map[string]any{"name": bootDVName(name), "labels": kubeLabels(name)},
				"spec":     map[string]any{"source": src, "storage": storageSpec(c, c.BootDiskGB)},
			}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": kubeLabels(name)},
				"spec": map[string]any{
					"nodeSelector": map[string]any{"kubernetes.io/hostname": node},
					// Cluster DNS is denied to the guest by the namespace's network policy.
					"dnsPolicy":                     "None",
					"dnsConfig":                     map[string]any{"nameservers": guestDNS},
					"terminationGracePeriodSeconds": 120,
					"domain": map[string]any{
						"cpu": map[string]any{"cores": c.VMCPU},
						"resources": map[string]any{
							"requests": map[string]any{"memory": mem, "cpu": strconv.Itoa(c.VMCPURequest)},
							"limits":   map[string]any{"memory": mem, "cpu": strconv.Itoa(c.VMCPU)},
						},
						"devices": map[string]any{
							"disks": []any{
								disk("boot", map[string]any{"bootOrder": 1}),
								disk("data", map[string]any{"serial": dataDiskSerial}),
								disk("cloudinit", nil),
							},
							// A fixed MAC: cloud-init's first-boot network config matches the NIC by
							// MAC, and KubeVirt picks a new random one at every start, which left the
							// guest with no address after a stop and start.
							"interfaces": []any{map[string]any{"name": "default", "masquerade": map[string]any{}, "macAddress": guestMAC(name)}},
						},
					},
					"networks": []any{map[string]any{"name": "default", "pod": map[string]any{}}},
					"volumes": []any{
						map[string]any{"name": "boot", "dataVolume": map[string]any{"name": bootDVName(name)}},
						map[string]any{"name": "data", "dataVolume": map[string]any{"name": dataDVName(name)}},
						map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": cloudInitName(name)}, "networkDataSecretRef": map[string]any{"name": cloudInitName(name)}}},
					},
				},
			},
		},
	}
}

// ---- preflight ----

// pfKubeVirt checks that a create could start: both CLIs, a cluster that
// answers, KubeVirt and CDI deployed, the namespace, the service account,
// the image and the storage class, and a node with KVM. Read-only; a note
// never carries more than object names.
func (e *env) pfKubeVirt(ctx context.Context) pfCheck {
	c := e.Cfg
	for _, b := range []string{kubectlBin, virtctlBin} {
		if _, err := lookPath(b); err != nil {
			return pfCheck{State: "missing", Note: b + " is not installed"}
		}
	}
	k := &kubeCLI{r: e.Run, cfg: c}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, _, err := k.kubectl(cctx, "get", "--raw", "/version"); err != nil {
		return pfCheck{State: "error", Note: "the cluster did not answer (context " + orDefault(c.KubeContext) + ")"}
	}
	var bad, errs []string
	deployed := func(label string, args ...string) {
		out, _, err := k.kubectl(cctx, append(args, "-o", "jsonpath={.items[*].status.phase}")...)
		switch {
		case err != nil:
			errs = append(errs, label+" could not be read")
		case !strings.Contains(out, "Deployed"):
			errs = append(errs, label+" is not Deployed")
		}
	}
	deployed("KubeVirt", "get", "kubevirt", "-A")
	deployed("CDI", "get", "cdi")
	check := func(what string, args ...string) {
		ok, err := k.exists(cctx, args...)
		switch {
		case err != nil:
			errs = append(errs, what+" could not be read")
		case !ok:
			bad = append(bad, what)
		}
	}
	check("namespace "+c.KubeNamespace, "namespace", c.KubeNamespace)
	if len(bad) == 0 {
		check("service account "+c.KubeServiceAcct, "-n", c.KubeNamespace, "serviceaccount", c.KubeServiceAcct)
		if c.VMImageDV != "" {
			check("image DataVolume "+c.VMImageDV, "-n", c.KubeNamespace, "datavolume", c.VMImageDV)
		}
	}
	check("storage class "+c.KubeStorageClass, "storageclass", c.KubeStorageClass)
	nodes, _, nerr := k.kubeNodes(cctx)
	cands := 0
	if nerr != nil {
		errs = append(errs, "the nodes could not be read")
	} else {
		for _, r := range kubeGate(c, nodes, false).Rows {
			if r.Candidate && r.Reason != "no devices.kubevirt.io/kvm" {
				cands++
			}
		}
		if cands == 0 {
			errs = append(errs, "no node with KVM is allowed by kube_node_prefer / kube_node_avoid")
		}
	}
	switch {
	case len(errs) > 0:
		return pfCheck{State: "error", Note: strings.Join(append(errs, missingNote(bad)...), "; ")}
	case len(bad) > 0:
		return pfCheck{State: "missing", Note: strings.Join(missingNote(bad), "; ")}
	}
	return pfCheck{State: "ok", Note: fmt.Sprintf("cluster answers; KubeVirt and CDI Deployed; namespace %s; %d node(s) may take a VM", c.KubeNamespace, cands)}
}

func missingNote(bad []string) []string {
	if len(bad) == 0 {
		return nil
	}
	return []string{"missing: " + strings.Join(bad, ", ") + " (apply the prerequisites manifest)"}
}
