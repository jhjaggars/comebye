package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/jhjaggars/comebye/internal/stt"
)

// sttFiles is the pinned file list (tests replace it to fetch from a local
// server).
var sttFiles = stt.Files

// cmdSTT is `comebye stt`: install and check the files the app's voice
// input needs. Local to this host (no server call): it reads and writes the
// state directory the server serves them from.
func cmdSTT(e *Env, args []string) error {
	if len(args) == 0 {
		return usageErr("want fetch or status")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("stt "+sub, flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "the server's state directory (default: COMEBYE_STATE_DIR, else ~/.local/state/comebye)")
	asJSON := fs.Bool("json", false, "machine-readable output (status)")
	verify := fs.Bool("verify", false, "status: hash every file against its pinned SHA-256 (reads about 290 MB)")
	pos, err := parse(fs, rest)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	root := *stateDir
	if root == "" {
		root = StateDir(e.Getenv)
	}
	dir := stt.Dir(root)
	switch sub {
	case "fetch":
		fmt.Fprint(e.Stdout, stt.Licenses, "\n")
		client := e.HTTP
		if client == nil {
			// No overall timeout: the files are large. The context bounds the run.
			client = &http.Client{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		list := sttFiles()
		if err := stt.Fetch(ctx, client, dir, list, func(m string) { fmt.Fprintln(e.Stdout, m) }); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "voice input %s is installed in %s; the app offers it at the next tap of the mic (no restart needed)\n", stt.Version, dir)
		return nil
	case "status":
		installed, missing := stt.Status(dir)
		var bad []string
		if installed && *verify {
			bad = stt.Verify(dir)
			installed = len(bad) == 0
		}
		if *asJSON {
			if err := json.NewEncoder(e.Stdout).Encode(map[string]any{
				"version": stt.Version, "dir": dir, "installed": installed, "missing": missing, "mismatched": bad,
				"bytes": map[string]int64{"webgpu": stt.Bytes(stt.WebGPU), "wasm": stt.Bytes(stt.WASM)},
			}); err != nil {
				return err
			}
			if !installed {
				return fmt.Errorf("not installed")
			}
			return nil
		}
		switch {
		case installed:
			fmt.Fprintf(e.Stdout, "voice input %s: installed in %s", stt.Version, dir)
			if *verify {
				fmt.Fprint(e.Stdout, " (every file matches its pinned SHA-256)")
			}
			fmt.Fprintln(e.Stdout)
		case len(bad) > 0:
			fmt.Fprintf(e.Stdout, "voice input %s: %d file(s) in %s do not match their pinned SHA-256: %v\nrun: comebye stt fetch\n", stt.Version, len(bad), dir, bad)
		default:
			fmt.Fprintf(e.Stdout, "voice input %s: not installed (%d of %d files missing in %s)\nrun: comebye stt fetch   (downloads about %d MB)\n",
				stt.Version, len(missing), len(sttFiles()), dir, stt.Total()/1e6)
		}
		if !installed {
			return fmt.Errorf("not installed")
		}
		return nil
	}
	return usageErr("unknown subcommand %q: want fetch or status", sub)
}
