#!/bin/bash
# build-test.sh COMMIT [--push]: a NON-release image, registry.jaggars.cc/comebye-sandbox:test-<short>,
# = the current release image with comebye replaced by a binary built from COMMIT, and this
# directory's init. For testing unreleased comebye changes in a sandbox; never used for workers
# you keep. Needs go and podman.
set -euo pipefail
commit=$(git rev-parse --verify "$1^{commit}"); short=${commit:0:7}; push=${2:-}
base=${SBX_BASE_IMAGE:-registry.jaggars.cc/comebye-sandbox:v0.5.12}
img=registry.jaggars.cc/comebye-sandbox:test-$short
here=$(cd "$(dirname "$0")" && pwd); ctx=$(mktemp -d); trap 'rm -rf "$ctx"' EXIT
git archive "$commit" | tar -x -C "$ctx" --one-top-level=src
(cd "$ctx/src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=test-$short" -o "$ctx/comebye" .)
cp "$here/comebye-sandbox-init" "$ctx/"
cat >"$ctx/Dockerfile" <<D
FROM $base
USER root
COPY comebye /usr/local/bin/comebye
COPY comebye-sandbox-init /usr/local/bin/comebye-sandbox-init
RUN chmod 0755 /usr/local/bin/comebye /usr/local/bin/comebye-sandbox-init
LABEL org.opencontainers.image.revision="$commit"
USER sandbox
D
podman build -t "$img" "$ctx"
pat='(ghp_|gho_|github_pat_)[A-Za-z0-9_]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|tskey-[a-z]+-[A-Za-z0-9]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----'
hits=$(podman run --rm --entrypoint /bin/bash "$img" -c \
  "grep -rIlE '$pat' / --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev 2>/dev/null | grep -v '/etc/ssl/\|/usr/share/doc/\|@npmcli/config/\|/usr/local/lib/node_modules/.*\.\(d\.ts\|md\|html\|[0-9]\)$' || true")
[ -z "$hits" ] || { echo "secret scan FAILED:"; echo "$hits"; exit 1; }
echo "secret scan clean"; podman run --rm --entrypoint /bin/bash "$img" -c 'comebye -version | head -1'
if [ "$push" = --push ]; then podman push "$img"; skopeo inspect --format '{{.Digest}}' "docker://$img"; fi
