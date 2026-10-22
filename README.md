# Comebye

Your [herdr](https://herdr.dev) coding agents, on your phone.

Comebye is a small web app you install on your phone's home screen. It shows
every agent herdr is running, live: which ones are working, which are done,
and which are waiting on you. Open one to read its conversation as a chat,
send it a message (or a screenshot), answer its questions with a tap, or stop
it. You get a push notification when an agent finishes or needs input.

It is a single Go binary that talks to herdr's local socket, so your phone and
your terminal drive the **same** agents; there is nothing to sync. It is
reached over your private [Tailscale](https://tailscale.com) network, never
the public internet.

Beyond watching agents, it can:

- **start new agents** in any project directory, from the phone;
- **hand work to workers**, each on its own branch in its own git worktree,
  and keep track of what they finished;
- **clean up** finished sessions after you confirm, keeping their branches;
- **span several machines** in one list, with one of them acting as the hub;
- let a **coordinator** agent (Claude Code, Pi or others) organize work for
  you and leave you a briefing for when you come back.

Chat view works for Claude Code and Pi; other agents show their terminal output.

> **Comebye is effectively a remote shell in a browser.** Anyone who can use
> it can make your agents run commands as you. Read the
> [security model](docs/security.md) before exposing it.

## Getting started

You need herdr and Tailscale on the machine running your agents (with
[HTTPS certificates](https://tailscale.com/kb/1153/enabling-https) enabled
for your tailnet), Tailscale on your phone, and Go 1.25+ to build.

**1. Install**

```sh
GOBIN=~/.local/bin go install github.com/jhjaggars/comebye@latest
```

**2. Say who may use it.** Comebye only admits the Tailscale logins you list.
Use the one your phone is signed in as (`tailscale status` shows it):

```sh
mkdir -p ~/.config/comebye
echo 'COMEBYE_TS_ALLOW_LOGIN=you@example.com' > ~/.config/comebye/env
chmod 600 ~/.config/comebye/env
```

**3. Run it as a service** (Linux with systemd):

```sh
mkdir -p ~/.config/systemd/user
curl -fsSL https://raw.githubusercontent.com/jhjaggars/comebye/main/deploy/comebye.service \
  -o ~/.config/systemd/user/comebye.service
systemctl --user daemon-reload
systemctl --user enable --now comebye
```

**4. Put it on your tailnet.** It listens on `127.0.0.1:8741`; Tailscale
Serve adds HTTPS and your identity:

```sh
tailscale serve --bg 8741
```

Never use `tailscale funnel` for this; that makes it public.

**5. Open it on your phone** at `https://<machine>.<tailnet>.ts.net`, add it
to your home screen, then tap the bell to turn on notifications. On iPhone
notifications need iOS 16.4+ and the home-screen app, not a Safari tab.

[docs/setup.md](docs/setup.md) covers the rest: notification options,
connecting several machines, and every setting.

## Documentation

- [Setup](docs/setup.md): install, notifications, multiple hosts, configuration,
  upgrading from herdr-phone
- [Agents, workers and coordinators](docs/agents.md): launching agents,
  handoffs and cleanup, coordinators and the `comebye` command line, Claude
  Code support, limitations
- [Security model](docs/security.md) and [SECURITY.md](SECURITY.md) (reporting
  vulnerabilities)
- [API](docs/api.md): the JSON API behind the app

## Development

```sh
go build ./... && go vet ./... && go test ./...
gofmt -l .                       # must print nothing
node --check web/app.js web/sw.js
go run . -insecure-no-auth       # local only: http://127.0.0.1:8741, needs herdr running
```

The server is Go with no module dependencies; the web app is plain
HTML/CSS/JS in `web/` with no build step, embedded into the binary. See
[CONTRIBUTING.md](CONTRIBUTING.md) before sending changes.

## License

Copyright 2026 Jesse Jaggars. Licensed under the
[Apache License, Version 2.0](LICENSE).

The web app bundles marked (MIT) and DOMPurify (Apache-2.0 / MPL-2.0) in
`web/vendor/`, with their license texts.
