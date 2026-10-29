# Debug: subagent problems on jjaggars-kubevirt

Investigation date: 2026-10-26 (worker `subagent-debug`, ledger h821d74fb29ca).
Services were not restarted or reconfigured during phase 1; in phase 2 the user
approved one config edit to `~/.pi/agent/models.json` (backed up first).

## Part 2 (same evening): the HTTP errors — root cause found and fixed

The user reported HTTP errors from subagents ("try a subagent now with thinking
on", "it happens with glm too"). Reproduced and root-caused:

### Repro matrix (via the Agent tool, before the fix)

| spawn | result |
|---|---|
| general-purpose, session model, thinking medium | READY (different provider path) |
| scout (openai/gpt-5.6-luna) + thinking high | **400** reasoning_effort error |
| worker (openai/gpt-5.6-terra) + thinking xhigh | **400** |
| scout + thinking off | READY |
| codebase-analyzer (pins luna + `thinking: medium`) | **400** out of the box |
| worker + model=enmaas glm + thinking medium | **400 naming gpt-5.6-terra** (pinned model wins over the tool's model param) |
| general-purpose + enmaas glm + thinking minimal/low/high/xhigh | READY at every level |

Error text: `400: Function tools with reasoning_effort are not supported for
gpt-5.6-<x> in /v1/chat/completions. To use function tools, use /v1/responses
or set reasoning_effort to 'none'.`

### Root cause

1. `~/.pi/agent/models.json` redirects the `openai` provider's baseUrl to the
   internal gateway `https://api.enmaas.devshift.net/v1` (and defines an
   `enmaas` provider with `api: openai-completions` and its own gpt-5.6
   copies). The gateway rejects any request combining `reasoning_effort` with
   function tools for gpt-5.6 models. Built-in gpt-5.6 definitions carry a
   `thinkingLevelMap` (low→low … max→max), so any thinking level ≥ low sends
   `reasoning_effort` → 400.
2. `~/.pi/agent/agents/*.md` pin gpt-5.6 models for every custom subagent
   type (worker→terra, scout/coordinator→luna, planner→sol, reviewer→terra,
   codebase-analyzer→luna **with `thinking: medium` pinned**), so most
   subagent types fail as soon as thinking is on.
3. The pinned model in an agent definition overrides the Agent tool's `model`
   parameter — spawning a `worker` with model=enmaas glm still ran terra and
   400'd. This explains "it happens with glm too": glm itself is fine
   (READY at minimal/low/high/xhigh); the failures attributed to it were the
   pinned gpt-5.6 models.
4. pi sends gpt-5.6 via the Responses API (`/v1/responses`, per the built-in
   `api: openai-responses` and the SDK's `responses.create`), yet the gateway
   error names `/v1/chat/completions` — its responses endpoint appears to be
   a shim over chat/completions and drops reasoning+tools there.

### Fix applied (user-approved: "null the thinking map")

`~/.pi/agent/models.json` (backed up to `models.json.bak-pre-thinking-fix`):
- `openai` provider: added `modelOverrides` for gpt-5.6-luna/sol/terra with
  `thinkingLevelMap` `{off: "none", everything else: null}` — pi clamps any
  requested thinking level down to off and sends `reasoning_effort: none`,
  which the gateway accepts. Built-in catalog otherwise untouched.
- `enmaas` provider's own gpt-5.6-terra/sol/luna entries: same nulled map.

Effect: gpt-5.6 models get no thinking through enmaas (the gateway can't do
reasoning+tools for them); everything else unchanged.

### Verification

- Fresh pi processes (config reloaded at startup):
  `pi --model enmaas/gpt-5.6-luna:medium "…"` → READY;
  `pi --model enmaas/gpt-5.6-terra:xhigh "…"` → READY (both 400'd before).
- models.json is valid JSON; diff vs backup shows only the intended changes.
- Already-running pi sessions keep their startup catalog — models.json is
  reloaded when `/model` is opened (or on restart). Subagent spawns from my
  own session still 400 until the user opens /model there.
- `openai/gpt-5.6-*` one-shot verification was not possible from bash (the
  one-shot process has no openai credentials); the override is staged for
  those IDs and the gateway path is identical to the verified enmaas one.

### Open items from part 2

1. **User action**: open `/model` (or restart) in each running pi session —
   including this one and the herdr panes — to pick up the new models.json.
2. The other gpt-5.x/6.x entries under the `enmaas` provider (gpt-6-astra,
   gpt-6.1-sol, gpt-6-sol/luna, gpt-5.5, 5.4*, 5.3, 5.2) still map
   low→low… — likely the same 400 with thinking on (untested). Same one-line
   treatment if they fail. Note gpt-6-astra and gpt-6.1-sol have
   `off: null` (thinking cannot be disabled) — if the gateway rejects them
   they are unusable via enmaas until their maps are changed.
3. Pinned model in an agent definition overrides the Agent tool's `model`
   param — verify with pi upstream whether that is intended; it surprised the
   user ("glm too").
4. Gateway-side: enmaas not supporting reasoning_effort + function tools for
   gpt-5.6 on either endpoint is worth reporting to the gateway team; the
   error's advice ("use /v1/responses") does not work through it.
5. The earlier Comebye/herdr brief-delivery failures (part 1 below) are a
   separate, already-fixed problem; both may have fed the "subagents are
   broken on this VM" impression.

## Part 1: Comebye/herdr worker launches — already fixed (Oct 23–25)

The "subagent trouble" was Comebye→herdr worker launches in which the pi agent
pane started but the handoff brief was never delivered — herdr refused the
prompt with `agent X is not an active named agent`, so the worker sat idle with
no instructions (ledger entry h57b67ed47c4a, `gateway-name-hostname`, Oct 22).
Root cause: a race in Comebye's launch path (prompt sent immediately after
`agent.start` queued the launch, before herdr had detected the agent in its
pane), compounded after the Oct 23 reboot by agent-kind discovery problems
(pi not found on PATH). Both were fixed on Oct 23–25 (commits 09308de,
0dc69e1, 0b7580b, d2e383c) and deployed to both hosts on Oct 25 09:15.
Every launch since then has succeeded. **No action needed on the launch path.**

## Evidence

### The failing entries (ledger `~/.local/state/comebye/ledger.json`)

| entry | host | created | error |
|---|---|---|---|
| h57b67ed47c4a `gateway-name-hostname` | kubevirt | Oct 22 17:39 | brief was not sent: … not an active named agent |
| h45d452464d81 `debug-prompt-delivery` | kubevirt | Oct 23 11:30 | same (its investigation followed) |
| hb60b367aab48 `rollout-smoke` | kubevirt | Oct 23 12:55 | same ("first attempt, brief failed pre-fix") |
| he2e726b57f27 `herdr-restore-bug` | laharl | Oct 24 00:51 | could not start claude: … did not become an active named agent within 1m0s |
| h5c826b9c85f2 `spec-analysis` | laharl | Oct 25 07:55 | same |

Post-deploy (Oct 25 09:15 →now): 11 consecutive handoffs on both hosts, zero
errors, including this worker's own launch (kinds.json shows pi+claude
`last_ok` Oct 26 19:53, i.e. this session).

### Root cause 1 — launch/brief race (the Oct 22 kubevirt failure)

herdr's socket API answers `agent.start` as soon as the launch is *queued*;
the agent is only promptable seconds later, once herdr detects it in its pane
(pi reports state via the herdr integration extension
`~/.pi/agent/extensions/herdr-agent-state.ts`, which connects to
`HERDR_SOCKET_PATH`). Pre-fix code (`git show 09308de^`) called `StartAgent`
and immediately `Prompt(name, brief)` — herdr refused every brief with
`agent_name_not_active` ("is not an active named agent"), and on a
just-spawned pane could leave the launch pending forever. The worker pane ran
pi with no brief → "agent idle; uncommitted changes" in the ledger.

Fix commits on herdr-phone main:
- `09308de` Oct 23 08:46 "Launch: wait for a started agent to become promptable"
  (`startAgent` retries `agent_pane_unavailable`/`agent_pane_busy` for 10s)
- `0dc69e1` Oct 23 09:02 "Launch: wait for the agent to report ready, not
  just its name" (`waitForNamedAgent` polls `agent.get` until
  `InteractiveReady`, default `agentReadyWait` 1m)

Verified by `debug-prompt-delivery` (w2J:p1, Oct 23): smoke test
h653b866c567f "verified: brief delivered, agent replied READY" (13:03).

Note: even with the wait, `worker.go` sends the brief with a single
unretried `Prompt` after `waitForNamedAgent` returns; if the agent deactivates
in between, the brief is still lost. This explains the residual failures at
Oct 23 11:30 and 12:55 (same binary as the 13:03 success — no restart or
commit in between; flaky race, not code).

### Root cause 2 — post-reboot kind discovery (the laharl failures, and "pi undetected")

- The VM rebooted Oct 23 19:34 (`uptime -s`). comebye (systemd user service)
  crash-looped ~7× until 19:46:45 because herdr's socket
  `~/.config/herdr/herdr.sock` did not exist yet — herdr `server` only starts
  when the user connects remotely (PPID chain → remote-client-bridge, started
  Oct 23 19:46:44). journalctl: `cannot reach herdr at …herdr.sock: … no such
  file or directory`, exit 1, restart counter 7.
- Kind detection looked on PATH + a fixed bin list, so pi installed under a
  node version manager was invisible while no pi agent was running → handoffs
  of kind pi (and claude on laharl) refused with "could not start …".

Fix commits:
- `0b7580b` Oct 24 09:27 "Launch: find agents installed under node version
  managers" (searches nvm/fnm/mise/volta/pnpm/asdf glob dirs)
- `d2e383c` Oct 25 04:12 "Handoff: restart a worker whose branch was renamed;
  remember agent kinds that worked" (adds `kinds.json` memory: kind → last_ok,
  90-day recall; `~/.local/state/comebye/kinds.json` currently shows pi and
  claude both ok as of Oct 26 19:53)

Deployed: laharl Oct 25 08:53 (deploy/laharl-rollout), hub/kubevirt Oct 25
09:15 (`~/.local/bin/comebye`, built from main @ df2f509, includes all four
fixes). No failure in the ledger after that.

### The OpenShell worktree the coordinator asked about

- The failed Oct 22 worker `gateway-name-hostname` left **no commits**: branch
  `feat/gateway-name-in-service-hostname` in /home/jjaggars/code/OpenShell is
  fully contained in main (`merge-base --is-ancestor` → yes).
- The worktree `/home/jjaggars/.worktrees/OpenShell/feat-gateway-name-in-service-hostname`
  is now on branch `feat/4840-gateway-name-service-hostname/jhjaggars` and its
  6 dirty files (crates/openshell-*) belong to the live w26:p2 session — they
  are that session's current work, not debris from the failed worker. Nothing
  to clean up from the failure itself.

## Ruled out

- Folder-trust prompt blocking first launch in a new worktree: not observed —
  smoke-test agents launched into brand-new worktrees replied READY; the
  failures were prompt-refusal by herdr, not a pi dialog.
- Resource limits / OOM / network / Tailscale: no evidence; comebye peak 430M
  during the day of heavy use, no OOM lines, peer links fine.
- cargo/rust env slowing pi startup: pi startup was never the bottleneck; the
  wait window is 1m and post-fix launches activate in seconds.

## Open items / risks (need a human decision or follow-up)

1. **Brief still has no retry** (`worker.go`: single `Prompt` after the wait).
   A rare deactivation between wait and prompt can still strand a worker.
   Candidate fix: retry the brief prompt through the same
   wait-for-ready helper before recording `PromptError`.
2. **Boot ordering**: after a reboot, comebye crash-loops until herdr's socket
   exists, and herdr only starts when the user connects. Consider a systemd
   user unit / socket dependency for herdr, or a comebye retry-with-backoff.
3. **Local main is ahead of origin/main by 4 commits** (df2f509…3929e27, the
   multiselect-dialog work, merged Oct 25) — unpushed; only copy is local.
4. Ledger hygiene: h45d452464d81 (`debug-prompt-delivery`) is still status
   "running" with a stale error though its worktree is clean and idle and its
   fixes are merged — it never filed a final report; close it with
   `comebye mark`. h57b67ed47c4a's `final_report` field holds an unrelated
   RBAC brief (pane reuse) — cosmetic.
5. The user could not be interviewed (declined the questionnaire); this report
   assumes the reported "subagent trouble" is the brief-delivery failure
   class. If the user meant something else (e.g. pi's in-session Agent tool
   failing), that would be a separate investigation.

## Key commands used (repro)

- `comebye ledger [--json]` — entries, errors, timestamps, marks
- `journalctl --user -u comebye --since … --no-pager` — restarts, crash loop,
  herdr-socket failure
- `git -C ~/code/herdr-phone log/show 09308de 0dc69e1 0b7580b d2e383c` — fix
  history and pre/post code
- `herdr integration status` — pi integration current (v9)
- `cat ~/.local/state/comebye/kinds.json` — kinds memory healthy
- `uptime -s`, `last reboot`, `ps -o ppid,lstart -p <herdr server pid>`
