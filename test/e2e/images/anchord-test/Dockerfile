# syntax=docker/dockerfile:1.6
#
# Test variant of anchord:dev with the resolve-vlan.sh entrypoint shim
# baked in. In v2 the shim picks the right external interface by
# scanning addresses and exports ANCHORD_EXT_IFACE, working around
# Docker's nondeterministic eth0/eth1 naming when the anchord container
# is on multiple bridges (vlan + transit).
#
# Build context: test/e2e/  (so we can COPY lib/resolve-vlan.sh).
ARG ANCHORD_BASE=anchord:dev
FROM ${ANCHORD_BASE}

COPY lib/resolve-vlan.sh /shim/resolve-vlan.sh
RUN chmod +x /shim/resolve-vlan.sh

ENTRYPOINT ["/shim/resolve-vlan.sh"]
