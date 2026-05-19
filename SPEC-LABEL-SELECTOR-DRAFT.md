# anchord — Spec Delta: Label-scoped backend discovery (F-42)

> **Status:** Draft. Captures one new feature (F-42) that emerged while planning the Authentik DMZ migration on 2026-05-19. anchord-v2 in its current form assumes **one network-anchor per Compose project**: it discovers backends by scanning containers labelled `com.docker.compose.project=$ANCHORD_PROJECT` for the `anchord.expose` label. Two limitations follow that block the Authentik case:
>
> 1. **Multiple anchords cannot coexist** in one project (each finds every `anchord.expose` container, DNAT-clobbers).
> 2. **Authentik-spawned containers** (LDAP outpost, proxy outpost) carry **no Compose-project label at all** — verified empirically on 2026-05-19: `docker inspect ak-outpost-ldap` shows only `io.goauthentik.outpost-uuid` and `org.opencontainers.image.*` labels. anchord's current project filter excludes them out of the gate, regardless of how we'd extend things downstream.
>
> **Motivation:** Authentik's worker spawns satellite containers via Docker socket. Their lifecycle is owned by authentik, version-pinned to the authentik-server image, automatically redeployed on update. We want to **keep that auto-managed lifecycle** but give each spawned outpost its **own DMZ IP** (own identity, own firewall surface). A natural design is: one network-anchor per outpost-flavour, all in the same `ix-authentik` Compose project, each watching only "its" containers. Today that doesn't work for either of the two reasons above.
>
> **Affected user code:** new optional env var, small refactor of the backend-discovery filter from "fixed project-label match" to "operator-defined label selector". No change to the DNAT engine, address-manager, service-anchor logic, or DHCP plumbing.

## F-42 — `ANCHORD_LABEL_SELECTOR` for backend discovery

### Problem

anchord's backend-discovery loop (in `cmd/anchord/main.go`, called from reconcile) does roughly:

1. List containers via Docker API with filter `label=com.docker.compose.project=$ANCHORD_PROJECT`.
2. For each container, inspect labels.
3. If `anchord.expose` is present: parse the port spec, look up the container's IP on the shared network, add a DNAT entry.

Two failure modes drop out:

**(a) Multiple anchords per project — DNAT clobber.** With more than one anchord in a project, each finds every `anchord.expose`-labelled container. No way to partition. Each reconcile cycle overwrites the others' nftables map. Useless.

**(b) Project-less containers — invisible to anchord.** Containers spawned *not* via Docker Compose (e.g. via the Docker API directly, as authentik-worker does for outposts) have no `com.docker.compose.project` label. The current filter excludes them before any `anchord.expose` check runs. Verified 2026-05-19 with `ak-outpost-ldap`:

```
$ docker inspect ak-outpost-ldap --format '{{range $k,$v := .Config.Labels}}{{$k}}={{$v}}{{println}}{{end}}'
io.goauthentik.outpost-uuid=ba835c8d0db440a9baef5819c1ca562e
org.opencontainers.image.*= …image metadata…
# no com.docker.compose.project label.
```

This is **upstream behaviour** of authentik-worker's Docker integration — it sets `io.goauthentik.outpost-uuid` and the image-default labels, nothing else. We could ask the operator to add a `docker_labels: { com.docker.compose.project: ix-authentik }` line in the authentik Outpost YAML, but that's *spoofing a Compose-managed reserved label*, which is fragile (any future Compose-level enforcement, `docker compose ls --filter`, etc. will see the spawned container as part of `ix-authentik` and may try to manage it). The cleaner answer is to let anchord match on **operator-chosen labels** instead of being hard-coded to the Compose-project label.

Concrete scenario driving this (Authentik):

```
Compose project ix-authentik (regular containers, Compose-managed):
  authentik_server          labels: anchord.role=authentik-server, anchord.expose=tcp/443
  authentik_worker          labels: (none anchord-relevant; just runs Docker spawn)
  authentik_postgresql, authentik_redis  (backend-only, no labels)

Project-less containers (authentik-worker spawned, Docker-API direct):
  ak-outpost-ldap           labels: anchord.role=ldap-outpost,    anchord.expose=tcp/389,tcp/636
                                    io.goauthentik.outpost-uuid=<uuid>
  ak-outpost-frigate-proxy  labels: anchord.role=frigate-proxy,   anchord.expose=tcp/443
                                    io.goauthentik.outpost-uuid=<uuid>

Network-anchors (all in ix-authentik):
  anchord-authentik   wants ONLY authentik_server         → DMZ .80
  anchord-ldap        wants ONLY ak-outpost-ldap          → DMZ .81
  anchord-frigate     wants ONLY ak-outpost-frigate-proxy → DMZ .82
```

The `anchord.role` and `anchord.expose` labels on the outposts are populated via authentik's existing `docker_labels` config option on each outpost (no authentik patch needed).

### Requirement

Introduce **`ANCHORD_LABEL_SELECTOR`** (optional). When set, it **replaces** the existing `ANCHORD_PROJECT`-based project filter as the primary backend-discovery scope. The selector is a comma-separated `key=value` list, AND-joined, equality only. A container is a candidate iff it carries **all** selector labels AND carries `anchord.expose`.

```
ANCHORD_LABEL_SELECTOR=anchord.role=ldap-outpost
ANCHORD_LABEL_SELECTOR=team=infra,env=prod      # AND-joined
```

Format details:
- Whitespace tolerated around commas and `=`.
- Empty value (`key=`) matches containers where `key` is literally the empty string. No "any value" wildcard — that's pursued in F-43 if needed.
- No `!=`, set-membership, regex.
- Duplicate key with conflicting values → fatal startup error.

When **unset** (default), the existing `ANCHORD_PROJECT`-based filter is used unchanged — backwards-compat for chocolatey, vaultwarden, xibo, traefik, cups, mailcow.

When **set** alongside a non-empty `ANCHORD_PROJECT`: the selector wins. `ANCHORD_PROJECT` is ignored (with a startup-log warning so the operator isn't surprised). Rationale: a single, unambiguous discovery scope is easier to reason about than two ANDed scopes, and the authentik case explicitly *wants* to escape the project filter. If the operator wants both, they put it in the selector: `ANCHORD_LABEL_SELECTOR=com.docker.compose.project=ix-mailcow,anchord.team=front`.

### Behaviour (verbal, no code)

1. At startup, `internal/config` parses `ANCHORD_LABEL_SELECTOR` into a `map[string]string`. Validation:
   - Empty env var → empty map (= use legacy `ANCHORD_PROJECT` filter, current behaviour).
   - Malformed pair (no `=`, or duplicate key with different values) → **fatal startup error** with a clear log line naming the offending pair. Same exit code as other malformed-env paths.
2. Discovery filter selection at startup:
   - If `ANCHORD_LABEL_SELECTOR` is non-empty → use the selector as the discovery filter. If `ANCHORD_PROJECT` is *also* set, log `"ANCHORD_LABEL_SELECTOR set; ANCHORD_PROJECT=<x> ignored"` and proceed.
   - Else → use `ANCHORD_PROJECT` as today, implicitly = `com.docker.compose.project=$ANCHORD_PROJECT`.
3. The container-list call to the Docker API uses the selected filter. Then, post-list, the existing `anchord.expose` check is applied unchanged.
4. The log line on each reconcile cycle continues to print `backends:N entries:M`, where `N` is the *post-filter* count. Add a single startup log line `"backend label selector active: k1=v1,k2=v2"` when the selector is non-empty, so it's obvious in the journal which anchord is which.
5. **No effect on:** address-manager, DHCP, service-anchor mode, nftables-table layout, conntrack flush, label-watch event handling. Selector applies to the same container-list code path that already handles `anchord.expose`; everything downstream is unchanged.

### Coexistence with multiple anchords in one project (or in no project at all)

The operator's contract:
- Each network-anchor **MUST** use a selector that yields a **disjoint** backend set from every other anchord that shares its discovery surface. anchord does not enforce disjointness across processes (each anchord only knows its own selector). Violating this — two anchords matching the same container — is operator error; symptom is the same nftables-clobber pattern that motivates F-42 in the first place. Document this loud, but no code-level check.
- It is fine to mix anchords using `ANCHORD_PROJECT` and `ANCHORD_LABEL_SELECTOR` in the same compose, as long as the backend sets don't overlap. Practically: pick **one regime per anchord-instance** and verify the resulting `backends:N` count at startup matches expectation.

### Coexistence with auto-spawned containers (e.g. authentik outposts)

The triggering use case spawns containers dynamically via the Docker socket from one of the anchored services (authentik-worker). For F-42 to "just work":

- The spawning service sets selector-matching labels on the spawned container. authentik exposes `docker_labels` as part of its Outpost YAML, so this is achievable from the authentik UI without a code patch on either side. Example operator configuration in authentik:
  ```yaml
  # Outpost YAML in Authentik UI for the LDAP outpost
  docker_network: ix-authentik_ldap-outpost  # bridge anchord-ldap is also on
  docker_labels:
    anchord.role: ldap-outpost
    anchord.expose: "tcp/389,tcp/636"
  ```
- The anchord-ldap container is configured with `ANCHORD_LABEL_SELECTOR=anchord.role=ldap-outpost`. When authentik-worker spawns the LDAP outpost container, anchord-ldap sees it on the shared bridge with the matching label, writes the DNAT entry.
- No Compose-project label needed on the spawned container. The selector entirely replaces the project filter for that anchord-ldap instance.
- The authentik-server itself (which IS in `ix-authentik` via Compose) carries `anchord.role: authentik-server` + `anchord.expose: tcp/443` via the Compose override file; the anchord-authentik instance uses `ANCHORD_LABEL_SELECTOR=anchord.role=authentik-server` and matches only that.

### Backwards-compat

- Stacks without `ANCHORD_LABEL_SELECTOR` set: zero behavioural change. Greenfield (chocolatey, vaultwarden, xibo, traefik, cups) and wrap (mailcow) all stay green. Greenfield e2e + wrap e2e — both unchanged.
- Setting `ANCHORD_LABEL_SELECTOR` while *also* setting `ANCHORD_PROJECT`: explicit choice (selector wins) + startup warning, so old configs continue to work if the operator just adds the selector without removing the legacy env.
- The new env var name is namespaced (`ANCHORD_…`) so no collision with Docker, lego, or other env-driven libraries.

### Acceptance tests

Unit:
- `parseLabelSelector("")` → empty map, no error.
- `parseLabelSelector("a=1")` → `{a:1}`.
- `parseLabelSelector("a=1, b = 2")` → `{a:1, b:2}` (whitespace stripped).
- `parseLabelSelector("a=1,a=2")` → fatal error `"label selector has duplicate key 'a' with conflicting values '1' and '2'"`.
- `parseLabelSelector("nokey")` → fatal error `"label selector entry 'nokey' missing '='"`.

Discovery-filter (mocked Docker API):
- Selector `{role:ldap}` and three candidate containers (one with role=ldap+expose, one with role=proxy+expose, one with expose only) → only the role=ldap container is returned as backend.
- Selector `{role:ldap, env:prod}` AND-semantics — only containers with **both** labels match.
- Empty selector → identical result to today (no F-42 path taken).

Integration (multi-anchord-in-project):
- Boot a compose with two anchords (selectors `role=A` and `role=B`) and three backends (`role=A`, `role=B`, `role=C`). Expect each anchord's nftables map to hold only its matching backend, and the `role=C` container to be ignored by both. No DNAT-clobber across reconcile cycles.

Integration (project-less containers):
- One anchord with `ANCHORD_LABEL_SELECTOR=anchord.role=spawned-thing` (no `ANCHORD_PROJECT` set). Then `docker run --label anchord.role=spawned-thing --label anchord.expose=tcp/80 --network shared-bridge nginx:alpine`. The anchord finds it via the selector despite no `com.docker.compose.project` label being present. DNAT entry appears within one reconcile cycle.

Compat:
- All existing tests with only `ANCHORD_PROJECT` set must pass unchanged.
- A unit test verifies the "warning + ignore" path when both env vars are set.

### Notes / current status

Not yet implemented. The estimate is ~40–60 LOC plus tests:
- `internal/config`: new field `LabelSelector map[string]string`, parser, validation, precedence resolution with `Project`.
- `cmd/anchord/main.go` (or wherever the Docker-API container-list call lives): switch the Docker filter argument from `label=com.docker.compose.project=$ANCHORD_PROJECT` to the selector-derived list when the selector is set.
- Startup log lines: `"backend label selector active: …"` and the `"ANCHORD_PROJECT=… ignored"` warning when both are set.
- Tests as listed.

The Authentik DMZ migration ([lammers-krueger-firewall/projects/10g-dmz-migration](../../lammers-krueger-firewall/projects/10g-dmz-migration/)) is gated on this feature. Until F-42 lands, authentik will stay LAN-side, and the meshcentral OIDC path will continue to block on `DMZ→LAN`-firewall (state.md, meshcentral section).

## Implementation priority

Single-shot feature, no dependency on other open Fs. Recommended to land before the next migration round:

1. F-42 implementation + tests.
2. Verify on a throwaway compose with two synthetic anchords with role-based selectors.
3. Verify project-less-container path with a `docker run` not via Compose.
4. Roll into authentik migration.

## Cross-references

- [SPEC.md §2.1](SPEC.md) — current network-anchor backend-discovery contract.
- [SPEC-WRAP-DRAFT.md F-38](SPEC-WRAP-DRAFT.md) — sibling spec for `ANCHORD_EXT_NETWORK` exclusion, similar small-surface-area pattern (env var + filter).
- The triggering migration plan: authentik on TrueNAS, with LDAP-outpost and Frigate-proxy-outpost wanting their own DMZ identities. See [lammers-krueger-firewall/projects/10g-dmz-migration/decisions.md](../../lammers-krueger-firewall/projects/10g-dmz-migration/decisions.md) (pending entry D-014 once F-42 lands).
