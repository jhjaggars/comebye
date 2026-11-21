#!/bin/bash
# Installed as /usr/local/sbin/comebye-idle-check, run every 5 minutes by
# comebye-idle.timer. Powers the VM off after WK_IDLE_MINUTES with no human
# using a shell, no herdr agent working or waiting on the user, and nothing
# in flight (a hold, an upgrade). The same rules as `comebye idle`
# (internal/idle).
#
# What counts as activity:
#  - an interactive ssh session: an sshd process holding a pty ("sshd: u@pts/N").
#    A bare ssh connection does NOT count: herdr keeps a ControlMaster/bridge
#    open (no pty) for as long as a machine is saved, which would keep the VM
#    up forever. Herdr exposes no "client attached" signal, so a TUI viewing
#    an idle agent is not activity either; typing makes the agent "working".
#  - a herdr agent whose agent_status is "working".
#  - a herdr agent that is "blocked": it waits on the user (a question, a
#    permission), and a poweroff would throw its session away. Only for
#    WK_IDLE_BLOCKED_MAX_MINUTES (default 1440, a day; 0 = no limit) from
#    the first tick that saw an agent blocked, so a user who has gone away
#    does not keep the VM up for ever.
#  - a hold that has not expired: comebye on this VM sets one when a prompt
#    or launch arrives that no agent has picked up yet (a handoff in flight)
#    and when it starts an upgrade. Each is a file "<unix until> <reason>"
#    in WK_IDLE_HOLDS (default ~WK_USER/.local/state/comebye/idle-holds).
#  - an upgrade unit (comebye-upgrade-*) still running, however long it
#    takes: the VM never powers off mid-upgrade.
#  - herdr is up but did not answer: do not power off on a guess.
# Not activity: idle, done, unknown.
#
# WK_IDLE_DRY=1 prints the decision and changes nothing. One line per tick goes
# to the journal: journalctl -t comebye-idle
set -u
. "${WK_IDLE_CONF:-/etc/comebye-worker/idle.conf}"
stamp=${WK_IDLE_STAMP:-/run/comebye-last-active}   # /run is tmpfs: a fresh boot starts the clock
dry=${WK_IDLE_DRY:-}
[ -e "$stamp" ] || { [ -n "$dry" ] || touch "$stamp"; }
blocked_stamp="$stamp.blocked"   # since when some agent has been blocked
blocked_max=${WK_IDLE_BLOCKED_MAX_MINUTES:-1440}
now=$(date +%s)

reason=""
ptys=$(ps -eo args= 2>/dev/null | grep -cE '^sshd: [^ ]+@pts/' || true)
if [ "${ptys:-0}" -gt 0 ]; then
  reason="$ptys interactive ssh session(s)"
fi
home=$(getent passwd "$WK_USER" | cut -d: -f6)
note=""
if out=$(runuser -u "$WK_USER" -- env HOME="$home" "$home/.local/bin/herdr" agent list 2>/dev/null); then
  n=$(grep -o '"agent_status":"working"' <<<"$out" | wc -l)
  if [ "$n" -gt 0 ]; then reason="${reason:+$reason, }$n herdr agent(s) working"; fi
  b=$(grep -o '"agent_status":"blocked"' <<<"$out" | wc -l)
  if [ "$b" -gt 0 ]; then
    [ -e "$blocked_stamp" ] || { [ -n "$dry" ] || touch "$blocked_stamp"; }
    since=$(stat -c %Y "$blocked_stamp" 2>/dev/null || echo "$now")
    if [ "$blocked_max" -eq 0 ] || [ $(( now - since )) -lt $(( blocked_max * 60 )) ]; then
      reason="${reason:+$reason, }$b herdr agent(s) blocked, waiting on the user"
    else
      note=", $b agent(s) blocked for $(( (now - since) / 60 )) min (past the limit of $blocked_max)"
    fi
  else
    [ -n "$dry" ] || rm -f "$blocked_stamp"
  fi
elif systemctl --user -M "$WK_USER@" is-active --quiet herdr.service 2>/dev/null; then
  reason="${reason:+$reason, }herdr is up but did not answer"
fi

holds=${WK_IDLE_HOLDS:-$home/.local/state/comebye/idle-holds}
for f in "$holds"/*; do
  case ${f##*/} in *.tmp | \*) continue ;; esac
  [ -f "$f" ] || continue
  until="" why=""
  read -r until why <"$f" 2>/dev/null || true
  case $until in '' | *[!0-9]*) continue ;; esac
  if [ "$until" -gt "$now" ]; then
    reason="${reason:+$reason, }held for $(( (until - now + 59) / 60 )) min: ${why:-${f##*/}}"
  fi
done
# An upgrade run by the app is a transient user unit; while it is there the
# VM stays up, whatever its hold says.
if units=$(systemctl --user -M "$WK_USER@" list-units --plain --no-legend --state=active,activating,deactivating 'comebye-upgrade-*' 2>/dev/null) &&
  [ -n "$units" ]; then
  reason="${reason:+$reason, }an upgrade is running"
fi

say() {
  if [ -n "$dry" ]; then echo "comebye-idle: $*"; else logger -t comebye-idle "$*"; fi
}
if [ -n "$reason" ]; then
  say "active: $reason"
  [ -n "$dry" ] || touch "$stamp"
  exit 0
fi
idle=$(( now - $(stat -c %Y "$stamp" 2>/dev/null || echo "$now") ))
if [ "$idle" -ge $(( WK_IDLE_MINUTES * 60 )) ]; then
  say "idle for $((idle / 60)) min (limit $WK_IDLE_MINUTES)$note: powering off"
  [ -n "$dry" ] || systemctl poweroff
else
  say "idle for $((idle / 60)) of $WK_IDLE_MINUTES min: no interactive ssh, no working agent, no hold$note"
fi
