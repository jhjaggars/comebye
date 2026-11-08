# Sandbox hosts: what comebye provides

A worker can run in an OpenShell sandbox (Kubernetes driver) instead of a VM.
There is no systemd and no root there; a small init loop keeps `tailscaled`,
`herdr` and `comebye` running. Three comebye features exist for that host
kind. The image, init and policy live in `tools/worker/openshell/`; the
provisioning is in [workers.md](workers.md).

## Restarting after an upgrade

`comebye upgrade --restart exit` (the default without a systemd unit)
installs the new binary and then SIGTERMs the running server, which exits 0.
**The contract with the init loop:** restart `comebye` whenever it exits, by
name, from a PATH with `~/.local/bin` first (so the upgraded copy wins over
the image's `/usr/local/bin/comebye`). A few seconds' delay is fine; the
upgrade waits up to 30 s. Nothing else is needed: no pidfile, no flag file.
See [setup.md](setup.md#without-systemd-sandbox-hosts) for the whole flow
and the rollback.

## One idle check

```sh
comebye idle [--minutes N] [--json] [--dry-run]
```

Exit 0: this host has been idle for N minutes (default 120, or
`COMEBYE_IDLE_MINUTES`): no herdr agent is `working` and no interactive ssh
session holds a pty. Exit 1: active, or the clock has not run out yet. Exit 2:
a usage error. The line it prints says which (`--json` gives `idle`,
`minutes`, `idle_minutes`, `reasons`, `agents_working`, `ssh_sessions`,
`herdr`).

It is polled, like the VM timer: each poll that finds activity renews a stamp
(`<state dir>/idle-stamp`; `--dry-run` never writes it), and the host is idle
once the stamp is N minutes old. The stamp is tied to the boot or container
run, so a stopped and started sandbox starts a new clock instead of counting
the time it was off.

What counts as activity, and what does not, is the VM script's rule:

| Activity | Not activity |
|---|---|
| a herdr agent with status `working` | `idle`, `done`, `unknown`, `blocked` agents |
| an `sshd: user@pts/N` process (a person at a shell) | a bare ssh connection (herdr keeps one open) |
| herdr running but not answering (do not stop on a guess) | herdr not running (no socket, or a stale one) |

A bare `openshell sandbox exec` session cannot be seen and does not count.

**Init loop:** poll every few minutes and exit 0 when it succeeds.

```sh
while sleep 300; do
  comebye idle --minutes "${SBX_IDLE_MINUTES:-120}" >/dev/null && { echo idle; exit 0; }
done
```

**VM script:** `tools/worker/scripts/idle-check.sh` can switch over by replacing
its activity probes and stamp logic with one call, run as the worker user:

```sh
if runuser -u "$WK_USER" -- env HOME="$home" "$home/.local/bin/comebye" idle \
     --minutes "$WK_IDLE_MINUTES" --stamp "$stamp"; then
  systemctl poweroff
fi
```

(`--stamp` keeps the clock in `/run`, where a reboot clears it, as today.)
Until a VM's comebye is upgraded to a version with `idle`, the old script
keeps working unchanged; the switch needs the comebye version on the VM to be
at least the one that adds the command, so do it with the next worker image
or after `comebye upgrade`.

## Reaching the hub from a sandbox

A sandbox has no DNS for tailnet names; its route to the hub is tailscaled's
SOCKS5 listener (`--socks5-server=127.0.0.1:1055`). A peer's calls to the hub
(`announce`, `report`, `prompt`, `ledger`, handoffs, marks) honour the usual
proxy variables, set for the `comebye` process:

```sh
ALL_PROXY=socks5h://127.0.0.1:1055
```

`HTTPS_PROXY` (same URL forms) takes precedence for the https hub URL;
`NO_PROXY` excludes hosts (comma list, suffixes with or without a leading dot,
IPs and CIDRs, `host:port`, `*`). The proxy may be `http://`, `https://`,
`socks5://` or `socks5h://`; the hub's name always reaches the proxy as a name,
so tailscaled resolves it with MagicDNS. Loopback is never proxied. A
malformed proxy setting makes the call fail (it never silently goes direct).

Only the peer-to-hub link uses the proxy variables. The hub's own calls to
its peers do not (tailnet names are reached directly), and neither does the
CLI's call to the local server. The archive upload to S3 runs on the hub, not
on a worker, so it is not affected.
