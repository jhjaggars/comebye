#!/bin/bash
# build.sh VERSION [--push]: build registry.jaggars.cc/comebye-sandbox:VERSION from this
# directory, scan the image for secrets, and (with --push) push it. VERSION is the comebye
# release the image contains (e.g. v0.5.12); the hub and its workers must run the same one.
set -euo pipefail
ver=${1:?usage: build.sh vX.Y.Z [--push]}; push=${2:-}
[[ $ver =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "VERSION must look like v0.5.12" >&2; exit 2; }
img=${SBX_IMAGE_REPO:-registry.jaggars.cc/comebye-sandbox}:$ver
cd "$(dirname "$0")"
podman build --build-arg COMEBYE_VERSION="$ver" -t "$img" .
# Secret scan: token shapes anywhere in the image filesystem (excluding /proc,/sys).
# The build takes no secrets, so any hit is a leak from a downloaded layer or a bug.
pat='(ghp_|gho_|github_pat_)[A-Za-z0-9_]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|tskey-[a-z]+-[A-Za-z0-9]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----'
hits=$(podman run --rm --entrypoint /bin/bash "$img" -c \
  "grep -rIlE '$pat' / --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev 2>/dev/null | grep -v '/etc/ssl/\|/usr/share/doc/' || true")
if [ -n "$hits" ]; then echo "secret scan FAILED, files:"; echo "$hits"; exit 1; fi
echo "secret scan clean"
podman run --rm --entrypoint /bin/bash "$img" -c 'comebye -version | head -1; tailscaled --version | head -1; herdr --version; pi --version; claude --version; id'
if [ "$push" = --push ]; then
  case $img in registry.jaggars.cc/comebye-sandbox:*) ;; *) echo "refusing to push $img" >&2; exit 1 ;; esac
  podman push "$img"
  skopeo inspect --format '{{.Digest}}' "docker://$img"
fi
