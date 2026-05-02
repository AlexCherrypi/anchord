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
bridge_flood_fix="${E2E_BRIDGE_FLOOD_FIX:-0}"

# Default to all five scenarios if no args given.
if [ "$#" -eq 0 ]; then
    set -- v4-only v6-only both none dhcpv6-stateful
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

    # Optional Docker Desktop workaround for the macvlan-on-bridge L2
    # broadcast quirk. See banner above and apply_bridge_flood_fix.
    if [ "$bridge_flood_fix" = "1" ]; then
        apply_bridge_flood_fix "$project"
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
        dhcpv6-stateful)
            check "anchord-ext has IPv4 from 10.99.0.0/24"      "$has_v4" "$v4_out"
            check "anchord-ext has IPv6 from fd99::/64 (DHCPv6)" "$has_v6" "$v6_out"
            ;;
        none)
            check "anchord-ext has no IPv4 lease (expected)" "$([ $has_v4 -eq 0 ] && echo 1 || echo 0)" "$v4_out"
            check "anchord-ext has no IPv6 (expected)"       "$([ $has_v6 -eq 0 ] && echo 1 || echo 0)" "$v6_out"
            ;;
    esac

    # 5. DNAT map populated for the smtp-anchor's exposed port (tcp/25).
    case "$scenario" in
        v4-only|both|dhcpv6-stateful)
            ax "$project" nft list map ip anchord_v4 dnat_tcp
            local has_25=0
            [[ "$REPLY_STDOUT" =~ 25[[:space:]]*: ]] && has_25=1
            check "anchord_v4 dnat_tcp contains port 25" "$has_25" "$REPLY_STDOUT"
            ;;
    esac
    case "$scenario" in
        v6-only|both|dhcpv6-stateful)
            ax "$project" nft list map ip6 anchord_v6 dnat_tcp
            local has_25=0
            [[ "$REPLY_STDOUT" =~ 25[[:space:]]*: ]] && has_25=1
            check "anchord_v6 dnat_tcp contains port 25" "$has_25" "$REPLY_STDOUT"
            ;;
    esac

    # ---- Phase 2: inbound dataplane (S-2, S-3) ---------------------------
    # Extract the project's external addresses (if any) and exercise the
    # full LAN-client → DNAT → service-anchor namespace path. Skipped when
    # no lease was obtained (e.g. scenario=none, or the documented Docker
    # Desktop macvlan-on-bridge limitation).
    local target_v4=$(extract_anchord_ext_v4 "$v4_out")
    local target_v6=$(extract_anchord_ext_v6 "$v6_out")
    phase2_inbound "$project" "$target_v4" "$target_v6"

    # ---- Phase 2: graceful teardown (S-6) --------------------------------
    phase2_teardown "$project"
}

# extract_anchord_ext_v4 reads the IPv4 (without /CIDR) from a single line
# of `ip -4 -o addr show anchord-ext`, or empty if none.
extract_anchord_ext_v4() {
    awk '$3=="inet" {print $4}' <<<"$1" | head -n1 | cut -d/ -f1
}

# extract_anchord_ext_v6 reads the first global IPv6 (skipping link-local
# fe80::) from `ip -6 -o addr show anchord-ext`.
extract_anchord_ext_v6() {
    awk '$3=="inet6" && $4 !~ /^fe80/ {print $4}' <<<"$1" | head -n1 | cut -d/ -f1
}

# phase2_inbound exercises S-2 and S-3 for whichever address families
# actually got a lease. Each family is tested independently so v4 can
# fail (Docker Desktop) without nullifying v6 results.
phase2_inbound() {
    local project=$1 v4=$2 v6=$3
    local probe="${project}-probe-1"
    local anchor="${project}-smtp-anchor-1"
    local listener="${project}-smtp-listener-1"

    if [ -z "$v4" ] && [ -z "$v6" ]; then
        info "phase 2: no external lease, skipping inbound assertions"
        return
    fi

    # Make sure probe + listener are actually up. compose up -d started
    # them; nothing to wait on beyond what we already slept above.
    if ! docker inspect "$probe" >/dev/null 2>&1; then
        check "phase 2 probe container running" 0 "container $probe missing"
        return
    fi
    if ! docker inspect "$listener" >/dev/null 2>&1; then
        check "phase 2 listener container running" 0 "container $listener missing"
        return
    fi

    if [ -n "$v4" ]; then
        phase2_s2 "$project" 4 "$v4"
    fi
    if [ -n "$v6" ]; then
        phase2_s2 "$project" 6 "$v6"
    fi

    # S-3 uses whichever family is available; v4 first (matches the
    # primary production code path).
    local s3_fam s3_target
    if [ -n "$v4" ]; then s3_fam=4; s3_target="$v4"
    elif [ -n "$v6" ]; then s3_fam=6; s3_target="$v6"
    fi
    phase2_s3 "$project" "$s3_fam" "$s3_target"
}

# phase2_s2 — SPEC scenario S-2: connect from probe to the project's
# external IP, listener echoes back its perceived peer address, must
# equal the probe's own VLAN-side address (no MASQUERADE on inbound).
phase2_s2() {
    local project=$1 fam=$2 target=$3
    local probe="${project}-probe-1"

    # Probe's own address on the vlan bridge.
    local probe_ip
    probe_ip=$(docker exec "$probe" ip -"$fam" -o addr show eth0 \
        | awk -v fam="$fam" '
            fam==4 && $3=="inet"  {print $4; exit}
            fam==6 && $3=="inet6" && $4 !~ /^fe80/ {print $4; exit}' \
        | cut -d/ -f1)
    if [ -z "$probe_ip" ]; then
        check "S-2 probe has v$fam address on vlan" 0
        return
    fi

    # Open a connection. ncat: -w 2 sets I/O timeout, -$fam forces the
    # family (avoids surprises if A/AAAA resolution ever sneaks in).
    # `</dev/null` on the outer shell becomes ncat's stdin via docker exec.
    local out
    out=$(docker exec -i "$probe" ncat -w 2 -"$fam" "$target" 25 \
        </dev/null 2>&1)

    # The listener uses TCP6-LISTEN with ipv6only=0, so v4 peers are
    # reported in v4-mapped-v6 hex form ("10.99.0.3" -> "0a63:0003"),
    # and v6 peers in fully-uncompressed form. Normalize both sides
    # before substring-matching so the test isn't fooled by formatting.
    local match_ip
    if [ "$fam" = "6" ]; then
        match_ip=$(expand_v6 "$probe_ip")
    else
        match_ip=$(v4_to_hex "$probe_ip")
    fi

    if printf '%s' "$out" | grep -Fq "$match_ip"; then
        check "S-2 (v$fam) source IP preserved through DNAT" 1
    else
        check "S-2 (v$fam) source IP preserved through DNAT" 0 \
            "expected '$match_ip' in '$out'"
    fi
}

# v4_to_hex turns "10.99.0.3" into "0a63:0003" — the v4-mapped-v6
# representation socat emits for v4 peers when the listener was opened
# via TCP6-LISTEN,ipv6only=0.
v4_to_hex() {
    local IFS=.
    set -- $1
    printf '%02x%02x:%02x%02x' "$1" "$2" "$3" "$4"
}

# expand_v6 turns a possibly-compact IPv6 address into the
# fully-uncompressed colon-separated form (eight 4-digit hex groups).
# Needed because socat's SOCAT_PEERADDR uses the uncompressed form
# while `ip -6 -o addr show` uses the compact one — naive substring
# matching across the two would always miss.
expand_v6() {
    awk -v a="$1" '
        function pad(s) { while (length(s) < 4) s = "0" s; return s }
        BEGIN {
            if (sub(/::/, ":#:", a)) {
                n = split(a, p, ":")
                cnt = 0
                for (i = 1; i <= n; i++) if (p[i] != "" && p[i] != "#") cnt++
                miss = 8 - cnt
                out = ""
                for (i = 1; i <= n; i++) {
                    if (p[i] == "#") { for (j = 0; j < miss; j++) out = out "0000:" }
                    else if (p[i] != "") { out = out pad(p[i]) ":" }
                }
                sub(/:$/, "", out)
                print out
            } else {
                n = split(a, p, ":")
                out = ""
                for (i = 1; i <= n; i++) out = out pad(p[i]) ":"
                sub(/:$/, "", out)
                print out
            }
        }'
}

# nft_map_lookup parses `nft list map …` stdout and returns the value
# associated with the given key, or empty if not present.
nft_map_lookup() {
    local key=$1
    # Split commas + braces onto their own lines so each "K : V" pair is
    # isolated, then match the key.
    awk -v k="$key" '
        {
            n = split($0, parts, /[,{}]/)
            for (i = 1; i <= n; i++) {
                if (match(parts[i], "^[[:space:]]*" k "[[:space:]]*:")) {
                    sub(/^[^:]*:[[:space:]]*/, "", parts[i])
                    gsub(/[[:space:]]/, "", parts[i])
                    print parts[i]
                    exit
                }
            }
        }
    '
}

# phase2_s3 — SPEC scenario S-3: force-recreate the service-anchor,
# wait for anchord to reconverge within F-15 (5s nominal, 8s with
# margin), assert the DNAT map points at the current container's
# transit IP and that the path is reachable.
#
# Note: we do NOT require Docker IPAM to assign a different IP after
# recreate — IPAM frequently hands the same address back when nothing
# else is holding it. The SPEC requirement is reachability + the map
# being current with the live container, regardless of whether the IP
# happened to change.
phase2_s3() {
    local project=$1 fam=$2 target=$3
    local anchor="${project}-smtp-anchor-1"
    local table=anchord_v4 fam_kw=ip
    if [ "$fam" = "6" ]; then table=anchord_v6; fam_kw=ip6; fi

    # Recreate. --no-deps avoids restarting anchord; smtp-listener is
    # listed because it shares smtp-anchor's namespace and is killed
    # by docker when its peer goes away.
    if ! (cd "$e2e_dir" && docker compose -p "$project" up -d \
            --force-recreate --no-deps \
            smtp-anchor smtp-listener >/dev/null 2>&1); then
        check "S-3 force-recreate smtp-anchor + listener" 0
        return
    fi

    # Read the new container's transit IP. inspect the running anchor —
    # the network name is the compose-derived "<project>_transit".
    local expected_ip=""
    local deadline_ip=$(( $(date +%s) + 5 ))
    while [ "$(date +%s)" -lt "$deadline_ip" ]; do
        expected_ip=$(docker exec "$anchor" ip -"$fam" -o addr show eth0 2>/dev/null \
            | awk -v fam="$fam" '
                fam==4 && $3=="inet"  {print $4; exit}
                fam==6 && $3=="inet6" && $4 !~ /^fe80/ {print $4; exit}' \
            | cut -d/ -f1)
        [ -n "$expected_ip" ] && break
        sleep 1
    done
    if [ -z "$expected_ip" ]; then
        check "S-3 smtp-anchor came back up with v$fam address" 0
        return
    fi

    # Wait up to ~8s for the reconciler to update the DNAT map to point
    # at the new container's IP.
    local deadline=$(( $(date +%s) + 8 ))
    local cur_ip=""
    while [ "$(date +%s)" -lt "$deadline" ]; do
        ax "$project" nft list map "$fam_kw" "$table" dnat_tcp
        cur_ip=$(printf '%s' "$REPLY_STDOUT" | nft_map_lookup 25)
        [ "$cur_ip" = "$expected_ip" ] && break
        sleep 1
    done

    if [ "$cur_ip" = "$expected_ip" ]; then
        check "S-3 dnat_tcp:25 reflects current transit IP within 8s" 1
    else
        check "S-3 dnat_tcp:25 reflects current transit IP within 8s" 0 \
            "expected=$expected_ip got=$cur_ip"
    fi

    # Reachability after reconverge.
    local probe="${project}-probe-1"
    local out
    out=$(docker exec -i "$probe" ncat -w 2 -"$fam" "$target" 25 \
        </dev/null 2>&1)
    if printf '%s' "$out" | grep -q "from="; then
        check "S-3 reachable on tcp/25 after recreate" 1
    else
        check "S-3 reachable on tcp/25 after recreate" 0 "got '$out'"
    fi
}

# phase2_teardown — SPEC scenario S-6: stop anchord cleanly, capture
# its exit code and final logs, then bring the stack down. Verifies
# that anchord exits 0 and that its shutdown path logged the macvlan
# removal (which is the observable side-effect of nat.Teardown +
# dhcp.removeLink running on SIGTERM).
#
# Note: SPEC S-6 also requires a DHCPRELEASE on shutdown. anchord's
# pure-Go DHCP client now sends one in its deferred cleanup before the
# IP is removed from the iface. We don't actively tcpdump for it here
# (would inflate harness complexity for low payoff), but the v4
# release is no longer a known gap.
phase2_teardown() {
    local project=$1
    local anchord="${project}-anchord-1"

    # Graceful stop with a generous timeout so the deferred Teardown +
    # removeLink log lines actually flush.
    if ! docker stop -t 10 "$anchord" >/dev/null 2>&1; then
        check "S-6 docker stop anchord (SIGTERM)" 0
        teardown "$project"
        return
    fi

    local exit_code
    exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$anchord" 2>/dev/null)
    if [ "$exit_code" = "0" ]; then
        check "S-6 anchord exited cleanly (code 0)" 1
    else
        check "S-6 anchord exited cleanly (code 0)" 0 "exit=$exit_code"
    fi

    local logs
    logs=$(docker logs "$anchord" 2>&1)
    if printf '%s' "$logs" | grep -q '"signal received'; then
        check "S-6 logs show graceful shutdown" 1
    else
        check "S-6 logs show graceful shutdown" 0
    fi
    if printf '%s' "$logs" | grep -q '"macvlan removed'; then
        check "S-6 logs show macvlan removed" 1
    else
        check "S-6 logs show macvlan removed" 0
    fi
    if printf '%s' "$logs" | grep -qi '"nat teardown'; then
        # nat.Teardown is logged ONLY on failure (slog.Warn), so seeing
        # the line means teardown raised. Absence == clean.
        check "S-6 nat teardown clean (no warnings)" 0 \
            "$(printf '%s' "$logs" | grep -i 'nat teardown' | head -n1)"
    else
        check "S-6 nat teardown clean (no warnings)" 1
    fi

    teardown "$project"
}

# apply_bridge_flood_fix is the Docker Desktop dev convenience.
#
# Empirically the v4-DHCPDISCOVER-blackholing on Docker Desktop is NOT
# a "macvlan rx_handler vs. bridge rx_handler" kernel quirk as one
# might first assume — it is `bridge-nf-call-iptables=1` (the WSL2 VM
# default) routing every Layer-2 bridge frame through the iptables
# FORWARD chain, where Docker's auto-generated DOCKER-FORWARD rules
# drop inter-bridge broadcasts. Confirmed by tcpdump: frames egress
# anchord-ext cleanly, the bridge sees them, but the FORWARD chain
# drops them before they reach peer veth ports. Setting
# bridge-nf-call-iptables=0 makes them flow.
#
# WARNING: This is dev-only. Setting bridge-nf-call-iptables=0 on a
# production host weakens Docker's inter-container filtering. Don't
# do this anywhere that isn't a throwaway dev VM. Production anchord
# deployments don't need any of this — there `lowerdev` is a physical
# VLAN sub-interface, frames don't traverse a Linux bridge at all.
apply_bridge_flood_fix() {
    # Host-wide; bridge-name not needed. We still keep the project arg
    # for a future per-bridge variant and for log clarity.
    local project=$1

    if docker run --rm --net=host --privileged alpine:3.21 sh -c "
        echo 0 > /proc/sys/net/bridge/bridge-nf-call-iptables  2>/dev/null
        echo 0 > /proc/sys/net/bridge/bridge-nf-call-ip6tables 2>/dev/null
    " >/dev/null 2>&1; then
        info "bridge-nf-call-iptables/ip6tables = 0 (host-wide)"
    else
        warn "[harness] bridge-nf-call disable helper failed"
    fi
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
NOT forwarded reliably to peer veth endpoints. anchord's DHCPDISCOVER
will leave anchord-ext but never reach the dnsmasq container's eth0,
so the "anchord-ext has IPv4 from 10.99.0.0/24" assertion fails here.

The Phase-2 inbound dataplane assertions (S-2, S-3) ride the same path
in reverse: probe -> bridge -> anchord-ext is also subject to the
broadcast/learning quirk. They are skipped automatically when no v4/v6
lease was obtained, so on Docker Desktop you'll see them run only when
SLAAC succeeded (v6-only / both scenarios).

On a real Linux host with a physical VLAN parent (eth0.42 etc.) this
path works as intended. Treat the v4-lease FAIL as an environment
limitation, not an anchord bug — every other assertion verifies the
code path that anchord actually owns.

Set E2E_BRIDGE_FLOOD_FIX=1 to apply a privileged bridge-flood
workaround to the docker network's vlan bridge after compose up. This
forces broadcast flooding to all bridge member ports, which bypasses
the macvlan-on-bridge quirk and lets v4 DHCP complete locally too.
The workaround is dev-convenience only — it is NOT applied in any
production setup, where the physical VLAN parent makes it unnecessary.
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
