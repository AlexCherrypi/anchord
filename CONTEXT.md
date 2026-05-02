# Design context

This document exists so that future contributors (including the original
author six months from now, and any LLM agent picking up the work) can
understand **why** anchord is shaped the way it is, not just what it does.
The SPEC.md tells you what's required; this tells you why those particular
requirements and not others.

## Origin

anchord came from a single design conversation that walked through, in
order:

1. Bind ports to specific host IPs in compose (`192.168.1.10:8080:80`) —
   trivially possible, but the host needs to actually own those IPs.
2. Tell DHCP to assign IPs in the VLAN per project, not per container.
3. Why per-project, not per-container? Because compose projects are units
   of deployment. Mailcow is "one logical server" with five containers,
   not five servers.
4. macvlan per-container was rejected — mac/IP per container leaks the
   internal structure to the network and burns IP space.
5. ipvlan L2 with shared host-MAC was rejected — defeats DHCP-by-MAC.
6. Anchor pattern (one container holds the macvlan, others
   `network_mode: service:anchor`) was the breakthrough — single external
   identity, full internal docker DNS still works.
7. But: `network_mode: service:` is exclusive of other networks. So we
   can't naively put backends in the same anchor namespace and still have
   them on a backend bridge.
8. Solution: split anchors. **Network-anchor** holds the external macvlan
   and runs the NAT logic. **Service-anchors** join two bridges (transit +
   backend) and adopt their app containers via `network_mode: service:`.
9. Hardcoding IPs across many projects is a maintenance trap. Use Docker
   DNS where possible, generate NAT rules dynamically from container
   labels and live-resolved IPs.
10. Watcher loop = events + polling, atomic map updates, conntrack flush
    on backend IP change.

The full conversation is preserved in `notes/` (if you got it as a
markdown export). Read it if a decision in the code seems weird — it
probably came from a specific tradeoff discussed there.

## Design principles

### 1. Server-shaped, not cluster-shaped

The user's mental model is a Linux server, not a Kubernetes cluster.
That dictates:
- One IP per project, not one IP per service.
- Source IPs preserved (a real server doesn't MASQUERADE its own
  inbound traffic).
- Egress identity is the project, not the container (mail reputation
  must be consistent).
- "Multiple things on port 443" is solved by reverse proxies, not by
  ingress controllers.

When a feature would only make sense in a cluster context (label
selectors across hosts, virtual services, multi-cluster routing),
it doesn't belong in anchord.

### 2. Configuration scarcity

Everything that can be derived must be derived. The user's hard floor
of input is:
- Project name (Compose gives this for free)
- VLAN parent interface (one variable)
- DHCP hostname (defaults to project name)

Everything else — MAC, internal IPs, NAT rules, conntrack handling —
is computed. This is non-negotiable: each additional knob is a way for
the user to hold it wrong.

### 3. Layer 4 hard stop

anchord is a NAT engine plus a DHCP client plus a label watcher. It is
**not** a proxy, not a load balancer, not a TLS terminator, not a service
mesh. Layer 7 problems get solved with Layer 7 tools (Traefik, Caddy,
nginx) running *as* service-anchors.

If someone proposes "could anchord also do X", and X requires inspecting
or modifying packet payloads, the answer is no.

### 4. Atomic state changes only

Inbound NAT rules are critical path. We never touch them in a way that
creates a window where packets fall through. nftables named maps allow
single-transaction replaces — that's what makes this design viable.

If a future feature can't be expressed as an atomic map replacement,
that feature needs a different mechanism, not a non-atomic shortcut.

### 5. Robust defaults, knobs as escape hatches

The default behavior must work for the 80% case with zero tuning.
Every environment variable beyond the two required ones is an escape
hatch for unusual environments, not a "you should configure this"
suggestion.

## Why these specific technologies

| Choice | Why | What was rejected |
|---|---|---|
| Go | Single static binary, native netlink, atomic concurrency | Bash (state diff gets ugly past 300 lines); Python (deps) |
| nftables | Atomic map updates, modern kernel API | iptables (no atomic replace, deprecated path) |
| `google/nftables` | Speaks netlink directly | Shelling out to `nft` (subprocess overhead, parser fragility) |
| dhclient (subprocess) | Battle-tested renewal/hooks | Pure-Go DHCP libs (less proven in weird DHCP servers) |
| macvlan, not ipvlan | DHCP-by-MAC works | ipvlan L2 (shared MAC defeats DHCP reservation) |
| Docker socket proxy | Read-only access to events/containers | Mounting the raw socket (root-equivalent) |
| Compose-label-driven | No central config to drift | Static config files |
| Docker DNS for gateway discovery | Service-anchors find the network-anchor's transit IP via the Compose-network resolver — survives recreates with no operator config | Static `ipv4_address`/`ipv6_address` pinning (operator burden); IPv6 RA emission (asymmetric — solves v6 but not v4); anchord exec-into-service-anchors via Docker socket (expands attack surface, breaks the read-only socket invariant) |
| Service-anchor mode in same binary | Single image, code share for netlink/slog/signal handling, narrative consistency ("one project = one server, anchord is its networking shim in two flavors") | Separate `anchord-anchor` binary (two images to maintain, no real benefit); user-written `command: ["sh","-c","ip route add ..."]` (fragile, can't react to network-anchor IP change without yet another supervisor) |

## Anti-patterns to reject in PRs

These have been considered and explicitly rejected. If a PR reintroduces
one, it needs a strong new argument, not just a restatement of the old
one.

- **A config file format** — no, see "Configuration scarcity".
- **A control plane / API server** — no, the Docker socket *is* the API.
- **Layer-7 awareness of any kind** — no, see "Layer 4 hard stop".
- **iptables fallback** — no, kernel ≥ 4.18 is a fine floor in 2026.
- **Static-IP mode for the external interface** — maybe later, but only
  if it doesn't add knobs to the DHCP path.
- **Multiple anchords per project** — no, scope conflict.
- **A web UI** — no, logs and metrics are sufficient.
- **The network-anchor reaching into service-anchor namespaces to fix
  routing** — no. The Docker socket is read-only on purpose. The
  service-anchor mode loop running *inside* the service-anchor is the
  right place for that concern; it owns its own netns and doesn't need
  privileged access to anyone else's.
- **Pinning the network-anchor's transit IP via `ipv4_address` /
  `ipv6_address`** — no. IPAM-stable IPs are a hidden coupling between
  Compose files; "Configuration scarcity" pushes toward DNS lookup
  instead.

## Why the binary has two modes

The network-anchor and the service-anchor are different roles, but they
share a lot: the netlink toolkit, the slog setup, the signal-driven
shutdown, the env-var configuration style. Splitting them into two
binaries would duplicate that scaffolding without solving any new
problem. Keeping them together also matches the single-image story
operators tell each other: "anchord is a Compose project's networking
shim — one network-anchor per project, N service-anchors, all the
same image."

The mode is selected by `ANCHORD_MODE`, defaulting to `network-anchor`.
The `command: [service-anchor]` form is a convenience equivalent. We
deliberately do **not** auto-detect the mode (e.g., from the presence
or absence of `ANCHORD_VLAN_PARENT`): silent fall-through to
service-anchor mode when an operator forgets to set the VLAN parent
would mask a misconfiguration as a working-but-pointless container.
Explicit mode selection produces loud, correct failures.

## When to break these rules

Never silently. If you're convinced a principle needs revising, write
a section in this file proposing the change before changing code. The
principles exist precisely because they got debated; un-debating them
in a commit message is the wrong shape.
