#!/bin/bash
# Run as root over verified ssh (stdin). Idempotent. The caller prepends
# `WK_USER=...` and `WK_IDLE_MINUTES=...` (validated by the provisioner).
set -euo pipefail
: "${WK_USER:?}" "${WK_IDLE_MINUTES:?}"
export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y -qq git jq bubblewrap socat curl ca-certificates gnupg \
  unzip build-essential dbus-user-session libpam-systemd >/dev/null

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
# A cloud image without dbus-user-session has no user bus until the user
# manager restarts with it installed (the user services below need the bus).
uid=$(id -u "$WK_USER")
if [ ! -S "/run/user/$uid/bus" ]; then
  systemctl restart "user@$uid.service"
  for _ in $(seq 1 30); do [ -S "/run/user/$uid/bus" ] && break; sleep 1; done
fi
# The data volume may still be attaching (EC2): give it a few minutes.
for _ in $(seq 1 180); do mountpoint -q /data && break; sleep 2; done
mountpoint -q /data || { echo "/data is not mounted" >&2; exit 1; }
install -d -o "$WK_USER" -g "$WK_USER" -m 0755 /data/code /data/worktrees

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

# Advisory max run time, where the cloud has no cap of its own (EC2). It is a
# timer on the VM, so root on the VM can defeat it: the idle poweroff above is
# the real backstop. GCE sets WK_MAX_RUN_SECONDS to nothing and uses the
# platform's max-run-duration instead.
if [ -n "${WK_MAX_RUN_SECONDS:-}" ]; then
  cat >/etc/systemd/system/comebye-maxrun.service <<'UNIT'
[Unit]
Description=Power off at the advisory maximum run time

[Service]
Type=oneshot
ExecStart=/usr/bin/systemctl poweroff
UNIT
  cat >/etc/systemd/system/comebye-maxrun.timer <<UNIT
[Unit]
Description=Advisory maximum run time (${WK_MAX_RUN_SECONDS}s after boot)

[Timer]
OnBootSec=${WK_MAX_RUN_SECONDS}s

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now comebye-maxrun.timer
else
  systemctl disable --now comebye-maxrun.timer 2>/dev/null || true
  rm -f /etc/systemd/system/comebye-maxrun.timer /etc/systemd/system/comebye-maxrun.service
fi
echo "bootstrap-root: done"
