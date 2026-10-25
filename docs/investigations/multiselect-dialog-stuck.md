# Investigation: coordinator stuck on Claude's multi-select question dialog

Read-only investigation; nothing implemented. Branch `investigate/multiselect-dialog-stuck`.
Findings come from reading the code; **no live reproduction was done**, so the
cause in Q1 is the best-supported hypothesis, not a confirmed one.

## Q1. How answers reach a Claude dialog, and why text landed in "Other"

Three routes exist:

| Route | Code | Guard against a dialog |
|---|---|---|
| Tap-to-answer card: `GET /dialog`, `POST /answer` | `internal/server/dialog.go`, `internal/dialog/*` | Re-reads the screen, fingerprint check, whitelisted keys only |
| Key pad: `POST /keys` | `server.go` `handleKeys`, `herdr.allowedKeys` | Whitelist; no printable text beyond `1-9`, `y`, `n` |
| Free text: `POST /prompt` (app composer, `comebye prompt`) | `handlePrompt` -> `herdr.Prompt` -> `agent.prompt` | **Only herdr's refusal while `blocked`** (409 `agent_blocked`); the app composer also refuses if `state.ask` is set |

Claude multi-select is explicitly not tappable: `detectNumbered` sets
`Multi=true, Answerable=false` for Claude with the comment "Claude's multi-select
commit flow is not pinned down; use the pad" (`internal/dialog/screen.go`, ~line 481).
So the card shows read-only options and the user is pushed to "More keys...".

Likely chain of events:
1. Claude Code's AskUserQuestion dialog does not reliably make herdr report
   `blocked` (the app already knows this for Pi; the coordinator showed `done`).
2. `needsAnswer()` in `web/app.js` (~2103) and `currentDialog()` in
   `internal/server/dialog.go` only treat an agent as waiting when status is
   `blocked`, or `working` with a transcript ask. With status `done`/`idle` and an
   open question, **no card is shown and `/dialog` returns null**.
3. With no guard and no 409 from herdr, free text sent via `/prompt` (app composer
   after `state.ask` was not yet loaded, or `comebye prompt` from the CLI, which has
   no ask check at all) is typed into the focused row. The focused row in a
   multi-select is "Other", a text field in vim INSERT mode; Enter/arrows do not
   leave it, hence the frozen look. Only Escape does, and Escape twice cancels.

To confirm: check which route the user used and the coordinator's `agent_status`
at the time (server log for `POST /prompt` vs `/answer`).

## Q2. Detecting "blocked on a dialog"

Already partly there: the screen reader (`dialog.Detect`) and the transcript ask
(`transcript.Ask`, parsed from the unanswered `AskUserQuestion` tool_use) both
identify the dialog independently of herdr's status. The hole is that both are
**gated on status** (`blocked`, or `working`+ask). Fix: treat a pending transcript
ask as "waiting" whatever the status (`done`, `idle`, `working`, `blocked`), and
surface a distinct state "waiting on question" in `/api/agents`, the agent list
badge, and `comebye agents` / `status` output. The ask is cheap to detect
(transcript tail read); screen read is only needed to get the highlight.
Edge: a stale ask (tool_result never written because the session was killed) must
be validated against the screen: if no dialog is visible, report "stale question".

## Q3. UX / protocol for multi-select and multi-tab

Observed Claude layout (from the report): tab bar (`Investigate`, `Output`, `Submit`),
numbered checkbox rows, a free-text "Other" row, then non-option rows `Next` and
`Chat about this`. Per tab: Space toggles, Down/Up move, `Next` + Enter advances;
a final Submit tab confirms; Esc cancels everything (when not in INSERT mode).

Options:
- **A. Teach `dialog` the Claude multi-select flow** (set `Answerable=true`, map
  toggle -> `[down*n, space]`, submit -> move to `Next` + `enter`, after each key
  batch re-read and verify checkbox state). Tab 2+ and the final Submit tab are
  already handled for single-select ("Submit answers"/"Cancel" review). Cost:
  medium; needs real screen captures of the Claude layout as test fixtures
  (we have none for multi-select, only Pi's). Risk: UI drift across Claude versions.
- **B. Avoid the dialog.** The playbook (`internal/playbook/playbook.md`) tells
  coordinators to ask the user as plain chat text; add an explicit rule: "never
  use AskUserQuestion with multiSelect; use a numbered list in chat". Cost: tiny.
  Weakness: advisory; workers and non-coordinator sessions still raise dialogs.
- **C. Structured path via Claude Code hooks** (see Q5): intercept AskUserQuestion
  and answer from the app without keystrokes. Cleanest, biggest change.

## Q4. Guard rails (recommended regardless of A/B/C)

1. `POST /prompt` (server side, so the CLI is covered too) refuses with 409
   `dialog_open` when `pendingAsk` is non-nil, or `Detect` finds a dialog on the
   visible screen, whatever the status. Cost: one transcript read per prompt;
   skip the screen read when no ask. Add `--force` only to the CLI/API, not the app.
2. "Other" is never typed into by the app: already true for `/answer` and
   `/keys` (`Keys` refuses RoleOther; whitelist has no letters). Keep it.
3. Safe **reset dialog** action: `POST /api/agents/{t}/dialog/reset`:
   read the screen; if the footer shows INSERT (or the focused row is an input),
   send one `esc`; re-read; return the new dialog. Never send a second `esc`
   unless the user explicitly chose "Cancel question" (labelled with the
   consequence: "discards answers on all tabs"). One Esc per call, verified by
   the footer change (`Enter to select` + INSERT -> `Esc to cancel`).
4. The card must show for any pending ask regardless of status (Q2), always with
   a visible "Cancel question" and the live screen snapshot ("More keys" already
   has it), so a stuck dialog is visible and cancellable from the phone.
5. CLI parity (see memory "coordinator-first control"): `comebye dialog AGENT`,
   `comebye answer AGENT --toggle N --submit`, `comebye dialog reset AGENT`.

## Q5. Structured API in Claude Code

Not verified against the current Claude Code docs from here (I did not fetch them;
treat as to-check). From knowledge: Claude Code hooks can match tool names, so a
`PreToolUse` hook on `AskUserQuestion` could intercept the call, and a `Notification`
hook fires for permission/idle prompts. A PreToolUse hook can return a decision
(allow/deny/ask) with a reason that is fed back to the model; if the hook can also
return `updatedInput`, or the tool accepts pre-filled answers, the app could answer
without keystrokes. The plausible design: hook posts the question to Comebye, waits
for the app's answer, and returns it as the denial reason ("User answered: ...")
which the model reads as the answer. That bypasses the TUI entirely, at the cost of
a per-machine hook install (`herdr integration install claude` is the precedent) and
semantics that differ slightly from a real tool result. Worth a spike by checking
the hooks reference first.

## Recommendation

Do in this order, each independently shippable:
1. **Guards + detection (Q2, Q4.1, Q4.4):** server-side prompt refusal while a
   dialog/ask is open; show the card and a "waiting on question" state for a pending
   ask whatever herdr's status says. Fixes the root cause of this incident.
2. **Safe reset action (Q4.3)** in API, app, and CLI.
3. **Playbook rule (Q3-B):** no multiSelect AskUserQuestion; ask in chat.
4. **Spike on hooks (Q5)**; only if viable build C. Otherwise do A, with fixtures
   captured from a real Claude multi-select screen.

## Files that would change

- `internal/server/dialog.go`: `currentDialog` waiting rule; new reset handler.
- `internal/server/server.go`: `handlePrompt` refusal (`dialog_open`); route for reset.
- `internal/server/slim.go`, `internal/herdr/client.go` (maybe): derived "question" state.
- `internal/dialog/screen.go`, `resolve.go`: INSERT-mode detection; (A) multi-select answerable.
- `web/app.js`: `needsAnswer`, `sendPrompt` handling of `dialog_open`, reset/cancel buttons.
- `internal/cli/cli.go`, `docs/api.md`: `dialog`/`answer`/reset commands; new endpoint docs.
- `internal/playbook/playbook.md`: no-multiSelect rule.
- Tests: `internal/dialog/dialog_test.go` (fixtures), `internal/server/dialog_test.go`.
