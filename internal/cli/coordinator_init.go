package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jhjaggars/comebye/internal/playbook"
)

// The coordinator directory (docs/agents.md): the directory a coordinator
// starts in, owned by the user, a git repository. It holds the rules
// (AGENTS.md) and the skills the coordinator loads.

const initAgentsMD = `Run ` + "`comebye playbook --harness claude`" + ` (` + "`--harness pi`" + ` under pi) for the commands; this file wins on conflict.

# Coordinator rules

This is your coordinator's own rule file. Add what you want it to always do
or never do, one line each. The playbook (above) says how the commands work;
only put here what is yours: your names for things, your habits, your limits.

- Answer in plain words, short. Name to-dos and agents by their titles, not ids.
- Never land, release or publish on your own: the user says when.

# Skills

Skills live in ` + "`skills/<name>/SKILL.md`" + `. Run one by name when the user asks
(for example "status").
`

const initClaudeMD = "@AGENTS.md\n"

const initStatusSkill = `---
name: status
description: A short status of the work in flight for the user. Use when they ask "status", "what's going on" or "where are we".
---

# Status

1. Run ` + "`comebye board`" + `, ` + "`comebye todo list`" + ` and ` + "`comebye ledger --active`" + `, and read the reports that are new since you last looked.
2. Answer in chat in about 12 lines.
3. Line 1: how many things need the user, and the main thing.
4. Keep "landed" (on main) and "released" (shipped) apart.
5. Flag every report whose only proof is unit tests: say so plainly.
6. Name to-dos by their plain titles, not ids.
7. Never publish anything for a status.
`

const initReadme = `# Your Comebye coordinator directory

The coordinator starts here (` + "`-coordinator-dir`" + `; Comebye picks ~/coordinator when it exists).

- ` + "`AGENTS.md`" + ` — the coordinator's rules. Edit it; it wins over the playbook.
- ` + "`CLAUDE.md`" + ` — one line, ` + "`@AGENTS.md`" + `, so Claude reads the same file as pi.
- ` + "`skills/<name>/SKILL.md`" + ` — skills. ` + "`.claude/skills`" + ` links to this folder for Claude; pi is given it with ` + "`--skill`" + `.

## Edit and sync

Edit the files, then commit and push (` + "`git add -A && git commit && git push`" + `). Comebye
runs ` + "`git pull --ff-only`" + ` here before it starts a coordinator and before a fresh
start when the directory is a git repository with a remote, so the same
directory on another host catches up. A pull that fails is shown as a notice and
the coordinator starts with the copy on disk.

Changes take effect at the next coordinator start or fresh start.
`

func cmdCoordInit(e *Env, args []string) error {
	fs := flag.NewFlagSet("coordinator init", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("want at most one directory, got %d", len(pos))
	}
	dir := "~/coordinator"
	if len(pos) == 1 {
		dir = pos[0]
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home := e.Getenv("HOME")
		if home == "" {
			if home, err = os.UserHomeDir(); err != nil {
				return err
			}
		}
		dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "status"), 0o755); err != nil {
		return err
	}
	var made, kept []string
	put := func(rel, text string) error {
		p := filepath.Join(dir, rel)
		if _, err := os.Lstat(p); err == nil {
			kept = append(kept, rel)
			return nil
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			return err
		}
		made = append(made, rel)
		return nil
	}
	for _, f := range []struct{ rel, text string }{
		{playbook.AgentsFile, initAgentsMD},
		{"CLAUDE.md", initClaudeMD},
		{"skills/status/SKILL.md", initStatusSkill},
		{"README.md", initReadme},
	} {
		if err := put(f.rel, f.text); err != nil {
			return err
		}
	}
	link := filepath.Join(dir, ".claude", "skills")
	if _, err := os.Lstat(link); err == nil {
		kept = append(kept, ".claude/skills")
	} else {
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join("..", "skills"), link); err != nil {
			return err
		}
		made = append(made, ".claude/skills -> ../skills")
	}
	repo := "already a git repository"
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			repo = "NOT a git repository (git init failed: " + strings.TrimSpace(string(out)) + ")"
		} else {
			repo = "git init done (nothing committed)"
		}
	}
	fmt.Fprintf(e.Stdout, "Coordinator directory %s\n", dir)
	for _, m := range made {
		fmt.Fprintf(e.Stdout, "  created  %s\n", m)
	}
	for _, k := range kept {
		fmt.Fprintf(e.Stdout, "  kept     %s (already there; not overwritten)\n", k)
	}
	fmt.Fprintf(e.Stdout, "  %s\n\nEdit AGENTS.md, commit, and add a remote to sync it across hosts. The hub uses ~/coordinator when it exists\n(or set -coordinator-dir); start the Coordinator preset again to load it.\n", repo)
	return nil
}

// cutUTF8 cuts s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
