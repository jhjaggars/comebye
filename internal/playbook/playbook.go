// Package playbook holds the coordinator playbook, the single source of
// truth for how any agent acts as Comebye's coordinator, and the
// per-harness permission presets that let it run the comebye CLI.
package playbook

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed playbook.md
var body string

// Harnesses are the playbook's output flavors.
var Harnesses = []string{"generic", "pi", "claude"}

// Body is the playbook's markdown, harness-neutral.
func Body() string { return body }

// Render returns the playbook for a harness: "pi" (a Pi agent definition
// with frontmatter), "claude" (for CLAUDE.md or a first prompt) or
// "generic"/"" (plain markdown).
func Render(harness string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "", "generic":
		return body, nil
	case "pi":
		return piFrontmatter + strings.TrimPrefix(body, "# Comebye coordinator playbook\n"), nil
	case "claude":
		return claudeHeader + strings.TrimPrefix(body, "# Comebye coordinator playbook\n"), nil
	}
	return "", fmt.Errorf("unknown harness %q (want one of %s)", harness, strings.Join(Harnesses, ", "))
}

// piFrontmatter makes the playbook a Pi agent definition, like pi-config's
// agents/coordinator.md: read-only built-ins plus bash for the CLI; no
// edit or write tools.
const piFrontmatter = `---
name: coordinator
description: Comebye coordinator. Hands work to worker agents, follows them through the Comebye ledger, proposes cleanup and publishes briefings, all through the comebye CLI. Never codes.
tools: read, grep, find, ls, bash
---

# Coordinator
`

// claudeHeader introduces the playbook for Claude Code (paste it into a
// CLAUDE.md in the coordinator's directory, or give it as the first
// prompt).
const claudeHeader = `# Comebye coordinator (Claude Code)

You are running in Claude Code as the Comebye coordinator. Use the Bash
tool to run ` + "`comebye`" + ` commands (allow them once with
` + "`comebye presets claude`" + `); do not use Edit, Write or NotebookEdit,
and do not spawn subagents to do the work: hand it off with
` + "`comebye handoff`" + ` instead.
`

// Preset returns the permission preset for a harness: how to let it run
// comebye without a prompt on every call.
func Preset(harness string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude":
		return claudePreset, nil
	case "pi":
		return piPreset, nil
	case "generic", "codex", "opencode", "gemini":
		return genericPreset, nil
	}
	return "", fmt.Errorf("unknown harness %q (want claude, pi or generic)", harness)
}

const claudePreset = `{
  "permissions": {
    "allow": [
      "Bash(comebye:*)"
    ]
  }
}
`

// ClaudeAllowedTools is the --allowedTools value the Coordinator launch
// preset passes to claude (the same rule as claudePreset).
const ClaudeAllowedTools = "Bash(comebye:*)"

const claudePresetNote = `# Merge the JSON above into the coordinator's Claude Code settings:
#   ~/.claude/settings.json          (every session), or
#   <dir>/.claude/settings.local.json (sessions started in <dir>).
# Comebye's "Coordinator" launch preset already starts claude with
#   --allowedTools "Bash(comebye:*)"
`

const piPreset = `Pi has no allow list to add a command to: pi-config's permission gate
(extensions/permission-gate) allows obvious reads by a built-in grammar and
sends every other bash command to its judge (with --auto) or to a prompt.
To stop being asked for each comebye call, pick one:

1. Approve comebye calls as they come (the default; nothing to set up).

2. Start the coordinator with automatic judging (pi --auto, a flag of
   pi-config's permission gate). For Comebye's Coordinator launch preset
   set COMEBYE_COORDINATOR_ARGS="pi=--auto" (or -coordinator-args). The judge
   allows calls that serve what the user asked for; comebye only talks to
   the local Comebye server, which itself refuses everything but the
   coordinator's workflow.

3. Teach the gate about comebye. In pi-config, either add an allow rule in
   extensions/permission-gate/rules.ts for commands whose program is
   ` + "`comebye`" + ` (returning allow("comebye-cli")), or add this to the
   "Allow exceptions" of extensions/permission-gate/judge-rules.md:

   - **The comebye CLI.** ` + "`comebye`" + ` subcommands (agents, ledger,
     read, handoff, prompt, mark, unmark, brief, playbook, whoami) talk
     only to the local Comebye server, which limits agents to the
     coordinator workflow.

Comebye's "Coordinator" launch preset starts pi with
  --name <name> --no-skills --no-context-files --tools read,grep,find,ls,bash
like pi-config's scripts/pi-coordinator (no edit or write tools).
`

const genericPreset = `Allow the coordinator's harness to run shell commands that start with
` + "`comebye`" + ` without asking (an allow rule for the command prefix
"comebye"). The CLI only talks to the local Comebye server, which limits
agents to the coordinator workflow: reading, handing off, prompting,
marking and the briefing. Nothing else needs to be allowed.
`

// PresetNote is printed after a preset on stderr (claude only).
func PresetNote(harness string) string {
	if strings.ToLower(strings.TrimSpace(harness)) == "claude" {
		return claudePresetNote
	}
	return ""
}

// HarnessForKind maps a herdr agent kind to a playbook harness.
func HarnessForKind(kind string) string {
	switch kind {
	case "pi", "claude":
		return kind
	}
	return "generic"
}

// FirstPrompt is the coordinator's first prompt: short, so the playbook
// stays the single source of truth.
func FirstPrompt(kind string) string {
	return "You are the Comebye coordinator. Run `comebye playbook --harness " + HarnessForKind(kind) +
		"` and follow it: it explains your role and the `comebye` commands you act with. Then run `comebye whoami` and `comebye ledger`, and tell me briefly what you see."
}

// AgentArgs are the extra arguments the Coordinator launch preset passes
// to the agent's executable (herdr agent.start args), nil when none.
func AgentArgs(kind, name string) []string {
	switch kind {
	case "claude":
		// claude --help: --allowedTools, --allowed-tools <tools...>.
		return []string{"--allowedTools", ClaudeAllowedTools}
	case "pi":
		// pi --help: --name, --no-skills, --no-context-files, --tools;
		// mirrors pi-config's scripts/pi-coordinator (minus the herdr
		// tools: the coordinator acts through comebye). These are all
		// built into pi; extension flags (e.g. a permission gate's) are
		// per-install, added with -coordinator-args.
		return []string{"--name", name, "--no-skills", "--no-context-files", "--tools", "read,grep,find,ls,bash"}
	}
	return nil
}
