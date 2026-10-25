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

## Ranked hypotheses

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
