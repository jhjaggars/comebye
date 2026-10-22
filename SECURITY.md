# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for security problems. Report them
privately through GitHub's
[private vulnerability reporting](https://github.com/jhjaggars/herdr-phone/security/advisories/new)
for this repository. Include what you found, how to reproduce it, and the
impact you expect. You should get a response within a week.

This is a small personal project with no bug bounty. Only the latest
commit on the default branch is supported.

## Threat model

herdr-phone lets a browser send prompts and keystrokes to coding agents
running on your machine. **Anyone who can use the API can run arbitrary
commands as your user**, so it is designed to be reachable only by you.

What it relies on:

- **Tailscale Serve** is the only path in. The server listens on loopback;
  Serve terminates HTTPS and accepts connections only from your tailnet.
- **Tailscale identity headers.** Serve sets `Tailscale-User-Login` on
  proxied requests and strips client-supplied copies. With
  `-ts-allow-login`, only listed logins are admitted. The server refuses to
  start without an allowlist (unless `-insecure-no-auth`) and with an
  allowlist on a non-loopback address.
- **Host checking.** Requests whose `Host` is not `localhost`, a loopback
  IP, a `*.ts.net` name or one given with `-allow-host` are refused (421),
  so a web page cannot reach the loopback port by DNS rebinding.
- **The local host is trusted.** Any local process can reach the loopback
  port and forge the identity header. Web pages in a browser on the host
  are not local processes: bypassing the Host check (DNS rebinding) is in
  scope.

In scope:

- Bypassing the login allowlist or the loopback restriction.
- Getting the server to act for a request that did not come from an
  allowlisted user through Serve (for example cross-site requests from a
  web page the user visits).
- Escaping the key whitelist, or reaching herdr socket methods the API does
  not intend to expose.
- Script injection in the web UI from agent output or transcripts.
- Leaking agent output, transcripts or settings to third parties.
  Web Push payloads are end-to-end encrypted (RFC 8291); the push service
  (Apple, Google, Mozilla) seeing anything beyond ciphertext, size and
  timing is in scope.
- Making the server send requests to hosts other than the known push
  services through a push subscription endpoint (SSRF).

Out of scope:

- Running with `-insecure-no-auth`, listing a name with `-allow-host`
  whose DNS an attacker controls, exposing the port with Tailscale Funnel,
  binding to a non-loopback address, or putting another proxy in front of
  it. These are unsupported configurations.
- Attacks by local users or processes on the same host.
- A compromised tailnet account or device that is on the allowlist.
- Content of ntfy notifications on a public ntfy server (agent names,
  repository/branch and status, plus last-message excerpts if you enable
  `-ntfy-excerpt`, which is off by default); use a long random topic or
  self-host ntfy.
- What the agents themselves do with the prompts you send them.
