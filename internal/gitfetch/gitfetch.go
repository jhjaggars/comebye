// Package gitfetch runs `git fetch origin` for the server's merge checks and the
// archive's git pass, so that two fetches never write into one object store at
// once. Two concurrent fetches into one repository can leave a remote-tracking
// ref pointing at an object that never arrived, after which every fetch fails and
// leaves a temporary pack behind.
//
// The lock is a file in the repository's common git directory, held with flock:
// it lives with the object store it protects, so it holds across processes (the
// server and the archive timer) whatever state directory each of them uses, and
// across every worktree of the repository. The kernel drops it when a process dies.
package gitfetch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LockName is the lock file in a repository's common git directory.
const LockName = "comebye-fetch.lock"

// Result is what one fetch did.
type Result struct {
	Common  string // the repository's common git directory; "" if dir is not in one
	NoRepo  bool   // dir is not in a repository, or it has no origin: nothing to do
	Fetched bool   // a fetch ran and succeeded
	Busy    bool   // another fetch of this repository held the lock for the whole wait
	Recent  bool   // skipped: this process fetched it a moment ago (Fetcher only)
	Reason  string // why the fetch failed, in a few words; "" if it did not fail
}

// Failed reports a fetch that ran and failed.
func (r Result) Failed() bool { return r.Reason != "" }

func git(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// English messages, for Reason; no prompts on a timer.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return cmd
}

// CommonDir is the repository's common git directory, shared by its worktrees.
func CommonDir(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if e, err := filepath.EvalSymlinks(p); err == nil {
		p = e
	}
	return p, nil
}

// lock takes the repository's fetch lock, waiting at most wait. The returned
// function releases it.
func lock(ctx context.Context, common string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(filepath.Join(common, LockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	end := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(end) || ctx.Err() != nil {
			f.Close()
			return nil, errBusy
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

var errBusy = errors.New("busy")

// Once fetches origin into the repository that holds dir, under the repository's
// lock: it waits up to wait for another fetch to finish, and the fetch itself
// runs at most timeout.
func Once(ctx context.Context, dir string, wait, timeout time.Duration) Result {
	common, err := CommonDir(ctx, dir)
	if err != nil {
		return Result{NoRepo: true}
	}
	return once(ctx, dir, common, wait, timeout)
}

func once(ctx context.Context, dir, common string, wait, timeout time.Duration) Result {
	res := Result{Common: common}
	url, err := git(ctx, dir, "remote", "get-url", "origin").Output()
	if err != nil {
		res.NoRepo = true
		return res
	}
	unlock, err := lock(ctx, common, wait)
	if errors.Is(err, errBusy) {
		res.Busy = true
		return res
	}
	if err != nil {
		res.Reason = "could not take the fetch lock"
		return res
	}
	defer unlock()
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := git(fctx, dir, "fetch", "--quiet", "--no-tags", "origin")
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		res.Reason = Reason(errb.String(), RemoteHost(string(url)), fctx.Err() == context.DeadlineExceeded, timeout)
		return res
	}
	res.Fetched = true
	return res
}

// Fetcher is for a long-running process that fetches the same repositories again
// and again: callers that arrive while a fetch of their repository is running
// wait for it and share it, a fetch within Fresh counts for the next caller too,
// and a failure is logged at most once per LogEvery for each repository, or when
// its reason changes.
type Fetcher struct {
	Fresh    time.Duration
	LogEvery time.Duration
	Wait     time.Duration // for another process's fetch to finish
	Timeout  time.Duration
	Logf     func(format string, args ...any)
	Now      func() time.Time

	mu    sync.Mutex
	repos map[string]*repoState
}

type repoState struct {
	mu       sync.Mutex
	last     time.Time
	result   Result
	logged   time.Time
	loggedAs string
}

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Fetcher) state(common string) *repoState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repos == nil {
		f.repos = map[string]*repoState{}
	}
	r := f.repos[common]
	if r == nil {
		r = &repoState{}
		f.repos[common] = r
	}
	return r
}

// Fetch fetches origin into the repository that holds dir, unless this process
// just did.
func (f *Fetcher) Fetch(ctx context.Context, dir string) Result {
	common, err := CommonDir(ctx, dir)
	if err != nil {
		return Result{NoRepo: true}
	}
	r := f.state(common)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.last.IsZero() && f.now().Sub(r.last) < f.Fresh {
		res := r.result
		res.Recent = true
		return res
	}
	res := once(ctx, dir, common, f.Wait, f.Timeout)
	if res.Busy {
		return res // the other process's fetch serves this round; try again next time
	}
	r.last, r.result = f.now(), res
	switch {
	case res.Failed() && f.Logf != nil && (res.Reason != r.loggedAs || f.now().Sub(r.logged) >= f.LogEvery):
		f.Logf("git fetch failed in %s (%s); merge checks use the last-known origin", filepath.Base(filepath.Dir(common)), res.Reason)
		r.logged, r.loggedAs = f.now(), res.Reason
	case !res.Failed():
		r.loggedAs = ""
	}
	return res
}

// RemoteHost is the host part of a remote URL, with no user, password, token or
// path: https://user:token@host/path, ssh://git@host:22/path or git@host:path.
func RemoteHost(url string) string {
	u := strings.TrimSpace(url)
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.IndexAny(u, "/?#"); j >= 0 {
			u = u[:j]
		}
		if j := strings.LastIndex(u, "@"); j >= 0 {
			u = u[j+1:]
		}
		if h, _, ok := strings.Cut(u, ":"); ok && !strings.HasPrefix(u, "[") {
			u = h
		}
		if u == "" {
			return "a local path"
		}
		return u
	}
	// scp-like: [user@]host:path. A local path has no colon before its first slash.
	if c := strings.Index(u, ":"); c > 0 && !strings.Contains(u[:c], "/") {
		h := u[:c]
		if j := strings.LastIndex(h, "@"); j >= 0 {
			h = h[j+1:]
		}
		return h
	}
	return "a local path"
}

var (
	urlRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	scpRE = regexp.MustCompile(`\S+@[^\s:]+:\S+`)
)

// Reason says in a few words why a fetch failed, from git's own stderr. It names
// the remote by host only, so that no user, token or path from a URL reaches a
// log, and it keeps to one short line.
func Reason(stderr, host string, timedOut bool, timeout time.Duration) string {
	if timedOut {
		return "timed out after " + timeout.String()
	}
	low := strings.ToLower(stderr)
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(low, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("could not resolve host"):
		return "could not resolve host " + host
	case has("permission denied (publickey", "host key verification failed"):
		return "ssh refused by " + host
	case has("authentication failed", "terminal prompts disabled", "could not read username", "could not read password"):
		return "no credentials for " + host
	case has("repository not found", "does not appear to be a git repository"):
		return "no repository at origin on " + host
	case has("connection timed out", "connection refused", "network is unreachable", "no route to host", "failed to connect"):
		return "could not connect to " + host
	case has("missing object", "bad object", "did not send all necessary objects", "unable to read", "corrupt"):
		return "the local repository has missing or damaged objects"
	}
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(l)
		for _, p := range []string{"fatal:", "error:"} {
			if strings.HasPrefix(strings.ToLower(l), p) {
				l = strings.TrimSpace(l[len(p):])
				l = urlRE.ReplaceAllString(l, host)
				l = scpRE.ReplaceAllString(l, host)
				l = strings.NewReplacer("'", "", `"`, "").Replace(l)
				l = strings.ToLower(l)
				if len(l) > 100 {
					l = l[:100] + "…"
				}
				return l
			}
		}
	}
	return "git gave no reason"
}
