package gitfetch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func gitDo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// world is a bare origin, a clone with two worktrees, and a log that the origin's
// upload-pack writes to each time it serves a fetch: "start NS" and "end NS".
type world struct {
	origin, clone, wt1, wt2, log string
}

func newWorld(t *testing.T, serve time.Duration) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := t.TempDir()
	w := &world{origin: filepath.Join(d, "origin.git"), clone: filepath.Join(d, "clone"), wt1: filepath.Join(d, "wt1"), wt2: filepath.Join(d, "wt2"), log: filepath.Join(d, "fetches.log")}
	gitDo(t, d, "init", "-q", "--bare", "-b", "main", w.origin)
	gitDo(t, d, "clone", "-q", w.origin, w.clone)
	gitDo(t, w.clone, "checkout", "-q", "-b", "main")
	gitDo(t, w.clone, "commit", "-q", "--allow-empty", "-m", "one")
	gitDo(t, w.clone, "push", "-q", "origin", "main")
	gitDo(t, w.clone, "worktree", "add", "-q", "-b", "a", w.wt1)
	gitDo(t, w.clone, "worktree", "add", "-q", "-b", "b", w.wt2)
	script := filepath.Join(d, "upload-pack")
	body := fmt.Sprintf("#!/bin/sh\necho start $(date +%%s%%N) >> %q\nsleep %s\necho end $(date +%%s%%N) >> %q\nexec git-upload-pack \"$@\"\n", w.log, strconv.FormatFloat(serve.Seconds(), 'f', 3, 64), w.log)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	gitDo(t, w.clone, "config", "remote.origin.uploadpack", script)
	return w
}

// served reads the log: how many fetches reached origin, and whether any two
// were being served at the same time.
func (w *world) served(t *testing.T) (n int, overlap bool) {
	t.Helper()
	b, err := os.ReadFile(w.log)
	if os.IsNotExist(err) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	type ev struct {
		at    int64
		start bool
	}
	var evs []ev
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l)
		at, _ := strconv.ParseInt(f[1], 10, 64)
		evs = append(evs, ev{at, f[0] == "start"})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].at < evs[j].at })
	open := 0
	for _, e := range evs {
		if e.start {
			n++
			open++
			if open > 1 {
				overlap = true
			}
		} else {
			open--
		}
	}
	return n, overlap
}

// The checks of two worktrees of one repository, arriving together, share one fetch.
func TestConcurrentChecksOfOneRepositoryShareOneFetch(t *testing.T) {
	w := newWorld(t, 200*time.Millisecond)
	f := &Fetcher{Fresh: 30 * time.Second, LogEvery: 24 * time.Hour, Wait: 10 * time.Second, Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	results := make([]Result, 8)
	for i := range results {
		dir := w.wt1
		if i%2 == 1 {
			dir = w.wt2
		}
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			results[i] = f.Fetch(context.Background(), dir)
		}(i, dir)
	}
	wg.Wait()
	if n, _ := w.served(t); n != 1 {
		t.Errorf("%d fetches", n)
	}
	for _, r := range results {
		if r.Failed() || r.Busy || r.NoRepo || r.Pending || !r.Fetched {
			t.Errorf("%+v", r)
		}
	}
	// The next round, after Fresh, fetches again.
	f.Now = func() time.Time { return time.Now().Add(time.Minute) }
	f.Fetch(context.Background(), w.wt1)
	if n, _ := w.served(t); n != 2 {
		t.Errorf("%d fetches", n)
	}
}

// One-off fetches (the archive's) and the server's, from any worktree, never run
// into one object store at once.
func TestFetchesOfOneRepositoryNeverOverlap(t *testing.T) {
	w := newWorld(t, 150*time.Millisecond)
	server := &Fetcher{Fresh: 0, LogEvery: 24 * time.Hour, Wait: 30 * time.Second, Timeout: 30 * time.Second}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); Once(context.Background(), w.wt1, 30*time.Second, 30*time.Second) }()
		go func() { defer wg.Done(); server.Fetch(context.Background(), w.wt2) }()
	}
	wg.Wait()
	n, overlap := w.served(t)
	if overlap {
		t.Error("two fetches were served at the same time")
	}
	if n < 7 {
		t.Errorf("only %d fetches: the test did not exercise concurrency", n)
	}
}

// The lock holds against another process: a program that holds it makes a fetch
// wait, and give up as busy if it holds too long.
func TestTheLockHoldsAcrossProcesses(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock")
	}
	w := newWorld(t, 0)
	common, err := CommonDir(context.Background(), w.wt1)
	if err != nil {
		t.Fatal(err)
	}
	hold := exec.Command("flock", filepath.Join(common, LockName), "sleep", "1")
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer hold.Wait()
	time.Sleep(200 * time.Millisecond) // let it take the lock
	if r := Once(context.Background(), w.wt2, 200*time.Millisecond, 10*time.Second); !r.Busy {
		t.Errorf("did not wait for the other process: %+v", r)
	}
	if n, _ := w.served(t); n != 0 {
		t.Errorf("fetched while the lock was held: %d", n)
	}
	t0 := time.Now()
	if r := Once(context.Background(), w.wt2, 5*time.Second, 10*time.Second); !r.Fetched {
		t.Errorf("%+v", r)
	}
	if time.Since(t0) < 300*time.Millisecond {
		t.Error("did not wait")
	}
}

// A repository that keeps failing is logged once a day, not every round; a new
// reason is logged at once, and a success starts afresh.
func TestAFailureIsLoggedOnceADay(t *testing.T) {
	w := newWorld(t, 0)
	var logs []string
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	f := &Fetcher{Fresh: 30 * time.Second, LogEvery: 24 * time.Hour, Wait: time.Second, Timeout: 10 * time.Second,
		Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		Now:  func() time.Time { return now }}
	gitDo(t, w.clone, "remote", "set-url", "origin", "https://someone:s3cr3t@archive-test.invalid/org/widgets.git")
	for i := 0; i < 12; i++ { // an hour of 5-minute rounds
		if r := f.Fetch(context.Background(), w.wt1); !r.Failed() {
			t.Fatalf("%+v", r)
		}
		now = now.Add(5 * time.Minute)
	}
	if len(logs) != 1 || logs[0] != "git fetch failed in clone (could not resolve host archive-test.invalid); merge checks use the last-known origin" {
		t.Fatalf("%q", logs)
	}
	now = now.Add(23 * time.Hour)
	f.Fetch(context.Background(), w.wt2)
	if len(logs) != 2 {
		t.Errorf("a day later: %q", logs)
	}
	// Another reason is news.
	now = now.Add(5 * time.Minute)
	gitDo(t, w.clone, "remote", "set-url", "origin", filepath.Join(filepath.Dir(w.origin), "gone.git"))
	f.Fetch(context.Background(), w.wt1)
	if len(logs) != 3 || !strings.Contains(logs[2], "(no repository at origin on a local path)") {
		t.Errorf("%q", logs)
	}
	// Fixed, then broken again: logged again at once.
	now = now.Add(5 * time.Minute)
	gitDo(t, w.clone, "remote", "set-url", "origin", w.origin)
	if r := f.Fetch(context.Background(), w.wt1); !r.Fetched {
		t.Fatalf("%+v", r)
	}
	now = now.Add(5 * time.Minute)
	gitDo(t, w.clone, "remote", "set-url", "origin", filepath.Join(filepath.Dir(w.origin), "gone.git"))
	f.Fetch(context.Background(), w.wt1)
	if len(logs) != 4 {
		t.Errorf("%q", logs)
	}
	for _, l := range logs {
		if strings.Contains(l, "s3cr3t") || strings.Contains(l, "someone") || strings.Contains(l, "org/widgets") || strings.Contains(l, "\n") {
			t.Errorf("leaks the URL or spans lines: %q", l)
		}
	}
}

func TestNoOriginIsNothingToDo(t *testing.T) {
	w := newWorld(t, 0)
	gitDo(t, w.clone, "remote", "remove", "origin")
	if r := Once(context.Background(), w.wt1, time.Second, time.Second); !r.NoRepo || r.Failed() {
		t.Errorf("%+v", r)
	}
	if r := Once(context.Background(), t.TempDir(), time.Second, time.Second); !r.NoRepo {
		t.Errorf("%+v", r)
	}
}
