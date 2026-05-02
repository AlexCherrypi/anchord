#!/usr/bin/env bash
# anchord e2e test harness — bash port of the assertion logic.
#
# This script is meant to run INSIDE the anchord-e2e-runner container,
# which has docker-cli + docker-cli-compose + bash, and is launched
# with /var/run/docker.sock + the repo bind-mounted at /repo.
#
# Use test/e2e/up.sh (Linux/macOS/WSL) or test/e2e/up.ps1 (Windows)
# to launch the runner from the host.
#
# Usage (inside the runner):
#   bash test/e2e/run.sh              # all four scenarios
#   bash test/e2e/run.sh v4-only      # just one
#   WAIT_SECONDS=30 bash test/e2e/run.sh   # custom settle time
set -uo pipefail

repo_root="${REPO_ROOT:-/repo}"
e2e_dir="$repo_root/test/e2e"
wait_seconds="${WAIT_SECONDS:-15}"
no_teardown="${NO_TEARDOWN:-0}"

# Default to all four scenarios if no args given.
if [ "$#" -eq 0 ]; then
    set -- v4-only v6-only both none
fi

# ---- log helpers ---------------------------------------------------------

# Colour only if stderr is a TTY (compose-up output via container logs is
# usually not, but Docker Desktop's terminal preserves TTYness).
if [ -t 2 ]; then
    c_step=$'\e[36m'   # cyan
    c_pass=$'\e[32m'   # green
    c_fail=$'\e[31m'   # red
    c_info=$'\e[90m'   # grey
    c_warn=$'\e[33m'   # yellow
    c_off=$'\e[0m'
else
    c_step=''; c_pass=''; c_fail=''; c_info=''; c_warn=''; c_off=''
fi

step() { printf '%s[harness]%s %s\n' "$c_step" "$c_off" "$*" >&2; }
info() { printf '%s  ...   %s%s\n' "$c_info" "$*" "$c_off" >&2; }
pass() { printf '%s  PASS  %s%s\n' "$c_pass" "$*" "$c_off" >&2; }
fail() { printf '%s  FAIL  %s%s\n' "$c_fail" "$*" "$c_off" >&2; }
warn() { printf '%s%s%s\n' "$c_warn" "$*" "$c_off" >&2; }

# ---- assertion engine ----------------------------------------------------

# Per-scenario counters (reset before each scenario).
scenario_pass=0
scenario_fail=0

# check NAME OK [DETAIL]   — NAME is a label, OK is "1" for pass / "0" for fail.
check() {
    local name=$1 ok=$2 detail=${3:-}
    if [ "$ok" = "1" ]; then
        pass "$name"
        scenario_pass=$((scenario_pass + 1))
    else
        fail "$name"
        if [ -n "$detail" ]; then
            printf '%s        %s%s\n' "$c_info" "$detail" "$c_off" >&2
        fi
        scenario_fail=$((scenario_fail + 1))
    fi
}

# ax: docker exec into the anchord container, capture stdout+stderr,
# return both via the global REPLY_STDOUT and REPLY_RC variables.
ax() {
    local project=$1; shift
    REPLY_STDOUT=$(docker exec "${project}-anchord-1" "$@" 2>&1)
    REPLY_RC=$?
}

run_scenario() {
    local scenario=$1
    local project="anchord-e2e-$scenario"
    scenario_pass=0
    scenario_fail=0

    step "scenario: $scenario (project=$project)"
    export SCENARIO="$scenario"

    # Bring up. --build forces rebuild if image source changed (compose's
    # default is to skip rebuild when an image with the right tag already
    # exists, even after Dockerfile edits). --quiet-pull suppresses noise.
    if ! (cd "$e2e_dir" && docker compose -p "$project" up -d --build --quiet-pull >/dev/null 2>&1); then
        fail "compose up"
        return 1
    fi
    info "stack up — waiting ${wait_seconds}s for DHCP / RA / reconcile"
    sleep "$wait_seconds"

    # 1. anchord container alive?
    local state
    state=$(docker inspect -f '{{.State.Status}}' "${project}-anchord-1" 2>/dev/null || echo "missing")
    if [ "$state" = "running" ]; then
        check "anchord container running" 1
    else
        check "anchord container running" 0 "state='$state'"
        # No point continuing if anchord crashed.
        teardown "$project"
        return 1
    fi

    # 2. macvlan child interface exists.
    ax "$project" ip link show anchord-ext
    if [ "$REPLY_RC" -eq 0 ]; then
        check "anchord-ext interface present" 1
    else
        check "anchord-ext interface present" 0 "$REPLY_STDOUT"
    fi

    # 3. nftables tables installed (both families).
    ax "$project" nft list table ip anchord_v4
    check "nftables anchord_v4 table installed" "$([ "$REPLY_RC" -eq 0 ] && echo 1 || echo 0)" "$REPLY_STDOUT"
    ax "$project" nft list table ip6 anchord_v6
    check "nftables anchord_v6 table installed" "$([ "$REPLY_RC" -eq 0 ] && echo 1 || echo 0)" "$REPLY_STDOUT"

    # 4. Per-scenario address assertions on anchord-ext.
    ax "$project" ip -4 -o addr show anchord-ext
    local v4_out=$REPLY_STDOUT
    ax "$project" ip -6 -o addr show anchord-ext
    local v6_out=$REPLY_STDOUT

    local has_v4=0 has_v6=0
    [[ "$v4_out" =~ inet[[:space:]]+10\.99\.0\. ]] && has_v4=1
    [[ "$v6_out" =~ inet6[[:space:]]+fd99: ]]      && has_v6=1

    case "$scenario" in
        v4-only)
            check "anchord-ext has IPv4 from 10.99.0.0/24" "$has_v4" "$v4_out"
            check "anchord-ext has no fd99:: address"      "$([ $has_v6 -eq 0 ] && echo 1 || echo 0)" "$v6_out"
            ;;
        v6-only)
            check "anchord-ext has no IPv4 (10.99.0/24)"     "$([ $has_v4 -eq 0 ] && echo 1 || echo 0)" "$v4_out"
            check "anchord-ext has IPv6 from fd99::/64 (RA)" "$has_v6" "$v6_out"
            ;;
        both)
            check "anchord-ext has IPv4 from 10.99.0.0/24"   "$has_v4" "$v4_out"
            check "anchord-ext has IPv6 from fd99::/64 (RA)" "$has_v6" "$v6_out"
            ;;
        none)
            check "anchord-ext has no IPv4 lease (expected)" "$([ $has_v4 -eq 0 ] && echo 1 || echo 0)" "$v4_out"
            check "anchord-ext has no IPv6 (expected)"       "$([ $has_v6 -eq 0 ] && echo 1 || echo 0)" "$v6_out"
            ;;
    esac

    # 5. DNAT map populated for the smtp-anchor's exposed port (tcp/25).
    if [ "$scenario" = "v4-only" ] || [ "$scenario" = "both" ]; then
        ax "$project" nft list map ip anchord_v4 dnat_tcp
        local has_25=0
        [[ "$REPLY_STDOUT" =~ 25[[:space:]]*: ]] && has_25=1
        check "anchord_v4 dnat_tcp contains port 25" "$has_25" "$REPLY_STDOUT"
    fi
    if [ "$scenario" = "v6-only" ] || [ "$scenario" = "both" ]; then
        ax "$project" nft list map ip6 anchord_v6 dnat_tcp
        local has_25=0
        [[ "$REPLY_STDOUT" =~ 25[[:space:]]*: ]] && has_25=1
        check "anchord_v6 dnat_tcp contains port 25" "$has_25" "$REPLY_STDOUT"
    fi

    teardown "$project"
}

teardown() {
    local project=$1
    if [ "$no_teardown" = "1" ]; then
        info "leaving $project running (NO_TEARDOWN=1)"
        return
    fi
    info "tearing down $project"
    (cd "$e2e_dir" && docker compose -p "$project" down -v --remove-orphans >/dev/null 2>&1)
}

# ---- main ----------------------------------------------------------------

step "building anchord:dev"
docker build -q -t anchord:dev "$repo_root" >/dev/null

step "building anchord:test (anchord:dev + resolve-vlan shim)"
docker build -q -t anchord:test \
    --build-arg ANCHORD_BASE=anchord:dev \
    -f "$e2e_dir/images/anchord-test/Dockerfile" \
    "$e2e_dir" >/dev/null

cat >&2 <<'BANNER'

NOTE: When running inside Docker (especially Docker Desktop on Windows
or macOS), L2 broadcasts from a macvlan child onto a Docker bridge are
NOT forwarded reliably to peer veth endpoints. dhclient's DHCPDISCOVER
will leave anchord-ext but never reach the dnsmasq container's eth0,
so the "anchord-ext has IPv4 from 10.99.0.0/24" assertion fails here.
On a real Linux host with a physical VLAN parent (eth0.42 etc.) this
path works as intended. Treat that single FAIL as an environment
limitation, not an anchord bug — every other assertion verifies the
code path that anchord actually owns (boot, nftables setup, discovery,
DNAT map population).
BANNER

declare -A summary_pass
declare -A summary_fail
declare -A summary_skip
total_fail=0

for scenario in "$@"; do
    config="$e2e_dir/dhcp-configs/$scenario.conf"
    if [ ! -f "$config" ]; then
        warn "[harness] unknown scenario '$scenario' (no $config), skipping"
        summary_skip[$scenario]=1
        continue
    fi
    run_scenario "$scenario"
    summary_pass[$scenario]=$scenario_pass
    summary_fail[$scenario]=$scenario_fail
    total_fail=$((total_fail + scenario_fail))
done

# ---- summary -------------------------------------------------------------

printf '\n%s=== summary ===%s\n' "$c_step" "$c_off" >&2
for scenario in "$@"; do
    if [ "${summary_skip[$scenario]:-0}" = "1" ]; then
        printf '  %-12s %sskipped%s\n' "$scenario" "$c_warn" "$c_off" >&2
        continue
    fi
    p=${summary_pass[$scenario]:-0}
    f=${summary_fail[$scenario]:-0}
    if [ "$f" -gt 0 ]; then
        printf '  %-12s %s%d pass, %d fail%s\n' "$scenario" "$c_fail" "$p" "$f" "$c_off" >&2
    else
        printf '  %-12s %s%d pass%s\n' "$scenario" "$c_pass" "$p" "$c_off" >&2
    fi
done

[ "$total_fail" -gt 0 ] && exit 1
exit 0
