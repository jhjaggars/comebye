# A comebye worker host as an OpenShell sandbox image (Kubernetes driver).
# Build: tools/worker/openshell/build.sh   (tag = the comebye release it contains)
FROM docker.io/library/debian:12-slim
# The image tag IS this version: the hub and its workers must run the same comebye.
ARG COMEBYE_VERSION=v0.5.12
ARG TS_VERSION=1.92.5
ARG PI_VERSION=1.5.0
ARG NODE_VERSION=v22.20.0
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git jq openssh-client socat procps iproute2 xz-utils bash tinyproxy \
 && rm -rf /var/lib/apt/lists/*
# Everything lives in /usr/local: on Kubernetes a PVC masks /sandbox, and the
# image root is read-only to the workload.
RUN set -eux; \
    case "$(uname -m)" in x86_64) arch=amd64; narch=x64 ;; aarch64) arch=arm64; narch=arm64 ;; *) exit 1 ;; esac; \
    base=https://github.com/jhjaggars/comebye/releases/download/${COMEBYE_VERSION}; \
    a=comebye_linux_${arch}.tar.gz; cd /tmp; curl -fsSL $base/$a -o $a; curl -fsSL $base/SHA256SUMS -o SUMS; \
    grep " \*\?$a\$" SUMS | sha256sum -c -; \
    tar -xzf $a; install -m0755 comebye_linux_${arch}/comebye /usr/local/bin/comebye; rm -rf comebye_linux_* SUMS; cd /; \
    curl -fsSL https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${arch}.tgz | tar -xz -C /tmp; \
    install -m0755 /tmp/tailscale_${TS_VERSION}_${arch}/tailscale /tmp/tailscale_${TS_VERSION}_${arch}/tailscaled /usr/local/bin/; rm -rf /tmp/tailscale_*; \
    HERDR_INSTALL_DIR=/usr/local/bin sh -c "$(curl -fsSL https://herdr.dev/install.sh)"; \
    curl -fsSL https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-${narch}.tar.xz | tar -xJ -C /usr/local --strip-components=1; \
    npm install -g @earendil-works/pi-coding-agent@${PI_VERSION}; \
    curl -fsSL https://claude.ai/install.sh | HOME=/tmp/cl bash; \
    cp -L /tmp/cl/.local/bin/claude /usr/local/bin/claude; rm -rf /tmp/cl; \
    comebye -version | head -1; tailscaled --version | head -1; herdr --version; pi --version
COPY comebye-sandbox-init /usr/local/bin/comebye-sandbox-init
RUN chmod 0755 /usr/local/bin/comebye-sandbox-init \
 && groupadd -g 1000 sandbox && useradd -m -u 1000 -g sandbox -d /sandbox -s /bin/bash sandbox
LABEL org.opencontainers.image.source="https://github.com/jhjaggars/comebye" org.opencontainers.image.version="${COMEBYE_VERSION}"
USER sandbox
WORKDIR /sandbox
