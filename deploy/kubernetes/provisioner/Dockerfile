# The provisioner image: comebye, comebye-worker (this repo, KubeVirt included),
# kubectl, virtctl and openssh-client. Build from the repository root:
#
#   podman build -f deploy/kubernetes/provisioner/Dockerfile \
#     --build-arg VERSION=$(git describe --tags --always) \
#     -t registry.example.com/comebye-provisioner:$(git rev-parse --short HEAD) .
#   podman push registry.example.com/comebye-provisioner:...   # your registry; none is assumed
#
# Then pin the pushed digest in deployment.yaml. No image is published from this
# repository.
#
# Everything downloaded is pinned to a version AND a sha256 and checked with
# `sha256sum -c`; a mismatch fails the build. To bump a tool: change the
# version, take the new sum from the vendor (kubectl: dl.k8s.io/release/VER/
# bin/linux/amd64/kubectl.sha256; virtctl: sha256sum of the release asset,
# which KubeVirt does not publish a sum for), and rebuild. Base images: pin the
# digest in your own registry mirror if you need byte-for-byte rebuilds.

ARG GO_VERSION=1.26.11
ARG ALPINE_VERSION=3.22

FROM golang:${GO_VERSION}-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# comebye-worker from THIS tree (main has the KubeVirt provider), not the
# separate -kv build.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/comebye . \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/comebye-worker ./tools/worker

FROM alpine:${ALPINE_VERSION} AS fetch
# linux/amd64 only. For arm64 add the matching sums.
ARG KUBECTL_VERSION=v1.34.2
ARG KUBECTL_SHA256=9591f3d75e1581f3f7392e6ad119aab2f28ae7d6c6e083dc5d22469667f27253
ARG VIRTCTL_VERSION=v1.8.4
ARG VIRTCTL_SHA256=7125d69c3b3e9c8185c106d9cdf77a817491e4657ac47f4509fb40d7aea59cca
RUN apk add --no-cache curl
RUN curl -fsSL -o /kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
 && echo "${KUBECTL_SHA256}  /kubectl" | sha256sum -c - \
 && curl -fsSL -o /virtctl "https://github.com/kubevirt/kubevirt/releases/download/${VIRTCTL_VERSION}/virtctl-${VIRTCTL_VERSION}-linux-amd64" \
 && echo "${VIRTCTL_SHA256}  /virtctl" | sha256sum -c - \
 && chmod 0755 /kubectl /virtctl

FROM alpine:${ALPINE_VERSION}
# openssh-client: the worker tool's start and bootstrap go over ssh through
# `virtctl port-forward --stdio`. Pinned by the base image's release (alpine
# keeps one openssh version per release).
RUN apk add --no-cache openssh-client ca-certificates \
 && adduser -D -u 1000 comebye
COPY --from=fetch /kubectl /virtctl /usr/local/bin/
COPY --from=build /out/comebye /out/comebye-worker /usr/local/bin/
USER 1000
ENTRYPOINT ["/usr/local/bin/comebye"]
