// Package reconciler turns Discovery State snapshots into nftables
// map updates and conntrack flushes.
//
// Strategy: maintain a "last applied" map keyed by (family, proto,
// port) -> backend IP. On each new State, compute the desired map,
// diff against the last applied, then push four atomic SetMap calls
// (one per family*proto). For any (family, proto, port) whose value
// changed or vanished, flush conntrack at the *previous* IP so live
// connections are forced to re-evaluate.
package reconciler

import (
	"context"
	"log/slog"
	"net"
	"sync"

	"github.com/AlexCherrypi/anchord/internal/conntrack"
	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/labels"
	"github.com/AlexCherrypi/anchord/internal/nat"
)

// Reconciler is the glue between Discovery and the NAT manager.
type Reconciler struct {
	nat *nat.Manager

	mu   sync.Mutex
	last map[key]net.IP // last successfully applied
}

type key struct {
	family nat.Family
	proto  string
	port   uint16
}

// New constructs a Reconciler.
func New(n *nat.Manager) *Reconciler {
	return &Reconciler{nat: n, last: map[key]net.IP{}}
}

// Run consumes State updates until ctx is cancelled or the channel
// closes.
func (r *Reconciler) Run(ctx context.Context, updates <-chan discovery.State) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case st, ok := <-updates:
			if !ok {
				return nil
			}
			if err := r.apply(ctx, st); err != nil {
				slog.Error("reconcile failed", "err", err)
			}
		}
	}
}

// apply computes desired state, diffs, pushes to nat, flushes conntrack.
func (r *Reconciler) apply(ctx context.Context, st discovery.State) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	desired := desiredFromState(st)

	// Push four maps atomically. We push even maps that didn't change
	// to keep the code simple — nftables map flush is cheap.
	for _, fam := range []nat.Family{nat.V4, nat.V6} {
		for _, proto := range []string{"tcp", "udp"} {
			entries := map[uint16]net.IP{}
			for k, ip := range desired {
				if k.family == fam && k.proto == proto {
					entries[k.port] = ip
				}
			}
			if err := r.nat.SetMap(fam, proto, entries); err != nil {
				return err
			}
		}
	}

	// Conntrack flush for every backend IP whose mapping changed or
	// disappeared. Flushing the *previous* IP is enough — that's where
	// stale state hangs out.
	flushed := map[string]struct{}{}
	for k, oldIP := range r.last {
		newIP, ok := desired[k]
		if !ok || !oldIP.Equal(newIP) {
			s := oldIP.String()
			if _, dup := flushed[s]; !dup {
				conntrack.FlushDestination(ctx, oldIP)
				flushed[s] = struct{}{}
			}
		}
	}

	// Snapshot for next diff.
	r.last = desired

	slog.Info("reconciled",
		"backends", len(st.Backends),
		"entries", len(desired),
		"conntrack_flushed", len(flushed))
	return nil
}

// desiredFromState explodes Backends into per-(family,proto,port) entries.
func desiredFromState(st discovery.State) map[key]net.IP {
	out := map[key]net.IP{}
	for _, b := range st.Backends {
		for _, rule := range b.Spec.Rules {
			if b.IPv4 != nil {
				out[key{nat.V4, rule.Proto, rule.Port}] = b.IPv4
			}
			if b.IPv6 != nil && b.Spec.V6 != labels.V6Off {
				out[key{nat.V6, rule.Proto, rule.Port}] = b.IPv6
			}
		}
	}
	return out
}
