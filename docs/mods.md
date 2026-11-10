# Claude Code mods

`comebye mod` ships small [Claude Code mods](https://code.claude.com/docs) with the binary: a status band, instant commands and warn-only guard notices (`comebye-hub`), and an early-access coordinator window (`comebye-coord-window`). They are **early warnings and conveniences only**: nothing here blocks a tool call or replaces the sandbox. They need Claude Code 2.1.321 or newer, whose mod API is early access; the Claude agents only.

## Install

```sh
comebye mod install                       # comebye-hub
comebye mod install --with coord-window   # also the coordinator window (the user's step, on the coordinator's host)
comebye mod status                        # files current/outdated/edited, marketplace, installed plugins
comebye mod uninstall                     # only what comebye wrote
```

`install` writes the embedded mods to `~/.local/share/comebye/mods` (`--dir`, or `$XDG_DATA_HOME/comebye/mods`), adds that folder as the local marketplace **`comebye-mods`** and installs `comebye-hub@comebye-mods` with `claude plugin …`. Claude reads a plugin whose marketplace entry is a relative path from the folder itself, so after `comebye upgrade` a second `comebye mod install` only rewrites files; run `/reload-plugins` in a running Claude.

- Idempotent. A file you edited is left alone (`--force` replaces it and keeps yours as `.bak`); a file an older comebye wrote is updated.
- It only touches its own folder, the marketplace `comebye-mods` and the plugins `…@comebye-mods`. Other plugins and marketplaces are never listed for change, updated or removed. `--no-claude` writes or reads the folder only.
- Nothing is written to `~/.claude/settings.json` except what `claude plugin marketplace add` and `claude plugin install` write themselves.

## comebye-hub

**The band.** One quiet line above the prompt, drawn only when something is pending: to-dos waiting on this agent, hub notices queued for it, its handoff's landing or CI state, and for a coordinator the briefing's headline. It asks `comebye mod band --json` (which uses the agent's token through the normal CLI; no new API) every 30 seconds (`poll_seconds`, at least 30) and after a turn or a command. After three failures in a row (not an agent, no server) it asks every tenth tick.

**Instant commands**, no model turn; the comebye CLI's output is shown to you: `/todo` (list; `/todo add …`, `/todo show ID`), `/handoff …`, `/show FILE`, `/queue`, `/ledger`, `/brief`. Arguments are split with shell-like quoting and passed as they are, with no shell. Output is cut at 6,000 characters.

**The guard** warns, never blocks: a toast and a line in the band for 2 minutes. Intent rules the sandbox cannot see: `git add -A` / `.`, a force-push, `gh pr comment|merge`, and a push to an owner that is not yours (`own_owners`, comma-separated; empty: only a remote named `upstream`). Quoted text is ignored.

## comebye-coord-window (early access, a coordinator)

A pilot beside [coord-reset](coordinator-reset.md), which stays the fallback. Past `trim_tokens` (default 120,000) at the end of a turn, and at most every `cooldown_minutes` (20), it asks for one `$.session.compact()` and answers it itself, with no summarizer call: the `comebye coordinator digest` plus the last `keep_turns` (10) prompts and everything after them, cut at a prompt so a tool call stays with its result. The pane, the session and the id stay; nothing is cleared. Each trim rewrites the prompt cache once, which is why it trims in steps, not every turn. `/window` shows its state (sizes only).

It does nothing unless the agent is a coordinator, Claude Code is 2.1.321 or newer, and nothing above it takes over compaction: if a managed profile (sec-default) skips user compaction hooks, the engine compacts normally once, the mod sees its hook did not run and turns itself off for the session. It is **only installed on the live coordinator by the user**.
