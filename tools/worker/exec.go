package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner runs external programs (gcloud, ssh, ssh-keygen, stty). Secrets go
// in Stdin, never in args: args show up in `ps` and in logs.
type Runner interface {
	// Run waits for the program and returns its stdout and stderr.
	Run(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr string, err error)
	// Interactive runs the program attached to this terminal.
	Interactive(ctx context.Context, name string, args []string) error
}

type osRunner struct{}

func (osRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s: %w: %s", name, err, firstLine(errb.String()))
	}
	return out.String(), errb.String(), err
}

func (osRunner) Interactive(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
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
}
