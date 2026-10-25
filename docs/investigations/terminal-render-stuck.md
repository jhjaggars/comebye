# Investigation: phone app terminal view stuck rendering

Report: the terminal view stopped updating, typing "ok" did nothing, and
force-quitting the app was the only recovery. No logs, device or OS version.
The cause is **not confirmed**; this lists what the code shows and ranks it.

## How the view works (end to end)

- There is no terminal emulator. The app is a PWA (`web/app.js`). "Terminal"
  is the raw-output fallback: a `<pre id="output">` filled with
  `el.textContent = read.text` (`refreshOutput`), so ANSI, wide characters and
  truncated escape sequences cannot throw in the app. The text comes from
  `GET /api/agents/{t}/output?lines=400` (`internal/server/server.go`
  `handleOutput`), which calls herdr `pane.read` (`internal/herdr/client.go`
  `ReadPane`) with a 15 s server-side timeout.
- Output is polled, not pushed. `startPolling` runs every 4 s and only calls
  `refreshOutput` while the agent is `working` or `blocked`. SSE
  (`/api/events`) carries only agent snapshots; it has a watchdog and
  reconnect (`startWatchdog`, `scheduleReconnect`, `resume`).
- Typed text goes to `POST /prompt` (`sendPrompt`), keypad keys to
  `POST /keys` (`sendKeys`).

## Findings

1. **No client-side request timeout existed.** `api()` called `fetch` with no
   `AbortSignal`, anywhere in the app. On a phone, a connection that dies
   half-way (network switch, wake from sleep, Tailscale path change) leaves a
   fetch pending indefinitely.
   - A pending `refreshOutput` read never settles; the 4 s `setInterval` keeps
     starting more, which pile up against the browser's six HTTP/1.1
     connections per origin (the same exhaustion `suspendEvents` documents for
     cached EventSources). The view shows its last text forever.
   - A pending `POST /prompt` never returns, so `state.sending` stays `true`
     (`sendPrompt` returns early on it) and the send button stays disabled.
     Typing "ok" then does nothing, with no toast. This matches the report.
   - Only killing the page cancels the requests, which matches the force-quit.
2. **A blocked agent refuses prompts.** herdr rejects prompts while an agent
   is `blocked`; the server returns 409 and the app toasts "agent is waiting
   for input - answer the dialog or press Esc first". If the worker was at the
   folder-trust prompt, "ok" was refused for that reason, and the toast lasts
   2.6 s and is easy to miss. This explains "ok did nothing", not the frozen
   output by itself.
3. **Staleness when not `working`/`blocked`.** Output refreshes only on the
   4 s poll in those two states, on `resume()`, after sends, and once when the
   agent settles. If herdr reports `idle`/`unknown` while the pane still
   changes, the view goes stale, and the raw view has no refresh or reconnect
   control.
4. **Alt-screen TUIs.** The view reads `recent_unwrapped` scrollback, which
   may not contain the live screen of an alt-screen app (Claude Code TUI);
   `source=visible` is used only for the answer-card snapshot. Unverified:
   depends on herdr behaviour.
5. Ruled out by reading: parse/render exceptions (plain `textContent`),
   unbounded buffers (server caps `lines` at `ReadLines`, client replaces the
   text each time), SSE stall (watchdog recycles a stream silent for 45 s;
   heartbeat every 15 s).

## Update: user is on iOS and could not see the prompt they sent

New facts: iOS, current build; the view looked stuck and the sent "ok" was
not visible either.

6. **The raw terminal view has no local echo.** `addPending()` returns early
   in terminal mode (`if (!state.chat || state.chat.terminal) return null`),
   and `#chat` is hidden there anyway. A sent prompt appears only when the
   `setTimeout(refreshOutput, 600)` after the POST succeeds and the pane
   shows it. "Can't see my prompt" is exactly what a stalled read, a hung
   POST or a stale pane produces; it needs no render fault.
7. **iOS suspends and half-kills connections.** On `visibilitychange` hidden
   the app closes SSE and clears timers (`suspendEvents`), and `resume()`
   reopens on visible. iOS (the Home Screen app especially) can drop a socket
   during suspension without rejecting the in-flight `fetch`, which then
   hangs forever. With no timeout, a read begun just before backgrounding
   wedged the view; a prompt POST hung the same way leaves `state.sending`
   set. This is the strongest candidate and fits "stuck, input does nothing,
   force-quit recovers". The new overlap guard would have blocked recovery
   too, so `resume()` now clears `outputBusy` before re-reading.
8. **Viewport/keyboard.** `#detail` is `100dvh` with `overflow: hidden`; the
   raw view autoscrolls only if it was at the bottom before the text was
   replaced. An iOS keyboard open/close resize could leave the newest lines
   (and the sent prompt) scrolled out of view with no fault. Low confidence;
   the app has no `visualViewport` handling.

Hypothesis 1 below is raised to medium-high by findings 6 and 7. Added
hypothesis 3b: keyboard/viewport resize hides the newest lines (finding 8);
a screenshot, or whether scrolling down shows the prompt, would settle it.

## Update 2: wrong agent's terminal shown after switching (root cause found)

User: Home Screen app, no keyboard or rotation change; after switching to
another agent the terminal view showed the same content from the wrong agent.

9. **The raw view was never reset when switching agents (confirmed in code).**
   `showTerminal()` hides `#chat` and shows `#output`, and nothing ever undid
   it: `openDetail()` only reset `state.chat`. So once any agent fell back to
   the raw view (no transcript, or "Show raw terminal"), every agent opened
   afterwards kept `#chat` hidden and `#output` visible with the previous
   agent's text. For an agent with a transcript `state.chat.terminal` is false,
   so `refreshOutput` never ran and the stale text stayed forever - a
   view that looks frozen, shows the wrong agent, and ignores a sent prompt
   (its echo goes to the hidden chat). This explains the whole report
   better than the network hypotheses; those remain real but secondary.
10. **A late read could overwrite the view.** `refreshOutput` did not check,
    after its `await`, that the agent was still selected (and its 404 handler
    closed whichever view was open). A slow read for A resolving after the
    switch to B painted A's text.
11. The service worker has no fetch handler and caches nothing
    (`web/sw.js`), so it is ruled out. `/output` responses carry no cache
    headers; reads now use `cache: "no-store"` anyway.

Fixes (all in `web/app.js`, one button in `web/index.html`):
- `resetTerminalView()` runs from `openDetail()` and `closeDetail()`: aborts
  the in-flight read, clears and hides `#output`, restores `#chat`.
- `refreshOutput()` captures its pane, takes an `AbortController`, and drops
  the result (and errors) if the agent or mode changed. `api()` accepts a
  `signal`, combined with the timeout.
- Header refresh button (`#d-refresh`, `refreshView()`): aborts reads, clears
  `state.sending` and the busy flag, reconnects the live stream, refetches
  the agent list, then reloads the chat from scratch or re-reads the output.
- Local echo of a sent prompt in the raw view (`echoInOutput`), replaced by the
  next read.
- `resume()` aborts an orphaned read instead of only clearing the flag.
- Tests: `TestAppTerminalViewIsKeyedByAgent` (source-level, as the repo has no
  JS runner). Not verified in a browser or on an iOS device.

Ranking after this: hypothesis 0 (below, added) is the likely cause of the
wrong-agent content; hypothesis 1 remains plausible for a read that hangs.

Hypothesis 0 (highest, for the wrong-agent symptom): the raw view was not
reset on agent switch (finding 9), and late reads could overwrite it
(finding 10). To reproduce: open an agent with no transcript, go back, then
open one that has a transcript.

## Update 3: reply does not appear after a send until the agent is re-selected

Report: after submitting a prompt, the response sometimes shows only after
going back to the list and re-opening the agent.

12. **After a send, only one re-read was scheduled; everything later depended
    on the status snapshot.** `sendPrompt` schedules a single `loadChat(false)`
    at 800 ms (raw view: `refreshOutput` at 600 ms). Further updates come from
    the 4 s poll, which ran only while `agent_status` was `working` or
    `blocked` (`startPolling`). Status arrives over SSE. If the snapshot lags
    the send (a fast reply, a status herdr reports late or never), or SSE is
    down (iOS resume), the poll does nothing and the one-shot re-read ran
    before the reply existed. The stream stays quiet until re-selecting runs
    `loadChat(true)`. The "working -> settled" catch-up in the SSE handler
    cannot fire either, since it needs the status to have been seen as working.
    History: the one-shot send re-read and the status-gated poll date from the
    first commit (`3dafe72`) and `f47908c`; not a recent edit, but the
    window got wider as polling moved to status gating and as iOS made SSE
    less reliable.
13. **My no-overlap guard (`c62d170`) could swallow the post-send raw read.**
    `refreshOutput` returned silently while `outputBusy`, so a read hung on a
    dead socket (up to the 30 s timeout) made the 600 ms post-send read a no-op.
    Chat view has no such guard (`loadChat` is not guarded).
14. 409 "agent is waiting" drops the pending bubble and toasts; it does not
    clear timers or poll state, so it is not the cause.

Fix: `state.pollUntil` is set for 2 minutes (`POLL_AFTER_SEND_MS`) after a
prompt, key or answer send, and the poll runs while it is in the future
regardless of status. `refreshOutput(true)` (post-send reads) aborts a stuck
read instead of being skipped. Test: `TestAppPollsAfterSend` (source-level).
Not reproduced in a browser.

## Ranked hypotheses (1 and below predate hypothesis 0)

| # | Hypothesis | Confidence | Data that would settle it |
|---|-----------|-----------|---------------------------|
| 1 | Hung fetches with no timeout froze the poll and the send state (finding 1) | Medium | Server log showing whether the output/prompt requests arrived; whether the phone had just switched networks or slept |
| 2 | Agent `blocked` at the trust prompt, so the prompt got 409 (finding 2) | Medium for "ok did nothing", low for "stuck" | Whether a red toast appeared; agent status at the time |
| 3 | Status not `working`/`blocked`, so no polling (finding 3) | Low-medium | Status shown in the app header; whether reloading fixed it |
| 4 | Alt-screen content absent from the scrollback read (finding 4) | Low | `comebye read` with `source=visible` vs `recent_unwrapped` on a running Claude TUI |

## Fix implemented (covers hypothesis 1)

- `api()` aborts after 30 s (`API_TIMEOUT_MS`) with "timed out - check the
  connection", via `AbortSignal.timeout`. Launch POSTs, which wait for the
  agent to be ready, get 180 s. `state.sending` and the poll now recover on
  their own and the user sees a toast instead of silence.
- `refreshOutput()` no longer overlaps itself, so slow reads cannot stack.
- A source-level test in `web/app_test.go` guards both (the repo has no JS
  runner). I did not reproduce the hang in a browser.

## Recommended follow-ups (not done)

- Add a refresh/reconnect control to the raw terminal view, and poll it while
  open regardless of agent status (finding 3).
- Make the 409 "waiting for input" hint persistent rather than a 2.6 s toast.
- The watchdog's `/api/agents` probe in `startWatchdog` also has no timeout.
- Consider reading `source=visible` for alt-screen agents (finding 4).

## What to ask the user

- Phone model, OS version, browser or Home Screen app, and app version
  (`X-App-Version`).
- Was the agent shown as blocked, working or idle? Did any toast appear on "ok"?
- Did the connection dot show offline? Had the phone switched networks, slept,
  or the app been backgrounded just before?
- Was it the raw terminal view ("Show raw terminal") or the chat view?
- Did leaving the agent and re-entering it, or reloading, help?
