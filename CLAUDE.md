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

Pre-alpha. The skeleton compiles end-to-end: `go vet ./...`,
`go build ./...`, `go test ./...` and `docker build` all pass clean as
of 2026-05-02. The `nftables` v0.2.0 API matched what the code already
expected, no fixes required.

Next gaps are real-host validation against an actual VLAN/DHCP server
and the integration test harness — see Open questions below.

## Code conventions

- **Package layout:** `cmd/anchord` is the only binary entry point.
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
- [ ] Phase-2 e2e: real listener inside the service-anchor namespace
  + probe container on the lan bridge to validate the inbound DNAT
  path end-to-end (S-2 source-IP preservation, S-3 restart, S-4 lease
  rotation, S-5 two-projects, S-6 teardown).
- [ ] Decide on Prometheus metrics surface (which counters/gauges).
- [ ] Health endpoint shape (`/healthz` returns what exactly?).
- [ ] DHCPv6 handling — currently we assume SLAAC is enough for v6.
  e2e v6-only run confirms SLAAC works through anchord's macvlan child.
- [ ] Behavior when the VLAN parent interface goes down mid-run.

## Style notes for human-facing text

The README and SPEC are *deliberately* terse. Don't pad them with
rhetorical framing or "important note" boxes. The reader is technical
and doesn't need to be sold on the project — they need to ship something
that works.

Funny is fine in commit messages and inline comments. Not in the SPEC.
