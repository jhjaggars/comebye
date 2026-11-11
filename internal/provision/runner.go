package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Tool runs tools/worker (the comebye-worker binary) as a subprocess for the
// read-only commands. It never passes a request's text to a shell: the
// arguments are built here from validated values, one argv element each.
type Tool struct {
	// Bin is the comebye-worker binary (a name looked up on PATH, or a path).
	Bin string
	// Config is the worker config file (-provision-config).
	Config string
	// Environ is the process environment to filter (default os.Environ).
	Environ func() []string
	// Real turns the real create path on. Off (the default), every job runs
	// `create --dry-run`: the plan and its events, nothing created.
	Real bool
	// MaxUSDHour is the ceiling on a host's estimated cost per hour, checked on
	// this node from its own price list (default 1.00).
	MaxUSDHour float64
	// PreflightWait and CatalogWait override the timeouts (tests).
	PreflightWait, CatalogWait time.Duration

	mu   sync.Mutex
	slot map[string]chan struct{} // one run of each kind at a time; the next waits its turn
}

// Timeouts for the two reads. A cold catalog is about ten seconds of calls.
const (
	PreflightTimeout = 40 * time.Second
	CatalogTimeout   = 5 * time.Minute
	maxOutput        = 8 << 20
)

// ErrBusy is returned when a run waited its turn for longer than its timeout.
var ErrBusy = errors.New("another run of this kind kept the tool busy")

// childEnv is the environment of the subprocess: an allow-list, so nothing of
// comebye's own (its agent token, its hub address) reaches the tool. The cloud
// CLIs need HOME and their own variables; those are the node's own.
func childEnv(environ []string) []string {
	allowed := func(k string) bool {
		switch k {
		case "PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "TZ", "SHELL":
			return true
		}
		for _, p := range []string{"XDG_", "AWS_", "CLOUDSDK_", "GOOGLE_", "GCLOUD_", "OPENSHELL_"} {
			if strings.HasPrefix(k, p) {
				return true
			}
		}
		return false
	}
	var out []string
	for _, kv := range environ {
		if k, _, ok := strings.Cut(kv, "="); ok && allowed(k) {
			out = append(out, kv)
		}
	}
	return out
}

// lock waits for the turn of this kind of run (the tool keeps its own cache
// and refresh lock; running two at once would only make one wait there).
func (t *Tool) lock(ctx context.Context, kind string) (func(), error) {
	t.mu.Lock()
	if t.slot == nil {
		t.slot = map[string]chan struct{}{}
	}
	ch := t.slot[kind]
	if ch == nil {
		ch = make(chan struct{}, 1)
		t.slot[kind] = ch
	}
	t.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ErrBusy
	}
}

// run runs the tool with args in its own process group and returns its stdout.
// A timeout or a cancelled context ends the whole group.
func (t *Tool) run(ctx context.Context, kind string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done, err := t.lock(ctx, kind)
	if err != nil {
		return nil, err
	}
	defer done()
	argv := append([]string{"-config", t.Config, "-no-prompt"}, args...)
	cmd := exec.Command(t.Bin, argv...)
	env := os.Environ
	if t.Environ != nil {
		env = t.Environ
	}
	cmd.Env = childEnv(env())
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{w: &out, max: maxOutput}, &limited{w: &errb, max: 64 << 10}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("the worker tool could not start: %v", Text(err.Error(), 200))
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			return out.Bytes(), &RunError{Msg: Text(lastLine(errb.String()), 300), Stdout: out.Bytes(), Err: err}
		}
		return out.Bytes(), nil
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waited
		}
		return nil, fmt.Errorf("the worker tool did not finish in %s", timeout)
	}
}

// RunError is a tool that ran and failed. Stdout may hold its JSON error.
type RunError struct {
	Msg    string
	Stdout []byte
	Err    error
}

func (e *RunError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Err.Error()
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

type limited struct {
	w   *bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.w.Len()+len(p) > l.max {
		return 0, errors.New("output too large")
	}
	return l.w.Write(p)
}

// Preflight runs `preflight --json`.
func (t *Tool) Preflight(ctx context.Context) (Preflight, error) {
	out, err := t.run(ctx, "preflight", orDur(t.PreflightWait, PreflightTimeout), "preflight", "--json")
	if err != nil {
		return Preflight{}, err
	}
	return ParsePreflight(out)
}

// CatalogReq is a request for the instance table.
type CatalogReq struct {
	Kind    string
	Region  string
	Score   []string
	Refresh bool
}

// CatalogError is the tool's own JSON error from `catalog --json`.
type CatalogError struct{ Class, Msg string }

func (e *CatalogError) Error() string { return e.Msg }

// Catalog runs `catalog --json` for a validated request.
func (t *Tool) Catalog(ctx context.Context, r CatalogReq) (*Catalog, error) {
	if r.Kind != "aws" {
		return nil, fmt.Errorf("kind %q has no price list yet", Text(r.Kind, 20))
	}
	if !regionRe.MatchString(r.Region) {
		return nil, errors.New("bad region")
	}
	args := []string{"catalog", "--kind", r.Kind, "--region", r.Region, "--json"}
	if len(r.Score) > 0 {
		if len(r.Score) > 10 {
			return nil, errors.New("at most 10 types can be scored")
		}
		for _, s := range r.Score {
			if !typeRe.MatchString(s) {
				return nil, errors.New("bad instance type")
			}
		}
		args = append(args, "--score", strings.Join(r.Score, ","))
	}
	if r.Refresh {
		args = append(args, "--refresh")
	}
	out, err := t.run(ctx, "catalog", orDur(t.CatalogWait, CatalogTimeout), args...)
	if err != nil {
		var re *RunError
		if errors.As(err, &re) {
			if ce := parseToolError(re.Stdout); ce != nil {
				return nil, ce
			}
		}
		return nil, err
	}
	return ParseCatalog(out)
}

func parseToolError(b []byte) *CatalogError {
	var e struct {
		Error struct{ Class, Msg string }
	}
	if len(b) == 0 || jsonUnmarshal(b, &e) != nil || e.Error.Msg == "" {
		return nil
	}
	cls := e.Error.Class
	switch cls {
	case "creds_expired", "quota", "other":
	default:
		cls = "other"
	}
	return &CatalogError{Class: cls, Msg: Text(e.Error.Msg, 300)}
}

func orDur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
