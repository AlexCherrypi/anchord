# Architecture (for AI agents and quick onboarding)

This document exists because the relationship between anchord, the
network-anchor concept, the service-anchors, and the external DHCP
interface is the part of this project that's easiest to misread. Get
this right and everything else in the codebase makes sense. Get this
wrong and you'll write subtly broken code while believing it's correct.

Read this once, end-to-end, before touching anything.

## The problem anchord solves

Docker Compose makes it trivial to run a stack of containers that talk
to each other. It does **not** make it trivial to give that stack a
single, server-shaped network identity:

- One IP on the LAN, announced via DHCP, with a real hostname.
- Clients connecting from outside see real source IPs preserved.
- All containers in the stack egress with that same one IP.
- Internal service discovery between containers still works via DNS.
- Multiple such stacks can coexist on the same host without IP planning.

The standard ways of getting one of these properties break the others:

- `ports: "1.2.3.4:80:80"` requires the host to own the IP, mangles
  source IPs through bridge NAT, and gives no DHCP/hostname integration.
- `network_mode: macvlan` per container gives every container its own
  MAC and IP — explodes IP space and breaks the "one server" abstraction.
- A reverse proxy in host mode handles inbound but not egress identity,
  and doesn't give per-stack IPs at all.

anchord is the missing piece that gives all of these properties at once,
to a single Compose project, with effectively zero per-project config.

## The three roles

There are exactly three roles a container can play in an anchord-managed
project. Understanding which role a container is in is the single most
important thing when reading a compose file or a bug report.

### 1. The network-anchor (one per project)

This is the `anchord` container itself. It is the project's connection
to the outside world. Concretely:

- It owns a **macvlan child interface** (`anchord-ext` by default) on
  the host's VLAN parent. This is the project's externally-visible
  network presence — its "network card".
- It runs a **DHCP client** on that interface. The DHCP-assigned IP
  is the project's public IP. The hostname announced to DHCP is the
  project's identity on the LAN (typically equals the Compose project
  name).
- It owns the **nftables tables** that implement DNAT (inbound port
  routing) and masquerade (outbound source-IP rewriting).
- It watches the Docker socket for label changes and updates the NAT
  state accordingly.

There is exactly one network-anchor per Compose project. Running two
in the same project is undefined behavior.

### 2. Service-anchors (zero or more per project)

A service-anchor is a small placeholder container — usually
`alpine + sleep infinity` — that exists for one reason: to be a stable
**network namespace** that one or more "real" application containers
can share.

A service-anchor is identified by carrying the `anchord.expose` label.
Example:

```yaml
smtp-anchor:
  image: alpine
  command: [sleep, infinity]
  networks: [transit, backend]
  labels:
    anchord.expose: "tcp/25,tcp/465,tcp/587"
```

This declares: "any inbound traffic to the project's public IP on
ports 25, 465 or 587 should land in *this* container's network
namespace."

The actual application — postfix, in this example — does not get the
label. It joins the service-anchor's namespace via:

```yaml
postfix:
  image: postfix:latest
  network_mode: "service:smtp-anchor"
```

This is the Compose equivalent of a Kubernetes pod sidecar pattern:
multiple containers, one shared network stack.

**Why this two-step?** Because `network_mode: service:X` is mutually
exclusive with normal `networks:` membership. If postfix joined the
networks directly, it couldn't share a namespace with anything else.
By giving the namespace to a placeholder (the service-anchor) and
joining the placeholder, postfix gets the namespace AND we keep
flexibility — multiple containers can share that one namespace if a
service-pack needs it.

### 3. Plain backend containers (zero or more per project)

Anything that doesn't need to be reachable from outside. Databases,
caches, queue workers. They live on the project's `backend` network
and talk to service-anchors and to each other via Docker's built-in
DNS. They never get an `anchord.expose` label and anchord ignores them
entirely.

## How traffic flows

### Inbound (a client connects from the LAN to port 25)

```
LAN client ──tcp:25──▶ DHCP-assigned IP on VLAN
                              │
                              ▼
              [host's eth0.42, sees macvlan MAC]
                              │
                              ▼
              [anchord-ext interface in anchord container]
                              │
                              ▼
              [nftables prerouting, family ip]
                  iifname "anchord-ext" tcp dport map @dnat_tcp
                              │       lookup yields smtp-anchor's transit IP
                              ▼
              [anchord container's transit-bridge interface]
                              │
                              ▼ (kernel routes via transit bridge)
              [smtp-anchor's transit IP]
                              │
                              ▼ (postfix shares the namespace)
                          postfix sees:
                            - dst: its own IP on its own interface
                            - src: the LAN client's real IP, untouched
```

Critical property: **no MASQUERADE on this path**. Postfix sees the
real client source IP because we never rewrote it. This is what makes
spam scoring, audit logs, IP allowlists work correctly.

### Outbound (postfix sends mail to a remote server)

```
postfix ──▶ smtp-anchor's network stack
              │
              ▼
        default route via transit bridge
              │
              ▼
        anchord container, transit interface
              │
              ▼
        anchord container, IP forwarding
              │
              ▼
        nftables postrouting, family ip
            oifname "anchord-ext" masquerade
              │   ← rewrites source IP to whatever DHCP gave us
              ▼
        anchord-ext (macvlan)
              │
              ▼
        out into the VLAN with the project's public source IP
```

Critical property: **every container in the project egresses with the
same source IP**, automatically tracked through DHCP renewals via
`masquerade` (not literal SNAT). This is the project's outbound identity
— what an SPF check, a PTR lookup, or a remote service's "who is talking
to me" sees.

### Internal (postfix queries mysql)

```
postfix ──▶ DNS query "mysql" via Docker's internal resolver
              │
              ▼
        Docker returns mysql container's backend-network IP
              │
              ▼
        traffic stays entirely inside backend bridge
              │
              ▼
        mysql receives connection from smtp-anchor's backend IP
```

This path doesn't touch anchord at all. anchord doesn't know about
mysql, doesn't proxy to it, doesn't see the traffic. That's intentional:
internal traffic should be as fast and as boring as standard Docker
networking already is.

## How anchord keeps the NAT state correct

anchord is a control plane, not a data plane. It never touches packets;
it only updates the kernel's nftables state so that the kernel's
fast path does the right thing.

The control loop:

1. **Discovery** — anchord lists containers in its Compose project
   (filtered by `com.docker.compose.project=$ANCHORD_PROJECT`) carrying
   the `anchord.expose` label. It reads each one's IP on the shared
   transit network.
2. **Reconciliation** — for each `(family, proto, port)` in the labels,
   anchord computes the desired backend IP. It compares against the
   last-applied state.
3. **Map replacement** — changes are applied as atomic nftables map
   replaces. The DNAT rule itself is static; only the lookup table
   changes. There is no observable window where a port has no backend.
4. **Conntrack flush** — for any backend whose IP changed, anchord
   flushes conntrack entries pointing at the old IP, so existing
   connections don't keep being routed to a dead address.
5. **Triggering** — the loop is driven primarily by Docker events
   (sub-second latency). A polling fallback every 30 seconds catches
   anything missed.

What anchord does NOT do:

- It does not configure the service-anchors. They're plain Compose
  services; their `networks` and `labels` are written by the user.
- It does not configure the application containers. They use
  `network_mode: service:<anchor>` like in any pod-pattern setup.
- It does not touch routing tables, except its own egress masquerade.
- It does not parse TLS, hostnames, HTTP, or anything above layer 4.

## What the user writes vs. what anchord generates

User writes (compose file):

```yaml
services:
  anchord:
    image: ghcr.io/alexcherrypi/anchord:latest
    cap_add: [NET_ADMIN]
    environment:
      ANCHORD_PROJECT: ${COMPOSE_PROJECT_NAME}
      ANCHORD_VLAN_PARENT: eth0.42
      ANCHORD_DHCP_HOSTNAME: mailcow
    networks: [transit]

  smtp-anchor:
    image: alpine
    command: [sleep, infinity]
    networks: [transit, backend]
    labels:
      anchord.expose: "tcp/25,tcp/465,tcp/587"

  postfix:
    image: catatnight/postfix
    network_mode: "service:smtp-anchor"
```

anchord generates (at runtime, dynamically):

```
table ip anchord_v4 {
    map dnat_tcp {
        type inet_service : ipv4_addr
        elements = { 25 : 172.20.0.5, 465 : 172.20.0.5, 587 : 172.20.0.5 }
    }
    chain prerouting {
        type nat hook prerouting priority dstnat
        iifname "anchord-ext" meta l4proto tcp dnat to tcp dport map @dnat_tcp
    }
    chain postrouting {
        type nat hook postrouting priority srcnat
        oifname "anchord-ext" masquerade
    }
}
```

Plus an `anchord-ext` macvlan child on `eth0.42`, plus a `dhclient`
running on it, plus periodic conntrack flushes when the maps change.

The user never writes any IP, any nftables rule, any dhclient config.
They write a project name, a VLAN parent, and labels. Everything else
is derived.

## Why this is hard to get right (and easy to misread)

A few traps that catch reviewers, contributors, and AI agents:

1. **It looks like Kubernetes pods, but it isn't.** The
   `network_mode: service:` pattern resembles pod sharing, but Compose
   has no concept of a pod. The "pod" is implicit, defined by which
   containers point at which service-anchor.
2. **It looks like a reverse proxy, but it isn't.** anchord does not
   inspect any traffic. It only programs the kernel. If a contributor
   proposes "let anchord look at the Host header to route", that's
   architecturally out of scope.
3. **The macvlan is on the anchord container, not on the host.** The
   host has the VLAN parent (`eth0.42`). The macvlan child lives inside
   the anchord container's network namespace. This is why anchord
   needs `CAP_NET_ADMIN`.
4. **There are TWO bridge networks per project, and they're both
   `internal: true`.** Transit (anchor↔anchor) and backend (anchor↔db).
   Marking them internal disables Docker's default-gateway-based
   masquerade — which is what we want, because anchord owns the
   masquerade rule and Docker's would conflict.
5. **The service-anchor has the label, the application doesn't.** If
   you put `anchord.expose` on postfix instead of on smtp-anchor,
   anchord can find postfix but postfix has no IP on the transit
   network (because of `network_mode: service:`). You'll see "no usable
   IP for container" in the logs and nothing will work. The label has
   to go on the namespace owner.
6. **One service-anchor can host multiple apps.** If a service-pack has
   two processes that need to share an IP and a port space (rare but
   it happens), they can both `network_mode: service:` the same anchor.
   They share the namespace, so they share the IP, and they must not
   collide on ports.

## Quick mental check

If you're about to write code or review a PR, ask yourself:

- Does this preserve "one project = one server" externally?
- Does this keep inbound source IPs intact?
- Does this keep outbound source IP consistent across all containers?
- Does this update nftables atomically?
- Does this avoid making the user write any IP?

If any answer is "no" or "I don't know", stop and check against
SPEC.md and CONTEXT.md before continuing.
