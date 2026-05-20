// Package extroute enforces the network-anchor's default route on the
// external (macvlan) interface — issue #6.
//
// Docker's default-route placement among multiple networks on a
// container is not deterministic across versions; empirically the
// bridge consistently wins over the macvlan even with the compose
// `priority:` field set on macvlan. With the default route on a
// bridge, the network-anchor's reply packets for clients outside the
// DMZ subnet leave the host via the bridge's L3 path, asymmetrically
// to the forward path through the macvlan — pf misses the state and
// long-lived TCP sessions (IMAP IDLE etc.) die at the TCP RTO
// terminal point after ~17 minutes.
//
// Loop:
//
//   1. Resolve the macvlan gateway. Operator pin via
//      ANCHORD_EXT_GATEWAY_IP wins; otherwise read from Docker
//      NetworkInspect(ExtNetwork).IPAM.Config[].Gateway.
//   2. RouteReplace default via <gw> for each family that resolved.
//   3. Every InsistInterval: re-assert. Cheap (one netlink list).
//      Re-installs if anything (Docker restart of a sibling, manual
//      operator intervention) flipped the default back to a bridge.
//   4. ctx cancellation returns without touching the route — Docker
//      handles netns teardown on container exit; the kernel reaps the
//      route automatically. No restore step (unlike serviceanchor's
//      F-29 wrap-restore) because there's nothing useful to restore:
//      Docker's bridge default was the *wrong* route to begin with.
package extroute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// DefaultInsistInterval is the periodic re-assert tick.
const DefaultInsistInterval = 5 * time.Second

// Manager keeps the network-anchor's default route on the external
// (macvlan) iface. Construct via New for production; tests use
// NewWithDeps to inject fakes.
type Manager struct {
	extNetwork string
	pinV4      net.IP
	pinV6      net.IP
	interval   time.Duration

	inspect networkInspector
	router  Router
}

// Router is the netlink surface this package uses. Mockable for tests.
type Router interface {
	// CurrentDefaultGateway returns the gateway of the current default
	// route for family, or nil if no default is installed.
	CurrentDefaultGateway(family int) (net.IP, error)
	// ReplaceDefaultRoute installs (or replaces) the default route
	// for family via gw. Atomic netlink RouteReplace.
	ReplaceDefaultRoute(family int, gw net.IP) error
}

// networkInspector returns the IPv4 / IPv6 gateways declared in the
// IPAM config of the named Docker network. Either may be nil when
// no gateway is declared for that family.
type networkInspector interface {
	Gateways(ctx context.Context, networkName string) (v4, v6 net.IP, err error)
}

// New constructs a Manager wired to the live Docker client and
// vishvananda/netlink. extNetwork is ANCHORD_EXT_NETWORK; pinV4 and
// pinV6 are the operator's optional ANCHORD_EXT_GATEWAY_IP override
// (either may be nil to leave that family's gateway to inspection).
// interval=0 falls back to DefaultInsistInterval.
func New(cli *client.Client, extNetwork string, pinV4, pinV6 net.IP, interval time.Duration) *Manager {
	if interval <= 0 {
		interval = DefaultInsistInterval
	}
	return &Manager{
		extNetwork: extNetwork,
		pinV4:      pinV4,
		pinV6:      pinV6,
		interval:   interval,
		inspect:    dockerInspector{cli: cli},
		router:     netlinkRouter{},
	}
}

// NewWithDeps is the test constructor.
func NewWithDeps(extNetwork string, pinV4, pinV6 net.IP, interval time.Duration, inspect networkInspector, router Router) *Manager {
	if interval <= 0 {
		interval = DefaultInsistInterval
	}
	return &Manager{
		extNetwork: extNetwork,
		pinV4:      pinV4,
		pinV6:      pinV6,
		interval:   interval,
		inspect:    inspect,
		router:     router,
	}
}

// Run blocks until ctx is cancelled. Resolves the macvlan gateway
// once at startup, asserts the default route, then re-asserts on
// each tick. Returns ctx.Err() — typically context.Canceled, which
// the caller treats as a clean exit.
//
// Skips silently (waits for ctx) when no gateway is resolvable: an
// empty ExtNetwork without an operator pin, or a NetworkInspect
// failure on the resolved network. Default-route enforcement is a
// quality-of-life feature; we don't kill the data plane over it.
func (m *Manager) Run(ctx context.Context) error {
	gwV4, gwV6, err := m.resolveGateways(ctx)
	if err != nil {
		slog.Warn("ext default-route enforcement disabled — gateway not resolved",
			"ext_network", m.extNetwork, "err", err)
		<-ctx.Done()
		return ctx.Err()
	}
	if gwV4 == nil && gwV6 == nil {
		slog.Info("ext default-route enforcement skipped — no gateway available",
			"ext_network", m.extNetwork)
		<-ctx.Done()
		return ctx.Err()
	}

	slog.Info("ext default-route enforcer starting",
		"ext_network", m.extNetwork,
		"gw_v4", gwV4,
		"gw_v6", gwV6,
		"interval", m.interval)

	m.assert(unix.AF_INET, gwV4)
	m.assert(unix.AF_INET6, gwV6)

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.assert(unix.AF_INET, gwV4)
			m.assert(unix.AF_INET6, gwV6)
		}
	}
}

// resolveGateways picks the per-family gateway address: operator pin
// wins; otherwise NetworkInspect on the macvlan network.
func (m *Manager) resolveGateways(ctx context.Context) (v4, v6 net.IP, err error) {
	if m.pinV4 != nil || m.pinV6 != nil {
		// Mixed pin / inspect is not supported: if the operator pinned
		// either family, treat the other as deliberately unset. Same
		// rule as the service-anchor's ANCHORD_GATEWAY_IP.
		return m.pinV4, m.pinV6, nil
	}
	if m.extNetwork == "" {
		return nil, nil, errors.New("ANCHORD_EXT_NETWORK empty and no ANCHORD_EXT_GATEWAY_IP set")
	}
	return m.inspect.Gateways(ctx, m.extNetwork)
}

// assert installs the default route via gw for the given family,
// unless it's already in place. No-ops on gw==nil.
func (m *Manager) assert(family int, gw net.IP) {
	if gw == nil {
		return
	}
	cur, err := m.router.CurrentDefaultGateway(family)
	if err != nil {
		slog.Debug("ext default-route lookup",
			"family", familyName(family), "err", err)
	}
	if cur != nil && cur.Equal(gw) {
		return
	}
	if err := m.router.ReplaceDefaultRoute(family, gw); err != nil {
		slog.Warn("ext default-route replace failed",
			"family", familyName(family), "gw", gw, "err", err)
		return
	}
	slog.Info("ext default route enforced",
		"family", familyName(family),
		"gw", gw,
		"prev", cur)
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

// ---- production inspector / router --------------------------------------

type dockerInspector struct {
	cli *client.Client
}

func (d dockerInspector) Gateways(ctx context.Context, name string) (v4, v6 net.IP, err error) {
	insp, err := d.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("NetworkInspect(%s): %w", name, err)
	}
	for _, cfg := range insp.IPAM.Config {
		gw := net.ParseIP(cfg.Gateway)
		if gw == nil {
			continue
		}
		if gw.To4() != nil && v4 == nil {
			v4 = gw.To4()
		} else if gw.To4() == nil && v6 == nil {
			v6 = gw
		}
	}
	return v4, v6, nil
}

type netlinkRouter struct{}

func (netlinkRouter) CurrentDefaultGateway(family int) (net.IP, error) {
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
		if r.Dst == nil || isAllZerosCIDR(r.Dst) {
			if r.Gw != nil {
				return r.Gw, nil
			}
		}
	}
	return nil, nil
}

func (netlinkRouter) ReplaceDefaultRoute(family int, gw net.IP) error {
	r := defaultRouteFor(family, gw)
	if r == nil {
		return errors.New("invalid gateway for family")
	}
	return netlink.RouteReplace(r)
}

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
	return &netlink.Route{Dst: dst, Gw: gw}
}

func isAllZerosCIDR(n *net.IPNet) bool {
	if n == nil {
		return false
	}
	ones, _ := n.Mask.Size()
	return ones == 0 && n.IP.IsUnspecified()
}
