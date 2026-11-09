# The coordinator's fresh start (in-place context reset)

A coordinator's context only grows: every briefing, ledger read and report adds to it, and the longer it is the dearer and less sharp each turn gets. The **fresh start** clears it without moving the coordinator: the hub sends `/clear` into the coordinator's own pane, and a Claude Code `SessionStart` hook loads a **digest** into the new session. The pane, the agent id, the name and the role stay as they were; only the session id changes.

It works for **Claude Code coordinators** only (the hook is Claude's). A Pi or other coordinator is never cleared.

## What the coordinator gets: `comebye coordinator digest`

A Markdown digest of at most 24 KB (about 7k tokens), made from records, never from transcript text:

- the playbook pointer (`comebye playbook --harness claude`) and the commands to start with;
- the memory index it loads;
- the briefing, in its current text;
- the open to-dos, one line each with the id;
- the running, waiting, landing and reported-but-not-landed handoffs, one line of state each;
- the ledger events since the last fresh start (handoffs started, reported, removed, lost; newest 40, at most 48 hours back);
- the **tail** of `<state dir>/coordinator/state.md`, the coordinator's own notes (the last 8 KB, from a heading or a line start; the file itself can be much larger).

Every part has its own budget and is cut with a note when it does not fit. Run it yourself any time: `comebye coordinator digest`. The text opens with `comebye-digest:v1`, which the hub looks for to confirm that the digest reached the new session.

## What the hub does

- **Trigger.** At each new briefing the hub reads the coordinator's context size from its transcript (the last request's input plus cache tokens). Past the threshold (`-coordinator-reset-tokens`, default **150000**; `0` turns the automatic reset off) the reset is *due*.
- **When it runs.** As soon as the coordinator is idle (or done) with nothing queued for it. A due reset waits up to three hours; the next briefing decides again. Two automatic resets of one coordinator are at least 30 minutes apart.
- **Never while** the coordinator is working or blocked; a dialog or half-typed text is on its screen; or the hub holds a message for it: a queued prompt (`comebye prompt --queue`), an undelivered worker notice, a to-do answer notice, or a blocked-agent notice.
- **Never without the hook.** The hook leaves a short note (`comebye-hook:v1`) in a session when it starts; the hub clears a session only if the note is in it, so a coordinator started without the hook is never cleared into a blank session.
- **After `/clear`.** The hub waits for the new session id, gives the agent its name back (herdr drops an agent's name when its session changes), and checks that the digest is in the new session's transcript. It then records the reset in the ledger's audit trail (`coordinator.reset`), logs it, and keeps it in `coordinator-reset.json` (the last 20). The hook also shows a one-line notice in the pane: "Comebye: fresh start — your context was cleared and a digest (N KB) was loaded."
- **By hand.** *Fresh start…* in the coordinator's ⋯ menu in the app (it shows the context size, and refuses with the reason when the hub's guards say no), or `comebye coordinator reset [AGENT]` (the user's alone: it needs your login, `--login` or `COMEBYE_LOGIN`, and `COMEBYE_URL` pointing at the hub). A manual start ignores the threshold, not the guards.
- `comebye coordinator status` shows the last reset and any due one; with `--refresh` (your login) it reads each coordinator's context size and whether its session has the hook.

When the coordinator runs on a peer (the usual case: the hub is another machine) the hub drives it through the peer's host-side routes (`GET /api/agents/{target}/context`, `POST /api/agents/{target}/reset`); both are the user's, which is the hub's identity to a peer, and an agent token is refused.

## Installing the hook (the user's step)

1. On the host that runs the coordinator, with the `comebye` you want the hook to call:

   ```sh
   comebye coordinator install-hook
   ```

   This writes `<state dir>/coordinator/settings.json` (by default `~/.local/state/comebye/coordinator/settings.json`): a `SessionStart` hook with the `clear` matcher that injects the digest, and one for `startup|resume|compact` that leaves the short note. It does **not** touch `~/.claude/settings.json` or any project settings.
2. **Start the coordinator again.** The app's Coordinator preset passes that file to Claude with `--settings` when it exists, so only the coordinator has the hook. Claude reads `--settings` when it starts: a coordinator that is already running does not get the hook, and the hub refuses to clear it (`hook_not_installed`).
   By hand: `claude --settings ~/.local/state/comebye/coordinator/settings.json …`.
3. Check: `comebye coordinator status --refresh --login you@example.com` shows `hook active` for it.
4. The hub needs this version, and the peer the coordinator runs on too (the reset routes are on the peer).

`comebye coordinator install-hook --uninstall` removes the file. Compaction stays manual (`autoCompactEnabled` is not touched).

## Keeping `state.md` useful

The digest carries the tail of the coordinator's `state.md`, so what the coordinator needs after a fresh start belongs at its end: update it at every briefing, newest entries last, and keep decisions and gotchas short. The playbook says so.

## If something goes wrong

| Symptom | Meaning |
|---|---|
| `hook_not_installed` | The running session was not started with the hook (step 2 above). |
| `queued` | The hub holds a message for the coordinator; it goes out first. Try again in a minute. |
| `agent_busy` / `dialog_open` / `input_busy` | The coordinator is working, blocked, has a dialog open, or you are typing in its pane. |
| `clear_not_confirmed` | `/clear` was sent but no new session appeared within 30 s. Look at the pane. |
| "cleared, but the digest was not seen" | The new session started but the hook's digest is not in its transcript: the hook failed or the `comebye` it calls moved. Run `comebye coordinator digest` in the pane by hand. |
