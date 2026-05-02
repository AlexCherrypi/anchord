# syntax=docker/dockerfile:1.6
#
# Test variant of anchord:dev with the resolve-vlan.sh entrypoint shim
# baked in. The shim picks the right macvlan parent interface by
# scanning addresses, working around Docker's nondeterministic eth0/eth1
# naming when a container is on multiple bridges.
#
# Build context: test/e2e/  (so we can COPY lib/resolve-vlan.sh).
ARG ANCHORD_BASE=anchord:dev
FROM ${ANCHORD_BASE}

COPY lib/resolve-vlan.sh /shim/resolve-vlan.sh
RUN chmod +x /shim/resolve-vlan.sh

ENTRYPOINT ["/shim/resolve-vlan.sh"]
