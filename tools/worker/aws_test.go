package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testAWSCfg(t *testing.T) *Config {
	t.Helper()
	c := testCfg(t)
	c.Provider = "aws"
	c.Project, c.Zone = "", ""
	c.AWSRegion, c.AWSAZ = "us-east-1", "us-east-1a"
	c.MachineType, c.DiskType = "t3.xlarge", "gp3"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

// awsFake is a small stateful EC2: enough for create, resume and destroy.
type awsFake struct {
	vpc, igw, igwAtt, subnet, rt, route, assoc, sg, vol, volAtt bool
	inst                                                        string // "", running, stopped
	foreign                                                     bool
	otherInVPC, enis                                            string
	consoleTries                                                int // console is empty until this many reads
	reads                                                       int
	console                                                     string
	userData                                                    string
	ops                                                         []string
	spot                                                        bool   // the instance is a persistent spot instance
	reason                                                      string // StateTransitionReason
	startErr                                                    string
	reqActive                                                   int // the spot request reads "active" this many more times
}

func (f *awsFake) did(op string) bool {
	for _, o := range f.ops {
		if o == op {
			return true
		}
	}
	return false
}

func qarg(cl call, flag string) string {
	for i, a := range cl.args {
		if a == flag && i+1 < len(cl.args) {
			return cl.args[i+1]
		}
	}
	return ""
}

func tf(b bool, yes string) string {
	if b {
		return yes
	}
	return "None"
}

func (f *awsFake) handler(w *fakeWorld, c *Config) func(call, *fakeRunner) (string, string, error) {
	return func(cl call, r *fakeRunner) (string, string, error) {
		if cl.name != "aws" {
			return gceFake(w, c)(cl, r)
		}
		svc, op := cl.args[0], cl.args[1]
		f.ops = append(f.ops, op)
		q := qarg(cl, "--query")
		switch op {
		case "get-parameters":
			return "ami-0abc12345678\n", "", nil
		case "describe-images":
			return "/dev/xvda\n", "", nil
		case "describe-vpcs":
			return tf(f.vpc, "vpc-1"), "", nil
		case "create-vpc":
			f.vpc = true
			return "vpc-1", "", nil
		case "describe-internet-gateways":
			if strings.Contains(q, "Attachments") {
				return tf(f.igwAtt, "vpc-1"), "", nil
			}
			return tf(f.igw, "igw-1"), "", nil
		case "create-internet-gateway":
			f.igw = true
			return "igw-1", "", nil
		case "attach-internet-gateway":
			f.igwAtt = true
		case "describe-subnets":
			return tf(f.subnet, "subnet-1"), "", nil
		case "create-subnet":
			f.subnet = true
			return "subnet-1", "", nil
		case "describe-route-tables":
			switch {
			case strings.Contains(q, "GatewayId"):
				return tf(f.route, "igw-1"), "", nil
			case strings.Contains(q, "Associations[?"):
				return tf(f.assoc, "rtbassoc-1"), "", nil
			case strings.Contains(q, "Associations[]"):
				return "rtbassoc-1", "", nil
			}
			return tf(f.rt, "rtb-1"), "", nil
		case "create-route-table":
			f.rt = true
			return "rtb-1", "", nil
		case "create-route":
			f.route = true
		case "associate-route-table":
			f.assoc = true
		case "describe-security-groups":
			return tf(f.sg, "sg-1"), "", nil
		case "create-security-group":
			f.sg = true
			return "sg-1", "", nil
		case "describe-volumes":
			if !strings.Contains(q, "Volumes[0]") {
				return "", "", nil
			}
			if !f.vol {
				return "None", "", nil
			}
			return "vol-1\t" + tf(f.volAtt, "i-1") + "\tus-east-1a", "", nil
		case "create-volume":
			f.vol = true
			return "vol-1", "", nil
		case "attach-volume":
			f.volAtt = true
		case "delete-volume":
			f.vol = false
		case "describe-instances":
			if strings.Contains(cl.line(), "vpc-id") {
				return f.otherInVPC, "", nil
			}
			if strings.Contains(q, "Reservations[]") {
				return "", "", nil
			}
			if f.inst == "" {
				return `{"Reservations":[]}`, "", nil
			}
			tag := `{"Key":"managed-by","Value":"comebye-worker"},`
			if f.foreign {
				tag = ""
			}
			spot := ""
			if f.spot {
				spot = `"InstanceLifecycle":"spot","SpotInstanceRequestId":"sir-1",`
			}
			return fmt.Sprintf(`{"Reservations":[{"Instances":[{"InstanceId":"i-1","InstanceType":"t3.xlarge","PublicIpAddress":"198.51.100.9",%s"StateTransitionReason":%q,"State":{"Name":%q},"Tags":[%s{"Key":"Name","Value":"w1"}]}]}]}`, spot, f.reason, f.inst, tag), "", nil
		case "run-instances":
			f.inst = "running"
			if p, ok := strings.CutPrefix(qarg(cl, "--user-data"), "file://"); ok {
				b, _ := os.ReadFile(p)
				f.userData = string(b)
			}
			return "i-1", "", nil
		case "describe-spot-instance-requests":
			if f.reqActive > 0 {
				f.reqActive--
				return "active", "", nil
			}
			return "disabled", "", nil
		case "start-instances":
			if f.reqActive > 0 {
				return "", "IncorrectSpotRequestState", errors.New("IncorrectSpotRequestState")
			}
			if f.startErr != "" {
				return "", f.startErr, errors.New(f.startErr)
			}
			f.inst = "running"
		case "stop-instances":
			f.inst = "stopped"
		case "terminate-instances":
			f.inst = ""
		case "get-console-output":
			f.reads++
			if f.reads <= f.consoleTries {
				return "None", "", nil
			}
			return f.console, "", nil
		case "describe-network-interfaces":
			return f.enis, "", nil
		case "delete-security-group":
			f.sg = false
		case "delete-vpc":
			f.vpc = false
		}
		_ = svc
		return "", "", nil
	}
}

func awsEnv(t *testing.T, f *awsFake, w *fakeWorld) (*env, *fakeRunner, *Config) {
	c := testAWSCfg(t)
	writeOAuth(t, c)
	r := &fakeRunner{state: map[string]bool{}}
	r.handle = f.handler(w, c)
	e, _, _ := newEnv(t, c, w, r)
	return e, r, c
}

const consoleKey = "[   12.3] cloud-init: booted\nCOMEBYE-HOSTKEY ssh-ed25519 AAAACONSOLEKEY\n"

// ---- config ----

func TestAWSConfig(t *testing.T) {
	c := testAWSCfg(t)
	if c.MachineType != "t3.xlarge" || c.DiskType != "gp3" {
		t.Errorf("aws defaults: %s %s", c.MachineType, c.DiskType)
	}
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte("provider=aws\naws_region=eu-west-1\naws_az=eu-west-1b\naws_profile=work\nssh_source_cidr=198.51.100.1/32\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=http://h.t.ts.net:8742\n"), 0o600)
	lc, err := LoadConfig(p)
	if err != nil || lc.MachineType != "t3.xlarge" || lc.DiskType != "gp3" || lc.AWSProfile != "work" {
		t.Fatalf("load: %+v %v", lc, err)
	}
	os.WriteFile(p, []byte("provider=aws\nmachine_type=m6g.large\naws_region=eu-west-1\naws_az=eu-west-1b\nssh_source_cidr=198.51.100.1/32\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=http://h.t.ts.net:8742\n"), 0o600)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "Graviton") {
		t.Errorf("Arm type accepted: %v", err)
	}
	bad := map[string]func(*Config){
		"unknown provider": func(c *Config) { c.Provider = "azure" },
		"region":           func(c *Config) { c.AWSRegion = "us_east" },
		"az not in region": func(c *Config) { c.AWSAZ = "us-west-2a" },
		"profile":          func(c *Config) { c.AWSProfile = "a b;c" },
		"ami":              func(c *Config) { c.ImageID = "debian" },
		"disk type":        func(c *Config) { c.DiskType = "pd-balanced" },
		"vpc range":        func(c *Config) { c.VPCRange = "192.168.0.0/16" },
		"graviton":         func(c *Config) { c.MachineType = "c7g.xlarge" },
	}
	for name, mut := range bad {
		cc := *c
		mut(&cc)
		if cc.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, ok := range []string{"t3.xlarge", "t3a.large", "m5.xlarge", "g5.xlarge", "c6in.large"} {
		cc := *c
		cc.MachineType = ok
		if err := cc.Validate(); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	// A GCP config does not need the AWS keys, and the default provider is gcp.
	if g := testCfg(t); g.Provider != "gcp" {
		t.Errorf("default provider %q", g.Provider)
	}
}

func TestGuestMaxRun(t *testing.T) {
	a := testAWSCfg(t)
	if guestMaxRun(a) != "86400" {
		t.Errorf("24h -> %s", guestMaxRun(a))
	}
	a.MaxRunDuration = "90m"
	if guestMaxRun(a) != "5400" {
		t.Errorf("90m -> %s", guestMaxRun(a))
	}
	a.MaxRunDuration = ""
	if guestMaxRun(a) != "" {
		t.Error("empty must disable")
	}
	if g := testCfg(t); guestMaxRun(g) != "" {
		t.Error("gcp uses the platform cap, not the guest timer")
	}
}

func TestParseConsoleKey(t *testing.T) {
	cases := map[string]string{
		consoleKey: "ssh-ed25519 AAAACONSOLEKEY",
		"COMEBYE-HOSTKEY ssh-ed25519 AAAAOLD\r\nnoise\r\n[  9.9] COMEBYE-HOSTKEY ssh-ed25519 AAAANEW\r\n": "ssh-ed25519 AAAANEW",
		"-----BEGIN SSH HOST KEY KEYS-----\nssh-ed25519 AAAACLOUDINIT root@h\n":                           "", // only our marker counts
		"echo COMEBYE-HOSTKEY ssh-ed25519 AAAA":                                                           "", // not at line start
		"COMEBYE-HOSTKEY ssh-rsa AAAA":                                                                    "",
		"":                                                                                                "",
	}
	for in, want := range cases {
		if got := parseConsoleKey(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// ---- create ----

func TestAWSCreateEndToEnd(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey, consoleTries: 2}
	e, r, c := awsEnv(t, f, w)
	var out strings.Builder
	e.Out = &out
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}

	// network pieces, in a sane order, and the volume exist
	for _, op := range []string{"create-vpc", "create-internet-gateway", "attach-internet-gateway", "create-subnet", "create-route-table", "create-route", "associate-route-table", "create-security-group", "authorize-security-group-ingress", "create-volume"} {
		if !f.did(op) {
			t.Errorf("missing %s", op)
		}
	}
	// the one ingress rule is tcp:22 from the configured /32
	var ingress, run, vol string
	for _, cl := range r.calls {
		switch {
		case strings.Contains(cl.line(), "authorize-security-group-ingress"):
			ingress = cl.line()
		case strings.Contains(cl.line(), "run-instances"):
			run = cl.line()
		case strings.Contains(cl.line(), "create-volume"):
			vol = cl.line()
		}
	}
	if !strings.Contains(ingress, "--port 22") || !strings.Contains(ingress, "--cidr 203.0.113.7/32") {
		t.Errorf("ingress: %s", ingress)
	}
	if !strings.Contains(vol, "--encrypted") || !strings.Contains(vol, "managed-by") {
		t.Errorf("volume: %s", vol)
	}
	// instance hardening
	for _, want := range []string{"HttpTokens=required", "HttpPutResponseHopLimit=1", "Encrypted=true", "--instance-initiated-shutdown-behavior stop",
		"Key=managed-by,Value=comebye-worker", "Key=Name,Value=w1", "AssociatePublicIpAddress=true", "--instance-type t3.xlarge", "--image-id ami-0abc12345678", "DeviceName=/dev/xvda"} {
		if !strings.Contains(run, want) {
			t.Errorf("run-instances lacks %q:\n%s", want, run)
		}
	}
	for _, bad := range []string{"iam-instance-profile", "--key-name", "--user-data-secret"} {
		if strings.Contains(run, bad) {
			t.Errorf("run-instances has %q", bad)
		}
	}
	// the volume is attached after the instance runs; the data volume is not in the launch mapping
	if !f.did("attach-volume") || strings.Contains(run, "/dev/sdf") {
		t.Error("data volume must be a separate volume attached after launch")
	}
	// user-data: public material only
	if !strings.HasPrefix(f.userData, "#cloud-config") || !strings.Contains(f.userData, "ssh-ed25519 AAAAPUBKEY") {
		t.Errorf("user-data:\n%s", f.userData)
	}
	decoded := f.userData
	for _, m := range regexp.MustCompile(`content: (\S+)`).FindAllStringSubmatch(f.userData, -1) {
		if b, err := base64.StdEncoding.DecodeString(m[1]); err == nil {
			decoded += "\n" + string(b)
		}
	}
	for _, bad := range []string{secretKey, "tskey-", "auth-key", "client_secret", "csecret"} {
		if strings.Contains(decoded, bad) {
			t.Errorf("user-data contains %q", bad)
		}
	}
	if !strings.Contains(decoded, "vol-1") || !strings.Contains(decoded, "COMEBYE-HOSTKEY") || !strings.Contains(decoded, "NOPASSWD") {
		t.Error("user-data lacks the volume id / boot script / user")
	}
	// host key came from the console (after it was empty twice) and is strict
	kh, _ := os.ReadFile(filepath.Join(c.WorkerDir("w1"), "known_hosts"))
	if string(kh) != "198.51.100.9 ssh-ed25519 AAAACONSOLEKEY\n" {
		t.Errorf("known_hosts %q", kh)
	}
	if f.reads != 3 {
		t.Errorf("console reads %d", f.reads)
	}
	// secrets: only stdin of tailscale up; nothing AWS-side sees the key
	for _, cl := range r.calls {
		if cl.name == "aws" && (strings.Contains(cl.line(), secretKey) || strings.Contains(cl.stdin, secretKey)) {
			t.Error("tailscale key reached aws")
		}
		if strings.Contains(cl.line(), secretKey) {
			t.Error("secret in argv")
		}
	}
	// the guest max-run timer is configured on aws
	var rootS string
	for _, cl := range r.calls {
		if cl.name == "ssh" && cl.args[len(cl.args)-1] == "sudo bash -s" {
			rootS = cl.stdin
		}
	}
	if !strings.Contains(rootS, "WK_MAX_RUN_SECONDS='86400'") {
		t.Errorf("root script lacks the guest timer var")
	}
	if !strings.Contains(out.String(), "hub: w1 is ok") {
		t.Errorf("no join:\n%s", out.String())
	}
	// no credentials anywhere in argv
	for _, cl := range r.calls {
		if cl.name == "aws" && regexp.MustCompile(`AKIA|aws_secret|AWS_SECRET`).MatchString(cl.line()) {
			t.Error("credentials on argv")
		}
	}
}

func TestAWSProfileIsOnlyPassedByName(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, r, c := awsEnv(t, f, w)
	c.AWSProfile = "work"
	if _, err := e.prov().Describe(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.calls[0].line(), "--profile work") || !strings.Contains(r.calls[0].line(), "--region us-east-1") {
		t.Errorf("call: %s", r.calls[0].line())
	}
}

func TestAWSCreateResumes(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey, vpc: true, igw: true, igwAtt: true, subnet: true, rt: true, route: true, assoc: true, sg: true, vol: true, volAtt: true, inst: "running"}
	e, r, _ := awsEnv(t, f, w)
	r.state["tsup"] = true
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"create-vpc", "create-subnet", "create-security-group", "create-volume", "run-instances", "attach-volume", "authorize-security-group-ingress"} {
		if f.did(op) {
			t.Errorf("resume redid %s", op)
		}
	}
	if len(w.mintBodies) != 0 {
		t.Error("minted a key for an enrolled node")
	}
}

func TestAWSStoppedInstanceIsStartedOnCreate(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey, vpc: true, igw: true, igwAtt: true, subnet: true, rt: true, route: true, assoc: true, sg: true, vol: true, inst: "stopped"}
	e, _, _ := awsEnv(t, f, w)
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if !f.did("start-instances") || !f.did("attach-volume") || f.did("run-instances") {
		t.Errorf("ops %v", f.ops)
	}
}

func TestAWSRefusesForeignInstance(t *testing.T) {
	w := &fakeWorld{peers: map[string]bool{}}
	f := &awsFake{inst: "running", foreign: true}
	e, _, _ := awsEnv(t, f, w)
	e.In = strings.NewReader("w1\n")
	for name, fn := range map[string]func() error{
		"stop":    func() error { return e.stop(context.Background(), "w1") },
		"start":   func() error { return e.start(context.Background(), "w1") },
		"destroy": func() error { return e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}) },
		"login":   func() error { return e.login(context.Background(), "w1", "claude") },
		"create":  func() error { w.hubVersion = "v0.3.1"; return e.create(context.Background(), "w1", createOpts{}) },
	} {
		if err := fn(); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, op := range []string{"stop-instances", "start-instances", "terminate-instances", "run-instances"} {
		if f.did(op) {
			t.Errorf("ran %s", op)
		}
	}
}

func TestAWSHostKeyPinIsPerInstance(t *testing.T) {
	w := &fakeWorld{peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, _, _ := awsEnv(t, f, w)
	p := e.prov()
	ctx := context.Background()
	k, _ := p.HostKey(ctx, "w1", &Inst{ID: "i-1"})
	if k != "ssh-ed25519 AAAACONSOLEKEY" || f.reads != 1 {
		t.Fatalf("first read: %q reads=%d", k, f.reads)
	}
	f.console = "" // a later boot's console does not show it
	k, _ = p.HostKey(ctx, "w1", &Inst{ID: "i-1"})
	if k != "ssh-ed25519 AAAACONSOLEKEY" || f.reads != 1 {
		t.Errorf("pin not used: %q reads=%d", k, f.reads)
	}
	k, _ = p.HostKey(ctx, "w1", &Inst{ID: "i-2"})
	if k != "" {
		t.Errorf("a different instance inherited the pin: %q", k)
	}
}

func TestAWSDryRunTouchesNothing(t *testing.T) {
	c := testAWSCfg(t)
	r := &fakeRunner{handle: func(call, *fakeRunner) (string, string, error) {
		t.Error("dry run executed a command")
		return "", "", nil
	}}
	e, out, _ := newEnv(t, c, &fakeWorld{}, r)
	e.DryRun = true
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"DRY RUN", "provider    aws", "aws ec2 run-instances", "HttpTokens=required", "--cidr 203.0.113.7/32", "no secret", "advisory max run time", "get-console-output"} {
		if !strings.Contains(s, want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	if strings.Contains(s, "iam-instance-profile") {
		t.Error("plan mentions an instance profile flag")
	}
}

// ---- destroy and network cleanup ----

func awsDestroyEnv(t *testing.T, f *awsFake, input string) (*env, *fakeRunner, *strings.Builder) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{"w1": true},
		devices: []tsDevice{{ID: "d1", Hostname: "w1", Name: "w1.tail1.ts.net", Tags: []string{"tag:worker"}, ConnectedToControl: true}}}
	e, r, _ := awsEnv(t, f, w)
	e.In = strings.NewReader(input)
	var out strings.Builder
	e.Out = &out
	return e, r, &out
}

func fullAWS() *awsFake {
	return &awsFake{console: consoleKey, vpc: true, igw: true, igwAtt: true, subnet: true, rt: true, route: true, assoc: true, sg: true, vol: true, volAtt: true, inst: "running"}
}

func TestAWSDestroyKeepsVolumeAndCleansUnusedNetwork(t *testing.T) {
	f := fullAWS()
	e, r, out := awsDestroyEnv(t, f, "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !f.did("terminate-instances") || f.did("delete-volume") {
		t.Errorf("ops %v", f.ops)
	}
	order := []string{"delete-security-group", "disassociate-route-table", "delete-route-table", "delete-subnet", "detach-internet-gateway", "delete-internet-gateway", "delete-vpc"}
	last := -1
	for _, op := range order {
		at := -1
		for i, o := range f.ops {
			if o == op {
				at = i
			}
		}
		if at < 0 || at < last {
			t.Errorf("%s missing or out of order in %v", op, f.ops)
		}
		last = at
	}
	if !r.saw("tailscale logout") {
		t.Error("no tailscale logout")
	}
}

func TestAWSDestroyDeletesVolumeOnlyWhenAsked(t *testing.T) {
	f := fullAWS()
	e, _, out := awsDestroyEnv(t, f, "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{DeleteDataDisk: true, NoArchive: true}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !f.did("delete-volume") {
		t.Error("volume kept despite --delete-data-disk")
	}
}

func TestAWSNetworkCleanupRefusals(t *testing.T) {
	for name, set := range map[string]func(*awsFake){
		"another instance in the VPC": func(f *awsFake) { f.otherInVPC = "i-other" },
		"a network interface left":    func(f *awsFake) { f.enis = "eni-9" },
	} {
		f := fullAWS()
		set(f)
		e, _, out := awsDestroyEnv(t, f, "w1\n")
		if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		for _, op := range []string{"delete-security-group", "delete-route-table", "delete-subnet", "delete-internet-gateway", "delete-vpc"} {
			if f.did(op) {
				t.Errorf("%s: ran %s", name, op)
			}
		}
		if !strings.Contains(out.String(), "kept") {
			t.Errorf("%s: no note printed:\n%s", name, out)
		}
	}
}

func TestAWSNetworkCleanupSkipsUntaggedVPC(t *testing.T) {
	f := fullAWS()
	f.vpc = false // no vpc carries managed-by + our name: nothing to clean
	e, _, _ := awsDestroyEnv(t, f, "w1\n")
	if err := e.prov().CleanupNetwork(context.Background(), e.printf); err != nil {
		t.Fatal(err)
	}
	for _, o := range f.ops {
		if strings.HasPrefix(o, "delete-") {
			t.Errorf("deleted something: %s", o)
		}
	}
}

// ---- GCE network cleanup (parity) ----

// gceNet drives CleanupNetwork: desc is each resource's description, absent
// means "not found", users are networks instances sit on.
func gceNetRunner(desc map[string]string, instNetworks []string) (*fakeRunner, *[]string) {
	var deleted []string
	r := &fakeRunner{}
	r.handle = func(cl call, _ *fakeRunner) (string, string, error) {
		line := cl.line()
		switch {
		case strings.Contains(line, "instances list"):
			var items []string
			for _, n := range instNetworks {
				items = append(items, fmt.Sprintf(`{"name":"other","networkInterfaces":[{"network":"https://www.googleapis.com/compute/v1/projects/p/global/networks/%s"}]}`, n))
			}
			return "[" + strings.Join(items, ",") + "]", "", nil
		case strings.Contains(line, " delete "):
			deleted = append(deleted, strings.Fields(line)[3]+" "+strings.Fields(line)[len(strings.Fields(line))-0-1+0])
			return "", "", nil
		case strings.Contains(line, "describe"):
			for key, d := range desc {
				if strings.Contains(line, key) {
					return d + "\n", "", nil
				}
			}
			return "", "ERROR: was not found", fmt.Errorf("exit 1")
		}
		return "", "", nil
	}
	return r, &deleted
}

func TestGCENetworkCleanupDeletesManagedUnusedNetwork(t *testing.T) {
	c := testCfg(t)
	r, _ := gceNetRunner(map[string]string{"firewall-rules": managedDesc, "subnets": managedDesc, "networks describe": managedDesc}, nil)
	e, out, _ := newEnv(t, c, &fakeWorld{}, r)
	if err := e.prov().CleanupNetwork(context.Background(), e.printf); err != nil {
		t.Fatal(err)
	}
	var dels []string
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), " delete ") {
			dels = append(dels, cl.args[1]+"/"+cl.args[2])
		}
	}
	if strings.Join(dels, ",") != "firewall-rules/delete,networks/subnets,networks/delete" {
		t.Errorf("deletes in order: %v\n%s", dels, out)
	}
}

func TestGCENetworkCleanupRefusals(t *testing.T) {
	c := testCfg(t)
	all := map[string]string{"firewall-rules": managedDesc, "subnets": managedDesc, "networks describe": managedDesc}
	cases := map[string]struct {
		desc  map[string]string
		insts []string
		note  string
	}{
		"another instance uses the network": {all, []string{c.Network}, "still uses it"},
		"network not made by this tool":     {map[string]string{"firewall-rules": managedDesc, "subnets": managedDesc, "networks describe": "my prod network"}, nil, "not made by this tool"},
		"firewall without the marker":       {map[string]string{"firewall-rules": "", "subnets": managedDesc, "networks describe": managedDesc}, nil, "not made by this tool"},
	}
	for name, tc := range cases {
		r, _ := gceNetRunner(tc.desc, tc.insts)
		e, out, _ := newEnv(t, c, &fakeWorld{}, r)
		if err := e.prov().CleanupNetwork(context.Background(), e.printf); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, cl := range r.calls {
			if strings.Contains(cl.line(), " delete ") {
				t.Errorf("%s: deleted %s", name, cl.line())
			}
		}
		if !strings.Contains(out.String(), tc.note) {
			t.Errorf("%s: note %q missing:\n%s", name, tc.note, out)
		}
	}
	// an instance on some other network does not block
	r, _ := gceNetRunner(all, []string{"default"})
	e, _, _ := newEnv(t, c, &fakeWorld{}, r)
	e.prov().CleanupNetwork(context.Background(), e.printf)
	if !r.saw("networks delete") {
		t.Error("an instance on another network blocked the cleanup")
	}
}

func TestGCEDestroyRunsNetworkCleanupAfterTheInstance(t *testing.T) {
	e, r, _, _ := destroyEnv(t, "RUNNING", "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
		t.Fatal(err)
	}
	del, clean := -1, -1
	for i, cl := range r.calls {
		if strings.Contains(cl.line(), "instances delete") {
			del = i
		}
		if strings.Contains(cl.line(), "instances list") {
			clean = i
		}
	}
	if del < 0 || clean < del {
		t.Errorf("instance delete at %d, network cleanup at %d", del, clean)
	}
}

// ---- scripts ----

func TestAWSScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	c := testAWSCfg(t)
	ud, err := awsUserData(c, "ssh-ed25519 AAAAPUBKEY comebye-worker", "vol-0123456789abcdef0")
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := rootScript(c)
	for name, s := range map[string]string{"boot": awsBootScript, "root(aws)": rs} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	if len(ud) > 16*1024 {
		t.Errorf("user-data is %d bytes (limit 16 KB)", len(ud))
	}
	for _, bad := range []string{"tskey-", "auth-key", "client_secret"} {
		if strings.Contains(awsBootScript, bad) {
			t.Errorf("boot script mentions %q", bad)
		}
	}
	if _, err := awsUserData(c, "ssh-rsa AAAA x", "vol-1"); err == nil {
		t.Error("accepted a non-ed25519 key")
	}
	if _, err := awsUserData(c, "ssh-ed25519 AAAA x", "vol-1; reboot"); err == nil {
		t.Error("accepted a bad volume id")
	}
}

// ---- spot ----

func runInstancesLine(r *fakeRunner) string {
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "run-instances") {
			return cl.line()
		}
	}
	return ""
}

func TestAWSSpotIsOptIn(t *testing.T) {
	for _, spot := range []bool{false, true} {
		w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
		f := &awsFake{console: consoleKey}
		e, r, c := awsEnv(t, f, w)
		c.AWSSpot = spot
		if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
			t.Fatal(err)
		}
		run := runInstancesLine(r)
		has := strings.Contains(run, "--instance-market-options MarketType=spot,SpotOptions={SpotInstanceType=persistent,InstanceInterruptionBehavior=stop}")
		if has != spot {
			t.Errorf("spot=%v: market options present=%v:\n%s", spot, has, run)
		}
		if strings.Contains(run, "MaxPrice") {
			t.Error("a max price was set")
		}
		for _, want := range []string{"HttpTokens=required", "--instance-initiated-shutdown-behavior stop", "Encrypted=true"} {
			if !strings.Contains(run, want) {
				t.Errorf("spot=%v: hardening flag %q lost", spot, want)
			}
		}
	}
}

func TestAWSSpotDestroyCancelsRequestBeforeTerminating(t *testing.T) {
	f := fullAWS()
	f.spot = true
	e, r, out := awsDestroyEnv(t, f, "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cancel, term := -1, -1
	for i, o := range f.ops {
		switch o {
		case "cancel-spot-instance-requests":
			cancel = i
		case "terminate-instances":
			term = i
		}
	}
	if cancel < 0 || term < 0 || cancel > term {
		t.Errorf("cancel must precede terminate: %v", f.ops)
	}
	found := false
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "cancel-spot-instance-requests") && strings.Contains(cl.line(), "--spot-instance-request-ids sir-1") {
			found = true
		}
	}
	if !found {
		t.Error("cancel did not name the instance's own request")
	}
}

func TestAWSOnDemandDestroyDoesNotCancel(t *testing.T) {
	f := fullAWS()
	e, _, out := awsDestroyEnv(t, f, "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.did("cancel-spot-instance-requests") {
		t.Error("cancelled a spot request for an on-demand instance")
	}
}

func TestAWSSpotStartCapacityHintAndStatusNote(t *testing.T) {
	w := &fakeWorld{peers: map[string]bool{}}
	f := &awsFake{inst: "stopped", spot: true, reason: "Server.SpotInstanceTermination: Spot capacity reclaimed", startErr: "InsufficientInstanceCapacity"}
	e, _, c := awsEnv(t, f, w)
	c.AWSSpot = true
	if err := e.start(context.Background(), "w1"); err == nil || !strings.Contains(err.Error(), "InsufficientInstanceCapacity") || !strings.Contains(err.Error(), "spot instance") {
		t.Errorf("start error: %v", err)
	}
	var out strings.Builder
	e.Out = &out
	if err := e.status(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "stopped by AWS") {
		t.Errorf("status does not say AWS stopped it:\n%s", out.String())
	}
}

func TestAWSSpotConfig(t *testing.T) {
	base := "provider=aws\naws_region=us-east-1\naws_az=us-east-1a\nssh_source_cidr=198.51.100.1/32\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=http://h.t.ts.net:8742\n"
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte(base+"aws_spot=true\n"), 0o600)
	if c, err := LoadConfig(p); err != nil || !c.AWSSpot {
		t.Fatalf("aws_spot=true: %v", err)
	}
	os.WriteFile(p, []byte(base), 0o600)
	if c, err := LoadConfig(p); err != nil || c.AWSSpot {
		t.Fatalf("default must be on-demand: %v", err)
	}
	os.WriteFile(p, []byte(base+"aws_spot=yes\n"), 0o600)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "aws_spot") {
		t.Errorf("aws_spot=yes accepted: %v", err)
	}
	c := testCfg(t)
	c.AWSSpot = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "aws_spot needs provider=aws") {
		t.Errorf("spot on gcp: %v", err)
	}
}

func TestAWSSpotStartWaitsForTheRequestToBeDisabled(t *testing.T) {
	polls := 0
	old := spotPoll
	spotPoll = func() { polls++ }
	defer func() { spotPoll = old }()
	w := &fakeWorld{peers: map[string]bool{}}
	f := &awsFake{inst: "stopped", spot: true, reqActive: 3, console: consoleKey}
	e, _, c := awsEnv(t, f, w)
	c.AWSSpot = true
	if err := e.prov().Start(context.Background(), "w1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if polls != 3 || !f.did("start-instances") || f.inst != "running" {
		t.Errorf("polls=%d inst=%s ops=%v", polls, f.inst, f.ops)
	}
	// an on-demand instance is never polled
	polls = 0
	g := &awsFake{inst: "stopped", console: consoleKey}
	e2, _, _ := awsEnv(t, g, &fakeWorld{peers: map[string]bool{}})
	if err := e2.prov().Start(context.Background(), "w1"); err != nil || polls != 0 || g.did("describe-spot-instance-requests") {
		t.Errorf("on-demand start: err=%v polls=%d ops=%v", err, polls, g.ops)
	}
}

// ---- ssh timeout hint ----

func TestSSHTimeoutNamesTheSourceCIDR(t *testing.T) {
	w := &fakeWorld{peers: map[string]bool{}}
	f := &awsFake{inst: "running", console: consoleKey}
	e, r, _ := awsEnv(t, f, w)
	inner := r.handle
	r.handle = func(cl call, fr *fakeRunner) (string, string, error) {
		if cl.name == "ssh" {
			return "", "Connection timed out", errors.New("exit status 255")
		}
		return inner(cl, fr)
	}
	err := e.connect(context.Background(), "w1")
	if err == nil {
		t.Fatal("connect succeeded with ssh down")
	}
	for _, want := range []string{"timed out waiting for ssh", "203.0.113.7/32", "ssh_source_cidr", "comebye-worker-ssh", "checkip", "(it resumes)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	// the host-key wait is a different failure and gets no firewall advice
	g := &awsFake{inst: "running", console: "None"}
	e2, _, _ := awsEnv(t, g, &fakeWorld{peers: map[string]bool{}})
	if err := e2.connect(context.Background(), "w1"); err == nil || strings.Contains(err.Error(), "ssh_source_cidr") {
		t.Errorf("host-key timeout: %v", err)
	}
}

func TestSSHTimeoutHintGCE(t *testing.T) {
	c := testCfg(t)
	h := sshTimeoutHint(c)
	if !strings.Contains(h, "203.0.113.7/32") || !strings.Contains(h, "gcloud compute firewall-rules update "+c.Network+"-ssh --source-ranges=<new-ip>/32") {
		t.Errorf("gce hint:\n%s", h)
	}
}
