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
