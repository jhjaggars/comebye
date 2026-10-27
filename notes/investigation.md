# Debug: subagent problems on jjaggars-kubevirt

Investigation date: 2026-10-26 (worker `subagent-debug`, ledger h821d74fb29ca).
All evidence read-only; nothing was restarted, killed, or reconfigured.

## TL;DR

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
