package cli

import (
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/jhjaggars/comebye/internal/modkit"
)

// modClaude is the claude program `mod` runs; tests replace it.
var modClaude modkit.Claude = modkit.ExecClaude{}

// cmdMod: mod install|status|uninstall|band. See package modkit.
func cmdMod(e *Env, args []string) error {
	fs := flag.NewFlagSet("mod", flag.ContinueOnError)
	dirF := fs.String("dir", "", "the mods folder (default $XDG_DATA_HOME/comebye/mods, else ~/.local/share/comebye/mods)")
	with := fs.String("with", "", "install: also install these early-access mods (coord-window)")
	force := fs.Bool("force", false, "install: replace a file you edited (kept as .bak)")
	ifInstalled := fs.Bool("if-installed", false, "install: refresh the files of mods installed here before, and do nothing where there are none (`comebye upgrade` runs this; it never runs claude)")
	noClaude := fs.Bool("no-claude", false, "write or read the folder only; do not run claude")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want mod install, status, uninstall or band")
	}
	if pos[0] == "band" {
		c, err := newClient(e)
		if err != nil {
			return err
		}
		b, err := collectBand(c)
		if err != nil {
			return err
		}
		return jsonOut(e, b)
	}
	dir := *dirF
	if dir == "" {
		if dir, err = modkit.DefaultDir(e.Getenv); err != nil {
			return err
		}
	}
	o := modkit.Options{Dir: dir, Claude: modClaude, Force: *force, NoClaude: *noClaude, IfInstalled: *ifInstalled}
	if *with != "" {
		o.With = strings.Split(*with, ",")
	}
	var r *modkit.Report
	switch pos[0] {
	case "install":
		if *ifInstalled && *with != "" {
			return usageErr("--if-installed changes only what is installed: it takes no --with")
		}
		r, err = modkit.Install(o)
	case "status":
		if *force || *with != "" || *ifInstalled {
			return usageErr("--force and --with are for install")
		}
		r, err = modkit.Status(o)
	case "uninstall":
		r, err = modkit.Uninstall(o)
	default:
		return usageErr("unknown mod action %q (want install, status, uninstall or band)", pos[0])
	}
	if r != nil {
		if *asJSON {
			if jerr := jsonOut(e, r); jerr != nil {
				return jerr
			}
		} else {
			printMod(e, pos[0], r)
		}
	}
	return err
}

func printMod(e *Env, action string, r *modkit.Report) {
	if action == "install" && len(r.Files) == 0 && len(r.Notes) > 0 {
		fmt.Fprintln(e.Stdout, r.Notes[0])
		return
	}
	fmt.Fprintf(e.Stdout, "folder: %s\n", r.Dir)
	counts := map[string]int{}
	for _, f := range r.Files {
		counts[f.State]++
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	for _, st := range []string{"installed", "updated", "replaced", "removed", "current", "outdated", "edited", "kept (edited)", "missing"} {
		if counts[st] > 0 {
			fmt.Fprintf(tw, "files %s\t%d\n", st, counts[st])
		}
	}
	tw.Flush()
	for _, f := range r.Files {
		switch f.State {
		case modkit.Edited, "kept (edited)":
			fmt.Fprintf(e.Stderr, "%s: left alone, it is not the file comebye wrote (you edited it?); install --force replaces it and keeps yours as .bak\n", f.Rel)
		case "replaced":
			fmt.Fprintf(e.Stderr, "%s: your edited copy is kept as %s\n", f.Rel, f.Backup)
		}
	}
	fmt.Fprintf(e.Stdout, "claude marketplace %s: %s\n", modkit.Marketplace, r.Marketplace)
	if len(r.Installed) > 0 {
		fmt.Fprintf(e.Stdout, "claude plugins installed: %s\n", strings.Join(r.Installed, ", "))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(e.Stderr, "note: %s\n", n)
	}
	switch {
	case action == "status" && counts[modkit.Missing]+counts[modkit.Outdated] > 0:
		fmt.Fprintln(e.Stderr, "run `comebye mod install` to bring the folder up to date, then /reload-plugins in a running claude")
	case action == "install":
		fmt.Fprintln(e.Stdout, "in a running claude: /reload-plugins")
	}
}
