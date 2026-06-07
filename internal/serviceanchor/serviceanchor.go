// Package serviceanchor implements anchord's service-anchor mode.
//
// A service-anchor is a small container that owns a Docker network
// namespace which one or more application containers join via
// `network_mode: service:<anchor>`. Its job is to keep that namespace
// equipped with a default route pointing at the project's
// network-anchor, so traffic going to or coming from anywhere outside
// the transit /64 (or /24) actually reaches it.
//
// The mode operates entirely from inside its own netns. It does NOT
// touch the Docker socket; the only outside dependency is Docker's
// embedded DNS resolver, which we use to look up the network-anchor
// by hostname.
//
// Loop:
//
//  1. Resolve ANCHORD_GATEWAY_HOSTNAME (default "anchord") via DNS.
//  2. For each address family that resolved, install or replace the
//     default route via that address using netlink RouteReplace
//     (atomic with respect to the kernel forwarding plane).
//  3. Re-resolve every ANCHORD_GATEWAY_RESOLVE_INTERVAL (default 5 s);
//     replace routes when the resolved address changes.
//  4. On context cancellation, remove the routes and return.
//
// See SPEC §2.6 (F-24..F-29) for the formal contract.
package serviceanchor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"
	"github.com/AlexCherrypi/anchord/internal/metrics"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Manager keeps a service-anchor's default route(s) pointing at the
// network-anchor's current transit address.
type Manager struct {
	cfg *config.ServiceAnchor

	// resolver is the DNS lookup function. Real code uses net.DefaultResolver;
	// tests inject a stub.
	resolver Resolver

	// router applies route changes. Real code uses the netlink package;
	// tests inject a stub.
	router Router

	// now provides the wall clock; tests can override.
	now func() time.Time

	// OnRouteInstalled, if set, is invoked after every successful
	// route install. Used by main to flip the readiness Tracker once
	// at least one default route is in place (SPEC F-35).
	OnRouteInstalled func()

	mu      sync.Mutex
	current map[int]net.IP // family (unix.AF_INET / AF_INET6) -> last-installed gateway

	// original is the per-family default gateway recorded at startup
	// (F-39). In greenfield mode the target netns has no default
	// route on internal:true bridges → original stays empty and
	// shutdown just removes our own routes (same as today). In wrap
	// mode (network_mode: container:) the target's Docker bridge
	// default gateway is captured here; cleanup restores it after
	// removing our own, so the wrapped app retains egress when the
	// wrap-stack is torn down.
	original map[int]net.IP
}

// Resolver looks up a hostname and returns its IPs. Mockable for tests.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

// Router installs and removes default routes. Mockable for tests.
type Router interface {
	// ReplaceDefaultRoute installs or replaces the default route for
	// the given family, pointing at gw. Atomic via netlink RouteReplace.
	ReplaceDefaultRoute(family int, gw net.IP) error
	// RemoveDefaultRoute deletes the default route via gw (best-effort).
	RemoveDefaultRoute(family int, gw net.IP) error
	// RecordDefaultRoute returns the gateway currently set as the
	// default route for family, or nil if none exists. Used by F-39
	// wrap mode: when service-anchor enters a target container's
	// netns via `network_mode: container:`, the target already has
	// Docker's bridge gateway as default; we capture it at startup
	// and restore it on shutdown so the wrapped app retains egress
	// after the wrap-stack is torn down.
	RecordDefaultRoute(family int) (net.IP, error)
}

// New constructs a Manager with the production resolver and netlink router.
func New(cfg *config.ServiceAnchor) *Manager {
	return &Manager{
		cfg:      cfg,
		resolver: defaultResolver{},
		router:   netlinkRouter{},
		now:      time.Now,
		current:  map[int]net.IP{},
		original: map[int]net.IP{},
	}
}

// NewWithDeps is a constructor used by tests to inject a fake resolver/router.
func NewWithDeps(cfg *config.ServiceAnchor, r Resolver, rt Router) *Manager {
	return &Manager{
		cfg:      cfg,
		resolver: r,
		router:   rt,
		now:      time.Now,
		current:  map[int]net.IP{},
		original: map[int]net.IP{},
	}
}

// Run blocks until ctx is cancelled. It returns ctx.Err() — typically
// context.Canceled, which the caller treats as a clean exit.
//
// Two operating modes (SPEC F-40):
//   - IP mode (cfg.GatewayIPs non-empty): install the configured
//     addresses as default routes once and block until ctx is done.
//     No periodic resolve loop — the configured IPs are static, and
//     skipping the tick avoids log noise and saves a few cycles.
//   - DNS mode (default): resolve cfg.GatewayHostname every
//     cfg.ResolveInterval and reconcile against the result.
func (m *Manager) Run(ctx context.Context) error {
	if len(m.cfg.GatewayIPs) > 0 {
		return m.runIPMode(ctx)
	}
	return m.runDNSMode(ctx)
}

// runIPMode installs the configured gateway IPs as default routes
// and waits. Used when the operator supplied ANCHORD_GATEWAY_IP.
func (m *Manager) runIPMode(ctx context.Context) error {
	slog.Info("service-anchor starting (IP mode)",
		"gateway_ips", m.cfg.GatewayIPs)
	m.recordOriginals()
	for _, ip := range m.cfg.GatewayIPs {
		fam := unix.AF_INET
		if ip.To4() == nil {
			fam = unix.AF_INET6
		}
		m.applyRoute(fam, ip)
	}
	<-ctx.Done()
	m.cleanup()
	return ctx.Err()
}

// runDNSMode is the existing DNS-resolve loop (F-24..F-26).
func (m *Manager) runDNSMode(ctx context.Context) error {
	slog.Info("service-anchor starting (DNS mode)",
		"gateway_hostname", m.cfg.GatewayHostname,
		"resolve_interval", m.cfg.ResolveInterval)

	m.recordOriginals()

	// First reconcile is best-effort: the network-anchor may not be
	// up yet. We log warnings but don't return errors — the loop
	// will keep trying.
	m.reconcile(ctx)

	t := time.NewTicker(m.cfg.ResolveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.cleanup()
			return ctx.Err()
		case <-t.C:
			m.reconcile(ctx)
		}
	}
}

// recordOriginals captures the existing default route(s) per family
// at startup, before any of our own routes are installed. Used to
// restore the wrapped target's egress on SIGTERM (F-39).
//
// Best-effort: a netlink error here is logged as a warning and the
// per-family slot is left empty — startup proceeds. In greenfield
// mode (no pre-existing default route, e.g. internal:true bridges)
// both lookups return nil and original stays empty; cleanup behaves
// exactly as before F-39.
func (m *Manager) recordOriginals() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, fam := range []int{unix.AF_INET, unix.AF_INET6} {
		gw, err := m.router.RecordDefaultRoute(fam)
		if err != nil {
			slog.Warn("could not record original default route",
				"family", familyName(fam), "err", err)
			continue
		}
		if gw == nil {
			// No default route in this family — common for greenfield
			// service-anchors on internal:true bridges. Nothing to
			// restore on shutdown for this family.
			continue
		}
		m.original[fam] = gw
		slog.Info("recorded existing default route for restore on shutdown",
			"family", familyName(fam), "gateway", gw)
	}
}

// reconcile resolves the gateway hostname and reconciles the kernel's
// default route(s) against the result. It is best-effort: any failure
// is logged and the existing routes (if any) are left in place, on
// the assumption that a transient lookup failure is better handled
// by leaving the last-known-good state than by ripping out routing.
func (m *Manager) reconcile(ctx context.Context) {
	addrs, err := m.resolver.LookupIP(ctx, m.cfg.GatewayHostname)
	if err != nil {
		// One lookup failure increments error for both families —
		// the resolver couldn't tell us anything, including whether
		// either family is reachable. That's the honest signal.
		metrics.GatewayResolve.WithLabelValues("v4", "error").Inc()
		metrics.GatewayResolve.WithLabelValues("v6", "error").Inc()
		slog.Warn("gateway DNS lookup failed",
			"host", m.cfg.GatewayHostname, "err", err)
		return
	}

	// Pick the first usable address per family. Multiple-A / multiple-AAAA
	// is rare in Compose; if it happens we accept the resolver's order.
	var v4, v6 net.IP
	for _, a := range addrs {
		if a4 := a.To4(); a4 != nil {
			if v4 == nil {
				v4 = a4
			}
		} else if a6 := a.To16(); a6 != nil {
			if v6 == nil {
				v6 = a6
			}
		}
	}

	if v4 != nil {
		metrics.GatewayResolve.WithLabelValues("v4", "ok").Inc()
		m.applyRoute(unix.AF_INET, v4)
	} else {
		metrics.GatewayResolve.WithLabelValues("v4", "error").Inc()
	}
	if v6 != nil {
		metrics.GatewayResolve.WithLabelValues("v6", "ok").Inc()
		m.applyRoute(unix.AF_INET6, v6)
	} else {
		metrics.GatewayResolve.WithLabelValues("v6", "error").Inc()
	}
}

// applyRoute installs or replaces the default route for one family.
//
// Defense against external flushes (e.g. `ip route del default`, an
// unrelated tool in the netns, a kernel quirk, a sidecar bug): instead
// of trusting an in-memory cache of "last gateway we installed", we
// read the kernel's *current* default gateway each tick and only
// short-circuit when it matches what we want.
//
// That mirrors the extroute package's `assert` pattern (issue #6).
// Why bother: an in-memory short-circuit silently misses the case
// where the resolved IP hasn't changed but the kernel route is gone —
// anchord would believe everything is fine forever. The kernel read
// is one cheap netlink list per family per tick (≈two syscalls at
// 5 s cadence; negligible cost for a hard correctness guarantee).
func (m *Manager) applyRoute(family int, gw net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Read kernel state. A lookup error is logged at debug and we
	// fall through to the unconditional Replace below — Replace is
	// idempotent so we can't make things worse, and the alternative
	// (skip on error) would be the very passive behaviour we're
	// fixing here.
	kernelGW, err := m.router.RecordDefaultRoute(family)
	if err != nil {
		slog.Debug("default route lookup failed; will replace unconditionally",
			"family", familyName(family), "err", err)
	}
	if kernelGW != nil && kernelGW.Equal(gw) {
		// Kernel already has our gateway. Cache the value for
		// cleanup()'s benefit and short-circuit.
		m.current[family] = gw
		return
	}

	reason := "drifted"
	if kernelGW == nil {
		reason = "missing"
	}

	if err := m.router.ReplaceDefaultRoute(family, gw); err != nil {
		slog.Warn("default route install failed",
			"family", familyName(family), "gw", gw,
			"reason", reason, "prev", kernelGW, "err", err)
		return
	}
	m.current[family] = gw
	metrics.GatewayRouteReplaces.WithLabelValues(familyName(family)).Inc()
	metrics.DefaultRoutePresent.WithLabelValues(familyName(family)).Set(1)
	slog.Info("default route updated",
		"family", familyName(family),
		"gateway", gw,
		"reason", reason,
		"prev", kernelGW)
	if m.OnRouteInstalled != nil {
		m.OnRouteInstalled()
	}
}

// cleanup removes default routes the manager installed, on shutdown.
// Errors during removal are logged at debug because the route may
// already be gone (e.g. interface taken down externally).
//
// F-39: after our own routes are removed, restore any default routes
// recorded at startup. This is the wrap-pattern case where the
// service-anchor joined an existing app container's netns: removing
// our route alone would leave the wrapped app with no default gateway
// until Docker recreates the container; restoring puts the
// Docker-managed bridge gateway back so the app keeps working.
//
// Restore failures are logged at warn (operator-visible) since they
// leave the target in a broken state.
func (m *Manager) cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for family, gw := range m.current {
		if err := m.router.RemoveDefaultRoute(family, gw); err != nil {
			slog.Debug("default route cleanup",
				"family", familyName(family), "gw", gw, "err", err)
			continue
		}
		metrics.DefaultRoutePresent.WithLabelValues(familyName(family)).Set(0)
		slog.Info("default route removed",
			"family", familyName(family), "gateway", gw)
	}
	for family, gw := range m.original {
		// Skip restore if the original IS the current — that would
		// mean we never replaced anything (e.g. our resolve never
		// produced a route). Nothing to put back.
		if cur, replaced := m.current[family]; !replaced || cur.Equal(gw) {
			continue
		}
		if err := m.router.ReplaceDefaultRoute(family, gw); err != nil {
			slog.Warn("could not restore original default route",
				"family", familyName(family), "gw", gw, "err", err)
			continue
		}
		slog.Info("restored original default route on shutdown",
			"family", familyName(family), "gateway", gw)
	}
	m.current = map[int]net.IP{}
	m.original = map[int]net.IP{}
}

func familyName(f int) string {
	switch f {
	case unix.AF_INET:
		return "v4"
	case unix.AF_INET6:
		return "v6"
	}
	return fmt.Sprintf("af=%d", f)
}

// ---- production resolver / router ---------------------------------------

type defaultResolver struct{}

func (defaultResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.IP
	}
	return out, nil
}

type netlinkRouter struct{}

func (netlinkRouter) ReplaceDefaultRoute(family int, gw net.IP) error {
	route := defaultRouteFor(family, gw)
	if route == nil {
		return errors.New("invalid gateway address for family")
	}
	return netlink.RouteReplace(route)
}

func (netlinkRouter) RemoveDefaultRoute(family int, gw net.IP) error {
	route := defaultRouteFor(family, gw)
	if route == nil {
		return errors.New("invalid gateway address for family")
	}
	return netlink.RouteDel(route)
}

// RecordDefaultRoute lists routes for the given family and returns the
// gateway of the existing default route (Dst==0.0.0.0/0 or ::/0), or
// nil if none is installed. Errors from netlink are returned verbatim;
// the caller treats them as "could not record" and proceeds with a
// warning — better than failing startup entirely just because we
// couldn't snapshot a fallback route.
func (netlinkRouter) RecordDefaultRoute(family int) (net.IP, error) {
	var nlFamily int
	switch family {
	case unix.AF_INET:
		nlFamily = netlink.FAMILY_V4
	case unix.AF_INET6:
		nlFamily = netlink.FAMILY_V6
	default:
		return nil, fmt.Errorf("unsupported family %d", family)
	}
	routes, err := netlink.RouteList(nil, nlFamily)
	if err != nil {
		return nil, fmt.Errorf("RouteList: %w", err)
	}
	for _, r := range routes {
		// A default route has either Dst nil or Dst with a /0 mask.
		if r.Dst == nil || isAllZerosCIDR(r.Dst) {
			if r.Gw != nil {
				return r.Gw, nil
			}
		}
	}
	return nil, nil
}

// isAllZerosCIDR reports whether ipnet is 0.0.0.0/0 or ::/0.
func isAllZerosCIDR(ipnet *net.IPNet) bool {
	if ipnet == nil {
		return false
	}
	ones, _ := ipnet.Mask.Size()
	return ones == 0 && ipnet.IP.IsUnspecified()
}

// defaultRouteFor builds a netlink.Route for the family's all-zeros
// destination ("default") via the given gateway. The kernel resolves
// the outgoing interface from the gateway's on-link reachability —
// no LinkIndex needed when our transit interface owns the matching
// subnet, which is the canonical anchord layout.
func defaultRouteFor(family int, gw net.IP) *netlink.Route {
	var dst *net.IPNet
	switch family {
	case unix.AF_INET:
		v4 := gw.To4()
		if v4 == nil {
			return nil
		}
		dst = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
		gw = v4
	case unix.AF_INET6:
		if gw.To4() != nil {
			return nil
		}
		dst = &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
	default:
		return nil
	}
	return &netlink.Route{
		Dst: dst,
		Gw:  gw,
	}
}
