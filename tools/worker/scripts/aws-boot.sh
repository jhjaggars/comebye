#!/bin/bash
# Installed by cloud-init user-data as /usr/local/sbin/comebye-boot and run
# on every boot by comebye-boot.service (root). User-data is readable via
# IMDS and the EC2 API, so nothing in it or here is a secret. This script:
#   1. prints the ssh host public key to the serial console with a marker;
#      the workstation reads it with the IAM-authenticated
#      `aws ec2 get-console-output`, so its first ssh connection is verified;
#   2. waits for the data volume, formats it only if blank, mounts /data.
set -u
pub=/etc/ssh/ssh_host_ed25519_key.pub
publish() {
  [ -s "$pub" ] && printf 'COMEBYE-HOSTKEY %s\n' "$(cut -d' ' -f1,2 "$pub")" >/dev/console
}
for _ in $(seq 1 120); do [ -s "$pub" ] && break; sleep 1; done
publish

vol=$(cat /etc/comebye-worker/volume-id)
dev="/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${vol//-/}"
for _ in $(seq 1 600); do [ -b "$dev" ] && break; sleep 1; done
if [ ! -b "$dev" ]; then
  echo "data volume $vol did not appear" >&2
  exit 1
fi
# blkid -p exits 2 only when it found no filesystem or partition signature.
# Anything else is left alone.
blkid -p "$dev" >/dev/null 2>&1
if [ $? -eq 2 ]; then
  mkfs.ext4 -q -L comebye-data "$dev"
fi
mkdir -p /data
grep -q '^LABEL=comebye-data ' /etc/fstab ||
  echo 'LABEL=comebye-data /data ext4 defaults,nofail 0 2' >>/etc/fstab
mountpoint -q /data || mount /data
# Print the key again: the first line can scroll out of a short console buffer
# on a slow volume.
publish
