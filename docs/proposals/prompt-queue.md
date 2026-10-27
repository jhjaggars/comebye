# Queued prompts (`comebye prompt --queue`)

Status: built on `feat/prompt-queue` (based on `feat/agent-comms`).

## Problem

`comebye prompt AGENT TEXT` to an agent with a question open fails with
409 `dialog_open` (`promptGuard`) or `agent_blocked` (herdr), and the
text is lost. A coordinator had to resend by hand after the user
cancelled a question.

## Decisions (user, 2026-10-26)

| Question | Decision |
| --- | --- |
| Opt-in or default | Opt-in: `--queue`. A plain prompt behaves as before |
| Limits | 5 pending per target, 4 KB per text, 50 in total, 10 per minute per sender, 2 h TTL, identical pending text deduped |
| Sender told | One fixed-text notice when a prompt expires or is dropped (best effort, to an idle sender) |
| Visibility | `comebye queue`, `GET/DELETE /api/queue`, and a "N queued" list with Cancel above the composer in the app |

## What is checked against the code

- `promptAgent` (briefsched.go) reaches any host and runs `guardPrompt`
  locally; the queue sends through it, so mark-ask, briefing and callback
  prompts keep their own paths and the queue adds none of its own
  delivery code.
- `deliverCallbacks` runs from `observeLedger` on each fleet update.
  `deliverQueue` runs next to it, with the same rules: idle or done only,
  15 s spacing, a 2 h life, identity = agent id plus the name it had.
  `findAgent` and `canTakePrompt` are the shared pieces (callbacks use
  `canTakePrompt`).
- `selfGuard` is for path routes; the queue route checks `isSelf` itself.

## Behaviour

- `POST /api/queue {agent, name, text}` (hub only; a peer forwards agent
  calls to the hub) answers 202 with the queued item. The agent must be in
  the fleet now. Whether it can take the prompt is decided at delivery,
  and an attempt is made at once.
- Per target FIFO: one prompt per target per pass, at least 15 s after
  the previous send to that target. Only an agent that is `idle` or `done`
  is tried; the send then refuses with `dialog_open` while a dialog shows
  and the item stays pending (`tries` counts these).
- The prompt is stamped so the receiver can tell it from the user's: 
  `[Queued by <sender> via comebye at HH:MM UTC; held while you could not
  take it]` then the text. The sender name is reduced to `[A-Za-z0-9._/-]`.
  The text is the sender's own words, as in a plain prompt.
- Only text is ever sent (never keys), and only to an agent with no
  dialog showing, so a queued prompt cannot answer or dismiss a dialog.
- Agent missing from the fleet (host restarting): keep waiting until the
  TTL. Pane present under another name: settle `gone` at once, never send.
  Past the TTL: `expired`. Both queue the sender notice. `cancelled` by
  `comebye queue --cancel ID` or the app. Settled items stay visible 24 h
  (last 20) via `comebye queue --all`.
- Persistence: `queue.json` in the state directory (atomic, 0600). Pending
  items survive a hub restart; retry spacing and the rate limit are in
  memory. An unreadable file is not overwritten (the queue runs in
  memory).

## Security

- Any agent may queue for any other, as it may prompt it today; the limits
  above bound what can pile up. No agent can mark, answer or hand off
  through the queue.
- `from` comes from the self-asserted agent header (as `By` in the ledger).
- The sender notice is fixed text with the prompt id and target name only.
- Not done: no hop budget. A notice is not itself queued, so there is no
  ping-pong; plan-scoped messaging (agent-comms phase 4) would need one.

## Not done

- Delivery result is reported only on failure (the notice). A success
  notice would equal the callback for handoffs and is left out.
- Identity is pane id plus name until `aid` exists.
- The app lists and cancels but does not queue: its composer keeps the
  immediate "answer the question first" refusal.
