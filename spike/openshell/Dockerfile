# Spike: a comebye worker host as an OpenShell sandbox image (BYOC).
FROM docker.io/library/debian:12-slim
ARG COMEBYE_VERSION=v0.5.11
ARG TS_VERSION=1.92.5
ARG PI_VERSION=1.5.0
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git jq openssh-client socat procps iproute2 xz-utils bash tinyproxy \
 && rm -rf /var/lib/apt/lists/*
# Everything lives in /usr/local: on Kubernetes a PVC masks /sandbox.
RUN set -eux; arch=amd64; \
    curl -fsSL https://github.com/jhjaggars/comebye/releases/download/${COMEBYE_VERSION}/comebye_linux_${arch}.tar.gz | tar -xz -C /tmp; \
    install -m0755 /tmp/comebye_linux_${arch}/comebye /usr/local/bin/comebye; rm -rf /tmp/comebye_*; \
    curl -fsSL https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${arch}.tgz | tar -xz -C /tmp; \
    install -m0755 /tmp/tailscale_${TS_VERSION}_${arch}/tailscale /tmp/tailscale_${TS_VERSION}_${arch}/tailscaled /usr/local/bin/; rm -rf /tmp/tailscale_*; \
    HERDR_INSTALL_DIR=/usr/local/bin sh -c "$(curl -fsSL https://herdr.dev/install.sh)"; \
    curl -fsSL https://nodejs.org/dist/v22.20.0/node-v22.20.0-linux-x64.tar.xz | tar -xJ -C /usr/local --strip-components=1; \
    npm install -g @earendil-works/pi-coding-agent@${PI_VERSION}; \
    curl -fsSL https://claude.ai/install.sh | HOME=/tmp/cl bash; \
    cp -L /tmp/cl/.local/bin/claude /usr/local/bin/claude; rm -rf /tmp/cl
COPY comebye-sandbox-init /usr/local/bin/comebye-sandbox-init
RUN groupadd -g 1000 sandbox && useradd -m -u 1000 -g sandbox -d /sandbox -s /bin/bash sandbox
USER sandbox
WORKDIR /sandbox
