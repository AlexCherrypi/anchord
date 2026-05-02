# Working on anchord

> Instructions for Claude Code (and any other LLM agent) picking up work
> on this project.
## Read these first, in order

1. `ARCHITECTURE.md` — the three-role model (network-anchor, service-anchors,
   backends) and how traffic flows. The mental map you need to read anything
   else correctly.
2. `SPEC.md` — what anchord must do (acceptance criteria, testable).
3. `CONTEXT.md` — why anchord is shaped the way it is (design principles,
   rejected alternatives).
4. `README.md` — how a user encounters anchord (mental model first, then
   architecture, then config).

If a request to you contradicts SPEC or CONTEXT, surface that contradiction
before writing code. Don't quietly route around the design.

## Project status

Pre-alpha. Functional surface as of 2026-05-02:

- Network-anchor and service-anchor modes both implemented and
  tested. Single binary, `ANCHORD_MODE=network-anchor` (default) or
  `ANCHORD_MODE=service-anchor` (or `command: [service-anchor]`).
- Phase-2 e2e harness lands at 54/54 across all four DHCP scenarios
  on Docker Desktop with `E2E_BRIDGE_FLOOD_FIX=1` (a one-shot
  `bridge-nf-call-iptables=0` host-wide tweak that's only needed on
  Docker Desktop's WSL2 bridge — production Linux hosts don't need
  it). Without the flag: 50/54, the 4 deltas are exactly the v4 DHCP
  path the workaround unlocks.
- Two F-20 fixes landed on the way: `main` no longer treats
  `context.Canceled` as fatal (exit 1 on SIGTERM was wrong), and the
  dhcp goroutine is now awaited via `WaitGroup` so its deferred
  `removeLink` actually runs to completion before main returns.
- Test-report machinery: `scripts/code-hash.sh` produces a
  deterministic SHA-256 over `*.go`, `go.mod`/`go.sum`, `Dockerfile`,
  `test/`, `scripts/`. `scripts/update-test-report.sh` self-execs in
  Docker, runs everything, only writes README's TEST-REPORT block on
  green. `.github/workflows/release-gate.yml` blocks any `v*` tag
  whose tagged commit is either off-main or whose recorded hash is
  stale.

Next gaps: see "Open questions / future work" below. Highest leverage
for v1.0 readiness is real-host validation; the rest are SPEC-level
decisions (metrics, health, DHCPv6).

## Code conventions

- **Package layout:** `cmd/anchord` is the only binary entry point;
  it dispatches on `ANCHORD_MODE` (or first non-flag argument) into
  either the network-anchor or service-anchor code paths.
  `internal/*` packages are leaf-shaped; `reconciler` is the only one
  that depends on multiple others.
- **Logging:** `log/slog` only, JSON handler, structured key/value pairs.
  No `fmt.Println` debug leftovers.
- **Errors:** wrap with `fmt.Errorf("context: %w", err)` at every layer
  boundary. Never discard.
- **Concurrency:** one goroutine per long-running subsystem (dhcp,
  discovery, reconciler), coordinated via context cancellation. No
  goroutines that aren't tied to a context.
- **Tests:** table-driven, `t.Run` subtests. Public surface of every
  package needs at least happy-path coverage. Integration tests with
  real netlink/nftables live alongside the package they exercise (e.g.
  `internal/nat/nat_integration_test.go`) and are gated behind a build
  tag (`//go:build integration`). The privileged Docker driver to run
  them lives in `test/integration/`.
- **Comments:** every exported symbol has a doc comment. Package doc
  comments explain the package's role in the system, not just what it is.

## Don't do

- Don't add a config file format. Environment variables only. (See
  CONTEXT.md "Configuration scarcity".)
- Don't add iptables paths. nftables only.
- Don't shell out to `nft`, `ip` or other userland tools when a netlink
  Go library is available. `dhclient` and `conntrack` are the only
  intentional subprocess dependencies; both have justifications in the
  Dockerfile.
- Don't add features that operate above layer 4. (See CONTEXT.md "Layer 4
  hard stop".)
- Don't break the atomic-update guarantee on inbound NAT. (See SPEC F-19.)

## Definition of "done" for any change

A change is ready to merge when:

1. `go vet ./...` clean.
2. `go test ./...` passes.
3. Touched code is covered by at least one test.
4. If user-visible: README updated. If design-visible: CONTEXT or SPEC
   updated.
5. Manual smoke test on real Docker: `docker compose up` against
   `compose.example.yaml` produces a working NAT path.
6. The commit message explains *why*, not just *what*. "Fix DNAT rule"
   is bad; "DNAT rule needs explicit l4proto match for nftables ≥ 1.0.6"
   is good.

## When to ask the human

- Anything that touches the SPEC (functional or non-functional
  requirements).
- Adding or removing a CLI / environment variable.
- Choosing between two implementations with materially different tradeoffs.
- Any change to the Mental Model section of README.md.

When *not* to ask: routine refactoring, test additions, documentation
fixes, dependency bumps that don't change behavior, obvious bug fixes
where the bug and fix are self-evident.

## Open questions / future work

Tracked in GitHub issues; this list is just the high-priority ones at
project genesis:

- [x] Verify nftables map atomic replace actually works the way we want
  on kernel 6.x — `internal/nat/nat_integration_test.go` (build tag
  `integration`) verifies `Setup`, `SetMap` populate/replace/clear, and
  the F-19 write-side atomicity (every post-write dump matches the
  written state across ~50 flips/s). Driven by `test/integration/
  run.ps1` (Windows) or `run.sh` (Linux/macOS) — privileged Docker
  container with `--cap-add=NET_ADMIN`. Concurrent userspace dumps can
  occasionally observe mixed snapshots; that's a kernel multi-message
  dump quirk, not a dataplane issue (see test doc-comment).
- [x] Phase-1 integration harness in `test/e2e/` covering boot,
  nftables setup, discovery, DNAT-map population across all four DHCP
  scenarios (v4-only, v6-only, both, none). Fully Docker-native:
  `test/e2e/run.ps1` (Windows) or `test/e2e/up.sh` (Linux/macOS) spawn
  a runner container that drives `docker compose`. 26/28 assertions
  green; the 2 fails are a Docker-side macvlan-on-bridge broadcast
  quirk, not anchord — re-verify v4 lease path on a real Linux host.
- [x] Phase-2 e2e: real listener inside the service-anchor namespace
  + probe container on the lan bridge, S-2/S-3/S-6 assertions in
  `run.sh`. Lands at 54/54 green on Docker Desktop with the opt-in
  `E2E_BRIDGE_FLOOD_FIX=1` (sets `bridge-nf-call-iptables=0` to undo
  Docker's default iptables-FORWARD drop of bridge broadcasts), and
  50/54 without — the 4 deltas are exactly the v4 DHCP path that
  needs that workaround. Real Linux hosts don't need the flag (see
  next item).
- [x] Implement service-anchor mode (SPEC §2.6, ARCHITECTURE role 2):
  `cmd/anchord` dispatches on `ANCHORD_MODE`, `internal/serviceanchor`
  package, F-24..F-29 contract honoured, e2e compose's smtp-anchor
  uses it. Done.
- [x] Code-hash test-report system + release gate:
  `scripts/{code-hash,update-test-report,verify-test-report}.{sh,ps1}`,
  `.github/workflows/{ci,release-gate}.yml`. README's
  auto-generated TEST-REPORT block is the release-readiness signal;
  release gate blocks tags that aren't on main or whose recorded
  hash is stale.
- [ ] Real-host validation: run the e2e harness on an actual Linux
  host with a physical VLAN sub-interface and confirm 54/54 without
  `E2E_BRIDGE_FLOOD_FIX`. Closes the env-quirk caveat for good.
- [ ] Decide on Prometheus metrics surface (which counters/gauges).
  Likely candidates: reconcile latency, DHCP lease age, dnat_tcp/udp
  map size, conntrack flushes, dhclient restart count. Needs a SPEC
  decision before implementation.
- [ ] Health endpoint shape (`/healthz` returns what exactly?).
  Liveness vs. readiness; what counts as "ready" — tables installed?
  first lease? first reconcile? Needs a SPEC decision.
- [ ] DHCPv6 handling — currently we assume SLAAC is enough for v6.
  e2e v6-only run confirms SLAAC works through anchord's macvlan child.
  DHCPv6 (e.g. for hostname-announcement on v6) is not implemented;
  decision pending: ship in v0.1 or defer.
- [ ] Behavior when the VLAN parent interface goes down mid-run.
  Today: dhclient dies, the supervisor backs off and retries; macvlan
  re-creation after parent recovery is untested. Worth a smoke test
  + maybe a small explicit re-attach.

## Style notes for human-facing text

The README and SPEC are *deliberately* terse. Don't pad them with
rhetorical framing or "important note" boxes. The reader is technical
and doesn't need to be sold on the project — they need to ship something
that works.

Funny is fine in commit messages and inline comments. Not in the SPEC.
