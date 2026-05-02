#!/usr/bin/env bash
# anchord integration test launcher (Linux / macOS / WSL / Git-Bash).
# See run.ps1 for the Windows equivalent.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

echo "[runner] building anchord-integration image"
docker build -q -t anchord-integration -f "$script_dir/Dockerfile" "$script_dir" >/dev/null

echo "[runner] running integration tests"
exec docker run --rm \
    --cap-add=NET_ADMIN \
    -v "${repo_root}:/repo" \
    -v anchord-go-cache:/go \
    anchord-integration "$@"
