# anchord — Spec Delta: External-network follower auto-rebind (F-48)

> **Status:** Draft. Captures one new feature (F-48) that emerged from a
> 2026-06-07 production incident: Mailcow's weekly auto-update recreated
> its compose bridge network with a new Docker ID, and
> `authentik-mailbox-sync` — an external follower attached to that
> bridge from a different compose project — was stranded against a stale
> network reference. Result: every IMAP login on every mailbox failed
> for ~24h until manually stopped+started.
>
> **Motivation:** This is the dual of the F-43/F-45 dead-netns problem,
> but one layer up. F-43/F-45 handle "the netns my sibling joined is
> gone." F-48 handles "the bridge network my sibling joined is gone."
> Both are recreate-side-effects on a peer stack that anchord-the-
> project doesn't own.
>
> **Affected user code:** new `ANCHORD_MODE=external-rebinder`. Single
> binary, new sub-command. Existing modes (network-anchor, service-
> anchor, doctor) are unaffected. Adopters add a sidecar container to
> the follower's compose; no changes needed on the target stack.

## F-48 — External-network follower auto-rebind

### Problem

Concrete real-world failure on 2026-06-07:

- **Target stack**: Mailcow (compose project `ix-mailcow`). Owns the
  bridge network `ix-mailcow_mailcow-network`.
- **Follower stack**: `authentik-mailbox-sync` — its own TrueNAS app,
  separate compose project. Joins Mailcow's bridge via
  `networks.mailcow.external: true` to reach `nginx-mailcow` for
  Dovecot HTTP-auth lookups.
- **Trigger**: Mailcow's weekly auto-update (Sa 04:00 cron) recreated
  the stack — `app.stop` + git-update + `app.start`. The bridge
  network was re-created in the process, picking up a new Docker
  network ID. The kernel bridge interface (`br-<id>`) was reaped and
  re-created under a new name.
- **Symptom**: `authentik-mailbox-sync` was technically attached to a
  network ID that no longer existed. `docker network inspect` still
  listed it as an attachee under the *new* network (Docker is
  forgiving), but the kernel had no bridge for that endpoint. Every
  outbound packet to `nginx-mailcow` got `Connection timed out` /
  `No route to host`.
- **Downstream blast**: Dovecot's auth proxy uses HTTP to the
  reconciler; the reconciler is the App-Password authority for every
  IMAP login. With the reconciler unreachable, IMAP auth failed for
  every mailbox for ~24h until the operator noticed.
- **Manual recovery**: `midclt app.stop authentik-mailbox-sync` +
  `app.start` (forces re-attach against the current network ID).
  Healthy thereafter.

The pattern is general: any compose stack that has external attachees
(`external: true` networks pointing at a sibling stack's bridge) is
vulnerable to silent dangle on the target's recreate. This includes
many real anchord deployments — e.g. authentik proxy outposts attached
to a downstream service's bridge.

### Requirement

Add a third mode to the anchord binary: `ANCHORD_MODE=external-rebinder`.

A rebinder sidecar runs in the **follower's** compose project (not the
target's). It watches Docker network events for a configured target
network name and re-attaches a configured follower container to the
current network ID whenever it changes. Single job, single binary.

**Activation (env vars on the sidecar container)**:

- **`ANCHORD_FOLLOW_NETWORK`** (required): the Docker network *name*
  the follower should remain attached to. Examples:
  `ix-mailcow_mailcow-network`, `myapp_default`. Matched by name on
  every event; the Docker ID is intentionally not stored in config
  because it's exactly what changes.
- **`ANCHORD_FOLLOW_TARGET`** (required): the follower container to
  rebind. Accepts either (a) a bare container *name* (matched against
  `Names` from `ContainerList`) or (b) a compose *service name* —
  resolved within the sidecar's own compose project (matched against
  `com.docker.compose.service=<X>` + `com.docker.compose.project=<self
  project>`). Service-name form is preferred because it survives
  compose-driven renames; bare-name form is the fallback for non-
  compose deployments.
- **`ANCHORD_FOLLOW_RESTART`** (optional, default `false`): when
  `true`, after a successful reattach, also issue `docker restart
  <follower>` to flush in-process connection pools / DNS caches.
  Default is conservative-off — most fresh-connect-per-request apps
  (Python `requests`, Go `http.Client` defaults, anything that
  re-resolves DNS per dial) don't need this and gain nothing from a
  restart. Turn it on for stacks with long-lived persistent pools
  (Java HikariCP, Go `http.Client` with `IdleConnTimeout` ≫ 30s,
  anything that caches peer IPs in-process). Restart is `docker
  restart` — preserves the container ID and config, just bounces the
  process.
- **`ANCHORD_FOLLOW_EVENT_BACKOFF`** (optional, default `2s`): grace
  period after a `network create` event before attempting reattach.
  Lets Docker finish wiring up the new bridge before we connect. Same
  shape as the discovery package's retry backoff.

**Permissions**: the rebinder needs `POST` access to the Docker
socket — `containers/<id>/network/{connect,disconnect}`,
`containers/<id>/restart`, and `network/<name>/{inspect}`. The
read-only socket-proxy default (used by network-anchor and service-
anchor today) is **not** sufficient. Operators wire the sidecar
against either a `POST=1` socket-proxy or directly against
`/var/run/docker.sock` (the simpler choice for a small sidecar that
runs as a system service).

### Behaviour

1. **Startup config**: parse env. `ANCHORD_FOLLOW_NETWORK` and
   `ANCHORD_FOLLOW_TARGET` are both required; missing or empty → exit
   1 with a clear message. Log a single line:
   `"external-rebinder starting: follow_network=<X> follow_target=<Y>
   restart=<bool>"`.

2. **Target resolution**: resolve the follower container ID once at
   startup.
   - If `ANCHORD_FOLLOW_TARGET` matches a `com.docker.compose.service`
     label in the sidecar's own project, that's the follower.
   - Else, if it matches a bare container name, that's the follower.
   - Else: log a warn and enter the event loop anyway — the follower
     may not yet exist (compose ordering, post-init creation). Re-
     resolve on every relevant event.

3. **Bootstrap recheck (Anmerkung 1)**: before subscribing to events,
   actively verify the follower's current attachment state.
   - `docker network inspect <ANCHORD_FOLLOW_NETWORK>` → current ID.
   - `docker inspect <follower>` →
     `.NetworkSettings.Networks[<name>].NetworkID`.
   - If the follower's stored network ID differs from the network's
     current ID (or the follower isn't attached at all), trigger the
     reattach path once. This closes the race where the rebinder
     itself was down during the target's recreate — the destroy/create
     events are already past, but the divergence is observable.
   - The bootstrap recheck is **non-blocking on errors**: if the
     network doesn't exist yet, or the follower doesn't exist yet,
     log a warn and proceed to the event loop. The first relevant
     event will re-trigger the path.

4. **Event subscription**: subscribe to `docker events --filter
   type=network` for actions `create`, `destroy`, `connect`,
   `disconnect`. Other action types are dropped at the consume layer.

5. **On `network destroy` for our network name**: log info. Take no
   action — destruction without a paired create just means the target
   stack is down. The follower will be unreachable for its peer
   anyway; Docker's `restart: unless-stopped` (on the follower) will
   loop it harmlessly until the target comes back.

6. **On `network create` for our network name**: after
   `ANCHORD_FOLLOW_EVENT_BACKOFF`, run the reattach path:
   - Re-resolve follower ID (in case the follower itself was recreated
     during the window).
   - `docker network disconnect <name> <follower>` — best-effort.
     If the follower isn't attached, Docker returns an error that we
     log at debug and ignore.
   - `docker network connect <new-id> <follower>` — connect to the
     CURRENT network ID, which by this point Docker has resolved by
     name internally.
   - If `ANCHORD_FOLLOW_RESTART=true`: `docker restart <follower>`
     after a successful connect.
   - Log info on success, warn on each failed sub-step. Failures don't
     exit the rebinder — the next event will retry.

7. **On `network connect/disconnect` events for our network and our
   follower**: log debug (visibility), take no action. These are
   typically the rebinder's own connect/disconnect calls echoing back
   through the event stream. Idempotency is the safety net.

8. **Event-stream resilience**: on terminal event-stream error
   (Docker daemon restart, socket flap), wait 2s and re-subscribe —
   same pattern `internal/discovery` and `internal/autostart` use.

### Race window

Between `network destroy` and the paired `network create`, the
follower has no valid attachment. The rebinder explicitly does NOT
pause, freeze, or kill the follower during this window. It relies on:

- The follower's own `restart: unless-stopped` policy to crash-loop
  it until the target comes back (consistent with how F-43/F-45
  handle dependents in a destroyed netns).
- Docker's idempotent connect API to make the reattach safe to retry.

This is a deliberate scope cap. anchord does not manage the
follower's lifecycle outside the reattach call.

### One sidecar = one (network, target) pair

For v1, an external-rebinder sidecar handles exactly one follower
attached to exactly one network. Need more pairs? Run more sidecars
(~10 MB each, the binary is already in the image). Reasoning: per-
pair sidecars stay debuggable from logs, restart-policies, and
permissions match a 1:1 mental model. Multiplexing in one process
adds config-shape complexity (label maps, JSON lists) that the v1
target pattern doesn't justify.

### Required deployment shape

The follower's compose project grows one new service:

```yaml
services:
  rebinder:
    image: ghcr.io/alexcherrypi/anchord:v1.3.0
    environment:
      ANCHORD_MODE: external-rebinder
      ANCHORD_FOLLOW_NETWORK: ix-mailcow_mailcow-network
      ANCHORD_FOLLOW_TARGET: sync           # compose service name in this project
      # ANCHORD_FOLLOW_RESTART: "true"      # opt-in; default false
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    restart: unless-stopped
```

The label-on-the-follower form sketched in the original issue body
(`anchord.follow-network: …`) is dropped — with the sidecar shape,
env-vars on the sidecar are the natural place for this config, and
labels-on-follower would be redundant.

### Idempotency and ordering guarantees

- `docker network connect <name> <id>` on an already-attached endpoint
  returns an error like `endpoint already exists`; the rebinder
  catches and logs at debug, treating it as success.
- `docker network disconnect <name> <id>` on a non-attached endpoint
  returns an error like `is not connected to network`; same handling.
- The event stream may deliver the same `network create` twice (e.g.
  when both `docker compose up` and a sibling stack's `--force-
  recreate` touch the same name in rapid succession). Reattach is
  safe to repeat.
- Order between bootstrap-recheck and first event is not guaranteed.
  Both code paths must be safe to run in either order.

### Backwards-compat

- Existing modes (network-anchor, service-anchor, doctor) are
  unaffected. The binary's mode-dispatch in `cmd/anchord/main.go`
  grows one more case.
- The `ANCHORD_FOLLOW_*` env vars are scoped to the new mode; the
  network-anchor's `config.LoadNetworkAnchor` and the service-
  anchor's `LoadServiceAnchor` neither read them nor reject them.

### Acceptance tests

**Unit** (in `internal/rebinder/rebinder_test.go`):

- `parseRebinderConfig(env)` rejects missing `ANCHORD_FOLLOW_NETWORK`,
  rejects missing `ANCHORD_FOLLOW_TARGET`, accepts both required vars
  with optional restart=false default, accepts and parses
  `ANCHORD_FOLLOW_RESTART=true`.
- `resolveFollower(containers, target, selfProject)` finds the
  follower by compose-service-name preference, falls back to
  container-name, returns `""` for neither.
- `bootstrapRecheck` triggers reattach when the follower's stored
  network ID diverges from the live one; no-ops when they match;
  handles "follower not found" and "network not found" without
  panicking.
- Event consumer: dispatches `network create` for our network to
  reattach path; dispatches `network destroy` to log-only; ignores
  unrelated actions and unrelated network names.
- Reattach path is idempotent: handles "already attached" /
  "not attached" docker errors as success.
- Restart toggle: when `Restart=true`, restart is called after a
  successful reattach; when `false`, restart is never called.

**Integration** (manual smoke test, documented in the SPEC):

- Compose two stacks on a real Docker host: `target` with a bridge
  network, and `follower` with the rebinder sidecar + a busybox
  follower that pings a service in `target`. Verify reattach on
  `docker compose --project target down/up`.

**End-to-end** (production):

- Deploy v1.3.0 to `authentik-mailbox-sync` stack on TrueNAS. Add
  rebinder sidecar pointing at `ix-mailcow_mailcow-network`. Wait for
  next Mailcow auto-update (Sa 04:00). Verify IMAP auth continues
  uninterrupted through the recreate.

### Open implementation questions

- **DNS-cache flush without restart**: a middle-ground between
  reconnect-only and full container restart could be to signal the
  follower with `SIGHUP` to reload its config. Several real apps
  (nginx, postfix) respond to this. Out of scope for v1 — would
  require declaring which signal per follower. The `Restart=true`
  knob covers the conservative "I don't know what my app caches"
  case, which is the production default we want.
- **Multiple followers per sidecar**: deliberately deferred. See
  "One sidecar = one (network, target) pair" above.
- **Watch by compose-project + suffix (b-ii from issue #12)**: deferred
  to Phase 2. The `ix-` project prefix on TrueNAS is stable in
  practice; if it ever stops being stable, we add a project-label
  selector as a second config shape.

### Notes / current status

Triggered live by the 2026-06-07 Mailcow-update incident. The manual
recovery is two `midclt` calls (~90s); the rebinder collapses that
into a single sidecar with one job. F-48 is squarely in anchord's
existing scope ("re-anchor when the world shifts under you") —
F-43/F-45 handle netns recreates; F-48 handles bridge recreates.

Implementation estimate: 200–300 LOC plus tests.

- `internal/config`: new `Rebinder` struct, `LoadRebinder()` loader.
- `internal/rebinder`: package with `Watcher`, narrow Docker ops
  interface for tests, event loop matching the autostart pattern.
- `cmd/anchord/main.go`: new `ModeRebinder = "external-rebinder"`
  case, `runRebinder(ctx)` dispatch.
- Tests: full unit coverage with fake ops; integration smoke
  documented but not automated for v1 (would need privileged Docker
  in CI; deferred).
