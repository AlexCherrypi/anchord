# anchord — Spec Delta: Wrap-Pattern Support

> **Status:** Draft. Captures three concrete requirements (F-38, F-39, F-40) plus a new architectural mode (F-41 wrap-pattern) that emerged from an attempted production migration of a 22-container Mailcow stack to anchord-v2 on 2026-05-18.
>
> **Motivation:** anchord-v2 (as landed in [SPEC.md](SPEC.md) §2 and [SPEC-v2-DRAFT.md](SPEC-v2-DRAFT.md)) works perfectly for new stacks where the operator can write the compose and put backends behind `network_mode: service:<service-anchor>` — that's how chocolatey/cups/vault/xibo migrated cleanly. It does **not** work for **wrapping** an existing Compose project (mailcow) where the operator can't easily change `network_mode` of running app containers without risk. The migration attempt hit:
>
> - **F-37-adjacent**: `detectSharedNetwork` (in `cmd/anchord/main.go`) picks the wrong network — it doesn't know to exclude `ANCHORD_EXT_NETWORK`. Already locally patched, needs formalising.
> - **Asymmetric routing**: backends in the wrapped project have their own Docker-managed default gateway, not the network-anchor. Forward path via DNAT works, reverse path bypasses anchord, conntrack reverse-DNAT never fires, TCP handshakes hang. No reasonable amount of MASQUERADE-on-inbound fixes this without breaking SPF/Mail-Reputation invariants.
> - **Cross-project DNS**: a service-anchor sitting in another project's container can't resolve `anchord` via the Docker DNS for that project (network-anchor is in a different project).
>
> **Affected user code:** the patches below are sufficient to make the wrap-pattern reach feature-parity with the standard pattern, without breaking F-1…F-37.

## F-38 — `detectSharedNetwork` excludes `ANCHORD_EXT_NETWORK`

### Problem

`cmd/anchord/main.go:detectSharedNetwork` iterates `insp.NetworkSettings.Networks`, prefers networks containing "transit" in the name, else falls back to the first (Go map iteration is random). With the wrap-pattern the network-anchor is on:

- `dmz_macvlan` (external network, configured as `ANCHORD_EXT_NETWORK`)
- `ix-mailcow_mailcow-network` (the wrapped project's bridge, where backends live)

There is no "transit" network. Random pick lands on `dmz_macvlan` ≈ 50 % of the time. anchord then attempts to read backend IPs from `dmz_macvlan` — backends aren't there → `"no usable IP for container"`, `backends:0`, no DNAT.

### Requirement

`detectSharedNetwork` must **never return the network named in `ANCHORD_EXT_NETWORK`**.

### Behaviour (verbal, no code)

1. Read the operator's `ANCHORD_EXT_NETWORK` from config.
2. Iterate `insp.NetworkSettings.Networks` and skip any entry whose key equals `ANCHORD_EXT_NETWORK`.
3. From the remaining candidates: prefer one whose name contains `transit` (case-insensitive). Otherwise return the first (still Go-map-random, but at least never the macvlan).
4. If `ANCHORD_EXT_NETWORK` is empty: behaviour unchanged from current implementation.
5. If after exclusion no candidates remain: return the existing "no networks on self" error — same code path as today, just a clearer log line: `"only EXT_NETWORK on self; need at least one project-internal network"`.

### Backwards-compat

- Stacks that don't set `ANCHORD_EXT_NETWORK` are unaffected.
- Stacks with a `transit` network still get it picked.
- Stacks without `transit` but with multiple non-EXT networks now get deterministic-by-exclusion behaviour (one less random pick).

### Acceptance

- Unit test: mock `ContainerInspect` returning two networks (`dmz_macvlan`, `ix-foo_bar`); cfg with `ExtNetwork="dmz_macvlan"`; result must equal `ix-foo_bar`.
- Unit test: only-EXT_NETWORK case → error mentioning `only EXT_NETWORK`.
- Existing tests for the `transit`-preference path must still pass.

### Notes / current status

Locally patched in `/mnt/Pool1/build/anchord` for the Mailcow attempt. Sat at ~10 lines: thread `cfg.ExtNetwork` into `detectSharedNetwork`, add the `if name == excludeNet { continue }` skip.

## F-39 — Service-anchor wrap mode (`network_mode: container:`)

### Problem

Standard pattern: an app container shares a service-anchor's netns via `network_mode: service:<anchor>`. The anchor is the namespace owner; the anchor's service-anchor mode installs a default route via the network-anchor; the app inherits it.

In a **wrap** scenario, the operator can't conveniently change `network_mode` on production app containers (e.g. Mailcow's `nginx-mailcow`, `postfix-mailcow`, `dovecot-mailcow`). The app containers already exist and have their own netns + Docker-managed default route via the bridge's gateway. The wrap-stack's network-anchor can't reach those netns to install a route.

### Requirement

Service-anchor mode must support **joining an existing container's netns from the outside** by declaring `network_mode: "container:<target>"` in compose. In that netns, run the existing service-anchor loop (F-25, F-26): install/maintain default route via the network-anchor.

### Compose convention

```yaml
nginx-anchor:
  image: ghcr.io/alexcherrypi/anchord:v2.1
  cap_add: [NET_ADMIN]
  network_mode: "container:ix-mailcow-nginx-mailcow-1"
  environment:
    ANCHORD_MODE: service-anchor
    ANCHORD_GATEWAY_HOSTNAME: anchord       # or IP, see F-40
    # (no `networks:` block — netns inherited from target)
```

### Behaviour

- `network_mode: container:X` is a Docker convention; service-anchor doesn't have to do anything special to enter the netns — Docker does it. Anchord runs as PID-1 in a different mount/IPC namespace but shares the target's network namespace, including `/etc/resolv.conf`, routing table, interfaces.
- Service-anchor's F-25 default-route-install runs as today. Subject: the route is installed in the **shared** netns, which is the target container's netns. The target container then sees the new default route.
- **F-26 re-resolution** continues — the gateway hostname is re-resolved every `ANCHORD_GATEWAY_RESOLVE_INTERVAL`. If the resolution returns a new address, the route is replaced atomically.
- **F-29 clean shutdown** is critical here: when the wrap-anchor service-anchor is stopped, it must **restore the target's original default route**, not just remove its own route. Otherwise the wrapped app container loses egress entirely once the wrap is torn down.
  - Strategy: at startup, before installing the new route, the service-anchor records the existing default route(s) in memory. On SIGTERM/SIGINT, it puts them back. If recording fails (no default route at startup), it logs a warning and on shutdown removes its own route only, leaving the target with no default route until the next Docker compose pass restores Docker's bridge gateway.
- **Lifecycle binding**: with `network_mode: container:X`, the wrap-anchor's lifecycle does **not** automatically follow the target's lifecycle. If the target restarts, the wrap-anchor's netns disappears; Docker restart-policy `unless-stopped` will recreate the wrap-anchor (which re-attaches to the newly-recreated target). Must be tested.

### Backwards-compat

- Existing service-anchors using `networks: [transit, backend]` (no `network_mode`) keep working unchanged.
- The new mode is purely additive — opt-in via `network_mode: container:` in compose.

### Acceptance

- Integration test: spin up a "fake app" container (e.g. nginx) on a bridge with Docker-managed gateway. Spin up a wrap-anchor with `network_mode: container:<app>` and `ANCHORD_MODE: service-anchor`. From inside the app container, `ip route show default` must show the network-anchor's IP within 5 s of wrap-anchor start.
- Clean shutdown test: `docker stop wrap-anchor` → the app container's `ip route show default` must restore the original Docker gateway within 2 s.
- 20-iteration recreate test: `docker restart fake-app` 20×, each time the wrap-anchor must re-install the route within 10 s of restart-policy bringing wrap-anchor back up. (`network_mode: container:` semantics may force wrap-anchor recreation, which is fine.)

## F-40 — Gateway addressable by IP

### Problem

Service-anchor F-24 resolves `ANCHORD_GATEWAY_HOSTNAME` (default `anchord`) via the netns's Docker DNS. With wrap-pattern (F-39), the service-anchor runs in a target container that lives in **a different Compose project** than the network-anchor. Docker DNS for the target's project doesn't know about the wrap-stack's `anchord` service by default.

Workarounds (alias the network-anchor on the shared external network, hack hosts files, etc.) are fragile. A clean solution: let the operator point service-anchor at an **explicit IP**.

### Requirement

- New env var `ANCHORD_GATEWAY_IP` (default empty).
- If non-empty: service-anchor skips DNS resolution and uses the supplied IP as the gateway target.
- `ANCHORD_GATEWAY_HOSTNAME` remains the default discovery mechanism; `ANCHORD_GATEWAY_IP` takes precedence when both are set, with a WARN log (mirror F-37's "both set; using IP" handling).

### Behaviour

- Validation at startup: `ANCHORD_GATEWAY_IP` must parse as a valid IPv4 or IPv6 address. Reject malformed values with a fatal error and clear log: `"ANCHORD_GATEWAY_IP=<value> is not a valid IP"`.
- For dual-stack: a single env var accepts either v4 or v6 (one or the other). For both: introduce `ANCHORD_GATEWAY_IPV4` and `ANCHORD_GATEWAY_IPV6` as alternatives. Or: keep one var, parse, treat as v4 if IPv4, v6 if IPv6, route only that family by IP, fall through to DNS for the other. Simpler: parse a comma-separated list (`192.168.0.1,fd00::1`).
- F-26 re-resolution: with IP mode, no re-resolution is needed — the route stays as configured. Skip the periodic re-resolve loop in this mode (drop CPU/log noise).
- IPv6 case: same behaviour for v6 routes.

### Backwards-compat

- Service-anchors without `ANCHORD_GATEWAY_IP` set behave unchanged — DNS-resolve as today.

### Acceptance

- Unit test: `ANCHORD_GATEWAY_IP=10.0.0.1` set → resolver returns `10.0.0.1` without any DNS call.
- Unit test: `ANCHORD_GATEWAY_IP=not-an-ip` → process exits with error.
- Integration: in a wrap setup with `network_mode: container:` and the target's DNS unable to resolve `anchord`, setting `ANCHORD_GATEWAY_IP` to the network-anchor's pinned IP yields a working default route within 5 s.

## F-41 — Wrap-pattern: architectural mode

### Problem statement

We have two distinct deployment patterns now:

1. **Greenfield pattern** (current SPEC.md §2.6 / compose.example.yaml): operator writes the compose, app containers use `network_mode: service:<anchor>`. Anchord owns everything end-to-end.
2. **Wrap pattern** (proposed): operator wraps an existing Compose project (Mailcow, Nextcloud-AIO, …) without touching app `network_mode`. Anchord runs alongside as a separate Compose project, scoped via `ANCHORD_PROJECT=<wrapped-project-name>`.

The wrap pattern requires F-38, F-39, F-40 plus the following architectural invariants.

### Architectural invariants for the wrap pattern

1. **Network-anchor placement:**
   - The network-anchor joins **the wrapped project's shared bridge** (external) AND the macvlan/external DMZ network.
   - On the shared bridge, the network-anchor has a **pinned IP** and a **stable alias** (`aliases: [anchord]`) so service-anchors can resolve it via Docker DNS scoped to the shared bridge. F-40 is a fallback when DNS doesn't work across project scopes.
   - Default gateway preference: the network-anchor's default route must go via the **macvlan** (DMZ), not via the wrapped bridge. Use compose `priority:` field on the macvlan network entry to bias Docker's pick — empirically `priority: 1000` on macvlan + default-priority on bridge has been observed to NOT flip the default route reliably (Docker version-dependent). Stronger: have the macvlan reference declared **first** in `networks:` AND `priority` set — both as belt-and-suspenders. Investigate whether anchord should `ip route replace default` explicitly at startup.

2. **Service-anchor placement (wrap mode):**
   - One service-anchor per labelled backend container, each using `network_mode: "container:<backend>"`.
   - Each service-anchor uses F-39 to install the default route in the backend's netns.
   - The labels (`anchord.expose: tcp/<port>,…`) live on the **backend containers**, not on the service-anchors — the network-anchor's discovery is label-scoped to `ANCHORD_PROJECT=<wrapped-project>`, which is the project that owns the backends.

3. **Backwards-compat with greenfield pattern:**
   - Both patterns coexist. The network-anchor doesn't need to know which pattern is in play — its job (DNAT + masquerade) is the same.
   - Service-anchor doesn't need to know if it's in greenfield or wrap mode — `network_mode: container:` is purely a compose-level concern.

### Bookkeeping in `cmd/anchord`

- Document in `ARCHITECTURE.md` that two modes exist for operators.
- `compose.example.yaml` keeps the greenfield example.
- Add `compose.example-wrap.yaml` with a complete Mailcow-style wrap example (no upstream mailcow YAML included, just the wrap-stack + override snippet to apply on the existing project).

### Open questions / explicitly out-of-scope for this delta

- **Mailcow + IPv6 NAT**: Mailcow ships an `ipv6nat-mailcow` sidecar that manages its own ip6tables for the project's IPv6 plane. Interaction with anchord's IPv6 DNAT is unspecified. Test before promoting v2.1.
- **Outbound egress in wrap-pattern**: with the new default route installed by F-39, backend egress now goes via anchord → masquerade → macvlan. This is the desired behaviour (PTR/SPF identity = anchord's DMZ IP). But: if the host has any iptables rule that source-NATs *to* the bridge gateway, that rule must not fire for traffic coming from anchord. Document a check.
- **Failure mode if F-39 service-anchor dies mid-flight**: the backend container retains anchord's IP as default route; if anchord vanishes, all backend egress hangs. Restart policy `unless-stopped` on the wrap-anchor mitigates. Document.
- **Multiple network-anchors on the same shared bridge** (e.g. two wrapped projects sharing one DMZ): each gets a different `anchord` alias? Or different aliases per project (`anchord-mailcow`, `anchord-nextcloud`)? Recommend the latter; document.

## Implementation priorities (suggested)

1. **F-38** first — small change, lifts the random-pick footgun.
2. **F-40** — also small, makes F-39 easier to test (no DNS-cross-project pain).
3. **F-39** — the meat. Requires lifecycle/route-restoration design.
4. **F-41 + compose.example-wrap.yaml** — once F-38/39/40 work end-to-end.

## Tests-to-add (consolidated)

- Unit:
  - F-38 detection with exclusion (3 cases above)
  - F-40 IP parsing + precedence vs DNS
- Integration / e2e:
  - F-39 default-route install in target container's netns
  - F-39 default-route restore on wrap-anchor shutdown
  - F-39 + F-40 working together with cross-project topology

## Cross-references

- [SPEC.md §2.1, §2.6](SPEC.md) — current network-anchor + service-anchor behaviour.
- [SPEC-v2-DRAFT.md](SPEC-v2-DRAFT.md) — F-37 EXT_NETWORK resolution context.
- The triggering incident: a 22-container Mailcow stack migration attempt on TrueNAS Scale (2026-05-18), where greenfield pattern was structurally unworkable without re-architecting Mailcow itself. See [lammers-krueger-firewall/projects/10g-dmz-migration/state.md](../../lammers-krueger-firewall/projects/10g-dmz-migration/state.md) for the chronology.
