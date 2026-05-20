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
// Source priority (highest first):
//
//   1. DHCP Option 3 — pushed onto the dynamic channel by
//      internal/dhcp's lease-apply path. Wins forever once it
//      arrives; the DHCP server is the L3 authority for its subnet.
//   2. ANCHORD_EXT_GATEWAY_IP — operator pin. Used before DHCP has
//      delivered, or when address-mode is bootstrap/slaac-ra-only.
//   3. NetworkInspect(ExtNetwork).IPAM.Config[].Gateway — Docker-
//      side static fallback. Used when neither DHCP nor pin set.
//   4. Nothing — no default route from this manager.
//
// Loop:
//
//   1. Resolve initial gateways: pin first; if missing, NetworkInspect.
//   2. RouteReplace default via <gw> for each family that resolved.
//   3. Every InsistInterval: re-assert. Cheap (one netlink list).
//      Re-installs if anything (Docker restart of a sibling, manual
//      operator intervention) flipped the default back to a bridge.
//   4. On each dynamic-source push: replace the effective v4 gateway
//      with the DHCP-supplied value, re-assert immediately. (v6
//      stays static — DHCPv6 has no Option 3 equivalent; the kernel
//      handles IPv6 default routing via Router Advertisements.)
//   5. ctx cancellation returns without touching the route — Docker
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
	"sync"
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
	// dynSrc is the optional DHCP-Option-3 channel from
	// internal/dhcp's lease-apply path. nil for non-dhcp-refresh
	// modes — a nil channel is never ready in select, so the loop
	// just sticks to pin/IPAM in that case.
	dynSrc <-chan net.IP

	inspect networkInspector
	router  Router

	mu             sync.Mutex
	ipamV4, ipamV6 net.IP // resolved once at startup if extNetwork set
	dynV4          net.IP // latest DHCP-Option-3 push; wins over pin/ipam
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
// pinV6 are the operator's optional ANCHORD_EXT_GATEWAY_IP override.
// dynSrc is the DHCP-Option-3 channel (typically dhcp.Supervisor.Routers());
// pass nil for non-dhcp-refresh deployments. interval=0 falls back
// to DefaultInsistInterval.
func New(cli *client.Client, extNetwork string, pinV4, pinV6 net.IP, dynSrc <-chan net.IP, interval time.Duration) *Manager {
	if interval <= 0 {
		interval = DefaultInsistInterval
	}
	return &Manager{
		extNetwork: extNetwork,
		pinV4:      pinV4,
		pinV6:      pinV6,
		dynSrc:     dynSrc,
		interval:   interval,
		inspect:    dockerInspector{cli: cli},
		router:     netlinkRouter{},
	}
}

// NewWithDeps is the test constructor.
func NewWithDeps(extNetwork string, pinV4, pinV6 net.IP, dynSrc <-chan net.IP, interval time.Duration, inspect networkInspector, router Router) *Manager {
	if interval <= 0 {
		interval = DefaultInsistInterval
	}
	return &Manager{
		extNetwork: extNetwork,
		pinV4:      pinV4,
		pinV6:      pinV6,
		dynSrc:     dynSrc,
		interval:   interval,
		inspect:    inspect,
		router:     router,
	}
}

// Run blocks until ctx is cancelled. Resolves the initial fallback
// gateways (pin > IPAM) at startup, asserts the effective default
// route, then on every tick OR every dynamic-source push re-asserts.
// Returns ctx.Err() — typically context.Canceled.
//
// Loop never bails: even when no gateway is resolvable at startup,
// the loop still ticks so a DHCP-Option-3 push that arrives later
// gets honoured. Enforcement is a QoL feature; failures here don't
// kill the data plane.
func (m *Manager) Run(ctx context.Context) error {
	// Startup IPAM lookup — only if there's an ext_network and at
	// least one family lacks a pin. (Pin always wins over IPAM, so
	// skipping the lookup when both pins are set saves one Docker
	// round-trip.)
	if m.extNetwork != "" && (m.pinV4 == nil || m.pinV6 == nil) {
		ipamV4, ipamV6, err := m.inspect.Gateways(ctx, m.extNetwork)
		if err != nil {
			slog.Warn("ext IPAM lookup failed; will fall back to pin / DHCP only",
				"ext_network", m.extNetwork, "err", err)
		} else {
			m.mu.Lock()
			m.ipamV4, m.ipamV6 = ipamV4, ipamV6
			m.mu.Unlock()
		}
	}

	slog.Info("ext default-route enforcer starting",
		"ext_network", m.extNetwork,
		"pin_v4", m.pinV4, "pin_v6", m.pinV6,
		"ipam_v4", m.ipamV4, "ipam_v6", m.ipamV6,
		"interval", m.interval)

	m.assertAll()

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.assertAll()
		case gw, ok := <-m.dynSrc:
			if !ok {
				// dhcp closed the channel — disable dynamic source
				// and keep ticking with pin/IPAM.
				m.dynSrc = nil
				continue
			}
			if gw == nil || gw.To4() == nil {
				continue
			}
			m.mu.Lock()
			changed := !gw.To4().Equal(m.dynV4)
			m.dynV4 = gw.To4()
			m.mu.Unlock()
			if changed {
				slog.Info("ext default-route dynamic source updated",
					"family", "v4", "gw", gw)
			}
			m.assert(unix.AF_INET, m.effectiveV4())
		}
	}
}

// effectiveV4 picks the active v4 gateway by priority: dynamic > pin > IPAM.
func (m *Manager) effectiveV4() net.IP {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.dynV4 != nil:
		return m.dynV4
	case m.pinV4 != nil:
		return m.pinV4
	default:
		return m.ipamV4
	}
}

// effectiveV6 picks the active v6 gateway: pin > IPAM. No dynamic
// source for IPv6 — DHCPv6 has no Option 3 equivalent.
func (m *Manager) effectiveV6() net.IP {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pinV6 != nil {
		return m.pinV6
	}
	return m.ipamV6
}

// assertAll re-asserts both families.
func (m *Manager) assertAll() {
	m.assert(unix.AF_INET, m.effectiveV4())
	m.assert(unix.AF_INET6, m.effectiveV6())
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
