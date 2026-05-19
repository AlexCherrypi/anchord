# anchord — Spec Delta: Anchord-managed service-anchor (F-45)

> **Status:** Draft. Captures one new feature (F-45) that emerged during the Authentik DMZ migration on 2026-05-19. F-43 added "auto-start `Created`-state sibling service-anchors when target appears" — that solves the case where the operator declares the service-anchor in Compose and Compose creates it (in `Created` state) at compose-up time. **But Compose doesn't actually accept that:** `docker compose up -d` calls `docker create` (succeeds for `network_mode: container:NONEXISTENT`) then `docker start` (fails) and treats the whole deployment as failed. F-43 never gets a chance to start the sibling because Compose already aborted.
>
> **Motivation:** Authentik's worker spawns outpost containers at runtime via Docker API. The naming is stable (`ak-outpost-ldap`), but they only exist *after* authentik_server is healthy and worker has read its config. Service-anchors for these outposts (to fix the asymmetric-routing problem on direct TCP DNAT — LDAP-bind from LAN to DMZ) cannot be declared in Compose because Compose halts during create-but-cant-start. Workaround applied today: a `docker:cli` init-sidecar that runs `docker create` via Docker API (no Compose involvement), then exits. F-43 picks up from there once the target spawns. Works, but it's ugly: an extra container, an extra layer of indirection, and the resulting service-anchor isn't tracked by Compose so `app.down` doesn't clean it up.
>
> F-45 lets the **network-anchor itself** create the service-anchor on demand. The operator declares the recipe via env vars on the network-anchor; when the network-anchor sees a matching target appear, it both **creates and starts** the service-anchor via the Docker socket. F-43 (start-only) becomes a subset of F-45 (create-then-start) for the on-demand path.
>
> **Affected user code:** new optional env vars on the network-anchor; one new code path in the autostart loop. Greenfield (F-26), wrap (F-39), and the F-43 auto-start path stay unchanged for stacks that declare the service-anchor in Compose.

## F-45 — Network-anchor creates service-anchor on demand

### Problem

The Authentik migration design wants three DMZ identities (`.150.80` web, `.150.81` LDAPS, `.150.82` frigate-proxy), with the LDAP outpost served on its own DMZ IP via direct TCP DNAT. Direct DNAT means asymmetric routing — outpost reply traffic goes via Docker bridge gateway, not back through anchord, conntrack reverse-DNAT never fires, client connection breaks. The fix is a service-anchor inside the outpost's netns (F-39) installing the default route via anchord (F-40).

But the outpost is **worker-spawned at runtime**. The service-anchor can't `depends_on: ak-outpost-ldap` in Compose because Compose doesn't know about it. Without the dep, Compose tries to `docker create` + `docker start` the service-anchor as part of `compose up`. Empirically (verified 2026-05-19):

```
$ docker compose up -d
…
 Container ix-authentik-ldap-service-anchor-1  Creating
 Container ix-authentik-ldap-service-anchor-1  Created                # create: ok
 …
Error response from daemon: cannot join network namespace of a non running container: container ak-outpost-ldap is created
```

Compose halts. TrueNAS' `app.start` returns the failure. F-43's "auto-start created-state sibling when target appears" never runs, because Compose itself reverted out before reaching steady state.

The workaround we shipped today: an init-sidecar that runs `docker -H tcp://docker-proxy:2375 create … anchord:v2-f43` — Docker-create directly, no Compose involvement. The service-anchor lives in `Created` state until F-43 starts it. Works, but:

- Adds an extra container to the Compose (`ldap-sa-init`) per identity, just to bootstrap.
- The created service-anchor doesn't carry Compose-project labels, so `docker compose down` doesn't remove it — operator-visible leftover after stack teardown.
- The recipe (image, env, name, capabilities) duplicates between Compose declaration intent and actual `docker create` call.

F-45 collapses that: the **network-anchor** has the recipe and does the create itself when a matching target appears.

### Requirement

Add an opt-in "managed service-anchor recipe" to the network-anchor. Activation:

- **`ANCHORD_MANAGED_SA_TARGET`** (optional): target container's stable name. If set, F-45 is active. If unset, network-anchor behaves exactly as F-43 today (only starts existing Created-state siblings).
- **`ANCHORD_MANAGED_SA_NAME`** (optional, default `<TARGET>-service-anchor`): name of the service-anchor container the network-anchor will create.
- **`ANCHORD_MANAGED_SA_IMAGE`** (optional, default: the network-anchor's own image as read from its own container inspect): image to use for the service-anchor.
- **`ANCHORD_MANAGED_SA_GATEWAY_IP`** (optional, defaults to the network-anchor's own IP on the shared network as detected by F-44): value to pass as `ANCHORD_GATEWAY_IP` to the spawned service-anchor.
- **`ANCHORD_MANAGED_SA_EXTRA_ENV`** (optional, JSON map): additional env vars to inject. Empty by default.

The set MUST be opt-in: zero new variables changed → F-45 inactive, F-43 unchanged behaviour. Single env var `ANCHORD_MANAGED_SA_TARGET` set → F-45 active with defaults for everything else.

Permissions: the network-anchor MUST have `POST` access to its Docker socket proxy (same `POST=1` enabling F-43 needs for `containers/start`). F-45 additionally calls `POST containers/create`. tecnativa/docker-socket-proxy's `POST=1` covers both endpoints — no new permission required.

### Behaviour (verbal, no code)

1. **Startup**: parse the env vars. If `ANCHORD_MANAGED_SA_TARGET` is unset → log nothing extra, skip F-45 logic entirely. If set → log a single line `"managed service-anchor recipe active: target=<X> name=<Y> image=<Z> gateway_ip=<W>"`.
2. **Event loop addition**: on every Docker `container start` event where the started container's name equals `ANCHORD_MANAGED_SA_TARGET` (or matches if we ever add label-based target selection — out of scope for F-45):
   - Check if the service-anchor (`ANCHORD_MANAGED_SA_NAME`) already exists:
     - Running: nothing to do. Log debug.
     - `Created` state with `network_mode: container:<target>`: this is the F-43 case. Issue `POST /containers/<name>/start` (existing F-43 code path).
     - Doesn't exist: **NEW F-45 path**. Issue `POST /containers/create?name=<name>` with:
       - `Image`: the configured image.
       - `HostConfig.NetworkMode`: `container:<TARGET>`.
       - `HostConfig.CapAdd`: `["NET_ADMIN"]`.
       - `HostConfig.RestartPolicy`: `{Name: "unless-stopped"}`.
       - `Env`: `["ANCHORD_MODE=service-anchor", "ANCHORD_GATEWAY_IP=<W>", "ANCHORD_LOG_LEVEL=info"]` + any extras from `ANCHORD_MANAGED_SA_EXTRA_ENV`.
       - `Labels`: `{"com.docker.compose.project": "<network-anchor's own compose project>", "anchord.managed-by": "<network-anchor's container ID>"}` — so `compose down` cleans it up.
     - Then `POST /containers/<id>/start`. Log `"created and started managed service-anchor: name=<X>"`.
3. **Startup-time backfill**: just like F-43 does for existing Created siblings. If on anchord-startup `ANCHORD_MANAGED_SA_TARGET` is set AND the target is already running AND the service-anchor doesn't exist (or exists in Created state) → execute the same create-or-start path. Handles the "anchord restarted while target was already up" case.
4. **Idempotency**: every step is safe to repeat. `docker create` with an existing name returns a clean error → log and continue. `docker start` on a running container returns 304 / no-op.
5. **Target-removal handling**: on `container die` for the target, anchord makes NO cleanup decisions. Docker's `network_mode: container:` semantics already kill the bound service-anchor when target dies; service-anchor's `restart: unless-stopped` then waits for target to come back, and the next `start` event re-triggers F-45.

### One-recipe-per-network-anchor

For simplicity, F-45 supports exactly ONE managed service-anchor recipe per network-anchor instance. The Authentik design has three network-anchors (one per identity); each manages at most one service-anchor:

- `anchord-authentik` (web): no service-anchor needed; Traefik is in front, symmetric.
- `anchord-ldap`: ONE recipe → `ldap-service-anchor` in netns of `ak-outpost-ldap`.
- `anchord-frigate`: no service-anchor needed; Traefik is in front.

If the operator ever needs an anchord that manages multiple service-anchors (e.g. anchord shared across two LDAP outposts), they can either run two network-anchors or fall back to the F-43 init-sidecar pattern. Out of scope for F-45.

### Backwards-compat

- Network-anchors without `ANCHORD_MANAGED_SA_TARGET`: zero behavioural change. F-43 auto-start of compose-declared service-anchors still works.
- Greenfield stacks (F-26): unaffected; no managed-SA env vars.
- Wrap stacks (F-39 + F-43): unaffected; service-anchors declared in Compose still work; the F-43 path still runs.
- Mixed: an anchord could in theory have an F-45 recipe AND F-43 Compose-declared siblings (different targets). Each is independent. Test this combination explicitly.

### Acceptance tests

Unit:
- `parseManagedSARecipe(env)` returns nil when no env vars set; returns a recipe struct with defaults filled in when only `TARGET` is set; rejects invalid env (`MANAGED_SA_EXTRA_ENV` not valid JSON).
- Recipe defaults: `NAME = TARGET + "-service-anchor"`, `IMAGE` = anchor's own image (looked up via Docker inspect on self), `GATEWAY_IP` = anchor's own IP on shared network.

Integration:
- Compose with one network-anchor configured for F-45 + an `ak-outpost-ldap` placeholder spawned 5 s after compose-up. Anchord MUST: detect spawn, create `ldap-service-anchor` in netns of outpost, start it. Verify with `docker exec ak-outpost-ldap ip route | grep default` shows the configured gateway.
- Restart the target (`docker restart ak-outpost-ldap`). Service-anchor gets killed by Docker (netns gone). After target re-starts, anchord re-triggers F-45 → service-anchor exists and is running again.
- Delete the target (`docker rm -f ak-outpost-ldap`). Spawn a new target with the same name. Anchord detects new start event, re-creates service-anchor (the old one was tied to the dead container's ID and is gone via Docker's lifecycle).
- `docker compose down` removes the managed service-anchor because of the `com.docker.compose.project` label F-45 sets.

End-to-end (Authentik scenario):
- ix-authentik compose: anchord-ldap declares `ANCHORD_MANAGED_SA_TARGET=ak-outpost-ldap`, `ANCHORD_MANAGED_SA_GATEWAY_IP=172.31.80.181`. No `ldap-sa-init` container needed, no manual docker create. After authentik_worker spawns ak-outpost-ldap (~30 s after compose-up), anchord-ldap creates the service-anchor inside its netns. Default route inside outpost netns reflects 172.31.80.181. OPNsense LDAP-bind to `192.168.150.81:636` works end-to-end with conntrack-reversed DNAT.

### Notes / current status

Triggered live by the Authentik migration 2026-05-19. The workaround (init-sidecar) was deployed and works, but it's the wrong primitive — Compose declaring an init-sidecar that pokes Docker API to bypass Compose is a strong "this should be one feature" smell.

Implementation estimate: 80–120 LOC plus tests.

- `internal/config`: new fields `ManagedSATarget`, `ManagedSAName`, `ManagedSAImage`, `ManagedSAGatewayIP`, `ManagedSAExtraEnv`. Parser, validation, default filler.
- `cmd/anchord` event-loop: extend the existing F-43 `container start` handler. The current F-43 path inspects siblings in `Created` state; F-45 path additionally creates them if they don't exist.
- Self-inspect helper (used to fill defaults for `MANAGED_SA_IMAGE` and `MANAGED_SA_GATEWAY_IP`): query Docker for anchord's own container, read image and IP-on-shared-network. Cache once at startup.
- New Docker API call: `POST /containers/create?name=<...>` with the recipe payload. tecnativa/docker-socket-proxy with `POST=1` already permits this.
- Tests: extend the F-43 integration harness with target-not-yet-existing-when-anchord-starts scenario.

After F-44 + F-45 land, the Authentik compose simplifies materially:

- Drop the `ldap-sa-init` Compose service (~30 lines).
- Drop the manual `transit_dp` workaround (F-44 picker handles multi-transit correctly).
- The compose ends up with: 3 anchords, 2 fe-anchors, 2 traefiks, 4 authentik-core, 1 docker-proxy = 12 services. Cleaner.

## Cross-references

- [SPEC.md §2.6](SPEC.md) — service-anchor contract.
- [SPEC-WRAP-DRAFT.md F-39](SPEC-WRAP-DRAFT.md) — wrap-pattern service-anchor declared in Compose via `network_mode: container:`.
- [SPEC-DEFERRED-ANCHOR-DRAFT.md F-43](SPEC-DEFERRED-ANCHOR-DRAFT.md) — autostart Created-state siblings. F-45 extends to "create + start" when sibling doesn't exist yet.
- [SPEC-LABEL-SELECTOR-DRAFT.md F-42](SPEC-LABEL-SELECTOR-DRAFT.md) — label-selector pattern; F-45's `ANCHORD_MANAGED_SA_TARGET` could in a future extension accept a selector instead of a literal name. Out of scope here.
- [SPEC-SHARED-NETWORK-PICKER-DRAFT.md F-44](SPEC-SHARED-NETWORK-PICKER-DRAFT.md) — sibling spec for the multi-transit picker issue surfaced in the same migration.
- The triggering migration: Authentik on TrueNAS, three anchord-instances, one of which manages a worker-spawned outpost (LDAP). See [lammers-krueger-firewall/projects/10g-dmz-migration/state.md](../../lammers-krueger-firewall/projects/10g-dmz-migration/state.md), 2026-05-19 entry.
