# Security policy

## Supported versions

anchord is in **beta** (feature-complete, awaiting real-host
validation before a v1 tag). There are no formal supported version
lines yet; the only intended target is `main` and the current
`:latest` container image. Once a v1.0 line exists, this section
will be updated.

## Reporting a vulnerability

Please do **not** file a public GitHub issue for security-sensitive
problems. Instead, use GitHub's private vulnerability reporting:

→ https://github.com/AlexCherrypi/anchord/security/advisories/new

Please include:

- A description of the issue and its impact.
- Reproduction steps (compose snippet, env vars, kernel/Docker version).
- Any logs or `nft list ruleset` output that demonstrates the problem.

You should receive a response within 7 days. If you don't, please
follow up — message-loss happens.

## Threat model

anchord runs with `CAP_NET_ADMIN` inside a container. Issues we
consider in scope:

- **NAT-rule confusion** — anchord installing rules that route traffic
  to the wrong backend, leak inbound traffic past the DNAT, or cause
  cross-project bleed.
- **Source-IP preservation failures** — anchord causing inbound
  connections to be seen with mangled or substituted source IPs when
  it claims to preserve them (an authentication-bypass class of issue
  for downstream services that trust source IPs).
- **Privilege escalation paths** — anchord providing a way for a
  compromised application container to gain capabilities it shouldn't
  have via the network namespace it joins through `network_mode:
  service:<anchor>`.
- **Docker socket exposure** — anchord requesting more from the
  socket-proxy than it should.

Out of scope:

- Misconfiguration of the host kernel sysctls listed in `compose.example.yaml`
  — those are the operator's responsibility.
- Bugs in `conntrack`, the kernel's nftables implementation, the
  `github.com/insomniacslk/dhcp` library, or Docker itself, unless
  anchord is using them in a way that predictably triggers a
  security-relevant failure mode.

## Dismissed advisories (audit trail)

Dependabot will occasionally flag CVEs in `github.com/docker/docker`
(or its transitive deps) that affect Docker's daemon-side code paths
which anchord does not link. Those alerts are dismissed with the
rationale below, kept here so the dismissal is reviewable.

### CVE-2026-34040 — GHSA-x744-4wpc-v9h2 (high, CVSS 8.8)

*"Moby has AuthZ plugin bypass when provided oversized request bodies"*

- **Vulnerable code:** moby/moby's daemon-side AuthZ middleware
  (`daemon/authorization/...`). The bug is that an authorisation
  plugin is invoked on a truncated request body; oversized bodies
  bypass the plugin check.
- **Why anchord is not affected:** anchord imports only
  `github.com/docker/docker/client` and `api/types/{container,events,filters,network}`
  (see `internal/discovery/discovery.go`). The `daemon/`,
  `authorization/`, and `plugin/` packages are not in anchord's
  link graph. anchord is a **client** of a Docker daemon; it does
  not run AuthZ plugins, does not implement an AuthZ-callable HTTP
  surface, and does not handle the request bodies on the daemon
  side.
- **Why we did not bump:** the patch shipped as moby's
  `docker-v29.3.1` release tag. That tag has a prefix, so the Go
  module proxy does not index it as a valid semver; `proxy.golang.org`
  for `github.com/docker/docker` (and `github.com/moby/moby`) tops
  out at `v28.5.2+incompatible`. Moby's new module path
  `github.com/moby/moby/v2` is still in beta (`v2.0.0-beta.11` at
  time of writing). There is no Go-module-importable version with
  the fix.
- **Revisit when:** moby publishes a clean semver tag in the v29.x
  line, or `github.com/moby/moby/v2` exits beta.

### CVE-2026-33997 — GHSA-pxq6-2prw-chj9 (medium, CVSS 6.8)

*"Moby has an Off-by-one error in its plugin privilege validation"*

- **Vulnerable code:** moby/moby's plugin privilege validator
  (`plugin/...`). CWE-193 (Off-by-One Error).
- **Why anchord is not affected:** anchord does not use Docker
  plugins at all. The plugin privilege validation path is not
  reachable from any anchord-linked code. anchord's only Docker
  interaction is enumerating containers and watching events via
  the SDK client — no plugin loading, no plugin privilege checks.
- **Why we did not bump:** same reason as the AuthZ bypass above —
  the fix is in `docker-v29.3.1`, which is not a Go-module-importable
  version.
- **Revisit when:** same as above.
