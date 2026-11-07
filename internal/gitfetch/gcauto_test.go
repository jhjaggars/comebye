package gitfetch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFetchArgsDisableAutoGC(t *testing.T) {
	a := FetchArgs()
	if !slices.Contains(a, "gc.auto=0") || !slices.Contains(a, "maintenance.auto=false") {
		t.Fatalf("FetchArgs %v lacks the gc.auto=0 / maintenance.auto=false settings", a)
	}
	// the -c options must come before the subcommand to take effect
	if i := slices.Index(a, "fetch"); i < 0 || slices.Index(a, "gc.auto=0") > i {
		t.Fatalf("FetchArgs %v: -c options must precede fetch", a)
	}
}

// With thresholds that would make git gc after any fetch, comebye's fetch must
// not start gc or maintenance (checked in git's own trace of started commands).
func TestFetchDoesNotStartGC(t *testing.T) {
	w := newWorld(t, 0)
	gitDo(t, w.clone, "config", "gc.auto", "1")
	gitDo(t, w.clone, "config", "gc.autoPackLimit", "1")
	gitDo(t, w.clone, "config", "fetch.unpackLimit", "1") // keep fetched objects as a pack
	// give the origin something new to fetch
	work := t.TempDir()
	gitDo(t, work, "clone", "-q", w.origin, "w")
	wd := filepath.Join(work, "w")
	os.WriteFile(filepath.Join(wd, "n.txt"), []byte("new"), 0o644)
	gitDo(t, wd, "add", "n.txt")
	gitDo(t, wd, "commit", "-qm", "new")
	gitDo(t, wd, "push", "-q", "origin", "HEAD")

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	res := Once(context.Background(), w.clone, time.Second, time.Minute)
	if !res.Fetched {
		t.Fatalf("fetch did not run: %+v", res)
	}
	b, _ := os.ReadFile(trace)
	if !strings.Contains(string(b), "fetch") {
		t.Fatalf("no trace of the fetch; trace:\n%s", b)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "built-in: git gc") || strings.Contains(l, "built-in: git maintenance") ||
			strings.Contains(l, "run_command: git gc") || strings.Contains(l, "run_command: git maintenance") ||
			strings.Contains(l, "git 'gc'") || strings.Contains(l, "git maintenance") {
			t.Fatalf("fetch started gc or maintenance: %s", l)
		}
	}
}
