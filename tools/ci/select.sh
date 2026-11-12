#!/usr/bin/env bash
# Which checks does a change need? Reads the changed paths (one per line) on stdin, or with --base REF
# from `git diff --name-only REF...HEAD`, and prints key=value lines (CI appends them to $GITHUB_OUTPUT):
#
#   lint=true|false         gofmt, vet, build, node --check, tokens, contrast (cheap: false only for docs)
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

while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    docs/*|*.md|LICENSE|.gitignore|deploy/*) ;;                       # nothing to test
    .github/*|go.mod|go.sum|tools/uitest/package.json|tools/uitest/package-lock.json|tools/uitest/run.sh|tools/uitest/lib.mjs|tools/ci/*)
      everything=true ;;
    web/themes.js) lint=true; smoke=true; visual=true; merge_browser=true ;;
    web/sidepanel.js) lint=true; smoke=true; add_script sidepanel; merge_browser=true; add_pkg ./web ;;
    web/test/*) lint=true; add_script markdown; merge_browser=true ;;
    web/*) lint=true; smoke=true; visual=true; merge_browser=true; merge_go=true; add_pkg ./web; add_pkg ./internal/server ;;
    main.go|announce.go|main_test.go) lint=true; smoke=true; merge_browser=true; merge_go=true; add_pkg . ;;
    internal/server/*|internal/herdr/*|internal/state/*)
      lint=true; smoke=true; merge_browser=true; merge_go=true; add_pkg "./$(dirname "$f")" ;;
    internal/pageskit/*) lint=true; smoke=true; add_script pages; add_script starters; merge_browser=true; merge_go=true; add_pkg ./internal/pageskit ;;
    internal/fleet/*) lint=true; add_script peers; merge_browser=true; merge_go=true; add_pkg ./internal/fleet ;;
    tools/worker/*|internal/provision/*) lint=true; add_script provision; add_script provhosts; merge_browser=true; merge_go=true; add_pkg "./$(dirname "$f")" ;;
    internal/cli/*) lint=true; add_script pages; add_script starters; merge_browser=true; merge_go=true; add_pkg ./internal/cli ;;
    internal/usage/*) lint=true; add_script usage; merge_browser=true; merge_go=true; add_pkg ./internal/usage ;;
    internal/release/*|internal/upgrade/*) lint=true; add_script releaseauto; merge_browser=true; merge_go=true; add_pkg "./$(dirname "$f")" ;;
    tools/uitest/*.mjs)
      lint=true; smoke=true; merge_browser=true
      b="$(basename "$f" .mjs)"; add_script "$b"; case "$b" in buttons|chips|controls|fontsetting|decisionsets|starters|empties|contrast|tokens) visual=true;; esac ;;
    tools/uitest/*|tools/devserver/*) lint=true; smoke=true; merge_browser=true ;;
    # A leaf Go package: nothing the browser scripts reach (they go through the hub), so Go tests only.
    internal/*/*.go|internal/*/*/*) lint=true; merge_go=true; add_pkg "./$(dirname "$f")" ;;
    tools/demo/*|tools/genicon/*|tools/release/*|tools/stt/*) lint=true; merge_go=true; add_pkg "./$(dirname "$f")" ;;
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
