#!/bin/bash
# GCE startup script (runs as root on every boot). It does three things:
#   1. mounts the data disk at /data, formatting it only if it is blank;
#   2. publishes this VM's ssh host key to GCE guest attributes, which the
#      workstation reads through the authenticated GCE API, so its first
#      ssh connection is verified rather than trusted on first use;
#   3. says it is ready.
# It holds no secret and installs nothing.
set -u
md=http://metadata.google.internal/computeMetadata/v1
put_attr() {
  curl -fsS -X PUT -H 'Metadata-Flavor: Google' --data "$2" \
    "$md/instance/guest-attributes/comebye/$1" >/dev/null
}

dev=/dev/disk/by-id/google-comebye-data
if [ -b "$dev" ]; then
  # blkid -p exits 2 only when it found no filesystem or partition signature.
  # Anything else (a filesystem, a partition table, an error) is left alone.
  blkid -p "$dev" >/dev/null 2>&1
  if [ $? -eq 2 ]; then
    mkfs.ext4 -q -L comebye-data "$dev"
  fi
  mkdir -p /data
  grep -q '^LABEL=comebye-data ' /etc/fstab ||
    echo 'LABEL=comebye-data /data ext4 defaults,nofail 0 2' >>/etc/fstab
  mountpoint -q /data || mount /data
fi

pub=/etc/ssh/ssh_host_ed25519_key.pub
for _ in $(seq 1 60); do
  [ -s "$pub" ] && break
  sleep 1
done
[ -s "$pub" ] || { echo "no ssh host key" >&2; exit 1; }
put_attr ssh-host-key "$(cut -d' ' -f1,2 "$pub")"
put_attr ready "$(date +%s)"
