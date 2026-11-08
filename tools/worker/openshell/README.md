# comebye worker sandbox image (OpenShell, Kubernetes driver)

A comebye worker host as an OpenShell sandbox. The gateway manages it (create, exec,
stop, start, delete); **comebye traffic rides the tailnet** (stock `tailscaled` in
userspace mode, DERP through an in-sandbox CONNECT proxy, `tailscale serve` inbound,
tailscaled's SOCKS5 outbound to the hub). Kubernetes driver only: a podman sandbox has
only `lo` and cannot reach a DERP relay.

| File | What |
| --- | --- |
| `Dockerfile` | the image; `COMEBYE_VERSION` build arg = the image tag |
| `comebye-sandbox-init` | the sandbox's main process (no systemd): supervisor + idle exit |
| `policy.yaml.tmpl`, `render-policy.sh` | least-privilege policy template and renderer |
| `profiles/github-push.yaml` | GitHub provider profile (https clone/fetch/push, token held by the gateway) |
| `build.sh` | build, secret-scan, push `registry.jaggars.cc/comebye-sandbox:vX.Y.Z` |
| `RUNBOOK.md` | one-time gateway setup: providers, login, differences from podman |

## Image

`registry.jaggars.cc/comebye-sandbox:v0.5.12` — the tag **is** the comebye release it
contains; the hub and its workers must run the same one. Contents (all under
`/usr/local`, because the image root is read-only to the workload and a PVC masks
`/sandbox`): `comebye`, `tailscale`/`tailscaled` 1.92.5, `herdr`, node 22.20 + `pi`
1.5.0, `claude`, plus `tinyproxy git jq ssh socat`. User `sandbox` (uid 1000), `HOME=/sandbox`.

Create it with the init as the command:

```sh
openshell sandbox create --name NAME --from registry.jaggars.cc/comebye-sandbox:v0.5.12 \
  --policy policy.yaml --provider anthropic-claude --provider github-push \
  --label managed-by=comebye-worker -- comebye-sandbox-init
```

(`--policy` comes from `render-policy.sh`; provider names are whatever the runbook created.)

## Contract: who starts what

| Step | Who | How |
| --- | --- | --- |
| tinyproxy :3128, tailscaled (userspace, SOCKS5 127.0.0.1:1055, `--statedir=/sandbox/.ts`), herdr server, herdr agent integrations (pi, claude) | `comebye-sandbox-init` | on every (re)start; each restarted 3 s after exit |
| write the env file | **provisioner (W1)** | `openshell sandbox exec --no-tty -- sh -c 'umask 077; cat >/sandbox/.config/comebye/env'` with the file on stdin |
| `tailscale up --authkey=… --hostname=… --advertise-tags=…` | **provisioner (W1)** | `sandbox exec --no-tty` with the key on stdin; socket `/sandbox/.ts/tailscaled.sock` (`tailscale --socket=…`). Needed only the first time: the node identity persists in `/sandbox/.ts` across stop/start |
| `tailscale serve --bg 8741` + certificate warm-up | `comebye-sandbox-init` | background task: waits for the env file **and** `BackendState=Running`, then serves and requests the cert (first issuance ≈ 36 s). Disable with `SBX_SERVE=0` |
| comebye server | `comebye-sandbox-init` | starts when the env file exists: `comebye -addr 127.0.0.1:8741 -ts-socket /sandbox/.ts/tailscaled.sock`, env file sourced, restarted on exit |
| idle exit | `comebye-sandbox-init` | `comebye idle` says idle → kill the whole process tree, exit 0 → sandbox `Completed`; `sandbox start` brings it back and re-runs everything |

The init never reads or stores a tailnet key. Order between the provisioner steps does
not matter: comebye starts when the env file appears; Serve starts when the node is joined.
comebye with `COMEBYE_ANNOUNCE=1` retries/fails until the node is joined, and init restarts
it, so write the env file and run `tailscale up` in either order.

### Env file (`/sandbox/.config/comebye/env`)

Shell syntax, sourced with `set -a` by a **bash** (`KEY=value`, no spaces around `=`, quote
values with spaces). Written mode 0600. Re-read on each restart of comebye.

comebye's own keys (same as the VM worker's bootstrap-user.sh, except the paths):

| Key | Value for a sandbox |
| --- | --- |
| `COMEBYE_HOST_NAME` | worker name (lowercase DNS label) |
| `COMEBYE_TS_ALLOW_LOGIN` | **required** — comebye refuses to start without a login allowlist |
| `COMEBYE_PUSH` | `false` |
| `COMEBYE_HUB` | hub Serve URL |
| `COMEBYE_ANNOUNCE` | `1` |
| `COMEBYE_LAUNCH_ROOTS` | `/sandbox/code` |
| `COMEBYE_WORKTREE_ROOT` | `/sandbox/worktrees` |

Init's keys (all optional):

| Key | Default | Meaning |
| --- | --- | --- |
| `SBX_IDLE_MINUTES` | `120` | passed to `comebye idle --minutes`; polled every 5 min |
| `SBX_IDLE_CMD` | unset | overrides the check (shell command, exit 0 = idle) |
| `SBX_TICK_SECONDS` | `300` | poll interval (tests) |
| `SBX_SERVE` | `1` | `0` skips `tailscale serve` + warm-up |
| `SBX_HUB_PROXY` | `socks5h://127.0.0.1:1055` | exported as `ALL_PROXY` (+ `NO_PROXY=127.0.0.1,localhost`) **to the comebye server only**, so its hub calls go through tailscaled. Empty string disables |

The panes get no proxy variables: the `comebye` CLI in a pane talks to the local server only (it
ignores proxy variables), and git/claude/pi must keep going through the sandbox policy (a
proxy in their environment would send them to tailscaled's SOCKS5, which the policy does not
let reach the internet).

Idle and upgrade (comebye >= the sandbox-support change): idle is `comebye idle --minutes N`
(0 = idle, 1 = not idle, anything else counts as not idle; the clock file is removed at init
start so stopped time never counts). `comebye upgrade --restart exit` installs to
`~/.local/bin` and SIGTERMs the server, which exits 0; init restarts `comebye` on **any** exit,
3 s later, resolved by name from a PATH with `~/.local/bin` first.

### What survives stop/start

Only `/sandbox` (the PVC). Logs go to `/tmp` (lost on restart). tailscaled state, herdr
config, agent logins, repos, sessions and `~/.local/bin` (an upgraded comebye wins over the
image's, `PATH` puts it first) are under `/sandbox`.

## Policy

`render-policy.sh --llm-host llm.example.com > policy.yaml`. Rules and why:

| Rule | Hosts | Binaries | Reason |
| --- | --- | --- | --- |
| `tailscale` | `controlplane`, `login`, `log`, `derp*.tailscale.com`, `acme-v02.api.letsencrypt.org` :443 | tinyproxy, tailscaled | control plane, DERP (all tailnet traffic; UDP is fenced), logging, and Let's Encrypt for `tailscale serve` certs (tailscaled does ACME itself). Only these two binaries may use it, so the in-sandbox proxy is worth nothing to anything else. The `tailscale` CLI is *not* listed: it only talks to the local socket |
| `anthropic` | `api.anthropic.com`, `claude.ai`, `console.anthropic.com`, `platform.claude.com` | claude | API (key injected by the provider) and subscription login. Telemetry hosts stay denied |
| `llm_gateway` (optional) | the LiteLLM host | node, pi | pi's model gateway |
| GitHub (profile, not this file) | `github.com`, `api.github.com` | git, gh, `git-remote-https` | clone/fetch/push over https; token held by the gateway |
| filesystem | rw: `/sandbox /tmp /var/tmp /dev/{null,pts,ptmx,shm,tty}`; ro: `/usr /lib /lib64 /bin /etc /proc /sys /var/log /dev/urandom` | | `/dev/pts,ptmx,shm,tty` are required for herdr PTYs |

Everything not listed is denied, including SSH to github.com:22 and IP-literal TCP.
