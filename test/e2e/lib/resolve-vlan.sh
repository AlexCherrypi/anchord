#!/bin/sh
# Test-only entrypoint shim. Resolves ANCHORD_VLAN_PARENT to whichever
# interface inside this container owns the test stack's VLAN subnet.
#
# Why: when an anchord container is attached to multiple Docker networks,
# the eth0/eth1 mapping is empirically nondeterministic between runs
# (compose's documented "order of networks: list" rule does not always
# hold). Hardcoding eth0 or eth1 in the harness causes the macvlan child
# to land on the wrong bridge half the time. This shim picks the right
# one by scanning addresses.
#
# Production anchord deploys do NOT use this shim — operators specify a
# concrete iface like eth0.42 and we honour it directly.
set -eu

v4_prefix="${VLAN_V4_PREFIX:-10.99.0.}"
v6_prefix="${VLAN_V6_PREFIX:-fd99:}"

# Try v4 first (Docker IPAM always assigns a v4 address to the bridge
# endpoint, even in v6-only DHCP scenarios). Fall back to v6.
iface=$(ip -4 -o addr show | awk -v s="$v4_prefix" '$4 ~ ("^" s) {print $2; exit}')
if [ -z "$iface" ]; then
    iface=$(ip -6 -o addr show | awk -v s="$v6_prefix" '$4 ~ ("^" s) {print $2; exit}')
fi

if [ -n "$iface" ]; then
    echo "[resolve-vlan] ANCHORD_VLAN_PARENT=${ANCHORD_VLAN_PARENT:-unset} -> $iface" >&2
    export ANCHORD_VLAN_PARENT="$iface"
else
    echo "[resolve-vlan] WARNING: no iface owns $v4_prefix* or $v6_prefix*; passing through" >&2
fi

exec /usr/local/bin/anchord "$@"
