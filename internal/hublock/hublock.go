// Package hublock keeps two comebye servers from writing the same state
// directory.
//
// The server holds an exclusive flock(2) on hub.lock in the state directory
// for as long as it runs, and writes into the file who holds it: pid, host,
// the -host-name, listen address and start time, plus a heartbeat time it
// refreshes every few seconds.
//
// Two layers decide who may take the lock:
//
//  1. flock. The kernel drops it when the holder dies, however it dies, so a
//     crashed holder never leaves a lock that needs removing. On a local
//     filesystem (including a block volume such as Longhorn RWO, which only
//     one node mounts) this alone is authoritative.
//  2. The heartbeat lease. flock is advisory and some network filesystems
//     do not carry it between machines. So a process that wins the flock but
//     finds a record from ANOTHER machine whose heartbeat is younger than
//     LeaseTTL still refuses: that holder may be alive on a machine that
//     shares the directory. A record from the same machine is taken over at
//     once (the flock is authoritative there). A heartbeat older than
//     LeaseTTL is stale (crash, or a node that went away) and is taken over,
//     with the previous holder named in the log.
package hublock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// FileName is the lock file in the state directory.
const FileName = "hub.lock"

// Defaults for Options.
const (
	DefaultLeaseTTL  = 20 * time.Second
	DefaultHeartbeat = 5 * time.Second
)

// Holder is what the lock file says about the process that holds it.
type Holder struct {
	PID       int       `json:"pid"`
	Host      string    `json:"host"`      // os.Hostname of the machine (a pod's name in Kubernetes)
	Name      string    `json:"name"`      // the instance's -host-name
	Addr      string    `json:"addr"`      // its listen address
	Started   time.Time `json:"started"`   // when this process took the lock
	Heartbeat time.Time `json:"heartbeat"` // refreshed while it runs
}

func (h Holder) String() string {
	return fmt.Sprintf("pid %d on %s (host-name %s), started %s, last heartbeat %s",
		h.PID, h.Host, h.Name, h.Started.Format(time.RFC3339), h.Heartbeat.Format(time.RFC3339))
}

// HeldError is returned by Acquire when another live process holds the
// directory.
type HeldError struct {
	Path   string
	Holder Holder
	// Why says which layer refused: "flock" or "lease".
	Why string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("state directory is in use by another comebye: %s (%s lock %s). Only one server may use a state directory; stop that one first",
		e.Holder, e.Why, e.Path)
}

// Options tunes Acquire; zero values give the defaults.
type Options struct {
	// Name and Addr are recorded in the lock file.
	Name, Addr string
	// Wait is how long Acquire retries (once a second) when the lock is
	// held; 0 refuses at once.
	Wait time.Duration
	// LeaseTTL is how old another machine's heartbeat may be before its
	// lock counts as stale. Default 20s.
	LeaseTTL time.Duration
	// Heartbeat is how often the holder refreshes its record. Default 5s.
	Heartbeat time.Duration
	// Logf receives the notices (a stale lock taken over). Default: none.
	Logf func(format string, args ...any)

	hostname func() (string, error) // tests
	now      func() time.Time       // tests
}

// Lock is a held state-directory lock.
type Lock struct {
	f      *os.File
	path   string
	opt    Options
	mine   Holder
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error // the last heartbeat failure, nil while healthy
}

// Path is the lock file.
func (l *Lock) Path() string { return l.path }

// Err is nil while the lock is held and its record is being refreshed. A
// non-nil error means the heartbeat is failing (the volume went read-only
// or away): the lease is running out, and another machine may take over.
func (l *Lock) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Acquire takes the lock for dir (created mode 0700 if missing). It returns
// a *HeldError when another live process holds it, after waiting
// Options.Wait.
func Acquire(dir string, opt Options) (*Lock, error) {
	if opt.LeaseTTL <= 0 {
		opt.LeaseTTL = DefaultLeaseTTL
	}
	if opt.Heartbeat <= 0 {
		opt.Heartbeat = DefaultHeartbeat
	}
	if opt.Heartbeat >= opt.LeaseTTL {
		return nil, fmt.Errorf("hublock: heartbeat %v must be shorter than the lease %v", opt.Heartbeat, opt.LeaseTTL)
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	if opt.hostname == nil {
		opt.hostname = os.Hostname
	}
	if opt.now == nil {
		opt.now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	path := filepath.Join(dir, FileName)
	host, _ := opt.hostname()

	deadline := time.Now().Add(opt.Wait)
	for {
		l, err := tryAcquire(path, host, opt)
		var held *HeldError
		if !errors.As(err, &held) {
			return l, err
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		opt.Logf("state directory held by %s; waiting", held.Holder)
		time.Sleep(time.Second)
	}
}

func tryAcquire(path, host string, opt Options) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		prev, _ := readHolder(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &HeldError{Path: path, Holder: prev, Why: "flock"}
		}
		return nil, fmt.Errorf("lock file: %w", err)
	}
	// We hold the flock. Look at who wrote the record last.
	prev, perr := readHolder(f)
	if perr == nil && prev.PID != 0 {
		age := opt.now().Sub(prev.Heartbeat)
		switch {
		case prev.Host != host && age < opt.LeaseTTL:
			// flock says free, but another machine refreshed its record
			// moments ago: the filesystem may not carry flock between
			// machines. Trust the lease.
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			return nil, &HeldError{Path: path, Holder: prev, Why: "lease"}
		case prev.Host != host:
			opt.Logf("state directory: taking over a stale lock from %s (heartbeat %s ago, past the %v lease; it did not shut down cleanly or its node is gone)", prev, age.Round(time.Second), opt.LeaseTTL)
		default:
			opt.Logf("state directory: the previous server on this machine did not shut down cleanly (%s)", prev)
		}
	}
	now := opt.now()
	l := &Lock{f: f, path: path, opt: opt, done: make(chan struct{}),
		mine: Holder{PID: os.Getpid(), Host: host, Name: opt.Name, Addr: opt.Addr, Started: now, Heartbeat: now}}
	if err := l.write(); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock file: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	go l.beat(ctx)
	return l, nil
}

func readHolder(f *os.File) (Holder, error) {
	var h Holder
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return h, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return Holder{}, err
	}
	return h, nil
}

func (l *Lock) write() error {
	b, err := json.Marshal(l.mine)
	if err != nil {
		return err
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.WriteAt(append(b, '\n'), 0); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *Lock) beat(ctx context.Context) {
	defer close(l.done)
	t := time.NewTicker(l.opt.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.mu.Lock()
			l.mine.Heartbeat = l.opt.now()
			err := l.write()
			if err != nil && l.err == nil {
				l.opt.Logf("state directory: heartbeat on %s failing: %v", l.path, err)
			}
			l.err = err
			l.mu.Unlock()
		}
	}
}

// Release stops the heartbeat, marks the record as released and drops the
// lock. The file stays (an empty record means nobody holds it).
func (l *Lock) Release() {
	l.cancel()
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Truncate(0)
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
