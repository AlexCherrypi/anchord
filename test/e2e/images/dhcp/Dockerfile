# syntax=docker/dockerfile:1.6
#
# Tiny dnsmasq image used by the e2e harness as a per-scenario DHCP/RA
# server. All four scenario configs are baked in; the active one is
# selected at runtime via the SCENARIO env var. This avoids host-side
# bind-mounting, which would break when the harness itself runs inside
# a container (Docker-in-Docker path translation issues).
#
# Build context: test/e2e/  (so we can COPY dhcp-configs/ alongside).
FROM alpine:3.21

RUN apk add --no-cache dnsmasq tcpdump iproute2

COPY dhcp-configs/*.conf /etc/dnsmasq-configs/

# Pick the active config from SCENARIO at startup. Default v4-only.
# -k: foreground; --log-facility=-: stderr so `docker logs` works.
ENTRYPOINT ["sh", "-c", "exec dnsmasq -k --log-facility=- --conf-file=/etc/dnsmasq-configs/${SCENARIO:-v4-only}.conf"]
