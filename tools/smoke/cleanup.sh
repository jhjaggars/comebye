# Sourced by run.sh: the exit trap's body. It may stop a gpg-agent only when GNUPGHOME is a directory under the run's own temp
# directory ($1): never the user's real agent (their ~/.gnupg, or whatever GNUPGHOME was in their shell), whose death would end a
# live unlock. Tested by cleanup_test.sh.
smoke_cleanup() {
  local tmp="$1" g="${GNUPGHOME:-}"
  if [ -n "$tmp" ] && [ -n "$g" ] && [ "$g" != "$HOME/.gnupg" ] && case "$g" in "$tmp"/*) true ;; *) false ;; esac; then
    gpgconf --kill gpg-agent 2>/dev/null || true
  fi
  kill $(jobs -p) 2>/dev/null || true
  [ -n "$tmp" ] && rm -rf "$tmp"
}
