package main

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

// The test runner: Go, Node and Chrome on a worker VM, so the heavy suites
// (go test ./internal/..., tools/ci/browser.sh) run there and not on the
// desktop host. Every tool is pinned by version and sha256 here, and the
// bootstrap refuses a download that does not match. Bump them on purpose:
// TestTestRunnerPins checks Go against go.mod's toolchain line and Node's
// major version against CI's.

//go:embed scripts/bootstrap-testrunner.sh
var bootstrapTestRunner string

//go:embed scripts/testcheck.sh
var testCheck string

type testRunnerPins struct {
	GoVersion, GoSHA256         string // go.dev/dl go<v>.linux-amd64.tar.gz
	NodeVersion, NodeSHA256     string // nodejs.org/dist node-v<v>-linux-x64.tar.xz (SHASUMS256.txt)
	ChromeVersion, ChromeSHA256 string // Chrome for Testing linux64/chrome-linux64.zip
}

var testPins = testRunnerPins{
	GoVersion:     "1.26.11",
	GoSHA256:      "9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec",
	NodeVersion:   "22.23.5",
	NodeSHA256:    "cdeb8670a0c81f51b4dc4ac61919f9b1ad917b304bf8221eb939570663d240f8",
	ChromeVersion: "158.0.8127.36",
	ChromeSHA256:  "dba9cc838e1ce269c33ea68170881127bd15d078fc3ca5891c90802819a4d7c7",
}

var (
	semverRe  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	chromeRe  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repoDirRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func (p testRunnerPins) validate() error {
	for _, c := range []struct {
		name, v string
		re      *regexp.Regexp
	}{
		{"go version", p.GoVersion, semverRe}, {"go sha256", p.GoSHA256, sha256Re},
		{"node version", p.NodeVersion, semverRe}, {"node sha256", p.NodeSHA256, sha256Re},
		{"chrome version", p.ChromeVersion, chromeRe}, {"chrome sha256", p.ChromeSHA256, sha256Re},
	} {
		if !c.re.MatchString(c.v) {
			return fmt.Errorf("test runner pin %s %q is malformed", c.name, c.v)
		}
	}
	return nil
}

// testRunnerScript is the root script that installs the pinned toolchain.
func testRunnerScript(p testRunnerPins) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	return withVars(bootstrapTestRunner, [][2]string{
		{"WK_GO_VERSION", p.GoVersion}, {"WK_GO_SHA256", p.GoSHA256},
		{"WK_NODE_VERSION", p.NodeVersion}, {"WK_NODE_SHA256", p.NodeSHA256},
		{"WK_CHROME_VERSION", p.ChromeVersion}, {"WK_CHROME_SHA256", p.ChromeSHA256},
	})
}

// testCheckScript is the user script that proves the worker runs the tests,
// in /data/code/REPO. quick checks the versions only.
func testCheckScript(p testRunnerPins, repo string, quick bool) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if !repoDirRe.MatchString(repo) || repo == "." || repo == ".." {
		return "", fmt.Errorf("bad repository name %q (a directory under /data/code)", repo)
	}
	q := "0"
	if quick {
		q = "1"
	}
	return withVars(testCheck, [][2]string{
		{"WK_GO_VERSION", p.GoVersion}, {"WK_NODE_VERSION", p.NodeVersion}, {"WK_CHROME_VERSION", p.ChromeVersion},
		{"WK_REPO_DIR", "/data/code/" + repo}, {"WK_QUICK", q},
	})
}

// installTestRunner runs the toolchain script as root on a VM that the
// provisioner can already reach over ssh.
func (e *env) installTestRunner(ctx context.Context, name string) error {
	s, err := testRunnerScript(testPins)
	if err != nil {
		return err
	}
	out, err := e.sh(ctx, name, "sudo bash -s", []byte(s))
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			e.printf("  %s\n", l)
		}
	}
	if err != nil {
		return fmt.Errorf("test runner bootstrap: %w", err)
	}
	return nil
}

// toolchain installs (or upgrades to) the pinned test toolchain on an existing
// VM, without the rest of create: no tailnet step, no service restart, /data untouched.
func (e *env) toolchain(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if e.Cfg.Provider == "openshell" {
		return fmt.Errorf("the test runner is for VM workers; a sandbox's image carries its own tools")
	}
	if err := e.owned(ctx, name); err != nil {
		return err
	}
	e.printf("%s: Go %s, Node %s, Chrome for Testing %s (sha256 pinned), Chrome's libraries and fonts\n",
		name, testPins.GoVersion, testPins.NodeVersion, testPins.ChromeVersion)
	return e.installTestRunner(ctx, name)
}

type testCheckOpts struct {
	Repo  string // under /data/code; default comebye
	Quick bool
}

// testcheck proves a VM can run go test ./internal/... and the browser smoke
// suite, streaming the script's output.
func (e *env) testcheck(ctx context.Context, name string, o testCheckOpts) error {
	if err := checkName(name); err != nil {
		return err
	}
	if e.Cfg.Provider == "openshell" {
		return fmt.Errorf("the test runner is for VM workers")
	}
	if o.Repo == "" {
		o.Repo = "comebye"
	}
	s, err := testCheckScript(testPins, o.Repo, o.Quick)
	if err != nil {
		return err
	}
	if err := e.owned(ctx, name); err != nil {
		return err
	}
	if err := e.tr().Stream(ctx, name, "bash -c "+shellQuote(s), e.Out); err != nil {
		return fmt.Errorf("testcheck on %s failed: %w", name, err)
	}
	return nil
}
