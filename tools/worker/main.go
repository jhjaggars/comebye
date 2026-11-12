// Command worker provisions a comebye worker: a GCE or EC2 VM, or an
// OpenShell sandbox. It creates the instance, bootstraps it over host-key-verified ssh, enrols it in the
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

const usage = `Usage: worker [-config FILE] [-no-prompt] <command> [flags] [args]

Commands:
  create NAME [--dry-run] [--comebye-version vX.Y.Z] [--spec FILE|-] [--events]
        build the VM (or sandbox), enrol it as tag:worker, bootstrap it, wait for the hub.
        --spec lays one request (JSON: kind, name, type, spot, zone, disks, idle_minutes,
        max_run, agents, llm_key, budget_cap_usd, destroy_after_hours) over the config;
        unknown fields are refused, and NAME may then come from the spec. --events prints
        one JSON line per stage on stdout and moves the human text to stderr
  status [NAME]       one worker, or (no NAME) every worker, disk and tagged node
  stop NAME           stop the VM; the tailnet node and peer entry stay
  start NAME          start it again and re-verify
  archive NAME | --all
        copy a running worker's sessions to the transcript archive (--all: every
        running worker; stopped ones are skipped)
  destroy NAME [--delete-data-disk] [--no-archive] [--yes] [--events]
        asks you to type NAME (--yes: the caller has confirmed, do not ask); saves the sessions to the archive, then removes the
        VM, tailnet node and hub peer entry. It refuses if the sessions cannot be
        saved; --no-archive loses them instead
  login NAME pi|claude|codex
        sign in on the worker (the credential is created there)
  preflight [--json]
        read-only: are the credentials this config needs usable (aws, gcloud, tailnet,
        openshell, ssh)? Never prints a key, token or account id
  catalog [--kind aws|gcp] [--region R] [--families f1,f2] [--types t1,t2] [--score t1,t2] [--refresh] [--json]
        read-only: instance types with vCPU, memory, prices and spot interruption bands,
        cached on disk in the state dir
  keys [--prune]
        the per-worker LLM gateway keys (worker-NAME) and whether a VM exists
        for each; --prune deletes the ones whose VM is gone

Status: status [--json] [NAME] prints one worker, or every worker, as JSON.

--no-prompt (global, or after the command) turns every terminal prompt into a
needs_input error: nothing reads a terminal.

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
	gNoPrompt := fs.Bool("no-prompt", false, "never prompt: a prompt becomes a needs_input error")
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
	prune := cf.Bool("prune", false, "keys: delete worker keys whose VM is gone")
	all := cf.Bool("all", false, "archive: every running worker")
	events := cf.Bool("events", false, "create, destroy: JSON lines on stdout, human text on stderr")
	specPath := cf.String("spec", "", "create: a JSON request laid over the config (FILE, or - for stdin)")
	yes := cf.Bool("yes", false, "destroy: the caller has confirmed; do not ask")
	cNoPrompt := cf.Bool("no-prompt", false, "never prompt: a prompt becomes a needs_input error")
	asJSON := cf.Bool("json", false, "preflight, catalog, status: JSON output")
	kind := cf.String("kind", "", "catalog: aws or gcp")
	region := cf.String("region", "", "catalog: the region (default: aws_region)")
	families := cf.String("families", "", "catalog: instance families to price (default: allowed_families)")
	types := cf.String("types", "", "catalog: only these instance types")
	score := cf.String("score", "", "catalog: ask for spot placement scores for these types")
	refresh := cf.Bool("refresh", false, "catalog: ignore the cache")
	noArchive := cf.Bool("no-archive", false, "destroy: do not save the worker's sessions (they are lost)")
	// Flags may follow the positional name: split them out.
	var pos, flags []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--comebye-version" || a == "-comebye-version" || a == "--spec" || a == "-spec" ||
			a == "--kind" || a == "-kind" || a == "--region" || a == "-region" || a == "--families" || a == "-families" ||
			a == "--types" || a == "-types" || a == "--score" || a == "-score":
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
	case "create", "status", "stop", "start", "destroy", "login", "keys", "archive", "preflight", "catalog":
	case "help":
		fmt.Fprint(e.Out, usage)
		return 0
	default:
		fmt.Fprintf(e.Err, "worker: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	want := map[string][2]int{"create": {1, 1}, "status": {0, 1}, "stop": {1, 1}, "start": {1, 1}, "destroy": {1, 1}, "login": {2, 2}, "keys": {0, 0}, "archive": {0, 1}, "preflight": {0, 0}, "catalog": {0, 0}}[cmd]
	if cmd == "create" && *specPath != "" {
		want[0] = 0 // the name may come from the spec
	}
	if len(pos) < want[0] || len(pos) > want[1] {
		fmt.Fprintf(e.Err, "worker %s: wrong number of arguments\n\n%s", cmd, usage)
		return 2
	}

	if cmd == "archive" && *all == (len(pos) == 1) {
		fmt.Fprintf(e.Err, "worker archive: give a NAME or --all\n\n%s", usage)
		return 2
	}
	if (*all && cmd != "archive") || (*noArchive && cmd != "destroy") {
		fmt.Fprintf(e.Err, "worker: --all is only for archive, and --no-archive only for destroy\n")
		return 2
	}

	for flag, bad := range map[string]bool{
		"--spec": *specPath != "" && cmd != "create", "--yes": *yes && cmd != "destroy",
		"--events": *events && cmd != "create" && cmd != "destroy",
		"--json":   *asJSON && cmd != "preflight" && cmd != "catalog" && cmd != "status",
	} {
		if bad {
			fmt.Fprintf(e.Err, "worker: %s does not apply to %s\n", flag, cmd)
			return 2
		}
	}
	e.NoPrompt = *gNoPrompt || *cNoPrompt
	if *events {
		// stdout is for events; whatever a person would read goes to stderr.
		e.Events = &eventSink{w: e.Out, env: e}
		e.Out = e.Err
	}
	// fail reports a problem found before any stage: in events mode it is an
	// event too, so a caller that reads only stdout still sees it.
	fail := func(code int, err error) int {
		fmt.Fprintf(e.Err, "worker: %v\n", err)
		e.emit(event{Stage: "config", State: stateEvFailed, Msg: err.Error(), Class: classify(err, "config")})
		return code
	}

	cfg, err := loadConfigRaw(*cfgPath)
	if err != nil {
		return fail(1, err)
	}
	var spec *Spec
	if *specPath != "" {
		if spec, err = ReadSpec(*specPath, e.In); err != nil {
			return fail(1, err)
		}
		if err = spec.Apply(cfg); err != nil {
			return fail(1, err)
		}
	} else if err = cfg.Validate(); err != nil {
		return fail(1, err)
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
	if spec != nil && spec.Name != "" {
		if name != "" && name != spec.Name {
			return fail(2, fmt.Errorf("the name %q and the spec's name %q differ", name, spec.Name))
		}
		name = spec.Name
	}
	if cmd == "create" && name == "" {
		return fail(2, fmt.Errorf("create needs a NAME, or a name in the spec"))
	}
	switch cmd {
	case "create":
		err = e.create(ctx, name, createOpts{Version: *ver})
	case "status":
		if *asJSON {
			err = e.statusJSON(ctx, name)
		} else {
			err = e.status(ctx, name)
		}
	case "preflight":
		err = e.preflight(ctx, *asJSON)
	case "catalog":
		err = e.catalog(ctx, catalogOpts{Kind: *kind, Region: *region, Families: splitList(*families), Types: splitList(*types), Score: splitList(*score), Refresh: *refresh, JSON: *asJSON})
	case "stop":
		err = e.stop(ctx, name)
	case "start":
		err = e.start(ctx, name)
	case "destroy":
		err = e.track([]string{"destroy"}, func() error {
			e.begin(1, "")
			return e.destroy(ctx, name, destroyOpts{DeleteDataDisk: *delData, NoArchive: *noArchive, Yes: *yes})
		})
	case "archive":
		if *all {
			err = e.archiveAll(ctx)
		} else if err = checkName(name); err == nil {
			err = e.archiveOne(ctx, name)
		}
	case "login":
		err = e.login(ctx, name, pos[1])
	case "keys":
		err = e.keys(ctx, *prune)
	}
	if err != nil {
		fmt.Fprintf(e.Err, "worker: %v\n", err)
		return 1
	}
	return 0
}
