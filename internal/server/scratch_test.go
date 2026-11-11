package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/scratch"
)

// scratchpadFor makes the scratchpad Claude would keep for a worktree.
func scratchpadFor(t *testing.T, tmp, wt string, size int, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()), scratch.ProjectDir(wt))
	f := filepath.Join(dir, "sess", "scratchpad", "shot.png")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	for p := f; len(p) > len(tmp); p = filepath.Dir(p) {
		os.Chtimes(p, old, old)
	}
	return dir
}

func TestRemoveCleansTheScratchpad(t *testing.T) {
	s, _, _, wt := removalSetup(t)
	tmp := t.TempDir()
	s.cfg.ScratchDir = tmp
	dir := scratchpadFor(t, tmp, wt, 2048, time.Minute)
	keep := scratchpadFor(t, tmp, "/somewhere/else", 10, time.Minute)
	code, r := removeReq(t, s, `{"agent":"w9:p1","name":"w","remove_worktree":true}`)
	if code != 200 || r["worktree_removed"] != true {
		t.Fatalf("remove = %d %v", code, r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the removed worktree's scratchpad is still there")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("another worktree's scratchpad was removed")
	}
	sc, _ := r["scratch"].(map[string]any)
	if sc == nil || sc["bytes"] != float64(2048) {
		t.Fatalf("the result should say what was removed: %v", r["scratch"])
	}
}

func TestRemoveKeepsTheScratchpadWhenTheWorktreeStays(t *testing.T) {
	s, _, _, wt := removalSetup(t)
	tmp := t.TempDir()
	s.cfg.ScratchDir = tmp
	dir := scratchpadFor(t, tmp, wt, 2048, time.Minute)
	os.WriteFile(wt+"/new.txt", []byte("x"), 0o644) // dirty: the worktree is kept
	code, r := removeReq(t, s, `{"agent":"w9:p1","remove_worktree":true}`)
	if code != 200 || r["worktree_removed"] != false {
		t.Fatalf("remove = %d %v", code, r)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("the scratchpad of a worktree that stays was removed")
	}
	if r["scratch"] != nil {
		t.Fatalf("scratch = %v", r["scratch"])
	}
}

func TestScratchPruneEndpointKeepsLiveAgentsAndHonoursDryRun(t *testing.T) {
	s, _, _, wt := removalSetup(t) // the worker agent is live in wt
	tmp := t.TempDir()
	s.cfg.ScratchDir = tmp
	live := scratchpadFor(t, tmp, wt, 100, 100*time.Hour)
	gone := scratchpadFor(t, tmp, "/work/gone/wt", 5000, 100*time.Hour)
	old := filepath.Join(tmp, "tmp.OLD123", "x")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, make([]byte, 300), 0o644)
	long := time.Now().Add(-100 * time.Hour)
	os.Chtimes(old, long, long)
	os.Chtimes(filepath.Dir(old), long, long)

	call := func(body string) scratch.Report {
		w := do(s, "POST", "/api/scratch", nil, body)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var r struct {
			Report scratch.Report `json:"report"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r.Report
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	if rep := call(`{"dry_run":true}`); !rep.DryRun || rep.Bytes != 5300 || !exists(gone) || !exists(filepath.Dir(old)) {
		t.Fatalf("dry run: %+v", rep)
	}
	rep := call(`{}`)
	if rep.Bytes != 5300 || exists(gone) || exists(filepath.Dir(old)) {
		t.Fatalf("prune: %+v", rep)
	}
	if !exists(live) {
		t.Fatal("the live agent's scratchpad was pruned")
	}
}

func TestScratchDisabledWithoutDir(t *testing.T) {
	s, _, _, _ := removalSetup(t)
	if w := do(s, "POST", "/api/scratch", nil, `{}`); w.Code != 403 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
