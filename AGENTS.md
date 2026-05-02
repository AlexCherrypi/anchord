# AGENTS.md

This file is the entry point for AI coding assistants (Cursor, Codex,
Aider, Continue, Cline, etc.) working in this repository. It mirrors
[CLAUDE.md](CLAUDE.md) — both are kept in sync; treat either as
authoritative.

## Read these first, in order

1. [`ARCHITECTURE.md`](ARCHITECTURE.md) — the three-role model
   (network-anchor, service-anchors, backends) and how traffic flows.
2. [`SPEC.md`](SPEC.md) — what anchord must do (acceptance criteria).
3. [`CONTEXT.md`](CONTEXT.md) — *why* anchord is shaped the way it is
   (design principles, rejected alternatives).
4. [`README.md`](README.md) — the user-facing story.
5. [`CLAUDE.md`](CLAUDE.md) — full agent playbook: code conventions,
   "don't do" list, definition of done, open questions.

If a request contradicts SPEC or CONTEXT, surface that contradiction
before writing code. Don't quietly route around the design.

## TL;DR for agents

- Go 1.25, single binary `cmd/anchord`, mode-dispatched on `ANCHORD_MODE`.
- Environment variables only — no config files.
- nftables only — no iptables paths.
- Layer 4 hard stop — no HTTP/TLS/L7 features.
- Tests must accompany behaviour changes; integration tests are gated
  behind `//go:build integration`.

For everything else, [`CLAUDE.md`](CLAUDE.md) is the canonical playbook.
