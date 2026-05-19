# anchord — Spec Delta: Smarter shared-network picker (F-44)

> **Status:** Draft. Captures one new feature (F-44) that emerged during the Authentik DMZ migration on 2026-05-19. `detectSharedNetwork` (current implementation, post-F-38) excludes `ANCHORD_EXT_NETWORK` and prefers networks containing "transit" in their name; on a tie among multiple "transit"-named networks it returns whatever Go's map iteration hands out first. That's deterministic enough for single-transit stacks (mailcow wrap, choco, vault, xibo, …) but breaks the moment one anchord lives on **multiple** transit-named networks.
>
> **Motivation:** The Authentik stack has three anchord instances per identity (web, LDAP, frigate-proxy). Each one needs to share a network with **its** front-end (fe-anchor) plus optionally with a docker-socket-proxy on a separate transit bridge. With the current picker, anchord-frigate ended up choosing `ix-authentik_transit_dp` (docker-proxy bridge) instead of `ix-authentik_transit_f` (fe-anchor bridge), then logged `"no usable IP for container ix-authentik-fe-anchor-frigate-1"` and silently created zero DNAT entries. Operator-debuggable only by reading the log line carefully.
>
> The same problem will hit any future wrap setup that joins more than one bridge per anchord — e.g. wrapping a multi-tier compose project (web + db + queue), where each backend network has "transit" in the name as part of a naming convention.
>
> **Affected user code:** small heuristic in the picker; one optional env var for explicit override. No new container, no new mode. Greenfield (F-26) and existing wrap (F-39) paths stay unchanged.

## F-44 — Pick the shared network where backends actually live

### Problem

`detectSharedNetwork` (in `cmd/anchord/main.go`, post-F-38) does roughly:

1. Iterate `insp.NetworkSettings.Networks`.
2. Skip the entry whose key equals `ANCHORD_EXT_NETWORK`.
3. If any remaining entry's name contains "transit" (case-insensitive) → return it.
4. Otherwise return the first remaining entry (Go map iteration order, random).

With **one** non-EXT network the result is deterministic. With **multiple** the picker is a coinflip among the matching "transit" entries. On the Authentik stack the four candidates are `backend`, `transit_a`, `transit_f`, and (until we removed it) `transit_dp` — picker had three "transit" matches and chose at runtime random.

The right answer is "the network where my future backends will live." We just don't know that statically. But we **do** know:

- The selector (`ANCHORD_LABEL_SELECTOR`, F-42, or the implicit Compose-project filter) tells us which containers are backends.
- We can ask Docker which networks those backends are attached to.
- The intersection of "networks I'm attached to" ∩ "networks my backends are attached to" is the answer.

### Requirement

`detectSharedNetwork` should select the network that maximises **co-attachment with the configured backend set**:

1. Read the operator's `ANCHORD_EXT_NETWORK` (existing).
2. From `insp.NetworkSettings.Networks` (the anchor's own networks), drop the EXT entry.
3. List candidate networks from step 2.
4. Query Docker for containers matching the selector (`ANCHORD_LABEL_SELECTOR`, or `com.docker.compose.project=$ANCHORD_PROJECT` if selector unset) AND carrying `anchord.expose`. Call this the **expected-backend set**.
5. For each candidate network, count how many expected backends are attached to it. Pick the network with the **highest count**. Ties: prefer one containing "transit" in the name (preserves existing behaviour); secondary tie-breaker: alphabetical.
6. If the expected-backend set is empty (zero backends discoverable yet — common at startup), fall back to current F-38 behaviour: prefer "transit", else first non-EXT (alphabetical for determinism, not random).
7. **`ANCHORD_SHARED_NETWORK` opt-out** (new optional env var): when set to a literal network name, the picker returns that and skips the heuristic. Same syntax as `ANCHORD_EXT_NETWORK`. Lets the operator pin the choice for cases the heuristic mispredicts.

### Behaviour (verbal, no code)

1. **Startup-time** picker call happens before the first reconcile (current code path). At that moment, the expected-backend set may be empty if Compose-up hasn't fully spawned the project. Behaviour: fall back to F-38 ordering; the picker can re-run on subsequent reconciles if no backends were found.
2. **Re-evaluation**: on each reconcile, if `backends == 0` and we previously fell back, re-run the picker. If a backend has appeared on a different network than the one we chose, switch — and log loudly: `"shared network re-picked: <old> → <new> (backend appeared on new network)"`. This handles the F-44 motivating bug: at boot time none of the candidates have backends yet, so picker falls back to alphabetic; later when fe-anchor comes up on the right transit, picker switches.
3. **Stable once decided**: once a backend is found on the chosen network, do NOT switch on subsequent reconciles (avoid flapping if a backend appears later on another network too — first wins).
4. **Tie behaviour**: if two candidate networks both have the same backend count (the maximum), apply the existing F-38 "transit" preference, then alphabetical. Document this so operators can predict the choice.
5. **Empty-after-retry**: after N reconcile cycles (default 5) with `backends == 0`, emit a one-time warning `"no backends discovered on any candidate network — verify ANCHORD_LABEL_SELECTOR and that backends are attached to the same network as anchord"`. Don't crash; the situation is recoverable when the operator notices.

### Backwards-compat

- Single-non-EXT-network stacks: zero behavioural change. The picker still returns that single network.
- Multi-non-EXT-network stacks with a backend present at startup: picker now selects the right one deterministically (current code: random).
- Multi-non-EXT-network stacks with zero backends at startup: same behaviour as today initially, but picker self-corrects on later reconciles.
- `ANCHORD_SHARED_NETWORK` set: explicit, deterministic. Useful for tests and pinning known-good configurations.

### Acceptance tests

Unit:
- `pickSharedNetwork(selfNets, ext, candidates, expectedBackendsPerNet)` with two candidates and backends on one of them → returns that one.
- Ties on backend count → returns the "transit"-named one.
- Both ties on backend count and "transit"-naming → returns alphabetically first.
- `ANCHORD_SHARED_NETWORK` set to a valid name → returned regardless of heuristic.
- `ANCHORD_SHARED_NETWORK` set to a name not in self-networks → fatal startup error.
- Empty backend set → fall back to F-38 ordering (alphabetical first non-EXT).

Integration (synthetic):
- Compose with anchord on three non-EXT networks (`a_transit`, `b_transit`, `c_transit`) plus a backend on `b_transit` only. Expect picker to choose `b_transit` and DNAT to work first try.
- Same as above but no backend present at startup; backend spawned 10 s later on `b_transit`. Expect re-pick log line and DNAT to appear within ~2 reconcile cycles.

### Notes / current status

Triggered live by the Authentik migration 2026-05-19. Symptom in `cmd/anchord` log:

```
{"level":"INFO","msg":"reconciled","backends":0,"entries":0,"conntrack_flushed":0}
{"level":"WARN","msg":"no usable IP for container","container":"ix-authentik-fe-anchor-frigate-1","shared_network":"ix-authentik_transit_dp"}
```

(anchord-frigate picked `transit_dp` randomly; fe-anchor-frigate was on `transit_f` and `backend`; backend IP unreadable from the wrong shared network.)

Workaround applied today: removed the `transit_dp` network and put docker-proxy on `backend` (already a peer of every anchord). Eliminates the ambiguity by reducing to a single non-EXT pickable network per anchord. Works but is a self-inflicted compose-topology constraint we'd prefer to remove.

Implementation estimate: 40–60 LOC plus tests.

- `internal/discovery`: backend-set query already exists (post-F-42). Add a "count per network" reducer.
- `cmd/anchord`: refactor `detectSharedNetwork` to call into discovery. Or stash the pickShared logic in `internal/discovery`.
- New env var: `ANCHORD_SHARED_NETWORK` (string, optional). Parsed in `internal/config`.

## Cross-references

- [SPEC.md §2.1](SPEC.md) — current shared-network role.
- [SPEC-WRAP-DRAFT.md F-38](SPEC-WRAP-DRAFT.md) — the earlier "exclude EXT" patch; F-44 builds on it.
- [SPEC-LABEL-SELECTOR-DRAFT.md F-42](SPEC-LABEL-SELECTOR-DRAFT.md) — selector that defines the expected-backend set; F-44 uses it directly.
- The triggering migration: Authentik on TrueNAS, three anchord-instances in one compose project. See [lammers-krueger-firewall/projects/10g-dmz-migration/state.md](../../lammers-krueger-firewall/projects/10g-dmz-migration/state.md), 2026-05-19 entry.
