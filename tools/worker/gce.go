package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	return g.argv("compute", "networks", "create", g.cfg.Network, "--subnet-mode=custom")
}
func (g *gcloud) subnetCreate() []string {
	return g.argv("compute", "networks", "subnets", "create", g.subnetName(),
		"--network="+g.cfg.Network, "--region="+regionOf(g.cfg.Zone), "--range="+g.cfg.SubnetRange)
}
func (g *gcloud) firewallCreate() []string {
	return g.argv("compute", "firewall-rules", "create", g.firewallName(),
		"--network="+g.cfg.Network, "--direction=INGRESS", "--action=ALLOW",
		"--rules=tcp:22", "--source-ranges="+g.cfg.SSHSourceCIDR)
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
	if g.cfg.MaxRunDuration != "" {
		a = append(a, "--max-run-duration="+g.cfg.MaxRunDuration, "--instance-termination-action=STOP")
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
	Status            string            `json:"status"`
	Zone              string            `json:"zone"`
	MachineType       string            `json:"machineType"`
	Labels            map[string]string `json:"labels"`
	NetworkInterfaces []struct {
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

func (g *gcloud) ensure(ctx context.Context, what string, check, create []string, w func(string, ...any)) error {
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

// checkOwned refuses to touch an instance this tool did not label.
func checkOwned(i *Instance) error {
	if i.Labels[labelKey] != labelVal {
		return fmt.Errorf("instance %s is not labelled %s=%s: refusing to manage it", i.Name, labelKey, labelVal)
	}
	return nil
}
