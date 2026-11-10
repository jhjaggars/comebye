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
the version (it checks every 30 s and whenever the tab loads). An **offline host
blocks that**: the tab says "3 of 4, 1 offline", the release is not done and the
item stays open until the host is back and upgraded. Closing the item by hand
while a host is behind or offline is refused (`release_pending`); drop it only to
abandon the release, and the tab still shows a tagged one until it is rolled out.
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
runs as a transient `comebye-upgrade-*` user unit, because it restarts the
service that started it; its verdict is kept in
`<state-dir>/release/upgrade.json`.

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
