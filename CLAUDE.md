# Working on anchord

> Instructions for Claude Code (and any other LLM agent) picking up work
> on this project.

## Read these first, in order

1. `SPEC.md` — what anchord must do (acceptance criteria, testable).
2. `CONTEXT.md` — why anchord is shaped the way it is (design principles,
   rejected alternatives).
3. `README.md` — how a user encounters anchord (mental model first, then
   architecture, then config).

If a request to you contradicts SPEC or CONTEXT, surface that contradiction
before writing code. Don't quietly route around the design.

## Project status

Pre-alpha. Skeleton was generated in one session and has not yet been
compiled end-to-end (the original sandbox couldn't reach the Go module
proxy). First task on a real machine is `go mod tidy && go build ./...`
and resolving whatever the actual `nftables` v0.2.0 API names are — the
DNAT rule construction in `internal/nat/nat.go` is the most likely spot
to need a small fix.

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
  real netlink/nftables go in `test/integration/` and are gated behind
  a build tag (`//go:build integration`).
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

- [ ] First successful end-to-end build on a real host (sandbox couldn't
  reach module proxy at generation time).
- [ ] Verify nftables map atomic replace actually works the way we want
  on kernel 6.x — write integration test.
- [ ] Decide on Prometheus metrics surface (which counters/gauges).
- [ ] Health endpoint shape (`/healthz` returns what exactly?).
- [ ] DHCPv6 handling — currently we assume SLAAC is enough for v6.
- [ ] Behavior when the VLAN parent interface goes down mid-run.

## Style notes for human-facing text

The README and SPEC are *deliberately* terse. Don't pad them with
rhetorical framing or "important note" boxes. The reader is technical
and doesn't need to be sold on the project — they need to ship something
that works.

Funny is fine in commit messages and inline comments. Not in the SPEC.
