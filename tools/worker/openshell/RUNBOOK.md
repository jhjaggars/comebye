# Runbook: gateway, providers, login, and what was verified

Placeholders only. Never paste a real key into this file, a shell history line, or a
`--credential KEY=VALUE` argument you will keep (see "Giving a provider its secret").

## 0. Gateway access

Homelab gateway: endpoint `https://openshell.tail4a1fd.ts.net`, OIDC issuer
`https://openshell-auth.tail4a1fd.ts.net`, client `openshell-cli`, workspace
`openshell-sandboxes` (`default` is refused). The CA is public, in Secret
`openshell/openshell-client-tls` (`ca.crt`).

```sh
# once per machine, on a user-owned tailnet node (laharl):
export OPENSHELL_WORKSPACE=openshell-sandboxes
bash ~/code/homelab/apps/tsidp-bridge/scripts/headless-login.sh homelab   # sessions last <= 12 h
openshell provider list
```

To test without touching your normal config, point `XDG_CONFIG_HOME` at a scratch dir that
holds a copy of `gateways/homelab/metadata.json` and `gateways/homelab/mtls/ca.crt`, then run
the login script with the same variable. (This is how the W2 tests ran; the 0.1.7 CLI works
against the 0.1.8-pre.3 gateway for everything below.)

## 1. Providers (once per gateway/workspace)

A provider holds the real credential in the gateway; a sandbox sees only
`openshell:resolve:env:…` placeholders and the supervisor substitutes the value on egress,
only toward the provider profile's endpoints and binaries.

Profiles first (they define the endpoints/binaries; these are workspace-scoped, no admin
needed on the homelab), from this directory:

```sh
for p in github-push anthropic litellm; do
  openshell provider profile lint   -f profiles/$p.yaml
  openshell provider profile import -f profiles/$p.yaml
done
```

Edit `profiles/litellm.yaml` first: set `host:` to your LiteLLM host (the same one you pass
to `render-policy.sh --llm-host`). Credentialed endpoints **must be L7** (`protocol: rest`);
the gateway refuses an L4-only endpoint that carries a credential (`uses L4-only; configure L7
inspection …`), which is also why the policy template marks `api.anthropic.com` and the LLM
host as `protocol: rest`.

Then one provider per credential:

| Provider (suggested name) | `--type` | Credential env var | Used by |
| --- | --- | --- | --- |
| `anthropic-claude` | `anthropic` | `ANTHROPIC_API_KEY` | `claude` in API-key mode (skip if you use subscription login) |
| `litellm-pi` | `litellm` | `LITELLM_API_KEY` | `pi` |
| `github-push` | `github-push` | `GITHUB_TOKEN` | `git` clone/fetch/**push** over https |

### Giving a provider its secret

`--credential NAME` (no `=VALUE`) reads the value from your **environment**, so the secret
never appears on a command line:

```sh
# the variable is read from your environment; set it without echo or history, e.g.
read -rs ANTHROPIC_API_KEY && export ANTHROPIC_API_KEY
openshell provider create --name anthropic-claude --type anthropic --credential ANTHROPIC_API_KEY
unset ANTHROPIC_API_KEY
read -rs LITELLM_API_KEY && export LITELLM_API_KEY
openshell provider create --name litellm-pi --type litellm --credential LITELLM_API_KEY
unset LITELLM_API_KEY
read -rs GITHUB_TOKEN && export GITHUB_TOKEN
openshell provider create --name github-push --type github-push --credential GITHUB_TOKEN
unset GITHUB_TOKEN
openshell provider list     # shows credential KEYS only, never values
```

GitHub token scope: a fine-grained PAT limited to the repositories workers may push to
(Contents: read/write; Metadata: read). It is shared by every sandbox that attaches the
provider, so scope it for that.

Attach with `--provider NAME` (repeatable) at `sandbox create` (README.md).

### Verified vs not (providers)

- Live (fake values): `ANTHROPIC_API_KEY`/`GITHUB_TOKEN` appear in the sandbox as placeholders;
  `claude -p` reaches `api.anthropic.com` and gets `Invalid API key` (the placeholder was
  replaced and the request left); `git clone` of a public repo works through the `github-push`
  profile rules.
- **Not verified**: a real push (`git-receive-pack` POST) and whether GitHub's git smart-HTTP
  accepts the injected `Authorization: bearer` header for a real token. Test it once with a
  scratch repo before relying on it. The LiteLLM provider was only lint-checked.

## 2. `claude` login (subscription accounts)

Subscription login cannot use a provider (it is an OAuth browser flow that writes a
credential file). Run it once per sandbox as an interactive exec, with a pty:

```sh
openshell sandbox exec --tty -n NAME -- claude auth login --claudeai
```

It prints a `https://claude.com/cai/oauth/authorize?…` URL and waits at
`Paste code here if prompted >`. Open the URL in your own browser, sign in, copy the code the
callback page shows and paste it at that prompt. The credential lands in
`/sandbox/.claude/` (the PVC), so it survives stop/start. The policy's `anthropic` rule allows
the sandbox side of the flow (`platform.claude.com`, `console.anthropic.com`, `claude.ai`,
`api.anthropic.com`).

Verified live: the command starts under `exec --tty` and prints the URL and the prompt.
**Not verified**: completing the login (needs a real account) and the post-login API call.
`tools/worker` (`comebye-worker login NAME claude`) is just this command.

## 3. Kubernetes vs podman: differences recorded

| Area | Kubernetes driver (homelab) | podman driver (laharl) |
| --- | --- | --- |
| Tailnet | pod has `eth0` + default route: stock tailscaled works, no test knob | `lo` only: tailscaled registers but never dials DERP; **unsupported** (init sets the test knob only here) |
| Workload uid | **10001**, no passwd entry: `whoami`/`ssh` say "No user exists for uid 10001", prompt shows `I have no name!`. Irrelevant for https git, claude, pi, herdr; ssh clients break | uid 1000 (`sandbox`) |
| `/sandbox` | PVC (Longhorn, ~2 GiB, fixed at create) — the only durable path | container volume |
| `/tmp` | **lost** on stop/start (logs live there) | survived in the spike |
| Image refs | node may cache a moving tag; use `@sha256:…` | local image store |
| Stop/start | `Completed` ↔ `Ready` verified; herdr workspaces, env file, marker files, repos survive; init restarts everything | same |
| Idle exit | exit 0 → `Completed` **only if every process is gone**: init reaps its tree (the supervisor, pid 1, waits for stragglers) | not tested |
| Credentialed endpoints | must be L7 (`protocol: rest`) | same gateway rule |

## 4. What was run (W2, homelab gateway, fake credentials)

Image `registry.jaggars.cc/comebye-sandbox:v0.5.12`, full policy from `render-policy.sh`:

| Check | Result |
| --- | --- |
| herdr PTY | workspace + pane created; `tty` → `/dev/pts/0`; `herdr pane run`/`read` work |
| herdr integrations | `pi` and `claude`: current |
| claude, pi | `claude --version` 2.1.319, `pi --version` 1.5.0 in a pane; `claude -p` reached Anthropic; `pi -p` got "Connection error" (its LLM host was `llm.example.com`, expected) |
| Policy denials | `example.com` and `api.github.com` via `curl` denied; `/dev/tcp/github.com/22` denied; tinyproxy → `derp1.tailscale.com` 200, → `example.com` refused |
| comebye | starts when the env file appears, `/api/agents` → 401 without a Serve identity |
| clone | `git clone --depth 1` of a public repo through the provider: works |
| stop/start | `/sandbox` marker, repos, env file, herdr workspace survived; `/tmp` marker lost |
| idle | `SBX_IDLE_MINUTES=1`: init logged `idle 1 min: exiting 0`, phase → `Completed`; `sandbox start` → `Ready`, all four services back |
| Not done | tailnet join (no key minted: W1's), `tailscale serve` + cert warm-up, announce through SOCKS5, real push, real claude login, `SBX_IDLE_CMD` with W3's command |

## 5. Proposal: building the image per comebye release (not wired in)

The image tag is the comebye version, and the Dockerfile downloads that release's tarball and
checks it against the release's `SHA256SUMS`, so the image can only be built **after** the
release assets exist. Suggested shape, in `.github/workflows/release.yml`, a final job
`sandbox-image` that `needs: [release]` (the job that publishes the assets):

1. Build `tools/worker/openshell` with `COMEBYE_VERSION=${GITHUB_REF_NAME}`, `docker buildx`
   (linux/amd64 first; arm64 works since the Dockerfile selects by `uname -m`).
2. Run the same secret scan as `build.sh` (reuse the script: `build.sh` is the single source).
3. Push `registry.jaggars.cc/comebye-sandbox:${GITHUB_REF_NAME}`; record the digest in the
   release notes. Do not push `latest`: the hub and workers must match exactly.

Where to push — decision for the maintainer:

- **registry.jaggars.cc** (what the cluster pulls from today) needs a CI credential for a
  registry that is not on the public internet; a GitHub-hosted runner may not reach it. A
  self-hosted runner on the tailnet solves that and keeps the image off a third party.
- **ghcr.io/jhjaggars/comebye-sandbox** is reachable from hosted runners with `GITHUB_TOKEN`,
  but the cluster would then pull from ghcr (add an image pull rule or mirror it with
  `skopeo copy` into `registry.jaggars.cc` as the last step).
- Interim (works today): run `tools/worker/openshell/build.sh vX.Y.Z --push` on laharl as
  part of the release checklist, right after the GitHub release is published.

Also to decide: the Tailscale/herdr/node/pi/claude versions are pinned by build args or the
installers' "latest" (herdr, claude install scripts are not pinned). For reproducible
images, pin those too and bump them deliberately.
