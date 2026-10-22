# Contributing

Thanks for your interest. Bug reports, fixes and small focused features are
welcome.

- **Open an issue first** for anything bigger than a small fix, so we can
  agree on the approach before you spend time on it.
- **Keep it small.** comebye is deliberately a single Go binary with a
  dependency-free web UI (plain HTML/CSS/JS in `web/`, no build step). Please
  do not add Go module dependencies or a JS toolchain without discussing it.
- **Before sending a pull request**, run:

  ```sh
  gofmt -l .            # must print nothing
  go vet ./...
  go test ./...
  node --check web/app.js web/sw.js
  # markdown rendering + sanitization in headless Chrome (Node 22+), against
  # a running instance; --agent/--screenshot also check a chat at 360px:
  node web/test/markdown.test.mjs http://127.0.0.1:8741/
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
