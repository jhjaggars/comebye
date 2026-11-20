// Package signin signs an agent CLI in on this host without a shell: it runs
// the CLI's own login command on a pseudo-terminal, hands the link it prints
// to the caller, takes the pasted one-time code and writes it to the
// terminal, then asks the CLI whether it is signed in.
//
// The credential is written by the CLI itself, on this host. The code is
// never kept: it lives in the request, one local variable and the write to
// the terminal, and the child's output after it is discarded.
package signin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// States of a sign-in.
const (
	StateWaitingCode = "waiting_code"
	StateChecking    = "checking"
	StateSignedIn    = "signed_in"
	StateFailed      = "failed"
)

// Reasons a sign-in failed: a short fixed list, never the child's output.
const (
	ReasonTimedOut       = "timed_out"
	ReasonCancelled      = "cancelled"
	ReasonCodeRejected   = "code_rejected"
	ReasonNotInstalled   = "not_installed"
	ReasonDidNotFinish   = "did_not_finish"
	ReasonUnexpectedText = "unexpected_output"
)

var reasonText = map[string]string{
	ReasonTimedOut:       "timed out waiting for the code",
	ReasonCancelled:      "cancelled",
	ReasonCodeRejected:   "the code was not accepted",
	ReasonNotInstalled:   "the agent is not installed on this host",
	ReasonDidNotFinish:   "the sign-in did not finish",
	ReasonUnexpectedText: "unexpected output from the agent's sign-in",
}

// Errors the routes map to statuses.
var (
	ErrUnknownAgent = errors.New("no sign-in for that agent")
	ErrNotFound     = errors.New("no such sign-in")
	ErrNotWaiting   = errors.New("the sign-in is not waiting for a code")
	ErrNoCodeInput  = errors.New("this agent's sign-in takes no code")
	ErrTooMany      = errors.New("too many sign-ins are running on this host")
)

// BusyError is returned by Start when a sign-in of that agent is running.
type BusyError struct{ Session Snapshot }

func (e *BusyError) Error() string { return "a sign-in for this agent is already running" }

// FailError is returned by Start when the child could not be started or its
// link was not usable. Reason is one of the fixed reasons.
type FailError struct{ Reason string }

func (e *FailError) Error() string { return reasonText[e.Reason] }

// Snapshot is what a caller sees of a sign-in. It holds no code.
type Snapshot struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	State     string `json:"state"`
	URL       string `json:"url,omitempty"`
	CodeInput bool   `json:"code_input"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	Method    string `json:"method,omitempty"`
}

// Config sets the manager's limits (zero values take the defaults).
type Config struct {
	// CodeWait is how long a sign-in waits for a code (10 minutes).
	CodeWait time.Duration
	// ResultWait is how long after the code the process may take (30 s).
	ResultWait time.Duration
	// URLWait is how long the process has to print its link (30 s).
	URLWait time.Duration
	// KeepFor is how long an ended sign-in is kept for reading (2 minutes).
	KeepFor time.Duration
	// MaxRunning is the most sign-ins running at once on the host (2).
	MaxRunning int
	// Drivers by agent name (default: claude).
	Drivers map[string]Driver
	// Env is the child's environment; default Environ().
	Env func() []string
}

// Environ is the child's environment: PATH (with ~/.local/bin first), HOME,
// TERM=dumb and a BROWSER that does nothing. Nothing else of comebye's own
// environment is passed on: no token, no key, no display.
func Environ() []string {
	home := os.Getenv("HOME")
	path := os.Getenv("PATH")
	if home != "" {
		path = filepath.Join(home, ".local", "bin") + string(os.PathListSeparator) + path
	}
	return []string{"PATH=" + path, "HOME=" + home, "TERM=dumb", "BROWSER=/bin/false"}
}

type session struct {
	id    string
	d     Driver
	snap  Snapshot
	ended bool // terminal state set

	master *os.File
	cmd    *exec.Cmd
	pid    int
	done   chan struct{} // closed when the process has been reaped
	timer  *time.Timer   // the current deadline
}

// Manager holds the sign-ins of one host, in memory only.
type Manager struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*session
	wg       sync.WaitGroup
	closed   bool
}

// NewManager makes a manager.
func NewManager(cfg Config) *Manager {
	if cfg.CodeWait <= 0 {
		cfg.CodeWait = 10 * time.Minute
	}
	if cfg.ResultWait <= 0 {
		cfg.ResultWait = 30 * time.Second
	}
	if cfg.URLWait <= 0 {
		cfg.URLWait = 30 * time.Second
	}
	if cfg.KeepFor <= 0 {
		cfg.KeepFor = 2 * time.Minute
	}
	if cfg.MaxRunning <= 0 {
		cfg.MaxRunning = 2
	}
	if cfg.Drivers == nil {
		cfg.Drivers = map[string]Driver{Claude.Name: Claude}
	}
	if cfg.Env == nil {
		cfg.Env = Environ
	}
	return &Manager{cfg: cfg, sessions: map[string]*session{}}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "si_" + hex.EncodeToString(b)
}

// Start begins a sign-in of agent and returns once its link is known. If one
// of that agent is running, it returns a *BusyError with its state.
func (m *Manager) Start(ctx context.Context, agent string) (Snapshot, error) {
	d, ok := m.cfg.Drivers[agent]
	if !ok {
		return Snapshot{}, ErrUnknownAgent
	}
	env := m.cfg.Env()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Snapshot{}, ErrTooMany
	}
	running := 0
	for _, s := range m.sessions {
		if s.ended {
			continue
		}
		running++
		if s.d.Name == agent {
			snap := s.snap
			m.mu.Unlock()
			return snap, &BusyError{Session: snap}
		}
	}
	if running >= m.cfg.MaxRunning {
		m.mu.Unlock()
		return Snapshot{}, ErrTooMany
	}
	s := &session{id: newID(), d: d, done: make(chan struct{})}
	s.snap = Snapshot{ID: s.id, Agent: agent, CodeInput: d.CodeInput}
	// Reserve the slot before the process starts.
	m.sessions[s.id] = s
	m.mu.Unlock()

	fail := func(reason string) (Snapshot, error) {
		m.mu.Lock()
		delete(m.sessions, s.id)
		s.ended = true
		m.mu.Unlock()
		m.stop(s)
		return Snapshot{}, &FailError{Reason: reason}
	}

	path, err := lookPath(d.Command[0], env)
	if err != nil {
		return fail(ReasonNotInstalled)
	}
	master, slave, err := openPTY()
	if err != nil {
		return fail(ReasonDidNotFinish)
	}
	cmd := exec.Command(path, d.Command[1:]...)
	cmd.Env = env
	if err := start(cmd, slave); err != nil {
		master.Close()
		slave.Close()
		return fail(ReasonDidNotFinish)
	}
	slave.Close()
	s.master, s.cmd, s.pid = master, cmd, cmd.Process.Pid

	urls := make(chan string, 1)
	m.wg.Add(2)
	go func() { // the only reader of the terminal
		defer m.wg.Done()
		readLink(master, urls)
	}()
	go func() { // reaps the process, then checks the result
		defer m.wg.Done()
		_ = cmd.Wait()
		close(s.done)
		m.exited(s, env)
	}()

	select {
	case u := <-urls:
		if !d.URLAllowed(u) {
			return fail(ReasonUnexpectedText)
		}
		m.mu.Lock()
		if s.ended {
			m.mu.Unlock()
			return fail(ReasonDidNotFinish)
		}
		s.snap.State = StateWaitingCode
		s.snap.URL = u
		s.timer = time.AfterFunc(m.cfg.CodeWait, func() { m.finish(s, StateFailed, ReasonTimedOut, "") })
		snap := s.snap
		m.mu.Unlock()
		return snap, nil
	case <-s.done:
		return fail(ReasonDidNotFinish)
	case <-time.After(m.cfg.URLWait):
		return fail(ReasonDidNotFinish)
	case <-ctx.Done():
		return fail(ReasonCancelled)
	}
}

// readLink reads the terminal until a link is complete, sends it, then
// discards everything else (this includes the echo of the code).
func readLink(r io.Reader, out chan<- string) {
	var acc []byte
	buf := make([]byte, 4096)
	found := false
	for {
		n, err := r.Read(buf)
		if n > 0 && !found {
			acc = append(acc, buf[:n]...)
			if len(acc) > 64<<10 {
				acc = acc[len(acc)-(64<<10):]
			}
			if u, ok := FindURL(string(acc)); ok {
				found = true
				acc = nil
				out <- u
			}
		}
		if err != nil {
			return
		}
	}
}

// exited runs when the process has ended: ask the agent whether it is signed
// in.
func (m *Manager) exited(s *session, env []string) {
	m.mu.Lock()
	if s.ended {
		m.mu.Unlock()
		m.stop(s)
		return
	}
	gotCode := s.snap.State == StateChecking
	m.mu.Unlock()
	in, method := s.d.probe(context.Background(), env)
	switch {
	case in:
		m.finish(s, StateSignedIn, "", method)
	case gotCode:
		m.finish(s, StateFailed, ReasonCodeRejected, "")
	default:
		m.finish(s, StateFailed, ReasonDidNotFinish, "")
	}
}

// finish sets the terminal state once, ends the process group and schedules
// the session's deletion.
func (m *Manager) finish(s *session, state, reason, method string) {
	m.mu.Lock()
	if s.ended {
		m.mu.Unlock()
		return
	}
	s.ended = true
	s.snap.State, s.snap.Reason, s.snap.Method = state, reason, method
	s.snap.Message = reasonText[reason]
	s.snap.URL = ""
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(m.cfg.KeepFor, func() {
		m.mu.Lock()
		delete(m.sessions, s.id)
		m.mu.Unlock()
	})
	m.mu.Unlock()
	m.stop(s)
}

// stop ends the process group and closes the terminal.
func (m *Manager) stop(s *session) {
	if s.pid > 0 {
		killGroup(s.pid)
	}
	if s.master != nil {
		s.master.Close()
	}
}

// Get returns the sign-in's state.
func (m *Manager) Get(id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	return s.snap, nil
}

// SendCode writes the pasted code and Enter to the terminal. The code is not
// kept.
func (m *Manager) SendCode(id, code string) (Snapshot, error) {
	if !ValidCode(code) {
		return Snapshot{}, ErrBadCode
	}
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return Snapshot{}, ErrNotFound
	}
	if !s.d.CodeInput {
		m.mu.Unlock()
		return Snapshot{}, ErrNoCodeInput
	}
	if s.ended || s.snap.State != StateWaitingCode {
		m.mu.Unlock()
		return Snapshot{}, ErrNotWaiting
	}
	s.snap.State = StateChecking
	s.snap.URL = ""
	s.timer.Stop()
	s.timer = time.AfterFunc(m.cfg.ResultWait, func() { m.finish(s, StateFailed, ReasonDidNotFinish, "") })
	snap := s.snap
	master := s.master
	m.mu.Unlock()

	line := make([]byte, 0, len(code)+1)
	line = append(append(line, code...), '\r')
	_, err := master.Write(line)
	for i := range line {
		line[i] = 0
	}
	if err != nil {
		m.finish(s, StateFailed, ReasonDidNotFinish, "")
		snap, _ = m.Get(id)
	}
	return snap, nil
}

// Cancel ends a running sign-in (it fails with reason cancelled) or removes
// an ended one.
func (m *Manager) Cancel(id string) (Snapshot, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return Snapshot{}, ErrNotFound
	}
	ended := s.ended
	m.mu.Unlock()
	if !ended {
		m.finish(s, StateFailed, ReasonCancelled, "")
		<-s.done
	} else {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.snap, nil
}

// Close ends every sign-in (the host is shutting down) and waits for the
// processes to be reaped.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var all []*session
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		m.finish(s, StateFailed, ReasonCancelled, "")
	}
	m.wg.Wait()
}

// lookPath finds name on the PATH of env.
func lookPath(name string, env []string) (string, error) {
	path := ""
	for _, e := range env {
		if len(e) > 5 && e[:5] == "PATH=" {
			path = e[5:]
		}
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}
