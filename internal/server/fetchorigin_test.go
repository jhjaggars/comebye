package server

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

	"github.com/jhjaggars/comebye/internal/gitfetch"
)

func fetchGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// The merge checks of two worktrees of one repository, as a round of worker
// checks runs them, fetch origin once between them, and a failing fetch is
// logged once however many rounds see it.
func TestChecksOfOneRepositoryShareOneFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := t.TempDir()
	origin, clone := filepath.Join(d, "origin.git"), filepath.Join(d, "clone")
	fetchGit(t, d, "init", "-q", "--bare", "-b", "main", origin)
	fetchGit(t, d, "clone", "-q", origin, clone)
	fetchGit(t, clone, "checkout", "-q", "-b", "main")
	fetchGit(t, clone, "commit", "-q", "--allow-empty", "-m", "one")
	fetchGit(t, clone, "push", "-q", "origin", "main")
	var wts []string
	for _, b := range []string{"a", "b", "c"} {
		wt := filepath.Join(d, "wt-"+b)
		fetchGit(t, clone, "worktree", "add", "-q", "-b", b, wt)
		wts = append(wts, wt)
	}
	served := filepath.Join(d, "served")
	script := filepath.Join(d, "upload-pack")
	if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\necho x >> %q\nsleep 0.2\nexec git-upload-pack \"$@\"\n", served)), 0o755); err != nil {
		t.Fatal(err)
	}
	fetchGit(t, clone, "config", "remote.origin.uploadpack", script)

	var logs []string
	var mu sync.Mutex
	saved := originFetcher
	t.Cleanup(func() { originFetcher = saved })
	originFetcher = &gitfetch.Fetcher{Fresh: fetchFresh, LogEvery: 24 * time.Hour, Wait: gitTimeout, Timeout: gitTimeout,
		Logf: func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }}

	round := func() []worktreeChecks {
		var wg sync.WaitGroup
		out := make([]worktreeChecks, len(wts))
		for i, wt := range wts {
			wg.Add(1)
			go func(i int, wt string) {
				defer wg.Done()
				p, _ := filepath.EvalSymlinks(wt)
				out[i] = checkWorktree(context.Background(), p, "", "")
			}(i, wt)
		}
		wg.Wait()
		return out
	}
	for _, c := range round() {
		if !c.Exists || c.Error != "" || c.Unmerged != 0 {
			t.Errorf("%+v", c)
		}
	}
	b, _ := os.ReadFile(served)
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Errorf("%d fetches for one round of %d worktrees", n, len(wts))
	}
	// Broken, over several rounds: the checks still answer, and one log line.
	fetchGit(t, clone, "remote", "set-url", "origin", origin+".gone")
	for i := 0; i < 3; i++ {
		originFetcher.Now = func() time.Time { return time.Now().Add(time.Duration(i+1) * time.Minute) }
		for _, c := range round() {
			if !c.Exists || c.Unmerged != 0 {
				t.Errorf("round %d: %+v", i, c)
			}
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "git fetch failed in clone (no repository at origin on a local path)") {
		t.Errorf("%q", logs)
	}
}
