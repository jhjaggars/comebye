package transcript

import (
	"os"
	"testing"
)

// TestMain points the OpenCode mirror at a scratch directory, so no test in
// this package reads or writes the user's real
// ~/.local/state/comebye/opencode-sessions.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "opencode-mirror-")
	if err != nil {
		panic(err)
	}
	os.Setenv("COMEBYE_OPENCODE_MIRROR_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
