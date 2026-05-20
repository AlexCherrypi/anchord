// Package dhcp drives the network-anchor's external addressing on a
// macvlan interface that Docker has already plumbed in.
//
// In v2 the macvlan child is no longer anchord's problem — the
// operator declares an `external: true` macvlan network in compose,
// Docker assigns the container its MAC and bootstrap IPv4 (and, if
// the network's IPAM has a v6 subnet, a bootstrap IPv6), and anchord
// just attaches to the resulting interface. What anchord still owns
// is the L3/L4 surface on that interface: optional DHCP refresh of
// the bootstrap address, optional v6 stateful DHCP, and the metrics
// that fall out of either.
//
// IPv4 and IPv6 are independent throughout — the runFamily loop runs
// per family and either can succeed/fail without affecting the other.
// The NAT plane (internal/nat) installs tables for both families
// unconditionally, so dual-stack works regardless of which families
// the LAN actually offers.
//
// Three modes (config.AddressMode):
//
//   - bootstrap: keep the Compose-assigned address(es), run nothing.
//     watchIP still polls so /metrics and event logs reflect the
//     current IP. Kernel SLAAC happens independently if RAs are seen.
//   - dhcp-refresh: run the v4 DHCP client lifecycle on the iface
//     (DISCOVER/REQUEST with a hostname-derived client-id, lease
//     renewal at T1, RELEASE on shutdown) and, in parallel, attempt
//     stateful DHCPv6 for environments that announce it. Kernel
//     SLAAC continues in parallel — anchord's v6 client coexists
//     with RA-derived addresses.
//   - slaac-ra-only: keep the Compose-assigned v4 and rely on the
//     kernel's RA/SLAAC processing for v6 entirely. Same
//     control-plane shape as bootstrap.
//
// On networks without a DHCPv6 server the v6 goroutine quietly retries
// SOLICIT forever — same end-state as kernel SLAAC via Router
// Advertisements (which the kernel handles independently of us).
package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"
	"github.com/AlexCherrypi/anchord/internal/metrics"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/vishvananda/netlink"
)

// Supervisor owns the per-family DHCP client goroutines and the IP
// watcher for the Docker-provided macvlan interface.
type Supervisor struct {
	mode       config.AddressMode
	ifaceName  string
	hostname   string
	backoffMax time.Duration

	currentIP chan net.IP
	// routers carries the DHCP-Option-3 "Router" IP from each
	// successful v4 lease. internal/extroute consumes this and
	// installs it as the network-anchor's default route (issue #6).
	// Buffered so a slow consumer never blocks the DHCP loop;
	// extroute is idempotent on duplicate values anyway.
	routers chan net.IP
}

// New constructs a Supervisor. iface is the in-container name of the
// macvlan interface Docker plumbed in (e.g. "eth0"); hostname is the
// DHCP hostname and the basis of the client-id; backoffMax caps the
// exponential retry between DHCP attempts.
func New(mode config.AddressMode, iface, hostname string, backoffMax time.Duration) *Supervisor {
	return &Supervisor{
		mode:       mode,
		ifaceName:  iface,
		hostname:   hostname,
		backoffMax: backoffMax,
		currentIP:  make(chan net.IP, 8),
		routers:    make(chan net.IP, 4),
	}
}

// IPs returns a channel that emits the current external IPv4 whenever
// it changes. The first emission signals that the kernel sees a
// usable address on the iface — in bootstrap that's the Compose-IP
// arriving via Docker, in dhcp-refresh it's either the bootstrap IP
// or the leased one (whichever the watcher samples first).
func (s *Supervisor) IPs() <-chan net.IP { return s.currentIP }

// Routers returns a channel that emits the DHCP-Option-3 "Router" IP
// from each successful v4 lease (issue #6). internal/extroute is the
// consumer; the DHCP path no longer installs the default route
// itself, so the channel is the only handoff. Returns nil never
// (the channel is created at New time); a nil receive just means
// no lease has provided a router yet. Buffered so this side never
// blocks if extroute is slow.
func (s *Supervisor) Routers() <-chan net.IP { return s.routers }

// Run blocks until ctx is cancelled. Always runs the IP watcher; only
// runs DHCP clients in dhcp-refresh mode.
func (s *Supervisor) Run(ctx context.Context) error {
	go s.watchIP(ctx)

	switch s.mode {
	case config.AddressModeDHCPRefresh:
		return s.runDHCPRefresh(ctx)
	case config.AddressModeBootstrap, config.AddressModeSLAACRAOnly:
		slog.Info("dhcp supervisor passive — no client will run",
			"mode", s.mode, "iface", s.ifaceName)
		<-ctx.Done()
		return ctx.Err()
	default:
		return fmt.Errorf("unknown address mode %q", s.mode)
	}
}

// runDHCPRefresh runs the v4 DHCP client and a best-effort v6 client
// in parallel. Returns when both have exited (only on context cancel —
// per-family DHCP errors are retried with backoff inside runFamily).
func (s *Supervisor) runDHCPRefresh(ctx context.Context) error {
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

// clientID builds the RFC 2132 §9.14 client-identifier from the
// configured hostname. Form: 0x00 (type=other) || ASCII hostname.
// Sending the same client-id across container recreates lets the DHCP
// server hand back the same reservation regardless of which MAC Docker
// happens to assign (e.g. when the user did not pin `mac_address:`).
func clientID(hostname string) []byte {
	out := make([]byte, 0, len(hostname)+1)
	out = append(out, 0x00)
	out = append(out, []byte(hostname)...)
	return out
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
	opts := []dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptHostName(s.hostname)),
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(clientID(s.hostname))),
	}
	lease, err := client.Request(requestCtx, opts...)
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
		newLease, err := client.Renew(renewCtx, lease, opts...)
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
// Docker-provided macvlan interface. Idempotent: re-running with the
// same lease is a no-op; running with a different IP replaces the
// previous one. This is the heart of dhcp-refresh: the iface starts
// with Docker's bootstrap IP, and the first lease swap replaces it.
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
		// Drop any other v4 addrs on the iface (including the Docker
		// bootstrap address if a lease landed us elsewhere).
		_ = netlink.AddrDel(link, &a)
	}
	if !already {
		if err := netlink.AddrAdd(link, want); err != nil {
			return fmt.Errorf("addr add %s: %w", ipnet, err)
		}
	}

	// Hand DHCP-Option-3 off to extroute as the default-route source
	// (issue #6 — single source of truth for the default route lives
	// in internal/extroute; dhcp's only job is to feed the gateway
	// upstream). Non-blocking: if extroute hasn't drained yet, drop
	// — extroute will see the next lease/renewal and is idempotent
	// on duplicate values.
	routers := ack.Router()
	if len(routers) > 0 {
		select {
		case s.routers <- routers[0]:
		default:
		}
	}
	return nil
}

// unapplyV4Lease removes the IP we applied. Best-effort; failures are
// logged at debug because shutdown is in progress. We deliberately do
// not try to restore the Docker bootstrap IP — the container itself is
// going away and Docker will plumb a fresh iface on next start.
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
// signal from the DHCP-client internals — works equally well for the
// Docker-bootstrap-only path (no DHCP) and the leased-IP path.
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
