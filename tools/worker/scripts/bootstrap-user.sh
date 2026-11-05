#!/bin/bash
# Run as the worker user over verified ssh (stdin). Idempotent. The caller
# prepends the WK_* assignments (validated by the provisioner) and appends
# nothing else. No secret passes through here.
set -euo pipefail
: "${WK_VERSION:?}" "${WK_RELEASES:?}" "${WK_HERDR_URL:?}" "${WK_HUB:?}" "${WK_NAME:?}" "${WK_LOGIN:?}"
WK_HERDR_SHA="${WK_HERDR_SHA:-}"
WK_AGENTS="${WK_AGENTS:-pi}"
WK_PI_VERSION="${WK_PI_VERSION:-}"
export PATH="$HOME/.local/bin:$PATH"
export XDG_RUNTIME_DIR="/run/user/$(id -u)"
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user" "$HOME/.config/comebye"

# The user manager starts with linger; wait for it before any systemctl --user.
for _ in $(seq 1 30); do [ -S "$XDG_RUNTIME_DIR/bus" ] && break; sleep 1; done
[ -S "$XDG_RUNTIME_DIR/bus" ] || { echo "no user systemd bus" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# ---- herdr, as a user service
if ! command -v herdr >/dev/null; then
  curl -fsSL "$WK_HERDR_URL" -o "$tmp/herdr-install.sh"
  if [ -n "$WK_HERDR_SHA" ]; then
    echo "$WK_HERDR_SHA  $tmp/herdr-install.sh" | sha256sum -c - >/dev/null
  else
    echo "warning: herdr_install_sha256 not set: the install script is not pinned" >&2
  fi
  sh "$tmp/herdr-install.sh"
  hash -r
fi
command -v herdr >/dev/null || { echo "herdr is not on PATH after installing" >&2; exit 1; }
[ -x "$HOME/.local/bin/herdr" ] || ln -sf "$(command -v herdr)" "$HOME/.local/bin/herdr"
cat >"$HOME/.config/systemd/user/herdr.service" <<'UNIT'
[Unit]
Description=herdr headless server

[Service]
# Panes inherit this PATH: the agent CLIs (claude, codex, mise) live in
# ~/.local/bin, which a user service does not have by default.
Environment=PATH=%h/.local/bin:%h/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/.local/bin/herdr server
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
UNIT

# ---- agent CLIs: installed, never signed in
case ",$WK_AGENTS," in *,claude,*)
  command -v claude >/dev/null || curl -fsSL https://claude.ai/install.sh | bash ||
    echo "warning: installing claude failed" >&2 ;; esac
case ",$WK_AGENTS," in *,codex,*)
  if ! command -v codex >/dev/null; then
    { command -v mise >/dev/null || curl -fsSL https://mise.run | sh; } &&
      "$HOME/.local/bin/mise" use -g node@lts >/dev/null &&
      "$HOME/.local/bin/mise" exec -- npm install -g @openai/codex >/dev/null &&
      printf '#!/bin/sh\nexec "$HOME/.local/bin/mise" exec -- codex "$@"\n' >"$HOME/.local/bin/codex" &&
      chmod +x "$HOME/.local/bin/codex" ||
      echo "warning: installing codex failed" >&2
  fi ;; esac
# pi, at the pinned version (npm's own integrity checks apply; the package ships
# a shrinkwrap). A missing harness is a failed worker, not a warning.
case ",$WK_AGENTS," in *,pi,*)
  : "${WK_PI_VERSION:?}"
  if [ "$("$HOME/.local/bin/pi" --version 2>/dev/null | head -1)" != "$WK_PI_VERSION" ]; then
    { command -v mise >/dev/null || curl -fsSL https://mise.run | sh; } &&
      "$HOME/.local/bin/mise" use -g node@lts >/dev/null &&
      "$HOME/.local/bin/mise" exec -- npm install -g "@earendil-works/pi-coding-agent@$WK_PI_VERSION" >/dev/null &&
      printf '#!/bin/sh\nexec "$HOME/.local/bin/mise" exec -- pi "$@"\n' >"$HOME/.local/bin/pi" &&
      chmod +x "$HOME/.local/bin/pi" ||
      { echo "installing pi $WK_PI_VERSION failed" >&2; exit 1; }
  fi
  got=$("$HOME/.local/bin/pi" --version 2>/dev/null | head -1)
  [ "$got" = "$WK_PI_VERSION" ] || { echo "pi reports '$got', wanted $WK_PI_VERSION" >&2; exit 1; } ;; esac
for a in pi claude codex; do
  if command -v "$a" >/dev/null; then herdr integration install "$a" >/dev/null 2>&1 || true; fi
done

# ---- comebye at the hub's version: release tarball, SHA256SUMS verified,
# then `comebye upgrade` (installs, restarts the unit, smoke-checks).
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
asset="comebye_linux_${arch}.tar.gz"
base="$WK_RELEASES/download/$WK_VERSION"
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
# The asset must be listed and must match: sha256sum -c fed only its own line
# fails on a mismatch and cannot pass with nothing checked.
grep -E " \*?${asset}\$" "$tmp/SHA256SUMS" >"$tmp/line" || { echo "$asset is not in SHA256SUMS" >&2; exit 1; }
(cd "$tmp" && sha256sum -c line)
tar -xzf "$tmp/$asset" -C "$tmp"
dir="$tmp/comebye_linux_${arch}"
cp "$dir/comebye.service" "$HOME/.config/systemd/user/comebye.service"
mkdir -p "$HOME/.config/systemd/user/comebye.service.d"
cat >"$HOME/.config/systemd/user/comebye.service.d/herdr.conf" <<'UNIT'
[Unit]
After=herdr.service
Requires=herdr.service
UNIT

# The env file is the worker's join: COMEBYE_HUB + COMEBYE_ANNOUNCE make it
# register itself with the hub (the hub must allow the worker tag).
umask 077
cat >"$HOME/.config/comebye/env" <<ENV
COMEBYE_HOST_NAME=$WK_NAME
COMEBYE_TS_ALLOW_LOGIN=$WK_LOGIN
COMEBYE_PUSH=false
COMEBYE_HUB=$WK_HUB
COMEBYE_ANNOUNCE=1
COMEBYE_LAUNCH_ROOTS=/data/code
COMEBYE_WORKTREE_ROOT=/data/worktrees
ENV
umask 022

systemctl --user daemon-reload
systemctl --user enable --now herdr.service
"$dir/comebye" upgrade --version "$WK_VERSION"
# --now: if the upgrade above failed its smoke check on an earlier run (the hub
# had not seen this node announce yet) and this run finds the version already
# installed, nothing else would have started the unit.
systemctl --user enable --now comebye.service
installed=$("$HOME/.local/bin/comebye" -version 2>&1 | head -1)
case "$installed" in *"$WK_VERSION"*) ;; *) echo "comebye reports '$installed', wanted $WK_VERSION" >&2; exit 1 ;; esac
echo "bootstrap-user: comebye $WK_VERSION, herdr, agents ready"
