# Contributing to anchord

Thanks for your interest. anchord is feature-complete and in beta;
contributions are very welcome — especially real-host validation,
edge-case bug reports, and SPEC-level discussion before code.

## Before you start

1. Read [`ARCHITECTURE.md`](ARCHITECTURE.md), [`SPEC.md`](SPEC.md),
   and [`CONTEXT.md`](CONTEXT.md) in that order. The three-role model
   is the part that's easiest to misread; getting it right makes the
   rest of the codebase obvious.
2. For non-trivial changes (anything touching SPEC, adding/removing
   environment variables, changing the mental model), open an issue
   first to discuss the approach. Routine refactors, test additions,
   docs fixes, and obvious bug fixes can go straight to a PR.

## Development setup

```sh
git clone https://github.com/AlexCherrypi/anchord
cd anchord
go mod tidy
go build ./cmd/anchord
docker build -t anchord:dev .
```

See [`TESTING.md`](TESTING.md) for the unit + e2e test commands per
platform. The full suite runs host-independently inside Docker via
`scripts/update-test-report.sh`.

## Definition of done

A change is ready to merge when:

1. `go vet ./...` clean.
2. `go test ./...` passes.
3. Touched code is covered by at least one test.
4. If user-visible: README updated. If design-visible: CONTEXT or SPEC
   updated.
5. Manual smoke test on real Docker: `docker compose up` against
   `compose.example.yaml` produces a working NAT path.
6. The commit message explains *why*, not just *what*.

## Things we don't take

- iptables paths — nftables only.
- Config file formats — environment variables only.
- Layer-7 features (HTTP routing, TLS termination, hostname-based
  multiplexing). anchord stops at layer 4 by design. If you need
  layer-7, run a reverse proxy as a service-anchor.
- Shelling out to `nft` / `ip` / other userland tools when a netlink
  Go library is available. `conntrack` is currently the only
  intentional subprocess dependency. (DHCP is pure-Go via
  `github.com/insomniacslk/dhcp`; don't regress that.)

See [`CLAUDE.md`](CLAUDE.md) "Don't do" for the full list.

## Reporting bugs

Open an issue with:

- What you ran (compose snippet, env vars).
- What you expected.
- What happened (logs, `nft list ruleset` output if relevant).
- Kernel version, Docker version, host OS.

For security-sensitive issues, see [`SECURITY.md`](SECURITY.md).
