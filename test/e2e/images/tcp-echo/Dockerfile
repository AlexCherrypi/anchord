# syntax=docker/dockerfile:1.6
#
# Tiny TCP listener used by Phase-2 e2e assertions. Joins a service-anchor's
# network namespace via `network_mode: service:<anchor>`; for each inbound
# connection it writes back a single line:
#
#     from=<peer-addr>:<peer-port>
#
# That lets the probe verify SPEC S-2 (source IP preserved through DNAT)
# without parsing logs or running tcpdump in the runner.
#
# socat's TCP6-LISTEN with ipv6only=0 binds dual-stack, so both IPv4 and
# IPv6 inbound paths land in the same listener.
FROM alpine:3.19

RUN apk add --no-cache socat

# Exec form — no outer shell, so $SOCAT_PEERADDR is not expanded prematurely.
# socat itself spawns /bin/sh -c "..." per connection with SOCAT_PEERADDR
# set in that child's environment.
ENTRYPOINT ["socat", "-d", \
    "TCP6-LISTEN:25,fork,reuseaddr,ipv6only=0", \
    "SYSTEM:echo from=$SOCAT_PEERADDR"]
