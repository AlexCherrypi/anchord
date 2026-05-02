# anchord integration test runner.
#
# Runs go test -tags=integration against the kernel's real netlink/nft
# stack. Needs CAP_NET_ADMIN inside the container's own netns. The
# repo is bind-mounted at /repo by the launcher; this image only
# provides the toolchain.

FROM golang:1.25-alpine

RUN apk add --no-cache nftables iproute2 ca-certificates

# /go is the default GOPATH inside golang:* images. Mounting an
# external named volume there from the launcher lets the module cache
# survive between runs (avoids re-downloading deps every invocation).
WORKDIR /repo

ENTRYPOINT ["go", "test", "-tags=integration", "-count=1", "-v", "./internal/nat/..."]
