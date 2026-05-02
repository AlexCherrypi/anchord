# syntax=docker/dockerfile:1.6

# ---- build stage ------------------------------------------------------------
# --platform=$BUILDPLATFORM pins this stage to the build host's native
# architecture so we cross-compile (fast) instead of running the Go
# toolchain under QEMU emulation (slow). TARGETOS/TARGETARCH/TARGETVARIANT
# are populated by buildx from the --platform flag of `docker buildx build`.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src

# Cache-friendly layer for module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled → fully static binary, no glibc/musl dependency at
# runtime. GOARM is the ARM ISA level: arm/v6 → GOARM=6, arm/v7 →
# GOARM=7. The ${TARGETVARIANT#v} strip handles both.
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -trimpath \
    -o /out/anchord \
    ./cmd/anchord

# ---- runtime stage ----------------------------------------------------------
# DHCP is pure-Go (github.com/insomniacslk/dhcp), so dhclient is no
# longer a runtime dependency. We still need conntrack-tools (for
# flushing stale entries on backend IP changes) and nftables/iproute2
# for diagnostics, so the runtime is Alpine rather than distroless.
#
# alpine:3.21 was chosen for its multi-arch coverage: it ships for
# linux/{amd64, arm64, arm/v7, arm/v6, 386, ppc64le, s390x, riscv64}
# — the same set anchord publishes via buildx.
FROM alpine:3.21

RUN apk add --no-cache \
        conntrack-tools \
        iproute2 \
        nftables \
        ca-certificates \
    && adduser -D -u 65532 -s /sbin/nologin anchord

COPY --from=build /out/anchord /usr/local/bin/anchord

# OCI image annotations — surface metadata on ghcr.io and in
# `docker inspect`. The build workflow's metadata-action overrides
# the version/created/revision triplet at push time; everything else
# stays static here so locally-built images carry the same labels.
LABEL org.opencontainers.image.title="anchord" \
      org.opencontainers.image.description="Per-project network anchor for Docker Compose: one external IP per project (DHCP+macvlan), real client source IPs, nftables DNAT to labelled service-anchors." \
      org.opencontainers.image.source="https://github.com/AlexCherrypi/anchord" \
      org.opencontainers.image.url="https://github.com/AlexCherrypi/anchord" \
      org.opencontainers.image.documentation="https://github.com/AlexCherrypi/anchord#readme" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.authors="Alexander Kirsch" \
      org.opencontainers.image.vendor="AlexCherrypi"

# anchord needs CAP_NET_ADMIN for netlink and macvlan operations.
# It does NOT need to run as root — set the capabilities at compose
# level via cap_add.
USER root

ENTRYPOINT ["/usr/local/bin/anchord"]
