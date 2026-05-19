# anchord — Spec Delta: Deferred service-anchor via `setns` (F-43)

> **Status:** Draft. Captures one new feature (F-43) that emerged from the Authentik DMZ migration planning on 2026-05-19. anchord's existing service-anchor mode (F-26, expanded by F-39) relies on Docker's `network_mode: container:<target>` to do the netns binding at container-create time. Empirically verified the same day: Docker accepts `docker create --network container:NONEXISTENT` (container lands in `Created` state) but rejects `docker start` until the target exists, and does **not** auto-retry on its own (`restart: always` only triggers after a successful first start). That blocks the wrap pattern for targets that are spawned *at runtime* by another container — the concrete blocker is Authentik's worker, which calls Docker API directly to spawn outpost containers, with no Compose-level dependency we could hang `depends_on` on.
>
> **Motivation:** Give the operator a way to attach a service-anchor to a target container whose lifecycle isn't Compose-bound, without:
> 1. fragile "Created"-state placeholder containers,
> 2. modifying the target's image / start script,
> 3. requiring the operator to manage the start/stop ordering by hand.
>
> The concrete trigger is the Authentik design (one DMZ identity per outpost), but the feature is general: any time a backend container is spawned out-of-band (by a worker, by k8s, by an operator's CI), anchord's service-anchor mode should be able to attach lazily.
>
> **Affected user code:** new optional env vars on service-anchor, new lifecycle (event-watch + re-attach), new mounted volume (`/var/run/docker/netns:ro`). The greenfield (F-26) and wrap (F-39 via Docker's `network_mode: container:`) paths stay unchanged.

## F-43 — Deferred service-anchor (`ANCHORD_TARGET_LABEL` / `ANCHORD_TARGET_CONTAINER`)

### Problem

Service-anchor currently has two activation paths:

- **Greenfield (F-26):** app container does `network_mode: service:<service-anchor>`. The anchor owns the netns; app shares it. service-anchor starts first, installs the route, app inherits it on start.
- **Wrap (F-39):** service-anchor does `network_mode: container:<backend>`. Docker binds the anchor to the backend's pre-existing netns at create-time. Requires backend container to exist and be reachable by name *before* `docker create` is called on the anchor.

When the backend is spawned at runtime by a third party (Authentik-worker via Docker API, a Kubernetes operator, etc.), neither path applies:

- Greenfield is impossible — we don't write the spawning code, we can't insert `network_mode: service:…` into the spawn parameters.
- Wrap is half-impossible — Docker accepts the create, refuses the start, and doesn't retry. We verified on 2026-05-19 with a synthetic test:

  ```
  $ docker create --restart=always --name sa-test --network container:tgt-test alpine sleep 300
  <id-returned>                                                       # create succeeds
  $ docker start sa-test
  Error response from daemon: joining network namespace of container: \
    No such container: tgt-test                                       # start fails
  # 20s later, tgt-test now exists. sa-test stays in 'Created'.       # no auto-retry
  $ docker start sa-test                                              # manual retry: works
  ```

  `restart: always` only re-launches a container that previously started successfully — it doesn't poll-retry an initial failure. So we'd need an external watchdog to detect the target's appearance and trigger the start, which is what F-43 Lite (a variant evaluated and rejected during design) would have done.

The cleaner design is to **lift the netns binding out of Docker** and into anchord itself.

### Requirement

A new service-anchor activation path: instead of relying on `network_mode: container:<x>`, the anchor's process opens the target's netns directly via Linux `setns(CLONE_NEWNET)` after the target appears. The anchor container itself runs with default Docker networking; the netns join is a runtime operation, not a Compose-time binding.

Activation:
- **`ANCHORD_TARGET_LABEL`** (preferred): the same `key=value` syntax as F-42, AND-joined. anchord uses Docker API to find the container whose labels match the selector. If more than one container matches: fail-fast with a fatal log naming the candidates — operator must tighten the selector.
- **`ANCHORD_TARGET_CONTAINER`** (alternative, by literal name): for cases where the operator wants to pin to a known stable name (e.g. `ak-outpost-ldap`, the authentik default).
- Setting both → fatal startup error. Pick one.

When either env is set, anchord starts in **deferred service-anchor mode**:
1. Existing `ANCHORD_MODE=service-anchor` plus one of the target-resolution envs.
2. Mode is mutually exclusive with `network_mode: container:` in compose (operator must omit it; in deferred mode the anchor's own default netns is fine).

### Behaviour (verbal, no code)

#### 1. Startup

- Read target-resolution config. If neither `ANCHORD_TARGET_LABEL` nor `ANCHORD_TARGET_CONTAINER` is set and `network_mode: container:` wasn't applied by Docker (= no `/proc/self/ns/net` mismatch detectable via comparison with `/proc/1/ns/net` of the container?) → fatal: "service-anchor mode requires either network_mode: container: or ANCHORD_TARGET_*"; this is a config check, not a runtime guess.
- Open Docker API via `DOCKER_HOST` (same env var as the network-anchor uses — `tcp://docker-proxy:2375` in standard compose).
- **Lock the dispatching goroutine to its OS thread** (`runtime.LockOSThread`) — `setns(CLONE_NEWNET)` only switches the current thread's netns. All subsequent netns-affected syscalls (route programming, etc.) MUST happen on this same thread. Standard CNI-plugin pattern.
- Save the anchor's own original netns FD (open `/proc/self/ns/net`) — used to switch back temporarily for outbound Docker API calls (the netns we'll enter is the target's, which usually has no DNS upstream).

#### 2. Target discovery

- Initial scan: list containers via Docker API, filter by `ANCHORD_TARGET_LABEL` or name. If found → proceed to step 3. If not found → enter wait loop.
- Wait loop:
  - Subscribe to Docker events stream, filter `event=start` `type=container`.
  - Also do a periodic re-poll every 30 s as belt-and-suspenders against missed events.
  - On each candidate: inspect labels (or compare name), match against selector, proceed when matched.
- Log every 60 s while waiting: `"deferred service-anchor: waiting for target {label or name}"`.

#### 3. Joining the target netns

- Get target's container info via Docker API. The relevant field is `NetworkSettings.SandboxKey`, which is the path on the host to the target's netns file (typically `/var/run/docker/netns/<hash>`).
- Mount expectation (compose-level): the operator binds `/var/run/docker/netns:/var/run/docker/netns:ro` into the anchor container. **Spec MUST document this**. Without it, the SandboxKey path isn't reachable from inside the anchor container.
- `open(SandboxKey, O_RDONLY)` → netns FD.
- `setns(fd, CLONE_NEWNET)` → current thread is now in the target's netns. Close the netns FD (kernel keeps the reference via the thread's namespace).
- From this point: any netlink operation on the locked OS thread targets the target's netns, not the anchor's.

#### 4. Recording target's original default route

- In target netns: read the current default route (v4 + v6) via netlink. Store in memory.
- This is the route to restore at shutdown / target-restart. Identical to F-39's "record" step, just executed from inside the target's netns rather than from a Docker-bound netns.

#### 5. Installing anchord's default route

- Resolve `ANCHORD_GATEWAY_IP` (F-40) or `ANCHORD_GATEWAY_HOSTNAME` (F-24 DNS path).
- Switch back to the anchor's own netns (use the FD saved in step 1) — to do DNS or HTTPS calls that need the anchor's own DNS resolver. Do the resolution, get the IP. Switch back to target's netns.
- `ip route replace default via <gw>` (v4) and analogous v6 — via netlink, inside target netns.
- Log: `"deferred service-anchor active: target={name} gateway={ip}"`.

#### 6. Watching for target restart

- Continue subscribing to Docker events. On `event=die` or `event=stop` for the target → log `"target {name} died, will re-attach when it returns"`. The default route we installed becomes stale (the netns is destroyed by Docker when the container exits).
- On `event=start` for the target → it's back with a new netns. Re-execute steps 3–5. Log `"re-attached to {name} new-pid={pid}"`.
- The anchor stays alive across target restarts. Its own restart policy (`unless-stopped`) handles anchor-process crashes.

#### 7. Graceful shutdown

- On SIGTERM:
  - If currently attached: switch into target netns, `ip route replace default via <recorded-original-gw>`. If target's netns is gone (target died first): nothing to restore, just exit.
  - Close all FDs, exit.
- This matches F-39's restore guarantee.

### Compose-level shape

```yaml
services:
  ldap-service-anchor:
    image: anchord:v2-labelsel  # any v2 image with F-43 support
    cap_add: [NET_ADMIN]        # for netlink in target netns
    environment:
      ANCHORD_MODE: service-anchor
      ANCHORD_TARGET_LABEL: anchord.role=ldap-outpost
      ANCHORD_GATEWAY_IP: 172.31.1.2
      DOCKER_HOST: tcp://docker-proxy:2375
    volumes:
      - /var/run/docker/netns:/var/run/docker/netns:ro
    networks: [transit]         # anchor's own egress for Docker API + DNS
    depends_on: [docker-proxy]
    restart: unless-stopped
    # NO network_mode: container: — that's F-43's whole point.
```

Note: **No `pid: host`, no `CAP_SYS_ADMIN`** needed. `setns(CLONE_NEWNET)` requires `CAP_SYS_ADMIN` only when entering a netns *owned by a different user namespace*. Within a single user namespace (the default — Docker rarely uses user-namespace remapping), `CAP_NET_ADMIN` plus mount access to the SandboxKey is sufficient. Verify this empirically during implementation — if the kernel does demand `CAP_SYS_ADMIN` on the host's user namespace, fall back to declaring it; document the requirement.

### Lifecycle invariants

1. **One deferred service-anchor per target.** anchord doesn't enforce this; two service-anchors competing for the same target's default route will clobber. Operator contract.
2. **Target restart preserves anchor.** The anchor process survives target restarts; its restart policy only handles anchor-internal failures.
3. **Anchor exit restores target.** If anchor exits cleanly (SIGTERM): target's original default route is restored. If anchor crashes hard (SIGKILL): target keeps the stale anchord route until next target-restart. Operator can recover by `docker restart <target>` (Docker will recreate the netns; target's image-default route comes back).
4. **Target exit is fine.** When target dies, anchord just waits for it to come back. No cleanup needed for the now-destroyed netns.

### Backwards-compat

- F-26 greenfield (`network_mode: service:<anchor>`) unchanged.
- F-39 wrap (`network_mode: container:<x>`) unchanged. Operators who can use it should — it's slightly simpler since lifecycle is Docker-managed end-to-end.
- F-43 is opt-in via the new env vars; absent them, service-anchor behaviour is exactly as today.
- F-40 `ANCHORD_GATEWAY_IP` / DNS path reused as the route source — no separate spec touching.

### Security considerations

- Mounting `/var/run/docker/netns:ro` lets the anchor read netns FDs of any container on the host. Combined with `CAP_NET_ADMIN`, the anchor could in theory enter *any* container's netns. This is the same trust level as the existing service-anchor (which can join any container via Docker's `network_mode:container:`).
- We do **not** mount the Docker socket directly; we go via docker-socket-proxy with read-only privileges (CONTAINERS, EVENTS, NETWORKS, INFO — same as the network-anchor needs today).
- The `ANCHORD_TARGET_LABEL` filter is enforced *inside* anchord, not by Docker. A buggy anchord or operator misconfig could pick the wrong target; mitigation is the "match must be unique" requirement (fail-fast on multiple candidates) plus startup logging.
- No new sudo/setuid paths. All capabilities are explicit in compose.

### Acceptance tests

Unit:
- `parseTargetSpec("LABEL=anchord.role=ldap")` → `LabelSelector{anchord.role: ldap}`.
- `parseTargetSpec("CONTAINER=ak-outpost-ldap")` → `ContainerName: "ak-outpost-ldap"`.
- Both set simultaneously → fatal error: "set exactly one of ANCHORD_TARGET_LABEL, ANCHORD_TARGET_CONTAINER".
- Neither set and `ANCHORD_MODE=service-anchor` → behave as today (F-26/F-39 path), no F-43 logic engaged.

Integration:
- Boot anchord in deferred mode with `ANCHORD_TARGET_CONTAINER=tgt`. No `tgt` exists. Anchor logs "waiting for target tgt", goes idle. Within 60 s: `docker run -d --name tgt --network shared-bridge alpine sleep 300`. Within 10 s of `tgt` starting: anchor logs "deferred service-anchor active: target=tgt", default route inside `tgt`'s netns now points to the configured gateway (verified via `nsenter -t <tgt-pid> -n ip route`).
- Restart `tgt`: `docker restart tgt`. Anchor detects die+start, re-attaches. Default route inside the new netns points to the gateway. Restart counts in anchord's logs increment.
- SIGTERM the anchor while attached: target's default route reverts to whatever it was before the anchor attached (recorded value). `nsenter -n` confirms.
- Label-selector match more than one container at startup: fail-fast with `"target selector matched 2 containers: tgt-a, tgt-b — refusing to attach ambiguously"`.

End-to-end (Authentik scenario):
- Compose with authentik_server + authentik_worker + LDAP-outpost-deferred-anchor in `ix-authentik`. Worker spawns `ak-outpost-ldap` via Docker API after DB ready (~30 s). Anchor (running already) detects the spawn, joins netns, installs `ANCHORD_GATEWAY_IP=<anchord-ldap-bridge-ip>`. OPNsense LDAP-bind to `.150.81:389` (DNAT'd by anchord-ldap to outpost's bridge IP) succeeds, with the **outpost's reply going through anchord-ldap, conntrack-reversing the DNAT cleanly** (verified by capturing reply on the anchor's macvlan interface).

### Notes / current status

Not yet implemented. Size estimate: 150–200 LOC plus tests:

- `internal/config`: new fields `TargetLabel`, `TargetContainer`, validation.
- `internal/serviceanchor` (or new sub-package `internal/deferredanchor`): the setns + event-loop logic. Uses `golang.org/x/sys/unix` for `Setns`. Uses existing Docker API client.
- New compose-level mount expectation (`/var/run/docker/netns:ro`); add to `ARCHITECTURE.md` and `compose.example-wrap.yaml` once the feature lands.
- Lifecycle tests in `internal/deferredanchor/*_test.go` using a Docker-in-Docker or socket-mock harness — match the style of existing serviceanchor tests.

The Authentik DMZ migration ([lammers-krueger-firewall/projects/10g-dmz-migration](../../lammers-krueger-firewall/projects/10g-dmz-migration/)) is gated on F-43 *for the full design* (own DMZ IP per outpost). A degraded Stage-1 design without F-43 is possible — only `authentik_server` moves to DMZ, outposts stay on the legacy `nginx_proxy` network — and would unblock the meshcentral OIDC path today.

## Implementation priority

Independent of other open Fs. Recommended sequence:

1. **F-43 implementation + unit tests** for setns + target resolution + event loop.
2. **Synthetic integration test** with two throwaway containers (alpine sleep) — verifies route install + restore + restart re-attach.
3. **Authentik dry-run on TrueNAS**: build the `ix-authentik` compose with three anchords + three deferred service-anchors, observe outpost spawn → anchor attach cycle.
4. **Authentik live migration** with full F-42 + F-43 — three DMZ identities.

## Cross-references

- [SPEC.md §2.6](SPEC.md) — current service-anchor contract (F-26).
- [SPEC-WRAP-DRAFT.md F-39](SPEC-WRAP-DRAFT.md) — wrap-pattern service-anchor via `network_mode: container:`.
- [SPEC-WRAP-DRAFT.md F-40](SPEC-WRAP-DRAFT.md) — `ANCHORD_GATEWAY_IP` (reused in F-43).
- [SPEC-LABEL-SELECTOR-DRAFT.md F-42](SPEC-LABEL-SELECTOR-DRAFT.md) — label-selector parsing/semantics (reused in F-43 for target resolution).
- The triggering migration: Authentik on TrueNAS, with worker-spawned outposts wanting their own DMZ identities. See [lammers-krueger-firewall/projects/10g-dmz-migration/decisions.md](../../lammers-krueger-firewall/projects/10g-dmz-migration/decisions.md), the planning entry for D-014 (gates on F-43).
- Empirical Docker-behaviour note from the 2026-05-19 verification:
  - `docker create --network container:NONEXISTENT alpine` → succeeds (container enters `Created` state).
  - `docker start` on it → fails with "joining network namespace of container: No such container".
  - `restart: always` does NOT trigger a retry — verified by waiting 20 s after the target appeared; the `Created`-state anchor stayed in `Created` until manually started. This rules out the "F-43 Lite" design (compose with `network_mode: container:` + restart-poll); F-43 must be the setns-based design.
