# syntax=docker/dockerfile:1.6
#
# Test runner image. Hosts the bash harness script and has the docker
# CLI + compose plugin so it can drive the host's Docker daemon via the
# bind-mounted /var/run/docker.sock. This is the "all tests run in
# Docker" pattern — the runner is identical on Linux/macOS/Windows.
FROM alpine:3.19

RUN apk add --no-cache \
        docker-cli \
        docker-cli-compose \
        bash \
        jq \
        ca-certificates

WORKDIR /repo

# Default to running the harness; can be overridden by `docker run ... <cmd>`.
ENTRYPOINT ["bash"]
CMD ["test/e2e/run.sh"]
