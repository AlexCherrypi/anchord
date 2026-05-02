# anchord

[![CI](https://github.com/AlexCherrypi/anchord/actions/workflows/ci.yml/badge.svg)](https://github.com/AlexCherrypi/anchord/actions/workflows/ci.yml)
[![Container](https://img.shields.io/badge/ghcr.io-anchord-blue?logo=docker)](https://github.com/AlexCherrypi/anchord/pkgs/container/anchord)
[![Go Version](https://img.shields.io/github/go-mod/go-version/AlexCherrypi/anchord)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> One IP per Compose project. No subnet bookkeeping. Real client source IPs.

**Built for self-hosted, homelab, and small-fleet workloads** that want
classical "one server, one service" semantics — Mailcow, Nextcloud,
Matrix/Synapse, Gitea, anything that historically ran on its own box.

`anchord` is a per-project network anchor for Docker Compose. It gives a Compose
project a single externally-routable IP (via macvlan + DHCP, with hostname
announcement) and dynamically maintains nftables DNAT rules pointing at
labelled service-anchor containers — without you ever hard-coding an IP
inside the project.

It exists because we wanted "one server, one service-pack" semantics back —
the way it used to be when Mailcow lived on its own physical box, Nextcloud
on another, and so on — but with the operational ergonomics of Compose.

## Status

**Pre-alpha, but it works.** Both modes implemented. 152/152 across unit
tests + e2e covering all four DHCP scenarios plus stateful DHCPv6 (the
auto-generated report at the bottom of this README is the release-readiness
signal). Outstanding before a v1 tag: real-host validation on a Linux box
with a physical VLAN sub-interface, plus SPEC decisions on the metrics
surface and the health-endpoint shape. Use as a starting point, not in
production.

*(Designed in a bathtub conversation. Has held up better than that has any
right to.)*

## The mental model

> **One Compose project = one classical server.**

That's the whole idea. Every service-anchor inside a project shares the same
externally-visible IPv4 and IPv6 — exactly as if `postfix`, `dovecot` and
friends were running side-by-side on a bare-metal host called `mailcow`. From
the outside there is no way to tell them apart; they're just ports on one
machine.

Concretely:

- **Inbound traffic** — clients connect to the project's single external IP.
  anchord's DNAT map routes each port to the right service-anchor inside.
  Postfix sees connections on 25/465/587, dovecot sees 143/993, both arriving
  on what they perceive as their own interface, with the original client
  source IP intact.
- **Outbound traffic** — every container in the project egresses with the
  same source IP, via masquerade. This matters for PTR records, SPF, IP
  reputation, audit trails — anything where "who was that?" needs a single
  consistent answer.
- **Internal addresses** — yes, the service-anchors do have separate
  Docker-bridge IPs on the transit network, because Docker has to route
  packets between them somehow. But that's an implementation detail. From
  the user's perspective, it doesn't exist.

### What this implies

**A given port can only point at one service-anchor.** Two containers that
both want to listen on 443 won't work — but that's also exactly what you'd
have on a real server. If you need multiple services on the same port (e.g.
multiple websites on 443), put a reverse proxy in front as a service-anchor
and let *it* handle the layer-7 multiplexing. anchord stops at layer 4.

This is intentional: anchord doesn't try to be a reverse proxy, an ingress
controller, or a service mesh. It gives you a server-shaped abstraction,
and you build the rest with whatever tools fit.

## How it compares

anchord lives in a niche the usual tools don't quite fill — it's not
a reverse proxy, not an ingress controller, not a service mesh. It's
a layer-4 NAT shim that gives a Compose project a server-shaped
network identity. Quick map:

| Approach | One IP per project? | Real source IPs preserved? | DHCP / hostname on the LAN? | Internal DNS service discovery? |
|---|:---:|:---:|:---:|:---:|
| `ports: "1.2.3.4:80:80"` | manual | no (bridge NAT mangles them) | no | yes |
| `network_mode: host` | shared with host | yes | host's only | no per-stack |
| `network_mode: macvlan` per service | no — one per container | yes | per container | broken (each container is its own L2 endpoint) |
| Traefik / Caddy / nginx in host mode | no | yes for HTTP(S) only | no | yes |
| Kubernetes ingress + LoadBalancer | yes (per Service) | depends on mode | not on bare LAN | yes |
| **anchord** | **yes** | **yes** | **yes** | **yes** |

It's specifically built for *"I want this Compose project to look like
a real server on my LAN"* — the problem nothing else solves cleanly.
anchord stops at layer 4 by design; if you need TLS termination,
hostname routing, or HTTP-aware load balancing, run a reverse proxy
*as* a service-anchor and let it own ports 80/443.

## How it looks

```yaml
networks:
  transit:  { driver: bridge, internal: true }
  backend:  { driver: bridge, internal: true }

services:
  anchord:
    image: ghcr.io/alexcherrypi/anchord:latest
    cap_add: [NET_ADMIN]
    sysctls:
      # Required: anchord forwards between macvlan and transit.
      net.ipv4.ip_forward: "1"
      net.ipv6.conf.all.forwarding: "1"
      # accept_ra=2 lets the kernel still take SLAAC from upstream RAs
      # while forwarding is enabled.
      net.ipv6.conf.all.accept_ra: "2"
      # arp_ignore=1 / arp_announce=2 keep anchord-ext's IP from being
      # ARP-claimed by the macvlan parent — without these, inbound TCP
      # can land on the parent interface and miss the DNAT rule.
      net.ipv4.conf.all.arp_ignore: "1"
      net.ipv4.conf.all.arp_announce: "2"
    environment:
      ANCHORD_PROJECT: ${COMPOSE_PROJECT_NAME}
      ANCHORD_VLAN_PARENT: eth0.42
      ANCHORD_DHCP_HOSTNAME: mailcow
      DOCKER_HOST: tcp://docker-proxy:2375
    networks: [transit]

  smtp-anchor:
    image: ghcr.io/alexcherrypi/anchord:latest
    cap_add: [NET_ADMIN]
    environment:
      ANCHORD_MODE: service-anchor
    networks: [transit, backend]
    labels:
      anchord.expose: "tcp/25,tcp/465,tcp/587"

  postfix:
    image: postfix:latest
    network_mode: "service:smtp-anchor"
```

That's it. No IPs, no subnets, no port-mappings on the host. anchord watches
the docker socket, finds containers in the same compose project that carry the
`anchord.expose` label, and wires up nftables DNAT entries pointing at their
current bridge-network IPs. When containers restart and get new IPs, the maps
update atomically and stale conntrack entries are flushed.

### One image, two modes

The `anchord` image plays two roles in a project:

- **Network-anchor** (`ANCHORD_MODE=network-anchor`, the default). One per
  project. Owns the macvlan child, runs the DHCP client, and maintains the
  nftables NAT state.
- **Service-anchor** (`ANCHORD_MODE=service-anchor`). One per exposed service.
  Resolves the network-anchor via Docker DNS, installs and maintains a default
  route via it, and serves as the namespace owner that real application
  containers join via `network_mode: service:<anchor>`.

Both roles run the same binary; the mode is just an env var. As an alternative
spelling, `command: [service-anchor]` does the same as setting `ANCHORD_MODE`.

## Architecture

```
                    VLAN (eth0.42)
                          │
            ┌─────────────┴────────────┐
            │   anchord container      │
            │                          │
            │   anchord-ext (macvlan)  │   ← DHCP-assigned IP, stable MAC
            │     │                    │
            │   nftables               │
            │   ├─ dnat_tcp { 25→…, 143→… }
            │   └─ masquerade on egress
            │     │                    │
            │   transit-bridge (Docker)│
            └─────────────┬────────────┘
                          │
        ┌─────────────────┼─────────────────┐
        │                                   │
   smtp-anchor                          imap-anchor
   (postfix joins via                   (dovecot joins via
    network_mode: service:smtp-anchor)   network_mode: service:imap-anchor)
        │                                   │
        └────── backend bridge (mysql, redis, …) ──┘
```

Three layers, by design:

1. **External** — macvlan child interface in the anchord container. DHCP
   client runs here. MAC is deterministic from the project name (so DHCP
   reservations are stable across container recreations).
2. **Transit** — internal Docker bridge connecting anchord to the
   service-anchors. `internal: true` ensures no Docker-managed MASQUERADE
   meddles with our paths.
3. **Backend** — internal Docker bridge for service-to-DB traffic. Most
   containers live here, never see the transit network.

### Why DNAT-by-map?

nftables named maps let us express the entire DNAT table as a single rule
that consults a key/value lookup:

```
iifname "anchord-ext" meta l4proto tcp dnat to tcp dport map @dnat_tcp
```

When a container restarts and its IP changes, we replace the map's contents
in one atomic transaction. No rule deletions, no microsecond windows where
packets fall through.

### Why masquerade outbound, not SNAT?

Masquerade automatically tracks the current source IP of the egress
interface — so when DHCP renews into a new lease, outbound traffic just
keeps working. SNAT to a literal IP would need re-pushing on every lease
change.

### Why no `ports:` mapping anywhere?

Because `ports:` invokes Docker's userland proxy and bridge-NAT, which both
mangle source IPs. anchord's whole point is to *not* go through that. Inbound
traffic enters the macvlan interface, hits anchord's DNAT in the kernel, and
arrives at the service-anchor with the original client IP intact.

## Configuration

All via environment variables.

### Common (both modes)

| Variable                     | Required | Default            | Notes |
|------------------------------|----------|--------------------|-------|
| `ANCHORD_MODE`               | no       | `network-anchor`   | `network-anchor` or `service-anchor`. `command: [service-anchor]` is an equivalent override. |
| `ANCHORD_LOG_LEVEL`          | no       | `info`             | `debug`/`info`/`warn`/`error` |

### Network-anchor mode

| Variable                     | Required | Default            | Notes |
|------------------------------|----------|--------------------|-------|
| `ANCHORD_PROJECT`            | yes      | `$COMPOSE_PROJECT_NAME` | Scope of containers anchord manages |
| `ANCHORD_VLAN_PARENT`        | yes      |                    | Host VLAN sub-interface, e.g. `eth0.42` |
| `ANCHORD_DHCP_HOSTNAME`      | no       | = project name     | Announced to DHCP server |
| `ANCHORD_EXT_MAC`            | no       | sha256(project)[:4] prefixed `02:42:` | Override only if you must |
| `ANCHORD_EXT_IFACE`          | no       | `anchord-ext`      | macvlan child interface name |
| `ANCHORD_POLL_INTERVAL`      | no       | `30s`              | Safety-net reconcile cadence |
| `ANCHORD_DHCP_BACKOFF_MAX`   | no       | `5m`               | Max backoff between dhclient retries |
| `DOCKER_HOST`                | no       | unix socket        | Set to `tcp://docker-proxy:2375` for socket-proxy mode |

### Service-anchor mode

| Variable                            | Required | Default   | Notes |
|-------------------------------------|----------|-----------|-------|
| `ANCHORD_GATEWAY_HOSTNAME`          | no       | `anchord` | Compose-network DNS name to look up for the network-anchor's transit IP |
| `ANCHORD_GATEWAY_RESOLVE_INTERVAL`  | no       | `5s`      | How often the service-anchor re-resolves and reconciles its default route |

## Container labels

On any container that should be exposed via the project's external IP:

| Label                | Example                       | Notes |
|----------------------|-------------------------------|-------|
| `anchord.expose`     | `"tcp/25,tcp/465,udp/4500"`   | Comma-separated `proto/port` entries |
| `anchord.expose.v6`  | `auto` (default) / `off`      | Whether to mirror v4 rules onto AAAA |

## Building

```sh
git clone https://github.com/AlexCherrypi/anchord
cd anchord
go mod tidy
go build ./cmd/anchord
docker build -t anchord:dev .
```

## Testing

The full test suite (Go unit tests + e2e harness across all four DHCP
scenarios) is invoked via `scripts/update-test-report.sh`, which runs
host-independently inside a Docker container and rewrites the
auto-generated **Test report** block at the bottom of this README on
green. See [TESTING.md](TESTING.md) for the per-platform commands and
the release-gate contract.

## Caveats and known limitations

- **Kernel ≥ 4.18** required for atomic nftables map replaces.
- **CAP_NET_ADMIN** is required on every anchord container — the
  network-anchor for macvlan + nftables, every service-anchor for
  managing its own default route via netlink.
- **The service-anchor's DNS name must match `ANCHORD_GATEWAY_HOSTNAME`.**
  Default is `anchord`, which matches the canonical service name in the
  example compose. If you rename the network-anchor service, set
  `ANCHORD_GATEWAY_HOSTNAME` on each service-anchor to match.
- **One network-anchor per Compose project** — the design assumes per-project
  scoping. Running multiple in the same project will race on nftables
  tables.
- **dhclient is shelled out** (not native Go DHCP). v0.1 trade-off; renewal
  hooks and lease persistence are battle-tested in dhclient.

## License

MIT — see [LICENSE](LICENSE).

<!-- TEST-REPORT-START -->
## Test report (auto-generated)

This block is rewritten by `scripts/update-test-report.sh` after a
green run of the full test suite — every test below was observed to
produce the listed status on the source tree whose hash is recorded
here. The release pipeline rejects any tag whose recorded hash does
not match the current source, so this block is the project's
release-readiness signal.

- **Last verified:** 2026-05-02T19:42:25Z
- **Code hash:** `sha256:36ed106cfd42bfc9f7d51a3b1ae4cda04fde67de84c692fc4716f9b2ea9730fe`
- **Flood-fix flag:** `E2E_BRIDGE_FLOOD_FIX=1`

### Summary

| Suite | Pass | Fail | Skip | Total |
|---|---:|---:|---:|---:|
| `go vet ./...` | clean | — | — | — |
| Go unit tests | 82 | 0 | 0 | 82 |
| E2E (test/e2e, 5 scenarios) | 70 | 0 | — | 70 |
| **All tests** | **152** | **0** | **0** | **152** |

<details>
<summary>Go unit tests &mdash; 82/82 passed</summary>

| Package | Test | Status |
|---|---|:---:|
| `cmd/anchord` | `TestSelectMode/ANCHORD_MODE=service-anchor` | ✓ |
| `cmd/anchord` | `TestSelectMode/explicit_network-anchor_subcommand` | ✓ |
| `cmd/anchord` | `TestSelectMode/flag-only_args_are_ignored` | ✓ |
| `cmd/anchord` | `TestSelectMode/no_args,_no_env_->_default_network-anchor` | ✓ |
| `cmd/anchord` | `TestSelectMode/subcommand_wins_over_env` | ✓ |
| `cmd/anchord` | `TestSelectMode/unknown_env_errors` | ✓ |
| `cmd/anchord` | `TestSelectMode/unknown_subcommand_errors` | ✓ |
| `internal/config` | `TestDeriveMAC` | ✓ |
| `internal/config` | `TestFingerprintDeterministic` | ✓ |
| `internal/config` | `TestGetenvDefault` | ✓ |
| `internal/config` | `TestLoadServiceAnchor_Defaults` | ✓ |
| `internal/config` | `TestLoadServiceAnchor_Overrides` | ✓ |
| `internal/config` | `TestLoadServiceAnchor_RejectsZeroInterval` | ✓ |
| `internal/config` | `TestLoad_ComposeProjectFallback` | ✓ |
| `internal/config` | `TestLoad_DefaultsAndDerivations` | ✓ |
| `internal/config` | `TestLoad_HostnameOverride` | ✓ |
| `internal/config` | `TestLoad_MACInvalid` | ✓ |
| `internal/config` | `TestLoad_MACOverride` | ✓ |
| `internal/config` | `TestLoad_PollIntervalOverride` | ✓ |
| `internal/config` | `TestLoad_ProjectOverridesCompose` | ✓ |
| `internal/config` | `TestLoad_RequiresProject` | ✓ |
| `internal/config` | `TestLoad_RequiresVLANParent` | ✓ |
| `internal/config` | `TestParseDuration/duration_string` | ✓ |
| `internal/config` | `TestParseDuration/empty_uses_default` | ✓ |
| `internal/config` | `TestParseDuration/invalid` | ✓ |
| `internal/config` | `TestParseDuration/plain_int_=_seconds` | ✓ |
| `internal/conntrack` | `TestFlushDestination_NilIPIsNoop` | ✓ |
| `internal/conntrack` | `TestFlushDestination_NonzeroExitIsSilent` | ✓ |
| `internal/conntrack` | `TestFlushDestination_V4Command` | ✓ |
| `internal/conntrack` | `TestFlushDestination_V6Command` | ✓ |
| `internal/dhcp` | `TestSleepBackoff_CapsAtMax` | ✓ |
| `internal/dhcp` | `TestSleepBackoff_DoublesBelowCap` | ✓ |
| `internal/dhcp` | `TestSleepBackoff_RespectsContextCancel` | ✓ |
| `internal/dhcp` | `TestWriteDhclientConf_Cleanup` | ✓ |
| `internal/dhcp` | `TestWriteDhclientConf_V4` | ✓ |
| `internal/dhcp` | `TestWriteDhclientConf_V6` | ✓ |
| `internal/discovery` | `TestBackendEqual/V6_mode_differs` | ✓ |
| `internal/discovery` | `TestBackendEqual/different_IPv4` | ✓ |
| `internal/discovery` | `TestBackendEqual/different_IPv6` | ✓ |
| `internal/discovery` | `TestBackendEqual/identical` | ✓ |
| `internal/discovery` | `TestBackendEqual/rules_differ` | ✓ |
| `internal/discovery` | `TestBackendEqual/rules_different_lengths` | ✓ |
| `internal/discovery` | `TestBackendEqual/rules_order_swapped` | ✓ |
| `internal/discovery` | `TestParseIP` | ✓ |
| `internal/discovery` | `TestPickIPs_NilNetworkSettings` | ✓ |
| `internal/discovery` | `TestPickIPs_NoSharedFallsBackToFirst` | ✓ |
| `internal/discovery` | `TestPickIPs_SharedNetworkAbsentReturnsNil` | ✓ |
| `internal/discovery` | `TestPickIPs_SharedNetworkExplicit` | ✓ |
| `internal/discovery` | `TestPickIPs_V4Only` | ✓ |
| `internal/discovery` | `TestPickIPs_V6Only` | ✓ |
| `internal/discovery` | `TestRuleLess` | ✓ |
| `internal/discovery` | `TestStateEqual` | ✓ |
| `internal/discovery` | `TestTrimName` | ✓ |
| `internal/labels` | `TestParse/absent` | ✓ |
| `internal/labels` | `TestParse/bad_port` | ✓ |
| `internal/labels` | `TestParse/bad_proto` | ✓ |
| `internal/labels` | `TestParse/empty_string_ignored` | ✓ |
| `internal/labels` | `TestParse/missing_port` | ✓ |
| `internal/labels` | `TestParse/mixed_protos_with_whitespace` | ✓ |
| `internal/labels` | `TestParse/port_zero` | ✓ |
| `internal/labels` | `TestParse/single_tcp` | ✓ |
| `internal/labels` | `TestParse/v6_off` | ✓ |
| `internal/nat` | `TestAddressFamily` | ✓ |
| `internal/nat` | `TestFamilyString` | ✓ |
| `internal/nat` | `TestIfaceBytes/empty` | ✓ |
| `internal/nat` | `TestIfaceBytes/short_name_padded` | ✓ |
| `internal/nat` | `TestIfaceBytes/typical_anchord-ext` | ✓ |
| `internal/nat` | `TestMapForFamProto` | ✓ |
| `internal/reconciler` | `TestDesiredFromState_DualStack` | ✓ |
| `internal/reconciler` | `TestDesiredFromState_Empty` | ✓ |
| `internal/reconciler` | `TestDesiredFromState_MultipleBackendsAndProtocols` | ✓ |
| `internal/reconciler` | `TestDesiredFromState_SamePortFromTwoBackends` | ✓ |
| `internal/reconciler` | `TestDesiredFromState_V4OnlyBackend` | ✓ |
| `internal/reconciler` | `TestDesiredFromState_V6Off` | ✓ |
| `internal/reconciler` | `TestDesiredFromState_V6OnlyBackend` | ✓ |
| `internal/serviceanchor` | `TestDefaultRouteFor_Validation` | ✓ |
| `internal/serviceanchor` | `TestReconcile_InstallsBothFamilies` | ✓ |
| `internal/serviceanchor` | `TestReconcile_KeepsLastGoodOnLookupError` | ✓ |
| `internal/serviceanchor` | `TestReconcile_NoOpWhenUnchanged` | ✓ |
| `internal/serviceanchor` | `TestReconcile_ReplacesOnIPChange` | ✓ |
| `internal/serviceanchor` | `TestReconcile_RetriesAfterFailedInstall` | ✓ |
| `internal/serviceanchor` | `TestRun_LoopsAndCleansUp` | ✓ |

</details>

<details>
<summary>E2E &mdash; 70/70 passed across 5 scenarios</summary>

| Scenario | Assertion | Status |
|---|---|:---:|
| `v4-only` | anchord container running | ✓ |
| `v4-only` | anchord-ext interface present | ✓ |
| `v4-only` | nftables anchord_v4 table installed | ✓ |
| `v4-only` | nftables anchord_v6 table installed | ✓ |
| `v4-only` | anchord-ext has IPv4 from 10.99.0.0/24 | ✓ |
| `v4-only` | anchord-ext has no fd99:: address | ✓ |
| `v4-only` | anchord_v4 dnat_tcp contains port 25 | ✓ |
| `v4-only` | S-2 (v4) source IP preserved through DNAT | ✓ |
| `v4-only` | S-3 dnat_tcp:25 reflects current transit IP within 8s | ✓ |
| `v4-only` | S-3 reachable on tcp/25 after recreate | ✓ |
| `v4-only` | S-6 anchord exited cleanly (code 0) | ✓ |
| `v4-only` | S-6 logs show graceful shutdown | ✓ |
| `v4-only` | S-6 logs show macvlan removed | ✓ |
| `v4-only` | S-6 nat teardown clean (no warnings) | ✓ |
| `v6-only` | anchord container running | ✓ |
| `v6-only` | anchord-ext interface present | ✓ |
| `v6-only` | nftables anchord_v4 table installed | ✓ |
| `v6-only` | nftables anchord_v6 table installed | ✓ |
| `v6-only` | anchord-ext has no IPv4 (10.99.0/24) | ✓ |
| `v6-only` | anchord-ext has IPv6 from fd99::/64 (RA) | ✓ |
| `v6-only` | anchord_v6 dnat_tcp contains port 25 | ✓ |
| `v6-only` | S-2 (v6) source IP preserved through DNAT | ✓ |
| `v6-only` | S-3 dnat_tcp:25 reflects current transit IP within 8s | ✓ |
| `v6-only` | S-3 reachable on tcp/25 after recreate | ✓ |
| `v6-only` | S-6 anchord exited cleanly (code 0) | ✓ |
| `v6-only` | S-6 logs show graceful shutdown | ✓ |
| `v6-only` | S-6 logs show macvlan removed | ✓ |
| `v6-only` | S-6 nat teardown clean (no warnings) | ✓ |
| `both` | anchord container running | ✓ |
| `both` | anchord-ext interface present | ✓ |
| `both` | nftables anchord_v4 table installed | ✓ |
| `both` | nftables anchord_v6 table installed | ✓ |
| `both` | anchord-ext has IPv4 from 10.99.0.0/24 | ✓ |
| `both` | anchord-ext has IPv6 from fd99::/64 (RA) | ✓ |
| `both` | anchord_v4 dnat_tcp contains port 25 | ✓ |
| `both` | anchord_v6 dnat_tcp contains port 25 | ✓ |
| `both` | S-2 (v4) source IP preserved through DNAT | ✓ |
| `both` | S-2 (v6) source IP preserved through DNAT | ✓ |
| `both` | S-3 dnat_tcp:25 reflects current transit IP within 8s | ✓ |
| `both` | S-3 reachable on tcp/25 after recreate | ✓ |
| `both` | S-6 anchord exited cleanly (code 0) | ✓ |
| `both` | S-6 logs show graceful shutdown | ✓ |
| `both` | S-6 logs show macvlan removed | ✓ |
| `both` | S-6 nat teardown clean (no warnings) | ✓ |
| `none` | anchord container running | ✓ |
| `none` | anchord-ext interface present | ✓ |
| `none` | nftables anchord_v4 table installed | ✓ |
| `none` | nftables anchord_v6 table installed | ✓ |
| `none` | anchord-ext has no IPv4 lease (expected) | ✓ |
| `none` | anchord-ext has no IPv6 (expected) | ✓ |
| `none` | S-6 anchord exited cleanly (code 0) | ✓ |
| `none` | S-6 logs show graceful shutdown | ✓ |
| `none` | S-6 logs show macvlan removed | ✓ |
| `none` | S-6 nat teardown clean (no warnings) | ✓ |
| `dhcpv6-stateful` | anchord container running | ✓ |
| `dhcpv6-stateful` | anchord-ext interface present | ✓ |
| `dhcpv6-stateful` | nftables anchord_v4 table installed | ✓ |
| `dhcpv6-stateful` | nftables anchord_v6 table installed | ✓ |
| `dhcpv6-stateful` | anchord-ext has IPv4 from 10.99.0.0/24 | ✓ |
| `dhcpv6-stateful` | anchord-ext has IPv6 from fd99::/64 (DHCPv6) | ✓ |
| `dhcpv6-stateful` | anchord_v4 dnat_tcp contains port 25 | ✓ |
| `dhcpv6-stateful` | anchord_v6 dnat_tcp contains port 25 | ✓ |
| `dhcpv6-stateful` | S-2 (v4) source IP preserved through DNAT | ✓ |
| `dhcpv6-stateful` | S-2 (v6) source IP preserved through DNAT | ✓ |
| `dhcpv6-stateful` | S-3 dnat_tcp:25 reflects current transit IP within 8s | ✓ |
| `dhcpv6-stateful` | S-3 reachable on tcp/25 after recreate | ✓ |
| `dhcpv6-stateful` | S-6 anchord exited cleanly (code 0) | ✓ |
| `dhcpv6-stateful` | S-6 logs show graceful shutdown | ✓ |
| `dhcpv6-stateful` | S-6 logs show macvlan removed | ✓ |
| `dhcpv6-stateful` | S-6 nat teardown clean (no warnings) | ✓ |

</details>
<!-- TEST-REPORT-END -->
