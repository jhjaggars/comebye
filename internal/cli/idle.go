package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/idle"
)

// errNotIdle is the exit status 1 of `comebye idle`: an answer, not a failure.
var errNotIdle = errors.New("not idle")

// cmdIdle exits 0 when this host has been idle for the limit: no herdr agent
// working and no interactive session for that long. Meant to be polled (the
// VM's timer, a sandbox's init loop); each poll that sees activity renews
// the clock. It works on this host only and does not need the server.
func cmdIdle(e *Env, args []string) error {
	fs := flag.NewFlagSet("idle", flag.ContinueOnError)
	def := 120
	if v, err := strconv.Atoi(e.Getenv("COMEBYE_IDLE_MINUTES")); err == nil && v >= 0 {
		def = v
	}
	minutes := fs.Int("minutes", def, "idle limit in minutes (COMEBYE_IDLE_MINUTES; default 120); 0 asks only whether anything is active now")
	asJSON := fs.Bool("json", false, "machine-readable answer")
	noTouch := fs.Bool("dry-run", false, "never write the clock (the stamp file)")
	stamp := fs.String("stamp", "", "the clock file (default <state dir>/idle-stamp)")
	sock := fs.String("sock", "", "herdr socket (default $COMEBYE_SOCK, $HERDR_SOCKET_PATH, herdr's default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	if *minutes < 0 {
		return usageErr("--minutes must not be negative")
	}
	if *stamp == "" {
		*stamp = filepath.Join(StateDir(e.Getenv), "idle-stamp")
	}
	o := idle.Options{Minutes: *minutes, Stamp: *stamp, NoTouch: *noTouch}
	if e.Now != nil {
		o.Now = e.Now
	}
	path := *sock
	if path == "" {
		path = e.Getenv("COMEBYE_SOCK")
	}
	if path == "" {
		path = e.Getenv("HERDR_SOCKET_PATH")
	}
	if path != "" {
		o.Herdr = herdr.NewClient(expandHome(path, e.Getenv))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := idle.Check(ctx, o)
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.Marshal(r)
		fmt.Fprintln(e.Stdout, string(b))
	} else {
		fmt.Fprintln(e.Stdout, r.Summary())
	}
	if !r.Idle {
		return errNotIdle
	}
	return nil
}
