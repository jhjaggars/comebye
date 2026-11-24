package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The capacity gate's disk and namespace checks: Longhorn room on the node
// for the disks a create still has to make, and the namespace's LimitRange
// and ResourceQuota. Each reads the cluster (get only); what it cannot read
// is a note in the gate's output, not a refusal.

const (
	longhornNS          = "longhorn-system"
	longhornProvisioner = "driver.longhorn.io"

	// cdiOverheadPct is what CDI adds to a filesystem disk's claim for the
	// filesystem itself (its default is 5.5%): a 40 GiB disk claims ~42.4 GiB.
	cdiOverheadPct = 6
)

// diskNeed is a disk the create still has to make.
type diskNeed struct {
	What     string // "boot disk", "data disk"
	Class    string
	Bytes    int64 // the claim's size, CDI's overhead included
	Longhorn bool  // the class is Longhorn's: its room is checked on the node
	Selector []string
}

// lhDisk is one schedulable Longhorn disk on a node.
type lhDisk struct {
	Name string
	Tags []string
	Room int64 // what a new replica may still take, by Longhorn's own two rules
}

func (d lhDisk) has(tags []string) bool {
	for _, t := range tags {
		found := false
		for _, h := range d.Tags {
			found = found || h == t
		}
		if !found {
			return false
		}
	}
	return true
}

// fitDisks places the Longhorn disks on the node's disks, largest first, each
// on the matching disk with the most room. It says why they do not fit, or "".
func fitDisks(n kubeNode, needs []diskNeed) string {
	var lh []diskNeed
	for _, d := range needs {
		if d.Longhorn {
			lh = append(lh, d)
		}
	}
	if len(lh) == 0 {
		return ""
	}
	sort.SliceStable(lh, func(i, j int) bool { return lh[i].Bytes > lh[j].Bytes })
	room := map[string]int64{}
	for _, d := range n.Longhorn {
		room[d.Name] = d.Room
	}
	for _, need := range lh {
		best, most := "", int64(-1)
		for _, d := range n.Longhorn {
			if d.has(need.Selector) && room[d.Name] > most {
				best, most = d.Name, room[d.Name]
			}
		}
		if best == "" || most < need.Bytes {
			tagged := "no schedulable Longhorn disk"
			if len(need.Selector) > 0 {
				tagged += " tagged " + strings.Join(need.Selector, ",")
			}
			if best == "" {
				return fmt.Sprintf("%s for the %s (%s on %s)", tagged, need.What, fmtGiB(need.Bytes), need.Class)
			}
			return fmt.Sprintf("Longhorn: the %s needs %s on %s, but the roomiest disk tagged %s has %s schedulable",
				need.What, fmtGiB(need.Bytes), need.Class, orDash(strings.Join(need.Selector, ",")), fmtGiB(most))
		}
		room[best] -= need.Bytes
	}
	return ""
}

// diskNeeds lists the VM's disks that do not exist yet, with their classes.
func (p *kubevirtProv) diskNeeds(ctx context.Context, name string, notes *[]string) ([]diskNeed, error) {
	c := p.cfg
	var res []diskNeed
	for _, d := range []struct {
		what, obj, class string
		gb               int
	}{
		{"boot disk", bootDVName(name), c.KubeStorageClass, c.BootDiskGB},
		{"data disk", dataDVName(name), c.dataStorageClass(), c.DataDiskGB},
	} {
		found := false
		for _, kind := range []string{"datavolume", "pvc"} {
			ok, err := p.k.exists(ctx, p.k.ns(kind, d.obj)...)
			if err != nil {
				return nil, fmt.Errorf("could not read %s %s: %w", kind, d.obj, err)
			}
			found = found || ok
		}
		if found {
			continue // made already: its space is taken, not wanted
		}
		need := diskNeed{What: d.what, Class: d.class, Bytes: int64(d.gb) * gib * (100 + cdiOverheadPct) / 100}
		out, errOut, err := p.k.kubectl(ctx, "get", "storageclass", d.class, "-o", "json")
		var sc struct {
			Provisioner string            `json:"provisioner"`
			Parameters  map[string]string `json:"parameters"`
		}
		switch {
		case err != nil:
			*notes = append(*notes, fmt.Sprintf("storage class %s could not be read (%s): the %s's space is not checked", d.class, firstLine(errOut), d.what))
		case json.Unmarshal([]byte(out), &sc) != nil:
			*notes = append(*notes, fmt.Sprintf("storage class %s was not JSON: the %s's space is not checked", d.class, d.what))
		case sc.Provisioner == longhornProvisioner:
			need.Longhorn = true
			for _, t := range strings.Split(sc.Parameters["diskSelector"], ",") {
				if t = strings.TrimSpace(t); t != "" {
					need.Selector = append(need.Selector, t)
				}
			}
		}
		res = append(res, need)
	}
	return res, nil
}

// readLonghorn fills each node's schedulable Longhorn disks when a disk to
// make is on Longhorn. If Longhorn cannot be read the disks are not checked,
// and the gate says so.
func (p *kubevirtProv) readLonghorn(ctx context.Context, in *gateInput) {
	want := false
	for _, d := range in.Disks {
		want = want || d.Longhorn
	}
	if !want {
		return
	}
	disks, err := p.k.longhornDisks(ctx)
	if err != nil {
		in.Notes = append(in.Notes, fmt.Sprintf("Longhorn could not be read (%v): disk space is not checked", err))
		for i := range in.Disks {
			in.Disks[i].Longhorn = false
		}
		return
	}
	for i := range in.Nodes {
		in.Nodes[i].Longhorn = disks[in.Nodes[i].Name]
	}
}

// longhornDisks reads Longhorn's nodes and its two space settings, and
// returns each node's schedulable disks with their room. A replica fits a
// disk when the disk's scheduled total stays within its maximum less the
// reserve, times storage-over-provisioning-percentage, and its available
// space stays above storage-minimal-available-percentage of the maximum.
func (k *kubeCLI) longhornDisks(ctx context.Context) (map[string][]lhDisk, error) {
	out, errOut, err := k.kubectl(ctx, "-n", longhornNS, "get", "nodes.longhorn.io", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("%s", firstLine(errOut+" "+err.Error()))
	}
	var nl struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				AllowScheduling bool `json:"allowScheduling"`
				Disks           map[string]struct {
					AllowScheduling bool     `json:"allowScheduling"`
					StorageReserved int64    `json:"storageReserved"`
					Tags            []string `json:"tags"`
				} `json:"disks"`
			} `json:"spec"`
			Status struct {
				Conditions []struct{ Type, Status string } `json:"conditions"`
				DiskStatus map[string]struct {
					StorageAvailable int64                           `json:"storageAvailable"`
					StorageMaximum   int64                           `json:"storageMaximum"`
					StorageScheduled int64                           `json:"storageScheduled"`
					Conditions       []struct{ Type, Status string } `json:"conditions"`
				} `json:"diskStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &nl); err != nil {
		return nil, fmt.Errorf("Longhorn's node list was not JSON: %w", err)
	}
	overPct, minPct := int64(100), int64(25) // Longhorn's defaults
	for _, s := range []struct {
		name string
		v    *int64
	}{{"storage-over-provisioning-percentage", &overPct}, {"storage-minimal-available-percentage", &minPct}} {
		if out, _, err := k.kubectl(ctx, "-n", longhornNS, "get", "settings.longhorn.io", s.name, "-o", "jsonpath={.value}"); err == nil {
			var n int64
			if _, err := fmt.Sscan(strings.TrimSpace(out), &n); err == nil && n >= 0 {
				*s.v = n
			}
		}
	}
	schedulable := func(cs []struct{ Type, Status string }) bool {
		for _, c := range cs {
			if c.Type == "Schedulable" {
				return c.Status == "True"
			}
		}
		return true
	}
	res := map[string][]lhDisk{}
	for _, n := range nl.Items {
		if !n.Spec.AllowScheduling || !schedulable(n.Status.Conditions) {
			res[n.Metadata.Name] = []lhDisk{} // read, and nothing goes there
			continue
		}
		var ds []lhDisk
		for id, d := range n.Spec.Disks {
			st, ok := n.Status.DiskStatus[id]
			if !d.AllowScheduling || !ok || !schedulable(st.Conditions) {
				continue
			}
			room := (st.StorageMaximum-d.StorageReserved)*overPct/100 - st.StorageScheduled
			if r := st.StorageAvailable - st.StorageMaximum*minPct/100; r < room {
				room = r
			}
			ds = append(ds, lhDisk{Name: id, Tags: d.Tags, Room: room})
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name })
		if ds == nil {
			ds = []lhDisk{}
		}
		res[n.Metadata.Name] = ds
	}
	return res, nil
}

// quotaFit reads the namespace's LimitRanges and ResourceQuotas and says what
// the VM would not fit in. inst is the VM when it exists already: a running
// VM's pod is counted in the quota's use, so only its new disks are added.
func (p *kubevirtProv) quotaFit(ctx context.Context, inst *Inst, disks []diskNeed, notes *[]string) []string {
	read := func(kind string) string {
		out, errOut, err := p.k.kubectl(ctx, p.k.ns("get", kind, "-o", "json")...)
		if err != nil {
			*notes = append(*notes, fmt.Sprintf("the namespace's %s could not be read (%s): not checked", kind, firstLine(errOut)))
			return ""
		}
		return out
	}
	return quotaMisfits(p.cfg, inst != nil, inst != nil && inst.State == stateRunning, disks, read("limitrange"), read("resourcequota"))
}

// quotaMisfits is quotaFit's pure half: the LimitRange and ResourceQuota
// lists as kubectl prints them ("" when unread).
func quotaMisfits(c *Config, exists, running bool, disks []diskNeed, limitRanges, quotas string) []string {
	var bad []string
	need := float64(c.vmMemoryBytes() + podOverheadBytes)

	var lrl struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				Limits []struct {
					Type string            `json:"type"`
					Max  map[string]string `json:"max"`
				} `json:"limits"`
			} `json:"spec"`
		} `json:"items"`
	}
	if limitRanges != "" && json.Unmarshal([]byte(limitRanges), &lrl) == nil {
		for _, lr := range lrl.Items {
			for _, l := range lr.Spec.Limits {
				max := func(res string) (float64, bool) {
					s, ok := l.Max[res]
					if !ok {
						return 0, false
					}
					f, err := parseQtyF(s)
					return f, err == nil
				}
				switch l.Type {
				case "Container", "Pod":
					if running {
						continue // the pod is admitted already
					}
					if m, ok := max("memory"); ok && need > m {
						bad = append(bad, fmt.Sprintf("LimitRange %s allows a %s at most %s of memory, the VM's pod needs %s", lr.Metadata.Name, strings.ToLower(l.Type), fmtGiB(int64(m)), fmtGiB(int64(need))))
					}
					if m, ok := max("cpu"); ok && float64(c.VMCPU) > m {
						bad = append(bad, fmt.Sprintf("LimitRange %s allows a %s at most %g CPU, vm_cpu is %d", lr.Metadata.Name, strings.ToLower(l.Type), m, c.VMCPU))
					}
				case "PersistentVolumeClaim":
					if m, ok := max("storage"); ok {
						for _, d := range disks {
							if float64(d.Bytes) > m {
								bad = append(bad, fmt.Sprintf("LimitRange %s allows a claim at most %s, the %s claims %s", lr.Metadata.Name, fmtGiB(int64(m)), d.What, fmtGiB(d.Bytes)))
							}
						}
					}
				}
			}
		}
	}

	add := map[string]float64{}
	if !running {
		for _, k := range []string{"memory", "requests.memory", "limits.memory"} {
			add[k] = need
		}
		add["cpu"], add["requests.cpu"], add["limits.cpu"] = float64(c.VMCPURequest), float64(c.VMCPURequest), float64(c.VMCPU)
		add["pods"] = 1
	}
	if !exists {
		add["count/virtualmachines.kubevirt.io"] = 1
	}
	for _, d := range disks {
		add["persistentvolumeclaims"]++
		add["count/persistentvolumeclaims"]++
		add["requests.storage"] += float64(d.Bytes)
	}
	var rql struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Status   struct {
				Hard map[string]string `json:"hard"`
				Used map[string]string `json:"used"`
			} `json:"status"`
		} `json:"items"`
	}
	if quotas != "" && json.Unmarshal([]byte(quotas), &rql) == nil {
		for _, q := range rql.Items {
			keys := make([]string, 0, len(q.Status.Hard))
			for k := range q.Status.Hard {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				a := add[k]
				if a == 0 {
					continue
				}
				hard, err := parseQtyF(q.Status.Hard[k])
				if err != nil {
					continue
				}
				used, _ := parseQtyF(q.Status.Used[k])
				if used+a <= hard {
					continue
				}
				amt := func(f float64) string {
					if strings.Contains(k, "memory") || strings.Contains(k, "storage") {
						return fmtGiB(int64(f))
					}
					return fmt.Sprintf("%g", f)
				}
				bad = append(bad, fmt.Sprintf("ResourceQuota %s has %s of %s left, the VM needs %s", q.Metadata.Name, amt(hard-used), k, amt(a)))
			}
		}
	}
	return bad
}
