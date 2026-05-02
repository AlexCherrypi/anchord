# Security policy

## Supported versions

anchord is **pre-alpha**. There are no formal supported versions yet;
the only intended target is `main` and the current `:latest` container
image. Once a v1.0 line exists, this section will be updated.

## Reporting a vulnerability

Please do **not** file a public GitHub issue for security-sensitive
problems. Instead, use GitHub's private vulnerability reporting:

→ https://github.com/AlexCherrypi/anchord/security/advisories/new

Or email the maintainer directly:

→ alexander.kirsch@wemotion.io

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
- Bugs in `dhclient`, `conntrack`, the kernel's nftables implementation,
  or Docker itself, unless anchord is using them in a way that
  predictably triggers a security-relevant failure mode.
