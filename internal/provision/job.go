package provision

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Failure classes the app can act on (the tool's events carry one).
const (
	ClassSpotCapacity = "spot_capacity"
	ClassCredsExpired = "creds_expired"
	ClassQuota        = "quota"
	ClassBootstrap    = "bootstrap_failed"
	ClassNeedsInput   = "needs_input"
	ClassOther        = "other"
)

// Stages of a VM build, in the order the tool reports them.
var Stages = []string{"network", "ssh_key", "data_disk", "instance", "ssh", "bootstrap", "tailnet", "agents", "hub"}

// Item is one line of a job's journal: a stage transition of the tool, or a
// line of its human text. Only these fields exist, so nothing else a peer
// sends can reach a browser.
type Item struct {
	Seq     int    `json:"seq"`
	Attempt int    `json:"attempt"`
	Kind    string `json:"kind"` // event or log
	Stage   string `json:"stage,omitempty"`
	Step    string `json:"step,omitempty"`
	State   string `json:"state,omitempty"` // running, ok, failed, planned
	Msg     string `json:"msg,omitempty"`
	Class   string `json:"class,omitempty"`
	Zone    string `json:"zone,omitempty"`
}

type toolEvent struct {
	V     int    `json:"v"`
	Seq   int    `json:"seq"`
	Stage string `json:"stage"`
	Step  string `json:"step"`
	State string `json:"state"`
	Msg   string `json:"msg"`
	Class string `json:"class"`
	Zone  string `json:"zone"`
}

var stageOK = func() map[string]bool {
	m := map[string]bool{"config": true, "create": true, "destroy": true, "sandbox": true, "settings": true}
	for _, s := range Stages {
		m[s] = true
	}
	return m
}()

// ParseEvent decodes one stdout line of the tool in --events mode. A line that
// is not a version-1 event with a known stage and state is refused.
func ParseEvent(line []byte) (Item, error) {
	var e toolEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return Item{}, errors.New("not an event")
	}
	if e.V != 1 || !stageOK[e.Stage] {
		return Item{}, errors.New("not an event this version knows")
	}
	switch e.State {
	case "running", "ok", "failed", "planned":
	default:
		return Item{}, errors.New("unknown event state")
	}
	it := Item{Kind: "event", Stage: e.Stage, Step: Text(e.Step, 8), State: e.State, Msg: Text(e.Msg, 300)}
	if e.State == "failed" {
		switch e.Class {
		case ClassSpotCapacity, ClassCredsExpired, ClassQuota, ClassBootstrap, ClassNeedsInput, ClassOther:
			it.Class = e.Class
		default:
			it.Class = ClassOther
		}
		if zoneRe.MatchString(e.Zone) {
			it.Zone = e.Zone
		}
	}
	return it, nil
}

// ---- the plan ----

// Plan is what a create would do, from the tool's dry run.
type Plan struct {
	Text   string   `json:"text"`
	Stages []string `json:"stages"`
}

const maxPlanText = 24 << 10

// MultiLine keeps lines, drops control characters and removes anything shaped
// like a credential.
func MultiLine(s string, max int) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		out = append(out, Text(l, 600))
	}
	r := strings.TrimSpace(strings.Join(out, "\n"))
	if len(r) > max {
		r = r[:max] + "\n…"
	}
	return r
}

// PlanFor runs the tool's dry run for a validated spec. Nothing is created
// and no credential is needed.
func (t *Tool) PlanFor(ctx context.Context, s Spec) (*Plan, error) {
	var stages []string
	var human bytes.Buffer
	_, err := t.stream(ctx, "plan", 60*time.Second, s.JSON(), []string{"create", "--dry-run", "--events", "--spec", "-"}, func(line []byte) {
		if it, err := ParseEvent(line); err == nil && it.State == "planned" {
			stages = append(stages, it.Stage)
		}
	}, func(line string) { human.WriteString(line + "\n") })
	if err != nil {
		var re *RunError
		msg := Text(human.String(), 300)
		if errors.As(err, &re) && re.Msg != "" {
			msg = re.Msg
		}
		return nil, fmt.Errorf("the plan was refused: %s", Text(msg, 300))
	}
	return &Plan{Text: MultiLine(human.String(), maxPlanText), Stages: stages}, nil
}

// ---- running a job ----

// JobResult is how a run of the tool ended.
type JobResult struct {
	OK    bool
	Class string
	Zone  string
	Msg   string
}

// CreateTimeout is how long one create may run before its process group ends.
const CreateTimeout = 30 * time.Minute

// RunCreate runs create for a validated spec, one Item per stage transition
// and per line of human text, to emit. dry runs it with --dry-run (the plan,
// nothing created). It returns when the tool has ended.
func (t *Tool) RunCreate(ctx context.Context, s Spec, dry bool, emit func(Item)) JobResult {
	args := []string{"create", "--events", "--spec", "-"}
	if dry {
		args = append(args[:1], append([]string{"--dry-run"}, args[1:]...)...)
	}
	return t.runJob(ctx, "create", s.JSON(), args, emit)
}

// RunCleanup removes what a failed create made (destroy NAME --yes). The name
// is validated by the caller; the tool refuses anything it did not label.
func (t *Tool) RunCleanup(ctx context.Context, name string, emit func(Item)) JobResult {
	if !nameRe.MatchString(name) {
		return JobResult{Class: ClassOther, Msg: "bad name"}
	}
	return t.runJob(ctx, "create", nil, []string{"destroy", name, "--yes", "--no-archive", "--events"}, emit)
}

func (t *Tool) runJob(ctx context.Context, kind string, stdin []byte, args []string, emit func(Item)) JobResult {
	var failed *Item
	var lastLine string
	_, err := t.stream(ctx, kind, CreateTimeout, stdin, args, func(line []byte) {
		it, err := ParseEvent(line)
		if err != nil {
			return
		}
		if it.State == "failed" {
			c := it
			failed = &c
		}
		emit(it)
	}, func(line string) {
		line = Text(line, 300)
		if line == "" {
			return
		}
		lastLine = line
		emit(Item{Kind: "log", Msg: line})
	})
	if err == nil {
		return JobResult{OK: true}
	}
	if failed != nil {
		return JobResult{Class: failed.Class, Zone: failed.Zone, Msg: failed.Msg}
	}
	var re *RunError
	msg := lastLine
	if errors.As(err, &re) && re.Msg != "" {
		msg = re.Msg
	} else if msg == "" {
		msg = Text(err.Error(), 300)
	}
	cls := ClassOther
	if ctx.Err() != nil {
		msg = "the run was interrupted"
	}
	return JobResult{Class: cls, Msg: Text(msg, 300)}
}

// stream runs the tool in its own process group with stdin, handing stdout
// lines and stderr lines to the callbacks as they come. A cancelled context or
// the timeout ends the whole group.
func (t *Tool) stream(ctx context.Context, kind string, timeout time.Duration, stdin []byte, args []string, onOut func([]byte), onErr func(string)) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done, err := t.lock(ctx, kind)
	if err != nil {
		return 0, err
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
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	so, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	se, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("the worker tool could not start: %v", Text(err.Error(), 200))
	}
	var last string
	lines := make(chan struct{}, 2)
	scan := func(r io.Reader, f func([]byte)) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 256<<10)
		for sc.Scan() {
			f(sc.Bytes())
		}
		io.Copy(io.Discard, r)
		lines <- struct{}{}
	}
	go scan(so, onOut)
	go scan(se, func(b []byte) {
		last = string(b)
		onErr(string(b))
	})
	waited := make(chan error, 1)
	go func() {
		<-lines
		<-lines
		waited <- cmd.Wait()
	}()
	select {
	case err := <-waited:
		if err != nil {
			return 1, &RunError{Msg: Text(last, 300), Err: err}
		}
		return 0, nil
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waited
		}
		if ctx.Err() == context.DeadlineExceeded {
			return 1, fmt.Errorf("the worker tool did not finish in %s", timeout)
		}
		return 1, ctx.Err()
	}
}
