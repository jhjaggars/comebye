#!/bin/bash
# Run as root over verified ssh (stdin). Idempotent. The caller prepends
# `WK_USER=...` and `WK_IDLE_MINUTES=...` (validated by the provisioner).
set -euo pipefail
: "${WK_USER:?}" "${WK_IDLE_MINUTES:?}"
export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y -qq git jq bubblewrap socat curl ca-certificates gnupg \
  unzip build-essential >/dev/null

if ! command -v tailscale >/dev/null; then
  # shellcheck disable=SC1091
  . /etc/os-release
  repo="https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}"
  install -d -m 0755 /usr/share/keyrings
  curl -fsSL "$repo.noarmor.gpg" -o /usr/share/keyrings/tailscale-archive-keyring.gpg
  curl -fsSL "$repo.tailscale-keyring.list" -o /etc/apt/sources.list.d/tailscale.list
  apt-get update -qq
  apt-get install -y -qq tailscale >/dev/null
fi
systemctl enable --now tailscaled

loginctl enable-linger "$WK_USER"
install -d -o "$WK_USER" -g "$WK_USER" -m 0755 /data/code /data/worktrees 2>/dev/null ||
  { echo "/data is not mounted" >&2; exit 1; }

# Idle auto-poweroff: a guest poweroff stops a GCE instance, so no service
# account is needed. The check script is installed separately.
install -d -m 0755 /etc/comebye-worker
printf 'WK_USER=%s\nWK_IDLE_MINUTES=%s\n' "$WK_USER" "$WK_IDLE_MINUTES" >/etc/comebye-worker/idle.conf
cat >/etc/systemd/system/comebye-idle.service <<'UNIT'
[Unit]
Description=Power off when idle (no ssh session, no working herdr agent)

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/comebye-idle-check
UNIT
cat >/etc/systemd/system/comebye-idle.timer <<'UNIT'
[Unit]
Description=Check whether the worker is idle

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now comebye-idle.timer
echo "bootstrap-root: done"
