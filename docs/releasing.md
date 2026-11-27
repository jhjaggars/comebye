# Releasing

A release is a `vMAJOR.MINOR.PATCH` tag on a commit of `main`. Pushing the tag
starts the Release workflow (`.github/workflows/release.yml`), which vets the
commit again, builds the binaries and publishes the GitHub release.

Agents cannot push tags, so a person runs:

```
tools/release/tag v0.5.9            # tip of origin/main
tools/release/tag v0.5.9 <commit>   # a specific commit on origin/main
```

The script fetches `origin` and refuses unless the version is well formed and
newer than the latest tag, the tag exists nowhere, the commit is on
`origin/main`, and the CI run for that commit succeeded (it says what is
pending or failed). It then shows the commit, the previous tag, the commits
since, and the CI run, and asks `Tag and push? [y/N]`. The tag is signed when
`tag.gpgSign` or `user.signingkey` is set, annotated otherwise, and only that
tag is pushed. It then prints the Release workflow run URL.

`--dry-run` does every check and prints what it would do; `--yes` skips the
question. It needs `git` and an authenticated `gh`, and runs from any checkout
or worktree of the repository.

## From the app

A coordinator that thinks a commit is ready raises a release item instead of
asking you to run the script:

```
comebye todo add --kind release --version v0.5.9 --commit <ref>   # the ref is resolved to the full id
```

The item in the To do list is a one-line ping with **Open in Peers**. Releases
live on the **Releases** tab of the Peers screen (`#/peers/~releases`, or
"Releases" in the command palette), next to **Hosts**; the tab shows an orange
badge while a release needs you (approve it, a build or upgrade failed, a host
is behind and can be upgraded). It holds the release card, the fleet table
(the hub and each host: running version, the release version, a progress bar,
and **Bump hub** / **Upgrade** buttons that stay until the version matches; a
failed deploy or upgrade shows its error and **Retry**) and the last five
releases (version, date, link to the notes). A tagged release stays in the tab
until every host runs it, even if its item was closed or dropped. Closing the
item is not your job: the hub closes it itself once the hub and every host run
the version (it checks every 30 s and whenever the tab loads). While the
automatic rollout is on, a host that is stopped or offline does not hold it:
once the hub and every reachable host run the version and no rollout is live,
the item closes with a note naming the hosts behind ("done; behind: wk-test
(stopped), laptop (offline)"), and the catch-up below upgrades them when they are
back. With the automatic rollout off, an **offline host blocks that**: the tab
says "3 of 4, 1 offline", the release is not done and the item stays open until
the host is back and upgraded. Closing the item by hand
while a host is behind or offline is refused (`release_pending`); drop it only to
abandon the release, and the tab still shows a tagged one until it is rolled out.
The same orange dot shows on the Peers entry of the navigation, the phone's fleet menu and the palette's "Releases" entry whenever a release needs you (the app looks every minute, not only inside Peers).

**Dismiss release** on the card (with a confirm) abandons a tagged rollout you will not finish: it leaves the tab, the badge and the automatic close, its item is dropped, nothing is changed on any host, the tag stays and it keeps its place in the history. It is the user's alone (CSRF token, audit line `release.dismiss`).
History comes from the to-do items, which are kept 14 days after they close.

The tab shows what matters, taken from the hub's own
checkout and GitHub rather than from the item's text: the full commit id, the
last tag, the CI result of that commit, and the commits since the last tag.
**Approve release** asks you to confirm, naming the version and the commit.
The server then runs this same script (`tools/release/tag VERSION COMMIT
--yes`; its text is embedded in the binary) in the checkout named by
`-release-repo` (a **dedicated clone** of the project that no agent works in), as
the user the server runs as, with that user's git and `gh` credentials. The script
works only with `github.com/jhjaggars/comebye` whatever the clone's configuration
says (`COMEBYE_RELEASE_SLUG` names another repository). It refuses, with the script's reason, unless the commit is on
`origin/main`, its CI run passed, the version is newer than the last tag and
the tag does not exist; it checks again at the moment of the click. The item
then shows the tag, the Release workflow run and its state.

When the release build has passed, each host in the item gets **Upgrade**
(a host needs `-release-upgrade`, and the hub `-release-repo` to see the build;
a peer is asked by the hub, and must run with `-hub` (or `-ledger=false`), since the
hub-only route it answers is refused on a host that keeps a ledger). It runs
`comebye upgrade --version vX.Y.Z` on that host, with the same checksum and
smoke checks, and keeps `comebye.prev`; the result and the host's version show
in the fleet table, and **Roll back** runs `comebye upgrade --rollback`. The upgrade
runs detached, because it restarts the service that started it: on Linux as a
transient `comebye-upgrade-*` systemd user unit (needs `systemd-run`); on macOS as
a one-shot LaunchAgent `com.jhjaggars.comebye-upgrade.<id>` in the user's GUI
domain (needs `launchctl`), started with `launchctl bootstrap` from a plist in
`<state-dir>/release/`, which removes itself and its plist when the run ends (its
output stays in the `.log` beside it). Its verdict is kept in
`<state-dir>/release/upgrade.json`. On a Mac, set `COMEBYE_RELEASE_UPGRADE` in the
launchd plist's `EnvironmentVariables` and reload the agent with
`launchctl bootout` then `bootstrap`: `kickstart -k` restarts the process but keeps
the environment launchd loaded earlier.

Every approval, refusal, tag, upgrade request and result is written to the
ledger's audit log (who, what, when, how it went) and to the journal as
`audit: ...` lines. `-release-dry-run` runs all the checks and changes
nothing. The script's `--check` mode (key=value lines, no changes) is what the
page reads; run it by hand to see what the page sees. Who can do this, and why
an agent cannot, is in [Security](security.md#releasing-and-upgrading-from-the-app).

## From the app, and on which host

The To do page's **Approve release** button runs this same script, as the user
of the server that owns the repository, with that user's git and `gh`
credentials. Where it runs is a choice of two settings:

- **On the hub itself:** `-release-repo <dedicated clone>` (`COMEBYE_RELEASE_REPO`)
  on the hub. The hub holds the credentials.
- **On a peer** (the setup to prefer when the hub should hold no release
  credentials): the peer that owns the clone sets `-release-repo <dedicated clone>`
  (and `-hub`, as every peer does), and the hub sets `-release-host <peer name>`
  (`COMEBYE_RELEASE_HOST`). The button stays on the hub's page; the hub does not
  run git or `gh` at all but asks that peer, over its fleet connection, to
  check the commit, to tag and push, and to report the tag's Release workflow
  run. The peer runs the embedded script in its own clone with its own
  credentials, after the same version and commit validation, and writes its own
  audit lines to its journal. The hub keeps the page, the CSRF check, the
  confirm step, the item's state, the retry and the audit log, and records on
  the item which host tagged.

The two are exclusive: `-release-repo` together with `-release-host` is refused
at start-up (and by the server, which then leaves tagging off), and so is a
`-release-host` that is not a peer the hub knows, a `-release-host` on a host with
`-hub` or `-ledger=false`, and `-release-repo` with `-ledger=false` but no `-hub`.
`-release-dry-run` is per host: a peer started with it never tags, whatever
the hub asks; a hub started with it asks every tagging to be a dry run.

If the peer cannot be reached, refuses, or the push fails, the item goes to
**failed** with the reason and **Approve** is offered again. When the hub got no
answer to a tag request, the reason says the tag may have been pushed: trying
again is safe, since the script refuses with "tag exists on origin" if it was.
In that case the page, when it next checks, sees (`--check` prints
`tag_on_origin=<commit>`) that origin has the tag at the item's own commit and
moves the approved item from **failed** to **tagged** itself, with a
`release.reconciled` audit line, so the hosts can be upgraded from it. A tag at
another commit, or an item nobody approved, is never reconciled.
A peer finishes a push it started even if the hub stopped waiting, and does one
tagging at a time. The hub's upgrade gate reads the release build from the same
peer, so the hub needs neither `gh` nor a clone for it.

## Rolling out to all hosts

On the Releases tab, **Roll out to all** upgrades the fleet in one go, run by the hub, so closing the tab or
restarting the hub loses nothing. The confirm names the version, the commit, the hosts in order and the hub wave.

- **Order:** the canary first, alone, then a soak, then the other peers one at a time (fleet order), then the hub
  last. The hub wave is the deploy command (Bump hub) when one is set, else the hub's own upgrade; it changes what
  is running in the cluster and has no automatic undo, so it only follows peers that came back healthy. Before the
  deploy it waits (up to 5 minutes, then it stops) only for a peer of an earlier wave that is reachable on an older
  version (it went back). A peer that is unreachable by then (a worker that powered off when idle) does not hold it,
  the same as everywhere else in the rollout: it was proven on the version when its step finished, and the deploy's
  step names it; a peer the rollout skipped (offline, or failed after the canary) is not counted at all. Its own
  gate is the hub: once the deploy's record is ok and the hub (the new one, after the restart the deploy causes)
  runs the version, the step passes. A peer of an earlier wave that is unreachable by then (a worker that powered
  off when idle) does not hold it, since it was proven on the version when its step finished; the step names it.
  One that is reachable on an older version went back, and stops the rollout.
- **The canary and its soak:** every rollout, automatic or tapped, upgrades the canary first. It is a hub setting
  (the hub's Settings, "Canary host"; `rollout.canary` in `PUT /api/settings`): empty is the default, the
  `-release-canary` peer (`COMEBYE_RELEASE_CANARY`) if one is set, else the release host (`-release-host`), which
  tags and runs Bump hub, so proving it first also proves the machine that runs the last wave; `none` turns the
  canary off. Once the canary is back on the version it soaks for 10 minutes (`rollout.soak_minutes`, 1 to 120)
  before any other host is touched. The soak fails on a restart (the host's comebye process started again: a crash
  loop, even one systemd brings back too fast for the hub to see it offline), a failed health check (offline, or
  it did not answer), a version that went back, or a new upgrade run there that failed. A failed soak stops the
  rollout: nothing else is touched, the canary keeps what it runs (its own `comebye.prev` when its smoke check put
  it back), and the hub files one To do saying what failed. The soak's end is saved in the job, so a hub restart in
  the middle carries on with the time that is left. The Releases tab shows the soak and the time left. A tapped
  rollout can skip the soak, for an urgent fix: **Skip the soak** at the confirm, or on the band while it soaks
  (audited `rollout.soak-skip`); an automatic rollout always soaks. A lone host is no canary. A rollout saved
  before the soak was kept in the job still resumes and finishes as it was planned.
- **Peers upgraded with the hub:** a `-hub-only` peer with no ledger (the in-cluster provisioner) runs from the hub's
  image and is rolled by the hub's deploy, never in place. It says so itself: its `GET /api/release/upgrade` answers
  `with_hub: true` (unless it is started with `-release-upgrade`). The rollout lists it after the hub as "upgraded
  with the hub", never starts it, and after the deploy waits up to 5 minutes for it to be back on the version; one
  that is not is skipped with the reason, not a stop. A hub already on the version is deployed again when such a
  peer is behind. Without a deploy command the hub and these peers are listed as skipped, with why. A deploy
  status is not decided at plan time when the release host is a peer (`-release-host`): the hub is planned as the
  deploy, and its turn decides. A release host that cannot be asked is retried for 5 minutes, then the rollout stops
  at the hub with the real error; one that has no command (`deploy_disabled`) stops it too. Every hub stop or skip
  files the hub's To do, and while the job is stopped there the loop asks the release host again once a minute (for
  a week) and retries the hub step once it answers with a command (`rollout.catchup`). A deploy that ran and failed
  waits for **Retry**.
- **Busy hosts:** restarting comebye leaves agents and herdr alone, but cuts short what the comebye process itself
  holds: an agent sign-in in progress (kept in memory only), a tagging it is pushing, or a deploy command it runs.
  Each host names these in its `GET /api/release/upgrade` (`busy`: `claude sign-in`, `tagging`, `deploy`), and a
  step whose host is busy waits: the Releases tab shows "waiting: busy (…)" on its row, audited `rollout.busy`. When
  the host is free the upgrade starts at once and the step says how long it waited. After 15 minutes it goes ahead
  anyway, and the step keeps "went ahead after waiting 15m: still busy (…)". The wait's start is saved in the job,
  so a hub restart does not start it again. The hub's own step waits for the hub in the same way. While a host
  restarts, the `comebye` CLI there retries for up to 20 seconds (see [agents.md](agents.md#the-cli-through-a-restart)).
- **Hosts the app may not upgrade** (no `-release-upgrade`) are listed as skipped, with why; they do not hold up
  the others.
- **Gates:** a host's own upgrade record says ok (its smoke check ran, and it put the old binary back if not), then
  the hub sees it back online on the version (5 minutes). A failure on the canary, on the hub's wave, or in a
  rollout with no canary that proved the release (none set, a lone host, the canary skipped while offline) stops the
  whole rollout at once: the rest are not touched, hosts already done stay done. **Retry** runs the failed step
  again; **Cancel** ends it and reverts nothing.
- **A host that fails after the canary:** once the canary has upgraded, been verified and soaked, a later peer whose
  upgrade or verify fails (its run failed or rolled back, it could not be started, it did not come back on the
  version) is skipped with the reason and its output (`rollout.skip`, ok=false), and the rollout goes on to the
  others; one broken host no longer holds the fleet, or every later release, back. The rollout files **one** To do
  for all such hosts ("The v0.5.70 rollout skipped wk1, wk2: the upgrade failed", each with its reason and the last
  lines of its output), rewritten as more fail, never one per host or per retry; the host's own "upgrade of HOST
  failed" item is not filed for them. It closes with the release item, or when a newer release supersedes it. The
  rollout finishes `not ok: … K skipped (…)`. **Retrying:** such a host is not caught up by this release (that would
  try a broken host again and again); **Roll out again** on the Fleet card retries it now, and the next release's
  rollout tries it once more, like any host behind (and skips it again, with its own one To do, if it fails again).
- **Offline hosts** are skipped after 2 minutes and listed. The loop keeps looking (once a minute): when a skipped
  host is back, it upgrades it through the same gates (`rollout.catchup`), and the job is done again after it. While
  the automatic rollout is on there is no time limit; with it off, it looks for a week after the rollout finished.
  **Roll out again** does the same at once for just those hosts.
- **Catch-up without a job:** while the automatic rollout is on, every 5 minutes the same catch-up also looks for a reachable
  host behind the newest release it rolled out (one that was stopped when it ran, or a worker made at an older
  version). A host in the job is caught up by it; a host it never had gets a step added to it; and when that release
  had no job at all (every running host was already current: "none: nothing to upgrade") the hub starts one then,
  for the hosts behind (`rollout.auto`, "a host is behind it"). The newest tagged release's item is kept past the
  to-do retention for this.
- **Done with hosts away:** with the automatic rollout off, while every host still behind is offline (a laptop away
  for a while), **Done, skip offline hosts** on the Fleet card (or `comebye todo done ID --skip-offline`) closes the release item. The item
  names the hosts left behind (audited `release.done-skip-offline`), and the catch-up above still upgrades the ones
  the rollout skipped when they are back. A plain done is refused while any host is behind, and the hub or a
  reachable host behind is never skipped.
- **A stuck rollout is always reachable:** there is one rollout at a time, and a live one (running, paused or
  stopped) of an older release than the one the Releases tab shows still shows its band there, saying which release
  it is (with a link to its item), with **Retry** and **Cancel** (or Pause / Resume); the tab's badge is on while it
  is stopped or paused, and the per-host buttons are locked meanwhile, as for the shown release's own rollout.
- **Pause** takes effect when the step in flight ends; it never interrupts an install or a restart. While a rollout
  is live the per-host Upgrade, Roll back and Bump hub buttons are locked.
- **State:** `<state-dir>/release/rollout.json`, written atomically and read by the ledger host's release loop every
  5 s while a rollout is live (every 30 s otherwise). That one loop also runs the automatic rollout (once a minute),
  the catch-up and the release sweep (every 30 s), and its steps share one answer from the fleet: each peer is asked
  for its release status once per tick, and asked afresh as soon as an upgrade or a deploy starts. Each step is derived from the file, the hosts' upgrade records and the fleet's versions, so a restart of
  the hub, including the one the hub wave itself causes, carries on where it was. Every step is in the audit log
  (`rollout.start`, `.step`, `.gate`, `.skip`, `.pause`, `.resume`, `.resume-after-restart`, `.stop`, `.cancel`, `.supersede`,
  `.finish`, `.catchup`) with the rollout id. A stopped rollout, a cancelled one and a skipped host leave a line on the
  release's to-do item.
- **Finishing:** a rollout with every host upgraded finishes `ok: all N hosts`. One that skipped only hosts that were
  stopped or offline (to be caught up) finishes `ok: X of N hosts upgraded, K behind until they are back (…)`. One
  that skipped a host for any other reason (the hub, a host the app may not upgrade, an error) finishes
  `not ok: X of N hosts upgraded, K skipped (…)` (audited with ok=false); when the hub was skipped it adds "the hub
  was not bumped", and the item's line says to bump it from its row in Peers (Roll out again never retries the hub).
- **One note:** the first time a rollout finishes or stops, automatic or tapped, the hub sends the user one push
  notification (a "done" kind, or a "blocked" kind for a stop; it opens the release item) and puts the same line on
  the item: "v0.5.59 is on every host", "v0.5.59 is on every host (2 behind until they're back: wk1, wk2)", or
  "v0.5.59 stopped at canary-host: …". It is audited `rollout.told`. The job saves when it was sent (`told`), so exactly
  one goes out per rollout: none after a hub restart, a Retry, the hub retried, a catch-up, or a job the hub starts
  only to catch a host up. A stop has already filed its To do (above), and the note adds none.
- **When the hub bump fails after its push:** homelab's `comebye-hub-bump.sh` pushes the new pins to homelab main
  before it makes Flux apply them and waits for the hub. A failure after that (phase `reconcile`, `rollout`, `readyz`
  or `provisioner`) leaves main pinning the new version, and Flux applies it anyway; there is no automatic undo, and
  the hub runs one pod. The hub's To do then names the commit that was pushed and the exact way back, run on the
  release host: `comebye-hub-bump vPREV --allow-older`, with the version the hub ran before (the rollout's hub step
  keeps it, else the hub's own when it still runs it). That pins vPREV again in the same two manifests, with its
  checksum from that release, pushes it, makes Flux apply it and waits until the hub reports it. `--allow-older` is
  what lets the script take a version older than the one main pins; without it the script still refuses.
- The user starts one from the page (user role and the page's CSRF token; the body repeats version, commit and the
  ordered hosts), or the hub does, by itself:
- **Automatic rollout** (on by default): once a release item the user approved is tagged and its Release workflow
  is green, the hub starts the same rollout Roll out to all starts (every peer behind, then the hub wave), with no
  tap (`rollout.auto`, and `auto=true` on `rollout.start`). The loop looks once a minute at the newest open, tagged
  item tagged in the last 24 hours, and waits while another rollout is live or a deploy runs. A red or running build
  starts nothing. A rollout of an older release that **stopped** does not hold a newer one back: once the newer
  release's build is green, the stopped job is ended (`rollout.supersede`, "superseded by v…"), its release item
  is closed with that line (`release.superseded`) and its rollout alarm with it, and the newer rollout starts; it
  goes through the same hosts and gates. A paused rollout, or a running one, is still waited for. Whenever a
  release's rollout starts (automatic or tapped), every older release item that never started one (a release the
  stop held back, then a newer one overtook) is closed too, with "vX was never rolled out: superseded by vY"
  (`release.superseded`); an item being tagged right now is left alone. It starts once per release: the item keeps the job's id (`auto_rollout`), and the job on file
  names its item, so a hub restart between the tag and the start, or between the start and that mark, neither
  misses the release nor starts it twice. An automatic rollout never wakes a stopped worker: it is listed as skipped
  and upgraded when it next starts. Turn it off with **Roll out automatically** in the hub's settings (Peers, the
  hub's row: `auto_rollout: false` in `settings.json`); Roll out to all is then the only way. Under `-test-mode`
  (the browser tests, which tap Roll out themselves) it is off unless the setting turns it on.

## Deploying a release: your own command

Some deployments of comebye are not upgraded by `comebye upgrade` on the host: a
hub in a container, a binary pinned in a GitOps repository. For those, the release
host can run **your own command** from a second button on the release item. comebye
knows nothing about what it does.

- `-release-deploy-cmd '/abs/path/to/script [args...]'` (`COMEBYE_RELEASE_DEPLOY_CMD`)
  on the host that owns `-release-repo` (the peer, when the hub uses `-release-host`;
  the hub then needs no setting). The program is named by absolute path and run
  without a shell; the released version (`vX.Y.Z`) is appended as the last argument
  and set in `COMEBYE_RELEASE_VERSION` (also `COMEBYE_RELEASE_TODO`, `COMEBYE_RELEASE_BY`).
  `-release-deploy-label` names the button (default **Deploy**).
- **Approve release never runs it.** The button appears on a tagged item once the
  Release workflow has passed, and needs its own confirm naming the version. It is
  the user's alone, with the same CSRF and user-role rules as the other buttons; the
  hub asks the release host, which checks the build itself again before it starts.
- The command may restart the hub that asked for it, so it runs in the background on
  the release host, one at a time (timeout 10 minutes, the whole process group is
  killed on timeout). Its verdict and the tail of its output are kept in
  `<state-dir>/release/deploy.json` and read by the page whenever it asks, also
  after the hub came back. A run that was cut short by a restart of the release host
  reads as failed. The audit log gets `deploy.request` and, once seen, `deploy.result`.
- **What the page shows of the output.** A failed run leads with its
  `FAILED phase=… reason=…` line (else `exit status N`) and the last 15 lines in a
  monospace block; a running one shows its latest line; a good one shows its
  `bump: ok …` line with the full output behind "Show output". The output is
  untrusted text: it is shown as plain text, never HTML. Before it is kept or
  served, terminal escapes are stripped, the tail is capped at 4000 bytes, and
  obvious secrets are blanked (`ghp_…`/`github_pat_…`/`glpat-…`/`xox…`/`sk-…`/`AKIA…`
  tokens, JWTs, `Bearer …`, `user:password@` in URLs, `token=`/`password=`/`secret=`
  style values, private key blocks). That is a safety net only: **the deploy command
  must not print secrets**, because what it prints is served to the page and kept
  in `deploy.json` on the release host.
- **A failure files a to-do.** When the hub settles a failed run of the deploy
  command, or a failed upgrade of a host to a tagged release (from the item, a
  rollout or the release sweep; a rollback or dry run does not count), it files one
  **action** item of its own: "The vX.Y.Z hub rollout failed" (or "… upgrade of
  HOST failed") with the phase, the reason, the last lines of the cleaned output and
  a link to the release item. There is one open item per release and target: a
  second failure while it is open adds nothing, and a later run that succeeds closes
  it. A new failure after it closed files a new one. A host a rollout skipped because it failed after the canary is
  named in that rollout's one To do instead (see "A host that fails after the canary" above).
- `-release-dry-run` runs nothing and records a dry run.

### Simple setup: your own comebye holds the clone

For a single user this is fine. The comebye you already run on a host (say
laharl) takes the clone and the credentials you already have there, with no
new account:

- A fresh dedicated clone, say `~/release/comebye`, of
  `github.com/jhjaggars/comebye` that no agent works in. Leave its git config at
  the clone defaults; `tools/release/tag --check` should print `ok=1` there.
- In that comebye's env: `COMEBYE_RELEASE_REPO=<the clone>`, and
  `COMEBYE_RELEASE_DRY_RUN=true` until you have rehearsed. The host runs with
  `-hub`, as every peer does. Restart it (agents keep running); the journal
  says `release: the app may tag releases from <clone> (dry run true)`.
- On the hub: `COMEBYE_RELEASE_HOST=<that peer's name>` and no
  `COMEBYE_RELEASE_REPO`.

The cost is the one explained below: an agent running as the same user could
forge the hub's call or read the credentials. The script's checks still bound
what a forged call can tag.

### Optional hardening: a dedicated OS user for the release peer

To close that gap, run the peer that tags as **its own OS user**, not as the user
the agents run as. On a host where agents work (laharl), that means a second comebye instance:

- A dedicated account (say `comebye-release`) with its **own home**: no shared
  `HOME`, and nothing of it readable by the agents' user (mode 700 on the home).
- Its own comebye service started as that user with `-hub <the hub's Serve URL>`
  and `-release-repo <its own dedicated clone>`, its own `-host-name` (say
  `laharl-release`), its own listen port and its own Tailscale Serve name or
  port. It is added to the hub as a separate peer, and the hub sets
  `-release-host laharl-release`. Its `-ts-allow-login` must include the hub
  device owner's login, as for any peer.
- Its own credentials, and only these: a `gh` login (or token) limited to this
  repository, with the access the Release workflow needs, and git push rights
  to `origin` (an SSH key or credential that only this account can read, ideally
  a deploy key or fine-grained token that can push tags to this one repository).
  Nothing else of the agents' user's keys, `~/.gitconfig` or `~/.config/gh`
  is shared.
- The clone is owned by that account and no agent works in it.

Why: the hub-call marker is a guard, not authentication. Anything that can send
the hub device owner's login to the peer's port with `X-Comebye-Hub-Call: 1`
can call `release-tag`. If the peer runs as the agents' own user, an agent there
can do that (or edit the clone's config and credentials) with no click. As a
separate user, an agent on that host cannot read the credentials or the clone,
and the script's checks still bound what a forged call to the loopback port can
tag: a CI-green commit of `main`, newer than the last tag. The agents' own
comebye on that host stays as it is and has no `-release-repo`.
