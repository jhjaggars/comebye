# Contributing

Thanks for your interest. Bug reports, fixes and small focused features are
welcome.

- **Open an issue first** for anything bigger than a small fix, so we can
  agree on the approach before you spend time on it.
- **Keep it small.** herdr-phone is deliberately a single Go binary with a
  dependency-free web UI (plain HTML/CSS/JS in `web/`, no build step). Please
  do not add Go module dependencies or a JS toolchain without discussing it.
- **Before sending a pull request**, run:

  ```sh
  gofmt -l .            # must print nothing
  go vet ./...
  go test ./...
  node --check web/app.js
  ```

  CI runs the same checks.
- **Security-sensitive changes** (authentication, anything that widens what
  the API can do, new key inputs) need a clear explanation of the risk in
  the PR description. Report vulnerabilities privately; see
  [SECURITY.md](SECURITY.md).
- Write commit messages that explain *why*, not just *what*.
