#!/usr/bin/env bash
# Host-side launcher (Linux/macOS/WSL/Git-Bash). Builds the e2e runner
# image and execs it against the host's Docker daemon. Forwards arguments
# to test/e2e/run.sh.
#
#   ./test/e2e/up.sh                 # all four scenarios
#   ./test/e2e/up.sh v4-only         # just one
#   NO_TEARDOWN=1 ./test/e2e/up.sh   # leave stack up after tests
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"

echo "[up.sh] building anchord-e2e-runner image"
docker build -q -t anchord-e2e-runner "$here/images/runner" >/dev/null

echo "[up.sh] starting runner"
exec docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$repo_root:/repo" \
    -e REPO_ROOT=/repo \
    -e WAIT_SECONDS="${WAIT_SECONDS:-15}" \
    -e NO_TEARDOWN="${NO_TEARDOWN:-0}" \
    anchord-e2e-runner \
    test/e2e/run.sh "$@"
