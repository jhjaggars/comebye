package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The methods of the kubevirt provider that change (or read the state of) the
// cluster. Every object carries managed-by=comebye-worker, and nothing is
// changed or deleted without finding that label first.
//
// The ssh host key is made here, before the VM exists: its private half goes
// into the cloud-init Secret (via kubectl's stdin) and its public half is
// kept on the Secret as an annotation, which is what HostKey returns. The
// guest never gets to say what its key is.

// vmDoc is the part of a VirtualMachine the tool reads.
type vmDoc struct {
	Metadata struct {
		Name              string
		UID               string
		CreationTimestamp time.Time
		Labels            map[string]string
	}
	Spec struct {
		RunStrategy string
		Template    struct {
			Spec struct {
				Domain struct {
					CPU       struct{ Cores int }
					Resources struct{ Limits map[string]string }
				}
			}
		}
	}
	Status struct{ PrintableStatus string }
}

func (d vmDoc) inst() Inst {
	st := d.Status.PrintableStatus
	switch st {
	case "Running":
		st = stateRunning
	case "Stopped":
		st = stateStopped
	default:
		st = strings.ToUpper(st)
	}
	return Inst{
		Name: d.Metadata.Name, ID: d.Metadata.UID, State: st,
		Type:    fmt.Sprintf("%d vCPU, %s", d.Spec.Template.Spec.Domain.CPU.Cores, d.Spec.Template.Spec.Domain.Resources.Limits["memory"]),
		Managed: d.Metadata.Labels[labelKey] == labelVal,
		Created: d.Metadata.CreationTimestamp,
	}
}

// managedLabel reads an object's managed-by label: found, managed, error.
func (p *kubevirtProv) managedLabel(ctx context.Context, kind, name string) (found, managed bool, err error) {
	out, errOut, err := p.k.kubectl(ctx, p.k.ns("get", kind, name, "-o", "jsonpath={.metadata.labels."+strings.ReplaceAll(labelKey, ".", `\.`)+"}")...)
	if err != nil {
		if strings.Contains(errOut+err.Error(), "NotFound") || strings.Contains(errOut, "not found") {
			return false, false, nil
		}
		return false, false, fmt.Errorf("kubectl get %s %s: %v", kind, name, firstLine(errOut))
	}
	return true, strings.TrimSpace(out) == labelVal, nil
}

// ownedOrAbsent refuses to go on when kind/name exists without our label.
func (p *kubevirtProv) ownedOrAbsent(ctx context.Context, kind, name string) (found bool, err error) {
	found, managed, err := p.managedLabel(ctx, kind, name)
	if err != nil {
		return false, err
	}
	if found && !managed {
		return true, fmt.Errorf("%s %s exists in %s without %s=%s: refusing to touch it", kind, name, p.cfg.KubeNamespace, labelKey, labelVal)
	}
	return found, nil
}

func (p *kubevirtProv) apply(ctx context.Context, obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, errOut, err := p.k.kubectlIn(ctx, b, "apply", "-f", "-")
	if err != nil {
		return fmt.Errorf("kubectl apply: %v", firstLine(errOut))
	}
	return nil
}

func (p *kubevirtProv) Describe(ctx context.Context, name string) (*Inst, error) {
	out, errOut, err := p.k.kubectl(ctx, p.k.ns("get", "vm", name, "-o", "json")...)
	if err != nil {
		if strings.Contains(errOut+err.Error(), "NotFound") || strings.Contains(errOut, "not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("kubectl get vm %s: %v", name, firstLine(errOut))
	}
	var d vmDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return nil, fmt.Errorf("the VM was not JSON: %w", err)
	}
	i := d.inst()
	if i.State == stateRunning {
		// The pod IP only tells connect that the VMI has a network; ssh does
		// not use it (the transport is virtctl port-forward).
		if vout, _, verr := p.k.kubectl(ctx, p.k.ns("get", "vmi", name, "-o", "json")...); verr == nil {
			var v struct {
				Status struct {
					NodeName   string
					Interfaces []struct {
						IPAddress string `json:"ipAddress"`
					}
				}
			}
			if json.Unmarshal([]byte(vout), &v) == nil {
				if len(v.Status.Interfaces) > 0 {
					i.IP = v.Status.Interfaces[0].IPAddress
				}
				i.Note = "on node " + shortNode(v.Status.NodeName)
			}
		}
	}
	return &i, nil
}

func (p *kubevirtProv) EnsureNetwork(context.Context, logf) error { return nil }

func (p *kubevirtProv) EnsureDataDisk(ctx context.Context, name string, w logf) error {
	found, err := p.ownedOrAbsent(ctx, "datavolume", dataDVName(name))
	if err != nil {
		return err
	}
	if !found {
		// CDI may have deleted a finished DataVolume and left the claim.
		found, err = p.k.exists(ctx, p.k.ns("pvc", dataDVName(name))...)
		if err != nil {
			return err
		}
	}
	if found {
		w("  data disk %s: present\n", dataDVName(name))
		return nil
	}
	w("  data disk %s: creating (%d GiB on %s)\n", dataDVName(name), p.cfg.DataDiskGB, p.cfg.dataStorageClass())
	return p.apply(ctx, renderDataDV(p.cfg, name))
}

// newHostKey makes an ed25519 host key pair and returns both halves.
func (p *kubevirtProv) newHostKey(ctx context.Context) (priv, pub string, err error) {
	dir, err := os.MkdirTemp("", "comebye-hostkey-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "k")
	if _, errOut, err := p.k.r.Run(ctx, "ssh-keygen", []string{"-q", "-t", "ed25519", "-N", "", "-C", "", "-f", f}, nil); err != nil {
		return "", "", fmt.Errorf("ssh-keygen: %v", firstLine(errOut))
	}
	pb, err := os.ReadFile(f + ".pub")
	if err != nil {
		return "", "", err
	}
	kb, err := os.ReadFile(f)
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(string(pb))
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", "", fmt.Errorf("ssh-keygen made no ed25519 key")
	}
	return string(kb), fields[0] + " " + fields[1], nil
}

func (p *kubevirtProv) Create(ctx context.Context, name, pubKey string) error {
	if _, err := p.ownedOrAbsent(ctx, "secret", cloudInitName(name)); err != nil {
		return err
	}
	g, err := p.Gate(ctx)
	if err != nil {
		return fmt.Errorf("capacity gate: %w", err)
	}
	if err := g.Err(); err != nil {
		return err
	}
	// A leftover Secret from an attempt that never made a VM is replaced: no
	// connection has pinned its key yet.
	priv, hostPub, err := p.newHostKey(ctx)
	if err != nil {
		return err
	}
	if err := p.apply(ctx, renderSecret(p.cfg, name, pubKey, priv, hostPub)); err != nil {
		return err
	}
	return p.apply(ctx, renderVM(p.cfg, name, g.Chosen))
}

func (p *kubevirtProv) Attach(context.Context, string) error { return nil }

func (p *kubevirtProv) patchRun(ctx context.Context, name, strategy string) error {
	_, errOut, err := p.k.kubectl(ctx, p.k.ns("patch", "vm", name, "--type", "merge", "-p", `{"spec":{"runStrategy":"`+strategy+`"}}`)...)
	if err != nil {
		return fmt.Errorf("kubectl patch vm %s: %v", name, firstLine(errOut))
	}
	return nil
}

// waitState polls until the VM's state is want, or the VMI is gone for stopped.
func (p *kubevirtProv) waitState(ctx context.Context, name, want string, tries int) error {
	for i := 0; i < tries; i++ {
		inst, err := p.Describe(ctx, name)
		if err != nil {
			return err
		}
		if inst != nil && inst.State == want {
			if want != stateStopped {
				return nil
			}
			if ok, _ := p.k.exists(ctx, p.k.ns("vmi", name)...); !ok {
				return nil
			}
		}
		p.wait(5)
	}
	return fmt.Errorf("VM %s did not become %s in time", name, want)
}

// Stop halts the VM (a graceful shutdown) and waits for it. The run strategy
// goes to Halted; Start puts RerunOnFailure back.
func (p *kubevirtProv) Stop(ctx context.Context, name string) error {
	if err := p.guard(ctx, name); err != nil {
		return err
	}
	if err := p.patchRun(ctx, name, "Halted"); err != nil {
		return err
	}
	return p.waitState(ctx, name, stateStopped, 36)
}

// Start boots a stopped VM. An idle poweroff leaves the strategy at
// RerunOnFailure with no VMI, which a patch to the same value would not
// restart, so it goes through Halted first.
func (p *kubevirtProv) Start(ctx context.Context, name string) error {
	if err := p.guard(ctx, name); err != nil {
		return err
	}
	if err := p.patchRun(ctx, name, "Halted"); err != nil {
		return err
	}
	if err := p.waitState(ctx, name, stateStopped, 36); err != nil {
		return err
	}
	return p.patchRun(ctx, name, "RerunOnFailure")
}

// guard is the label check in front of every change to a VM.
func (p *kubevirtProv) guard(ctx context.Context, name string) error {
	inst, err := p.Describe(ctx, name)
	if err != nil {
		return err
	}
	if inst == nil {
		return fmt.Errorf("no VM %s in %s", name, p.cfg.KubeNamespace)
	}
	return checkOwned(inst)
}

// Delete removes the VM (its boot disk goes with it: the boot DataVolume is
// owned by the VM) and the cloud-init Secret. The data disk is kept.
func (p *kubevirtProv) Delete(ctx context.Context, name string) error {
	if err := p.guard(ctx, name); err != nil {
		return err
	}
	if _, errOut, err := p.k.kubectl(ctx, p.k.ns("delete", "vm", name, "--wait=true", "--timeout=5m")...); err != nil {
		return fmt.Errorf("kubectl delete vm %s: %v", name, firstLine(errOut))
	}
	// A boot DataVolume that outlived the VM goes too, if it is ours.
	for _, o := range [][2]string{{"datavolume", bootDVName(name)}, {"secret", cloudInitName(name)}} {
		found, managed, err := p.managedLabel(ctx, o[0], o[1])
		if err != nil {
			return err
		}
		if found && managed {
			if _, errOut, err := p.k.kubectl(ctx, p.k.ns("delete", o[0], o[1], "--ignore-not-found")...); err != nil {
				return fmt.Errorf("kubectl delete %s %s: %v", o[0], o[1], firstLine(errOut))
			}
		}
	}
	return nil
}

// DeleteDataDisk deletes the data DataVolume (and its claim) if it is ours.
func (p *kubevirtProv) DeleteDataDisk(ctx context.Context, name string) error {
	for _, kind := range []string{"datavolume", "pvc"} {
		found, managed, err := p.managedLabel(ctx, kind, dataDVName(name))
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if !managed {
			if kind == "datavolume" {
				return fmt.Errorf("%s %s is not labelled %s=%s: refusing to delete it", kind, dataDVName(name), labelKey, labelVal)
			}
			continue // a claim CDI made without our label goes with its DataVolume
		}
		if _, errOut, err := p.k.kubectl(ctx, p.k.ns("delete", kind, dataDVName(name), "--ignore-not-found", "--wait=true", "--timeout=5m")...); err != nil {
			return fmt.Errorf("kubectl delete %s %s: %v", kind, dataDVName(name), firstLine(errOut))
		}
	}
	return nil
}

// HostKey returns the public host key this tool pinned at create, kept on the
// cloud-init Secret. Nothing the guest says is believed.
func (p *kubevirtProv) HostKey(ctx context.Context, name string, _ *Inst) (string, error) {
	out, _, err := p.k.kubectl(ctx, p.k.ns("get", "secret", cloudInitName(name), "-o", `jsonpath={.metadata.annotations.comebye-worker/host-key}`)...)
	if err != nil {
		return "", nil
	}
	return parseHostKey(out)
}

func (p *kubevirtProv) vms(ctx context.Context) ([]Inst, error) {
	out, errOut, err := p.k.kubectl(ctx, p.k.ns("get", "vm", "-l", labelKey+"="+labelVal, "-o", "json")...)
	if err != nil {
		return nil, fmt.Errorf("kubectl get vm: %v", firstLine(errOut))
	}
	var l struct{ Items []vmDoc }
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return nil, err
	}
	var res []Inst
	for _, d := range l.Items {
		res = append(res, d.inst())
	}
	return res, nil
}

func (p *kubevirtProv) Instances(ctx context.Context) ([]Inst, error) { return p.vms(ctx) }

func (p *kubevirtProv) List(ctx context.Context, w logf) error {
	vms, err := p.vms(ctx)
	if err != nil {
		return err
	}
	w("VMs in %s labelled %s=%s:\n", p.cfg.KubeNamespace, labelKey, labelVal)
	for _, i := range vms {
		w("  %-16s %-10s %s\n", i.Name, i.State, i.Type)
	}
	out, _, err := p.k.kubectl(ctx, p.k.ns("get", "datavolume", "-l", labelKey+"="+labelVal, "-o", "custom-columns=NAME:.metadata.name,PHASE:.status.phase", "--no-headers")...)
	if err != nil {
		return nil
	}
	w("DataVolumes:\n")
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			w("  %s\n", l)
		}
	}
	return nil
}

func (p *kubevirtProv) CleanupNetwork(context.Context, logf) error { return nil }

// DiskLines says, for status, where each disk is: the claim's phase, size and
// class, and the DataVolume's phase (an import or clone in progress shows
// here). A disk that cannot be read says so; nothing here changes anything.
func (p *kubevirtProv) DiskLines(ctx context.Context, name string) []string {
	var res []string
	for _, d := range []struct{ label, obj, keep string }{
		{"boot disk", bootDVName(name), "deleted with the VM"},
		{"data disk", dataDVName(name), "kept by destroy unless --delete-data-disk"},
	} {
		res = append(res, fmt.Sprintf("%-11s %s: %s (%s)", d.label, d.obj, p.diskState(ctx, d.obj), d.keep))
	}
	return res
}

func (p *kubevirtProv) diskState(ctx context.Context, obj string) string {
	notFound := func(errOut string, err error) bool {
		return strings.Contains(errOut+err.Error(), "NotFound") || strings.Contains(errOut, "not found")
	}
	var parts []string
	out, errOut, err := p.k.kubectl(ctx, p.k.ns("get", "pvc", obj, "-o", "json")...)
	switch {
	case err == nil:
		var c struct {
			Spec struct {
				StorageClassName string `json:"storageClassName"`
			}
			Status struct {
				Phase    string
				Capacity map[string]string
			}
		}
		if json.Unmarshal([]byte(out), &c) != nil {
			parts = append(parts, "claim unreadable")
			break
		}
		s := "claim " + orDash(c.Status.Phase)
		if size := c.Status.Capacity["storage"]; size != "" {
			s += ", " + size
		}
		if c.Spec.StorageClassName != "" {
			s += " on " + c.Spec.StorageClassName
		}
		parts = append(parts, s)
	case notFound(errOut, err):
		parts = append(parts, "no claim")
	default:
		parts = append(parts, "claim: "+firstLine(errOut))
	}
	out, errOut, err = p.k.kubectl(ctx, p.k.ns("get", "datavolume", obj, "-o", "json")...)
	switch {
	case err == nil:
		var dv struct {
			Status struct{ Phase, Progress string }
		}
		if json.Unmarshal([]byte(out), &dv) == nil && dv.Status.Phase != "" {
			s := "DataVolume " + dv.Status.Phase
			if dv.Status.Phase != "Succeeded" && dv.Status.Progress != "" && dv.Status.Progress != "N/A" {
				s += " " + dv.Status.Progress
			}
			parts = append(parts, s)
		}
	case notFound(errOut, err):
		// CDI may delete a finished DataVolume and leave the claim.
	default:
		parts = append(parts, "DataVolume: "+firstLine(errOut))
	}
	return strings.Join(parts, ", ")
}
