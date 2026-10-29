# Investigation: handoff briefs silently lost (Oct 29, kubevirt)

Investigation date: 2026-10-29 (worker `fix-brief-delivery-race`, ledger
entry on laharl). Reproduced the failure mode on a scratch pi over a pty;
the fix (verified delivery with idempotent retry) is in
`internal/server/deliver.go`.

## The failure

Four pi workers handed off to jjaggars-kubevirt (ledger entries hf023c837cf0d,
h9a70a01117e4, h946333b2d269, h2b6c86a37ef7, Oct 29) started but never
received their briefs: `comebye handoff` printed success, the sessions sat
idle at 0.0% context with pi's "Package updates available" banner, `comebye
read` said "no session transcript", and the ledger said `running` with no
error. Resending the same text later with `comebye prompt AGENT - < brief`
worked immediately. This is distinct from the Oct 22–25 failures
(kubevirt-subagent-errors.md), where herdr *refused* the prompt
(`agent_name_not_active`) and comebye recorded the error; here herdr
*accepted* the prompt and the text was lost after it.

## Reproduction (pi 1.4.0 over a pty, no tmux)

Driving `pi` from Python's `pty` module and writing input at various points
of its startup (`/tmp/pi-pty-test/experiment*.py` during the investigation):

| input written to the pty | result |
|---|---|
| text, no Enter, at exec (tty still canonical) | lands in the composer — survives |
| text + Enter at exec (before the TUI reads stdin) | **lost**: composer empty, no session file, agent idle, no error anywhere |
| bracketed-paste-wrapped text + Enter at exec | **lost** the same way |
| text + Enter ~1.5s in (after the alternate screen) | delivered: session file written, turn starts |

A control with a dumb reader (sleep, then switch the tty to raw mode, then
read) shows the kernel/tty always delivers the queued bytes across the
canonical→raw transition, ICRNL converting the `\r` Enter to `\n` — so the
loss is in pi itself, not the tty.

The mechanism: pi emits `session_start` (the herdr integration extension
reports idle, herdr marks the agent `interactive_ready`) during runtime
creation, *before* the TUI's input loop (`ProcessTerminal.start` →
`stdin.resume()`) is consuming input. A prompt written in that window is
read by pi once input processing begins, but the submission is silently
discarded — the composer ends up empty, no session JSONL is ever created
(pi only creates the file on the first submitted message), no turn runs,
and nothing errors. Text *without* an Enter survives in the composer; the
Enter is what makes pi attempt the submit-and-clear during the window.

On an idle machine the window is tiny (herdr's own `agent start` blocked
3.4s in a live check and the prompt then went through). On kubevirt, four
handoffs at once meant four concurrent pi boots (each loading ~15
extensions, MCP servers, skill discovery after `session_start`) on one
loaded machine: a window of seconds, wide enough for all four briefs to
land in it. The update banner in the evidence is incidental — it is a
plain notification in the chat area (`showPackageUpdateNotification`), not
a modal; it just proves the TUI had rendered while the brief was gone.

## Why the old code could not see it

`startWorker` sent the brief with a single unretried `agent.prompt`
(PromptOptions{}) after `waitForNamedAgent`. herdr's `agent.prompt`
reports success once the text and Enter are *written to the pane* — it
does not prove the agent read them (herdr's own docs say so). So the send
"succeeded", `PromptError` stayed empty, and the ledger recorded a healthy
running worker with an idle, brief-less agent.

## The fix

`deliverPrompt` (internal/server/deliver.go), now used for a handoff's
brief and a launch's first prompt:

1. Each send uses `agent.prompt` with `wait: {until: [working, blocked]}`.
   herdr's activity gate observes the agent start working (or returns
   `agent_prompt_stalled` after ~5s when it does not), so the answer says
   whether the agent took the brief, not just that bytes were written.
2. An unverified send is retried with backoff (3 tries by default) — but
   before every resend, `briefArrived` checks that the earlier send did
   not arrive late: agent status working/blocked, or the agent's session
   transcript (resolved like `comebye read` does) exists and contains the
   start of the brief. A stalled or timed-out wait does not prove the
   prompt was not delivered, so this check is what keeps a retry from
   pasting the brief twice.
3. `agent_name_not_active` (the launch deactivating between the ready
   wait and the send — the residual race from Oct 23) re-runs the
   wait-for-ready before the next try instead of failing.
4. `agent_blocked` fails fast: a dialog is open, resending cannot help.
5. If the brief still cannot be delivered, the worker result carries
   `prompt_error` ("... not delivered after 3 tries: ..."), the ledger
   entry records it in its `error` field (status stays `running`: the
   agent did start and can be rescued with `comebye prompt`), `comebye
   ledger` shows it in NOTES, and the app shows a "no brief" chip on the
   agent.

Also fixed here: after a reboot comebye used to exit when herdr's socket
did not exist yet (herdr's server starts only when the user connects
remotely), crash-looping into systemd's start limit. It now waits with
backoff for the socket to appear (main.go `waitForHerdr`) and still fails
fast when the socket exists but herdr is unreachable.

## Verification

- pty experiments above (before/after behavior of pi itself).
- `go test ./...`, `go vet ./...`.
- Unit tests in `internal/server/deliver_test.go`: send before ready then
  ready, silent drop then retry, late arrival not duplicated (status and
  transcript variants), bounded failure recording through a full handoff.
- Not verified live end-to-end against a real herdr+pi handoff: the race
  window on an idle machine is too narrow to hit (the live check above
  delivered on the first try); the mechanism is covered by the pty
  reproduction and the unit tests script herdr's answers.

## Open items

- herdr reports `agent_prompt_stalled` after its 5s activity gate; a
  slower-than-5s agent start (very loaded machine) could still produce a
  false stall. The late-arrival check makes that harmless (the retry is
  skipped when the brief shows up), but the stall itself is herdr-side.
- Claude Code and other kinds: the transcript check resolves sessions the
  same way `comebye read` does, but the late-arrival semantics were only
  exercised against pi's session format in tests.
