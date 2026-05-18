# anchord — SPEC v2 (Draft)

> **Status:** Implemented on `main` (2026-05-18). Kept as a draft document
> because v1 (`SPEC.md`) is still the canonical, fully-specified contract;
> this file records the v2 deltas until SPEC.md absorbs them.

## The one-sentence change

**v1:** anchord owns L2 (creates a macvlan child), L3 (DHCP) and L4 (DNAT/MASQUERADE).
**v2:** Docker owns L2 (macvlan network); anchord joins it like any other container and owns L3/L4.

That is the whole pivot. The mental model and the user-facing acceptance scenarios from SPEC.md are unchanged — what changes is who installs the host-side plumbing. **Dual-stack (IPv4 + IPv6) remains a first-class property:** the NAT plane is family-agnostic, and every address mode permits both families independently (see F-1/F-2).

## Why

The L2 part was the only piece coupling anchord to host-OS specifics (which sysctls are accepted in the container netns, whether `network_mode: host` lets us set them, exactly how the kernel deals with macvlan-on-VLAN-sub-interface on a given distro). Docker has well-tested macvlan support across distros; anchord doesn't need to reinvent it. By delegating that, anchord becomes a plain container on the macvlan network and stops being a per-distro bug magnet.

## Functional deltas vs. SPEC.md

### F-1 (revised) — External IPv4 sourcing

A v2 network-anchor obtains its external IPv4 in one of three ways, selected by `ANCHORD_ADDRESS_MODE`:

- `bootstrap` *(default)* — accept the IPv4 Docker assigned via `ipv4_address:` (or via the macvlan network's IPAM) and keep it for the lifetime of the container.
- `dhcp-refresh` — start on Docker's bootstrap IP so the container is reachable immediately, then run a DHCP client with a hostname-derived client-id. When a lease arrives, replace the bootstrap address atomically on the iface. Honour T1 renewals and send `DHCPRELEASE` on shutdown.
- `slaac-ra-only` — keep Docker's bootstrap IPv4; the v6 side is kernel-managed (see F-2). No DHCP client runs.

In all three modes the iface itself is created and destroyed by Docker. anchord never adds or removes a link.

### F-2 (revised) — External IPv6 sourcing

v2 is fully dual-stack — v4 and v6 are independent and both surface in the DNAT/MASQUERADE plane (anchord's nft tables exist for both families unconditionally). How the v6 address is sourced depends on the address mode and what the network announces:

- `bootstrap`: if the shared macvlan network's IPAM has a v6 subnet, Docker assigns a v6 address via compose `ipv6_address:` or its IPAM, same as v4. Kernel SLAAC additionally adds an RA-derived address if the LAN announces one.
- `dhcp-refresh`: kernel SLAAC plus a best-effort stateful DHCPv6 SOLICIT/REQUEST. On a SLAAC-only network the SOLICIT times out and the client retries silently — the kernel's SLAAC address is what's used.
- `slaac-ra-only`: kernel SLAAC only. No DHCPv6 client runs.

v6-less environments must not block v4 operation; v4-less environments must not block v6 operation (each family is independently functional).

### F-3 (revised) — MAC stability

The MAC is set by Docker via `mac_address:` in compose, not derived by anchord. The operator picks a stable MAC (or accepts Docker's deterministic-from-container-name default). DHCP identification across recreates is anchored by the client-id (derived from `ANCHORD_DHCP_HOSTNAME`), not the MAC, so swapping the MAC does not lose the reservation.

### F-20 (revised) — Clean teardown

anchord still removes its nftables tables on SIGTERM/SIGINT and (in dhcp-refresh mode) sends `DHCPRELEASE`. It no longer needs to delete a macvlan child — Docker reaps that when the container exits.

### F-23 (revised) — Runtime capabilities

The network-anchor still needs `CAP_NET_ADMIN` (for nftables and, in dhcp-refresh mode, netlink address replacement on the iface) and a small set of forwarding sysctls (`net.ipv4.ip_forward`, `net.ipv6.conf.all.forwarding`, `net.ipv6.conf.all.accept_ra=2`) because it still routes packets between the macvlan and the project-internal transit bridge. It no longer needs:

- `network_mode: host`
- the v1-era ARP-tuning sysctls (`arp_ignore`/`arp_announce`) that worked around macvlan-on-parent ARP collisions
- a separate host-side VLAN-sub-interface argument (`ANCHORD_VLAN_PARENT` removed)

It is now a regular Docker container on the macvlan network.

## Environment variable changes

| Variable | v1 | v2 |
|---|---|---|
| `ANCHORD_PROJECT` | required | unchanged |
| `ANCHORD_VLAN_PARENT` | required | **removed** (Docker owns the parent) |
| `ANCHORD_EXT_IFACE` | name of the macvlan child anchord creates, default `anchord-ext` | name of the macvlan iface Docker plumbed in, default `eth0` |
| `ANCHORD_EXT_MAC` | optional, derived from project name otherwise | **removed** — declare via `mac_address:` in compose |
| `ANCHORD_ADDRESS_MODE` | — | **new**, default `bootstrap` |
| `ANCHORD_DHCP_HOSTNAME` | unchanged | unchanged (also basis of DHCP client-id) |
| `ANCHORD_DHCP_BACKOFF_MAX` | unchanged | only meaningful in `dhcp-refresh` mode |
| everything else | unchanged | unchanged |

## Compose surface

The user supplies:

```yaml
networks:
  dmz:
    external: true
    name: dmz_macvlan       # created out-of-band, one-time per host

services:
  anchord:
    image: ghcr.io/alexcherrypi/anchord:latest
    cap_add: [NET_ADMIN]
    mac_address: "02:4c:4b:50:0a:01"
    networks:
      dmz:
        ipv4_address: 192.168.150.100   # bootstrap IP
    environment:
      ANCHORD_PROJECT: ${COMPOSE_PROJECT_NAME}
      ANCHORD_ADDRESS_MODE: bootstrap   # or dhcp-refresh / slaac-ra-only
      ANCHORD_DHCP_HOSTNAME: mailcow
```

Plus the project-internal `transit` bridge that v1 already required, joined as a second network so service-anchors can reach the network-anchor by Docker DNS (`anchord`).

Service-anchor compose surface is unchanged.

## Acceptance scenarios that need re-validation

- **S-1 Cold start** — requires the `dmz_macvlan` network to exist before the project comes up. Otherwise unchanged.
- **S-4 DHCP lease rotation** — only applies when `ANCHORD_ADDRESS_MODE=dhcp-refresh`. The bootstrap → leased IP swap is the new code path to exercise.
- **S-5 Two projects, same host, same VLAN** — trivially true: each project joins the shared macvlan; Docker prevents IP collisions at compose-up time.
- **S-9 Network-anchor recreate** — restart no longer recreates a macvlan child (Docker owns it). NAT state is rebuilt from labels, same as before.

S-2, S-3, S-6, S-7, S-8 are unchanged in form and outcome.

## Non-goals (still)

- anchord-v2 does not own the macvlan network. If the shared network is gone, the project can't start — that's correct, fail-fast.
- anchord-v2 does not create VLAN sub-interfaces on the host. That's an OS-level operator concern.
- v2 still doesn't try to be a load-balancer, service-mesh, or cert-distributor. Same focus: one IP per project, identity-stable.

## Migration path for existing v1 users

1. Create the shared macvlan network once per host (or via a tiny network-only compose project).
2. In each project:
   - Bump the anchord image to v2.
   - Replace `networks: [transit]` on the network-anchor with `networks: { dmz: { ipv4_address: ... }, transit: {} }`.
   - Trim the sysctls block: keep `net.ipv4.ip_forward=1`, `net.ipv6.conf.all.forwarding=1`, `net.ipv6.conf.all.accept_ra=2`; drop the v1 ARP gymnastics (`arp_ignore`/`arp_announce`).
   - Drop `ANCHORD_VLAN_PARENT`/`ANCHORD_EXT_MAC` env vars.
   - Add `mac_address:` if you want a stable MAC (most do).
   - Pick an `ANCHORD_ADDRESS_MODE` (start with `bootstrap`).
3. Backend containers and `anchord.expose` labels are unchanged.
