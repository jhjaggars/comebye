package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The pins follow the repository: Go is go.mod's toolchain and CI's
// GO_VERSION, Node's major is CI's node-version. Bumping one without the
// other fails here.
func TestTestRunnerPins(t *testing.T) {
	if err := testPins.validate(); err != nil {
		t.Fatal(err)
	}
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^toolchain go(\S+)$`).FindSubmatch(gomod); m == nil || string(m[1]) != testPins.GoVersion {
		t.Errorf("go.mod's toolchain is %q, the test runner pins Go %s", m, testPins.GoVersion)
	}
	ci, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`GO_VERSION: "([^"]+)"`).FindSubmatch(ci); m == nil || string(m[1]) != testPins.GoVersion {
		t.Errorf("CI's GO_VERSION is %q, the test runner pins Go %s", m, testPins.GoVersion)
	}
	majors := regexp.MustCompile(`node-version: (\S+)`).FindAllSubmatch(ci, -1)
	if len(majors) == 0 {
		t.Fatal("no node-version in ci.yml")
	}
	nodeMajor, _, _ := strings.Cut(testPins.NodeVersion, ".")
	for _, m := range majors {
		if string(m[1]) != nodeMajor {
			t.Errorf("CI uses Node %s, the test runner pins %s", m[1], testPins.NodeVersion)
		}
	}
}

func TestTestRunnerScriptCarriesEveryPin(t *testing.T) {
	s, err := testRunnerScript(testPins)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"WK_GO_VERSION='" + testPins.GoVersion + "'", "WK_GO_SHA256='" + testPins.GoSHA256 + "'",
		"WK_NODE_VERSION='" + testPins.NodeVersion + "'", "WK_NODE_SHA256='" + testPins.NodeSHA256 + "'",
		"WK_CHROME_VERSION='" + testPins.ChromeVersion + "'", "WK_CHROME_SHA256='" + testPins.ChromeSHA256 + "'",
		"https://go.dev/dl/go$WK_GO_VERSION.linux-amd64.tar.gz",
		"https://nodejs.org/dist/v$WK_NODE_VERSION/node-v$WK_NODE_VERSION-linux-x64.tar.xz",
		"https://storage.googleapis.com/chrome-for-testing-public/$WK_CHROME_VERSION/linux64/chrome-linux64.zip",
		"sha256sum -c", "/usr/local/bin/google-chrome", "ln -sfn /usr/local/bin/google-chrome /usr/bin/google-chrome", "/usr/local/bin/go", "/usr/local/bin/npm",
		"python3", "gnupg", "sqlite3", "fonts-liberation", "libnss3", "libasound2",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the test runner script lacks %q", want)
		}
	}
	// Each download is checked before it is unpacked: the fetch helper checks, and nothing else downloads.
	if n := strings.Count(s, "curl -"); n != 1 {
		t.Errorf("%d curl calls; only fetch() may download", n)
	}
	for tool, unpack := range map[string]string{"go.tgz": `tar -xzf "$tmp/go.tgz"`, "node.txz": `tar -xJf "$tmp/node.txz"`, "chrome.zip": `unzip -q "$tmp/chrome.zip"`} {
		f := regexp.MustCompile(`fetch "[^\n]*" "\$tmp/` + regexp.QuoteMeta(tool) + `"`).FindStringIndex(s)
		u := strings.Index(s, unpack)
		if f == nil || u < 0 || f[0] > u {
			t.Errorf("%s is not fetched (and checked) before it is unpacked", tool)
		}
	}
	// A malformed pin never reaches a script.
	for name, mut := range map[string]func(*testRunnerPins){
		"sha":     func(p *testRunnerPins) { p.GoSHA256 = "abc" },
		"shell":   func(p *testRunnerPins) { p.NodeVersion = "22.1.0;reboot" },
		"chrome":  func(p *testRunnerPins) { p.ChromeVersion = "stable" },
		"upper":   func(p *testRunnerPins) { p.ChromeSHA256 = strings.ToUpper(testPins.ChromeSHA256) },
		"missing": func(p *testRunnerPins) { p.NodeSHA256 = "" },
	} {
		p := testPins
		mut(&p)
		if _, err := testRunnerScript(p); err == nil {
			t.Errorf("%s: a malformed pin was accepted", name)
		}
	}
}

func TestTestCheckScript(t *testing.T) {
	s, err := testCheckScript(testPins, "comebye", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WK_REPO_DIR='/data/code/comebye'", "WK_QUICK='0'", "WK_GO_VERSION='" + testPins.GoVersion + "'",
		"WK_CHROME_VERSION='" + testPins.ChromeVersion + "'", "go test -count=1 -p 4 ./internal/...", "tools/ci/browser.sh smoke", "GOTOOLCHAIN=local"} {
		if !strings.Contains(s, want) {
			t.Errorf("the check lacks %q", want)
		}
	}
	if q, _ := testCheckScript(testPins, "comebye", true); !strings.Contains(q, "WK_QUICK='1'") {
		t.Error("--quick is not passed")
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../etc", "x y", "a;b"} {
		if _, err := testCheckScript(testPins, bad, false); err == nil {
			t.Errorf("repository %q accepted", bad)
		}
	}
}

func TestTestRunnerScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	rs, _ := testRunnerScript(testPins)
	cs, _ := testCheckScript(testPins, "comebye", false)
	for name, s := range map[string]string{"testrunner": rs, "testcheck": cs} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: bash -n: %v\n%s", name, err, out)
		}
	}
	// The quick check runs anywhere: with no matching tools it fails on the versions, before any test.
	cmd := exec.Command(bash, "-c", strings.Replace(cs, "WK_QUICK='0'", "WK_QUICK='1'", 1))
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/nonexistent", "XDG_RUNTIME_DIR=/nonexistent"}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "the toolchain is not as pinned") || strings.Contains(string(out), "go test") {
		t.Errorf("the check passed without the tools, or ran tests: %v\n%s", err, out)
	}
}

// On by default for kubevirt, off for the clouds, refused for sandboxes; the
// config file decides when it says.
func TestTestRunnerConfig(t *testing.T) {
	if c := kubeCfg(t); !c.TestRunner {
		t.Error("kubevirt: the test runner is off by default")
	}
	c := testCfg(t)
	c.finish()
	if c.TestRunner {
		t.Error("gcp: the test runner is on by default")
	}
	base := "ts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=https://h.t.ts.net\n"
	kube := "provider=kubevirt\n" + base
	gcp := "project=comebye-test-0001\nzone=us-east1-b\nssh_source_cidr=198.51.100.1/32\n" + base
	for _, tc := range []struct {
		body string
		want bool
		bad  bool
	}{
		{kube, true, false},
		{kube + "test_runner=false\n", false, false},
		{gcp, false, false},
		{gcp + "test_runner=true\n", true, false},
		{kube + "test_runner=yes\n", false, true},
		{"provider=openshell\nopenshell_gateway=g\nopenshell_workspace=w\nsandbox_image=r/i:{version}\nsandbox_policy=/p\n" + base + "test_runner=true\n", false, true},
	} {
		p := filepath.Join(t.TempDir(), "config")
		os.WriteFile(p, []byte(tc.body+"state_dir="+t.TempDir()+"\n"), 0o600)
		c, err := LoadConfig(p)
		if tc.bad {
			if err == nil {
				t.Errorf("accepted:\n%s", tc.body)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v\n%s", err, tc.body)
			continue
		}
		if c.TestRunner != tc.want {
			t.Errorf("test_runner %v, want %v:\n%s", c.TestRunner, tc.want, tc.body)
		}
	}
}

// create installs the toolchain right after the root bootstrap (before the
// tailnet), and only when test_runner is on.
func TestCreateInstallsTheTestRunnerWhenOn(t *testing.T) {
	for _, on := range []bool{true, false} {
		c := testCfg(t)
		c.TestRunner = on
		writeOAuth(t, c)
		w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
		r := &fakeRunner{state: map[string]bool{"instance": true}}
		r.handle = gceFake(w, c)
		e, out, _ := newEnv(t, c, w, r)
		if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
			t.Fatalf("create: %v\n%s", err, out)
		}
		trI, rootI, upI := -1, -1, -1
		for i, cl := range r.calls {
			if cl.name != "ssh" {
				continue
			}
			remote := cl.args[len(cl.args)-1]
			switch {
			case remote == "sudo bash -s" && strings.Contains(cl.stdin, "WK_GO_SHA256="):
				trI = i
			case remote == "sudo bash -s" && rootI < 0:
				rootI = i
			case strings.HasPrefix(remote, "sudo tailscale up"):
				upI = i
			}
		}
		if on && !(rootI >= 0 && rootI < trI && trI < upI) {
			t.Errorf("on: root %d, test runner %d, tailscale up %d", rootI, trI, upI)
		}
		if !on && trI >= 0 {
			t.Error("off: the test runner was installed")
		}
		if on != strings.Contains(out.String(), "test runner: Go "+testPins.GoVersion) {
			t.Errorf("on=%v: output:\n%s", on, out)
		}
	}
}

func TestDryRunShowsTheTestRunner(t *testing.T) {
	c := testCfg(t)
	c.TestRunner = true
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{}}
	r.handle = gceFake(w, c)
	e, out, _ := newEnv(t, c, w, r)
	e.DryRun = true
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "test runner (test_runner=true): Go "+testPins.GoVersion) {
		t.Errorf("the plan does not show the test runner:\n%s", out)
	}
}

func TestToolchainAndTestcheckCommands(t *testing.T) {
	c := testCfg(t)
	w := &fakeWorld{peers: map[string]bool{}}
	r := &fakeRunner{state: map[string]bool{"instance": true}}
	r.handle = gceFake(w, c)
	e, _, _ := newEnv(t, c, w, r)
	ctx := context.Background()
	if err := e.toolchain(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	last := r.calls[len(r.calls)-1]
	if last.name != "ssh" || last.args[len(last.args)-1] != "sudo bash -s" || !strings.Contains(last.stdin, "WK_CHROME_SHA256=") {
		t.Errorf("toolchain ran %s", last.line())
	}
	if err := e.testcheck(ctx, "w1", testCheckOpts{Quick: true}); err != nil {
		t.Fatal(err)
	}
	last = r.calls[len(r.calls)-1]
	remote := last.args[len(last.args)-1]
	if last.name != "ssh" || !strings.HasPrefix(remote, "bash -c '") || !strings.Contains(remote, "WK_QUICK='\\''1'\\''") ||
		!strings.Contains(remote, "/data/code/comebye") || strings.Contains(remote, "sudo") {
		t.Errorf("testcheck ran %s", remote)
	}
	if err := e.testcheck(ctx, "w1", testCheckOpts{Repo: "../x"}); err == nil {
		t.Error("a repository outside /data/code was accepted")
	}
	// a foreign or missing VM is refused before anything runs on it
	n := len(r.calls)
	r.state["instance"] = false
	if e.toolchain(ctx, "w1") == nil || e.testcheck(ctx, "w1", testCheckOpts{}) == nil {
		t.Error("ran on a VM that does not exist")
	}
	for _, cl := range r.calls[n:] {
		if cl.name == "ssh" {
			t.Errorf("ssh to a missing VM: %s", cl.line())
		}
	}
	// flags
	var out, errb bytes.Buffer
	e2 := &env{Out: &out, Err: &errb, Run: &fakeRunner{handle: func(c call, f *fakeRunner) (string, string, error) { return "", "", nil }}}
	cfgPath := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfgPath, []byte("project=comebye-test-0001\nzone=us-east1-b\nssh_source_cidr=198.51.100.1/32\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=https://h.t.ts.net\nstate_dir="+t.TempDir()+"\n"), 0o600)
	if code := run([]string{"-config", cfgPath, "status", "--quick"}, e2); code != 2 {
		t.Errorf("--quick on status: exit %d", code)
	}
	if code := run([]string{"-config", cfgPath, "toolchain"}, e2); code != 2 {
		t.Errorf("toolchain without a name: exit %d", code)
	}
	if code := run([]string{"-config", cfgPath, "testcheck", "a", "b"}, e2); code != 2 {
		t.Errorf("testcheck with two names: exit %d", code)
	}
}
