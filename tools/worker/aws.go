package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// awsCLI builds and runs `aws` commands. Credentials are the user's own AWS
// CLI profile or environment: this tool passes at most --profile NAME and
// never reads or stores a key.
type awsCLI struct {
	r   Runner
	cfg *Config
}

const ssmDebian = "/aws/service/debian/release/12/latest/amd64"

func (a *awsCLI) argv(parts ...string) []string {
	out := append([]string{}, parts...)
	out = append(out, "--region", a.cfg.AWSRegion, "--no-cli-pager")
	if a.cfg.AWSProfile != "" {
		out = append(out, "--profile", a.cfg.AWSProfile)
	}
	return out
}

// text builds a command whose answer is one text value (a --query result).
func (a *awsCLI) text(parts ...string) []string {
	return append(a.argv(parts...), "--output", "text")
}

func (a *awsCLI) json(parts ...string) []string {
	return append(a.argv(parts...), "--output", "json")
}

func (a *awsCLI) runText(ctx context.Context, args []string) (string, error) {
	out, _, err := a.r.Run(ctx, "aws", args, nil)
	out = strings.TrimSpace(out)
	if out == "None" {
		out = ""
	}
	return out, err
}

// tags renders the tag list for --tag-specifications and filters.
func (a *awsCLI) tagSpec(resource, name string) string {
	return fmt.Sprintf("ResourceType=%s,Tags=[{Key=%s,Value=%s},{Key=Name,Value=%s}]", resource, labelKey, labelVal, name)
}

func (a *awsCLI) managedFilters(name string, extra ...string) []string {
	f := []string{"--filters", "Name=tag:" + labelKey + ",Values=" + labelVal, "Name=tag:Name,Values=" + name}
	return append(f, extra...)
}

type awsProv struct {
	a   *awsCLI
	cfg *Config
}

func (p *awsProv) Kind() string { return "aws" }

func (p *awsProv) sgName() string     { return p.cfg.Network + "-ssh" }
func (p *awsProv) subnetName() string { return p.cfg.Network + "-subnet" }
func (p *awsProv) rtName() string     { return p.cfg.Network + "-rt" }

// ---- lookups (each returns "" when the resource is absent) ----

func (p *awsProv) vpcID(ctx context.Context) (string, error) {
	return p.a.runText(ctx, p.a.text(append([]string{"ec2", "describe-vpcs"},
		append(p.a.managedFilters(p.cfg.Network), "--query", "Vpcs[0].VpcId")...)...))
}

func (p *awsProv) inVPC(ctx context.Context, op, query, name, vpc string) (string, error) {
	return p.a.runText(ctx, p.a.text(append([]string{"ec2", op},
		append(p.a.managedFilters(name, "Name=vpc-id,Values="+vpc), "--query", query)...)...))
}

func (p *awsProv) igwID(ctx context.Context) (string, error) {
	return p.a.runText(ctx, p.a.text(append([]string{"ec2", "describe-internet-gateways"},
		append(p.a.managedFilters(p.cfg.Network), "--query", "InternetGateways[0].InternetGatewayId")...)...))
}

func (p *awsProv) sgID(ctx context.Context, vpc string) (string, error) {
	return p.a.runText(ctx, p.a.text("ec2", "describe-security-groups", "--filters",
		"Name=group-name,Values="+p.sgName(), "Name=vpc-id,Values="+vpc, "Name=tag:"+labelKey+",Values="+labelVal,
		"--query", "SecurityGroups[0].GroupId"))
}

type awsVol struct{ ID, Instance, AZ string }

func (p *awsProv) volume(ctx context.Context, name string) (*awsVol, error) {
	out, err := p.a.runText(ctx, p.a.text(append([]string{"ec2", "describe-volumes"},
		append(p.a.managedFilters(dataDiskName(name)), "--query", "Volumes[0].[VolumeId,Attachments[0].InstanceId,AvailabilityZone]")...)...))
	if err != nil || out == "" {
		return nil, err
	}
	f := strings.Fields(out)
	v := &awsVol{ID: f[0]}
	if len(f) > 1 && f[1] != "None" {
		v.Instance = f[1]
	}
	if len(f) > 2 {
		v.AZ = f[2]
	}
	return v, nil
}

// ---- network ----

func (p *awsProv) EnsureNetwork(ctx context.Context, w logf) error {
	a, c := p.a, p.cfg
	step := func(what, id string, err error) error {
		if err != nil {
			return fmt.Errorf("checking %s: %w", what, err)
		}
		if id != "" {
			w("  %s: present (%s)\n", what, id)
		}
		return nil
	}
	create := func(what string, args []string) (string, error) {
		w("  %s: creating\n", what)
		id, err := a.runText(ctx, args)
		if err != nil || id == "" {
			return "", fmt.Errorf("creating %s: %v", what, err)
		}
		return id, nil
	}

	vpc, err := p.vpcID(ctx)
	if err := step("vpc "+c.Network, vpc, err); err != nil {
		return err
	}
	if vpc == "" {
		if vpc, err = create("vpc "+c.Network, a.text("ec2", "create-vpc", "--cidr-block", c.VPCRange,
			"--tag-specifications", a.tagSpec("vpc", c.Network), "--query", "Vpc.VpcId")); err != nil {
			return err
		}
	}

	igw, err := p.igwID(ctx)
	if err := step("internet gateway", igw, err); err != nil {
		return err
	}
	if igw == "" {
		if igw, err = create("internet gateway", a.text("ec2", "create-internet-gateway",
			"--tag-specifications", a.tagSpec("internet-gateway", c.Network), "--query", "InternetGateway.InternetGatewayId")); err != nil {
			return err
		}
	}
	att, err := a.runText(ctx, a.text("ec2", "describe-internet-gateways", "--internet-gateway-ids", igw, "--query", "InternetGateways[0].Attachments[0].VpcId"))
	if err != nil {
		return err
	}
	if att == "" {
		if _, err := a.runText(ctx, a.text("ec2", "attach-internet-gateway", "--internet-gateway-id", igw, "--vpc-id", vpc)); err != nil {
			return fmt.Errorf("attaching the internet gateway: %w", err)
		}
	}

	sn, err := p.inVPC(ctx, "describe-subnets", "Subnets[0].SubnetId", p.subnetName(), vpc)
	if err := step("subnet "+p.subnetName(), sn, err); err != nil {
		return err
	}
	if sn == "" {
		if sn, err = create("subnet "+p.subnetName(), a.text("ec2", "create-subnet", "--vpc-id", vpc, "--cidr-block", c.SubnetRange,
			"--availability-zone", c.AWSAZ, "--tag-specifications", a.tagSpec("subnet", p.subnetName()), "--query", "Subnet.SubnetId")); err != nil {
			return err
		}
	}

	rt, err := p.inVPC(ctx, "describe-route-tables", "RouteTables[0].RouteTableId", p.rtName(), vpc)
	if err := step("route table "+p.rtName(), rt, err); err != nil {
		return err
	}
	if rt == "" {
		if rt, err = create("route table "+p.rtName(), a.text("ec2", "create-route-table", "--vpc-id", vpc,
			"--tag-specifications", a.tagSpec("route-table", p.rtName()), "--query", "RouteTable.RouteTableId")); err != nil {
			return err
		}
	}
	gw, err := a.runText(ctx, a.text("ec2", "describe-route-tables", "--route-table-ids", rt, "--query",
		"RouteTables[0].Routes[?DestinationCidrBlock=='0.0.0.0/0'].GatewayId|[0]"))
	if err != nil {
		return err
	}
	if gw == "" {
		if _, err := a.runText(ctx, a.text("ec2", "create-route", "--route-table-id", rt, "--destination-cidr-block", "0.0.0.0/0", "--gateway-id", igw)); err != nil {
			return fmt.Errorf("creating the default route: %w", err)
		}
	}
	as, err := a.runText(ctx, a.text("ec2", "describe-route-tables", "--route-table-ids", rt, "--query",
		"RouteTables[0].Associations[?SubnetId=='"+sn+"'].RouteTableAssociationId|[0]"))
	if err != nil {
		return err
	}
	if as == "" {
		if _, err := a.runText(ctx, a.text("ec2", "associate-route-table", "--route-table-id", rt, "--subnet-id", sn)); err != nil {
			return fmt.Errorf("associating the route table: %w", err)
		}
	}

	sg, err := p.sgID(ctx, vpc)
	if err := step("security group "+p.sgName(), sg, err); err != nil {
		return err
	}
	if sg == "" {
		if sg, err = create("security group "+p.sgName(), a.text("ec2", "create-security-group", "--group-name", p.sgName(),
			"--description", "comebye worker ssh from one address", "--vpc-id", vpc,
			"--tag-specifications", a.tagSpec("security-group", p.sgName()), "--query", "GroupId")); err != nil {
			return err
		}
		if _, err := a.runText(ctx, a.text("ec2", "authorize-security-group-ingress", "--group-id", sg,
			"--protocol", "tcp", "--port", "22", "--cidr", c.SSHSourceCIDR)); err != nil {
			return fmt.Errorf("opening tcp:22 to %s: %w", c.SSHSourceCIDR, err)
		}
	}
	return nil
}

func (p *awsProv) EnsureDataDisk(ctx context.Context, name string, w logf) error {
	v, err := p.volume(ctx, name)
	if err != nil {
		return fmt.Errorf("checking volume: %w", err)
	}
	if v != nil {
		if v.AZ != "" && v.AZ != p.cfg.AWSAZ {
			return fmt.Errorf("volume %s is in %s, not %s: a volume cannot move between zones", v.ID, v.AZ, p.cfg.AWSAZ)
		}
		w("  volume %s: present (%s)\n", dataDiskName(name), v.ID)
		return nil
	}
	w("  volume %s: creating\n", dataDiskName(name))
	_, err = p.a.runText(ctx, p.a.text(p.createVolume(name)...))
	if err != nil {
		return fmt.Errorf("creating volume: %w", err)
	}
	return nil
}

func (p *awsProv) createVolume(name string) []string {
	return p.a.argv("ec2", "create-volume", "--availability-zone", p.cfg.AWSAZ, "--size", fmt.Sprint(p.cfg.DataDiskGB),
		"--volume-type", p.cfg.DiskType, "--encrypted",
		"--tag-specifications", p.a.tagSpec("volume", dataDiskName(name)), "--query", "VolumeId")
}

// ---- instance ----

type awsInstance struct {
	InstanceID   string `json:"InstanceId"`
	InstanceType string
	PublicIP     string `json:"PublicIpAddress"`
	State        struct{ Name string }
	Lifecycle    string `json:"InstanceLifecycle"`
	SpotRequest  string `json:"SpotInstanceRequestId"`
	Reason       string `json:"StateTransitionReason"`
	Tags         []struct{ Key, Value string }
	RootDevice   string `json:"RootDeviceName"`
	Devices      []struct {
		DeviceName string
		Ebs        struct{ AttachTime string }
	} `json:"BlockDeviceMappings"`
}

// created is when the root volume was attached: when the VM was first made.
// LaunchTime will not do, because every start changes it.
func (i *awsInstance) created() time.Time {
	for _, d := range i.Devices {
		if d.DeviceName == i.RootDevice {
			t, _ := time.Parse(time.RFC3339, d.Ebs.AttachTime)
			return t
		}
	}
	return time.Time{}
}

func (p *awsProv) find(ctx context.Context, name string) (*awsInstance, error) {
	out, _, err := p.a.r.Run(ctx, "aws", p.a.json("ec2", "describe-instances", "--filters", "Name=tag:Name,Values="+name,
		"Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down"), nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Reservations []struct{ Instances []awsInstance }
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, fmt.Errorf("parsing instances: %w", err)
	}
	for _, rv := range r.Reservations {
		for i := range rv.Instances {
			return &rv.Instances[i], nil
		}
	}
	return nil, nil
}

func (p *awsProv) Describe(ctx context.Context, name string) (*Inst, error) {
	i, err := p.find(ctx, name)
	if err != nil || i == nil {
		return nil, err
	}
	managed := false
	for _, t := range i.Tags {
		if t.Key == labelKey && t.Value == labelVal {
			managed = true
		}
	}
	st := strings.ToUpper(i.State.Name)
	note := ""
	if i.Lifecycle == "spot" {
		note = "spot instance (persistent request, stops when AWS reclaims the capacity)"
		if strings.Contains(i.Reason, "Spot") {
			note = "spot instance, stopped by AWS (" + i.Reason + "): it restarts by itself when capacity returns"
		}
	}
	return &Inst{Name: name, ID: i.InstanceID, State: st, Type: i.InstanceType, IP: i.PublicIP, Managed: managed, Note: note, Created: i.created()}, nil
}

func (p *awsProv) Instances(ctx context.Context) ([]Inst, error) {
	out, err := p.a.runText(ctx, p.a.text("ec2", "describe-instances", "--filters", "Name=tag:"+labelKey+",Values="+labelVal,
		"Name=instance-state-name,Values=pending,running,stopping,stopped",
		"--query", "Reservations[].Instances[].[Tags[?Key=='Name']|[0].Value,State.Name]"))
	if err != nil {
		return nil, err
	}
	var insts []Inst
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			insts = append(insts, Inst{Name: f[0], State: strings.ToUpper(f[1])})
		}
	}
	return insts, nil
}

func (p *awsProv) imageID(ctx context.Context) (string, error) {
	if p.cfg.ImageID != "" {
		return p.cfg.ImageID, nil
	}
	id, err := p.a.runText(ctx, p.a.text("ssm", "get-parameters", "--names", ssmDebian, "--query", "Parameters[0].Value"))
	if err != nil || !amiRe.MatchString(id) {
		return "", fmt.Errorf("resolving the Debian 12 AMI from %s: %v %q", ssmDebian, err, id)
	}
	return id, nil
}

// runInstances is the launch command. Nothing in it is a secret; there is
// no instance profile, IMDSv2 is required with hop limit 1, the root volume
// is encrypted, and a guest poweroff stops the instance.
func (p *awsProv) runInstances(name, ami, rootDev, subnet, sg, userDataFile string) []string {
	c := p.cfg
	var spot []string
	if c.AWSSpot {
		// persistent + stop: AWS stops a reclaimed instance (disks survive) and
		// restarts it when capacity returns; no max price means up to on-demand.
		spot = []string{"--instance-market-options", "MarketType=spot,SpotOptions={SpotInstanceType=persistent,InstanceInterruptionBehavior=stop}"}
	}
	args := append([]string{"ec2", "run-instances", "--count", "1"}, spot...)
	args = append(args,
		"--image-id", ami, "--instance-type", c.MachineType,
		"--network-interfaces", fmt.Sprintf("DeviceIndex=0,SubnetId=%s,Groups=%s,AssociatePublicIpAddress=true,DeleteOnTermination=true", subnet, sg),
		"--block-device-mappings", fmt.Sprintf("DeviceName=%s,Ebs={VolumeSize=%d,VolumeType=%s,Encrypted=true,DeleteOnTermination=true}", rootDev, c.BootDiskGB, c.DiskType),
		"--metadata-options", "HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled",
		"--instance-initiated-shutdown-behavior", "stop",
		"--tag-specifications", p.a.tagSpec("instance", name), p.a.tagSpec("volume", name+"-root"),
		"--user-data", "file://"+userDataFile,
		"--query", "Instances[0].InstanceId")
	return p.a.text(args...)
}

func (p *awsProv) Create(ctx context.Context, name, pubKey string) error {
	ami, err := p.imageID(ctx)
	if err != nil {
		return err
	}
	rootDev, err := p.a.runText(ctx, p.a.text("ec2", "describe-images", "--image-ids", ami, "--query", "Images[0].RootDeviceName"))
	if err != nil || rootDev == "" {
		return fmt.Errorf("reading the root device of %s: %v", ami, err)
	}
	vpc, err := p.vpcID(ctx)
	if err != nil || vpc == "" {
		return fmt.Errorf("no vpc %s (run create again): %v", p.cfg.Network, err)
	}
	sn, err := p.inVPC(ctx, "describe-subnets", "Subnets[0].SubnetId", p.subnetName(), vpc)
	if err != nil || sn == "" {
		return fmt.Errorf("no subnet %s: %v", p.subnetName(), err)
	}
	sg, err := p.sgID(ctx, vpc)
	if err != nil || sg == "" {
		return fmt.Errorf("no security group %s: %v", p.sgName(), err)
	}
	v, err := p.volume(ctx, name)
	if err != nil || v == nil {
		return fmt.Errorf("no data volume %s: %v", dataDiskName(name), err)
	}
	ud, err := awsUserData(p.cfg, pubKey, v.ID)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "comebye-userdata-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(ud); err != nil {
		return err
	}
	f.Close()
	id, err := p.a.runText(ctx, p.runInstances(name, ami, rootDev, sn, sg, f.Name()))
	if err != nil || id == "" {
		return fmt.Errorf("launching the instance: %v", err)
	}
	return nil
}

func (p *awsProv) Attach(ctx context.Context, name string) error {
	i, err := p.find(ctx, name)
	if err != nil || i == nil {
		return err
	}
	v, err := p.volume(ctx, name)
	if err != nil || v == nil {
		return fmt.Errorf("no data volume %s: %v", dataDiskName(name), err)
	}
	switch v.Instance {
	case i.InstanceID:
		return nil
	case "":
	default:
		return fmt.Errorf("volume %s is attached to %s, not %s", v.ID, v.Instance, i.InstanceID)
	}
	if _, err := p.a.runText(ctx, p.a.text("ec2", "wait", "instance-running", "--instance-ids", i.InstanceID)); err != nil {
		return err
	}
	_, err = p.a.runText(ctx, p.a.text("ec2", "attach-volume", "--volume-id", v.ID, "--instance-id", i.InstanceID, "--device", "/dev/sdf", "--query", "State"))
	return err
}

func (p *awsProv) byName(ctx context.Context, name string) (string, error) {
	i, err := p.find(ctx, name)
	if err != nil {
		return "", err
	}
	if i == nil {
		return "", fmt.Errorf("no instance %s", name)
	}
	return i.InstanceID, nil
}

// spotPoll is how long Start waits between looks at a spot request; tests
// replace it.
var spotPoll = func() { time.Sleep(10 * time.Second) }

const spotWaitTries = 54 // about 9 minutes

// waitSpotStartable waits for a persistent spot request to leave "active".
// After a stop, AWS keeps the request active ("marked-for-stop") for a couple
// of minutes and refuses start-instances (IncorrectSpotRequestState) until it
// is "disabled".
func (p *awsProv) waitSpotStartable(ctx context.Context, reqID string) error {
	for i := 0; i < spotWaitTries; i++ {
		st, err := p.a.runText(ctx, p.a.text("ec2", "describe-spot-instance-requests", "--spot-instance-request-ids", reqID, "--query", "SpotInstanceRequests[0].State"))
		if err != nil {
			return err
		}
		if st != "active" {
			return nil
		}
		spotPoll()
	}
	return nil // try the start anyway; its error says what AWS thinks
}

func (p *awsProv) Start(ctx context.Context, name string) error {
	i, err := p.find(ctx, name)
	if err != nil {
		return err
	}
	if i == nil {
		return fmt.Errorf("no instance %s", name)
	}
	id := i.InstanceID
	if i.SpotRequest != "" {
		if err := p.waitSpotStartable(ctx, i.SpotRequest); err != nil {
			return err
		}
	}
	_, err = p.a.runText(ctx, p.a.text("ec2", "start-instances", "--instance-ids", id, "--query", "StartingInstances[0].InstanceId"))
	if err != nil && p.cfg.AWSSpot {
		return fmt.Errorf("%w\n(spot instance: if AWS has no capacity right now, wait and run start again; it also restarts by itself when capacity returns)", err)
	}
	return err
}

func (p *awsProv) Stop(ctx context.Context, name string) error {
	id, err := p.byName(ctx, name)
	if err != nil {
		return err
	}
	if _, err := p.a.runText(ctx, p.a.text("ec2", "stop-instances", "--instance-ids", id, "--query", "StoppingInstances[0].InstanceId")); err != nil {
		return err
	}
	_, err = p.a.runText(ctx, p.a.text("ec2", "wait", "instance-stopped", "--instance-ids", id))
	return err
}

func (p *awsProv) Delete(ctx context.Context, name string) error {
	id, err := p.byName(ctx, name)
	if err != nil {
		return err
	}
	// A persistent spot request outlives its instance and would launch a
	// replacement, so cancel it first.
	if i, err := p.find(ctx, name); err == nil && i != nil && i.SpotRequest != "" {
		if _, err := p.a.runText(ctx, p.a.text("ec2", "cancel-spot-instance-requests", "--spot-instance-request-ids", i.SpotRequest, "--query", "CancelledSpotInstanceRequests[0].State")); err != nil {
			return fmt.Errorf("cancelling spot request %s: %w", i.SpotRequest, err)
		}
	}
	if _, err := p.a.runText(ctx, p.a.text("ec2", "terminate-instances", "--instance-ids", id, "--query", "TerminatingInstances[0].InstanceId")); err != nil {
		return err
	}
	_, err = p.a.runText(ctx, p.a.text("ec2", "wait", "instance-terminated", "--instance-ids", id))
	return err
}

func (p *awsProv) DeleteDataDisk(ctx context.Context, name string) error {
	v, err := p.volume(ctx, name)
	if err != nil || v == nil {
		return err
	}
	_, err = p.a.runText(ctx, p.a.text("ec2", "delete-volume", "--volume-id", v.ID))
	return err
}

// ---- host key ----

var consoleKeyRe = regexp.MustCompile(`(?m)^(?:\[[^\]]*\]\s*)?COMEBYE-HOSTKEY (ssh-ed25519 [A-Za-z0-9+/]+={0,2})\s*$`)

// parseConsoleKey picks the last marker line the VM printed.
func parseConsoleKey(out string) string {
	m := consoleKeyRe.FindAllStringSubmatch(strings.ReplaceAll(out, "\r", ""), -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// HostKey reads the key the VM printed to its console, through the IAM
// authenticated API. Once an instance id's key has been seen it is pinned in
// the worker's state dir, so a stop/start (the console of a later boot may
// not show it yet) reuses it. A different instance id never inherits a pin.
func (p *awsProv) HostKey(ctx context.Context, name string, inst *Inst) (string, error) {
	pin := filepath.Join(p.cfg.WorkerDir(name), "hostkey."+inst.ID)
	if b, err := os.ReadFile(pin); err == nil {
		if k, err := parseHostKey(string(b)); err == nil {
			return k, nil
		}
	}
	out, _, err := p.a.r.Run(ctx, "aws", p.a.text("ec2", "get-console-output", "--instance-id", inst.ID, "--latest", "--query", "Output"), nil)
	if err != nil {
		return "", nil
	}
	k := parseConsoleKey(out)
	if k == "" {
		return "", nil
	}
	if os.MkdirAll(filepath.Dir(pin), 0o700) == nil {
		os.WriteFile(pin, []byte(k+"\n"), 0o600)
	}
	return k, nil
}

// ---- listing ----

func (p *awsProv) List(ctx context.Context, w logf) error {
	out, err := p.a.runText(ctx, p.a.text("ec2", "describe-instances", "--filters", "Name=tag:"+labelKey+",Values="+labelVal,
		"Name=instance-state-name,Values=pending,running,stopping,stopped",
		"--query", "Reservations[].Instances[].[Tags[?Key=='Name']|[0].Value,State.Name,PublicIpAddress]"))
	if err != nil {
		return err
	}
	w("instances (tag %s=%s):\n", labelKey, labelVal)
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			ip := "-"
			if len(f) > 2 && f[2] != "None" {
				ip = f[2]
			}
			w("  %-24s %-11s %s\n", f[0], f[1], ip)
		}
	}
	out, err = p.a.runText(ctx, p.a.text("ec2", "describe-volumes", "--filters", "Name=tag:"+labelKey+",Values="+labelVal,
		"--query", "Volumes[].[Tags[?Key=='Name']|[0].Value,Size,State]"))
	if err != nil {
		return err
	}
	w("volumes:\n")
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 3 {
			use := "attached"
			if f[2] == "available" {
				use = "unattached (billed, kept)"
			}
			w("  %-24s %s GB  %s\n", f[0], f[1], use)
		}
	}
	return nil
}

// ---- network cleanup ----

// CleanupNetwork deletes the security group, route table, subnet, internet
// gateway and VPC this tool made. Only resources tagged managed-by are looked
// up, and nothing is deleted while any non-terminated instance or network
// interface remains in the VPC.
func (p *awsProv) CleanupNetwork(ctx context.Context, w logf) error {
	a := p.a
	vpc, err := p.vpcID(ctx)
	if err != nil {
		return err
	}
	if vpc == "" {
		return nil
	}
	used, err := a.runText(ctx, a.text("ec2", "describe-instances", "--filters", "Name=vpc-id,Values="+vpc,
		"Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down", "--query", "Reservations[].Instances[].InstanceId"))
	if err != nil {
		return err
	}
	if used != "" {
		w("  network %s kept: instance(s) %s still use it\n", p.cfg.Network, strings.Join(strings.Fields(used), ", "))
		return nil
	}
	eni, err := a.runText(ctx, a.text("ec2", "describe-network-interfaces", "--filters", "Name=vpc-id,Values="+vpc,
		"--query", "NetworkInterfaces[].NetworkInterfaceId"))
	if err != nil {
		return err
	}
	if eni != "" {
		w("  network %s kept: network interface(s) %s still exist\n", p.cfg.Network, strings.Join(strings.Fields(eni), ", "))
		return nil
	}
	del := func(what string, args []string) error {
		if _, err := a.runText(ctx, args); err != nil {
			return fmt.Errorf("deleting %s: %w", what, err)
		}
		w("  deleted %s\n", what)
		return nil
	}
	if sg, err := p.sgID(ctx, vpc); err != nil {
		return err
	} else if sg != "" {
		if err := del("security group "+p.sgName(), a.text("ec2", "delete-security-group", "--group-id", sg)); err != nil {
			return err
		}
	}
	if rt, err := p.inVPC(ctx, "describe-route-tables", "RouteTables[0].RouteTableId", p.rtName(), vpc); err != nil {
		return err
	} else if rt != "" {
		assoc, err := a.runText(ctx, a.text("ec2", "describe-route-tables", "--route-table-ids", rt, "--query", "RouteTables[0].Associations[].RouteTableAssociationId"))
		if err != nil {
			return err
		}
		for _, id := range strings.Fields(assoc) {
			if _, err := a.runText(ctx, a.text("ec2", "disassociate-route-table", "--association-id", id)); err != nil {
				return err
			}
		}
		if err := del("route table "+p.rtName(), a.text("ec2", "delete-route-table", "--route-table-id", rt)); err != nil {
			return err
		}
	}
	if sn, err := p.inVPC(ctx, "describe-subnets", "Subnets[0].SubnetId", p.subnetName(), vpc); err != nil {
		return err
	} else if sn != "" {
		if err := del("subnet "+p.subnetName(), a.text("ec2", "delete-subnet", "--subnet-id", sn)); err != nil {
			return err
		}
	}
	if igw, err := p.igwID(ctx); err != nil {
		return err
	} else if igw != "" {
		if _, err := a.runText(ctx, a.text("ec2", "detach-internet-gateway", "--internet-gateway-id", igw, "--vpc-id", vpc)); err != nil {
			return err
		}
		if err := del("internet gateway", a.text("ec2", "delete-internet-gateway", "--internet-gateway-id", igw)); err != nil {
			return err
		}
	}
	return del("vpc "+p.cfg.Network, a.text("ec2", "delete-vpc", "--vpc-id", vpc))
}

// ---- dry run ----

func (p *awsProv) Plan(name, pub string, w logf) {
	a, c := p.a, p.cfg
	w("   profile %s, region %s, zone %s; credentials are your AWS CLI profile's, never read by this tool\n", orDefault(c.AWSProfile), c.AWSRegion, c.AWSAZ)
	w("   network, each piece created only if missing and tagged %s=%s:\n", labelKey, labelVal)
	for _, args := range [][]string{
		a.text("ec2", "create-vpc", "--cidr-block", c.VPCRange, "--tag-specifications", a.tagSpec("vpc", c.Network), "--query", "Vpc.VpcId"),
		a.text("ec2", "create-internet-gateway", "--tag-specifications", a.tagSpec("internet-gateway", c.Network)),
		a.text("ec2", "create-subnet", "--vpc-id", "<vpc>", "--cidr-block", c.SubnetRange, "--availability-zone", c.AWSAZ),
		a.text("ec2", "create-route-table", "--vpc-id", "<vpc>"),
		a.text("ec2", "create-route", "--route-table-id", "<rt>", "--destination-cidr-block", "0.0.0.0/0", "--gateway-id", "<igw>"),
		a.text("ec2", "create-security-group", "--group-name", p.sgName(), "--vpc-id", "<vpc>"),
		a.text("ec2", "authorize-security-group-ingress", "--group-id", "<sg>", "--protocol", "tcp", "--port", "22", "--cidr", c.SSHSourceCIDR),
	} {
		w("     %s\n", cmdLine("aws", args))
	}
	w("   data volume (encrypted; survives termination; kept by destroy unless --delete-data-disk)\n     %s\n", cmdLine("aws", p.createVolume(name)))
	ami := c.ImageID
	if ami == "" {
		ami = "<read-only: aws ssm get-parameters --names " + ssmDebian + ">"
	}
	w("   instance: no IAM instance profile, IMDSv2 required (hop limit 1), encrypted root, shutdown behaviour stop;\n   user-data is cloud-config with the worker user, YOUR PUBLIC ssh key and the boot script, no secret\n     %s\n",
		cmdLine("aws", p.runInstances(name, ami, "<root device of the AMI>", "<subnet>", "<sg>", "<user-data.yaml>")))
	w("   then: aws ec2 wait instance-running; aws ec2 attach-volume --device /dev/sdf\n")
	w("   host key, printed by the VM to its console (marker COMEBYE-HOSTKEY) and read through the EC2 API:\n     %s\n",
		cmdLine("aws", a.text("ec2", "get-console-output", "--instance-id", "<id>", "--latest", "--query", "Output")))
	if c.AWSSpot {
		w("   spot: persistent request, interruption behaviour stop (AWS stops a reclaimed instance and restarts it when capacity returns; no max price, so up to on-demand)\n")
	}
	if c.MaxRunDuration != "" {
		w("   advisory max run time: a guest-side timer powers the VM off %s after boot (EC2 has no platform cap)\n", c.MaxRunDuration)
	}
}

func orDefault(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}
