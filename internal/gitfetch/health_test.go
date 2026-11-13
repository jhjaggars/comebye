package gitfetch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func headTree(t *testing.T, dir, path string) string {
	t.Helper()
	return gitDo(t, dir, "rev-parse", "HEAD:"+path)
}

// A remote-tracking ref pointing at an object the store does not have: the fetch
// fails, Health lists the repository with that ref and a hint, logs it once, and
// clears it when a fetch goes through after the repair.
func TestABrokenRemoteRefIsReportedAndCleared(t *testing.T) {
	w := newWorld(t, 0)
	var logs []string
	var mu sync.Mutex
	logf := func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }
	changes := 0
	h := &Health{Logf: logf, OnChange: func() { changes++ }}
	f := &Fetcher{Wait: 30 * time.Second, Timeout: 30 * time.Second, LogEvery: 24 * time.Hour, Health: h}
	// What git printed for laharl's OpenShell, where the ref pointed at a commit
	// the store never got. (Over a local path git takes such a ref as up to date
	// and does not fail, so the fetch here is a stand-in with git's own words.)
	fakeGit(t, `echo "fatal: missing object a0b27ba6df8f61c2715e78feda483ea2867894a0 for refs/remotes/origin/pull-request/4789" >&2; exit 128`)
	res := f.Fetch(context.Background(), w.wt1)
	if !res.Failed() || !res.Damaged || res.BrokenRef != "refs/remotes/origin/pull-request/4789" {
		t.Fatalf("%+v", res)
	}
	list := h.List()
	if len(list) != 1 || list[0].Source != "fetch" || list[0].Ref != "refs/remotes/origin/pull-request/4789" || list[0].Repo != "clone" {
		t.Fatalf("%+v", list)
	}
	d := list[0]
	if !strings.Contains(d.Hint, "update-ref -d refs/remotes/origin/pull-request/4789") || strings.Contains(d.Hint, "gc") {
		t.Errorf("hint: %s", d.Hint)
	}
	if len(d.Worktrees) != 3 {
		t.Errorf("worktrees: %v", d.Worktrees)
	}
	// Again, a round later: not logged again, not a change.
	f.Now = func() time.Time { return time.Now().Add(time.Minute) }
	f.Fetch(context.Background(), w.wt2)
	mu.Lock()
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "is damaged") {
			n++
		}
	}
	mu.Unlock()
	if n != 1 || changes != 1 {
		t.Errorf("logged %d times, %d changes: %q", n, changes, logs)
	}
	// Repaired as the hint says: the next fetch, a real one, clears it.
	gitBin = "git"
	f.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if res := f.Fetch(context.Background(), w.wt1); !res.Fetched {
		t.Fatalf("%+v", res)
	}
	if l := h.List(); len(l) != 0 || changes != 2 {
		t.Errorf("%+v, %d changes", l, changes)
	}
}

// fsck finds a tree missing from the store, under the fetch lock; a clean store
// clears it; and what Health knows is saved.
func TestFsckFindsMissingObjects(t *testing.T) {
	w := newWorld(t, 0)
	if err := os.MkdirAll(filepath.Join(w.wt1, "tests", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.wt1, "tests", "a", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, w.wt1, "add", "tests")
	gitDo(t, w.wt1, "commit", "-q", "-m", "a local commit, never pushed")
	common, _ := CommonDir(context.Background(), w.wt1)
	tree := headTree(t, w.wt1, "tests/a")
	path := filepath.Join(h0(t, common), "objects", tree[:2], tree[2:])
	state := filepath.Join(t.TempDir(), "repo-health.json")
	h := OpenHealth(state, nil)
	h.Known(common)
	if err := h.Fsck(context.Background(), common); err != nil || len(h.List()) != 0 {
		t.Fatalf("a clean store: %v %+v", err, h.List())
	}
	loose, err := os.ReadFile(path) // kept to put back below, as `fetch --refetch` would
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := h.Fsck(context.Background(), common); err != nil {
		t.Fatal(err)
	}
	list := h.List()
	if len(list) != 1 || list[0].Source != "fsck" || !strings.Contains(list[0].Reason, "problems with missing or damaged objects") || !strings.Contains(list[0].Hint, "fetch --refetch") {
		t.Fatalf("%+v", list)
	}
	// Saved: a restart still knows.
	if l := OpenHealth(state, nil).List(); len(l) != 1 {
		t.Errorf("not saved: %+v", l)
	}
	// While another process holds the lock, fsck waits rather than run beside it.
	if _, err := exec.LookPath("flock"); err == nil {
		hold := exec.Command("flock", filepath.Join(common, LockName), "sleep", "1")
		if err := hold.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		h.FsckTimeout = 100 * time.Millisecond
		if err := h.Fsck(context.Background(), common); err == nil || !strings.Contains(err.Error(), "busy") {
			t.Errorf("ran beside a fetch: %v", err)
		}
		hold.Wait()
		h.FsckTimeout = 0
	}
	// The object comes back (as `fetch --refetch` would bring it): clear.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The same bytes back, so the test does not depend on git rewriting the tree.
	if err := os.WriteFile(path, loose, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := h.Fsck(context.Background(), common); err != nil {
		t.Fatal(err)
	}
	if l := h.List(); len(l) != 0 {
		t.Errorf("%+v", l)
	}
}

func h0(t *testing.T, common string) string { t.Helper(); return common }

func TestFsckSchedule(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
	h := &Health{Now: func() time.Time { return now }}
	h.Known("/r/a")
	h.Known("/r/b")
	if d := h.Due(); len(d) != 0 {
		t.Fatalf("new repositories wait for the off-peak hours: %v", d)
	}
	now = time.Date(2026, 11, 8, 3, 0, 0, 0, time.Local)
	if d := h.Due(); len(d) != 2 {
		t.Fatalf("%v", d)
	}
	h.repos["/r/a"].LastFsck = now
	h.repos["/r/b"].LastFsck = now.Add(-30 * time.Hour)
	now = now.Add(9 * time.Hour) // midday: only what is two days overdue
	if d := h.Due(); len(d) != 0 {
		t.Fatalf("%v", d)
	}
	now = now.Add(12 * time.Hour)
	if d := h.Due(); len(d) != 1 || d[0] != "/r/b" {
		t.Fatalf("%v", d)
	}
}

// A repository that is gone is forgotten by its next check.
func TestFsckForgetsAGoneRepository(t *testing.T) {
	h := &Health{}
	gone := filepath.Join(t.TempDir(), "gone", ".git")
	h.Known(gone)
	if err := h.Fsck(context.Background(), gone); err != nil {
		t.Fatal(err)
	}
	if len(h.repos) != 0 {
		t.Errorf("%v", h.repos)
	}
}
