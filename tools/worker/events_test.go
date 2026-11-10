package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- spec ----

func TestSpecRefusesUnknownFields(t *testing.T) {
	for name, in := range map[string]string{
		"unknown field":   `{"kind":"aws","colour":"red"}`,
		"unknown nested":  `{"kind":"aws","name":"w1","ssh_source_cidr":"0.0.0.0/0"}`,
		"two objects":     `{"kind":"aws"}{"kind":"gcp"}`,
		"trailing text":   `{"kind":"aws"} x`,
		"not an object":   `["aws"]`,
		"wrong type":      `{"spot":"yes"}`,
		"empty input":     ``,
		"unknown profile": `{"aws_profile":"admin"}`,
	} {
		if _, err := ParseSpec([]byte(in)); err == nil {
			t.Errorf("%s: accepted %s", name, in)
		}
	}
	if _, err := ParseSpec([]byte(`{"kind":"aws","name":"w1","type":"t3.large","spot":true,"zone":"us-east-1b","boot_disk_gb":20,"data_disk_gb":50,"idle_minutes":120,"max_run":"24h","agents":["pi"],"llm_key":false,"budget_cap_usd":10,"destroy_after_hours":72}`)); err != nil {
		t.Errorf("full spec: %v", err)
	}
}

func TestSpecRefusesWhatTheConfigWould(t *testing.T) {
	for name, in := range map[string]string{
		"bad name":            `{"name":"W 1"}`,
		"graviton":            `{"kind":"aws","type":"t4g.large"}`,
		"type with a space":   `{"kind":"aws","type":"t3.large --foo"}`,
		"zone outside region": `{"kind":"aws","zone":"eu-west-1a"}`,
		"spot on gcp":         `{"kind":"gcp","spot":true}`,
		"tiny disk":           `{"data_disk_gb":1}`,
		"idle too short":      `{"idle_minutes":1}`,
		"bad max run":         `{"max_run":"1 day"}`,
		"unknown agent":       `{"agents":["rm"]}`,
		"no agents":           `{"agents":[]}`,
		"negative cap":        `{"budget_cap_usd":-1}`,
		"huge destroy":        `{"destroy_after_hours":100000}`,
		"llm on, none set":    `{"llm_key":true}`,
		"kind unknown":        `{"kind":"azure"}`,
	} {
		c := testAWSCfg(t)
		c.baseAZ = c.AWSAZ
		s, err := ParseSpec([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "spot on gcp" || name == "kind unknown" {
			c.Project, c.Zone = "comebye-test-0001", "us-central1-a"
		}
		if err := s.Apply(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSpecOpenShellRefusesMachineFields(t *testing.T) {
	c := openshellCfg(t)
	for _, in := range []string{`{"type":"x"}`, `{"zone":"us-east-1a"}`, `{"data_disk_gb":50}`, `{"spot":false}`, `{"agents":["pi"]}`, `{"max_run":"1h"}`} {
		s, _ := ParseSpec([]byte(in))
		if err := s.Apply(c); err == nil || !strings.Contains(err.Error(), "openshell") {
			t.Errorf("%s: %v", in, err)
		}
	}
	s, _ := ParseSpec([]byte(`{"idle_minutes":30,"budget_cap_usd":5}`))
	if err := s.Apply(c); err != nil || c.IdleMinutes != 30 {
		t.Errorf("%v %d", err, c.IdleMinutes)
	}
}

func TestSpecOverlaysOnlyWhatItNames(t *testing.T) {
	c := testAWSCfg(t)
	c.baseAZ = c.AWSAZ
	before := *c
	s, _ := ParseSpec([]byte(`{"type":"m5.large","zone":"us-east-1c","spot":true,"budget_cap_usd":12.5,"destroy_after_hours":72}`))
	if err := s.Apply(c); err != nil {
		t.Fatal(err)
	}
	if c.MachineType != "m5.large" || c.AWSAZ != "us-east-1c" || !c.AWSSpot || c.BudgetCapUSD != 12.5 || c.DestroyAfterHours != 72 {
		t.Errorf("not applied: %+v", c)
	}
	if c.DataDiskGB != before.DataDiskGB || c.IdleMinutes != before.IdleMinutes || c.SSHSourceCIDR != before.SSHSourceCIDR || c.HubURL != before.HubURL || c.AWSProfile != before.AWSProfile {
		t.Errorf("touched what the spec did not name")
	}
	if c.baseAZ != "us-east-1a" {
		t.Errorf("baseAZ moved to %s", c.baseAZ)
	}
}

// ---- helpers ----

func openshellCfg(t *testing.T) *Config {
	t.Helper()
	c := testCfg(t)
	c.Provider = "openshell"
	c.Project, c.Zone, c.SSHSourceCIDR = "", "", ""
	c.OpenShellGateway, c.OpenShellWorkspace = "gw", "ws"
	c.SandboxImage, c.SandboxPolicy = "registry.example/sbx:{version}", "/tmp/policy.yaml"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

// writeCfgFile writes a config file that holds the settings of all three kinds.
func writeCfgFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	body := fmt.Sprintf(`provider=aws
aws_region=us-east-1
aws_az=us-east-1a
project=comebye-test-0001
zone=us-central1-a
ssh_source_cidr=203.0.113.7/32
ts_allow_login=you@example.com
hub_url=https://hub.tail1.ts.net
hub_peer_url=https://hub.tail1.ts.net
state_dir=%[1]s/state
ts_oauth_file=%[1]s/oauth
ts_authkey_file=%[1]s/authkey
archive_env=%[1]s/archive.env
openshell_gateway=gw
openshell_workspace=ws
sandbox_image=registry.example/sbx:{version}
sandbox_policy=%[1]s/policy.yaml
`, dir)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type noRunner struct{ t *testing.T }

func (n noRunner) Run(_ context.Context, name string, args []string, _ []byte) (string, string, error) {
	n.t.Errorf("ran %s %v", name, args)
	return "", "", fmt.Errorf("no commands in this test")
}
func (n noRunner) Interactive(context.Context, string, []string) error {
	n.t.Error("interactive")
	return nil
}

func readEvents(t *testing.T, out string) []event {
	t.Helper()
	var evs []event
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var ev event
		dec := json.NewDecoder(strings.NewReader(l))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("stdout line is not an event: %q: %v", l, err)
		}
		evs = append(evs, ev)
	}
	for i, ev := range evs {
		if ev.V != 1 || ev.Seq != i+1 {
			t.Errorf("event %d: v=%d seq=%d", i, ev.V, ev.Seq)
		}
	}
	return evs
}

// runCLI runs the real argument handling with fake everything else.
func runCLI(t *testing.T, args []string, stdin string, r Runner) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &env{Out: &out, Err: &errb, In: strings.NewReader(stdin), Run: r,
		HTTP: httpFunc(func(*http.Request) (*http.Response, error) { return reply(500, ""), nil }), Sleep: func(int) {},
		ReadSecret: func(string) (string, error) { t.Error("read a terminal"); return "", fmt.Errorf("no tty") }}
	code = run(args, e)
	return code, out.String(), errb.String()
}

// ---- dry run with events and a spec, per kind ----

func TestDryRunEventsAndSpecForEachKind(t *testing.T) {
	cfg := writeCfgFile(t)
	for _, tc := range []struct {
		spec   string
		stages int
		plan   string
	}{
		{`{"kind":"aws","name":"w1","type":"m5.large","spot":true,"zone":"us-east-1b"}`, 9, "create-subnet --vpc-id '<vpc>' --cidr-block 10.77.17.0/24 --availability-zone us-east-1b"},
		{`{"kind":"gcp","name":"w1","type":"e2-standard-8","zone":"us-central1-b"}`, 9, "--zone=us-central1-b"},
		{`{"kind":"openshell","name":"w1"}`, 4, "openshell"},
	} {
		code, out, errs := runCLI(t, []string{"-config", cfg, "create", "--dry-run", "--events", "--spec", "-"}, tc.spec, noRunner{t})
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", tc.spec, code, errs)
		}
		evs := readEvents(t, out)
		if len(evs) != tc.stages {
			t.Errorf("%s: %d events, want %d: %s", tc.spec, len(evs), tc.stages, out)
		}
		for _, ev := range evs {
			if ev.State != "planned" || ev.Stage == "" || !strings.Contains(ev.Step, "/") {
				t.Errorf("event %+v", ev)
			}
		}
		if !strings.Contains(errs, "DRY RUN") || !strings.Contains(errs, tc.plan) {
			t.Errorf("%s: plan on stderr lacks %q:\n%s", tc.spec, tc.plan, errs)
		}
		if strings.Contains(out, "DRY RUN") {
			t.Errorf("human text on stdout")
		}
	}
}

func TestDryRunWithoutEventsIsUnchanged(t *testing.T) {
	cfg := writeCfgFile(t)
	code, out, errs := runCLI(t, []string{"-config", cfg, "create", "--dry-run", "w1"}, "", noRunner{t})
	if code != 0 || !strings.Contains(out, "DRY RUN") || strings.Contains(out, `"seq"`) || errs != "" {
		t.Errorf("code %d\nout: %s\nerr: %s", code, out, errs)
	}
}

func TestSpecErrorsAreEventsToo(t *testing.T) {
	cfg := writeCfgFile(t)
	code, out, errs := runCLI(t, []string{"-config", cfg, "create", "--dry-run", "--events", "--spec", "-"}, `{"kind":"aws","name":"w1","bogus":1}`, noRunner{t})
	if code == 0 || !strings.Contains(errs, "unknown field") {
		t.Fatalf("code %d: %s", code, errs)
	}
	evs := readEvents(t, out)
	if len(evs) != 1 || evs[0].State != "failed" || evs[0].Stage != "config" {
		t.Errorf("%+v", evs)
	}
}

func TestSpecNameAndArgumentMustAgree(t *testing.T) {
	cfg := writeCfgFile(t)
	if code, _, _ := runCLI(t, []string{"-config", cfg, "create", "--dry-run", "other", "--spec", "-"}, `{"name":"w1"}`, noRunner{t}); code == 0 {
		t.Error("accepted two names")
	}
	if code, _, _ := runCLI(t, []string{"-config", cfg, "create", "--dry-run", "--spec", "-"}, `{"kind":"aws"}`, noRunner{t}); code == 0 {
		t.Error("accepted no name at all")
	}
	if code, _, _ := runCLI(t, []string{"-config", cfg, "create", "--dry-run", "--spec", "-", "w1"}, `{"name":"w1"}`, noRunner{t}); code != 0 {
		t.Error("refused matching names")
	}
}

// ---- events on a real (fake-cloud) create ----

func TestAWSCreateEmitsStageEvents(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey, consoleTries: 1}
	e, r, _ := awsEnv(t, f, w)
	var human, stdout bytes.Buffer
	e.Out = &human
	e.Events = &eventSink{w: &stdout, env: e}
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, human.String())
	}
	_ = r
	evs := readEvents(t, stdout.String())
	var ok []string
	for _, ev := range evs {
		if ev.State == "ok" {
			ok = append(ok, ev.Stage)
		}
		if ev.State == "failed" {
			t.Errorf("failed event %+v", ev)
		}
	}
	if strings.Join(ok, ",") != strings.Join(vmStages, ",") {
		t.Errorf("stages ok: %v", ok)
	}
	// every running is followed by its ok before the next stage starts
	if evs[0].State != "running" || evs[0].Stage != "network" || evs[0].Step != "1/9" {
		t.Errorf("first event %+v", evs[0])
	}
	if !strings.Contains(human.String(), "4/8 instance") {
		t.Errorf("human text lost its numbering:\n%s", human.String())
	}
	if strings.Contains(stdout.String(), "4/8") {
		t.Errorf("human text on the event stream")
	}
}

func TestEventFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		op     string
		stderr string
		class  string
		zone   string
		stage  string
	}{
		{"no spot capacity", "run-instances", "An error occurred (InsufficientInstanceCapacity) when calling RunInstances: no capacity", "spot_capacity", "us-east-1a", "instance"},
		{"login expired", "describe-vpcs", "Your session has expired. Please reauthenticate using 'aws login'.", "creds_expired", "", "network"},
		{"expired token", "create-volume", "An error occurred (ExpiredToken) when calling CreateVolume: The security token included in the request is expired", "creds_expired", "", "data_disk"},
		{"quota", "run-instances", "An error occurred (VcpuLimitExceeded): You have requested more vCPU capacity", "quota", "", "instance"},
		{"other", "describe-vpcs", "something odd", "other", "", "network"},
	} {
		w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
		f := &awsFake{console: consoleKey}
		e, r, _ := awsEnv(t, f, w)
		inner := r.handle
		r.handle = func(cl call, fr *fakeRunner) (string, string, error) {
			if cl.name == "aws" && cl.args[1] == tc.op {
				return "", tc.stderr, fmt.Errorf("aws: %s", tc.stderr)
			}
			return inner(cl, fr)
		}
		var stdout bytes.Buffer
		e.Out = &bytes.Buffer{}
		e.Events = &eventSink{w: &stdout, env: e}
		if err := e.create(context.Background(), "w1", createOpts{}); err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
		evs := readEvents(t, stdout.String())
		last := evs[len(evs)-1]
		if last.State != "failed" || last.Class != tc.class || last.Zone != tc.zone || last.Stage != tc.stage {
			t.Errorf("%s: %+v", tc.name, last)
		}
	}
}

func TestBootstrapFailureIsClassified(t *testing.T) {
	e, out := nopEnv(t)
	e.stages = &stageTracker{names: vmStages}
	e.begin(6, "6/8 packages\n")
	_ = out
	e.failStage(fmt.Errorf("root bootstrap: ssh: exit status 100"))
	last := lastEvent(t, e)
	if last.Class != "bootstrap_failed" || last.Stage != "bootstrap" {
		t.Errorf("%+v", last)
	}
}

func nopEnv(t *testing.T) (*env, *bytes.Buffer) {
	var stdout bytes.Buffer
	c := testCfg(t)
	e, _, _ := newEnv(t, c, &fakeWorld{}, &fakeRunner{})
	e.Events = &eventSink{w: &stdout, env: e}
	return e, &stdout
}

func lastEvent(t *testing.T, e *env) event {
	t.Helper()
	evs := readEvents(t, e.Events.w.(*bytes.Buffer).String())
	return evs[len(evs)-1]
}

func TestEventsNeverCarrySecrets(t *testing.T) {
	e, out := nopEnv(t)
	e.addSecret("hunter2-the-llm-key")
	e.stages = &stageTracker{names: vmStages}
	e.begin(7, "")
	e.failStage(fmt.Errorf("tailscale up: bad key %s and hunter2-the-llm-key; Authorization: Bearer abc.def-123 token=xyz AKIAABCDEFGHIJKLMNOP sk-live-abcdefghijk\nsecond line", secretKey))
	s := out.String()
	for _, bad := range []string{secretKey, "tskey", "hunter2", "abc.def", "xyz", "AKIA", "sk-live"} {
		if strings.Contains(s, bad) {
			t.Errorf("event holds %q: %s", bad, s)
		}
	}
	if strings.Count(s, "\n") != 2 { // running + failed: a message is one line
		t.Errorf("lines: %q", s)
	}
	long := strings.Repeat("x", 1000)
	e.emit(event{Stage: "s", State: "running", Msg: long})
	if l := lastEvent(t, e); len(l.Msg) > 320 {
		t.Errorf("message not capped: %d", len(l.Msg))
	}
}

// ---- no prompts ----

type failReader struct{ t *testing.T }

func (f failReader) Read([]byte) (int, error) {
	f.t.Error("read the terminal")
	return 0, fmt.Errorf("terminal")
}

func TestDestroyYesNeverReadsTheTerminal(t *testing.T) {
	e, _, w, out := destroyEnv(t, "RUNNING", "")
	e.In = failReader{t}
	e.ReadSecret = func(string) (string, error) { t.Error("hidden prompt"); return "", fmt.Errorf("no") }
	e.NoPrompt = true
	if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true, Yes: true}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(w.peerRemoved) == 0 {
		t.Error("did not destroy")
	}
}

func TestNoPromptDestroyWithoutYesChangesNothing(t *testing.T) {
	e, r, w, _ := destroyEnv(t, "RUNNING", "w1\n")
	e.In = failReader{t}
	e.NoPrompt = true
	e.stages = &stageTracker{names: []string{"destroy"}}
	err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true})
	if _, ok := err.(*needsInputError); !ok {
		t.Fatalf("want needs_input, got %v", err)
	}
	if classify(err, "destroy") != "needs_input" {
		t.Error("class")
	}
	if len(r.calls) > 1 || len(w.deleted) != 0 || len(w.peerRemoved) != 0 {
		t.Errorf("changed things: %v", r.calls)
	}
}

func TestYesAloneStillAsksNoQuestionsButDefaultStillPrompts(t *testing.T) {
	// without --yes and without --no-prompt, the typed name is still needed
	e, _, w, _ := destroyEnv(t, "RUNNING", "nope\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err == nil || len(w.peerRemoved) != 0 {
		t.Errorf("proceeded without the name: %v", err)
	}
}

func TestNoPromptCreateWithoutAKeyIsNeedsInput(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, _, c := awsEnv(t, f, w)
	os.Remove(c.TSOAuthFile) // no OAuth client, no key file
	e.NoPrompt = true
	e.ReadSecret = func(string) (string, error) { t.Error("hidden prompt"); return "", fmt.Errorf("no tty") }
	e.In = failReader{t}
	var stdout bytes.Buffer
	e.Out = &bytes.Buffer{}
	e.Events = &eventSink{w: &stdout, env: e}
	err := e.create(context.Background(), "w1", createOpts{})
	if _, ok := err.(*needsInputError); !ok {
		t.Fatalf("got %v", err)
	}
	evs := readEvents(t, stdout.String())
	if last := evs[len(evs)-1]; last.Class != "needs_input" || last.Stage != "tailnet" {
		t.Errorf("%+v", last)
	}
}

func TestPromptsStillWorkByDefault(t *testing.T) {
	// existing behaviour: with a terminal the hidden prompt is used
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, _, c := awsEnv(t, f, w)
	os.Remove(c.TSOAuthFile)
	asked := false
	e.ReadSecret = func(string) (string, error) { asked = true; return secretKey, nil }
	e.create(context.Background(), "w1", createOpts{})
	if !asked {
		t.Error("the prompt is gone")
	}
}

// ---- per-request zone ----

func TestPerRequestZoneGetsItsOwnSubnet(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, r, c := awsEnv(t, f, w)
	c.baseAZ = "us-east-1a"
	c.AWSAZ = "us-east-1c"
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	var sub, vol string
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "create-subnet") {
			sub = cl.line()
		}
		if strings.Contains(cl.line(), "create-volume") {
			vol = cl.line()
		}
	}
	if !strings.Contains(sub, "--cidr-block 10.77.18.0/24") || !strings.Contains(sub, "--availability-zone us-east-1c") || !strings.Contains(sub, "comebye-worker-subnet-us-east-1c") {
		t.Errorf("subnet: %s", sub)
	}
	if !strings.Contains(vol, "--availability-zone us-east-1c") {
		t.Errorf("volume: %s", vol)
	}
	var lookup string
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "describe-subnets") {
			lookup = cl.line()
			break
		}
	}
	if !strings.Contains(lookup, "tag:Name,Values=comebye-worker-subnet-us-east-1c") {
		t.Errorf("lookup by zone name: %s", lookup)
	}
}

func TestConfigZoneKeepsTheOriginalSubnet(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, r, c := awsEnv(t, f, w)
	c.baseAZ = c.AWSAZ
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "create-subnet") && !strings.Contains(cl.line(), "--cidr-block 10.77.0.0/24") {
			t.Errorf("%s", cl.line())
		}
		if strings.Contains(cl.line(), "describe-subnets") && strings.Contains(cl.line(), "subnet-us-") {
			t.Errorf("zone-named subnet looked up for the config's zone: %s", cl.line())
		}
	}
}

func TestSubnetRangesPerZone(t *testing.T) {
	c := testAWSCfg(t)
	c.baseAZ = "us-east-1a"
	p := &awsProv{cfg: c}
	seen := map[string]string{}
	for _, z := range []string{"us-east-1b", "us-east-1c", "us-east-1d", "us-east-1f"} {
		c.AWSAZ = z
		r, err := p.subnetRange()
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[r]; dup || r == c.SubnetRange {
			t.Errorf("%s reuses %s (%s)", z, r, prev)
		}
		seen[r] = z
	}
	c.VPCRange, c.SubnetRange, c.AWSAZ = "10.77.0.0/24", "10.77.0.0/24", "us-east-1b"
	if _, err := p.subnetRange(); err == nil {
		t.Error("a /24 VPC cannot hold per-zone subnets")
	}
}

func TestAWSDestroyDeletesEverySubnetItMade(t *testing.T) {
	f := fullAWS()
	e, r, out := awsDestroyEnv(t, f, "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !f.did("delete-subnet") {
		t.Fatal("no subnet deleted")
	}
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "describe-subnets") && strings.Contains(cl.line(), "Subnets[]") && strings.Contains(cl.line(), "tag:Name") {
			t.Errorf("cleanup finds subnets by one name, not by label: %s", cl.line())
		}
	}
}
