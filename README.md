# anchord

> One IP per Compose project. No subnet bookkeeping. Real client source IPs.

`anchord` is a per-project network anchor for Docker Compose. It gives a Compose
project a single externally-routable IP (via macvlan + DHCP, with hostname
announcement) and dynamically maintains nftables DNAT rules pointing at
labelled service-anchor containers — without you ever hard-coding an IP
inside the project.

It exists because we wanted "one server, one service-pack" semantics back —
the way it used to be when Mailcow lived on its own physical box, Nextcloud
on another, and so on — but with the operational ergonomics of Compose.

## Status

**Pre-alpha.** Designed in a bathtub conversation. Use as a starting point, not
in production.

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

## Why not just `network_mode: macvlan` per service?

That gives every container its own MAC and IP. Our goal is the opposite:
several containers in one project sharing a single externally-visible IP,
while still getting full internal DNS-based service discovery. anchord
gives you exactly that — see [Architecture](#architecture).

## How it looks

```yaml
networks:
  transit:  { driver: bridge, internal: true }
  backend:  { driver: bridge, internal: true }

services:
  anchord:
    image: ghcr.io/alexcherrypi/anchord:latest
    cap_add: [NET_ADMIN]
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

## Caveats and known limitations

- **Kernel ≥ 4.18** required for atomic nftables map replaces.
- **CAP_NET_ADMIN** is required on every anchord container — the
  network-anchor for macvlan + nftables, every service-anchor for
  managing its own default route via netlink.
- **The service-anchor's DNS name must match `ANCHORD_GATEWAY_HOSTNAME`.**
  Default is `anchord`, which matches the canonical service name in the
  example compose. If you rename the network-anchor service, set
  `ANCHORD_GATEWAY_HOSTNAME` on each service-anchor to match.
- **Host ↔ project access** does not work over the macvlan from the docker
  host itself (Linux kernel quirk, not anchord's fault). If the host needs
  to reach the project, add a small macvlan shim on the host.
- **One network-anchor per Compose project** — the design assumes per-project
  scoping. Running multiple in the same project will race on nftables
  tables.
- **dhclient is shelled out** (not native Go DHCP). v0.1 trade-off; renewal
  hooks and lease persistence are battle-tested in dhclient.

## License

MIT — see [LICENSE](LICENSE).
