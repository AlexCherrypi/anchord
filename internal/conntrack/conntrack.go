// Package conntrack flushes connection-tracking entries that point at
// stale backend IPs. Without this, existing TCP connections continue
// being routed to a backend that no longer exists when a container
// gets a new IP.
//
// We shell out to `conntrack -D` for v0.1 — the kernel API works via
// netlink (NFNL_SUBSYS_CTNETLINK) and there are Go bindings, but the
// volume here is low (one flush per backend change) and the conntrack
// CLI ships in every distro that runs nftables.
package conntrack

import (
	"context"
	"log/slog"
	"net"
	"os/exec"
)

// FlushDestination removes all conntrack entries whose post-DNAT
// destination matches ip. Called after we update an nftables map so
// that already-tracked connections re-evaluate the new mapping.
func FlushDestination(ctx context.Context, ip net.IP) {
	if ip == nil {
		return
	}
	flag := "-d"
	cmd := exec.CommandContext(ctx, "conntrack", flag, ip.String(), "-D")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// `conntrack -D` exits non-zero with code 1 when no entries
		// match. That's noise, not a failure.
		slog.Debug("conntrack flush", "ip", ip, "out", string(out), "err", err)
		return
	}
	slog.Debug("conntrack flushed", "ip", ip)
}
