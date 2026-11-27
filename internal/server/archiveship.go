package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The janitor's ship stage (#143, slice 5): what `comebye archive run` and
// `comebye archive index` did on timers outside the binary.
//
//   - Every host with an archive set up copies its idle session files to the
//     archive every shipEvery (`comebye archive run`). A host with no agents
//     (a hub-only hub) has nothing to copy and skips it.
//   - The hub (the host with the ledger) also builds the all-hosts recall
//     index hourly (`comebye archive index --all-hosts --if-changed`), and
//     `comebye recall` on a peer reads that index over the API (recallq.go).
//     Peers only ship: two hosts no longer build the same index.
//   - A host set up with -archive-workers also pulls its worker VMs'
//     sessions hourly (`comebye-worker archive --all`). A worker VM holds no
//     archive key and has no route to the bucket, so it cannot ship its own;
//     the machine that runs the workers pulls them, as it did from its timer.
//
// Each is a child process, niced (nice 10, and idle I/O where ionice exists),
// with a timeout, so a 36-second index run does not starve the server, and a
// hung one is stopped (SIGTERM to its process group, SIGKILL 15 s later).
// They run one at a time under the same flock the old systemd timer took
// ($XDG_RUNTIME_DIR/comebye-archive.lock): while the old timer is still
// installed, a tick that finds it running skips, so the two never overlap.
// The child inherits the lock, so it is held until the child is gone even if
// the server is not.

const (
	shipEvery      = 5 * time.Minute // the old timer's OnCalendar=*:0/5
	indexEvery     = time.Hour       // the hub pod's indexer sidecar
	workersEvery   = time.Hour       // comebye-worker-archive.timer
	shipFirst      = 2 * time.Minute // let the server and the tailnet come up first
	shipTimeout    = 10 * time.Minute
	indexTimeout   = 20 * time.Minute
	workersTimeout = 30 * time.Minute
	shipKillDelay  = 15 * time.Second
	shipOutputKeep = 4 << 10
)

// ArchiveShip configures the janitor's ship stage. Nil: no archive is set
// up here, and the stage is off.
type ArchiveShip struct {
	// Bin is the comebye binary to run (the server's own).
	Bin string
	// Env is what the child adds to the server's environment where the
	// server's environment has no value: the archive settings the server was
	// given as flags.
	Env []string
	// Workers: also pull the worker VMs' sessions (WorkerBin, default
	// comebye-worker, `archive --all`).
	Workers   bool
	WorkerBin string
	// Lock is the flock file (default $XDG_RUNTIME_DIR/comebye-archive.lock,
	// else comebye-archive.lock in the state directory).
	Lock string
}

// shipJob is one of the stage's child runs.
type shipJob struct {
	what          string
	every, limit  time.Duration
	argv          func() []string
	next          time.Time
	lastErr       string
	lastOK, lastT time.Time
}

type shipper struct {
	s    *Server
	cfg  ArchiveShip
	jobs []*shipJob
	// timeouts and the first wait; tests shorten them.
	first time.Duration

	mu       sync.Mutex
	warned   bool // the old timers' notice was logged
	lockHeld int  // ticks skipped because another run held the lock (tests)
	runs     map[string]int
}

func newShipper(s *Server, c *ArchiveShip) *shipper {
	if c == nil || c.Bin == "" {
		return nil
	}
	sh := &shipper{s: s, cfg: *c, first: shipFirst, runs: map[string]int{}}
	if sh.cfg.Lock == "" {
		sh.cfg.Lock = defaultShipLock(s.cfg.StateDir)
	}
	if sh.cfg.WorkerBin == "" {
		sh.cfg.WorkerBin = "comebye-worker"
	}
	if !s.cfg.HubOnly {
		sh.jobs = append(sh.jobs, &shipJob{what: "ship", every: shipEvery, limit: shipTimeout,
			argv: func() []string { return []string{sh.cfg.Bin, "archive", "run"} }})
	}
	if s.ledger != nil {
		sh.jobs = append(sh.jobs, &shipJob{what: "index", every: indexEvery, limit: indexTimeout, argv: func() []string {
			a := []string{sh.cfg.Bin, "archive", "index", "--all-hosts", "--if-changed"}
			if _, err := exec.LookPath("git"); err != nil {
				a = append(a, "--git=false") // no git here (the hub pod): no "is it on main" check
			}
			return a
		}})
	}
	if sh.cfg.Workers {
		sh.jobs = append(sh.jobs, &shipJob{what: "workers", every: workersEvery, limit: workersTimeout,
			argv: func() []string { return []string{sh.cfg.WorkerBin, "archive", "--all"} }})
	}
	if len(sh.jobs) == 0 {
		return nil
	}
	if s.cfg.TestMode {
		// The dev harness and live checks: seconds, not minutes and hours.
		sh.first = 3 * time.Second
		for _, j := range sh.jobs {
			j.every = map[string]time.Duration{"ship": 5 * time.Second, "index": 10 * time.Second, "workers": 30 * time.Second}[j.what]
		}
	}
	return sh
}

// defaultShipLock is the old timer's lock file (%t/comebye-archive.lock).
func defaultShipLock(stateDir string) string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "comebye-archive.lock")
	}
	return filepath.Join(stateDir, "comebye-archive.lock")
}

// step runs the jobs that are due, one after the other, under the lock.
func (sh *shipper) step(ctx context.Context, now time.Time) {
	sh.warnOldTimers()
	var due []*shipJob
	sh.mu.Lock()
	for _, j := range sh.jobs {
		if j.next.IsZero() {
			j.next = now.Add(sh.first)
		}
		if !now.Before(j.next) {
			due = append(due, j)
		}
	}
	sh.mu.Unlock()
	if len(due) == 0 {
		return
	}
	lock, err := flockTry(sh.cfg.Lock)
	if err != nil {
		sh.mu.Lock()
		sh.lockHeld++
		n := sh.lockHeld
		sh.mu.Unlock()
		if !errors.Is(err, errLockHeld) || n == 1 {
			log.Printf("archive: skipped this round: %v", err)
		}
		return // the next tick tries again
	}
	defer lock.Close()
	for _, j := range due {
		if ctx.Err() != nil {
			return
		}
		t0 := time.Now()
		out, err := sh.runChild(ctx, lock, j.limit, j.argv())
		sh.mu.Lock()
		j.next = time.Now().Add(j.every)
		j.lastT = time.Now()
		sh.runs[j.what]++
		if err != nil {
			j.lastErr = err.Error()
		} else {
			j.lastErr, j.lastOK = "", time.Now()
		}
		sh.mu.Unlock()
		if err != nil {
			log.Printf("archive: %s failed after %s: %v%s", j.what, time.Since(t0).Round(time.Second), err, tailText(out))
		} else if line := shipSummary(j.what, out); line != "" {
			log.Printf("archive: %s: %s (%s)", j.what, line, time.Since(t0).Round(time.Second))
		}
	}
}

var errLockHeld = errors.New("another archive run holds the lock (the old comebye-archive.timer, or a run by hand)")

// flockTry takes the lock without waiting.
func flockTry(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLockHeld
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

// runChild runs argv niced, in its own process group, holding lock, for at
// most limit. It returns the end of its output.
func (sh *shipper) runChild(ctx context.Context, lock *os.File, limit time.Duration, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if p, err := exec.LookPath("ionice"); err == nil {
		argv = append([]string{p, "-c", "3"}, argv...)
	}
	if p, err := exec.LookPath("nice"); err == nil {
		argv = append([]string{p, "-n", "10"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = sh.env()
	cmd.Stdin = nil
	out := &tailBuffer{max: shipOutputKeep}
	cmd.Stdout, cmd.Stderr = out, out // one writer: exec copies both through one pipe
	cmd.ExtraFiles = []*os.File{lock} // fd 3: the lock stays held while the child lives
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = shipKillDelay
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // whatever outlived the SIGTERM (sqlite3)
		}
		err = fmt.Errorf("stopped at its %s time limit", limit)
	}
	return []byte(out.String()), err
}

// env is the server's environment with the archive settings filled in
// where it has none.
func (sh *shipper) env() []string {
	env := os.Environ()
	have := map[string]bool{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" {
			have[k] = true
		}
	}
	for _, kv := range sh.cfg.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" && !have[k] {
			env = append(env, kv)
		}
	}
	return env
}

// warnOldTimers logs once, when the outside schedulers this stage replaces
// are still installed.
func (sh *shipper) warnOldTimers() {
	sh.mu.Lock()
	if sh.warned {
		sh.mu.Unlock()
		return
	}
	sh.warned = true
	sh.mu.Unlock()
	for _, t := range oldArchiveTimers(userUnitDirs()) {
		log.Printf("archive: %s is still installed (%s). The server now does its work itself (the janitor's ship stage), under the same lock; disable it: systemctl --user disable --now %s", t.unit, t.path, t.unit)
	}
}

type oldTimer struct{ unit, path string }

// userUnitDirs are where a user's systemd units are installed.
func userUnitDirs() []string {
	d := os.Getenv("XDG_CONFIG_HOME")
	if d == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		d = filepath.Join(h, ".config")
	}
	return []string{filepath.Join(d, "systemd", "user")}
}

// oldArchiveTimers finds the old timers in dirs.
func oldArchiveTimers(dirs []string) []oldTimer {
	var out []oldTimer
	for _, unit := range []string{"comebye-archive.timer", "comebye-worker-archive.timer"} {
		for _, d := range dirs {
			for _, p := range []string{filepath.Join(d, "timers.target.wants", unit), filepath.Join(d, unit)} {
				if _, err := os.Lstat(p); err == nil {
					out = append(out, oldTimer{unit: unit, path: p})
					goto next
				}
			}
		}
	next:
	}
	return out
}

// shipSummary is the log line for a run that worked: nothing for a copy that
// copied nothing (every 5 minutes); an index run (hourly) always says how it went.
func shipSummary(what string, out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	switch what {
	case "ship":
		var files, updated int
		if n, _ := fmt.Sscanf(last, "%d files looked at, %d updated", &files, &updated); n == 2 && updated == 0 {
			return ""
		}
	}
	return clip(last, 300)
}

func tailText(out []byte) string {
	t := strings.TrimSpace(string(out))
	if t == "" {
		return ""
	}
	if i := strings.LastIndexByte(t[:max(0, len(t)-600)], '\n'); i >= 0 && len(t) > 600 {
		t = t[i+1:]
	}
	return ": " + strings.ReplaceAll(clip(t, 600), "\n", " | ")
}
