# Contributing

Thanks for your interest. Bug reports, fixes and small focused features are
welcome.

- **Open an issue first** for anything bigger than a small fix, so we can
  agree on the approach before you spend time on it.
- **Keep it small.** comebye is deliberately a single Go binary with a
  dependency-free web UI (plain HTML/CSS/JS in `web/`, no build step). Please
  do not add Go module dependencies without discussing it. The only JS
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
  ```

  CI runs the same checks.
- **Security-sensitive changes** (authentication, anything that widens what
  the API can do, new key inputs) need a clear explanation of the risk in
  the PR description. Report vulnerabilities privately; see
  [SECURITY.md](SECURITY.md).
- Write commit messages that explain *why*, not just *what*.

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
