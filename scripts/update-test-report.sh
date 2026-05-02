#!/usr/bin/env bash
# Run the full anchord test suite and, only if every step passes,
# rewrite the auto-generated TEST-REPORT block in README.md with the
# current code hash and a per-scenario summary.
#
# Host-independent: when invoked from outside Docker, the script
# re-execs itself inside a transient alpine + bash + git + docker-cli
# container so the only host requirement is Docker. Works the same
# on Linux, macOS, Windows (Docker Desktop), and CI.
#
# Pipeline contract: this script is the only legitimate way the
# README's TEST-REPORT block changes. The release-gate workflow
# refuses tags whose recorded hash does not match the current source.
#
# Env:
#   E2E_BRIDGE_FLOOD_FIX  Forwarded to the e2e harness. Default 1
#                         (Docker Desktop dev workaround). Set to 0
#                         on a real Linux host with a physical VLAN
#                         parent.
set -euo pipefail

# ---- self-exec into a Docker runner if not already there --------------------
#
# Outer pass: detect that we're on a host (no IN_DOCKER_RUNNER env),
# mount the repo + the docker socket into a fresh alpine, install the
# few tools we need, and re-exec ourselves there. HOST_REPO_PATH
# carries the host-side path forward so nested `docker run -v` calls
# (for go tests and the e2e harness) get the right mount source.
#
# Inner pass: IN_DOCKER_RUNNER=1 is set, we have apk-installed deps,
# and we proceed with the actual work.
if [ "${IN_DOCKER_RUNNER:-0}" != "1" ]; then
    here=$(pwd -P)
    cd "$(dirname "$0")/.."
    repo_host=$(pwd -P)
    cd "$here"

    # MSYS_NO_PATHCONV stops Git Bash on Windows from rewriting the
    # docker.sock path into "C:\Program Files\Git\var\...". On Linux
    # and macOS the variable is harmless.
    exec env MSYS_NO_PATHCONV=1 docker run --rm \
        -v "//var/run/docker.sock:/var/run/docker.sock" \
        -v "${repo_host}:/repo" \
        -w /repo \
        -e IN_DOCKER_RUNNER=1 \
        -e HOST_REPO_PATH="${repo_host}" \
        -e E2E_BRIDGE_FLOOD_FIX="${E2E_BRIDGE_FLOOD_FIX:-1}" \
        alpine:3.19 \
        sh -c '
            apk add -q --no-cache bash git docker-cli docker-cli-compose >/dev/null
            exec bash scripts/update-test-report.sh
        '
fi

# ---- inner pass: the actual work --------------------------------------------

cd /repo
host_repo="${HOST_REPO_PATH:?HOST_REPO_PATH not set in inner pass}"
flood_fix="${E2E_BRIDGE_FLOOD_FIX:-1}"

hash=$(bash scripts/code-hash.sh)
when=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "[test-report] code hash: $hash"

cleanup() { rm -f /tmp/go.log /tmp/e2e.log; }
trap cleanup EXIT

# ---- 1. go vet + go test ----------------------------------------------------

echo "[test-report] go vet + go test ..."
if ! docker run --rm \
        -v "${host_repo}:/src" -w /src \
        golang:1.25-alpine \
        sh -c 'go vet ./... && go test -count=1 ./...' \
        > /tmp/go.log 2>&1; then
    echo "[test-report] go test FAILED — not updating report" >&2
    sed 's/^/    /' /tmp/go.log >&2
    exit 1
fi
go_summary=$(grep -E '^(ok|FAIL|---|\?)\s' /tmp/go.log)

# ---- 2. e2e harness ---------------------------------------------------------

echo "[test-report] e2e harness (E2E_BRIDGE_FLOOD_FIX=$flood_fix) ..."

# Build the runner image and start it directly with the host repo
# path — we can't reuse test/e2e/up.sh because that script computes
# its own $(pwd) which is /repo here, not the host path.
docker build -q -t anchord-e2e-runner test/e2e/images/runner > /dev/null

if ! docker run --rm \
        -v "//var/run/docker.sock:/var/run/docker.sock" \
        -v "${host_repo}:/repo" \
        -e REPO_ROOT=/repo \
        -e WAIT_SECONDS="${WAIT_SECONDS:-25}" \
        -e NO_TEARDOWN=0 \
        -e E2E_BRIDGE_FLOOD_FIX="$flood_fix" \
        anchord-e2e-runner \
        test/e2e/run.sh \
        > /tmp/e2e.log 2>&1; then
    echo "[test-report] e2e harness FAILED — not updating report" >&2
    tail -80 /tmp/e2e.log | sed 's/^/    /' >&2
    exit 1
fi

# Strip ANSI colour codes the harness emits, then take the summary.
e2e_summary=$(awk '/=== summary ===/,0' /tmp/e2e.log \
              | sed -E 's/\x1b\[[0-9;]*m//g' \
              | sed '1d' \
              | grep -E '^[[:space:]]*(v4-only|v6-only|both|none)[[:space:]]' \
              | sed 's/^[[:space:]]*/  /')

# ---- 3. assemble the new report block ---------------------------------------

# We use a sentinel placeholder for the closing fence so the heredoc
# can sit inside this script's own ``` block without confusing
# editors/linters.
report_file=/tmp/report.block
cat > "$report_file" <<REPORT
<!-- TEST-REPORT-START -->
## Test report (auto-generated)

This block is rewritten by \`scripts/update-test-report.sh\` after a
green run of the full test suite. The release pipeline rejects any
tag whose recorded hash does not match the current source — so this
block is the project's release-readiness signal.

\`\`\`
Last verified: $when
Code hash:     $hash
Flood fix:     E2E_BRIDGE_FLOOD_FIX=$flood_fix

go vet + go test ./...
$go_summary

e2e harness (test/e2e, all four DHCP scenarios)
$e2e_summary
\`\`\`
<!-- TEST-REPORT-END -->
REPORT

# ---- 4. splice into README --------------------------------------------------

if grep -q '<!-- TEST-REPORT-START -->' README.md; then
    sed '/<!-- TEST-REPORT-START -->/,/<!-- TEST-REPORT-END -->/d' README.md \
        > /tmp/README.head
else
    cp README.md /tmp/README.head
fi

# Strip trailing blank lines from head, then append a separator and
# the new report block.
{
    awk 'NR==FNR && /\S/ { last = NR } NR == FNR { lines[NR] = $0; next }
         END { for (i = 1; i <= last; i++) print lines[i] }' \
        /tmp/README.head /tmp/README.head
    printf '\n'
    cat "$report_file"
} > README.md

rm -f /tmp/README.head "$report_file"
echo "[test-report] README.md updated."
