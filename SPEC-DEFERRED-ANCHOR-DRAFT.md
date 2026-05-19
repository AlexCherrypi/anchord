# anchord — Spec Delta: Sibling auto-start for deferred service-anchors (F-43)

> **Status:** Draft. Captures one new feature (F-43) that emerged from the Authentik DMZ migration planning on 2026-05-19. anchord's existing service-anchor mode (F-26, expanded by F-39) relies on Docker's `network_mode: container:<target>` to do the netns binding. Empirically verified the same day: Docker accepts `docker create --network container:NONEXISTENT` (container lands in `Created` state) but rejects `docker start` until the target exists, and does **not** auto-retry on its own (`restart: always` only triggers after a successful first start). Concrete blocker: Authentik-worker spawns outpost containers via Docker API at runtime, with no Compose-level hook for `depends_on` to wait on.
>
> **Motivation:** Let the operator declare a service-anchor with `network_mode: container:<target>` even when `<target>` doesn't yet exist at compose-up time, and have anchord transparently start the service-anchor once the target appears. Service-anchor remains a standard `network_mode: container:`-bound container (Docker keeps managing the netns binding); the only new behaviour is "watch Docker events, kick `start` on siblings in `Created` state whose `network_mode: container:X` resolves now". A trivial extension to the event loop the network-anchor already runs.
>
> **Affected user code:** new optional env var on the network-anchor; one new POST permission on docker-socket-proxy. No new container, no new mode, no setns. Greenfield (F-26) and standard wrap (F-39) paths stay unchanged.
>
> **History:** an earlier draft of this delta (commit `318fcc6`) specced a substantially larger "F-43 Full" variant — service-anchor with its own setns()-based netns entry, no `network_mode: container:` binding, ~150–200 LOC. Rejected on 2026-05-19 in favour of the Lite design captured below: smaller diff, reuses Docker's existing lifecycle plumbing, "Created"-state cosmetics deemed acceptable by the operator.

## F-43 — Auto-start `Created`-state service-anchors when target appears

### Problem

Service-anchor's "wrap" activation (F-39) puts the anchor's netns binding under Docker's control via `network_mode: container:<target>`. Docker resolves the target at `docker create` time… actually it doesn't — empirically verified 2026-05-19:

```
$ docker create --restart=always --name sa-test --network container:tgt-test alpine sleep 300
<id-returned>                                                       # CREATE: succeeds
$ docker start sa-test
Error response from daemon: joining network namespace of container: \
  No such container: tgt-test                                       # START:  fails
# 20s pass; tgt-test is now `docker run …`-spawned.
$ docker ps -a | grep sa-test
sa-test     Created                                                 # Docker does NOT auto-retry
$ docker start sa-test
sa-test                                                             # MANUAL retry: works
```

Docker's `restart: always` policy re-launches only containers that successfully started at least once. It does not poll-retry an initial-start failure. Without external help, a service-anchor whose target is spawned later will sit in `Created` state forever.

The minimal fix: have **the network-anchor** (which already watches Docker events for backend discovery) extend its event handler with one more concern — on each `container start` event, look for siblings in `Created` state whose declared `network_mode` points to the just-started container, and call `POST /containers/<sibling>/start`. Idempotent; if another anchord (or operator, or anything) already started the sibling, the second call returns 304 / no-op. The "Created" state appears briefly until the target spawns, then the anchor transitions to `Up` like any other container — including its restart policy taking over from there on.

### Requirement

Add to the network-anchor's Docker-events handler an additional step: **"start `Created`-state siblings whose `HostConfig.NetworkMode == container:<just-started-container-name>`"**. Gated behind one optional env var, defaulting to ON.

- **`ANCHORD_AUTOSTART_SIBLINGS`** (optional, default `true`). When set to `false`, anchord doesn't auto-start anything — the operator handles ordering manually. When `true` (or unset), every network-anchor in the project participates in the auto-start race.

Race semantics: if multiple network-anchors in the same Compose project (F-42 scenario) see the same `start` event, each will independently issue `POST /containers/<sibling>/start`. Docker handles the duplicate call idempotently. The log line acknowledges this — `"sibling already started"` is the expected outcome for the loser of the race; not an error.

### Behaviour (verbal, no code)

1. **Detection on existing event-watch loop.** anchord already subscribes to Docker events filtered by `type=container` for its discovery. When `ANCHORD_AUTOSTART_SIBLINGS=true`:
   - On every `container start` event (regardless of whether the event's container matches anchord's own selector — siblings of any flavour can need rescuing), capture the started container's name.
   - Query Docker API: list all containers in `Created` state. For each: read `HostConfig.NetworkMode`. If it equals `container:<name-of-just-started>` (or, internally, `container:<id>` — Docker normalises) → it's a sibling we should start now.
   - For each matched sibling: call `POST /containers/<sibling-id>/start`. Log the result.
2. **No new mode, no new container.** F-43 is purely additional logic in the network-anchor's existing event handler. Service-anchor containers remain standard `network_mode: container:`-bound — Docker manages their netns binding and lifecycle (including restarts) once the initial start succeeds.
3. **Idempotency.** The auto-start call is safe to repeat. Docker returns success (or "container already running") for a container already started. We don't track state across anchord-restarts; on anchord-startup, the initial scan covers any siblings that are still in `Created` state at that moment by looking at every container's `NetworkMode` and the live state of its referenced target.
4. **Startup-time backfill.** On anchord startup, before entering the event loop, do one pass over the Created-state containers in the project: for each, if its `network_mode: container:<X>` resolves to a currently-running container, start it. This handles the case where anchord is restarted while the cluster is mid-deploy and siblings are stranded.
5. **Logging.** On each auto-start attempt:
   - `"auto-starting sibling <name> waiting for target <target-name>"` on success.
   - `"sibling <name> already running"` on the 304-equivalent — informational, not warning.
   - `"sibling <name> failed to start: <docker-error>"` on actual failure — operator must intervene.

### Compose-level shape

```yaml
services:
  anchord:
    image: anchord:v2-labelsel
    cap_add: [NET_ADMIN]
    networks: [dmz, transit]
    environment:
      ANCHORD_PROJECT: ix-authentik
      ANCHORD_EXT_NETWORK: dmz_macvlan
      ANCHORD_LABEL_SELECTOR: anchord.role=ldap-outpost
      ANCHORD_AUTOSTART_SIBLINGS: "true"   # default; can omit
      DOCKER_HOST: tcp://docker-proxy:2375
    depends_on: [docker-proxy]
    restart: unless-stopped

  ldap-service-anchor:
    image: anchord:v2-labelsel
    cap_add: [NET_ADMIN]
    network_mode: "container:ak-outpost-ldap"   # target not yet running at compose-up
    environment:
      ANCHORD_MODE: service-anchor
      ANCHORD_GATEWAY_IP: 172.31.1.2
    restart: unless-stopped
    # No `depends_on: [ak-outpost-ldap]` — Compose doesn't know about
    # worker-spawned containers. anchord auto-starts this once the
    # target appears.

  docker-proxy:
    image: tecnativa/docker-socket-proxy:latest
    environment:
      CONTAINERS: "1"
      EVENTS: "1"
      INFO: "1"
      NETWORKS: "1"
      POST: "1"            # NEW — needed for containers/start
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks: [transit]
    restart: unless-stopped
```

### docker-socket-proxy permissions

Existing wrap stacks use the docker-socket-proxy with read-only flags (`CONTAINERS=1, EVENTS=1, INFO=1, NETWORKS=1`). F-43 requires the proxy to also forward `POST` requests so anchord can call `containers/<id>/start`. tecnativa's image gates write operations behind a single `POST=1` flag — there's no granular "only containers/start" option upstream. Operator decision:

- **Default for F-43 stacks:** enable `POST=1` on the docker-proxy. This exposes start/stop/exec/etc. through the proxy, but only to the anchord container (which is the only one talking to the proxy). Same trust level we already grant the network-anchor.
- **More restrictive path (operator opt-in):** stand up a second docker-socket-proxy instance with only `POST=1` for the auto-start path, fronted by an HTTP filter (nginx + Lua, or a tiny Go side-proxy) that allows only `POST /containers/{id}/start`. Out of scope for the spec; document as an optional hardening.

The spec must explicitly call out this permission widening so it doesn't surprise an operator copying compose-from-cookbook.

### Lifecycle invariants

1. **One auto-start per sibling.** anchord doesn't deduplicate across its own retries — Docker's idempotency handles repeats. If two anchords in the same project both race for the same sibling, both fire the API call; Docker accepts one and 304's the other. Operator-visible cost: one extra log line per race.
2. **`Created` state is transient — at most a few seconds.** The interval between target-start and anchord-detection is whatever the docker-events polling delay is plus the API round-trip. In practice well under 2 seconds. Operators monitoring with `docker ps` will see the `Created` state briefly during deploys, then `Up`. Document this so it's not mistaken for a stuck container.
3. **Target restart pulls service-anchor with it.** If the target dies, Docker also kills the `network_mode: container:`-bound service-anchor (it's a Docker invariant — netns owner dying = bound container is sigkilled). When target restarts, the service-anchor's `restart: always` (or `unless-stopped`) kicks in… but only if Docker considers it as having previously-started-successfully. Empirical question: does the post-target-death state record the anchor as having run? Almost certainly yes (it transitioned to `Up` at least once after the original auto-start). So the anchor restarts when target restarts, and Docker handles the second-and-onwards netns binding. F-43's job is only the very first start. Verify this in implementation testing.
4. **Anchor process exits → state lost.** If anchord-itself restarts mid-deploy, the auto-start might miss the start event for the target. The startup-time backfill (item 4 in Behaviour) covers this: on anchord boot, scan for stranded Created-state siblings and start them.

### Backwards-compat

- Stacks without `ANCHORD_AUTOSTART_SIBLINGS=true` (i.e. with explicit `false`): exact current behaviour.
- Default `true`: in greenfield and wrap stacks where no container is in `Created` state waiting on another, the additional event-handler code path simply finds zero matches and does nothing — no behaviour change.
- The new `POST=1` on docker-socket-proxy: operator must opt in by updating their compose. Existing compose files keep working with `POST=0`, in which case F-43's auto-start calls fail (logged) — exactly equivalent to the pre-F-43 state where the sibling stays in Created.

### Security considerations

- `POST=1` on docker-socket-proxy lets anchord (and only anchord, since the proxy is on the project-internal transit network) issue any Docker write API call. Trust model: anchord already has CAP_NET_ADMIN, manages nftables on the host kernel, and runs operator-trusted code. Promoting it to "can also start/stop sibling containers" is a strict superset of its existing power but not a meaningful escalation in practice.
- The operator can opt for the more-restrictive second-proxy variant documented in "docker-socket-proxy permissions" above.
- No new socket mounts, no new capabilities, no new namespaces.

### Acceptance tests

Unit:
- `parseAutostartSiblingsConfig(env)` — defaults to `true`, parses `"false"` correctly, rejects garbage.
- Sibling-matcher logic: given a list of containers with various NetworkModes (host, none, bridge, container:X), and a "just-started" container name X → matcher returns the right subset.

Integration:
- Boot a two-container compose: `anchord` (network-anchor) + `sa-test` (service-anchor with `network_mode: container:tgt-test`, `restart: unless-stopped`). `tgt-test` not present at compose-up. Verify `sa-test` is in `Created` state. Then `docker run -d --name tgt-test alpine sleep 300`. Within 5 s: `sa-test` should be `Up`. Verify anchord's log lines.
- Restart `tgt-test`: `docker restart tgt-test`. Docker kills `sa-test` (netns owner gone), `tgt-test` comes back, `restart: unless-stopped` on `sa-test` brings it back. F-43 logic should NOT fire (the start event is for `tgt-test`, but `sa-test` is in `restarting` state, not `Created`). Verify this empirically — important that we don't fight Docker's normal restart machinery.
- `ANCHORD_AUTOSTART_SIBLINGS=false`: same setup, `sa-test` stays in `Created` forever. anchord logs no auto-start activity.
- Two anchords in the project (F-42 scenario), both with `ANCHORD_AUTOSTART_SIBLINGS=true`. Target spawn → both attempt to start. One succeeds, one logs "already running". Both log lines emitted, no error.

End-to-end (Authentik scenario):
- Compose with authentik_server + authentik_worker + anchord-ldap (with F-42 selector `anchord.role=ldap-outpost`) + ldap-service-anchor (with `network_mode: container:ak-outpost-ldap`, in Created state).
- Worker spawns `ak-outpost-ldap` after DB ready. anchord-ldap detects start event, auto-starts ldap-service-anchor. ldap-service-anchor enters Up state, installs default route in `ak-outpost-ldap`'s netns. OPNsense LDAP-bind to `192.168.150.81:389` works end-to-end with symmetric routing (reply returns via anchord's macvlan).

### Notes / current status

Not yet implemented. Size estimate: **30–50 LOC plus tests**:

- `internal/config`: new bool `AutostartSiblings`, default true. Tiny.
- `cmd/anchord/main.go` (event handler): add the sibling-discovery + start-call. ~15 LOC.
- Startup backfill: scan once on boot. ~10 LOC.
- Tests: unit for matcher, integration for the auto-start cycle.

The Authentik DMZ migration ([lammers-krueger-firewall/projects/10g-dmz-migration](../../lammers-krueger-firewall/projects/10g-dmz-migration/)) is gated on F-43 + F-42 *for the full design* (own DMZ IP per outpost). A degraded Stage-1 design (only `authentik_server` moves, outposts stay on `nginx_proxy`) is possible without F-43 and would unblock the meshcentral OIDC path today.

## Implementation priority

Independent of other open Fs. Recommended sequence:

1. **F-43 implementation + unit tests** for matcher + auto-start.
2. **Synthetic integration test** with two throwaway alpine containers — verifies the Created→Up cycle on target spawn.
3. **Authentik dry-run on TrueNAS**: build the `ix-authentik` compose with three anchords + three service-anchors (Created at compose-up, Up after worker spawns outposts), observe.
4. **Authentik live migration** with F-42 + F-43 — three DMZ identities, all auto-managed by authentik for image versioning + token-mgmt, with anchord handling DMZ-IP plumbing.

## Cross-references

- [SPEC.md §2.6](SPEC.md) — current service-anchor contract (F-26).
- [SPEC-WRAP-DRAFT.md F-39](SPEC-WRAP-DRAFT.md) — wrap-pattern service-anchor via `network_mode: container:`. F-43 piggy-backs on F-39's compose shape, adding only the auto-start trigger.
- [SPEC-WRAP-DRAFT.md F-40](SPEC-WRAP-DRAFT.md) — `ANCHORD_GATEWAY_IP` reused by service-anchors in this pattern.
- [SPEC-LABEL-SELECTOR-DRAFT.md F-42](SPEC-LABEL-SELECTOR-DRAFT.md) — label-selector for backend discovery; F-43 is orthogonal but typically used together (F-42 for the network-anchor's selector, F-43 for its sibling-bootstrap behaviour).
- The triggering migration: Authentik on TrueNAS, with worker-spawned outposts wanting their own DMZ identities. See [lammers-krueger-firewall/projects/10g-dmz-migration/decisions.md](../../lammers-krueger-firewall/projects/10g-dmz-migration/decisions.md), the planning entry for D-014 (gates on F-42 + F-43).
- 2026-05-19 empirical Docker-behaviour verification (the create-succeeds / start-fails / restart-doesn't-retry sequence) recorded inline in the Problem section.
