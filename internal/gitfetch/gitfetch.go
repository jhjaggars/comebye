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
//
// A fetch that runs too long is stopped with SIGTERM, so that git removes its
// temporary pack, and killed only if it is still running after TermGrace.
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

// DefaultTimeout bounds one fetch. An ordinary fetch of the largest repository
// here takes seconds; a first fetch after weeks, or one that brings a large
// branch, can take a minute or two. Stopping it sooner wastes the transfer, and
// a stopped fetch is what leaves partial state behind.
const DefaultTimeout = 5 * time.Minute

// TermGrace is how long a fetch gets to exit after SIGTERM before SIGKILL.
var TermGrace = 15 * time.Second

// gitBin is the git binary; a test can put a fake in its place.
var gitBin = "git"

// Result is what one fetch did.
type Result struct {
	Common  string // the repository's common git directory; "" if dir is not in one
	NoRepo  bool   // dir is not in a repository, or it has no origin: nothing to do
	Fetched bool   // a fetch ran and succeeded
	Busy    bool   // another fetch of this repository held the lock for the whole wait
	Recent  bool   // skipped: this process fetched it a moment ago (Fetcher only)
	Pending bool   // still running when the caller stopped waiting (Fetcher only)
	Reason  string // why the fetch failed, in a few words; "" if it did not fail
	// Damaged: git says the repository has missing or damaged objects. BrokenRef
	// is the ref it named, if any.
	Damaged   bool
	BrokenRef string
}

// Failed reports a fetch that ran and failed.
func (r Result) Failed() bool { return r.Reason != "" }

// Command is a git command in dir: English messages (Reason reads them), no
// prompts, and SIGTERM then SIGKILL when ctx ends.
func Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, gitBin, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = TermGrace
	return cmd
}

// CommonDir is the repository's common git directory, shared by its worktrees.
func CommonDir(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if e, err := filepath.EvalSymlinks(p); err == nil {
		p = e
	}
	return p, nil
}

// Lock takes the repository's fetch lock, waiting at most wait. The returned
// function releases it. ErrBusy means another holder kept it the whole time.
func Lock(ctx context.Context, common string, wait time.Duration) (func(), error) {
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
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ErrBusy: the lock stayed taken for the whole wait.
var ErrBusy = errors.New("busy")

// Once fetches origin into the repository that holds dir, under the repository's
// lock: it waits up to wait for another fetch to finish, and the fetch itself
// runs at most timeout. Remote-tracking refs of branches deleted upstream go.
func Once(ctx context.Context, dir string, wait, timeout time.Duration) Result {
	common, err := CommonDir(ctx, dir)
	if err != nil {
		return Result{NoRepo: true}
	}
	return once(ctx, dir, common, wait, timeout)
}

func once(ctx context.Context, dir, common string, wait, timeout time.Duration) Result {
	res := Result{Common: common}
	url, err := Command(ctx, dir, "remote", "get-url", "origin").Output()
	if err != nil {
		res.NoRepo = true
		return res
	}
	unlock, err := Lock(ctx, common, wait)
	if errors.Is(err, ErrBusy) {
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
	// --prune: a remote-tracking ref of a branch deleted upstream would otherwise
	// stay, pointing at objects nothing on the remote keeps any more.
	cmd := Command(fctx, dir, "fetch", "--quiet", "--no-tags", "--prune", "origin")
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		res.Reason = Reason(errb.String(), RemoteHost(string(url)), fctx.Err() == context.DeadlineExceeded, timeout)
		res.Damaged = IsDamage(errb.String())
		res.BrokenRef = BrokenRef(errb.String())
		return res
	}
	res.Fetched = true
	return res
}

// Fetcher is for a long-running process that fetches the same repositories again
// and again. A fetch runs in the background, bounded by Timeout; a caller waits
// for it at most Wait and then goes on with the refs it has. Callers that arrive
// while a fetch of their repository is running wait for that one, a fetch within
// Fresh counts for the next caller too, and a failure is logged at most once per
// LogEvery for each repository, or when its reason changes. Health, if set, hears
// about every result.
type Fetcher struct {
	Fresh    time.Duration
	LogEvery time.Duration
	Wait     time.Duration // how long a caller waits for the fetch
	LockWait time.Duration // how long the fetch waits for another process's; 0 is Timeout
	Timeout  time.Duration
	Logf     func(format string, args ...any)
	Now      func() time.Time
	Health   *Health

	mu    sync.Mutex
	repos map[string]*repoState
}

type repoState struct {
	mu       sync.Mutex
	inflight chan struct{}
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
	if r.inflight == nil && !r.last.IsZero() && f.now().Sub(r.last) < f.Fresh {
		res := r.result
		r.mu.Unlock()
		res.Recent = true
		return res
	}
	if r.inflight == nil {
		done := make(chan struct{})
		r.inflight = done
		go f.run(r, dir, common, done)
	}
	done := r.inflight
	r.mu.Unlock()
	select {
	case <-done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.result
	case <-ctx.Done():
	case <-time.After(f.Wait):
	}
	return Result{Common: common, Pending: true}
}

func (f *Fetcher) run(r *repoState, dir, common string, done chan struct{}) {
	wait := f.LockWait
	if wait <= 0 {
		wait = f.Timeout
	}
	// Not the caller's context: a fetch stopped halfway is worse than one that
	// finishes after nobody is waiting.
	res := once(context.Background(), dir, common, wait, f.Timeout)
	if f.Health != nil && !res.Busy && !res.NoRepo {
		f.Health.Fetched(common, res)
	}
	r.mu.Lock()
	r.inflight = nil
	if !res.Busy { // another process's fetch serves this round; try again next time
		r.last, r.result = f.now(), res
		switch {
		case res.Failed() && f.Logf != nil && (res.Reason != r.loggedAs || f.now().Sub(r.logged) >= f.LogEvery):
			f.Logf("git fetch failed in %s (%s); merge checks use the last-known origin", filepath.Base(filepath.Dir(common)), res.Reason)
			r.logged, r.loggedAs = f.now(), res.Reason
		case !res.Failed():
			r.loggedAs = ""
		}
	} else {
		r.result = res
	}
	r.mu.Unlock()
	close(done)
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
	urlRE    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	scpRE    = regexp.MustCompile(`\S+@[^\s:]+:\S+`)
	damageRE = regexp.MustCompile(`(?i)missing object|bad object|did not send all necessary objects|invalid sha1 pointer|broken link from|missing (blob|tree|commit|tag) |unable to read [0-9a-f]{7,}|does not point to a valid object|corrupt|could not read [0-9a-f]{40}`)
	refRE    = regexp.MustCompile(`\b(refs/[A-Za-z0-9._/@+-]+?)(?::|\s+does not point| |$)`)
)

// IsDamage reports whether git's output says the repository has missing or
// damaged objects, as against a network or credentials failure.
func IsDamage(out string) bool { return damageRE.MatchString(out) }

// BrokenRef is the first ref named next to a damage message, if any.
func BrokenRef(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if !damageRE.MatchString(l) {
			continue
		}
		if m := refRE.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

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
	case IsDamage(stderr):
		return "the local repository has missing or damaged objects"
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
