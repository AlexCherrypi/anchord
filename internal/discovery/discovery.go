// Package discovery watches the docker daemon for containers in the
// configured compose project and produces State snapshots.
//
// Two sources feed the same channel:
//   - the docker event stream (push, sub-second latency)
//   - a periodic full re-list (pull, safety net for missed events
//     and for IP changes that don't generate events)
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/AlexCherrypi/anchord/internal/labels"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Backend is one container that wants to be exposed.
type Backend struct {
	ID   string
	Name string
	IPv4 net.IP
	IPv6 net.IP
	Spec labels.Spec
}

// State is the full set of backends at a point in time, keyed by
// container ID.
type State struct {
	Backends map[string]Backend
}

// Equal returns true if two states are operationally equivalent.
// Compares the data the reconciler actually cares about.
func (s State) Equal(o State) bool {
	if len(s.Backends) != len(o.Backends) {
		return false
	}
	for id, a := range s.Backends {
		b, ok := o.Backends[id]
		if !ok || !backendEqual(a, b) {
			return false
		}
	}
	return true
}

func backendEqual(a, b Backend) bool {
	if !a.IPv4.Equal(b.IPv4) || !a.IPv6.Equal(b.IPv6) {
		return false
	}
	if a.Spec.V6 != b.Spec.V6 || len(a.Spec.Rules) != len(b.Spec.Rules) {
		return false
	}
	ar, br := append([]labels.Rule(nil), a.Spec.Rules...), append([]labels.Rule(nil), b.Spec.Rules...)
	sort.Slice(ar, func(i, j int) bool { return ruleLess(ar[i], ar[j]) })
	sort.Slice(br, func(i, j int) bool { return ruleLess(br[i], br[j]) })
	for i := range ar {
		if ar[i] != br[i] {
			return false
		}
	}
	return true
}

func ruleLess(a, b labels.Rule) bool {
	if a.Proto != b.Proto {
		return a.Proto < b.Proto
	}
	return a.Port < b.Port
}

// Discoverer emits state updates.
type Discoverer struct {
	cli           *client.Client
	project       string
	pollInterval  time.Duration
	sharedNetwork string // network anchord itself is in; used for IP resolution

	out  chan State
	stop chan struct{}
}

// New constructs a Discoverer. sharedNetwork is the docker network name
// from which to read backend IPs — typically the compose "transit" net
// that the anchord container also belongs to.
func New(cli *client.Client, project, sharedNetwork string, poll time.Duration) *Discoverer {
	return &Discoverer{
		cli:           cli,
		project:       project,
		pollInterval:  poll,
		sharedNetwork: sharedNetwork,
		out:           make(chan State, 4),
		stop:          make(chan struct{}),
	}
}

// Updates returns the channel emitting state snapshots. The channel
// is closed when the Discoverer's context is done.
func (d *Discoverer) Updates() <-chan State { return d.out }

// Run blocks until ctx is cancelled.
func (d *Discoverer) Run(ctx context.Context) error {
	defer close(d.out)

	// Initial reconcile.
	if err := d.snapshot(ctx); err != nil {
		slog.Warn("initial snapshot failed", "err", err)
	}

	go d.pollLoop(ctx)
	return d.eventLoop(ctx)
}

func (d *Discoverer) pollLoop(ctx context.Context) {
	t := time.NewTicker(d.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.snapshot(ctx); err != nil {
				slog.Warn("poll snapshot failed", "err", err)
			}
		}
	}
}

func (d *Discoverer) eventLoop(ctx context.Context) error {
	f := filters.NewArgs()
	f.Add("type", "container")
	f.Add("label", "com.docker.compose.project="+d.project)

	for {
		msgs, errs := d.cli.Events(ctx, events.ListOptions{Filters: f})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("docker event stream error, retrying", "err", err)
			time.Sleep(2 * time.Second)
			continue
		case msg := <-msgs:
			// We don't filter by action — any container event in our
			// project is a reason to re-scan. Cheap.
			slog.Debug("docker event", "action", msg.Action, "actor", msg.Actor.ID[:12])
			if err := d.snapshot(ctx); err != nil {
				slog.Warn("event-driven snapshot failed", "err", err)
			}
		}
	}
}

func (d *Discoverer) snapshot(ctx context.Context) error {
	f := filters.NewArgs()
	f.Add("label", "com.docker.compose.project="+d.project)
	f.Add("label", labels.LabelExpose)

	list, err := d.cli.ContainerList(ctx, container.ListOptions{Filters: f})
	if err != nil {
		return fmt.Errorf("ContainerList: %w", err)
	}

	state := State{Backends: make(map[string]Backend, len(list))}
	for _, c := range list {
		spec, err := labels.Parse(c.Labels)
		if err != nil {
			slog.Warn("invalid expose labels", "container", c.Names, "err", err)
			continue
		}
		if spec == nil {
			continue
		}
		ipv4, ipv6 := pickIPs(c, d.sharedNetwork)
		if ipv4 == nil && ipv6 == nil {
			slog.Warn("no usable IP for container",
				"container", trimName(c.Names),
				"shared_network", d.sharedNetwork)
			continue
		}
		state.Backends[c.ID] = Backend{
			ID:   c.ID,
			Name: trimName(c.Names),
			IPv4: ipv4,
			IPv6: ipv6,
			Spec: *spec,
		}
	}

	select {
	case d.out <- state:
	case <-ctx.Done():
	}
	return nil
}

// pickIPs selects the v4/v6 addresses from the shared network. If
// sharedNetwork is empty, picks the first non-empty entry.
func pickIPs(c container.Summary, sharedNetwork string) (net.IP, net.IP) {
	if c.NetworkSettings == nil {
		return nil, nil
	}
	if sharedNetwork != "" {
		if n, ok := c.NetworkSettings.Networks[sharedNetwork]; ok && n != nil {
			return parseIP(n.IPAddress), parseIP(n.GlobalIPv6Address)
		}
		return nil, nil
	}
	for _, n := range c.NetworkSettings.Networks {
		if n == nil {
			continue
		}
		if v4 := parseIP(n.IPAddress); v4 != nil {
			return v4, parseIP(n.GlobalIPv6Address)
		}
	}
	return nil, nil
}

func parseIP(s string) net.IP {
	if s == "" {
		return nil
	}
	return net.ParseIP(s)
}

func trimName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
