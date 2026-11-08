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

The To do page shows the item with what matters, taken from the hub's own
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
in the item, and **Roll back** runs `comebye upgrade --rollback`. The upgrade
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
