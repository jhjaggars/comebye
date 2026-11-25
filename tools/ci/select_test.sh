#!/usr/bin/env bash
# Self-test of the CI selection (no network, no browser, a few seconds): tools/ci/select.sh's answers for a few changes,
# that tools/ci/browser.sh's parts hold every script exactly once, and gotest.sh's --shard argument checks.
#
#   tools/ci/select_test.sh
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }

# sel PATHS...: select.sh's output for these changed paths. key OUT NAME: one value.
sel() { printf '%s\n' "$@" | tools/ci/select.sh; }
key() { sed -n "s/^$2=//p" <<< "$1"; }
expect() { # NAME OUT KEY WANT
  local got; got="$(key "$2" "$3")"
  if [ "$got" = "$4" ]; then ok "$1: $3=$4"; else bad "$1: $3 is '$got', want '$4'"; fi
}

o="$(sel docs/x.md)"
expect "docs only" "$o" lint false
expect "docs only" "$o" go none
expect "docs only" "$o" scripts ""
expect "docs only" "$o" go_shards 1
expect "docs only" "$o" browser_parallel false

o="$(sel .github/workflows/ci.yml)"
expect "CI change" "$o" scripts all
expect "CI change" "$o" go all
expect "CI change" "$o" go_shards 3
expect "CI change" "$o" browser_parallel true

# One small extra script: the smoke job (about 130 s plus 7 s).
o="$(sel tools/uitest/peers.mjs)"
expect "one script" "$o" scripts peers
expect "one script" "$o" smoke true
expect "one script" "$o" browser_parallel false
expect "one script" "$o" go_shards 1

# The side panel's five scripts (offline and saved alone are nearly 4 minutes): the parallel jobs.
o="$(sel web/sidepanel.js)"
expect "side panel" "$o" scripts "sidepanel statussplit pagessplit offline saved shell"
expect "side panel" "$o" browser_parallel true
expect "side panel" "$o" visual false

# internal/server is among the packages: go-changed in 3 jobs.
o="$(sel internal/server/x.go)"
expect "server" "$o" go_shards 3
case " $(key "$o" go) " in *" ./internal/server "*) ok "server: go names ./internal/server" ;; *) bad "server: go is '$(key "$o" go)'" ;; esac

# go_shards follows whether ./internal/server is in go, whatever imports what.
# (Labels name the directory: "x.go: " in the log would look like a compiler error to the runner's Go problem matcher.)
for d in internal/pageskit internal/usage tools/genicon internal/fleet; do
  o="$(sel "$d/x.go")"; g=" $(key "$o" go) "; want=1
  case "$g" in *" ./internal/server "*) want=3 ;; esac
  expect "a change in $d" "$o" go_shards "$want"
done

# A visual script among the extras turns the visual pass on when the run goes parallel (the rest parts leave it out),
# and leaves it as it was when it stays in the smoke job.
o="$(sel internal/pageskit/x.go)"
expect "pages kit" "$o" scripts "pages starters"
expect "pages kit" "$o" browser_parallel false
expect "pages kit" "$o" visual false
o="$(printf 'internal/pageskit/x.go\n' | SMOKE_MAX=100 tools/ci/select.sh)"
expect "pages kit, SMOKE_MAX=100" "$o" browser_parallel true
expect "pages kit, SMOKE_MAX=100" "$o" visual true
expect "pages kit, SMOKE_MAX=100" "$o" scripts "pages starters"

# run.sh's fixture hubs each have a port of their own (keyed and pullrefresh once shared 283).
dup="$(grep -o 'fixture_hub [a-z]* [0-9]*' tools/uitest/run.sh | awk '{print $3}' | sort | uniq -d | tr '\n' ' ')"
if [ -z "$dup" ]; then ok "run.sh: every fixture hub has its own port"; else bad "run.sh: fixture hubs share the port offset(s) $dup"; fi

# The parts: every rest and visual script in exactly one part, for the counts CI uses and a few others.
all="$(grep -o 'want [a-z]*;' tools/uitest/run.sh | awk '{print $2}' | tr -d ';' | sort)"
visual="appearance depth gradients landscape shapes starters"
rest="$(comm -23 <(echo "$all") <(printf '%s\n' drive $visual | sort))"
check_parts() { # SUITE N WANT
  local got; got="$(tools/ci/browser.sh parts "$1" "$2" | sed 's/^[^:]*: //' | tr ' ' '\n' | sed '/^$/d' | sort)"
  if [ "$got" = "$3" ]; then ok "$1 in $2 parts: every script once ($(wc -l <<< "$got"))"
  else bad "$1 in $2 parts: $(diff <(echo "$3") <(echo "$got") | grep '^[<>]' | tr '\n' ' ')"; fi
  if tools/ci/browser.sh parts "$1" "$2" | grep -q ':\s*$'; then bad "$1 in $2 parts: an empty part"; fi
}
for n in 4 6 7; do check_parts rest "$n" "$rest"; done
check_parts visual 2 "$(printf '%s\n' $visual | sort)"

# gotest.sh --shard with packages needs internal/server among them (it splits that package's tests).
if out="$(TEST_VM=local tools/ci/gotest.sh --shard 1/2 ./internal/usage 2>&1)"; then bad "gotest --shard without internal/server ran: $out"
else case "$out" in *"name ./internal/server"*) ok "gotest --shard without internal/server is refused" ;; *) bad "gotest --shard: $out" ;; esac; fi
if out="$(TEST_VM=local tools/ci/gotest.sh --shard 4/3 2>&1)"; then bad "gotest --shard 4/3 ran"
else ok "gotest --shard 4/3 is refused"; fi
if out="$(TEST_VM=local tools/ci/gotest.sh --shard 1/4 2>&1)"; then bad "gotest --shard 1/4 ran"
else ok "gotest --shard 1/4 is refused"; fi

echo
[ "$fails" = 0 ] && echo "select_test: all ok" || echo "select_test: $fails FAILED"
[ "$fails" = 0 ]
