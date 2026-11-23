#!/bin/bash
# The test runner: what `go test ./internal/...` and tools/ci/browser.sh need,
# so a worker runs the heavy suites itself instead of the desktop host.
# Run as root over verified ssh (stdin), after bootstrap-root.sh. Idempotent:
# a tool already at its pinned version is left alone, a different one is
# replaced. The caller prepends the WK_* pins (versions and sha256 checksums,
# validated by the provisioner); every download is checked against them before
# anything is unpacked. Nothing here touches /data, the user's services or
# their sessions.
set -euo pipefail
: "${WK_GO_VERSION:?}" "${WK_GO_SHA256:?}" "${WK_NODE_VERSION:?}" "${WK_NODE_SHA256:?}"
: "${WK_CHROME_VERSION:?}" "${WK_CHROME_SHA256:?}"
export DEBIAN_FRONTEND=noninteractive

[ "$(uname -m)" = x86_64 ] || { echo "the test runner is pinned for x86_64 only (Chrome for Testing has no Linux arm64 build)" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# fetch URL SHA256 FILE: download, then refuse anything that does not match.
fetch() {
  curl -fsSL --retry 3 "$1" -o "$3"
  echo "$2  $3" | sha256sum -c --quiet - || { echo "checksum mismatch for $1" >&2; exit 1; }
}

# ---- system packages: Chrome's libraries (its deb.deps), fonts, and what
# what the tests call (python3, gpg, git, curl, sqlite3 for internal/archive; xz
# for Node's tarball).
# Debian 13 renamed some libraries for 64-bit time_t (libasound2 is
# libasound2t64 there; the old name is only a virtual package apt will not
# pick), so take the t64 name where the release has one.
pkgs="python3 gnupg sqlite3 git curl ca-certificates xz-utils unzip
  fonts-liberation fonts-dejavu-core fonts-noto-color-emoji
  libnss3 libnspr4 libdbus-1-3 libexpat1 libgbm1 libdrm2 libvulkan1 libudev1
  libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 libxshmfence1
  libpango-1.0-0 libcairo2 xdg-utils wget"
t64="libasound2 libcurl4 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcups2 libglib2.0-0 libgtk-3-0"
apt-get update -qq
for p in $t64; do
  if apt-cache show "${p}t64" >/dev/null 2>&1; then pkgs="$pkgs ${p}t64"; else pkgs="$pkgs $p"; fi
done
# shellcheck disable=SC2086
apt-get install -y -qq --no-install-recommends $pkgs >/dev/null

# ---- Go, at go.mod's toolchain version, in /usr/local/go
if [ "$(head -1 /usr/local/go/VERSION 2>/dev/null)" != "go$WK_GO_VERSION" ]; then
  fetch "https://go.dev/dl/go$WK_GO_VERSION.linux-amd64.tar.gz" "$WK_GO_SHA256" "$tmp/go.tgz"
  tar -xzf "$tmp/go.tgz" -C "$tmp"
  rm -rf /usr/local/go.old
  [ ! -e /usr/local/go ] || mv /usr/local/go /usr/local/go.old
  mv "$tmp/go" /usr/local/go
  rm -rf /usr/local/go.old
fi
ln -sfn /usr/local/go/bin/go /usr/local/bin/go
ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# ---- Node, at CI's version, in /usr/local/lib/node-vX
node_dir="/usr/local/lib/node-v$WK_NODE_VERSION-linux-x64"
if [ ! -x "$node_dir/bin/node" ]; then
  fetch "https://nodejs.org/dist/v$WK_NODE_VERSION/node-v$WK_NODE_VERSION-linux-x64.tar.xz" "$WK_NODE_SHA256" "$tmp/node.txz"
  tar -xJf "$tmp/node.txz" -C "$tmp"
  rm -rf "$node_dir"
  mv "$tmp/node-v$WK_NODE_VERSION-linux-x64" "$node_dir"
fi
for b in node npm npx; do ln -sfn "$node_dir/bin/$b" "/usr/local/bin/$b"; done
# Older pinned versions go once the new one is linked.
for d in /usr/local/lib/node-v*-linux-x64; do [ "$d" = "$node_dir" ] || rm -rf "$d"; done

# ---- Chrome for Testing (a fixed build at a fixed URL, unlike the stable
# channel's deb), as google-chrome on PATH: tools/ci/browser.sh looks for it there.
chrome_dir="/opt/chrome-for-testing/$WK_CHROME_VERSION"
if [ ! -x "$chrome_dir/chrome-linux64/chrome" ]; then
  fetch "https://storage.googleapis.com/chrome-for-testing-public/$WK_CHROME_VERSION/linux64/chrome-linux64.zip" "$WK_CHROME_SHA256" "$tmp/chrome.zip"
  unzip -q "$tmp/chrome.zip" -d "$tmp/chrome"
  rm -rf "$chrome_dir"
  install -d -m 0755 "$chrome_dir"
  mv "$tmp/chrome/chrome-linux64" "$chrome_dir/chrome-linux64"
fi
ln -sfn "$chrome_dir/chrome-linux64/chrome" /usr/local/bin/google-chrome
# /usr/bin too, where CI's Chrome is: tools/uitest/lib.mjs defaults to it, and the pages
# checks run `comebye publish` with PATH=/usr/bin:/bin. Never over a packaged Chrome.
if [ ! -e /usr/bin/google-chrome ] || [ "$(readlink /usr/bin/google-chrome)" = /usr/local/bin/google-chrome ]; then
  ln -sfn /usr/local/bin/google-chrome /usr/bin/google-chrome
fi
for d in /opt/chrome-for-testing/*; do [ "$d" = "$chrome_dir" ] || rm -rf "$d"; done

# ---- what was installed, for `worker testcheck` and a person reading the VM
install -d -m 0755 /etc/comebye-worker
cat >/etc/comebye-worker/test-runner <<EOF
go=$WK_GO_VERSION
node=$WK_NODE_VERSION
chrome=$WK_CHROME_VERSION
EOF

# Each tool must run, not merely exist (a missing library shows up here, not mid-suite).
/usr/local/bin/go version
/usr/local/bin/node --version
/usr/local/bin/npm --version >/dev/null
/usr/local/bin/google-chrome --headless=new --no-sandbox --disable-gpu --dump-dom about:blank >/dev/null 2>"$tmp/chrome.err" ||
  { cat "$tmp/chrome.err" >&2; echo "Chrome $WK_CHROME_VERSION does not start" >&2; exit 1; }
/usr/local/bin/google-chrome --version
echo "bootstrap-testrunner: done"
