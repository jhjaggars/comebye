#!/bin/bash
# Installed as /usr/local/sbin/comebye-idle-check, run every 5 minutes by
# comebye-idle.timer. Powers the VM off after WK_IDLE_MINUTES with no ssh
# session and no herdr agent in the "working" state.
set -u
. /etc/comebye-worker/idle.conf
stamp=/run/comebye-last-active   # /run is tmpfs: a fresh boot starts the clock
[ -e "$stamp" ] || touch "$stamp"

active=0
if ss -Htn state established '( sport = :22 )' | grep -q .; then
  active=1
fi
home=$(getent passwd "$WK_USER" | cut -d: -f6)
if out=$(runuser -u "$WK_USER" -- env HOME="$home" "$home/.local/bin/herdr" agent list 2>/dev/null); then
  if grep -q '"agent_status":"working"' <<<"$out"; then active=1; fi
elif systemctl --user -M "$WK_USER@" is-active --quiet herdr.service 2>/dev/null; then
  # herdr is up but did not answer: do not power off on a guess.
  active=1
fi

if [ "$active" = 1 ]; then
  touch "$stamp"
  exit 0
fi
idle=$(( $(date +%s) - $(stat -c %Y "$stamp") ))
if [ "$idle" -ge $(( WK_IDLE_MINUTES * 60 )) ]; then
  logger -t comebye-idle "idle for $((idle / 60)) min: powering off"
  systemctl poweroff
fi
