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
	"sync"
	"time"

	"github.com/AlexCherrypi/anchord/internal/conntrack"
	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/labels"
	"github.com/AlexCherrypi/anchord/internal/metrics"
	"github.com/AlexCherrypi/anchord/internal/nat"
)

// Reconciler is the glue between Discovery and the NAT manager.
type Reconciler struct {
	nat *nat.Manager

	// OnReconciled, if set, is invoked after every successful apply.
	// Used by main to flip the readiness Tracker once the data plane
	// has produced its first map (SPEC F-34). Idempotent on the
	// caller side — we don't bother dedup'ing here.
	OnReconciled func()

	mu   sync.Mutex
	last map[key]nat.Target // last successfully applied (F-46: target includes backend port)
}

type key struct {
	family nat.Family
	proto  string
	port   uint16
}

// New constructs a Reconciler.
func New(n *nat.Manager) *Reconciler {
	return &Reconciler{nat: n, last: map[key]nat.Target{}}
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
			start := time.Now()
			err := r.apply(ctx, st)
			metrics.ReconcileDuration.Observe(time.Since(start).Seconds())
			if err != nil {
				metrics.ReconcileTotal.WithLabelValues("error").Inc()
				slog.Error("reconcile failed", "err", err)
			} else {
				metrics.ReconcileTotal.WithLabelValues("ok").Inc()
				if r.OnReconciled != nil {
					r.OnReconciled()
				}
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
			entries := map[uint16]nat.Target{}
			for k, tgt := range desired {
				if k.family == fam && k.proto == proto {
					entries[k.port] = tgt
				}
			}
			if err := r.nat.SetMap(fam, proto, entries); err != nil {
				return err
			}
			metrics.DnatEntries.WithLabelValues(fam.String(), proto).Set(float64(len(entries)))
		}
	}

	// Conntrack flush for every backend whose target changed (IP or
	// port). Flushing the *previous* target's IP is enough — that's
	// where stale state hangs out.
	flushed := map[string]struct{}{}
	for k, oldTgt := range r.last {
		newTgt, ok := desired[k]
		if !ok || !oldTgt.IP.Equal(newTgt.IP) || oldTgt.Port != newTgt.Port {
			s := oldTgt.IP.String()
			if _, dup := flushed[s]; !dup {
				conntrack.FlushDestination(ctx, oldTgt.IP)
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

// desiredFromState explodes Backends into per-(family,proto,port)
// DNAT targets. F-46: the target may carry a backend port that
// differs from the DMZ-side key port (`anchord.expose=tcp/636:6636`
// produces key=636, target={IP, Port=6636}). When the operator
// didn't request translation, BackendPort==Port and the target
// behaves identically to pre-F-46 anchord.
func desiredFromState(st discovery.State) map[key]nat.Target {
	out := map[key]nat.Target{}
	for _, b := range st.Backends {
		for _, rule := range b.Spec.Rules {
			if b.IPv4 != nil {
				out[key{nat.V4, rule.Proto, rule.Port}] = nat.Target{IP: b.IPv4, Port: rule.BackendPort}
			}
			if b.IPv6 != nil && b.Spec.V6 != labels.V6Off {
				out[key{nat.V6, rule.Proto, rule.Port}] = nat.Target{IP: b.IPv6, Port: rule.BackendPort}
			}
		}
	}
	return out
}
