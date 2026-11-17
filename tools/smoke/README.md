# Day in the life: the end-to-end smoke

One script that does what the user and the agents actually do, on a throwaway hub, so a hardening change
that makes normal use hard fails here first. Every hardening slice must keep it green. It runs in CI
(`day-in-the-life` in `.github/workflows/ci.yml`, pre-submit with the browser smoke set and on every push to main,
and again in the nightly) and takes about 15 seconds after the build.

```sh
tools/smoke/run.sh [OUT_DIR]      # needs go, node, npm, git, gpg and google-chrome (CHROME=/path to override)
SMOKE_HOLD=1 tools/smoke/run.sh   # leave the world up and print how to talk to it (for debugging a step by hand)
```

Screenshots (`N-name-390.png`, phone width), the hub's log and the stand-in herdr's land in `OUT_DIR`. A failed check
also writes `state-FAIL-N.txt`: the agents, the ledger, the to-dos and every prompt typed into each stand-in agent.

## What it does

| Step | What happens | Real or stand-in |
|---|---|---|
| setup | a coordinator runs; the user makes it the coordinator | stand-in agent, real hub role |
| 1 | the coordinator hands off a worker (own branch, real `git worktree`); the worker commits and reports; the hub types the finished notice into the coordinator's pane | stand-in agents, real hub, CLI and git |
| 2 | a second worker files a decision to-do with buttons; the user taps a choice **on the phone** (390px headless Chrome, the real Content-Security-Policy); the hub types the answer, labelled as the user's, into the worker and closes the item | same |
| 3 | the coordinator files "Land?", the user answers in the app, the coordinator runs `comebye land`; the worker is told, labelled as an agent's, and pushes to a **bare throwaway origin**; the hub sees it and the board and the app show it landed | same |
| 4 | the worker publishes a Page; the user opens it in the viewer; "Chat about this" starts a read-only `claude` helper and opens its conversation | the helper is a stand-in agent |
| 5 | park the idle second worker (pane closes, worktree, branch and session stay), resume it (new pane, same session, catch-up note), restart it (same pane, same flags) | stand-in agents |
| 6 | GPG unlock with Face ID: Chrome's virtual authenticator (PRF), real gpg in a throwaway `GNUPGHOME`, the real set-up item and card; the key then signs with no prompt | real gpg and CLI; reuses `tools/uitest/gpghelp.mjs` |
| 7 | as the coordinator: `comebye board`, `comebye log` and `comebye coordinator digest` show what happened | real CLI |

## Real and stand-in, and why

- **Real:** the hub (the built `comebye`), the `comebye` CLI run as each agent (its pane id and the local agent token, so
  the agent role and the coordinator-only rules apply), git worktrees and the bare origin, gpg, headless Chrome.
- **Stand-in herdr:** `tools/smoke/fakeherdr`, a small stateful stand-in for herdr's socket API (workspaces, panes, agents,
  real `git worktree add`, the agent's `/exit`, session files). herdr is not installed on CI runners, so CI cannot use
  the real one; the script never touches a real herdr and never reads its config: the socket is a path under the run's
  temp directory. **No step uses a real herdr, claude or pi, locally or in CI.** `claude` and `pi` on the hub's `PATH`
  are stubs that only say "installed". (`tools/devserver` is the other fake: fixed agents for the browser checks.)
- **Stand-in agents:** `day.mjs` plays every agent. It reads what the hub typed into each agent's pane (the brief, the
  notices, the relay, the land prompt) and does what a well-behaved agent does in answer. That keeps it deterministic and
  free; it does not test a model's judgment.

## Rules

- **No retries.** A wait polls a condition (`until`); nothing is run again to get a different answer. There are no known
  flakes here. If one turns up, document it where it is handled and in this file.
- **Fast:** the whole run, build included, is under a minute on a laptop; the budget is 5 minutes.
- **A usability regression is reported, not worked around.** If a hardening change blocks a step, the step fails; do not
  loosen the step. Say which step and what blocked it.
- Everything lives under one temp directory (`HOME`, the hub's state, `GNUPGHOME`, the herdr socket, the repositories) and is
  removed at the end. CI sets `SMOKE_REQUIRE_GPG=1` so a runner without gpg fails step 6 instead of skipping it.

## Not covered yet

- **No theme flash in the Page viewer** (commit b6d05c6, "paint in the app's theme from the first frame") is not on
  main yet. Once it lands, add a check to step 4 that the frame's first paint is in the app's theme (the viewer today
  shows a white page in a dark app until the page's own theme listener runs: see `4-page-390.png`).
- Real herdr, claude and pi. A local-only mode that runs a throwaway `herdr --session <name>` (never the real config)
  would exercise herdr's own agent detection; it is not built.
