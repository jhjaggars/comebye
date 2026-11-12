package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// gcloud builds and runs the gcloud commands. Every builder returns the
// argument list alone so --dry-run can print exactly what a real run does.
type gcloud struct {
	r   Runner
	cfg *Config
}

func (g *gcloud) base() []string {
	return []string{"--project=" + g.cfg.Project, "--quiet"}
}

func (g *gcloud) argv(parts ...string) []string {
	return append(append([]string{}, parts...), g.base()...)
}

func (g *gcloud) run(ctx context.Context, args []string) (string, error) {
	out, _, err := g.r.Run(ctx, "gcloud", args, nil)
	return strings.TrimSpace(out), err
}

func (g *gcloud) subnetName() string   { return g.cfg.Network + "-subnet" }
func (g *gcloud) firewallName() string { return g.cfg.Network + "-ssh" }

func (g *gcloud) networkCreate() []string {
	return g.argv("compute", "networks", "create", g.cfg.Network, "--subnet-mode=custom", "--description="+managedDesc)
}
func (g *gcloud) subnetCreate() []string {
	return g.argv("compute", "networks", "subnets", "create", g.subnetName(),
		"--network="+g.cfg.Network, "--region="+regionOf(g.cfg.Zone), "--range="+g.cfg.SubnetRange, "--description="+managedDesc)
}
func (g *gcloud) firewallCreate() []string {
	return g.argv("compute", "firewall-rules", "create", g.firewallName(),
		"--network="+g.cfg.Network, "--direction=INGRESS", "--action=ALLOW",
		"--rules=tcp:22", "--source-ranges="+g.cfg.SSHSourceCIDR, "--description="+managedDesc)
}
func (g *gcloud) dataDiskCreate(name string) []string {
	return g.argv("compute", "disks", "create", dataDiskName(name),
		"--zone="+g.cfg.Zone, "--size="+fmt.Sprint(g.cfg.DataDiskGB)+"GB",
		"--type="+g.cfg.DiskType, "--labels="+labelKey+"="+labelVal)
}

// instanceCreate carries no secret: the metadata is the public ssh key, the
// startup script and flags. The Tailscale key never goes through GCE.
func (g *gcloud) instanceCreate(name, pubKey, startupFile string) []string {
	a := g.argv("compute", "instances", "create", name,
		"--zone="+g.cfg.Zone,
		"--machine-type="+g.cfg.MachineType,
		"--image-project="+g.cfg.ImageProject, "--image-family="+g.cfg.ImageFamily,
		"--boot-disk-size="+fmt.Sprint(g.cfg.BootDiskGB)+"GB", "--boot-disk-type="+g.cfg.DiskType,
		"--disk=name="+dataDiskName(name)+",device-name=comebye-data,mode=rw,boot=no,auto-delete=no",
		"--network-interface=subnet="+g.subnetName()+",network-tier=PREMIUM",
		"--no-service-account", "--no-scopes",
		"--shielded-secure-boot", "--shielded-vtpm", "--shielded-integrity-monitoring",
		"--labels="+labelKey+"="+labelVal,
		"--metadata=block-project-ssh-keys=TRUE,enable-oslogin=FALSE,serial-port-enable=FALSE,enable-guest-attributes=TRUE,"+
			"ssh-keys="+g.cfg.SSHUser+":"+pubKey,
		"--metadata-from-file=startup-script="+startupFile,
	)
	if g.cfg.GCESpot {
		// A Spot VM that Google reclaims is stopped, not deleted: the disks and the
		// host's place on the tailnet survive, and `start` brings it back.
		a = append(a, "--provisioning-model=SPOT")
	}
	if g.cfg.MaxRunDuration != "" {
		a = append(a, "--max-run-duration="+g.cfg.MaxRunDuration)
	}
	if g.cfg.GCESpot || g.cfg.MaxRunDuration != "" {
		a = append(a, "--instance-termination-action=STOP")
	}
	return a
}
func (g *gcloud) instanceDescribe(name string) []string {
	return g.argv("compute", "instances", "describe", name, "--zone="+g.cfg.Zone, "--format=json")
}
func (g *gcloud) instanceAction(verb, name string) []string {
	return g.argv("compute", "instances", verb, name, "--zone="+g.cfg.Zone)
}
func (g *gcloud) guestAttr(name, path string) []string {
	return g.argv("compute", "instances", "get-guest-attributes", name, "--zone="+g.cfg.Zone,
		"--query-path="+path, "--format=value(value)")
}
func (g *gcloud) diskDelete(disk string) []string {
	return g.argv("compute", "disks", "delete", disk, "--zone="+g.cfg.Zone)
}
func (g *gcloud) instanceList() []string {
	return g.argv("compute", "instances", "list", "--filter=labels."+labelKey+"="+labelVal, "--format=json")
}
func (g *gcloud) diskList() []string {
	return g.argv("compute", "disks", "list", "--filter=labels."+labelKey+"="+labelVal, "--format=json")
}

func regionOf(zone string) string { return zone[:strings.LastIndex(zone, "-")] }

// Instance is the part of `instances describe` the tool reads.
type Instance struct {
	Name              string            `json:"name"`
	ID                string            `json:"id"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Status            string            `json:"status"`
	Zone              string            `json:"zone"`
	MachineType       string            `json:"machineType"`
	Labels            map[string]string `json:"labels"`
	NetworkInterfaces []struct {
		Network       string `json:"network"`
		AccessConfigs []struct {
			NatIP string `json:"natIP"`
		} `json:"accessConfigs"`
	} `json:"networkInterfaces"`
}

func (i *Instance) ExternalIP() string {
	for _, n := range i.NetworkInterfaces {
		for _, a := range n.AccessConfigs {
			if a.NatIP != "" {
				return a.NatIP
			}
		}
	}
	return ""
}

// describe returns nil, nil when the instance does not exist.
func (g *gcloud) describe(ctx context.Context, name string) (*Instance, error) {
	out, stderr, err := g.r.Run(ctx, "gcloud", g.instanceDescribe(name), nil)
	if err != nil {
		if strings.Contains(stderr, "was not found") || strings.Contains(stderr, "notFound") {
			return nil, nil
		}
		return nil, err
	}
	var i Instance
	if err := json.Unmarshal([]byte(out), &i); err != nil {
		return nil, fmt.Errorf("parsing instance %s: %w", name, err)
	}
	return &i, nil
}

// exists runs a describe-style command and reports whether the resource is
// there; "not found" is false, anything else an error.
func (g *gcloud) exists(ctx context.Context, args []string) (bool, error) {
	_, stderr, err := g.r.Run(ctx, "gcloud", args, nil)
	if err == nil {
		return true, nil
	}
	if strings.Contains(stderr, "was not found") || strings.Contains(stderr, "notFound") {
		return false, nil
	}
	return false, err
}

func (g *gcloud) ensure(ctx context.Context, what string, check, create []string, w logf) error {
	ok, err := g.exists(ctx, check)
	if err != nil {
		return fmt.Errorf("checking %s: %w", what, err)
	}
	if ok {
		w("  %s: present\n", what)
		return nil
	}
	w("  %s: creating\n", what)
	if _, err := g.run(ctx, create); err != nil {
		return fmt.Errorf("creating %s: %w", what, err)
	}
	return nil
}

// ---- Provider ----

func (g *gcloud) Kind() string { return "gcp" }

func (g *gcloud) Describe(ctx context.Context, name string) (*Inst, error) {
	i, err := g.describe(ctx, name)
	if err != nil || i == nil {
		return nil, err
	}
	st := i.Status
	if st == "TERMINATED" {
		st = stateStopped
	}
	created, _ := time.Parse(time.RFC3339, i.CreationTimestamp)
	return &Inst{Name: i.Name, ID: i.ID, State: st, Type: shortType(i.MachineType), IP: i.ExternalIP(),
		Managed: i.Labels[labelKey] == labelVal, Created: created}, nil
}

func (g *gcloud) Instances(ctx context.Context) ([]Inst, error) {
	out, err := g.run(ctx, g.instanceList())
	if err != nil {
		return nil, err
	}
	var list []Instance
	if out != "" {
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return nil, err
		}
	}
	insts := make([]Inst, 0, len(list))
	for _, i := range list {
		st := i.Status
		if st == "TERMINATED" {
			st = stateStopped
		}
		insts = append(insts, Inst{Name: i.Name, State: st})
	}
	return insts, nil
}

func (g *gcloud) EnsureNetwork(ctx context.Context, w logf) error {
	for _, s := range []struct {
		what          string
		check, create []string
	}{
		{"network " + g.cfg.Network, g.argv("compute", "networks", "describe", g.cfg.Network), g.networkCreate()},
		{"subnet " + g.subnetName(), g.argv("compute", "networks", "subnets", "describe", g.subnetName(), "--region="+regionOf(g.cfg.Zone)), g.subnetCreate()},
		{"firewall " + g.firewallName(), g.argv("compute", "firewall-rules", "describe", g.firewallName()), g.firewallCreate()},
	} {
		if err := g.ensure(ctx, s.what, s.check, s.create, w); err != nil {
			return err
		}
	}
	return nil
}

func (g *gcloud) EnsureDataDisk(ctx context.Context, name string, w logf) error {
	return g.ensure(ctx, "disk "+dataDiskName(name),
		g.argv("compute", "disks", "describe", dataDiskName(name), "--zone="+g.cfg.Zone), g.dataDiskCreate(name), w)
}

func (g *gcloud) Create(ctx context.Context, name, pubKey string) error {
	f, err := os.CreateTemp("", "comebye-startup-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(startupScript); err != nil {
		return err
	}
	f.Close()
	if _, err := g.run(ctx, g.instanceCreate(name, pubKey, f.Name())); err != nil {
		return fmt.Errorf("creating instance: %w", err)
	}
	return nil
}

// Attach is a no-op: the data disk is attached by instances create.
func (g *gcloud) Attach(context.Context, string) error { return nil }

func (g *gcloud) Start(ctx context.Context, name string) error {
	_, err := g.run(ctx, g.instanceAction("start", name))
	return err
}
func (g *gcloud) Stop(ctx context.Context, name string) error {
	_, err := g.run(ctx, g.instanceAction("stop", name))
	return err
}
func (g *gcloud) Delete(ctx context.Context, name string) error {
	_, err := g.run(ctx, g.instanceAction("delete", name))
	return err
}

func (g *gcloud) DeleteDataDisk(ctx context.Context, name string) error {
	ok, err := g.exists(ctx, g.argv("compute", "disks", "describe", dataDiskName(name), "--zone="+g.cfg.Zone))
	if err != nil || !ok {
		return err
	}
	_, err = g.run(ctx, g.diskDelete(dataDiskName(name)))
	return err
}

func (g *gcloud) HostKey(ctx context.Context, name string, _ *Inst) (string, error) {
	out, _, err := g.r.Run(ctx, "gcloud", g.guestAttr(name, "comebye/ssh-host-key"), nil)
	if err != nil {
		return "", nil
	}
	return parseHostKey(out)
}

func (g *gcloud) List(ctx context.Context, w logf) error {
	out, err := g.run(ctx, g.instanceList())
	if err != nil {
		return err
	}
	var insts []Instance
	if out != "" {
		if err := json.Unmarshal([]byte(out), &insts); err != nil {
			return err
		}
	}
	w("instances (label %s=%s):\n", labelKey, labelVal)
	for _, i := range insts {
		w("  %-24s %-11s %s\n", i.Name, i.Status, orDash(i.ExternalIP()))
	}
	out, err = g.run(ctx, g.diskList())
	if err != nil {
		return err
	}
	var disks []struct {
		Name  string   `json:"name"`
		Users []string `json:"users"`
		Size  string   `json:"sizeGb"`
	}
	if out != "" {
		if err := json.Unmarshal([]byte(out), &disks); err != nil {
			return err
		}
	}
	w("disks:\n")
	for _, d := range disks {
		use := "unattached (billed, kept)"
		if len(d.Users) > 0 {
			use = "attached"
		}
		w("  %-24s %s GB  %s\n", d.Name, d.Size, use)
	}
	return nil
}

// CleanupNetwork deletes the firewall rule, subnet and network this tool
// made, but only when all of them carry the managed description (GCE
// networks have no labels) and no instance in the project uses the network.
// Anything else is left alone with a note.
func (g *gcloud) CleanupNetwork(ctx context.Context, w logf) error {
	out, err := g.run(ctx, g.argv("compute", "instances", "list", "--format=json"))
	if err != nil {
		return err
	}
	var insts []Instance
	if out != "" {
		if err := json.Unmarshal([]byte(out), &insts); err != nil {
			return err
		}
	}
	for _, i := range insts {
		for _, n := range i.NetworkInterfaces {
			if n.Network == g.cfg.Network || strings.HasSuffix(n.Network, "/networks/"+g.cfg.Network) {
				w("  network %s kept: instance %s still uses it\n", g.cfg.Network, i.Name)
				return nil
			}
		}
	}
	region := "--region=" + regionOf(g.cfg.Zone)
	type res struct {
		what     string
		describe []string
		delete   []string
	}
	rs := []res{
		{"firewall rule " + g.firewallName(), g.argv("compute", "firewall-rules", "describe", g.firewallName(), "--format=value(description)"),
			g.argv("compute", "firewall-rules", "delete", g.firewallName())},
		{"subnet " + g.subnetName(), g.argv("compute", "networks", "subnets", "describe", g.subnetName(), region, "--format=value(description)"),
			g.argv("compute", "networks", "subnets", "delete", g.subnetName(), region)},
		{"network " + g.cfg.Network, g.argv("compute", "networks", "describe", g.cfg.Network, "--format=value(description)"),
			g.argv("compute", "networks", "delete", g.cfg.Network)},
	}
	var present []res
	for _, r := range rs {
		o, stderr, err := g.r.Run(ctx, "gcloud", r.describe, nil)
		if err != nil {
			if strings.Contains(stderr, "was not found") || strings.Contains(stderr, "notFound") {
				continue
			}
			return err
		}
		if strings.TrimSpace(o) != managedDesc {
			w("  network cleanup skipped: %s is not marked %s (not made by this tool); leaving all of it\n", r.what, managedDesc)
			return nil
		}
		present = append(present, r)
	}
	for _, r := range present {
		if _, err := g.run(ctx, r.delete); err != nil {
			return fmt.Errorf("deleting %s: %w", r.what, err)
		}
		w("  deleted %s\n", r.what)
	}
	return nil
}

func (g *gcloud) Plan(name, pub string, w logf) {
	w("   network, subnet and firewall (each created only if missing; marked %s in their description)\n", managedDesc)
	for _, a := range [][]string{g.networkCreate(), g.subnetCreate(), g.firewallCreate()} {
		w("     %s\n", cmdLine("gcloud", a))
	}
	w("   data disk (kept by destroy unless --delete-data-disk)\n     %s\n", cmdLine("gcloud", g.dataDiskCreate(name)))
	w("   instance (the startup script only mounts /data and publishes the ssh host key)\n     %s\n", cmdLine("gcloud", g.instanceCreate(name, pub, "<startup.sh>")))
	w("   host key, read from GCE guest attributes:\n     %s\n", cmdLine("gcloud", g.guestAttr(name, "comebye/ssh-host-key")))
}
