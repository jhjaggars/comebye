package scratch

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

type fx struct {
	t    *testing.T
	tmp  string
	proc string
	kill []int
	o    Options
}

func newFx(t *testing.T) *fx {
	t.Helper()
	d := t.TempDir()
	f := &fx{t: t, tmp: filepath.Join(d, "tmp"), proc: filepath.Join(d, "proc")}
	for _, p := range []string{f.tmp, f.proc} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.o = Options{TmpDir: f.tmp, ProcDir: f.proc, UID: os.Getuid(), StopWait: 200 * time.Millisecond}
	f.o.Kill = func(pid int, sig syscall.Signal) error {
		f.kill = append(f.kill, pid)
		os.RemoveAll(filepath.Join(f.proc, strconv.Itoa(pid))) // the process exits
		return nil
	}
	return f
}

func (f *fx) file(path string, size int, age time.Duration) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		f.t.Fatal(err)
	}
	old := time.Now().Add(-age)
	for p := path; p != f.tmp && len(p) > len(f.tmp); p = filepath.Dir(p) {
		os.Chtimes(p, old, old)
	}
	return path
}

// scratchpad makes the scratchpad project of worktree wt with one session file.
func (f *fx) scratchpad(wt string, size int, age time.Duration) string {
	dir := filepath.Join(f.tmp, "claude-"+strconv.Itoa(os.Getuid()), ProjectDir(wt))
	f.file(filepath.Join(dir, "sess1", "scratchpad", "shot.png"), size, age)
	return dir
}

// proc fakes a process in /proc.
func (f *fx) process(pid int, comm, cwd string, args ...string) {
	d := filepath.Join(f.proc, strconv.Itoa(pid))
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644)
	cmd := ""
	for _, a := range args {
		cmd += a + "\x00"
	}
	os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmd), 0o644)
	os.Symlink(cwd, filepath.Join(d, "cwd"))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestProjectDir(t *testing.T) {
	got := ProjectDir("/home/jhjaggars/.worktrees/comebye/fix-scratch-cleanup")
	if want := "-home-jhjaggars--worktrees-comebye-fix-scratch-cleanup"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCleanWorktreeRemovesScratchpadAndStopsOrphan(t *testing.T) {
	f := newFx(t)
	wt := "/work/gone/wt"
	dir := f.scratchpad(wt, 4096, time.Hour)
	other := f.scratchpad("/work/other/wt", 10, time.Hour)
	f.process(101, "devserver", wt+" (deleted)")                                       // cwd in the removed worktree
	f.process(102, "chrome", "/", "--user-data-dir="+filepath.Join(dir, "sess1", "p")) // profile in the scratchpad
	f.process(103, "bash", wt)                                                         // a shell: protected
	f.process(104, "devserver", "/work/other/wt")                                      // unrelated
	rep := f.clean(wt, nil)
	if exists(dir) {
		t.Fatal("scratchpad of the removed worktree is still there")
	}
	if !exists(other) {
		t.Fatal("another worktree's scratchpad was removed")
	}
	if rep.Bytes != 4096 || len(rep.Removed) != 1 {
		t.Fatalf("report %+v", rep)
	}
	if len(f.kill) != 2 || f.kill[0] != 101 && f.kill[1] != 101 {
		t.Fatalf("killed %v, want 101 and 102 only", f.kill)
	}
	for _, p := range f.kill {
		if p == 103 || p == 104 {
			t.Fatalf("stopped pid %d that must stay", p)
		}
	}
}

func (f *fx) clean(wt string, live []string) Report { return CleanWorktree(f.o, wt, live) }

func TestCleanWorktreeKeepsLiveAgent(t *testing.T) {
	f := newFx(t)
	wt := "/work/live/wt"
	dir := f.scratchpad(wt, 100, time.Hour)
	f.process(201, "devserver", wt)
	rep := f.clean(wt, []string{wt + "/sub"})
	if !exists(dir) || len(f.kill) != 0 || len(rep.Removed) != 0 || len(rep.Kept) != 1 {
		t.Fatalf("a live agent's worktree was touched: %+v kill=%v", rep, f.kill)
	}
}

func TestCleanWorktreeDryRun(t *testing.T) {
	f := newFx(t)
	f.o.DryRun = true
	wt := "/work/gone/wt"
	dir := f.scratchpad(wt, 2048, time.Hour)
	f.process(301, "devserver", wt)
	rep := f.clean(wt, nil)
	if !exists(dir) || len(f.kill) != 0 {
		t.Fatal("dry run changed something")
	}
	if !rep.DryRun || rep.Bytes != 2048 || len(rep.Stopped) != 1 || len(rep.Removed) != 1 {
		t.Fatalf("dry run should still report: %+v", rep)
	}
}

func TestPrune(t *testing.T) {
	f := newFx(t)
	liveWT := "/work/live/wt"
	liveDir := f.scratchpad(liveWT, 100, 100*time.Hour)           // worktree exists: kept however old
	goneDir := f.scratchpad("/work/gone/wt", 5000, 100*time.Hour) // gone and idle: pruned
	freshDir := f.scratchpad("/work/fresh/wt", 100, time.Minute)  // gone but just touched: kept
	oldTmp := f.file(filepath.Join(f.tmp, "tmp.AbC123", "x"), 3000, 72*time.Hour)
	oldHL := f.file(filepath.Join(f.tmp, "hl.Zz9", "y"), 1000, 72*time.Hour)
	newTmp := f.file(filepath.Join(f.tmp, "tmp.NEW", "x"), 10, time.Hour)
	notMine := f.file(filepath.Join(f.tmp, "keepme", "x"), 10, 500*time.Hour) // not a tmp.* name
	busyTmp := f.file(filepath.Join(f.tmp, "tmp.busy", "x"), 10, 500*time.Hour)
	f.process(401, "devserver", goneDir+"/sess1") // orphan inside a pruned scratchpad
	f.process(402, "bash", filepath.Dir(busyTmp)) // a shell sits in tmp.busy

	rep := Prune(f.o, PruneParams{Live: []string{liveWT}})
	for _, p := range []string{liveDir, freshDir, newTmp, notMine, busyTmp} {
		if !exists(p) {
			t.Errorf("%s was removed but must stay", p)
		}
	}
	for _, p := range []string{goneDir, filepath.Dir(oldTmp), filepath.Dir(oldHL)} {
		if exists(p) {
			t.Errorf("%s should have been pruned", p)
		}
	}
	if rep.Bytes != 5000+3000+1000 {
		t.Errorf("bytes = %d", rep.Bytes)
	}
	if len(f.kill) != 1 || f.kill[0] != 401 {
		t.Errorf("killed %v, want only 401", f.kill)
	}
}

func TestPruneDryRunTouchesNothing(t *testing.T) {
	f := newFx(t)
	f.o.DryRun = true
	gone := f.scratchpad("/work/gone/wt", 700, 100*time.Hour)
	old := f.file(filepath.Join(f.tmp, "tmp.old", "x"), 300, 100*time.Hour)
	f.process(501, "devserver", gone)
	rep := Prune(f.o, PruneParams{})
	if !exists(gone) || !exists(old) || len(f.kill) != 0 {
		t.Fatal("dry run changed something")
	}
	if rep.Bytes != 1000 || len(rep.Removed) != 2 || len(rep.Stopped) != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestPruneNeverTouchesOtherUsers(t *testing.T) {
	f := newFx(t)
	f.o.UID = os.Getuid() + 12345 // every file here now belongs to "someone else"
	gone := f.scratchpad("/work/gone/wt", 700, 100*time.Hour)
	old := f.file(filepath.Join(f.tmp, "tmp.old", "x"), 300, 100*time.Hour)
	f.process(601, "devserver", gone)
	rep := Prune(f.o, PruneParams{})
	if !exists(gone) || !exists(old) || len(f.kill) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("touched another user's files: %+v", rep)
	}
}

func TestUsageOf(t *testing.T) {
	u := UsageOf(t.TempDir())
	if u == nil || u.Total <= 0 || u.UsedPct < 0 || u.UsedPct > 100 {
		t.Fatalf("usage %+v", u)
	}
}
