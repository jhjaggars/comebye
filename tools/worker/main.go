// Command worker provisions a comebye worker VM: it creates the GCE or EC2
// instance, bootstraps it over host-key-verified ssh, enrols it in the
// tailnet as a tagged node and lets it join the comebye hub. Agent
// orchestration is not its job; once the VM is a peer, comebye drives it.
//
//	go run ./tools/worker create NAME
//
// See docs/workers.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"
)

const usage = `Usage: worker [-config FILE] <command> [flags] [args]

Commands:
  create NAME [--dry-run] [--comebye-version vX.Y.Z]
        build the VM, enrol it as tag:worker, bootstrap it, wait for the hub
  status [NAME]       one worker, or (no NAME) every worker, disk and tagged node
  stop NAME           stop the VM; the tailnet node and peer entry stay
  start NAME          start it again and re-verify
  destroy NAME [--delete-data-disk]
        asks you to type NAME; removes the VM, tailnet node and hub peer entry
  login NAME claude|codex
        sign in on the VM (the credential is created there)

Config: ~/.config/comebye-worker/config (see tools/worker/config.example).
`

func main() { os.Exit(run(os.Args[1:], osEnv())) }

func osEnv() *env {
	return &env{
		Out: os.Stdout, Err: os.Stderr, In: os.Stdin,
		Run:        osRunner{},
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		Getenv:     os.Getenv,
		ReadSecret: readSecretTTY,
	}
}

// readSecretTTY reads one line with terminal echo off (stty, so no new
// dependency). It refuses to run without a terminal.
func readSecretTTY(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal for a hidden prompt: use an OAuth client file instead")
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	off := exec.Command("stty", "-echo")
	off.Stdin = tty
	if err := off.Run(); err != nil {
		return "", fmt.Errorf("cannot turn terminal echo off: %w", err)
	}
	defer func() {
		on := exec.Command("stty", "echo")
		on.Stdin = tty
		on.Run()
		fmt.Fprintln(tty)
	}()
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := tty.Read(buf)
		if n == 1 {
			if buf[0] == '\n' || buf[0] == '\r' {
				break
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
	}
	return b.String(), nil
}

func run(args []string, e *env) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(e.Err)
	fs.Usage = func() { fmt.Fprint(e.Err, usage) }
	cfgPath := fs.String("config", DefaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	cmd, rest := rest[0], rest[1:]

	cf := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cf.SetOutput(e.Err)
	dry := cf.Bool("dry-run", false, "create: print the plan only")
	ver := cf.String("comebye-version", "", "create: a release tag instead of the hub's version")
	delData := cf.Bool("delete-data-disk", false, "destroy: also delete the data disk")
	// Flags may follow the positional name: split them out.
	var pos, flags []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--comebye-version" || a == "-comebye-version":
			flags = append(flags, a)
			if i+1 < len(rest) {
				i++
				flags = append(flags, rest[i])
			}
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			pos = append(pos, a)
		}
	}
	if err := cf.Parse(flags); err != nil {
		return 2
	}

	switch cmd {
	case "create", "status", "stop", "start", "destroy", "login":
	case "help":
		fmt.Fprint(e.Out, usage)
		return 0
	default:
		fmt.Fprintf(e.Err, "worker: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	want := map[string][2]int{"create": {1, 1}, "status": {0, 1}, "stop": {1, 1}, "start": {1, 1}, "destroy": {1, 1}, "login": {2, 2}}[cmd]
	if len(pos) < want[0] || len(pos) > want[1] {
		fmt.Fprintf(e.Err, "worker %s: wrong number of arguments\n\n%s", cmd, usage)
		return 2
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(e.Err, "worker: %v\n", err)
		return 1
	}
	e.Cfg = cfg
	e.DryRun = *dry
	if *dry && cmd != "create" {
		fmt.Fprintln(e.Err, "worker: --dry-run is only for create")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	switch cmd {
	case "create":
		err = e.create(ctx, name, createOpts{Version: *ver})
	case "status":
		err = e.status(ctx, name)
	case "stop":
		err = e.stop(ctx, name)
	case "start":
		err = e.start(ctx, name)
	case "destroy":
		err = e.destroy(ctx, name, destroyOpts{DeleteDataDisk: *delData})
	case "login":
		err = e.login(ctx, name, pos[1])
	}
	if err != nil {
		fmt.Fprintf(e.Err, "worker: %v\n", err)
		return 1
	}
	return 0
}
