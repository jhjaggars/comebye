package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"text/tabwriter"

	"github.com/jhjaggars/comebye/internal/pageskit"
)

// ---- skill: the comebye-pages skill for claude and pi ----

// cmdSkill: skill install|status [--claude] [--pi] [--force] [--json].
// It writes only under $HOME/.claude/skills/comebye-pages and
// $HOME/.pi/agent/skills/comebye-pages, and never replaces a SKILL.md the
// user edited unless --force (which keeps the old one as SKILL.md.bak).
func cmdSkill(e *Env, args []string) error {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	claude := fs.Bool("claude", false, "only claude (~/.claude/skills)")
	pi := fs.Bool("pi", false, "only pi (~/.pi/agent/skills)")
	force := fs.Bool("force", false, "install: replace a SKILL.md you edited (kept as SKILL.md.bak)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (pos[0] != "install" && pos[0] != "status") {
		return usageErr("want skill install or skill status")
	}
	var names []string
	if *claude {
		names = append(names, "claude")
	}
	if *pi {
		names = append(names, "pi")
	}
	home := e.Getenv("HOME") // never a fallback: tests and odd setups must not reach the real home
	if home == "" {
		return fmt.Errorf("HOME is not set: it says where ~/.claude and ~/.pi are")
	}
	var rs []pageskit.Result
	if pos[0] == "install" {
		rs, err = pageskit.Install(home, names, *force)
	} else {
		if *force {
			return usageErr("--force is for install")
		}
		rs, err = pageskit.Status(home, names)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{"skill": pageskit.SkillName, "targets": rs}, "", "  ")
		_, err := fmt.Fprintf(e.Stdout, "%s\n", b)
		return err
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Target, r.State, r.Path)
	}
	tw.Flush()
	for _, r := range rs {
		switch {
		case r.State == pageskit.Edited && pos[0] == "install":
			fmt.Fprintf(e.Stderr, "%s: left alone, it is not the SKILL.md comebye wrote (you edited it?); --force replaces it and keeps yours as SKILL.md.bak\n", r.Target)
		case r.State == pageskit.Linked:
			fmt.Fprintf(e.Stderr, "%s: left alone, it is a symlink (managed elsewhere)\n", r.Target)
		case r.State == pageskit.Replaced:
			fmt.Fprintf(e.Stderr, "%s: your edited copy is kept in %s\n", r.Target, r.Backup)
		case r.State == pageskit.Outdated || r.State == pageskit.Missing:
			fmt.Fprintf(e.Stderr, "%s: run `comebye skill install` to bring it up to date\n", r.Target)
		}
	}
	return nil
}
