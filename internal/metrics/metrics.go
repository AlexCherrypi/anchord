// Package metrics owns anchord's Prometheus exposition.
//
// All metric vars are package-level: callers in reconciler, dhcp,
// conntrack, discovery and serviceanchor reach in directly. The
// surface is small and the labels are bounded (see SPEC §2.7), so
// the implicit-global-state cost is low and the call-site noise of
// dependency-injecting a Metrics struct everywhere isn't worth it.
//
// Serve(ctx, addr) starts the HTTP listener that exposes /metrics.
// The same mux is reused for /healthz and /readyz when those land —
// one port, multiple paths, configured via ANCHORD_METRICS_ADDR.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds anchord-emitted collectors. We use a private
// registry rather than the default so test runs in the same process
// don't trip on duplicate registration. promhttp.HandlerFor wraps it
// for the HTTP path.
var Registry = prometheus.NewRegistry()

// Network-anchor metrics.
var (
	ReconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "anchord_reconcile_duration_seconds",
		Help:    "Time from reconcile start to nftables maps written.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	})

	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anchord_reconcile_total",
		Help: "Total reconciles, partitioned by outcome.",
	}, []string{"result"})

	// DHCPLeaseRemaining is a custom collector so the value is
	// recomputed at scrape time from each family's stored deadline.
	// A static gauge would freeze at "lease length at last renew"
	// between renewals — useless for "remaining < threshold" alerts.
	DHCPLeaseRemaining = newLeaseRemainingCollector()

	DHCPAcquired = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anchord_dhcp_acquired_total",
		Help: "DHCP lease acquisition attempts, partitioned by outcome.",
	}, []string{"family", "outcome"})

	DHCPClientRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anchord_dhcp_client_restarts_total",
		Help: "DHCP client restarts (parent-link flap or rebind failure).",
	}, []string{"family"})

	DnatEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "anchord_dnat_entries",
		Help: "Current size of each DNAT map.",
	}, []string{"family", "proto"})

	ConntrackFlushes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anchord_conntrack_flushes_total",
		Help: "Conntrack flushes triggered by backend IP changes.",
	}, []string{"family"})

	DockerEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anchord_docker_events_total",
		Help: "Discovery snapshots, partitioned by trigger source.",
	}, []string{"source"})
)

// Service-anchor metrics.
var (
	GatewayResolve = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anchord_gateway_resolve_total",
		Help: "Gateway-hostname DNS lookups, partitioned by outcome.",
	}, []string{"family", "outcome"})

	GatewayRouteReplaces = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "anchord_gateway_route_replaces_total",
		Help: "Default-route replacements driven by a changed gateway address.",
	}, []string{"family"})

	DefaultRoutePresent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "anchord_default_route_present",
		Help: "1 if the service-anchor currently has a default route installed for this family, else 0.",
	}, []string{"family"})
)

// BuildInfo is a constant-1 gauge labelled with version + commit.
// Populated lazily by ensureBuildInfo() on first Register().
var BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "anchord_build_info",
	Help: "anchord build info; constant 1 for label-introspection.",
}, []string{"version", "commit"})

func init() {
	Registry.MustRegister(
		ReconcileDuration,
		ReconcileTotal,
		DHCPLeaseRemaining,
		DHCPAcquired,
		DHCPClientRestarts,
		DnatEntries,
		ConntrackFlushes,
		DockerEvents,
		GatewayResolve,
		GatewayRouteReplaces,
		DefaultRoutePresent,
		BuildInfo,
	)
	v, c := readBuildInfo()
	BuildInfo.WithLabelValues(v, c).Set(1)
	warmup()
}

// warmup touches every bounded label combination so each metric
// family appears in /metrics from boot. Without this, a Prometheus
// CounterVec with no observed labels is omitted entirely from the
// scrape output — and an alert like `rate(anchord_reconcile_total{result="error"}[5m]) > 0`
// would never have a baseline series to compare against, only firing
// (and firing inconsistently) once the first error happens.
func warmup() {
	for _, r := range []string{"ok", "error"} {
		ReconcileTotal.WithLabelValues(r)
	}
	for _, f := range []string{"v4", "v6"} {
		DHCPClientRestarts.WithLabelValues(f)
		ConntrackFlushes.WithLabelValues(f)
		GatewayRouteReplaces.WithLabelValues(f)
		DefaultRoutePresent.WithLabelValues(f).Set(0)
		// Lease-remaining: deadline=now → reads 0 until acquired.
		// Honest signal in the "between leases" state.
		DHCPLeaseRemaining.Set(f, time.Now())
		for _, o := range []string{"ok", "error"} {
			DHCPAcquired.WithLabelValues(f, o)
			GatewayResolve.WithLabelValues(f, o)
		}
		for _, p := range []string{"tcp", "udp"} {
			DnatEntries.WithLabelValues(f, p).Set(0)
		}
	}
	for _, s := range []string{"event", "poll"} {
		DockerEvents.WithLabelValues(s)
	}
}

// readBuildInfo extracts module version + VCS revision from the
// embedded build info. Returns "unknown" for either when the binary
// was built outside a module / without VCS metadata (e.g. `go test`).
func readBuildInfo() (version, commit string) {
	version, commit = "unknown", "unknown"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			commit = s.Value
			if len(commit) > 12 {
				commit = commit[:12]
			}
		}
	}
	return
}

// Serve runs an HTTP server exposing /metrics on addr until ctx is
// cancelled. Returns nil on graceful shutdown; returns the listener
// error otherwise. A bind failure is the caller's signal that
// metrics are off (per SPEC F-32, that's logged-and-tolerated, not
// fatal).
//
// extraHandlers maps URL paths to additional handlers — used to mount
// /healthz and /readyz on the same listener (SPEC §2.8).
func Serve(ctx context.Context, addr string, extraHandlers map[string]http.Handler) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		Registry: Registry,
	}))
	for path, h := range extraHandlers {
		mux.Handle(path, h)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	slog.Info("metrics listener started", "addr", addr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

// leaseRemainingCollector emits anchord_dhcp_lease_remaining_seconds
// with a value computed at scrape time from the deadline stored per
// family. Zero remaining is reported when no deadline has been set
// for a family — that's the state when the DHCP client is up but
// hasn't yet acquired (or has just released) a lease.
type leaseRemainingCollector struct {
	desc      *prometheus.Desc
	mu        sync.Mutex
	deadlines map[string]time.Time
}

func newLeaseRemainingCollector() *leaseRemainingCollector {
	return &leaseRemainingCollector{
		desc: prometheus.NewDesc(
			"anchord_dhcp_lease_remaining_seconds",
			"Seconds until the current DHCP lease expires. Zero when no lease is held.",
			[]string{"family"}, nil,
		),
		deadlines: map[string]time.Time{},
	}
}

func (c *leaseRemainingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *leaseRemainingCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for fam, deadline := range c.deadlines {
		d := time.Until(deadline).Seconds()
		if d < 0 {
			d = 0
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, d, fam)
	}
}

// Set records the deadline for `family`. Subsequent scrapes report
// time-until-deadline (clamped at zero).
func (c *leaseRemainingCollector) Set(family string, deadline time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlines[family] = deadline
}

// Clear forgets the recorded deadline for `family`, dropping the
// gauge series. Used on lease release / supervisor shutdown.
func (c *leaseRemainingCollector) Clear(family string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.deadlines, family)
}
