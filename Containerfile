#
# Containerfile — provider image for cluster-api-hypervisor.
#
# Multi-stage build: the provider binary is compiled in a Go builder stage
# and packaged into a minimal runtime image together with the pinned tool
# binaries the provider shells out to (the install contract):
#   - cloud-hypervisor : per-VM subprocess management
#   - qemu-img         : root disk convert/resize
#   - mksquashfs       : confext data disk packaging (squashfs-tools)
#   - dnsmasq          : cluster DNS forwarder
#
# The runtime base is Alpine edge (pinned by digest) because cloud-hypervisor
# is packaged only in Alpine's edge/testing repository. Every tool is pinned to
# an exact Alpine package version below; the authoritative version list is
# maintained in docs/VERSIONS.md by a later phase. busybox ships both `sh` and
# `which`, so PATH lookups inside the container work with either tool.

FROM golang:1.26-alpine@sha256:1a9c10cf505a9e6b1e96ea77ebdbfe79a0f10380181faf88bc3b51d7e4315fae AS builder

ARG GOPROXY=https://proxy.golang.org
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# Cache module downloads before copying sources so dependency changes alone do
# not invalidate the layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The provider binary is static (no cgo) and runs on musl.
RUN CGO_ENABLED=0 go build -trimpath -o /out/cluster-api-hypervisor .

FROM alpine:edge@sha256:266f29255458134745f2bf588cb23ed1ed1768b96ff2580a05d70a8aba59e145 AS runtime

# Pinned tool versions (Alpine edge, x86_64). cloud-hypervisor is only
# available in the edge/testing repository; the remaining tools are in
# edge/main. Each pin is the exact package version shipped by Alpine today.
ARG CLOUD_HYPERVISOR_VERSION=48.0-r0
ARG QEMU_IMG_VERSION=11.0.3-r1
ARG SQUASHFS_TOOLS_VERSION=4.7.5-r0
ARG DNSMASQ_VERSION=2.93-r0

RUN apk add --no-cache \
        --repository https://dl-cdn.alpinelinux.org/alpine/edge/testing \
        cloud-hypervisor=${CLOUD_HYPERVISOR_VERSION} \
        qemu-img=${QEMU_IMG_VERSION} \
        squashfs-tools=${SQUASHFS_TOOLS_VERSION} \
        dnsmasq=${DNSMASQ_VERSION}

COPY --from=builder /out/cluster-api-hypervisor /usr/local/bin/cluster-api-hypervisor

ENTRYPOINT ["/usr/local/bin/cluster-api-hypervisor"]
