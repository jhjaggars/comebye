#!/bin/bash
# build.sh VERSION [--push]: build $SBX_REGISTRY/comebye-sandbox:VERSION from this
# directory, scan the image for secrets, and (with --push) push it. VERSION is the comebye
# release the image contains (e.g. v0.5.12); the hub and its workers must run the same one.
# SBX_REGISTRY (required) is your image registry, e.g. registry.example.com; SBX_IMAGE_REPO
# replaces the whole repository for a local build (it is never pushed).
set -euo pipefail
ver=${1:?usage: build.sh vX.Y.Z [--push]}; push=${2:-}
[[ $ver =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "VERSION must look like v0.5.12" >&2; exit 2; }
[ -n "${SBX_REGISTRY:-}" ] || { echo "SBX_REGISTRY is not set: set it to your image registry, e.g. SBX_REGISTRY=registry.example.com (the image is \$SBX_REGISTRY/comebye-sandbox:$ver)" >&2; exit 2; }
repo=$SBX_REGISTRY/comebye-sandbox
img=${SBX_IMAGE_REPO:-$repo}:$ver
cd "$(dirname "$0")"
podman build --build-arg COMEBYE_VERSION="$ver" -t "$img" .
# Secret scan: token shapes anywhere in the image filesystem (excluding /proc,/sys).
# The build takes no secrets, so any hit is a leak from a downloaded layer or a bug.
pat='(ghp_|gho_|github_pat_)[A-Za-z0-9_]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|tskey-[a-z]+-[A-Za-z0-9]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----'
hits=$(podman run --rm --entrypoint /bin/bash "$img" -c \
  "grep -rIlE '$pat' / --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev 2>/dev/null | grep -v '/etc/ssl/\|/usr/share/doc/\|@npmcli/config/\|/usr/local/lib/node_modules/.*\.\(d\.ts\|md\|html\|[0-9]\)$' || true")
if [ -n "$hits" ]; then echo "secret scan FAILED, files:"; echo "$hits"; exit 1; fi
echo "secret scan clean"
podman run --rm --entrypoint /bin/bash "$img" -c 'comebye -version | head -1; tailscaled --version | head -1; herdr --version; pi --version; claude --version; id'
if [ "$push" = --push ]; then
  case $img in "$repo":*) ;; *) echo "refusing to push $img (only $repo:VERSION is pushed)" >&2; exit 1 ;; esac
  podman push "$img"
  skopeo inspect --format '{{.Digest}}' "docker://$img"
fi
