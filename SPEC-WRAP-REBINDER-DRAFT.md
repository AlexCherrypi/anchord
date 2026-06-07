# anchord — Spec Delta: Wrap-anchor netns-target auto-rebind (F-49)

> **Status:** Draft. Sibling to F-48 (external-network follower
> auto-rebind, issue #12); the two features were triggered by the same
> 2026-06-07 operator incident at adjacent layers — F-48 handles
> stale-bridge-network references, F-49 handles stale-netns-target
> references.
>
> **Motivation:** Wrap-pattern anchors use
> `network_mode: container:<X>` to share their target's netns. Docker
> resolves `<X>` once at start time and hard-pins the anchor to that
> exact container ID. If the target is later recreated by an external
> actor (`compose down/up`, auto-updater, `midclt app.stop/start`), it
> gets a new container ID. The anchor's netns reference is now stale.
> The anchor process keeps running but has zero connectivity. The
> wrap-stack's DMZ identity appears dead on Layer 3.
>
> **Affected user code:** new `ANCHORD_MODE=wrap-rebinder`. Single
> binary, parallel to `external-rebinder` (F-48). Existing modes are
> unaffected. Adopters add a sidecar container to the wrap-stack's
> compose — zero env-vars beyond the mode selector, the sidecar
> auto-discovers wrap-anchors in its own compose project.

## F-49 — Wrap-anchor netns-target auto-rebind

### Problem

Concrete real-world failure on 2026-06-07 (same incident chain as #12):

- **Target stack**: Mailcow (compose project `ix-mailcow`). Backend
  containers `nginx-mailcow`, `dovecot-mailcow`, `postfix-mailcow`.
- **Wrap stack**: `ix-mailcow-anchord-wrap`. Three anchors, each with
  `network_mode: container:<backend-name>`:
  ```yaml
  nginx-anchor:    network_mode: "container:nginx-mailcow"
  dovecot-anchor:  network_mode: "container:dovecot-mailcow"
  postfix-anchor:  network_mode: "container:postfix-mailcow"
  ```
- **Trigger**: per resolution of #12, the operator ran `midclt
  app.stop mailcow` + `app.start mailcow`. Mailcow's backend
  containers got new IDs.
- **State after**: each `*-anchor.HostConfig.NetworkMode` still held
  the *previous* ID:
  ```
  ix-mailcow-anchord-wrap-dovecot-anchor-1  →  container:2a7efbe7cb1f… (dead)
  ix-mailcow-anchord-wrap-postfix-anchor-1  →  container:41d43802d884… (dead)
  ix-mailcow-anchord-wrap-nginx-anchor-1    →  container:e22bab316f37… (dead)
  ```
- **Symptom**: anchors `Up` in `docker ps`, no L3 connectivity. DMZ
  IP `.150.100` ARP'd from the LAN but every connect timed out. From
  the public internet, mail ports (IMAP/SMTPS/submission) dead.
- **Manual recovery**: `midclt app.stop/start mailcow-anchord-wrap`.
  Each anchor's `network_mode: container:<name>` re-resolved on start
  to the current backend ID. All ports back within ~30s.

The pattern: any wrap-stack with `network_mode: container:<X>`
anchors is vulnerable to silent dangle on the target stack's
recreate. Affects every anchord wrap-pattern deployment, not just
Mailcow.

### Requirement

Add a fourth mode to the anchord binary: `ANCHORD_MODE=wrap-rebinder`.

The wrap-rebinder is a sidecar in the **wrap-stack's** compose
project. It auto-discovers every wrap-anchor in its own project via
their `HostConfig.NetworkMode` (`container:<X>`) declarations, watches
for target-container recreates, and **recreates** the affected anchor
with a freshly-resolvable NetworkMode pointing at the target's name.

> **Critical implementation note (verified 2026-06-07):** `docker
> restart` does **not** re-resolve `network_mode: container:<X>`.
> Docker resolves the reference at container *creation* time and
> stores the resolved long-ID in `HostConfig.NetworkMode`. A
> subsequent restart against a dead target ID fails with
> `joining network namespace of container: No such container`, and
> the anchor lands in `exited`. The only working recovery is to
> remove the anchor and re-create it with a fresh NetworkMode
> string — the same `RecreateWithNetworkMode` primitive
> `internal/autostart` uses for F-45 wrap-dep rebinding (issue #10).
> The issue-body original proposal assumed restart-only would work;
> empirical verification ruled that out.

**Activation**:

- **`ANCHORD_MODE=wrap-rebinder`** — required.
- **`COMPOSE_PROJECT_NAME`** — supplied by Compose automatically;
  scopes the auto-discovery to the sidecar's own project.

No other required env vars. No per-anchor JSON list. No labels. The
sidecar reads `HostConfig.NetworkMode` from every sibling in its own
project — that's already where the operator declared their targets.

Optional knobs (defaults sized so most operators never set them):

- **`ANCHORD_WRAP_POLL_INTERVAL`** (default `30s`): safety-net
  resync cadence on top of the event stream. Catches missed events
  during sidecar restarts/socket flaps.
- **`ANCHORD_WRAP_RESTART_TIMEOUT`** (default `10s`): timeout passed
  to `docker restart` for each wrap-anchor.

**Permissions**: needs `POST` on `containers/<id>/restart` and read
on `containers/json` + `containers/<id>/json`. The `:ro` socket-proxy
default isn't sufficient — same constraint as F-48. A `POST=1`
socket-proxy works; direct `docker.sock` mount works.

### Behaviour

1. **Startup config**: read `COMPOSE_PROJECT_NAME`. If empty, exit 1
   — the entire discovery model collapses without a project scope.
   Log a single line:
   `"wrap-rebinder starting: project=<P> poll_interval=<D>"`.

2. **Target-pool enumeration**:
   - List all containers (`ContainerList(All=true)`).
   - Filter to those carrying `com.docker.compose.project=<self>`
     (i.e. our own compose project).
   - Drop the sidecar itself (never restart ourselves).
   - For each remaining sibling, parse `HostConfig.NetworkMode`.
     Entries of shape `container:<NAME>` indicate a wrap-anchor; the
     `<NAME>` is its target. Entries of any other shape (`host`,
     `bridge`, `none`, `service:`, or empty) are skipped.
   - Build a map `target_name → list[wrap_anchor_id]`. A single
     target may have several wrap-anchors (uncommon but legal); the
     map handles that without special-casing.

3. **Bootstrap recheck (mirrors F-48's Anmerkung 1)**: for each
   `(target_name, [anchors…])` pair, inspect the live target by
   name and compare its current container ID against each anchor's
   stored `HostConfig.NetworkMode` reference. On drift → restart
   the affected anchor. Closes the race where the sidecar itself
   was down through the target's recreate window.

4. **Event subscription**: subscribe to `docker events --filter
   type=container --filter event=start`. **No name filter at the
   daemon** — Docker's filter API is OR-ed for the same key and we
   need to match a dynamic set of names. Filtering happens in the
   consume layer.

5. **On `container start` event** (Anmerkung 1 from #13 review):
   - Look up `event.actor.attributes.name` in the target-pool map.
     If present → at least one of our wrap-anchors targets this
     container; restart each of them. Docker re-resolves
     `network_mode: container:<name>` to the current ID on each
     restart.
   - **Plus** (Anmerkung 2 from #13 review): if the event's
     `com.docker.compose.project` attribute equals our own project,
     the event belongs to one of our siblings. Re-enumerate the
     target-pool — the sibling that just started may be a freshly-
     recreated wrap-anchor whose own ID we no longer know about (so
     a future name-lookup wouldn't find it in our restart list).
     Without this re-enumeration the sidecar holds stale
     wrap-anchor IDs after every `compose up --force-recreate` of
     the wrap-stack itself.
   - All other `start` events: silently ignored. Filtering in-
     process is the only way to support the dynamic name-set
     reliably.

6. **Recreate semantics**: inspect the anchor for its full
   Config/HostConfig, remove it (force=true, timeout
   `ANCHORD_WRAP_RESTART_TIMEOUT`), re-create with the same name and
   HostConfig **but with `NetworkMode = container:<target-name>`**
   (the live name, not the new long-ID — so a future drift cycle
   re-resolves cleanly again), then start. Failure logged at warn,
   sidecar continues — next event or the periodic poll retries.
   Docker's own `restart: unless-stopped` policy on the recreated
   wrap-anchor catches the rest.

7. **Periodic poll**: every `ANCHORD_WRAP_POLL_INTERVAL`, re-run
   steps 2 + 3 (re-enumerate target-pool, recheck for drift). Same
   safety-net pattern the discovery package uses against missed
   events.

8. **Event-stream resilience**: on terminal event-stream error,
   2s back-off, resubscribe — same pattern F-48 and discovery use.

### Race window: target destroy → create

When `compose up --force-recreate` cycles the target, Docker tears
down the target container, then brings it back up. During the gap
the wrap-anchor is bound to a destroyed netns; its ports are
unreachable. The sidecar does NOT try to bridge this gap:

- It does not pause the wrap-anchor (anchord doesn't manage
  lifecycle outside the recreate call).
- It does not recreate on `destroy` — that would race with the
  pending re-create and Docker would reject the wrap-anchor's
  start (`network_mode: container:<name>` resolves to nothing
  during the gap, exactly what the issue body flagged as Q5).
- It recreates on `start` — by which point the new target exists
  and `network_mode: container:<name>` resolves.

The downside: the wrap-stack is unreachable for the few seconds
between target destroy and the sidecar's recreate of the anchor.
Acceptable — operator-driven recreates already imply transient
downtime; the fix is "back in seconds without manual intervention",
not "zero-downtime".

### One sidecar = one compose project

Unlike F-48 (one sidecar = one network-target pair), F-49's sidecar
covers **every** wrap-anchor in its own compose project. Discovery is
automatic via `HostConfig.NetworkMode`. The Mailcow case (3 anchors,
3 targets) needs exactly one sidecar — same minimal deployment
footprint regardless of anchor count.

A wrap-stack with no `network_mode: container:*` siblings produces
an empty target-pool. The sidecar logs `"target pool is empty;
nothing to track"` and waits in the event loop in case the operator
adds anchors later (the periodic poll picks them up). Not an error.

### Cross-project targets

The target named in `network_mode: container:<X>` is not required to
live in the same compose project as the wrap-anchor — the Mailcow
case is exactly cross-project (wrap-stack `mailcow-anchord-wrap`,
targets in `ix-mailcow`). The sidecar resolves target names against
*all* containers on the host, not just its own project. This is the
common case, not an edge.

### Required deployment shape

The wrap-stack's compose project grows one new service:

```yaml
services:
  # ... existing wrap-anchor declarations ...
  rebinder:
    image: ghcr.io/alexcherrypi/anchord:v1.3.0
    environment:
      ANCHORD_MODE: wrap-rebinder
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    restart: unless-stopped
```

`COMPOSE_PROJECT_NAME` is set automatically by Compose at runtime,
so the sidecar's discovery scope is already correct. No
configuration beyond mounting the docker socket.

### Idempotency and edge cases

- Calling `docker restart` on a container already mid-restart returns
  success at the API layer; the daemon serialises.
- The same `(target, anchor)` recreate-pair may trigger multiple
  restart calls under high event load (e.g. when both bootstrap-
  recheck and a `start` event for the same target fire close
  together). Idempotent at the daemon, but the sidecar deduplicates
  in a small window using a per-anchor "last restarted at" timestamp
  — restarts within `<1s` of a prior restart on the same anchor are
  dropped at warn-level (`"suppressing rapid restart"`). Prevents
  thrash if the target itself is in a tight crash-loop and emits
  back-to-back `start` events.

### Backwards-compat

- Existing modes (network-anchor, service-anchor, doctor,
  external-rebinder) are unaffected. Mode dispatch in
  `cmd/anchord/main.go` grows one case.
- The optional env vars (`ANCHORD_WRAP_POLL_INTERVAL`,
  `ANCHORD_WRAP_RESTART_TIMEOUT`) are scoped to this mode.
- Existing wrap-stacks (anchord `network_mode: container:*`
  patterns from F-39/F-40) continue to work without the sidecar.
  Adoption is purely additive.

### Acceptance tests

**Unit** (in `internal/wraprebinder/wraprebinder_test.go`):

- `enumerateTargetPool(containers, selfProject)` returns a
  `map[targetName][]anchorID` populated only from containers in
  the self project whose NetworkMode parses as `container:<X>`.
  Excludes the sidecar's own ID (matched by self-hostname).
- `bootstrapRecheck` triggers restart on stale references, no-ops
  on matching ones, handles missing-target gracefully (target
  not yet started — empty restart list, no error).
- Event filtering: `start` events for names in the target-pool
  trigger restart of mapped anchors; `start` events for unrelated
  names are dropped; `start` events for siblings in the same
  compose project trigger re-enumeration of the pool (Anmerkung
  2).
- Rapid-restart suppression: two `start` events for the same
  anchor within 1s → only the first triggers an API call.
- Event-stream errors / closed channels return to caller (so Run
  can resubscribe).

**Integration** (in `wraprebinder_integration_test.go`, build tag
`integration`):

- Create a `target` container, a `wrap-anchor` with `network_mode:
  container:target`, and a sidecar Watcher. Recreate `target` (with
  a different ID, same name). After running the bootstrap-recheck
  path against a real Docker daemon, the wrap-anchor must have a
  new container creation time / new container ID (proof of
  restart).
- Idempotency: a second bootstrap-recheck against a now-current
  wrap-anchor must not restart it.

**End-to-end** (production, deferred to verification window):

- Deploy v1.3.0 sidecar to `mailcow-anchord-wrap`. Wait for next
  Mailcow auto-update cycle (Sa 04:00). Verify all three
  wrap-anchors come back up cleanly without manual intervention
  and DMZ ports stay reachable within ≤ 30 s of target restart.

### Open implementation questions

- **`docker pause`-vs-`docker restart`**: in principle we could
  `pause` the wrap-anchor during the target's destroy/create gap
  to suppress healthcheck noise. Deferred — adds lifecycle
  complexity for a marginal noise reduction, and the operator-
  visible "few seconds of unreachability" is honest signal,
  not noise to hide.
- **Watch service-anchor mode containers separately**: a wrap-
  anchor in `ANCHORD_MODE=service-anchor` (the F-40 pattern)
  also resolves `network_mode: container:<X>` at start. F-49's
  discovery doesn't distinguish — anything with
  `network_mode: container:*` in the self project counts. That's
  the right call: the rebind action is the same regardless of
  what the anchor's process does once it has the netns.
- **Coordinate with F-48's external-rebinder when both run in the
  same compose project**: in theory the wrap-stack could also
  have F-48 sidecars (one wrap-anchor that also follows an
  external network). The two modes don't conflict — they watch
  different event types — but two `docker.sock` mounts in one
  project is mildly redundant. Not worth merging the modes today;
  revisit if anyone actually deploys both in the same project.

### Notes / current status

Triggered live by the 2026-06-07 incident chain. F-48 fixed the
bridge-network half; F-49 fixes the netns half. The two together
cover the full operator-pain class of "a peer stack recreated
itself and I'm now silently broken."

Implementation estimate: 250–350 LOC plus tests. Closely mirrors
F-48's structure:

- `internal/config`: `WrapRebinder` struct + `LoadWrapRebinder()`.
- `internal/wraprebinder`: package with `Watcher`, narrow Docker ops
  interface for tests, target-pool map, event loop with name-based
  filtering, periodic poll, rapid-restart suppression.
- `cmd/anchord/main.go`: new `ModeWrapRebinder = "wrap-rebinder"`
  case, `runWrapRebinder(ctx)` dispatch.
- Tests: full unit coverage with fake ops; integration test against
  real Docker for the headline scenario.
