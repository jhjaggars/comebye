#!/usr/bin/env bash
# The guard in cleanup.sh: gpgconf runs only for a GNUPGHOME inside the run's temp directory. A fake gpgconf logs its calls.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$here/cleanup.sh"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"; printf '#!/bin/sh\necho "$GNUPGHOME $*" >> "%s/calls"\n' "$work" > "$work/bin/gpgconf"; chmod +x "$work/bin/gpgconf"
export PATH="$work/bin:$PATH"
fail=0
try() { # name, GNUPGHOME (or "unset"), want calls
  local name="$1" g="$2" want="$3" tmp; tmp="$(mktemp -d)"; : > "$work/calls"
  if [ "$g" = unset ]; then unset GNUPGHOME; else export GNUPGHOME="${g//@TMP@/$tmp}"; fi
  smoke_cleanup "$tmp"
  local got; got="$(wc -l < "$work/calls" | tr -d ' ')"
  [ -d "$tmp" ] && { echo "FAIL $name: the temp directory is still there"; fail=1; }
  if [ "$got" != "$want" ]; then echo "FAIL $name: gpgconf ran $got times, want $want"; fail=1; else echo "ok   $name"; fi
}
HOME_REAL="$HOME"
try "unset GNUPGHOME: the user's own agent is left alone" unset 0
try "the real ~/.gnupg is left alone" "$HOME_REAL/.gnupg" 0
try "a directory outside the run's temp dir is left alone" "/tmp/somebody-elses-gnupg" 0
try "the run's own GNUPGHOME is cleaned up" "@TMP@/gnupg" 1
exit $fail
