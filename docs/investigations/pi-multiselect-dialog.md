# Investigation: Comebye does not handle pi's multi-select question dialogs

Read-only investigation; nothing implemented. Branch
`investigate/pi-multiselect-dialog`.

Symptom (from the user): when pi shows a multi-select `ask_user_question`
dialog, the phone app and CLI show the **default read-only dialog card**
("Tap-to-answer isn't available for this dialog: use More keys…") instead of
a tappable multi-select card.

## How it was reproduced

No tmux (it changes key handling and screen capture). Instead, faithful to
the real path: a scratch herdr pane (`w2M:p2`, split from this agent's own
pane, cwd `/tmp`) running a throwaway pi agent (`herdr agent start pimulti
--kind pi`), prompted to call `ask_user_question`. Screens were captured with
`herdr pane read --source visible --format text` — the exact call Comebye's
`ReadPane(pane, "visible", 0)` makes. The pane was closed afterwards.

pi version 1.4.0. Captures (also used as test fixtures) are in
`internal/dialog/testdata/`:

| File | State |
|---|---|
| `pi_multi_initial.txt` | single-question multi-select, highlight on row 1 |
| `pi_multi_toggled.txt` | Apple ticked `[✔]` |
| `pi_multi_on_other.txt` | highlight on the free-text row (label vanishes: `❯ 4. [ ]`) |
| `pi_multi_other_typed.txt` | text typed into the free-text row: `❯ 4. [✔] hello` |
| `pi_multi_on_submit.txt` | highlight on the `Submit` row |
| `pi_multi_answered.txt` | after submitting (dialog gone, agent `done`) |
| `pi_multi_cancelled.txt` | after Esc (question dismissed) |
| `pi_single_initial.txt` | single-select question for comparison |
| `pi_multi_two_questions.txt` | two-question ask, tab 1 (multi-select) |
| `pi_multi_two_q_toggled.txt` | tab 1 with a box ticked, tab marker `■` |
| `pi_multi_two_q_on_next.txt` | highlight on the `Next` commit row |
| `pi_multi_two_q_tab2.txt` | tab 2 (single-select question inside the ask) |
| `pi_multi_two_q_answered.txt` | after the last question was answered |

## The real pi layouts (v1.4.0)

Single-question multi-select:

```
  Fruit

 Which fruits do you like?

❯ 1. [ ] Apple
  keeps the doctor away
  2. [ ] Banana
  yellow
  3. [ ] Cherry
  red
  4. [ ] Type something.
  Submit

 Enter to select · ↑/↓ to navigate · Space to toggle · n to add notes · Esc to cancel · Ctrl+] to collapse
```

Two-question ask (tab 1 of 2):

```
 ←  □ Size   □ Speed   ✓ Submit  →

 What size?

❯ 1. [ ] Small
  The smallest option
  ...
  Next

 Enter to select · ↑/↓ to navigate · Space to toggle · n to add notes · Tab to switch questions · Esc to cancel · Ctrl+] to collapse
```

Single-select question (for comparison):

```
  Color

 Pick one color

❯ 1. Red
     stop
  2. Green
     go
  3. Blue
     sky
  4. Type something.

 Enter to select · ↑/↓ to navigate · n to add notes · Esc to cancel · Ctrl+] to collapse
```

Facts pinned down by driving the live dialog:

- **Multi-select descriptions sit at the same indent as the row text**
  (column 2 = the marker column + 2). Single-select descriptions sit deeper
  (column 5, aligned under the label). The checkbox `[ ] ` prefix is what
  pushes the multi-select label right without moving its description.
- The commit row is unnumbered and unmarked: `Submit` for a single-question
  ask, `Next` for every tab of a multi-question ask.
- The free-text row is `4. [ ] Type something.`; focused it renders as
  `❯ 4. [ ]` (label gone), and typing shows inline as `❯ 4. [✔] hello`.
  **It is not a vim editor**: there is no `-- INSERT --`, arrows and Enter
  keep working from the typed state (verified live). So pi needs no
  `insert`/`reset` handling, unlike Claude.
- Arrow keys **wrap** (up from row 1 lands on Submit). Space toggles, Enter
  on the commit row submits, Esc cancels (verified: agent reported the
  question dismissed), `n` opens a notes editor (must never be sent), Tab
  switches questions in a multi-question ask.
- Answering the **last** question of a multi-question ask submits the whole
  questionnaire immediately (no separate review tab was shown).
- Tab bar glyphs are pi's `□` (unanswered), `■` (answered), `✓ Submit` —
  Claude's are `☐`/`✔`/`☒`.
- While the question is open, herdr reports the agent `blocked`, and the
  pi session transcript carries the open ask (`transcript.Read` parses it:
  header/question/multiSelect/options confirmed against the live session).

## What the current code does with these screens

Ran all captures through the current parser (temporary test harness):

- **`Detect(screen, "pi")` returns nil for every multi-select capture.**
  Single-select captures work: `pi_question`, answerable, correct keys
  (`enter`, `down enter`, …), tab 2 of a multi-question ask included.
- `Resolve` then falls back to the transcript-only `KindAsk` dialog
  (`internal/dialog/resolve.go:43`): options without checkboxes or
  highlight, `Answerable=false` — the read-only "default" card the user
  sees (`web/app.js` `ask` case, ~line 2281).
- `promptGuard` (`internal/server/dialog.go:113`) does not fire: with
  `Detect` returning nil the resolved dialog is `KindAsk`, and the guard
  exempts `KindAsk`. Herdr itself refused prompts while `blocked` in this
  reproduction, but herdr's refusal is known to be unreliable (the Claude
  incident), so the server-side guard should work for pi too.

## Root cause

`Detect` never reaches the multi-select rows. Tracing
`pi_multi_initial.txt` through `internal/dialog/screen.go`:

1. The highlighted row `❯ 1. [ ] Apple` matches `numberedRe`, so
   `detectChevron` → `detectNumbered` (screen.go:330) → `collectRows`
   (screen.go:151, `numbered=true`).
2. `collectRows` stops after the **first row**: the description line
   `  keeps the doctor away` is at indent 2, and `isDesc` requires
   `indentOf(l) > c+2` (screen.go:165) — strictly deeper than the row-text
   column. The line is neither a numbered row nor a description, so the
   list ends → `len(rows) < 2` → `detectNumbered` returns nil
   (screen.go:437) → `Detect` returns nil. **This is the primary bug.**
3. Two further divergences would still break answering if only (2) were
   fixed:
   - the commit row reads `Submit`, but `collectRows`' `isRow` accepts only
     a bare `Next` (screen.go:160), so the list would end before it and
     `hasNext` stays false → `Answerable = hasNext && allCheck` = false for
     pi (screen.go:481);
   - `mergeAsk` keys on `KindPiQuestion` and already contains pi
     multi-select support (`resolve.go:180`: `d.Multi && d.Kind ==
     KindPiQuestion && d.Selected >= 0 → Answerable`), i.e. the downstream
     machinery (mergeAsk, `Keys` toggle/submit, server, app card) is ready —
     only the screen parser never produces the dialog.
4. Minor: the tab-bar detector looks for Claude's glyphs `☐`/`✔ Submit`/`☒`
   (screen.go:464), not pi's `□`/`■`/`✓`; it is only used on the claude
   branch today, but any pi-specific work should not rely on it.

## Proposed fix

Extend the generic numbered-list path in `internal/dialog` (the smallest
change that makes pi multi-select work end-to-end through the existing
mergeAsk/Keys/server/app machinery), scoped so Claude and other agents
cannot regress:

1. `collectRows` (`screen.go`): treat a line at exactly the row-text column
   (`indentOf == c+2`) as a description when it directly follows a
   **checkbox** row (the row's label starts with one of the `checkboxes`
   prefixes). Deeper-indented lines stay descriptions as today; unchanged
   for lists without checkboxes (pi gate, Claude permission wraps).
2. `collectRows`/`detectNumbered`: accept a bare `Submit` row as the commit
   row (`RoleNext`) the way `Next` is accepted — for pi questionnaires
   only, so a generic numbered list ending in `Submit` (other agents)
   cannot become unanswerable through the `default:` branch of
   `detectNumbered`.
3. Tests from the captured fixtures: detect each `pi_multi_*` state
   (kind `pi_question`, `Multi=true`, checkboxes, `Submit`/`Next` role,
   selected index, focused-other row), `Resolve` with the transcript ask
   (merged, answerable, `Questions` count), and `Keys` mapping
   (toggle → arrows+space, submit → arrows+enter on the commit row,
   cancel → esc). Single-select fixtures pin the current behavior.

Alternative considered: a dedicated `detectPiMulti` parser modeled on
`claude_multi.go`, triggered by the `Space to toggle` footer. Rejected as
the first choice because `detectNumbered` + `mergeAsk` already implement
almost all of it; a second parser would duplicate the numbered-row,
checkbox and other-row logic. The dedicated parser remains the fallback if
the scoped generic change proves riskier than expected.

No server or web changes should be needed: `currentDialog` already reads
the screen for a `blocked` pi agent or an open transcript ask, `mergeAsk`
already un-mutes pi multi-select, and `web/app.js` already renders
`multi` dialogs with toggle/submit (built for Claude's, agent-agnostic).

## Test plan

- `go test ./internal/dialog/` with the new fixture-driven cases; existing
  Claude/pi cases must stay green (the scoping in fix 1–2 exists exactly
  for that).
- `go build ./... && go vet ./... && go test -count=1 ./...`.
- Live check with a fresh scratch pi agent: open a multi-select, verify
  `comebye`'s dialog endpoint returns a tappable card, toggle + submit from
  the API, confirm the agent received the answer.
