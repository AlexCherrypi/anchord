// Package dhcp manages the macvlan interface and DHCP client lifecycle
// inside the anchord container.
//
// We shell out to dhclient for v0.1 — pure-Go DHCP clients exist but
// dhclient's hook system, lease persistence and renewal logic are
// battle-tested in ways we don't want to reinvent. The subprocess is
// supervised with exponential backoff capped at the user's chosen max.
package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/vishvananda/netlink"
)

// Supervisor owns the external macvlan link and the dhclient process.
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
// dhclient subprocess with exponential backoff (capped at backoffMax).
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.ensureLink(); err != nil {
		return fmt.Errorf("ensure link: %w", err)
	}
	defer s.removeLink()

	go s.watchIP(ctx)

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := s.runDhclient(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("dhclient exited, retrying", "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > s.backoffMax {
			backoff = s.backoffMax
		}
	}
}

// ensureLink creates the macvlan link if missing, sets its MAC, and
// brings it up. Idempotent.
func (s *Supervisor) ensureLink() error {
	parent, err := netlink.LinkByName(s.parentName)
	if err != nil {
		return fmt.Errorf("parent %s: %w", s.parentName, err)
	}

	// Remove any stale link from a previous run.
	if existing, err := netlink.LinkByName(s.ifaceName); err == nil {
		_ = netlink.LinkDel(existing)
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

func (s *Supervisor) removeLink() {
	if l, err := netlink.LinkByName(s.ifaceName); err == nil {
		_ = netlink.LinkDel(l)
		slog.Info("macvlan removed", "iface", s.ifaceName)
	}
}

// runDhclient runs `dhclient -d` in the foreground, attached to the
// macvlan, and returns when the process exits. -d keeps it foreground;
// -v gets us log output we can capture; -1 would make it one-shot but
// we want continuous renewal handling, so we omit it.
func (s *Supervisor) runDhclient(ctx context.Context) error {
	args := []string{
		"-d", // foreground
		"-v", // verbose
		"-H", s.hostname,
		s.ifaceName,
	}
	cmd := exec.CommandContext(ctx, "dhclient", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	slog.Info("starting dhclient", "iface", s.ifaceName, "hostname", s.hostname)
	return cmd.Run()
}

// watchIP polls the macvlan interface every second and emits whenever
// the IPv4 address changes. Cheap, robust, and avoids any dhclient hook
// scripting.
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
