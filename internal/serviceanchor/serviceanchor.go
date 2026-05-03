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
}

// Resolver looks up a hostname and returns its IPs. Mockable for tests.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

// Router installs and removes default routes. Mockable for tests.
type Router interface {
	ReplaceDefaultRoute(family int, gw net.IP) error
	RemoveDefaultRoute(family int, gw net.IP) error
}

// New constructs a Manager with the production resolver and netlink router.
func New(cfg *config.ServiceAnchor) *Manager {
	return &Manager{
		cfg:      cfg,
		resolver: defaultResolver{},
		router:   netlinkRouter{},
		now:      time.Now,
		current:  map[int]net.IP{},
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
	}
}

// Run blocks until ctx is cancelled. It returns ctx.Err() — typically
// context.Canceled, which the caller treats as a clean exit.
func (m *Manager) Run(ctx context.Context) error {
	slog.Info("service-anchor starting",
		"gateway_hostname", m.cfg.GatewayHostname,
		"resolve_interval", m.cfg.ResolveInterval)

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

// applyRoute installs or replaces the default route for one family if
// it differs from the last-applied gateway.
func (m *Manager) applyRoute(family int, gw net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cur, ok := m.current[family]; ok && cur.Equal(gw) {
		return
	}
	if err := m.router.ReplaceDefaultRoute(family, gw); err != nil {
		slog.Warn("default route install failed",
			"family", familyName(family), "gw", gw, "err", err)
		return
	}
	m.current[family] = gw
	metrics.GatewayRouteReplaces.WithLabelValues(familyName(family)).Inc()
	metrics.DefaultRoutePresent.WithLabelValues(familyName(family)).Set(1)
	slog.Info("default route updated",
		"family", familyName(family), "gateway", gw)
	if m.OnRouteInstalled != nil {
		m.OnRouteInstalled()
	}
}

// cleanup removes default routes the manager installed, on shutdown.
// Errors are logged at debug because the route may already be gone
// (e.g. interface taken down externally).
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
	m.current = map[int]net.IP{}
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
