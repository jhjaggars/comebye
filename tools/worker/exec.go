package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs external programs (gcloud, ssh, ssh-keygen, stty). Secrets go
// in Stdin, never in args: args show up in `ps` and in logs.
type Runner interface {
	// Run waits for the program and returns its stdout and stderr.
	Run(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr string, err error)
	// Interactive runs the program attached to this terminal.
	Interactive(ctx context.Context, name string, args []string) error
}

// streamRunner is a Runner that can send a program's stdout to a writer, for
// output too large to hold in memory.
type streamRunner interface {
	RunStream(ctx context.Context, name string, args []string, stdout io.Writer) error
}

// runStream runs a program with its stdout going to w. A Runner that cannot
// stream still works: the output is copied once the program ends.
func runStream(ctx context.Context, r Runner, name string, args []string, w io.Writer) error {
	if s, ok := r.(streamRunner); ok {
		return s.RunStream(ctx, name, args, w)
	}
	out, _, err := r.Run(ctx, name, args, nil)
	if _, werr := io.WriteString(w, out); err == nil {
		err = werr
	}
	return err
}

type osRunner struct{}

func (osRunner) RunStream(ctx context.Context, name string, args []string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, lastLines(errb.String(), 6))
	}
	return nil
}

func (osRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s: %w: %s", name, err, lastLines(errb.String(), 6))
	}
	return out.String(), errb.String(), err
}

func (osRunner) Interactive(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// lastLines is the tail of a command's stderr: the reason is at the end,
// warnings come first.
func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// shellQuote quotes s for a POSIX shell (used only for ssh remote command
// words that this program composed itself).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// env bundles what the commands need, so tests can replace all of it.
type env struct {
	Out, Err io.Writer
	In       io.Reader
	Run      Runner
	P        Provider // built from Cfg on first use
	Cfg      *Config
	DryRun   bool
	HTTP     httpDoer
	Sleep    func(d int) // seconds
	Getenv   func(string) string
	// ReadSecret reads one hidden line from the terminal (tests replace it).
	ReadSecret func(prompt string) (string, error)

	// NoPrompt turns every terminal prompt (the destroy confirmation, the
	// hidden auth-key prompt, an interactive login) into a needs_input error.
	NoPrompt bool
	// Events, when set, receives one JSON line per stage transition; Out is
	// then the human text's stream, which main points at stderr.
	Events  *eventSink
	stages  *stageTracker
	secrets []string
	// Now is the clock (tests replace it).
	Now func() time.Time
}

func (e *env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
