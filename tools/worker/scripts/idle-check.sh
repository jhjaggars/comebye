#!/bin/bash
# Installed as /usr/local/sbin/comebye-idle-check, run every 5 minutes by
# comebye-idle.timer. Powers the VM off after WK_IDLE_MINUTES with no human
# using a shell and no herdr agent in the "working" state.
#
# What counts as activity:
#  - an interactive ssh session: an sshd process holding a pty ("sshd: u@pts/N").
#    A bare ssh connection does NOT count: herdr keeps a ControlMaster/bridge
#    open (no pty) for as long as a machine is saved, which would keep the VM
#    up forever. Herdr exposes no "client attached" signal, so a TUI viewing
#    an idle agent is not activity either; typing makes the agent "working".
#  - a herdr agent whose agent_status is "working".
#  - herdr is up but did not answer: do not power off on a guess.
# Not activity: idle, done, unknown, and blocked (it waits on the user, who
# can start the VM again; keeping it up for ever would defeat the idle stop).
#
# WK_IDLE_DRY=1 prints the decision and changes nothing. One line per tick goes
# to the journal: journalctl -t comebye-idle
set -u
. "${WK_IDLE_CONF:-/etc/comebye-worker/idle.conf}"
stamp=${WK_IDLE_STAMP:-/run/comebye-last-active}   # /run is tmpfs: a fresh boot starts the clock
dry=${WK_IDLE_DRY:-}
[ -e "$stamp" ] || { [ -n "$dry" ] || touch "$stamp"; }

reason=""
ptys=$(ps -eo args= 2>/dev/null | grep -cE '^sshd: [^ ]+@pts/' || true)
if [ "${ptys:-0}" -gt 0 ]; then
  reason="$ptys interactive ssh session(s)"
fi
home=$(getent passwd "$WK_USER" | cut -d: -f6)
if out=$(runuser -u "$WK_USER" -- env HOME="$home" "$home/.local/bin/herdr" agent list 2>/dev/null); then
  n=$(grep -o '"agent_status":"working"' <<<"$out" | wc -l)
  if [ "$n" -gt 0 ]; then reason="${reason:+$reason, }$n herdr agent(s) working"; fi
elif systemctl --user -M "$WK_USER@" is-active --quiet herdr.service 2>/dev/null; then
  reason="${reason:+$reason, }herdr is up but did not answer"
fi

say() {
  if [ -n "$dry" ]; then echo "comebye-idle: $*"; else logger -t comebye-idle "$*"; fi
}
if [ -n "$reason" ]; then
  say "active: $reason"
  [ -n "$dry" ] || touch "$stamp"
  exit 0
fi
idle=$(( $(date +%s) - $(stat -c %Y "$stamp" 2>/dev/null || date +%s) ))
if [ "$idle" -ge $(( WK_IDLE_MINUTES * 60 )) ]; then
  say "idle for $((idle / 60)) min (limit $WK_IDLE_MINUTES): powering off"
  [ -n "$dry" ] || systemctl poweroff
else
  say "idle for $((idle / 60)) of $WK_IDLE_MINUTES min: no interactive ssh, no working agent"
fi
