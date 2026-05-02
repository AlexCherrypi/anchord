# syntax=docker/dockerfile:1.6
#
# Phase-2 probe: a host on the simulated VLAN bridge. The harness uses
# it via `docker exec` to:
#   - read its own v4/v6 address (so S-2 assertions know the expected
#     source IP)
#   - open a TCP connection to the project's external IP
#
# nmap-ncat is used in preference to busybox nc because busybox's -w
# behavior is inconsistent across alpine versions; ncat handles
# `-w 2` + `< /dev/null` predictably.
FROM alpine:3.21

RUN apk add --no-cache nmap-ncat iproute2

ENTRYPOINT ["sleep", "infinity"]
