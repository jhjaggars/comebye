# The briefing board

`GET /api/board` is a read-only view that the app draws above the
coordinator's Markdown briefing (the Status page, and the top of the phone's
briefings screen). It is built only from records the hub already holds: the
to-do list, the ledger, the fleet's agents and the release items. Nothing is
stored, and nothing is guessed. The coordinator writes a headline, a reason
for each "Needs you" item and a few notes; the board draws everything else,
so the briefing no longer retypes state. The design is "A. Board plus note"
from the briefing-UX spike (Page `pg_a1112e4cfd`).

Who may read it: the user, and agents with the agent role (the coordinator
reads it before it writes a briefing); a tagged worker may not.

## Lane rules

Every item is in at most one lane, and the lane is the whole rule.

| Lane | An item is here when |
| --- | --- |
| **Needs you** (`needs_you`) | an open to-do item, other than a tagged release (below); or a ledger entry in state `running` whose agent is, right now, blocked on a dialog (`herdr` status `blocked`), shown as type `dialog`. Oldest first. An `answered` item is the coordinator's to relay, so it is not here. |
| **Running** (`running`) | a ledger entry in state `running` whose agent is not blocked on a dialog. |
| **Waiting on others** (`waiting`) | a ledger entry in state `waiting` (queued behind earlier steps; the detail names them), or an open release item in state `tagged` that is not dismissed (the tag is out and the hosts are upgrading; the Releases tab says what is next). |
| **Ready to land, or landing** (`landing`) | a ledger entry in state `landing`; or in state `finished` with no purpose of `research`, `plan`, `review` or `spike` and no mark: work that is meant to merge and has not. |
| **Landed, recent** (`landed`) | a ledger entry in state `landed`, or `ready_to_remove` after it had landed (`marked_from`), whose landing check (else finish time) is within 48 hours. |

Left out on purpose, and counted in `loose`: `reports` (finished work that
is not meant to land, waiting to be read) and `ready_to_remove` (marked
entries that never landed). `failed`, `lost` and `removed` entries are not
on the board.

**What the board cannot know.** "Waiting on others" is only what the records
say. A pull request waiting for `/lgtm`, an RFC that must come first, or an
idea parked on purpose is not a field; the coordinator says it in a note keyed
to the handoff or to-do that stands for it.

**A tagged release needing the user** ("press Bump hub") is a release item,
so it sits under *Waiting on others* here. The Releases tab, and the orange
dot, are the live source for what the user must do next; the coordinator's
"Needs you" line for it carries the to-do id, so its reason is shown with the
release.

## What the coordinator writes

Parsed from the latest briefing by the hub:

- **`headline`**: the first line of text, without Markdown marks.
- **`why`**: on a Needs you item, the text after its id in the briefing's
  "Needs you" section.
- **`note`**: a list item outside "Needs you" that starts with an id, then `:`
  or a dash. The id is a handoff id, a to-do id or a worker's name, bold, in
  backticks or bare: `- **h45500d313d85:** waits for the RFC`. A bullet that
  starts with an `h`/`t` id the board does not know is returned in
  `unkeyed_notes` and counted in the app, so it gets fixed. Any other
  Markdown stays in the briefing under the board.

## In the app

Needs you is open at first and is the same in both groupings. The rest is
grouped *by status* (the lanes above) or *by project* (the ledger's repository;
a to-do's project is that of the agent waiting on it, else "Not tied to a
project"); the choice is remembered in the browser. A Needs you item opens its
to-do on the To do page (desktop) or the To do card (phone).
