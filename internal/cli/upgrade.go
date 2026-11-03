package cli

import (
	"flag"
	"path/filepath"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// cmdUpgrade installs a new comebye over the installed one and restarts
// the user service; it works on this host only and does not need the
// server (it is the thing that restarts it).
func cmdUpgrade(e *Env, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	o := upgrade.Options{}
	fs.StringVar(&o.Version, "version", "latest", "release tag to install (v0.2.0) or latest")
	fs.BoolVar(&o.FromSource, "from-source", false, "build from a checkout instead of installing a release")
	fs.StringVar(&o.Ref, "ref", "", "with --from-source: commit, tag or branch (default origin/main)")
	fs.StringVar(&o.RepoDir, "repo", "", "with --from-source: the checkout (default the current directory)")
	fs.BoolVar(&o.SkipChecks, "skip-checks", false, "with --from-source: skip go vet and go test")
	fs.BoolVar(&o.DryRun, "dry-run", false, "download and verify, print the plan, install nothing")
	fs.BoolVar(&o.Force, "force", false, "allow a downgrade, or reinstall the same version")
	fs.BoolVar(&o.Rollback, "rollback", false, "restore the previous binary (comebye.prev) and restart")
	fs.StringVar(&o.Releases, "releases", e.Getenv("COMEBYE_RELEASES_URL"), "release base URL (COMEBYE_RELEASES_URL; default the GitHub releases)")
	bin := fs.String("bin", "", "the installed binary (default ~/.local/bin/comebye)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	if *bin == "" {
		home := e.Getenv("HOME")
		if home == "" {
			return usageErr("HOME is not set: pass --bin")
		}
		*bin = filepath.Join(home, ".local", "bin", "comebye")
	}
	o.Bin = *bin
	if u := e.Getenv("COMEBYE_URL"); u != "" {
		o.LocalURL = u
	}
	o.HTTP = e.HTTP
	o.Out, o.Err = e.Stdout, e.Stderr
	o.Sleep = e.Sleep
	return upgrade.Run(o)
}
