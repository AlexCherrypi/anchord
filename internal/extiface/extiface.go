// Package extiface resolves the network-anchor's external interface
// by Docker-network name rather than by in-container iface name.
//
// Motivation (SPEC-v2-DRAFT F-37): Docker's eth0/eth1 assignment on a
// multi-network container is not deterministic across recreates —
// empirical measurement gave a 4:4 split over 8 sequential redeploys
// of the same compose file. Picking the external interface purely by
// `ANCHORD_EXT_IFACE=eth0` is therefore a coin flip for realistic v2
// stacks (dmz macvlan + transit bridge). When the operator supplies
// `ANCHORD_EXT_NETWORK=<docker-net-name>`, anchord asks the Docker API
// for its own container's NetworkSettings[<name>].MacAddress and
// matches that MAC to a local netlink interface — name-agnostic.
//
// Resolution is one-shot at startup: Docker only changes the
// container-to-iface assignment on a full recreate, which re-runs the
// whole binary. The compose-up race (network not yet attached when
// the container starts) is handled by an exponential-backoff retry
// (100 ms → cap 2 s) within a 10 s window.
//
// All three failure modes (network missing, MAC missing, API
// unreachable) are fatal after the retry window — anchord without a
// correctly-resolved external iface is not functional.
package extiface

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// DefaultDeadline is the total wall-clock budget for resolution
// retries. After this, attempt errors become fatal.
const DefaultDeadline = 10 * time.Second

// ErrNetworkNotFound means the Docker API responded but the requested
// network is not present in NetworkSettings.Networks. After the retry
// window this almost always means a configuration typo (the
// ANCHORD_EXT_NETWORK value doesn't match any attached network's name).
var ErrNetworkNotFound = errors.New("network not found in container's NetworkSettings")

// ErrMACNotFound means the Docker API reported a MAC for the network
// but no local netlink interface carries that MAC. Indicates a sync
// problem between Docker's view and the netns it set up.
var ErrMACNotFound = errors.New("MAC from docker not present on any local interface")

// inspector is the slice of the Docker client surface this package
// uses. Kept narrow so unit tests can swap in fakes without standing
// up a fake Docker daemon.
type inspector func(ctx context.Context, id string) (NetworkMACs, error)

// linkResolver returns the local interface name whose hardware
// address matches the supplied MAC, or ErrMACNotFound if none does.
type linkResolver func(mac net.HardwareAddr) (string, error)

// NetworkMACs is the minimal projection of ContainerInspect output
// we care about: each attached Docker network mapped to the MAC
// Docker stamped onto our endpoint there. Empty string values are
// preserved (some Docker setups omit the MAC on bridge endpoints) so
// the caller can distinguish "no MAC reported" from "network absent".
type NetworkMACs map[string]string

// Resolver is the production entry point. Construct via New for live
// use; unit tests construct it directly with fake inspector / link
// resolver functions.
type Resolver struct {
	inspect  inspector
	resolve  linkResolver
	selfHost string
	deadline time.Duration
}

// New wires a Resolver against a live Docker client and the netlink
// link list. selfHost should be os.Hostname(), which Docker sets to
// the short container ID — the form ContainerInspect accepts as an
// identifier.
func New(cli *client.Client, selfHost string) *Resolver {
	return &Resolver{
		inspect:  inspectFromDocker(cli),
		resolve:  resolveByMAC,
		selfHost: selfHost,
		deadline: DefaultDeadline,
	}
}

// Resolve looks up the local interface attached to network, retrying
// with exponential backoff (100 ms → cap 2 s) until success or the
// 10 s deadline. Returns the iface name, or a wrapped error with one
// of the sentinel errors above (ErrNetworkNotFound, ErrMACNotFound,
// or a wrapped Docker API failure).
func (r *Resolver) Resolve(ctx context.Context, network string) (string, error) {
	if network == "" {
		return "", errors.New("ExtNetwork is empty; caller should fall back to ExtIfaceName")
	}
	deadline := time.Now().Add(r.deadline)
	backoff := 100 * time.Millisecond
	var lastErr error
	for {
		iface, err := r.attempt(ctx, network)
		if err == nil {
			return iface, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", lastErr
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", lastErr
			}
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// attempt runs a single resolution cycle: inspect self, find the
// network's MAC, find the local iface with that MAC.
func (r *Resolver) attempt(ctx context.Context, network string) (string, error) {
	nets, err := r.inspect(ctx, r.selfHost)
	if err != nil {
		return "", fmt.Errorf("docker API unreachable: %w", err)
	}
	macStr, ok := nets[network]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNetworkNotFound, network)
	}
	if macStr == "" {
		// Network attached but Docker hasn't filled MacAddress yet
		// (very early in attach). Treat as transient; the caller will
		// retry within the window.
		return "", fmt.Errorf("%w: %q (no MAC yet)", ErrNetworkNotFound, network)
	}
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return "", fmt.Errorf("parse MAC %q: %w", macStr, err)
	}
	iface, err := r.resolve(mac)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrMACNotFound, macStr)
	}
	return iface, nil
}

// inspectFromDocker adapts a *client.Client into the inspector
// signature this package uses internally.
func inspectFromDocker(cli *client.Client) inspector {
	return func(ctx context.Context, id string) (NetworkMACs, error) {
		insp, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			return nil, err
		}
		out := NetworkMACs{}
		if insp.NetworkSettings != nil {
			for name, n := range insp.NetworkSettings.Networks {
				if n != nil {
					out[name] = n.MacAddress
				}
			}
		}
		return out, nil
	}
}

// resolveByMAC scans netlink links for one whose HardwareAddr matches
// mac. Returns ErrMACNotFound if no link matches; multiple matches
// (impossible in a sane netns) take the first and log a warn.
func resolveByMAC(mac net.HardwareAddr) (string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return "", fmt.Errorf("netlink LinkList: %w", err)
	}
	var first string
	hits := 0
	for _, l := range links {
		if bytes.Equal(l.Attrs().HardwareAddr, mac) {
			if first == "" {
				first = l.Attrs().Name
			}
			hits++
		}
	}
	if hits == 0 {
		return "", ErrMACNotFound
	}
	if hits > 1 {
		slog.Warn("multiple interfaces share MAC, picking first",
			"mac", mac.String(), "iface", first, "hits", hits)
	}
	return first, nil
}
