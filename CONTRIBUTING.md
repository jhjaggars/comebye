# Contributing

Thanks for your interest. Bug reports, fixes and small focused features are
welcome.

- **Open an issue first** for anything bigger than a small fix, so we can
  agree on the approach before you spend time on it.
- **Keep it small.** comebye is deliberately a single Go binary with a
  dependency-free web UI (plain HTML/CSS/JS in `web/`, no build step). Please
  do not add Go module dependencies without discussing it (the only ones are `filippo.io/age` and `klauspost/compress`, for the transcript archive). The only JS
  tooling is the dev/CI-only browser harness in `tools/uitest` (pinned,
  never shipped, not part of the binary).
- **Before sending a pull request**, run:

  ```sh
  gofmt -l .            # must print nothing
  go vet ./...
  go test ./...
  node --check web/app.js web/sw.js
  # markdown rendering + sanitization in headless Chrome (Node 22+), against
  # a running instance; --agent/--screenshot also check a chat at 360px:
  node web/test/markdown.test.mjs http://127.0.0.1:8741/
  # the app in headless Chrome against a fake herdr (see "Browser checks"
  # below); run it for any change under web/ that affects what the user sees
  tools/uitest/run.sh
  # every theme in web/themes.js: WCAG AA contrast + all tokens defined
  node tools/uitest/contrast.mjs
  # spacing, radius and type use the tokens in docs/design-tokens.md; counts may only go down
  node tools/uitest/tokens.mjs
  ```

  CI runs the same checks.
- **Security-sensitive changes** (authentication, anything that widens what
  the API can do, new key inputs) need a clear explanation of the risk in
  the PR description. Report vulnerabilities privately; see
  [SECURITY.md](SECURITY.md).
- Write commit messages that explain *why*, not just *what*.

## Seeing the UI while you work (dev server + screenshots)

**For any UI change** (`web/`): start the dev server, take *before* and
*after* screenshots with `shot.mjs` at 390px and at desktop width (1280) in
both themes, **look at them** (open the PNG with your file-reading tool; it
shows images, or read the printed geometry) and list the screenshots in your
report. A passing `run.sh` does not tell you the UI looks right.

    tools/uitest/dev.sh start [--port P]   # prints the URL; runs in the background
    node tools/uitest/shot.mjs ROUTE [options]
    tools/uitest/dev.sh stop               # always stop it when you are done

`dev.sh` starts the fake herdr with fixture agents (a worker, a coordinator,
home agents, one blocked on a dialog, one marked ready to remove, long
names, a briefing, and a second host `peerbox`) plus comebye built from the
current checkout, on a free port, with all state in a `mktemp` dir. `web/` is
served from disk, so **edit JS/CSS/HTML and reload, no rebuild**; after a Go
change run `dev.sh restart`. `dev.sh status|url|logs` help when stuck. Each
checkout has its own server (recorded in the git-ignored
`tools/uitest/.dev-run`), so several agents can run side by side.

`shot.mjs ROUTE` opens the page in headless Chrome, runs the steps, writes a
PNG and prints console errors, failed requests and the geometry of the
selectors you ask about:

    shot.mjs /                                        # the agent list, 390px
    shot.mjs agent/w3:p1 --wait '#detail' \
        --inspect '#d-more'                            # is the ⋯ menu visible?
    shot.mjs agent/w3:p1 --width 1280 --light --out after-1280-light.png
    shot.mjs / --click '#btn-launch' --wait '#launch'  # open a sheet
    shot.mjs agent/w6:p1 --dom '#d-menu'                # print some DOM

Routes: a path or `#/agent/<pane>` (`agent/w3:p1` for short; a peer's is
`agent/peerbox/w21:p1`). Options: `--width N` (390, 768, 1280), `--dark` /
`--light`, `--click`, `--wait`, `--scroll SEL:PX`, `--full`, `--out`,
`--inspect SEL` (visible/hidden and x y w h), `--dom SEL`, `--a11y SEL`;
the header of `shot.mjs` has the full list. Fixture panes: `w1` alpha and
`w2` bravo (the `run.sh` agents), `w3` fix-detail-menu (working worker), `w4`
laharl (coordinator), `w5` notes-home, `w6` needs-input (blocked), `w7`
old-worker (marked), `w8` the long names (`tools/devserver/fixtures.go`).

Inputs must answer within a frame whatever the network does: a tap changes
the app's own state first (optimistic), the request follows, a refusal rolls
it back with an error, and a refresh in flight is laid over, never allowed to
undo it (`Overlay` in `web/app.js`). `drive.mjs` holds each request for 1.5 s
to check that (`ONLY=latency tools/uitest/run.sh` runs just those checks), and
`node tools/uitest/measure.mjs URL 0 300 1500` against a dev server prints
click-to-first-change times per input at the latencies you give it.

If a Playwright MCP server is configured for your session, point it at the
URL `dev.sh url` prints for interactive clicking; the CLI above is the
baseline that always works.

## Notes

- The only third-party code is the two vendored browser libraries in
  `web/vendor/`; its README has their versions, hashes, licenses and how to
  update them (`go test` checks the hashes).
- `go run tools/genicon/main.go` regenerates the PWA icons.

## herdr's socket protocol

- The socket answers **one NDJSON request per connection** (then closes),
  except `events.subscribe`, which turns the connection into a long-lived
  push stream. Every API call dials a fresh connection, and exactly one
  subscription connection is kept alive.
- `pane.agent_status_changed` subscriptions need a concrete `pane_id` and
  the subscription set is fixed per connection, so the server resubscribes
  (new connection) whenever the set of panes changes.
- If the server falls behind, herdr sends `events_lost` and closes the
  stream; the server reconnects with backoff, re-reads state, and tells
  browsers to resync.
- Event bursts are debounced into a single `agent.list` refresh; a 30s poll
  is the safety net.
- `herdr api schema --json` prints the full JSON Schema for the protocol.

## Browser checks against a fake herdr

A UI change that touches what happens after a send, switching agents, or the
raw/chat views should add a check to `tools/uitest/drive.mjs` (and a fake
behaviour to `tools/devserver` if it needs one) rather than only a source
test. CI runs this harness.

`web/app_test.go` only checks the source; `tools/uitest` drives the real
app in headless Chrome (a phone-sized viewport) against `tools/devserver`,
a fake herdr with two agents (`alpha`: Claude with a transcript, the chat
view; `bravo`: no session, the raw terminal view). It checks that a reply
shows after a prompt without re-selecting the agent, in both views, and that
opening a chat agent after a raw one shows its own chat.

    tools/uitest/run.sh [APP_DIR] [OUT_DIR]

Needs go, node, npm and google-chrome (`CHROME=/path` overrides; the first
run installs the pinned `playwright-core` into the git-ignored
`tools/uitest/node_modules`). `APP_DIR` is the checkout whose binary and
`web/` are tested (default: this one); point it at a scratch
`git worktree add --detach <dir> <commit>` to bisect a regression. Screenshots,
the browser console, API responses and the server logs go to `OUT_DIR`.
`FAKE_ARGS` passes flags to the fake herdr: `-status-lag`, `-reply-delay`,
and `-no-status` (herdr never reports working/idle, so the app cannot rely
on the status to know it should keep reading). `REPLY_WAIT_MS` sets how long
a reply may take to show (default 9000).

## Themes

Colours live only in `web/themes.js`: one `theme({...})` entry per theme
(a core palette; chips, code, marks and the rest are derived), and
`index.html` uses `var(--token)` only. To add a theme, add an entry (note the
palette's source and licence in a comment above it), then run
`node tools/uitest/contrast.mjs`: it fails if body, muted, chip or button
text is under 4.5:1, a focus ring or status dot is under 3:1, or a theme
lacks a token `index.html` uses. A palette colour that fails AA is nudged
(lightness only) and listed as `adjusted` in that script's output. Take
screenshots of a new theme with `shot.mjs ROUTE --theme ID`.
