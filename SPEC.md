# anchord — Specification

This document defines **what anchord must do** to be considered complete,
independent of how it does it. Use it as the acceptance criteria for v1.0
and as the canonical source of truth when implementation and README drift.

## 1. Mental model (the non-negotiable invariant)

> **One Compose project = one classical server.**

Every observable property of the system must be consistent with the fiction
that the project is a single bare-metal host with one IPv4 + one IPv6,
running multiple services on different ports.

If a behavior would let an outside observer distinguish "this is actually
several containers", that's a bug.

## 2. Functional requirements

### 2.1 External addressing

- **F-1** Project obtains exactly one IPv4 from DHCP on the configured VLAN.
- **F-2** Project obtains zero or one IPv6 (SLAAC or DHCPv6, whichever the
  network provides). v6-less environments must not block v4 operation.
- **F-3** The MAC used for DHCP is stable across container recreations,
  derived deterministically from the project name unless overridden.
- **F-4** The DHCP hostname announced equals the project name unless
  overridden.
- **F-5** DHCP lease renewals must not interrupt established connections.
- **F-6** If DHCP fails at startup, anchord retries with exponential backoff
  capped at a configurable maximum (default 5 minutes).

### 2.2 Inbound traffic (DNAT)

- **F-7** A container in the same compose project, labelled with
  `anchord.expose: "<proto>/<port>[,…]"`, receives all inbound traffic to
  the project's external IP on those ports.
- **F-8** Multiple labelled containers may coexist; each (proto, port) tuple
  maps to exactly one container. Conflicts are reported as a startup error
  for the offending container, not silently resolved.
- **F-9** The original client source IP (v4 and v6) is preserved through
  DNAT — no MASQUERADE on inbound paths.
- **F-10** Both TCP and UDP are supported. v0.1 scope.
- **F-11** IPv6 exposure is automatic by default (mirrors v4 rules onto the
  AAAA address). Per-container opt-out via `anchord.expose.v6: off`.

### 2.3 Outbound traffic (egress)

- **F-12** All containers in the project egressing through anchord appear
  externally with the same source IP (v4 and, where applicable, v6).
- **F-13** Egress source IP automatically tracks the current DHCP lease —
  no reconfiguration needed on lease change.
- **F-13a** Egress works on `internal: true` Docker bridges (which carry
  no default route) without the operator pinning the network-anchor's
  transit IP or writing routing logic in compose `command:` strings.

### 2.4 Service discovery and reconciliation

- **F-14** Backends are discovered solely from Docker labels, not from
  config files or static IP tables.
- **F-15** Backend IP changes (container restart, recreation) are reflected
  in the NAT state within 5 seconds under normal load, without manual
  intervention.
- **F-16** A new container appearing with valid `anchord.expose` labels is
  reachable from outside within 5 seconds of becoming healthy.
- **F-17** A removed container is no longer reachable within 5 seconds; its
  former IP is flushed from conntrack so live connections re-evaluate.
- **F-18** Reconciliation is driven by Docker events (push) with a periodic
  polling fallback (default 30 seconds) to recover from missed events.
- **F-19** State changes are applied as atomic nftables map replacements;
  no observable window where DNAT is broken.

### 2.5 Operational

- **F-20** anchord exits cleanly on SIGTERM/SIGINT, removing its nftables
  tables and macvlan child interface.
- **F-21** All log output is structured JSON to stdout.
- **F-22** All configuration is via environment variables. No config files.
- **F-23** The network-anchor runs as a single container, requiring only
  `CAP_NET_ADMIN` and access to a Docker socket (read-only via socket-proxy
  is the documented default).

### 2.6 Service-anchor mode

The same `anchord` binary, invoked with `ANCHORD_MODE=service-anchor`
(or equivalently `command: [service-anchor]`), runs in service-anchor
mode. This mode is what makes egress and inbound-response paths work
on `internal: true` Docker bridges (see F-13a).

- **F-24** Service-anchor mode resolves the network-anchor's hostname
  (`anchord` by default; configurable via `ANCHORD_GATEWAY_HOSTNAME`)
  via the project's Docker DNS.
- **F-25** On startup, service-anchor mode installs a default route in
  its own network namespace via the resolved address — for v4, v6, or
  both, depending on which families resolve. If neither resolves at
  startup, it retries with backoff and does not exit.
- **F-26** Service-anchor mode re-resolves on a configurable cadence
  (default 5 s, `ANCHORD_GATEWAY_RESOLVE_INTERVAL`) and replaces its
  installed default route atomically (netlink `RouteReplace`) when the
  resolved address changes. Existing non-default routes in the namespace
  are not touched.
- **F-27** Service-anchor mode requires `CAP_NET_ADMIN` and read-only
  access to the Docker DNS resolver (i.e., normal Compose-network
  membership). It does **not** need access to the Docker socket.
- **F-28** Service-anchor mode keeps the namespace alive for the
  lifetime of the loop — it is the namespace owner that application
  containers join via `network_mode: service:<anchor>`.
- **F-29** On SIGTERM/SIGINT, service-anchor mode removes the default
  route(s) it installed and exits cleanly.

## 3. Non-functional requirements

- **N-1** The container image is ≤ 20 MB compressed.
- **N-2** Resource footprint at idle: ≤ 30 MB RSS, ≤ 1% of one CPU.
- **N-3** Reconciliation latency from Docker event to nftables map updated:
  ≤ 500 ms p99 under normal load.
- **N-4** No external runtime dependencies beyond the host kernel
  (≥ 4.18) and Docker daemon (≥ 20.10).

## 4. Explicit non-goals (v0.1 → v1.0)

- Layer-7 routing, TLS termination, hostname-based dispatch.
- Multi-host meshing or replication.
- Automatic certificate management.
- Web UI, dashboard, or interactive CLI.
- Automatic firewall hardening beyond what DNAT/MASQUERADE require.
- Support for non-DHCP external addressing (static, SLAAC-only, etc.).
  May come later, but not v0.1.
- iptables-legacy fallback. nftables only.
- Running outside a container (no "anchord on the host" mode).

## 5. Out-of-scope edge cases (documented, not handled)

- **Host ↔ project communication via the macvlan** — a Linux kernel
  limitation; document the macvlan-shim workaround, do not work around it.
- **Multiple anchord instances per project** — undefined behavior. Document
  as unsupported, do not detect or guard.
- **Two projects requesting the same DHCP hostname** — DHCP server's
  problem, not anchord's.

## 6. Acceptance scenarios

These are the integration tests that, when all green, signal v1.0.

### S-1: Cold start
1. Compose up a project with anchord + one labelled service-anchor.
2. Within 30s, the service-anchor is reachable from another VLAN host on
   the labelled port.
3. The project's external IP is visible in the DHCP server's lease table
   under the configured hostname.

### S-2: Source IP preservation
1. Connect to a labelled TCP port from a known source IP.
2. The service-anchor logs the connection.
3. The logged source IP equals the original client IP (not the
   transit-bridge gateway).

### S-3: Live container restart
1. Project is running and reachable.
2. `docker restart <service-anchor>`.
3. Within 5s of the container being healthy, external connections succeed
   again. Existing connections to the old IP are gracefully terminated
   (not held open in conntrack).

### S-4: DHCP lease rotation
1. Force the DHCP server to issue a new IP to the project.
2. Outbound traffic from the project carries the new source IP within
   one renewal cycle.
3. Inbound traffic to the new IP works without restart.

### S-5: Two projects, same host, same VLAN
1. Run two compose projects on the same host, both using anchord against
   the same VLAN parent.
2. Each gets a distinct DHCP lease.
3. Each is independently reachable. Neither's NAT state interferes with
   the other.

### S-6: Project teardown
1. `docker compose down`.
2. anchord's nftables tables are gone from the host.
3. The macvlan child interface is gone.
4. The DHCP lease is released.

### S-7: Multi-port single anchor
1. One service-anchor labelled `tcp/25,tcp/465,tcp/587`.
2. All three ports are independently reachable.
3. All three see correct client source IPs.

### S-8: IPv6 mirroring
1. Project has both v4 and v6 external addresses.
2. Service-anchor labelled `tcp/443` without v6 override.
3. Connections on v4:443 and v6:443 both succeed and reach the same
   container.

### S-9: Network-anchor recreate
1. Project is running, service-anchors reachable.
2. `docker compose up -d --force-recreate anchord` — Docker IPAM may
   hand the network-anchor a different transit IP.
3. Within `ANCHORD_GATEWAY_RESOLVE_INTERVAL` + 1 s, every
   service-anchor's default route is updated to the new transit IP.
4. Inbound and outbound paths work without restarting any
   service-anchor or application container.
