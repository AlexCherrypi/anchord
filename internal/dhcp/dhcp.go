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

		// Run dhclient under a child context so we can cancel it
		// independently of the supervisor's own context. dhclient
		// does NOT exit when the interface it's bound to is deleted
		// — it just retries internally and prints
		// "receive_packet failed on …: Network is down". Without an
		// external watcher we'd never re-run ensureLink. The watcher
		// goroutine cancels the child context as soon as the link
		// stops being usable, which kills dhclient (exec.CommandContext
		// SIGKILLs on cancel) and falls back into this loop's
		// ensureLink path.
		dhCtx, dhCancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go s.watchLinkUsable(dhCtx, dhCancel, watchDone)

		err := s.runDhclient(dhCtx)
		dhCancel()
		<-watchDone

		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("dhclient exited, retrying",
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
				slog.Warn("macvlan child no longer usable, killing dhclient",
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

// runDhclient runs `dhclient -d` in the foreground, attached to the
// macvlan, and returns when the process exits. -d keeps it foreground;
// -v gets us log output we can capture; -1 would make it one-shot but
// we want continuous renewal handling, so we omit it.
//
// Hostname is announced to the DHCP server via a generated config file
// (send host-name "..."). The -H flag is not portable — Alpine's ISC
// dhclient build does not accept it.
func (s *Supervisor) runDhclient(ctx context.Context) error {
	confPath, cleanup, err := s.writeDhclientConf()
	if err != nil {
		return fmt.Errorf("write dhclient.conf: %w", err)
	}
	defer cleanup()

	args := []string{
		"-d",          // foreground
		"-v",          // verbose
		"-cf", confPath,
		s.ifaceName,
	}
	cmd := exec.CommandContext(ctx, "dhclient", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	slog.Info("starting dhclient", "iface", s.ifaceName, "hostname", s.hostname)
	return cmd.Run()
}

// writeDhclientConf produces a minimal dhclient.conf that asks the
// server to record our hostname. The returned cleanup removes the
// temp file on completion.
func (s *Supervisor) writeDhclientConf() (string, func(), error) {
	// `send host-name` is the standard ISC dhclient incantation. We
	// quote the hostname; any embedded `"` or `\` would need escaping
	// but anchord's hostname comes from project name / env, neither
	// of which legitimately contains those characters.
	content := fmt.Sprintf("send host-name \"%s\";\n", s.hostname)
	f, err := os.CreateTemp("", "anchord-dhclient-*.conf")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
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
