# anchord — Spec Delta: Port-Translation in DNAT (F-46)

> **Status:** Draft. Triggered live by the Authentik DMZ migration on 2026-05-19. The Authentik LDAP-outpost binds LDAPS to `0.0.0.0:6636` (unprivileged default — outposts run as non-root by upstream design). When anchord-ldap DNATs the DMZ-facing `tcp/636` to the outpost's IP at `:636`, the connection is refused: no listener on 636 inside the netns. Today's pragmatic workaround is an in-band `nft REDIRECT 636 -> 6636` inside the service-anchor netns (installed once by an init sidecar). That works but is non-persistent across container restarts and clearly belongs in anchord, not in user-side YAML.
>
> **Motivation:** The current `anchord.expose=tcp/<port>` label is interpreted as "this is both the DMZ-side listen port AND the backend-side dial port." Real Docker/Compose backends regularly listen on *different* (usually higher) ports than the well-known port the operator wants to expose. Authentik's LDAP outpost is the canonical case; any future wrap-target that respects the "don't run as root, don't bind <1024" convention will hit the same edge.
>
> **Affected user code:** label-schema extension; small change in the nft-map element type; ~20-40 LOC in the discovery+nftables module. No new env vars, no new mode.

## F-46 — Port-translating DNAT

### Problem

Current `anchord.expose` label syntax is `<proto>/<port>` (e.g. `tcp/443`). The picker reads it, builds a `dnat_<proto>` map of `<port> : <backend_ipv4>`, and the prerouting chain applies:

```
iifname "eth0" dnat to tcp dport map @dnat_tcp
```

That's address-translation only. The destination port stays equal to the inbound port. Works fine when the backend listens on the same port as the DMZ-facing port (Traefik on 443, dovecot on 25, etc.), but fails when they differ:

```
DMZ:636 -> ak-outpost-ldap:636   (REFUSED — outpost listens on 6636)
```

### Requirement

Extend the label grammar from `<proto>/<port>` to `<proto>/<dmz_port>[:<backend_port>]`. When `<backend_port>` is absent, behaviour is unchanged from F-39 (DMZ port == backend port). When present, anchord installs port-translating DNAT so that DMZ traffic on `<dmz_port>` is rewritten to `<backend_ip>:<backend_port>`.

Examples:
- `anchord.expose: "tcp/443"` — same as today. DMZ 443 → backend :443.
- `anchord.expose: "tcp/636:6636"` — DMZ 636 → backend :6636.
- `anchord.expose: "tcp/443,tcp/636:6636,udp/636:6636"` — comma-separated list still supported. Each entry has its own optional translation.

### Behaviour (verbal, no code)

1. **Label parser**: Update the parser in `internal/discovery` to accept the optional `:<backend_port>` suffix. Validate that `<backend_port>` is `1..65535`. When omitted, default to `<dmz_port>` (preserve F-39 behaviour exactly).
2. **nft-map element type**: Today's `map dnat_tcp { type inet_service : ipv4_addr }` only maps port → address. Change to `map dnat_tcp { type inet_service : ipv4_addr . inet_service }`, where the value is a *tuple* of `(backend_addr, backend_port)`. Then the DNAT rule becomes:
   ```
   iifname "eth0" dnat ip to tcp dport map @dnat_tcp
   ```
   (nftables supports this since 0.9.0; verify on the target kernel before merging.)
3. **Map population**: For each backend `b` with `anchord.expose=tcp/Dmz:Backend`, insert `Dmz : b.ip4 . Backend` into the v4 map and the analogous `Dmz : b.ip6 . Backend` into the v6 map. When no translation is configured, insert `Dmz : b.ip4 . Dmz`.
4. **Conntrack flush**: When a backend's `<backend_port>` changes (label edit, container recreate), conntrack entries for `(DMZ-IP, Dmz)` should be flushed. Same hook as today for IP changes, just add port to the comparison key.
5. **Log line**: When a translating expose is installed, log:
   ```
   {"msg":"port-translating DNAT installed","dmz_port":636,"backend":"ak-outpost-ldap","backend_addr":"172.31.80.9","backend_port":6636}
   ```
   When a non-translating expose, keep today's log line so single-port stacks see no spurious info.

### Backwards-compat

- All existing `anchord.expose=<proto>/<port>` labels keep the same semantics. No re-deploys required for any current wrap stack.
- New `:<backend_port>` syntax is opt-in. Stacks that don't need translation are unaffected.

### Acceptance tests

Unit:
- `parseExpose("tcp/443")` → `{Proto:tcp, DmzPort:443, BackendPort:443}`.
- `parseExpose("tcp/636:6636")` → `{Proto:tcp, DmzPort:636, BackendPort:6636}`.
- `parseExpose("tcp/636:")` → parse error.
- `parseExpose("tcp/636:99999")` → out-of-range error.
- Conntrack-flush hook fires when backend's port part of the tuple changes.

Integration (synthetic):
- Compose: anchord on dmz+backend, one backend container with `anchord.expose=tcp/636:6636` and a process listening on 6636. Expect:
  - `nft list table ip anchord_v4` shows `636 : <backend_ip4> . 6636` in `dnat_tcp`.
  - Connecting `<dmz_ip>:636` from outside reaches `<backend_ip>:6636` and gets data back.
  - Conntrack flush triggers when backend container recreated on a different port.

Migration scenario:
- Authentik LDAP outpost: replace today's `nft REDIRECT 636->6636` workaround. After F-46 lands, the `ldap-sa-init` script in `lammers-krueger-firewall/projects/10g-dmz-migration/state.md` can stop installing the redirect; just change `anchord.expose=tcp/636` to `anchord.expose=tcp/636:6636` on the outpost and remove the in-band nft step.

### Notes / current status

Triggered live by the Authentik migration 2026-05-19. Symptom:

```
# DMZ → :636 refused, outpost listens on 6636
# ak-outpost-ldap log: "Starting LDAP SSL server" "listen":"0.0.0.0:6636"
```

Today's workaround: in-band nft REDIRECT installed by an init sidecar in the same netns as the outpost (ldap-service-anchor + ak-outpost-ldap share netns via `--network container:ak-outpost-ldap`). Works because the service-anchor has NET_ADMIN; idempotent re-run on every compose-up via the init script. Not persistent if just the service-anchor is restarted (rare in practice).

Implementation estimate: 20-40 LOC plus tests.

- `internal/discovery`: label parser + small struct extension (`BackendPort int`).
- `internal/nftables`: change map element type and dnat-statement form. Test against kernel 5.10+ which is what TrueNAS Scale ships.
- No new env var, no new mode.

Order vs. F-44/F-45: independent. Can ship in any order. F-46 is arguably the lowest-LOC and highest-pain-relief of the three pending features for the lammers-krueger-firewall stack (it lets us delete the ldap-sa-init nft step entirely).

## Cross-references

- [SPEC.md §2.1](SPEC.md) — current DNAT model.
- [SPEC-WRAP-DRAFT.md F-39](SPEC-WRAP-DRAFT.md) — `anchord.expose` label semantics; F-46 extends this.
- [SPEC-LABEL-SELECTOR-DRAFT.md F-42](SPEC-LABEL-SELECTOR-DRAFT.md) — selector to find the expose-labelled backend.
- The triggering migration: Authentik LDAP outpost on TrueNAS. See [lammers-krueger-firewall/projects/10g-dmz-migration/state.md](../../lammers-krueger-firewall/projects/10g-dmz-migration/state.md), 2026-05-19 entry.
