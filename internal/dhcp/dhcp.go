// Package dhcp manages the macvlan interface and DHCP client lifecycle
// inside the anchord container.
//
// DHCP is implemented as a pure-Go client (github.com/insomniacslk/dhcp)
// — no subprocess. Each address family runs in its own goroutine that
// performs DISCOVER/REQUEST (or SOLICIT/REQUEST for v6), applies the
// leased address and default route to the macvlan child via netlink,
// and renews on the lease's T1 timer. On supervisor shutdown the
// goroutines send DHCPRELEASE before the link is torn down.
//
// On networks without a DHCPv6 server, the v6 goroutine quietly retries
// SOLICIT forever — same end-state as the kernel's SLAAC autoconf via
// Router Advertisements (which the kernel handles independently of us).
package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/AlexCherrypi/anchord/internal/metrics"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/vishvananda/netlink"
)

// Supervisor owns the external macvlan link and the per-family DHCP
// client goroutines.
type Supervisor struct {
	parentName string
	ifaceName  string
	mac        net.HardwareAddr
	hostname   string
	backoffMax time.Duration

	currentIP chan net.IP
}

// New constructs a Supervisor.
func New(parent, iface string, mac net.HardwareAddr, hostname string, backoffMax time.Duration) *Supervisor {
	return &Supervisor{
		parentName: parent,
		ifaceName:  iface,
		mac:        mac,
		hostname:   hostname,
		backoffMax: backoffMax,
		currentIP:  make(chan net.IP, 8),
	}
}

// IPs returns a channel that emits the current external IPv4 whenever
// it changes. The first emission signals that the link is up and a
// lease has been obtained.
func (s *Supervisor) IPs() <-chan net.IP { return s.currentIP }

// Run blocks until ctx is cancelled. Manages the link lifecycle and
// the per-family DHCP client goroutines with exponential backoff
// (capped at backoffMax).
//
// ensureLink runs at the top of every iteration so we recover from
// the macvlan child being deleted externally or the VLAN parent
// flapping. Failures (e.g. parent missing) are not fatal — we back
// off and retry, which is the right behaviour for a long-running
// supervisor: an operator may bring the VLAN parent back at any time.
func (s *Supervisor) Run(ctx context.Context) error {
	defer s.removeLink()

	go s.watchIP(ctx)

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := s.ensureLink(); err != nil {
			slog.Warn("ensure link failed, will retry",
				"iface", s.ifaceName,
				"parent", s.parentName,
				"err", err,
				"backoff", backoff)
			if !s.sleepBackoff(ctx, &backoff) {
				return ctx.Err()
			}
			continue
		}
		// Reset backoff on a successful link state.
		backoff = time.Second

		// Run DHCP clients (v4 + v6 in parallel) under a child context so
		// we can cancel them independently of the supervisor's own
		// context. The watcher goroutine cancels the child context as
		// soon as the link stops being usable, which terminates both
		// client goroutines and falls back into this loop's ensureLink
		// path.
		dhCtx, dhCancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go s.watchLinkUsable(dhCtx, dhCancel, watchDone)

		err := s.runClients(dhCtx)
		dhCancel()
		<-watchDone

		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("dhcp clients exited, retrying",
			"err", err, "backoff", backoff)

		if !s.sleepBackoff(ctx, &backoff) {
			return ctx.Err()
		}
	}
}

// watchLinkUsable polls the macvlan child every 2s and cancels the
// caller's context as soon as the link is missing or down. Closes
// `done` on return so the caller can synchronise with shutdown.
func (s *Supervisor) watchLinkUsable(ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.linkUsable() {
				slog.Warn("macvlan child no longer usable, cancelling DHCP clients",
					"iface", s.ifaceName)
				cancel()
				return
			}
		}
	}
}

// linkUsable reports whether anchord-ext currently exists and is up.
// "Up" is checked via the IFF_UP flag — when the parent goes admin-
// down or is deleted, the kernel either drops the flag or removes
// the child entirely.
func (s *Supervisor) linkUsable() bool {
	l, err := netlink.LinkByName(s.ifaceName)
	if err != nil {
		return false
	}
	return l.Attrs().Flags&net.FlagUp != 0
}

// sleepBackoff blocks for `*backoff` (or until ctx is cancelled) and
// then doubles `*backoff`, capped at s.backoffMax. Returns false iff
// ctx was cancelled while waiting.
func (s *Supervisor) sleepBackoff(ctx context.Context, backoff *time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(*backoff):
	}
	*backoff *= 2
	if *backoff > s.backoffMax {
		*backoff = s.backoffMax
	}
	return true
}

// ensureLink makes sure the macvlan child exists with our configured
// MAC, parent and mode, and is up. Idempotent: a no-op when the link
// already matches; only re-creates when the link is missing or its
// shape diverged from ours (rare, but possible after a partial-state
// crash of a previous run).
func (s *Supervisor) ensureLink() error {
	if existing, err := netlink.LinkByName(s.ifaceName); err == nil {
		if s.linkMatchesConfig(existing) {
			// Already correct — just make sure it's up. The kernel
			// brings the child back up automatically when the parent
			// returns, but it can land in a transient down state.
			if existing.Attrs().Flags&net.FlagUp == 0 {
				if err := netlink.LinkSetUp(existing); err != nil {
					return fmt.Errorf("LinkSetUp %s: %w", s.ifaceName, err)
				}
			}
			return nil
		}
		// Wrong shape (likely stale state from a crashed previous
		// run). Wipe so the create below produces a clean child.
		if err := netlink.LinkDel(existing); err != nil {
			return fmt.Errorf("LinkDel stale %s: %w", s.ifaceName, err)
		}
	}

	parent, err := netlink.LinkByName(s.parentName)
	if err != nil {
		return fmt.Errorf("parent %s: %w", s.parentName, err)
	}

	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:         s.ifaceName,
			ParentIndex:  parent.Attrs().Index,
			HardwareAddr: s.mac,
		},
		Mode: netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := netlink.LinkAdd(mv); err != nil {
		return fmt.Errorf("LinkAdd %s: %w", s.ifaceName, err)
	}
	if err := netlink.LinkSetUp(mv); err != nil {
		return fmt.Errorf("LinkSetUp %s: %w", s.ifaceName, err)
	}

	slog.Info("macvlan up",
		"iface", s.ifaceName,
		"parent", s.parentName,
		"mac", s.mac.String())
	return nil
}

// linkMatchesConfig is true if the given link is a macvlan child with
// the same MAC, mode and parent as we expect. Used to decide whether
// an existing link is reusable or needs to be wiped.
func (s *Supervisor) linkMatchesConfig(l netlink.Link) bool {
	mv, ok := l.(*netlink.Macvlan)
	if !ok {
		return false
	}
	if mv.HardwareAddr.String() != s.mac.String() {
		return false
	}
	if mv.Mode != netlink.MACVLAN_MODE_BRIDGE {
		return false
	}
	parent, err := netlink.LinkByName(s.parentName)
	if err != nil {
		// Parent gone — we can't validate the relationship. Treat as
		// non-matching so the caller wipes; the recreate path will
		// itself fail until the parent is back.
		return false
	}
	return mv.ParentIndex == parent.Attrs().Index
}

func (s *Supervisor) removeLink() {
	if l, err := netlink.LinkByName(s.ifaceName); err == nil {
		_ = netlink.LinkDel(l)
		slog.Info("macvlan removed", "iface", s.ifaceName)
	}
}

// runClients runs the v4 and v6 DHCP client goroutines in parallel.
// Returns when both have exited (which only happens on context cancel
// or a fatal error in one of them — internal DHCP-protocol errors are
// handled with backoff inside the per-family runners).
func (s *Supervisor) runClients(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.runFamily(ctx, 4)
	}()
	go func() {
		defer wg.Done()
		s.runFamily(ctx, 6)
	}()
	wg.Wait()
	return ctx.Err()
}

// runFamily is the per-family client loop. It opens a fresh nclient,
// performs the full DISCOVER/REQUEST exchange, applies the lease, and
// then runs a renewal loop driven by the lease's T1 timer. On any
// DHCP-protocol error it logs and backs off, then retries — the loop
// only exits on context cancel.
func (s *Supervisor) runFamily(ctx context.Context, family int) {
	famLabel := familyLabel(family)
	defer metrics.DHCPLeaseRemaining.Clear(famLabel)
	backoff := time.Second
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		if !first {
			metrics.DHCPClientRestarts.WithLabelValues(famLabel).Inc()
		}
		first = false
		var err error
		if family == 4 {
			err = s.runV4Once(ctx)
		} else {
			err = s.runV6Once(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		metrics.DHCPAcquired.WithLabelValues(famLabel, "error").Inc()
		slog.Warn("dhcp client lost lease, retrying",
			"family", family, "err", err, "backoff", backoff)
		if !s.sleepBackoff(ctx, &backoff) {
			return
		}
	}
}

// familyLabel maps the integer family to the "v4"/"v6" string used by
// the Prometheus surface (see SPEC F-31).
func familyLabel(family int) string {
	if family == 6 {
		return "v6"
	}
	return "v4"
}

// runV4Once runs one full lease lifecycle: DORA, apply, renew loop.
// Returns when the lease is irrecoverable (NAK, renew-failed across
// rebind window, or context cancel). The deferred release fires on
// every exit path.
func (s *Supervisor) runV4Once(ctx context.Context) error {
	client, err := nclient4.New(s.ifaceName)
	if err != nil {
		return fmt.Errorf("nclient4.New: %w", err)
	}
	defer client.Close()

	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lease, err := client.Request(requestCtx, dhcpv4.WithOption(dhcpv4.OptHostName(s.hostname)))
	if err != nil {
		return fmt.Errorf("DORA: %w", err)
	}

	if err := s.applyV4Lease(lease.ACK); err != nil {
		return fmt.Errorf("apply v4 lease: %w", err)
	}
	metrics.DHCPAcquired.WithLabelValues("v4", "ok").Inc()
	metrics.DHCPLeaseRemaining.Set("v4", time.Now().Add(leaseTime(lease.ACK)))
	// Cleanup must happen RELEASE -> unapply (LIFO of declaration: the
	// release packet is built from the freshest `lease`, then the IP
	// is removed from the iface). We capture `lease` by reference so
	// the renewal loop's reassignments are visible at exit time.
	defer func() {
		s.unapplyV4Lease(lease.ACK)
	}()
	defer func() {
		s.releaseV4(client, lease)
	}()

	slog.Info("dhcp v4 lease acquired",
		"iface", s.ifaceName,
		"ip", lease.ACK.YourIPAddr,
		"server", lease.ACK.ServerIPAddr,
		"lease", leaseTime(lease.ACK))

	for {
		renewIn := renewalInterval(lease.ACK)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(renewIn):
		}

		// Renew is a unicast REQUEST to the server. If it fails we'd
		// normally try rebind (broadcast REQUEST) at T2, but the
		// library doesn't expose that as a separate primitive — its
		// Renew already falls back to broadcast on unicast failure.
		// On total failure we return so runFamily restarts the DORA
		// from scratch, which is equivalent to a kernel restart.
		renewCtx, renewCancel := context.WithTimeout(ctx, 30*time.Second)
		newLease, err := client.Renew(renewCtx, lease, dhcpv4.WithOption(dhcpv4.OptHostName(s.hostname)))
		renewCancel()
		if err != nil {
			return fmt.Errorf("renew: %w", err)
		}
		if err := s.applyV4Lease(newLease.ACK); err != nil {
			return fmt.Errorf("apply renewed v4 lease: %w", err)
		}
		lease = newLease
		metrics.DHCPAcquired.WithLabelValues("v4", "ok").Inc()
		metrics.DHCPLeaseRemaining.Set("v4", time.Now().Add(leaseTime(lease.ACK)))
		slog.Debug("dhcp v4 lease renewed",
			"iface", s.ifaceName,
			"ip", lease.ACK.YourIPAddr,
			"lease", leaseTime(lease.ACK))
	}
}

// renewalInterval computes when to send the next REQUEST. We honour
// the server-supplied T1 if present, falling back to lease/2 (the
// RFC-2131 default behaviour).
func renewalInterval(ack *dhcpv4.DHCPv4) time.Duration {
	if t1 := ack.IPAddressRenewalTime(0); t1 > 0 {
		return t1
	}
	lease := leaseTime(ack)
	if lease > 0 {
		return lease / 2
	}
	// Conservative default if the server gave us nothing: renew in
	// 30 minutes. Any real DHCP server will have provided a lease
	// time; this is just a guard against pathological replies.
	return 30 * time.Minute
}

func leaseTime(ack *dhcpv4.DHCPv4) time.Duration {
	return ack.IPAddressLeaseTime(time.Hour)
}

// applyV4Lease installs the leased IP and a default route on the
// macvlan child. Idempotent: re-running with the same lease is a
// no-op; running with a different IP replaces the previous one.
func (s *Supervisor) applyV4Lease(ack *dhcpv4.DHCPv4) error {
	link, err := netlink.LinkByName(s.ifaceName)
	if err != nil {
		return fmt.Errorf("link lookup: %w", err)
	}

	mask := ack.SubnetMask()
	if mask == nil {
		// No subnet mask in the reply is unusual but defensible —
		// fall back to a /32 host route. The default route below
		// then handles everything else.
		mask = net.CIDRMask(32, 32)
	}
	ipnet := &net.IPNet{IP: ack.YourIPAddr, Mask: mask}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("addr list: %w", err)
	}
	want := &netlink.Addr{IPNet: ipnet}
	already := false
	for _, a := range addrs {
		if a.IP.Equal(ipnet.IP) && a.Mask.String() == ipnet.Mask.String() {
			already = true
			continue
		}
		// Drop any other v4 addrs on the iface — there should only
		// ever be ours.
		_ = netlink.AddrDel(link, &a)
	}
	if !already {
		if err := netlink.AddrAdd(link, want); err != nil {
			return fmt.Errorf("addr add %s: %w", ipnet, err)
		}
	}

	// Default route via the DHCP-provided gateway, if any.
	routers := ack.Router()
	if len(routers) > 0 {
		gw := routers[0]
		// Replace any existing default route on this iface.
		if err := netlink.RouteReplace(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Gw:        gw,
			Dst:       nil, // 0.0.0.0/0
		}); err != nil {
			return fmt.Errorf("default route via %s: %w", gw, err)
		}
	}
	return nil
}

// unapplyV4Lease removes the IP we applied. Best-effort; failures are
// logged at debug because shutdown is in progress.
func (s *Supervisor) unapplyV4Lease(ack *dhcpv4.DHCPv4) {
	link, err := netlink.LinkByName(s.ifaceName)
	if err != nil {
		return
	}
	mask := ack.SubnetMask()
	if mask == nil {
		mask = net.CIDRMask(32, 32)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: ack.YourIPAddr, Mask: mask}}
	if err := netlink.AddrDel(link, addr); err != nil {
		slog.Debug("v4 addr cleanup", "err", err)
	}
}

// releaseV4 sends a DHCPRELEASE so the server marks our lease free
// rather than holding it until expiry. Best-effort: a failed release
// is not fatal — the lease will time out on the server side.
func (s *Supervisor) releaseV4(client *nclient4.Client, lease *nclient4.Lease) {
	if err := client.Release(lease); err != nil {
		slog.Debug("v4 release", "err", err)
	}
}

// runV6Once runs one full DHCPv6 lease lifecycle. On networks without
// a DHCPv6 server this returns an error after the SOLICIT timeout;
// runFamily then retries forever, which is the desired behaviour for
// SLAAC-only networks (the kernel handles RAs independently).
func (s *Supervisor) runV6Once(ctx context.Context) error {
	client, err := nclient6.New(s.ifaceName)
	if err != nil {
		return fmt.Errorf("nclient6.New: %w", err)
	}
	defer client.Close()

	// Flags=0 (S=0 O=0 N=0) — let the server decide on DNS updates,
	// per RFC 4704 §4.5 ("client wants to be told what was done").
	fqdn := dhcpv6.WithFQDN(0, s.hostname)

	solicitCtx, solicitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer solicitCancel()
	advertise, err := client.Solicit(solicitCtx, fqdn)
	if err != nil {
		return fmt.Errorf("solicit: %w", err)
	}

	requestCtx, requestCancel := context.WithTimeout(ctx, 30*time.Second)
	reply, err := client.Request(requestCtx, advertise, fqdn)
	requestCancel()
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}

	addrs := extractV6Addrs(reply)
	if len(addrs) == 0 {
		return fmt.Errorf("v6 reply contained no IA_NA addresses")
	}
	if err := s.applyV6Addrs(addrs); err != nil {
		return fmt.Errorf("apply v6 addrs: %w", err)
	}
	defer s.unapplyV6Addrs(addrs)
	metrics.DHCPAcquired.WithLabelValues("v6", "ok").Inc()
	metrics.DHCPLeaseRemaining.Set("v6", time.Now().Add(v6LeaseLifetime(reply)))
	// We don't send DHCPRELEASE for v6 here — most servers honour the
	// SOLICIT/REQUEST without persistent-binding, and a release on
	// shutdown is not as important as for v4 because IPv6 address
	// space is large and renumbering is fast.

	slog.Info("dhcp v6 lease acquired",
		"iface", s.ifaceName,
		"addrs", addrs)

	// DHCPv6 also has T1/T2; we approximate by renewing every hour.
	// The lease's IA_NA T1 would be more correct, but pure-Go
	// renewal of v6 leases requires building a RENEW message with
	// the IA_NA echoed back, which is non-trivial. For the projects
	// anchord targets (small fleets, day-scale lease times), an
	// hourly Solicit-Request from scratch is fine.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Hour):
		}
	}
}

// v6LeaseLifetime returns the smallest non-zero ValidLifetime across
// all IA addresses in the reply, or 1 hour if no usable lifetime is
// found (matches the supervisor's existing 1h re-solicit cadence).
// The smallest is the right pick — that's when *something* expires
// and we'd start to lose addresses.
func v6LeaseLifetime(msg *dhcpv6.Message) time.Duration {
	const fallback = time.Hour
	var best time.Duration
	for _, iana := range msg.Options.IANA() {
		for _, iaAddr := range iana.Options.Addresses() {
			lt := iaAddr.ValidLifetime
			if lt <= 0 {
				continue
			}
			if best == 0 || lt < best {
				best = lt
			}
		}
	}
	if best == 0 {
		return fallback
	}
	return best
}

// extractV6Addrs returns the IPv6 addresses contained in IA_NA options
// of a DHCPv6 reply. Returns nil if there are no usable addresses.
func extractV6Addrs(msg *dhcpv6.Message) []net.IP {
	var out []net.IP
	for _, ianaOpt := range msg.Options.IANA() {
		for _, iaAddr := range ianaOpt.Options.Addresses() {
			if iaAddr.IPv6Addr != nil {
				out = append(out, iaAddr.IPv6Addr)
			}
		}
	}
	return out
}

func (s *Supervisor) applyV6Addrs(addrs []net.IP) error {
	link, err := netlink.LinkByName(s.ifaceName)
	if err != nil {
		return fmt.Errorf("link lookup: %w", err)
	}
	for _, ip := range addrs {
		// /128 host address; default route comes from the kernel's RA
		// processing, not from us.
		a := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}}
		if err := netlink.AddrReplace(link, a); err != nil {
			return fmt.Errorf("addr add %s: %w", ip, err)
		}
	}
	return nil
}

func (s *Supervisor) unapplyV6Addrs(addrs []net.IP) {
	link, err := netlink.LinkByName(s.ifaceName)
	if err != nil {
		return
	}
	for _, ip := range addrs {
		a := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}}
		if err := netlink.AddrDel(link, a); err != nil {
			slog.Debug("v6 addr cleanup", "ip", ip, "err", err)
		}
	}
}

// watchIP polls the macvlan interface every second and emits whenever
// the IPv4 address changes. Cheap, robust, and decouples the IP-change
// signal from the DHCP-client internals.
func (s *Supervisor) watchIP(ctx context.Context) {
	var last net.IP
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ip := s.currentV4()
			if ip == nil {
				continue
			}
			if last == nil || !last.Equal(ip) {
				slog.Info("external IPv4 changed", "old", last, "new", ip)
				last = ip
				select {
				case s.currentIP <- ip:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (s *Supervisor) currentV4() net.IP {
	link, err := netlink.LinkByName(s.ifaceName)
	if err != nil {
		return nil
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if a.IP.IsGlobalUnicast() {
			return a.IP
		}
	}
	return nil
}
