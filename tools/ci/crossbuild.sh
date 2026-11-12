#!/usr/bin/env bash
# Build the release archives: static binaries for 4 targets, the version stamped in, and SHA256SUMS.
#
#   tools/ci/crossbuild.sh VERSION OUT_DIR
#
# Archive names carry no version, so releases/latest/download/<name> is a stable URL. The release workflow
# publishes OUT_DIR; the nightly builds it to prove the release would build.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
version="${1:?version}"; dist="$(mkdir -p "${2:?out dir}" && cd "$2" && pwd)"
build="$(mktemp -d)"; trap 'rm -rf "$build"' EXIT
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*} arch=${target#*/}
  name="comebye_${os}_${arch}"
  mkdir -p "$build/$name"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "$build/$name/comebye" .
  cp LICENSE README.md deploy/comebye.service "$build/$name/"
  tar -C "$build" -czf "$dist/$name.tar.gz" "$name"
done
(cd "$dist" && sha256sum ./*.tar.gz | sed 's| \./| |' > SHA256SUMS)
cat "$dist/SHA256SUMS"
