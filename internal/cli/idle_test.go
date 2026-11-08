package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `comebye idle` over a socket path that does not exist: herdr is not
// running, so nothing is working; the first poll starts the clock.
func TestIdleExitCodes(t *testing.T) {
	_, e, out, errb := newFake(t)
	sock := filepath.Join(t.TempDir(), "none.sock")
	// a fresh clock with a limit: not idle yet, exit 1, no error text
	if code := run(e, "idle", "--minutes", "5", "--sock", sock); code != 1 {
		t.Fatalf("code %d, out %q err %q", code, out, errb)
	}
	if !strings.Contains(out.String(), "idle for 0 of 5 min") || errb.Len() != 0 {
		t.Fatalf("out %q err %q", out, errb)
	}
	out.Reset()
	// a limit of 0: idle now, exit 0, JSON
	if code := run(e, "idle", "--minutes", "0", "--sock", sock, "--json"); code != 0 {
		t.Fatalf("code %d, out %q err %q", code, out, errb)
	}
	var r struct {
		Idle    bool     `json:"idle"`
		Minutes int      `json:"minutes"`
		Reasons []string `json:"reasons"`
		Herdr   string   `json:"herdr"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || !r.Idle || r.Minutes != 0 || r.Herdr != "not_running" || r.Reasons == nil {
		t.Fatalf("%v %+v (%s)", err, r, out)
	}
	if _, err := os.Stat(filepath.Join(StateDir(e.Getenv), "idle-stamp")); err != nil {
		t.Fatalf("the clock was not written: %v", err)
	}
	// the environment sets the default limit; usage errors are 2
	if code := run(e, "idle", "--minutes", "-1"); code != 2 {
		t.Fatalf("negative: %d", code)
	}
	if code := run(e, "idle", "extra"); code != 2 {
		t.Fatalf("extra arg: %d", code)
	}
}
