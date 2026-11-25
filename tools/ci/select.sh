#!/usr/bin/env bash
# Which checks does a change need? Reads the changed paths (one per line) on stdin, or with --base REF
# from `git diff --name-only REF...HEAD`, and prints key=value lines (CI appends them to $GITHUB_OUTPUT):
#
#   lint=true|false         gofmt, vet, build, node --check, static checks, contrast (cheap: false only for docs)
#   go=none|all|PKGS        pre-submit: Go packages to test (the ones touched and their importers)
#   scripts=...|all         pre-submit: browser scripts for the touched paths, besides the smoke set
#   smoke=true|false        pre-submit: the browser smoke set
#   visual=true|false       the two-theme visual pass is needed
#   merge_go=true|false     merge: all Go tests
#   merge_browser=true|false  merge: all browser checks
#
# The rule table is docs/test-tiers.md. A path that matches no rule means "everything", so a new directory
# costs a full run until it is added here. This script never runs fewer checks than the table says.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

if [ "${1:-}" = "--base" ]; then
  files="$(git diff --name-only "$2...HEAD")"
else
  files="$(cat)"
fi

go_all=false; go_pkgs=""; smoke=false; visual=false; scripts=""; lint=false
merge_go=false; merge_browser=false; everything=false

add_script() { case " $scripts " in *" $1 "*) ;; *) scripts="$scripts $1";; esac; }
add_pkg() { case " $go_pkgs " in *" $1 "*) ;; *) go_pkgs="$go_pkgs $1";; esac; }

# The package a path belongs to: its first two components (internal/x, tools/x), so a testdata file or a
# subdirectory names the package that owns it, not a directory with no Go files.
pkg_of() { local a b rest; IFS=/ read -r a b rest <<< "$1"; echo "./$a/$b"; }

while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    # Files that Go tests of OTHER packages read straight from disk (os.ReadFile("../../...")): they are not
    # imports, so the importer walk below cannot see them. Keep this list in step with those tests:
    #   pageskit_test.go: web/themes.js, web/index.html   lifecycle_test.go, handoff_test.go: docs/lifecycle.md
    #   handoff_test.go, playbook_test.go: internal/playbook/playbook.md   interop_test.go: web/test/unlockinterop.mjs
    #   release_test.go: tools/release/tag   provision/server tests: tools/worker/testdata, internal/provision/testdata
    #   dialog tests (state, server): internal/dialog/testdata
    docs/lifecycle.md) lint=true; merge_go=true; add_pkg ./internal/cli; add_pkg ./internal/server ;;
    internal/playbook/*) lint=true; merge_go=true; add_pkg ./internal/playbook; add_pkg ./internal/server ;;
    internal/dialog/*) lint=true; merge_go=true; add_pkg ./internal/dialog; add_pkg ./internal/state; add_pkg ./internal/server ;;
    web/test/unlockinterop.mjs) lint=true; merge_browser=true; merge_go=true; add_script markdown; add_pkg ./internal/gpgunlock ;;
    tools/release/tag) lint=true; merge_go=true; add_pkg .; add_pkg ./internal/release ;;   # main.go embeds it
    tools/worker/testdata/*) lint=true; add_script provision; add_script provhosts; add_script provkubevirt; add_script wake; add_script multiconfig; add_script wakeuse; add_script wakepolish; merge_browser=true; merge_go=true; add_pkg ./tools/worker; add_pkg ./internal/provision; add_pkg ./internal/server ;;
    # The provisioner example manifests have a Go test (deploy/kubernetes/provisioner/manifests_test.go): the Role's allow-list, the Secret lint, kustomize.
    deploy/kubernetes/provisioner/*) lint=true; merge_go=true; add_pkg ./deploy/kubernetes/provisioner ;;
    docs/*|*.md|LICENSE|.gitignore|deploy/*) ;;                       # nothing to test
    .github/*|go.mod|go.sum|tools/uitest/package.json|tools/uitest/package-lock.json|tools/uitest/run.sh|tools/uitest/lib.mjs|tools/ci/*)
      everything=true ;;
    web/themes.js) lint=true; smoke=true; visual=true; merge_browser=true; merge_go=true; add_pkg ./internal/pageskit ;;
    web/sidepanel.js) lint=true; smoke=true; add_script sidepanel; add_script statussplit; add_script pagessplit; add_script offline; merge_browser=true; add_pkg ./web ;;
    web/test/*) lint=true; add_script markdown; merge_browser=true ;;
    web/app.js) lint=true; smoke=true; visual=true; add_script offline; merge_browser=true; merge_go=true; add_pkg ./web; add_pkg ./internal/server; add_pkg ./internal/pageskit ;;
    web/*) lint=true; smoke=true; visual=true; merge_browser=true; merge_go=true; add_pkg ./web; add_pkg ./internal/server; add_pkg ./internal/pageskit ;;
    main.go|announce.go|main_test.go) lint=true; smoke=true; merge_browser=true; merge_go=true; add_pkg . ;;
    internal/server/*|internal/herdr/*|internal/state/*)
      lint=true; smoke=true; merge_browser=true; merge_go=true; add_pkg "$(pkg_of "$f")" ;;
    internal/pageskit/*) lint=true; smoke=true; add_script pages; add_script starters; merge_browser=true; merge_go=true; add_pkg ./internal/pageskit ;;
    internal/fleet/*) lint=true; add_script peers; add_script testruns; merge_browser=true; merge_go=true; add_pkg ./internal/fleet ;;
    internal/testruns/*) lint=true; add_script testruns; merge_browser=true; merge_go=true; add_pkg ./internal/testruns; add_pkg ./internal/fleet ;;
    tools/worker/*|internal/provision/*) lint=true; add_script provision; add_script provhosts; add_script provkubevirt; add_script wake; add_script multiconfig; add_script wakeuse; add_script wakepolish; merge_browser=true; merge_go=true; add_pkg "$(pkg_of "$f")"
      # internal/server's tests read the provision and tools/worker golden files and testdata
      add_pkg ./internal/provision; add_pkg ./internal/server ;;
    internal/cli/*) lint=true; add_script pages; add_script starters; merge_browser=true; merge_go=true; add_pkg ./internal/cli ;;
    # `comebye api`'s module: the cli runs it, internal/server's golden test checks the generated records.
    internal/apipy/*) lint=true; merge_go=true; add_pkg ./internal/apipy; add_pkg ./internal/cli; add_pkg ./internal/server ;;
    internal/usage/*) lint=true; add_script usage; merge_browser=true; merge_go=true; add_pkg ./internal/usage ;;
    internal/release/*|internal/upgrade/*) lint=true; add_script releaseauto; merge_browser=true; merge_go=true; add_pkg "$(pkg_of "$f")" ;;
    # The day-in-the-life smoke (tools/smoke) runs whenever the browser smoke set does (see ci.yml); its own files run it.
    tools/smoke/*) lint=true; smoke=true; merge_browser=true; merge_go=true; add_pkg ./tools/smoke/fakeherdr ;;
    # The helper the GPG scripts share: both of them, and (through tools/smoke/day.mjs) the day in the life.
    tools/uitest/gpghelp.mjs) lint=true; smoke=true; merge_browser=true; add_script gpgunlock ;;
    tools/uitest/*.mjs)
      lint=true; smoke=true; merge_browser=true
      b="$(basename "$f" .mjs)"; add_script "$b"; case "$b" in shapes|appearance|starters|depth|landscape|empties|contrast|tokens|static) visual=true;; esac ;;
    tools/uitest/*|tools/devserver/*) lint=true; smoke=true; merge_browser=true ;;
    # A leaf Go package: nothing the browser scripts reach (they go through the hub), so Go tests only.
    internal/*/*.go|internal/*/*/*) lint=true; merge_go=true; add_pkg "$(pkg_of "$f")" ;;
    tools/genicon/*) lint=true; merge_go=true; add_pkg "$(pkg_of "$f")" ;;
    # No Go files: node --check and the static checks only.
    tools/demo/*|tools/release/*|tools/stt/*) lint=true ;;
    *.go) lint=true; merge_go=true; add_pkg . ;;
    *) everything=true ;;                                             # unknown: run everything
  esac
done <<< "$files"

if $everything; then
  echo "lint=true"; echo "go=all"; echo "scripts=all"; echo "smoke=true"; echo "visual=true"
  echo "merge_go=true"; echo "merge_browser=true"; exit 0
fi

# Importers: a changed package's tests are not enough, so add every package that imports it.
go_out="none"
if [ -n "$go_pkgs" ]; then
  affected="$go_pkgs"
  if command -v go >/dev/null 2>&1; then
    mod="$(go list -m)"
    all="$(go list -f '{{.ImportPath}} {{join .Deps " "}}' ./... 2>/dev/null || true)"
    for p in $go_pkgs; do
      imp="$mod/${p#./}"; [ "$p" = . ] && imp="$mod"
      while read -r pkg deps; do
        case " $deps " in *" $imp "*) affected="$affected ./${pkg#"$mod"/}";; esac
      done <<< "$all"
    done
  fi
  go_out="$(for p in $affected; do echo "$p"; done | sed 's|^\./\.$|.|;s|^\./'"${mod:-x}"'$|.|' | sort -u | tr '\n' ' ' | sed 's/ $//')"
fi

echo "lint=$lint"
echo "go=$go_out"
echo "scripts=$(echo $scripts)"
echo "smoke=$smoke"
echo "visual=$visual"
echo "merge_go=$merge_go"
echo "merge_browser=$merge_browser"
