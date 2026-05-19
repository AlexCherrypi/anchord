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
	"github.com/AlexCherrypi/anchord/internal/metrics"

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

// SharedNetworkResolver returns the Docker network anchord should
// read backend IPs from for the current backend snapshot. It is
// called once per snapshot with the per-network co-attachment count
// of the just-listed backends; implementations may be stateful
// (e.g. internal/sharednet.Picker settles after the first observed
// backend, F-44 §"Stable once decided"). The chosen network must be
// one anchord itself is attached to — picker enforces that at
// construction, callers don't have to.
type SharedNetworkResolver interface {
	Pick(backendNetworks map[string]int) string
	// Chosen returns the most recently picked network without
	// re-running the heuristic. Used for logging only.
	Chosen() string
}

// Discoverer emits state updates.
type Discoverer struct {
	cli           *client.Client
	discriminator []string              // label predicates ("key=value") all containers must match (F-42)
	pollInterval  time.Duration
	shared        SharedNetworkResolver // F-44 — network for IP reads, possibly stateful

	out  chan State
	stop chan struct{}
}

// New constructs a Discoverer scoped by `discriminator` label
// predicates (each formatted as the Docker filter expects:
// "key=value"). All predicates are AND-joined — a container must
// carry every label to be considered a backend candidate. anchord
// additionally enforces the anchord.expose presence check internally;
// that is not a caller concern.
//
// `shared` resolves the per-snapshot shared-network choice (F-44).
// Pass a sharednet.Picker in production; tests inject stubs.
//
// Two callers exist in production today (see cmd/anchord/main.go):
//   - legacy ANCHORD_PROJECT mode: discriminator =
//     ["com.docker.compose.project=<project>"]
//   - F-42 selector mode: discriminator = the parsed
//     ANCHORD_LABEL_SELECTOR rendered as one "key=value" entry per
//     selector pair, deterministically ordered.
func New(cli *client.Client, discriminator []string, shared SharedNetworkResolver, poll time.Duration) *Discoverer {
	return &Discoverer{
		cli:           cli,
		discriminator: discriminator,
		pollInterval:  poll,
		shared:        shared,
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
			metrics.DockerEvents.WithLabelValues("poll").Inc()
			if err := d.snapshot(ctx); err != nil {
				slog.Warn("poll snapshot failed", "err", err)
			}
		}
	}
}

func (d *Discoverer) eventLoop(ctx context.Context) error {
	f := buildEventFilter(d.discriminator)

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
			metrics.DockerEvents.WithLabelValues("event").Inc()
			slog.Debug("docker event", "action", msg.Action, "actor", msg.Actor.ID[:12])
			if err := d.snapshot(ctx); err != nil {
				slog.Warn("event-driven snapshot failed", "err", err)
			}
		}
	}
}

func (d *Discoverer) snapshot(ctx context.Context) error {
	f := buildSnapshotFilter(d.discriminator)

	list, err := d.cli.ContainerList(ctx, container.ListOptions{Filters: f})
	if err != nil {
		return fmt.Errorf("ContainerList: %w", err)
	}

	// F-44: count backend co-attachment per network and ask the
	// resolver which network to use. Stateful Pickers (production
	// internal/sharednet.Picker) settle the first time a backend
	// is seen on the chosen network — subsequent calls are cheap.
	prevShared := ""
	if d.shared != nil {
		prevShared = d.shared.Chosen()
	}
	counts := countBackendsPerNetwork(list)
	shared := ""
	if d.shared != nil {
		shared = d.shared.Pick(counts)
	}
	if shared != prevShared && prevShared != "" {
		slog.Info("shared network re-picked",
			"old", prevShared, "new", shared)
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
		ipv4, ipv6 := pickIPs(c, shared)
		if ipv4 == nil && ipv6 == nil {
			slog.Warn("no usable IP for container",
				"container", trimName(c.Names),
				"shared_network", shared)
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

// buildSnapshotFilter constructs the Docker container-list filter for
// a backend snapshot: every discriminator label (AND-joined) plus
// presence of `anchord.expose`. Extracted so tests can verify the
// filter shape without a live Docker daemon.
func buildSnapshotFilter(discriminator []string) filters.Args {
	f := filters.NewArgs()
	for _, predicate := range discriminator {
		f.Add("label", predicate)
	}
	f.Add("label", labels.LabelExpose)
	return f
}

// buildEventFilter constructs the Docker event-stream filter:
// container events whose actor carries every discriminator label.
// The anchord.expose presence check is NOT included here on purpose —
// we still want to see "container destroyed" events for things that
// used to be exposed; the snapshot path re-evaluates membership.
func buildEventFilter(discriminator []string) filters.Args {
	f := filters.NewArgs()
	f.Add("type", "container")
	for _, predicate := range discriminator {
		f.Add("label", predicate)
	}
	return f
}

// countBackendsPerNetwork sums network attachments across the backend
// container list — the input the sharednet.Picker needs to apply the
// F-44 co-attachment heuristic. A backend on N networks contributes N
// to the totals.
func countBackendsPerNetwork(list []container.Summary) map[string]int {
	out := map[string]int{}
	for _, c := range list {
		if c.NetworkSettings == nil {
			continue
		}
		for name, n := range c.NetworkSettings.Networks {
			if n == nil {
				continue
			}
			out[name]++
		}
	}
	return out
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
