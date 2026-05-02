# syntax=docker/dockerfile:1.6

# ---- build stage ------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache-friendly layer for module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static build — anchord runs in a distroless image.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -trimpath \
    -o /out/anchord \
    ./cmd/anchord

# ---- runtime stage ----------------------------------------------------------
# We need dhclient + conntrack tools at runtime, so we can't go full
# distroless. Alpine + the two packages weighs ~12 MB.
FROM alpine:3.19

RUN apk add --no-cache \
        dhclient \
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
