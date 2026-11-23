package server

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestMain points the OpenCode mirror at a scratch directory, so no test in
// this package reads or writes the user's real
// ~/.local/state/comebye/opencode-sessions, and stands in for gh and k3d
// (see noExternalTools).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "opencode-mirror-")
	if err != nil {
		panic(err)
	}
	os.Setenv("COMEBYE_OPENCODE_MIRROR_DIR", dir)
	noExternalTools()
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// errNoGH is what every gh and k3d call answers in tests.
var errNoGH = errors.New("gh and k3d are not run in tests")

// noExternalTools replaces every probe that runs gh or k3d with one that
// fails the way an unsigned-in gh does on CI, so a test's outcome never
// depends on whether gh is signed in where it runs, or on the real state of
// a PR or a cluster. A test that needs an answer stubs the probe itself
// (stubProbes, ghMergedPRs, ghOpenPRs, ghRuns, userLogin).
func noExternalTools() {
	probeGHPR = func(context.Context, string, int) (*probeResult, error) { return nil, errNoGH }
	probeGHIssue = func(context.Context, string, int) (*probeResult, error) { return nil, errNoGH }
	probeK3D = func(context.Context, string) (bool, error) { return false, errNoGH }
	ghMergedPRs = func(context.Context, string, string) ([]ghPR, error) { return nil, errNoGH }
	ghOpenPRs = func(context.Context, string, string) ([]ghPR, error) { return nil, errNoGH }
	ghRuns = func(context.Context, string, string) ([]byte, error) { return nil, errNoGH }
	userLogin = func(context.Context) string { return "" }
}
