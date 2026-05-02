#!/usr/bin/env bash
# Run the full anchord test suite and, only if every step passes,
# rewrite the auto-generated TEST-REPORT block in README.md with the
# current code hash, a high-level summary table, and a per-test
# breakdown for both the Go unit suite and the e2e harness.
#
# Host-independent: when invoked from outside Docker, the script
# re-execs itself inside a transient alpine + bash + git + docker-cli
# + jq container so the only host requirement is Docker. Identical
# behaviour on Linux, macOS, Windows (Docker Desktop), and CI.
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
            apk add -q --no-cache bash git docker-cli docker-cli-compose jq >/dev/null
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

cleanup() {
    rm -f /tmp/go.json /tmp/go.log /tmp/e2e.log \
          /tmp/go-events.tsv /tmp/report.block /tmp/README.head
}
trap cleanup EXIT

# ---- 1. go vet + go test ----------------------------------------------------

echo "[test-report] go vet ..."
if ! docker run --rm \
        -v "${host_repo}:/src" -w /src \
        golang:1.25-alpine \
        go vet ./... \
        > /tmp/go.log 2>&1; then
    echo "[test-report] go vet FAILED — not updating report" >&2
    sed 's/^/    /' /tmp/go.log >&2
    exit 1
fi

echo "[test-report] go test ..."
# -json is JSON-per-line; keep stderr separate so vet/build noise
# (which go test still prints on success when there's nothing to log)
# can't poison the JSON parse below.
if ! docker run --rm \
        -v "${host_repo}:/src" -w /src \
        golang:1.25-alpine \
        go test -count=1 -json ./... \
        > /tmp/go.json 2> /tmp/go.log; then
    echo "[test-report] go test FAILED — not updating report" >&2
    sed 's/^/    /' /tmp/go.log >&2
    exit 1
fi

# Each line of /tmp/go.json is one test event. We only care about
# pass/fail/skip on actual tests (Test field set). Sort by package
# then test name so re-runs produce a stable README diff regardless
# of parallel-execution order.
jq -r '
    select(.Test != null and (.Action=="pass" or .Action=="fail" or .Action=="skip"))
    | [.Package, .Test, .Action] | @tsv
' /tmp/go.json | LC_ALL=C sort -t $'\t' -k1,1 -k2,2 > /tmp/go-events.tsv

# Identify "parent" tests — those that have at least one subtest
# event of the form "Parent/Subtest". We hide the parent row in the
# breakdown so each leaf test appears exactly once.
declare -A go_parent
while IFS=$'\t' read -r pkg test _action; do
    if [[ "$test" == */* ]]; then
        go_parent["${pkg}|${test%%/*}"]=1
    fi
done < /tmp/go-events.tsv

go_pass=0 go_fail=0 go_skip=0
go_rows=""
while IFS=$'\t' read -r pkg test action; do
    [ -n "${go_parent[${pkg}|${test}]:-}" ] && continue
    case "$action" in
        pass) go_pass=$((go_pass+1)); status="✓" ;;
        fail) go_fail=$((go_fail+1)); status="✗" ;;
        skip) go_skip=$((go_skip+1)); status="–" ;;
        *)    continue ;;
    esac
    pkg_short="${pkg#github.com/AlexCherrypi/anchord/}"
    # Pipes inside test names (rare, but Go subtests can contain
    # almost anything) need escaping for markdown table cells.
    test_escaped="${test//|/\\|}"
    go_rows+="| \`${pkg_short}\` | \`${test_escaped}\` | ${status} |"$'\n'
done < /tmp/go-events.tsv

# ---- 2. e2e harness ---------------------------------------------------------

echo "[test-report] e2e harness (E2E_BRIDGE_FLOOD_FIX=$flood_fix) ..."

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

# Strip ANSI colour codes; parse per-scenario PASS/FAIL lines.
e2e_pass=0 e2e_fail=0
e2e_scenarios=""
e2e_rows=""
e2e_scenario=""

# `[harness] scenario: <name> (project=...)`        — scenario header
# `  PASS  <label>` / `  FAIL  <label>`             — assertion result
# `        <detail>`                                — failure detail (skip)
while IFS= read -r line; do
    line=$(printf '%s' "$line" | sed -E 's/\x1b\[[0-9;]*m//g')
    if [[ "$line" =~ scenario:\ ([a-zA-Z0-9_-]+) ]]; then
        e2e_scenario="${BASH_REMATCH[1]}"
        e2e_scenarios+="${e2e_scenario} "
        continue
    fi
    if [[ "$line" =~ ^\ \ PASS\ \ +(.+)$ ]]; then
        name="${BASH_REMATCH[1]}"
        e2e_pass=$((e2e_pass+1))
        e2e_rows+="| \`${e2e_scenario}\` | ${name//|/\\|} | ✓ |"$'\n'
    elif [[ "$line" =~ ^\ \ FAIL\ \ +(.+)$ ]]; then
        name="${BASH_REMATCH[1]}"
        e2e_fail=$((e2e_fail+1))
        e2e_rows+="| \`${e2e_scenario}\` | ${name//|/\\|} | ✗ |"$'\n'
    fi
done < /tmp/e2e.log

e2e_scenario_count=$(printf '%s\n' $e2e_scenarios | grep -c .)
go_total=$((go_pass + go_fail + go_skip))
e2e_total=$((e2e_pass + e2e_fail))
all_pass=$((go_pass + e2e_pass))
all_fail=$((go_fail + e2e_fail))
all_total=$((go_total + e2e_total))

echo "[test-report] go tests:  ${go_pass}/${go_total} passed"
echo "[test-report] e2e tests: ${e2e_pass}/${e2e_total} passed across ${e2e_scenario_count} scenarios"

# ---- 3. assemble the new report block ---------------------------------------

cat > /tmp/report.block <<REPORT
<!-- TEST-REPORT-START -->
## Test report (auto-generated)

This block is rewritten by \`scripts/update-test-report.sh\` after a
green run of the full test suite — every test below was observed to
produce the listed status on the source tree whose hash is recorded
here. The release pipeline rejects any tag whose recorded hash does
not match the current source, so this block is the project's
release-readiness signal.

- **Last verified:** ${when}
- **Code hash:** \`${hash}\`
- **Flood-fix flag:** \`E2E_BRIDGE_FLOOD_FIX=${flood_fix}\`

### Summary

| Suite | Pass | Fail | Skip | Total |
|---|---:|---:|---:|---:|
| \`go vet ./...\` | clean | — | — | — |
| Go unit tests | ${go_pass} | ${go_fail} | ${go_skip} | ${go_total} |
| E2E (test/e2e, ${e2e_scenario_count} scenarios) | ${e2e_pass} | ${e2e_fail} | — | ${e2e_total} |
| **All tests** | **${all_pass}** | **${all_fail}** | **${go_skip}** | **${all_total}** |

<details>
<summary>Go unit tests &mdash; ${go_pass}/${go_total} passed</summary>

| Package | Test | Status |
|---|---|:---:|
${go_rows}
</details>

<details>
<summary>E2E &mdash; ${e2e_pass}/${e2e_total} passed across ${e2e_scenario_count} scenarios</summary>

| Scenario | Assertion | Status |
|---|---|:---:|
${e2e_rows}
</details>
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
    cat /tmp/report.block
} > README.md

echo "[test-report] README.md updated."
