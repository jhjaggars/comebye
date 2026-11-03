package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const secretKey = "tskey-auth-SECRETSECRET-DONOTLEAK"

// ---- fakes ----

type call struct {
	name  string
	args  []string
	stdin string
}

func (c call) line() string { return c.name + " " + strings.Join(c.args, " ") }

// fakeRunner stands in for gcloud, ssh, ssh-keygen. handle returns the
// result for one call.
type fakeRunner struct {
	calls  []call
	handle func(c call, f *fakeRunner) (out, errOut string, err error)
	state  map[string]bool
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string, stdin []byte) (string, string, error) {
	c := call{name, args, string(stdin)}
	f.calls = append(f.calls, c)
	return f.handle(c, f)
}
func (f *fakeRunner) Interactive(_ context.Context, name string, args []string) error {
	f.calls = append(f.calls, call{name: name, args: args})
	return nil
}
func (f *fakeRunner) saw(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c.line(), sub) {
			return true
		}
	}
	return false
}

type httpFunc func(*http.Request) (*http.Response, error)

func (h httpFunc) Do(r *http.Request) (*http.Response, error) { return h(r) }

func reply(code int, body string, hdr ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: h}
}

func testCfg(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	c := defaults()
	c.Project, c.Zone = "comebye-test-0001", "us-central1-a"
	c.SSHSourceCIDR = "203.0.113.7/32"
	c.TSAllowLogin = "you@example.com"
	c.HubURL = "https://hub.tail1.ts.net"
	c.HubPeerURL = "http://hub.tail1.ts.net:8742"
	c.StateDir = filepath.Join(dir, "state")
	c.TSOAuthFile = filepath.Join(dir, "oauth")
	c.TSAuthKeyFile = filepath.Join(dir, "authkey")
	c.TSAPIBase = "https://api.test"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return &c
}

func writeOAuth(t *testing.T, c *Config) {
	t.Helper()
	if err := os.WriteFile(c.TSOAuthFile, []byte("client_id=cid\nclient_secret=csecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeWorld is a tailnet API and a hub, in memory.
type fakeWorld struct {
	devices     []tsDevice
	deleted     []string
	mintBodies  []string
	peers       map[string]bool // name -> registered
	hubVersion  string
	peerRemoved []string
	apiCalls    []string
}

func (w *fakeWorld) Do(r *http.Request) (*http.Response, error) {
	w.apiCalls = append(w.apiCalls, r.Method+" "+r.URL.Host+r.URL.Path)
	switch r.URL.Host {
	case "api.test":
		switch {
		case r.URL.Path == "/api/v2/oauth/token":
			return reply(200, `{"access_token":"tok"}`), nil
		case r.Header.Get("Authorization") != "Bearer tok":
			return reply(401, ""), nil
		case r.Method == "POST" && r.URL.Path == "/api/v2/tailnet/-/keys":
			b, _ := io.ReadAll(r.Body)
			w.mintBodies = append(w.mintBodies, string(b))
			return reply(200, fmt.Sprintf(`{"id":"kid1","key":%q}`, secretKey)), nil
		case r.Method == "GET" && r.URL.Path == "/api/v2/tailnet/-/devices":
			b, _ := json.Marshal(map[string]any{"devices": w.devices})
			return reply(200, string(b)), nil
		case r.Method == "DELETE":
			w.deleted = append(w.deleted, strings.TrimPrefix(r.URL.Path, "/api/v2/device/"))
			return reply(200, ""), nil
		}
	case "hub.tail1.ts.net":
		switch {
		case r.URL.Path == "/api/whoami":
			return reply(200, fmt.Sprintf(`{"version":%q}`, w.hubVersion), "X-Comebye-Version", w.hubVersion), nil
		case r.Method == "GET" && r.URL.Path == "/api/peers":
			var ps []hubPeer
			for n, ok := range w.peers {
				if ok {
					ps = append(ps, hubPeer{Name: n, Status: "ok", Version: w.hubVersion})
				}
			}
			b, _ := json.Marshal(map[string]any{"peers": ps})
			return reply(200, string(b)), nil
		case r.Method == "DELETE":
			n := strings.TrimPrefix(r.URL.Path, "/api/peers/")
			w.peerRemoved = append(w.peerRemoved, n)
			delete(w.peers, n)
			return reply(200, "{}"), nil
		}
	}
	return reply(404, ""), nil
}

const instJSON = `{"name":"w1","status":"RUNNING","machineType":"zones/z/machineTypes/e2-standard-4","labels":{"managed-by":"comebye-worker"},"networkInterfaces":[{"accessConfigs":[{"natIP":"198.51.100.9"}]}]}`

// gceFake simulates enough of gcloud + ssh for a full create.
func gceFake(w *fakeWorld, c *Config) func(call, *fakeRunner) (string, string, error) {
	return func(cl call, f *fakeRunner) (string, string, error) {
		line := cl.line()
		notFound := fmt.Errorf("exit 1")
		switch cl.name {
		case "ssh-keygen":
			kp := cl.args[len(cl.args)-1]
			os.WriteFile(kp, []byte("PRIVATE"), 0o600)
			os.WriteFile(kp+".pub", []byte("ssh-ed25519 AAAAPUBKEY comebye-worker\n"), 0o644)
			return "", "", nil
		case "gcloud":
			switch {
			case strings.Contains(line, "networks describe"), strings.Contains(line, "subnets describe"),
				strings.Contains(line, "firewall-rules describe"), strings.Contains(line, "disks describe"):
				if f.state[strings.Fields(line)[3]+strings.Fields(line)[4]+"made"] {
					return "{}", "", nil
				}
				return "", "ERROR: The resource 'x' was not found", notFound
			case strings.Contains(line, "instances describe"):
				if !f.state["instance"] {
					return "", "ERROR: was not found", notFound
				}
				return instJSON, "", nil
			case strings.Contains(line, "instances create"):
				f.state["instance"] = true
				return "", "", nil
			case strings.Contains(line, "get-guest-attributes"):
				return "ssh-ed25519 AAAAHOSTKEY root@w1\n", "", nil
			default:
				return "", "", nil
			}
		case "ssh":
			remote := cl.args[len(cl.args)-1]
			switch {
			case remote == "tailscale ip -4":
				if f.state["tsup"] {
					return "100.64.0.9\n", "", nil
				}
				return "", "", fmt.Errorf("not up")
			case strings.HasPrefix(remote, "sudo tailscale up"):
				f.state["tsup"] = true
				w.peers["w1"] = false // joins only after comebye starts
				return "", "", nil
			case remote == "bash -s":
				w.peers["w1"] = true // comebye announces itself
				return "", "", nil
			}
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected %s", line)
	}
}

func newEnv(t *testing.T, c *Config, w *fakeWorld, r *fakeRunner) (*env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &env{Out: &out, Err: &errb, In: strings.NewReader(""), Run: r, Cfg: c, HTTP: w,
		Sleep: func(int) {}, ReadSecret: func(string) (string, error) { return "", fmt.Errorf("no tty") }}, &out, &errb
}

// ---- group 1: pure functions ----

func TestConfigValidation(t *testing.T) {
	c := testCfg(t)
	bad := map[string]func(*Config){
		"wide ssh source":  func(c *Config) { c.SSHSourceCIDR = "203.0.113.0/24" },
		"open ssh source":  func(c *Config) { c.SSHSourceCIDR = "0.0.0.0/0" },
		"placeholder":      func(c *Config) { c.SSHSourceCIDR = "CHANGE_ME" },
		"no login":         func(c *Config) { c.TSAllowLogin = "" },
		"shell in project": func(c *Config) { c.Project = "p; rm -rf /" },
		"hub http":         func(c *Config) { c.HubURL = "http://hub.tail1.ts.net" },
		"hub not tailnet":  func(c *Config) { c.HubURL = "https://evil.example.com" },
		"peer url no port": func(c *Config) { c.HubPeerURL = "http://hub.tail1.ts.net" },
		"peer url https":   func(c *Config) { c.HubPeerURL = "https://hub.tail1.ts.net:8742" },
		"root user":        func(c *Config) { c.SSHUser = "root" },
		"short idle":       func(c *Config) { c.IdleMinutes = 1 },
		"bad tag":          func(c *Config) { c.Tag = "worker" },
		"bad version pin":  func(c *Config) { c.HubVersion = "main" },
		"bad sha":          func(c *Config) { c.HerdrSHA256 = "abc" },
		"subnet not a net": func(c *Config) { c.SubnetRange = "10.77.0.5/24" },
	}
	for name, mut := range bad {
		cc := *c
		mut(&cc)
		if cc.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte("# c\nproject = comebye-test-0001\nzone=us-east1-b\nssh_source_cidr=198.51.100.1/32\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=http://h.t.ts.net:8742\nidle_minutes=30\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleMinutes != 30 || c.MachineType != "e2-standard-4" || c.Tag != "tag:worker" {
		t.Errorf("defaults/overrides wrong: %+v", c)
	}
	os.WriteFile(p, []byte("projcet=x\n"), 0o600)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Errorf("typo not caught: %v", err)
	}
	if _, err := LoadConfig(p + ".nope"); err == nil || !strings.Contains(err.Error(), "config.example") {
		t.Errorf("missing file message: %v", err)
	}
}

func TestNames(t *testing.T) {
	for _, n := range []string{"w1", "worker-2", "a"} {
		if checkName(n) != nil {
			t.Errorf("%q rejected", n)
		}
	}
	for _, n := range []string{"", "W1", "w_1", "1w", "w1;id", "-w", "w-", strings.Repeat("a", 40), "a b"} {
		if checkName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestParseHostKey(t *testing.T) {
	k, err := parseHostKey("ssh-ed25519 AAAAC3Nza root@w1\n")
	if err != nil || k != "ssh-ed25519 AAAAC3Nza" {
		t.Errorf("got %q, %v", k, err)
	}
	for _, bad := range []string{"", "ssh-rsa AAAA", "ssh-ed25519 AAAA\nssh-ed25519 BBBB", "ssh-ed25519 A;rm"} {
		if _, err := parseHostKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestStaleDevices(t *testing.T) {
	devs := []tsDevice{
		{ID: "1", Hostname: "w1", Tags: []string{"tag:worker"}, ConnectedToControl: false}, // stale: yes
		{ID: "2", Hostname: "w1", Tags: []string{"tag:worker"}, ConnectedToControl: true},  // online: no
		{ID: "3", Hostname: "w1", Tags: nil, ConnectedToControl: false},                    // untagged laptop: never
		{ID: "4", Hostname: "w2", Tags: []string{"tag:worker"}, ConnectedToControl: false}, // other worker: no
		{ID: "5", Hostname: "w1", Tags: []string{"tag:other"}, ConnectedToControl: false},  // other tag: no
	}
	got := staleDevices(devs, "w1", "tag:worker")
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("stale = %+v", got)
	}
	if own := ownDevices(devs, "w1", "tag:worker"); len(own) != 2 {
		t.Errorf("own = %+v", own)
	}
}

func TestSSHConfigIsStrict(t *testing.T) {
	c := testCfg(t)
	s := sshConfig(c, "w1", "198.51.100.9", "/kh")
	for _, want := range []string{"StrictHostKeyChecking yes", "UserKnownHostsFile /kh", "GlobalKnownHostsFile /dev/null", "IdentitiesOnly yes", "IdentityAgent none", "PasswordAuthentication no"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh_config lacks %q", want)
		}
	}
	p, err := writeSSHConfig(c, "w1", "198.51.100.9", "ssh-ed25519 AAAA")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("ssh_config mode %o", st.Mode().Perm())
	}
	kh, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "known_hosts"))
	if string(kh) != "198.51.100.9 ssh-ed25519 AAAA\n" {
		t.Errorf("known_hosts = %q", kh)
	}
}

func TestWithVarsRefusesShellSyntax(t *testing.T) {
	if _, err := withVars("#!/bin/bash\necho\n", [][2]string{{"A", "x; reboot"}}); err == nil {
		t.Error("accepted ;")
	}
	if _, err := withVars("#!/bin/bash\necho\n", [][2]string{{"A", "$(id)"}}); err == nil {
		t.Error("accepted $()")
	}
	s, err := withVars("#!/bin/bash\necho hi\n", [][2]string{{"A", "http://h.ts.net:8742"}, {"B", ""}})
	if err != nil || !strings.HasPrefix(s, "#!/bin/bash\nA='http://h.ts.net:8742'\nB=''\nexport A B\necho hi") {
		t.Errorf("got %q, %v", s, err)
	}
}

func TestResolveVersion(t *testing.T) {
	c := testCfg(t)
	w := &fakeWorld{hubVersion: "v0.3.1"}
	e, _, _ := newEnv(t, c, w, &fakeRunner{})
	if v, err := e.resolveVersion(context.Background(), ""); err != nil || v != "v0.3.1" {
		t.Errorf("hub version: %q, %v", v, err)
	}
	if v, _ := e.resolveVersion(context.Background(), "v0.2.0"); v != "v0.2.0" {
		t.Errorf("flag wins: %q", v)
	}
	c.HubVersion = "v0.2.5"
	if v, _ := e.resolveVersion(context.Background(), ""); v != "v0.2.5" {
		t.Errorf("config pin: %q", v)
	}
	c.HubVersion = ""
	for _, dev := range []string{"dev", "v0.3.1-4-gabc123", "(devel)", ""} {
		w.hubVersion = dev
		if _, err := e.resolveVersion(context.Background(), ""); err == nil {
			t.Errorf("accepted hub version %q", dev)
		}
	}
}

func TestOAuthFileMode(t *testing.T) {
	c := testCfg(t)
	writeOAuth(t, c)
	if _, _, err := LoadOAuth(c.TSOAuthFile); err != nil {
		t.Fatal(err)
	}
	os.Chmod(c.TSOAuthFile, 0o644)
	if _, _, err := LoadOAuth(c.TSOAuthFile); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("group/world readable accepted: %v", err)
	}
}

// ---- group 2: flows against fakes ----

func TestCreateEndToEnd(t *testing.T) {
	c := testCfg(t)
	writeOAuth(t, c)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{},
		devices: []tsDevice{
			{ID: "old", Hostname: "w1", Name: "w1.tail1.ts.net", Tags: []string{"tag:worker"}},
			{ID: "laptop", Hostname: "w1", Name: "w1-laptop"},
		}}
	r := &fakeRunner{state: map[string]bool{}}
	r.handle = func(cl call, f *fakeRunner) (string, string, error) {
		// network resources come into being once created
		if cl.name == "gcloud" && strings.Contains(cl.line(), " create ") && !strings.Contains(cl.line(), "instances") {
			f.state[strings.Fields(cl.line())[3]+strings.Fields(cl.line())[4]+"made"] = true
		}
		return gceFake(w, c)(cl, f)
	}
	e, out, errb := newEnv(t, c, w, r)
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}

	// stale tagged offline device removed; the untagged namesake is not
	if len(w.deleted) != 1 || w.deleted[0] != "old" {
		t.Errorf("deleted %v", w.deleted)
	}
	// the key was minted single-use, pre-authorized, tagged, short-lived
	if len(w.mintBodies) != 1 {
		t.Fatalf("mints: %v", w.mintBodies)
	}
	for _, want := range []string{`"reusable":false`, `"preauthorized":true`, `"tags":["tag:worker"]`, `"expirySeconds":600`} {
		if !strings.Contains(w.mintBodies[0], want) {
			t.Errorf("mint body lacks %s: %s", want, w.mintBodies[0])
		}
	}
	// the secret reached exactly one place: stdin of tailscale up
	var stdinHits int
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), secretKey) {
			t.Errorf("secret in argv: %s", cl.name)
		}
		if strings.Contains(cl.stdin, secretKey) {
			stdinHits++
			if !strings.Contains(cl.line(), "tailscale up --auth-key=file:/dev/stdin") {
				t.Errorf("secret on stdin of the wrong command: %s", cl.line())
			}
		}
	}
	if stdinHits != 1 {
		t.Errorf("secret on stdin %d times", stdinHits)
	}
	for _, s := range []string{out.String(), errb.String()} {
		if strings.Contains(s, secretKey) {
			t.Error("secret printed")
		}
	}
	// the instance carries no service account, no scopes, no secret, and the
	// data disk is not auto-deleted
	var create string
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "instances create") {
			create = cl.line()
		}
	}
	for _, want := range []string{"--no-service-account", "--no-scopes", "auto-delete=no", "block-project-ssh-keys=TRUE", "enable-oslogin=FALSE", "--max-run-duration=24h", "--labels=managed-by=comebye-worker", "ssh-ed25519 AAAAPUBKEY"} {
		if !strings.Contains(create, want) {
			t.Errorf("instance create lacks %q", want)
		}
	}
	// the bootstrap ran in order: root, idle check, tailscale up, user
	idx := func(prefix string) int {
		for i, cl := range r.calls {
			if cl.name == "ssh" && strings.HasPrefix(cl.args[len(cl.args)-1], prefix) {
				return i
			}
		}
		return -1
	}
	rootI, upI, serveI, userI := idx("sudo bash -s"), idx("sudo tailscale up"), idx("tailscale serve"), idx("bash -s")
	if rootI < 0 || !(rootI < upI && upI < serveI && serveI < userI) {
		t.Errorf("bootstrap order wrong: root %d, up %d, serve %d, user %d", rootI, upI, serveI, userI)
	}
	// the user script carried the hub's version and the join env
	var user string
	for _, cl := range r.calls {
		if cl.name == "ssh" && cl.args[len(cl.args)-1] == "bash -s" {
			user = cl.stdin
		}
	}
	for _, want := range []string{"WK_VERSION='v0.3.1'", "WK_HUB='http://hub.tail1.ts.net:8742'", "COMEBYE_ANNOUNCE=1", "sha256sum -c"} {
		if !strings.Contains(user, want) {
			t.Errorf("user script lacks %q", want)
		}
	}
	if !strings.Contains(out.String(), "hub: w1 is ok") {
		t.Errorf("did not report the join:\n%s", out)
	}
	// strict known_hosts came from the GCE attribute
	kh, _ := os.ReadFile(filepath.Join(c.WorkerDir("w1"), "known_hosts"))
	if string(kh) != "198.51.100.9 ssh-ed25519 AAAAHOSTKEY\n" {
		t.Errorf("known_hosts %q", kh)
	}
}

func TestCreateResumesWithoutReminting(t *testing.T) {
	c := testCfg(t)
	writeOAuth(t, c)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{"instance": true, "tsup": true}}
	r.handle = gceFake(w, c)
	r.handle = func(cl call, f *fakeRunner) (string, string, error) {
		if cl.name == "gcloud" && strings.Contains(cl.line(), "describe") && !strings.Contains(cl.line(), "instances") {
			return "{}", "", nil // network, disk present
		}
		return gceFake(w, c)(cl, f)
	}
	e, _, _ := newEnv(t, c, w, r)
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(w.mintBodies) != 0 || r.saw("instances create") {
		t.Error("a resumed create minted a key or recreated the VM")
	}
}

func TestCreateWithoutHubRegistrationPrintsFallback(t *testing.T) {
	c := testCfg(t)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{"instance": true}}
	r.handle = func(cl call, f *fakeRunner) (string, string, error) {
		if cl.name == "gcloud" && strings.Contains(cl.line(), "describe") && !strings.Contains(cl.line(), "instances") {
			return "{}", "", nil
		}
		if cl.name == "ssh" && cl.args[len(cl.args)-1] == "bash -s" {
			return "", "", nil // comebye never registers
		}
		return gceFake(w, c)(cl, f)
	}
	e, out, _ := newEnv(t, c, w, r)
	e.ReadSecret = func(string) (string, error) { return secretKey, nil } // no OAuth file: prompt
	err := e.create(context.Background(), "w1", createOpts{})
	if err == nil || !strings.Contains(out.String(), "comebye peer add w1 https://w1.tail1.ts.net --tag tag:worker") {
		t.Errorf("err=%v out=%s", err, out)
	}
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), secretKey) {
			t.Error("prompted key reached argv")
		}
	}
}

func TestCreateRefusesBeforeBuildingOnUnusableHubVersion(t *testing.T) {
	c := testCfg(t)
	w := &fakeWorld{hubVersion: "dev", peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{}, handle: func(call, *fakeRunner) (string, string, error) { return "", "", nil }}
	e, _, _ := newEnv(t, c, w, r)
	if err := e.create(context.Background(), "w1", createOpts{}); err == nil {
		t.Fatal("expected refusal")
	}
	if len(r.calls) != 0 {
		t.Errorf("ran %d commands before refusing", len(r.calls))
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	c := testCfg(t)
	r := &fakeRunner{handle: func(call, *fakeRunner) (string, string, error) {
		t.Error("dry run executed a command")
		return "", "", nil
	}}
	e, out, _ := newEnv(t, c, &fakeWorld{}, r)
	e.HTTP = httpFunc(func(*http.Request) (*http.Response, error) {
		t.Error("dry run made a request")
		return reply(500, ""), nil
	})
	e.DryRun = true
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "DRY RUN") || !strings.Contains(out.String(), "--source-ranges=203.0.113.7/32") {
		t.Errorf("plan:\n%s", out)
	}
	if _, err := os.Stat(c.StateDir); err == nil {
		t.Error("dry run created the state dir")
	}
}

func destroyEnv(t *testing.T, status string, input string) (*env, *fakeRunner, *fakeWorld, *bytes.Buffer) {
	c := testCfg(t)
	writeOAuth(t, c)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{"w1": true},
		devices: []tsDevice{{ID: "d1", Hostname: "w1", Name: "w1.tail1.ts.net", Tags: []string{"tag:worker"}, ConnectedToControl: true},
			{ID: "d2", Hostname: "w1", Name: "someone-elses"}}}
	r := &fakeRunner{state: map[string]bool{"instance": true}}
	inst := strings.Replace(instJSON, "RUNNING", status, 1)
	r.handle = func(cl call, f *fakeRunner) (string, string, error) {
		if cl.name == "gcloud" && strings.Contains(cl.line(), "instances describe") {
			return inst, "", nil
		}
		if cl.name == "gcloud" && strings.Contains(cl.line(), "disks describe") {
			return "{}", "", nil
		}
		return gceFake(w, c)(cl, f)
	}
	e, out, _ := newEnv(t, c, w, r)
	e.In = strings.NewReader(input)
	return e, r, w, out
}

func TestDestroyNeedsTypedName(t *testing.T) {
	e, r, w, _ := destroyEnv(t, "RUNNING", "yes\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{}); err == nil {
		t.Fatal("proceeded without the name")
	}
	if len(r.calls) > 1 || len(w.deleted) != 0 || len(w.peerRemoved) != 0 {
		t.Errorf("changed things: %v %v %v", r.calls, w.deleted, w.peerRemoved)
	}
}

func TestDestroyKeepsDataDiskByDefault(t *testing.T) {
	e, r, w, out := destroyEnv(t, "RUNNING", "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !r.saw("instances delete w1") || r.saw("disks delete") {
		t.Error("expected instance delete only")
	}
	if !r.saw("tailscale logout") || !r.saw("auth logout") {
		t.Error("no logout on the VM")
	}
	if len(w.deleted) != 1 || w.deleted[0] != "d1" {
		t.Errorf("tailnet devices deleted: %v (must be only the tagged, named one)", w.deleted)
	}
	if len(w.peerRemoved) != 1 || w.peerRemoved[0] != "w1" {
		t.Errorf("peer entry: %v", w.peerRemoved)
	}
}

func TestDestroyWithDataDiskAndStoppedVM(t *testing.T) {
	e, r, _, _ := destroyEnv(t, "TERMINATED", "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{DeleteDataDisk: true}); err != nil {
		t.Fatal(err)
	}
	if !r.saw("disks delete w1-data") {
		t.Error("data disk not deleted")
	}
	for _, cl := range r.calls {
		if cl.name == "ssh" {
			t.Error("tried ssh to a stopped VM")
		}
	}
}

func TestRefusesToManageForeignInstance(t *testing.T) {
	c := testCfg(t)
	w := &fakeWorld{peers: map[string]bool{}}
	r := &fakeRunner{handle: func(cl call, _ *fakeRunner) (string, string, error) {
		return strings.Replace(instJSON, `"managed-by":"comebye-worker"`, `"env":"prod"`, 1), "", nil
	}}
	e, _, _ := newEnv(t, c, w, r)
	e.In = strings.NewReader("w1\n")
	for name, f := range map[string]func() error{
		"stop":    func() error { return e.stop(context.Background(), "w1") },
		"start":   func() error { return e.start(context.Background(), "w1") },
		"destroy": func() error { return e.destroy(context.Background(), "w1", destroyOpts{}) },
		"login":   func() error { return e.login(context.Background(), "w1", "claude") },
	} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r.saw("stop") || r.saw("delete") || r.saw("start w1") {
		t.Error("a mutating command ran")
	}
}

func TestLoginRunsOnTheVM(t *testing.T) {
	c := testCfg(t)
	w := &fakeWorld{peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{"instance": true}}
	r.handle = gceFake(w, c)
	e, _, _ := newEnv(t, c, w, r)
	if err := e.login(context.Background(), "w1", "codex"); err != nil {
		t.Fatal(err)
	}
	last := r.calls[len(r.calls)-1]
	if last.name != "ssh" || !strings.Contains(last.line(), "-t") || !strings.HasSuffix(last.line(), "codex login") {
		t.Errorf("login call: %s", last.line())
	}
	if err := e.login(context.Background(), "w1", "pi"); err == nil {
		t.Error("accepted an unknown agent")
	}
}

func TestTailscaleErrorsDoNotEchoBodies(t *testing.T) {
	c := testCfg(t)
	ts := &tsClient{base: "https://api.test", id: "i", secret: "s", http: httpFunc(func(r *http.Request) (*http.Response, error) {
		return reply(401, "client_secret=s tskey-REFLECTED"), nil
	})}
	_, _, err := ts.MintKey(context.Background(), c.Tag, "d")
	if err == nil || strings.Contains(err.Error(), "REFLECTED") || strings.Contains(err.Error(), "client_secret") {
		t.Errorf("error leaks body: %v", err)
	}
}

// ---- group 3: the shell scripts ----

func TestScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	c := testCfg(t)
	rs, _ := rootScript(c)
	us, _ := userScript(c, "w1", "v0.3.1")
	for name, s := range map[string]string{"startup": startupScript, "root": rs, "user": us, "idle": idleCheck} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script: bash -n: %v\n%s", name, err, out)
		}
	}
}

func TestScriptsHoldNoSecrets(t *testing.T) {
	for name, s := range map[string]string{"startup": startupScript, "root": bootstrapRoot, "user": bootstrapUser, "idle": idleCheck} {
		for _, bad := range []string{"tskey-", "auth-key", "client_secret"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s script mentions %q", name, bad)
			}
		}
	}
}

func TestAuthKeyFile(t *testing.T) {
	c := testCfg(t)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{}, handle: func(call, *fakeRunner) (string, string, error) { return "", "", nil }}
	e, out, _ := newEnv(t, c, w, r)
	ctx := context.Background()

	// a 0600 file is used
	os.WriteFile(c.TSAuthKeyFile, []byte(secretKey+"\n"), 0o600)
	k, used, err := e.authKey(ctx, "w1")
	if err != nil || k != secretKey || used != c.TSAuthKeyFile {
		t.Fatalf("file key: %q %q %v", k, used, err)
	}
	// group-readable is refused, and so is junk
	os.Chmod(c.TSAuthKeyFile, 0o644)
	if _, _, err := e.authKey(ctx, "w1"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("mode: %v", err)
	}
	os.WriteFile(c.TSAuthKeyFile, []byte("not a key"), 0o600)
	if _, _, err := e.authKey(ctx, "w1"); err == nil {
		t.Error("junk accepted")
	}
	os.Remove(c.TSAuthKeyFile)

	// no file, no terminal: it announces and waits, then takes the file when it appears
	waits := 0
	e.Sleep = func(int) {
		waits++
		if waits == 3 {
			os.WriteFile(c.TSAuthKeyFile, []byte(secretKey), 0o600)
		}
	}
	k, used, err = e.authKey(ctx, "w1")
	if err != nil || k != secretKey || used == "" || waits != 3 {
		t.Fatalf("wait: %q %q %v waits=%d", k, used, err, waits)
	}
	if !strings.Contains(out.String(), "WAITING FOR A TAILSCALE KEY") || strings.Contains(out.String(), secretKey) {
		t.Errorf("announcement/leak:\n%s", out)
	}
}

func TestAuthKeyFileIsDeletedAfterEnrol(t *testing.T) {
	c := testCfg(t)
	os.WriteFile(c.TSAuthKeyFile, []byte(secretKey), 0o600)
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{}}
	r.handle = gceFake(w, c)
	e, _, _ := newEnv(t, c, w, r)
	if err := e.enroll(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.TSAuthKeyFile); !os.IsNotExist(err) {
		t.Error("the key file is still on disk")
	}
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), secretKey) {
			t.Error("key in argv")
		}
	}
}

func TestRootBootstrapInstallsTheUserBus(t *testing.T) {
	// Debian cloud images lack dbus-user-session; without it `systemctl --user` has no bus.
	if !strings.Contains(bootstrapRoot, "dbus-user-session") || !strings.Contains(bootstrapRoot, `user@$uid.service`) {
		t.Error("the root bootstrap must install dbus-user-session and restart the user manager")
	}
}

func TestHerdrServiceHasTheAgentPath(t *testing.T) {
	// Found on the first real run: panes opened by herdr could not find claude.
	if !strings.Contains(bootstrapUser, "Environment=PATH=%h/.local/bin:") {
		t.Error("herdr.service must put ~/.local/bin on the PATH its panes inherit")
	}
}
