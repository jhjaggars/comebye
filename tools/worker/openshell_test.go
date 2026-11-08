package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeOpenShell is a shell script standing in for the openshell CLI. It keeps
// one sandbox ("w1") in a state directory and logs every call (argv on one
// line, exec stdin in a file), so the tests drive the real osRunner and the
// real argument handling.
const fakeOpenShell = `#!/bin/bash
D=$FAKE_OS_DIR
echo "$*" >>"$D/calls"
sub="$1 $2"
label=${FAKE_OS_LABEL-managed-by=comebye-worker}
case "$sub" in
"sandbox list")
  [ -f "$D/exists" ] || { echo '{"next_page_token":"","sandboxes":[]}'; exit 0; }
  lab='{}'; [ -n "$label" ] && lab="{\"${label%%=*}\":\"${label#*=}\"}"
  printf '{"next_page_token":"","sandboxes":[{"id":"sbx-0123","name":"w1","workspace":"ws1","labels":%s,"phase":"%s","created_at":"2026-11-08 10:00:00","exit_code":null}]}\n' "$lab" "$(cat "$D/phase")"
  ;;
"sandbox create") touch "$D/exists"; echo Ready >"$D/phase"; echo '{}' ;;
"sandbox start") echo Ready >"$D/phase" ;;
"sandbox stop") echo Stopped >"$D/phase" ;;
"sandbox delete") rm -f "$D/exists" ;;
"sandbox exec")
  script="${@: -1}"
  n=$(( $(cat "$D/n" 2>/dev/null || echo 0) + 1 )); echo $n >"$D/n"
  stdin="$D/stdin.$n"; cat >"$stdin" 2>/dev/null || true
  echo "$script" >"$D/script.$n"
  case "$script" in
    *"up --auth-key"*) touch "$D/joined"; cp "$stdin" "$D/key" ;;
    *" ip -4"*) [ -f "$D/joined" ] && echo 100.64.0.9 || exit 1 ;;
    *"logout"*"claude auth"*|*"tailscale"*" logout"*) rm -f "$D/joined" ;;
    *'printf %s "$HOME"'*) printf /sandbox ;;
    *env.tmp*) cp "$stdin" "$D/envfile"; : >"$D/hub-announces" ;;
    *"tar -czf"*) cat "$D/sessions.tgz" ;;
    *"comebye -version"*) echo "comebye v0.3.1" ;;
    *pgrep*) echo active ;;
    *"herdr server stop"*) echo "$n stop" >>"$D/order" ;;
    *"seq 1 30"*) echo "$n start" >>"$D/order" ;;
  esac
  case "$script" in *"tar -czf"*) echo "$n pull" >>"$D/order" ;; *"up --auth-key"*) echo "$n up" >>"$D/order" ;; *"tailscale"*" logout"*) echo "$n logout" >>"$D/order" ;; esac
  ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
`

type osFake struct {
	dir      string
	e        *env
	w        *fakeWorld
	out      interface{ String() string }
	instance string // body of the hub's PUT /api/peers/NAME/instance
}

func (f *osFake) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return string(b)
}
func (f *osFake) calls() []string {
	return strings.Split(strings.TrimSpace(f.read("calls")), "\n")
}
func (f *osFake) anyCall(sub string) bool {
	for _, c := range f.calls() {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func osCfg(t *testing.T, bin string) *Config {
	t.Helper()
	c := testCfg(t)
	c.Provider = "openshell"
	c.OpenShellBin = bin
	c.OpenShellGateway = "homelab"
	c.OpenShellWorkspace = "ws1"
	c.SandboxImage = "registry.test/comebye-sandbox:{version}"
	c.SandboxPolicy = "/etc/policy.yaml"
	c.SandboxProviders = []string{"github-push", "anthropic"}
	c.SandboxCPU, c.SandboxMemory = "2", "6Gi"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func newOSFake(t *testing.T) *osFake {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "openshell")
	if err := os.WriteFile(bin, []byte(fakeOpenShell), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OS_DIR", dir)
	c := osCfg(t, bin)
	writeOAuth(t, c)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	e, out, _ := newEnv(t, c, w, nil)
	e.Run = osRunner{}
	return &osFake{dir: dir, e: e, w: w, out: out}
}

// The hub lists the worker as soon as comebye has its settings.
func (f *osFake) hubFollowsEnv() {
	f.e.HTTP = httpFunc(func(r *http.Request) (*http.Response, error) {
		if _, err := os.Stat(filepath.Join(f.dir, "hub-announces")); err == nil {
			f.w.peers["w1"] = true
		}
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/instance") {
			b, _ := io.ReadAll(r.Body)
			f.instance = string(b)
			return reply(200, "{}"), nil
		}
		return f.w.Do(r)
	})
}

func TestOpenShellConfig(t *testing.T) {
	c := osCfg(t, "openshell")
	c.SSHSourceCIDR, c.Project, c.Zone = "", "", "" // none of the cloud settings apply
	if err := c.Validate(); err != nil {
		t.Fatalf("gcp settings are required for openshell: %v", err)
	}
	for _, tc := range []struct {
		mod  func(*Config)
		want string
	}{
		{func(c *Config) { c.SandboxImage = "" }, "sandbox_image is required"},
		{func(c *Config) { c.OpenShellWorkspace = "" }, "openshell_workspace is required"},
		{func(c *Config) { c.OpenShellEndpoint = "https://gw.example.com" }, "alternatives"},
		{func(c *Config) { c.OpenShellGateway = ""; c.OpenShellEndpoint = "http://gw" }, "bare https"},
		{func(c *Config) { c.SandboxCPU = "two" }, "sandbox_cpu"},
		{func(c *Config) { c.SandboxProviders = []string{"a b"} }, "bad provider"},
		{func(c *Config) { c.OpenShellGateway = "x;rm" }, "openshell_gateway"},
		{func(c *Config) { c.LLMGatewayURL = "https://llm.tail1.ts.net" }, "llm_gateway_url is not supported"},
	} {
		c := osCfg(t, "openshell")
		tc.mod(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
}

func TestOpenShellConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte(`provider=openshell
openshell_gateway=homelab
openshell_workspace=openshell-sandboxes
sandbox_image=registry.jaggars.cc/comebye-sandbox:{version}
sandbox_providers=anthropic-claude, github-push
sandbox_cpu=2
sandbox_memory=6Gi
ts_allow_login=you@example.com
hub_url=https://hub.tail1.ts.net
hub_peer_url=https://hub.tail1.ts.net
`), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenShellBin != "openshell" || len(c.SandboxProviders) != 2 || c.SandboxProviders[1] != "github-push" || c.OpenShellWorkspace != "openshell-sandboxes" {
		t.Errorf("%+v", c)
	}
}

func TestOpenShellCreateEndToEnd(t *testing.T) {
	f := newOSFake(t)
	f.hubFollowsEnv()
	f.w.devices = []tsDevice{
		{ID: "old", Hostname: "w1", Name: "w1.tail1.ts.net", Tags: []string{"tag:worker"}},
		{ID: "laptop", Hostname: "w1", Name: "w1-laptop"},
	}
	if err := f.e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatalf("create: %v\n%s", err, f.out.String())
	}
	// the sandbox is made from the hub's version, labelled ours, with the providers and the init
	var create string
	for _, c := range f.calls() {
		if strings.HasPrefix(c, "sandbox create") {
			create = c
		}
	}
	for _, want := range []string{
		"--name w1", "--from registry.test/comebye-sandbox:v0.3.1", "--label managed-by=comebye-worker",
		"--provider github-push --provider anthropic", "--policy /etc/policy.yaml", "--cpu 2", "--memory 6Gi",
		"-g homelab --workspace ws1", "-o json -- comebye-sandbox-init",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("create lacks %q:\n%s", want, create)
		}
	}
	// stale device cleared, key single use, and delivered on stdin only
	if len(f.w.deleted) != 1 || f.w.deleted[0] != "old" {
		t.Errorf("deleted %v", f.w.deleted)
	}
	if len(f.w.mintBodies) != 1 || !strings.Contains(f.w.mintBodies[0], `"reusable":false`) || !strings.Contains(f.w.mintBodies[0], `"preauthorized":true`) {
		t.Errorf("mint: %v", f.w.mintBodies)
	}
	if f.read("key") != secretKey {
		t.Errorf("the key did not arrive on stdin: %q", f.read("key"))
	}
	if strings.Contains(f.read("calls"), secretKey) || strings.Contains(f.out.String(), secretKey) {
		t.Error("the key leaked into argv or output")
	}
	up := ""
	for i := 1; ; i++ {
		s := f.read("script." + itoa(i))
		if s == "" {
			break
		}
		if strings.Contains(s, "up --auth-key") {
			up = s
		}
	}
	for _, want := range []string{`--socket="$HOME/.ts/tailscaled.sock"`, "--auth-key=file:/dev/stdin", "--hostname='w1'", "--advertise-tags='tag:worker'"} {
		if !strings.Contains(up, want) {
			t.Errorf("tailscale up lacks %s: %s", want, up)
		}
	}
	// the settings file the init waits for
	env := f.read("envfile")
	for _, want := range []string{"COMEBYE_HOST_NAME=w1\n", "COMEBYE_TS_ALLOW_LOGIN=you@example.com\n", "COMEBYE_HUB=https://hub.tail1.ts.net\n",
		"COMEBYE_ANNOUNCE=1\n", "COMEBYE_LAUNCH_ROOTS=/sandbox/code\n", "COMEBYE_WORKTREE_ROOT=/sandbox/worktrees\n", "SBX_IDLE_MINUTES=120\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env file lacks %q:\n%s", want, env)
		}
	}
	if f.instance != `{"instance":"sbx-0123"}` || !strings.Contains(f.out.String(), "hub: w1 is ok") {
		t.Errorf("hub join not reported:\n%s", f.out.String())
	}
	if f.anyCall("tailscale serve") {
		t.Error("serve is the init's job")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestOpenShellCreateResumesAnEnrolledSandbox(t *testing.T) {
	f := newOSFake(t)
	f.hubFollowsEnv()
	os.WriteFile(filepath.Join(f.dir, "exists"), nil, 0o600)
	os.WriteFile(filepath.Join(f.dir, "phase"), []byte("Ready\n"), 0o600)
	os.WriteFile(filepath.Join(f.dir, "joined"), nil, 0o600)
	if err := f.e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if len(f.w.mintBodies) != 0 || f.anyCall("sandbox create") {
		t.Errorf("re-minted or re-created: %v", f.w.mintBodies)
	}
}

func TestOpenShellCreateStartsACompletedSandbox(t *testing.T) {
	f := newOSFake(t)
	f.hubFollowsEnv()
	os.WriteFile(filepath.Join(f.dir, "exists"), nil, 0o600)
	os.WriteFile(filepath.Join(f.dir, "phase"), []byte("Completed\n"), 0o600)
	os.WriteFile(filepath.Join(f.dir, "joined"), nil, 0o600)
	if err := f.e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if !f.anyCall("sandbox start") {
		t.Error("a Completed sandbox was not started")
	}
}

func TestOpenShellRefusesForeignSandbox(t *testing.T) {
	f := newOSFake(t)
	t.Setenv("FAKE_OS_LABEL", "")
	os.WriteFile(filepath.Join(f.dir, "exists"), nil, 0o600)
	os.WriteFile(filepath.Join(f.dir, "phase"), []byte("Ready\n"), 0o600)
	if err := f.e.create(context.Background(), "w1", createOpts{}); err == nil || !strings.Contains(err.Error(), "refusing to manage") {
		t.Fatalf("create: %v", err)
	}
	f.e.In = strings.NewReader("w1\n")
	if err := f.e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err == nil {
		t.Fatal("destroyed a sandbox this tool did not create")
	}
	for _, bad := range []string{"sandbox delete", "sandbox stop", "sandbox create"} {
		if f.anyCall(bad) {
			t.Errorf("ran %s", bad)
		}
	}
}

func TestOpenShellDryRunTouchesNothing(t *testing.T) {
	f := newOSFake(t)
	f.e.DryRun = true
	if err := f.e.create(context.Background(), "w1", createOpts{Version: "v0.3.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "calls")); err == nil {
		t.Errorf("ran openshell: %s", f.read("calls"))
	}
	if len(f.w.apiCalls) != 0 {
		t.Errorf("network: %v", f.w.apiCalls)
	}
	for _, want := range []string{"sandbox create", "comebye-sandbox:v0.3.1", "COMEBYE_ANNOUNCE=1", "key on stdin: <not shown>"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, f.out.String())
		}
	}
}

func TestOpenShellStatusStopStart(t *testing.T) {
	f := newOSFake(t)
	os.WriteFile(filepath.Join(f.dir, "exists"), nil, 0o600)
	os.WriteFile(filepath.Join(f.dir, "phase"), []byte("Ready\n"), 0o600)
	os.WriteFile(filepath.Join(f.dir, "joined"), nil, 0o600)
	f.w.hubVersion = "v0.3.1"
	f.w.peers["w1"] = true
	ctx := context.Background()
	if err := f.e.status(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RUNNING", "archive     host w1.sbx-0123", "tailnet     100.64.0.9", "comebye v0.3.1", "hub         ok"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, f.out.String())
		}
	}
	if err := f.e.stop(ctx, "w1"); err != nil || !f.anyCall("sandbox stop -g homelab --workspace ws1 w1") {
		t.Fatalf("stop: %v", err)
	}
	if err := f.e.start(ctx, "w1"); err != nil || !f.anyCall("sandbox start -g homelab --workspace ws1 w1") {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(f.out.String(), "w1 is up, comebye comebye v0.3.1") {
		t.Errorf("start output:\n%s", f.out.String())
	}
}

func TestOpenShellLoginIsATty(t *testing.T) {
	f := newOSFake(t)
	os.WriteFile(filepath.Join(f.dir, "exists"), nil, 0o600)
	os.WriteFile(filepath.Join(f.dir, "phase"), []byte("Ready\n"), 0o600)
	r := &fakeRunner{state: map[string]bool{}}
	r.handle = func(c call, _ *fakeRunner) (string, string, error) { return osRunnerFor(f, c) }
	f.e.Run = r
	if err := f.e.login(context.Background(), "w1", "claude"); err != nil {
		t.Fatal(err)
	}
	last := r.calls[len(r.calls)-1]
	if !strings.Contains(last.line(), "sandbox exec -g homelab --workspace ws1 -n w1 --tty --no-login-shell -- bash -c") || !strings.Contains(last.line(), "claude auth login") {
		t.Errorf("login ran: %s", last.line())
	}
}

// osRunnerFor answers the lookups a login makes, through the fake script.
func osRunnerFor(f *osFake, c call) (string, string, error) {
	out, errOut, err := osRunner{}.Run(context.Background(), c.name, c.args, []byte(c.stdin))
	return out, errOut, err
}

func TestOpenShellNeedsALogin(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "openshell")
	os.WriteFile(bin, []byte("#!/bin/bash\necho 'Error: status: Unauthenticated, message: token expired' >&2\nexit 1\n"), 0o755)
	c := osCfg(t, bin)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	e, _, _ := newEnv(t, c, w, nil)
	e.Run = osRunner{}
	err := e.status(context.Background(), "w1")
	if err == nil || !strings.Contains(err.Error(), "gateway login homelab") {
		t.Fatalf("err = %v", err)
	}
}

// destroyFake is a running, enrolled sandbox with sessions, an archive bucket,
// a hub peer entry and a tagged tailnet device.
func destroyFake(t *testing.T, phase string) (*osFake, *miniS3) {
	t.Helper()
	f := newOSFake(t)
	os.WriteFile(filepath.Join(f.dir, "exists"), nil, 0o600)
	os.WriteFile(filepath.Join(f.dir, "phase"), []byte(phase+"\n"), 0o600)
	os.WriteFile(filepath.Join(f.dir, "joined"), nil, 0o600)
	os.WriteFile(filepath.Join(f.dir, "sessions.tgz"), []byte(tgz(t, tarEntry{name: piStream, body: sessionBody()})), 0o600)
	f.w.peers["w1"] = true
	f.w.devices = []tsDevice{{ID: "d1", Hostname: "w1", Name: "w1.tail1.ts.net", Tags: []string{"tag:worker"}}}
	s3, srv := startS3(t)
	writeArchiveEnv(t, f.e.Cfg, srv.URL)
	f.e.In = strings.NewReader("w1\n")
	return f, s3
}

func TestOpenShellDestroyOrder(t *testing.T) {
	f, s3 := destroyFake(t, "Ready")
	if err := f.e.destroy(context.Background(), "w1", destroyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	// archive (agents running), stop the agents, archive again, leave the tailnet; then the
	// tailnet device, the hub entry and the sandbox
	var order []string
	for _, l := range strings.Fields(strings.ReplaceAll(f.read("order"), "\n", " ")) {
		if l == "pull" || l == "stop" || l == "logout" {
			order = append(order, l)
		}
	}
	if got := strings.Join(order, ","); got != "pull,stop,pull,stop,logout" {
		t.Errorf("order on the sandbox: %s", got)
	}
	if !f.anyCall("sandbox delete -g homelab --workspace ws1 w1") {
		t.Error("the sandbox was not deleted")
	}
	if len(f.w.deleted) != 1 || f.w.deleted[0] != "d1" || len(f.w.peerRemoved) != 1 || f.w.peerRemoved[0] != "w1" {
		t.Errorf("tailnet %v hub %v", f.w.deleted, f.w.peerRemoved)
	}
	segs := s3.keys("v1/w1.sbx-0123/pi/")
	if len(segs) != 1 {
		t.Fatalf("archive host should be NAME.SANDBOX_ID: %v", s3.keys(""))
	}
	// the delete is the last thing done to the sandbox
	cs := f.calls()
	last := ""
	for _, c := range cs {
		if strings.HasPrefix(c, "sandbox exec") || strings.HasPrefix(c, "sandbox delete") {
			last = c
		}
	}
	if !strings.HasPrefix(last, "sandbox delete") {
		t.Errorf("something ran on the sandbox after the delete: %s", last)
	}
}

func TestOpenShellDestroyRefusesACompletedSandboxUnlessNoArchive(t *testing.T) {
	f, _ := destroyFake(t, "Completed")
	err := f.e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "worker start w1") || !strings.Contains(err.Error(), "volume") {
		t.Fatalf("err = %v", err)
	}
	if f.anyCall("sandbox delete") || len(f.w.peerRemoved) != 0 {
		t.Error("changed something")
	}
	f.e.In = strings.NewReader("w1\n")
	if err := f.e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if !f.anyCall("sandbox delete") || f.anyCall("sandbox exec") {
		t.Errorf("a stopped sandbox is deleted without exec: %v", f.calls())
	}
}

func TestOpenShellDestroyHasNoDataDisk(t *testing.T) {
	f, _ := destroyFake(t, "Ready")
	if err := f.e.destroy(context.Background(), "w1", destroyOpts{DeleteDataDisk: true}); err == nil {
		t.Fatal("accepted --delete-data-disk")
	}
	if len(f.calls()) > 1 {
		t.Errorf("did things: %v", f.calls())
	}
}

func TestOpenShellArchiveAll(t *testing.T) {
	f, s3 := destroyFake(t, "Ready")
	if err := f.e.archiveAll(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if !f.anyCall("--selector managed-by=comebye-worker") {
		t.Error("archive --all listed sandboxes this tool did not create")
	}
	if len(s3.keys("v1/w1.sbx-0123/pi/")) != 1 {
		t.Errorf("not archived: %v", s3.keys(""))
	}
}
