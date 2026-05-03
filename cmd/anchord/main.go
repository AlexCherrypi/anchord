// Command anchord is a per-compose-project networking shim. It runs in
// one of two modes:
//
//   - network-anchor (default): owns the macvlan + DHCP client, maintains
//     the project's nftables DNAT/masquerade state.
//   - service-anchor: owns a service's network namespace and keeps it
//     pointed at the network-anchor via a default route, resolved by
//     Docker DNS.
//
// Mode is selected via ANCHORD_MODE, or equivalently by passing
// "network-anchor" or "service-anchor" as the first argument.
//
// See README.md for the user-facing story; SPEC.md §2.6 for the
// service-anchor contract; ARCHITECTURE.md for the role model.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"net/http"

	"github.com/AlexCherrypi/anchord/internal/config"
	"github.com/AlexCherrypi/anchord/internal/dhcp"
	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/health"
	"github.com/AlexCherrypi/anchord/internal/metrics"
	"github.com/AlexCherrypi/anchord/internal/nat"
	"github.com/AlexCherrypi/anchord/internal/reconciler"
	"github.com/AlexCherrypi/anchord/internal/serviceanchor"

	"github.com/docker/docker/client"
)

// Mode identifies which subsystem the binary runs.
type Mode string

const (
	ModeNetworkAnchor Mode = "network-anchor"
	ModeServiceAnchor Mode = "service-anchor"
)

func main() {
	if err := run(); err != nil {
		// context.Canceled is the expected signal-driven shutdown path
		// — exit 0 so SPEC F-20 ("exits cleanly on SIGTERM/SIGINT") is
		// observable from PID 1.
		if errors.Is(err, context.Canceled) {
			return
		}
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	mode, err := selectMode(os.Args, os.Getenv("ANCHORD_MODE"))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap SIGTERM/SIGINT for graceful shutdown — same handler for
	// both modes; the mode-specific Run respects context cancellation.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		slog.Info("signal received, shutting down", "signal", s)
		cancel()
	}()

	switch mode {
	case ModeServiceAnchor:
		return runServiceAnchor(ctx)
	default:
		return runNetworkAnchor(ctx)
	}
}

// selectMode picks the run mode from (in priority order): first non-flag
// CLI argument, then the ANCHORD_MODE env var, else network-anchor.
// Returns an error for unrecognized values rather than silently falling
// through, so misconfiguration is loud.
func selectMode(args []string, envMode string) (Mode, error) {
	var argMode string
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		argMode = args[1]
	}
	mode := argMode
	if mode == "" {
		mode = envMode
	}
	if mode == "" {
		return ModeNetworkAnchor, nil
	}
	switch Mode(mode) {
	case ModeNetworkAnchor, ModeServiceAnchor:
		return Mode(mode), nil
	default:
		return "", fmt.Errorf("unknown mode %q (want %q or %q)",
			mode, ModeNetworkAnchor, ModeServiceAnchor)
	}
}

// runNetworkAnchor is the original anchord behaviour: macvlan + DHCP +
// nftables DNAT + reconciler driven by Docker events.
func runNetworkAnchor(ctx context.Context) error {
	cfg, err := config.LoadNetworkAnchor()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	setupLogger(cfg.LogLevel)

	tracker := health.NewTracker()
	startMetrics(ctx, cfg.MetricsAddr, map[string]http.Handler{
		"/healthz": health.LivenessHandler(),
		"/readyz":  health.NetworkAnchorReadinessHandler(tracker),
	})

	slog.Info("anchord starting (network-anchor mode)",
		"project", cfg.ComposeProject,
		"vlan_parent", cfg.VLANParent,
		"ext_iface", cfg.ExtIfaceName,
		"mac", cfg.MACString(),
		"hostname", cfg.DHCPHostname,
		"fp", cfg.Fingerprint())

	// 1. NAT subsystem — install tables/chains immediately so we can
	//    accept reconciles before the first DHCP lease arrives.
	natMgr := nat.New(cfg.ExtIfaceName)
	if err := natMgr.Setup(); err != nil {
		return fmt.Errorf("nat setup: %w", err)
	}
	tracker.MarkTablesInstalled()
	defer func() {
		if err := natMgr.Teardown(); err != nil {
			slog.Warn("nat teardown", "err", err)
		}
	}()

	// 2. DHCP supervisor — runs in the background, emits IPs as it
	//    learns them. We don't block on the first IP: maps work without
	//    knowing our external address (the DNAT rule is interface-bound,
	//    not IP-bound). Masquerade auto-tracks the assigned address.
	//
	//    We track its goroutine via WaitGroup so that the deferred
	//    Supervisor.removeLink (which deletes the macvlan child) is
	//    guaranteed to finish before main returns. Without this the
	//    container can exit before removeLink runs, leaving SPEC F-20
	//    (clean teardown) unverifiable.
	dhcpSup := dhcp.New(cfg.VLANParent, cfg.ExtIfaceName, cfg.ExtMAC, cfg.DHCPHostname, cfg.DHCPBackoffMax)
	var wg sync.WaitGroup
	wg.Add(1)
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer wg.Done()
		if err := dhcpSup.Run(cancelCtx); err != nil && cancelCtx.Err() == nil {
			slog.Error("dhcp supervisor exited", "err", err)
			cancel()
		}
	}()
	defer wg.Wait()
	// Drain the IP channel so the supervisor doesn't block. Logging
	// of changes happens inside the dhcp package itself.
	go func() {
		for range dhcpSup.IPs() {
		}
	}()

	// 3. Docker client.
	cli, err := client.NewClientWithOpts(
		client.WithHost(cfg.DockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	// 4. Discovery — finds the shared transit network by inspecting our
	//    own container and emits state snapshots.
	sharedNet, err := detectSharedNetwork(cancelCtx, cli)
	if err != nil {
		slog.Warn("could not auto-detect shared network", "err", err)
	} else {
		slog.Info("discovered shared network", "name", sharedNet)
	}
	disc := discovery.New(cli, cfg.ComposeProject, sharedNet, cfg.PollInterval)
	go func() {
		if err := disc.Run(cancelCtx); err != nil && cancelCtx.Err() == nil {
			slog.Error("discovery exited", "err", err)
			cancel()
		}
	}()

	// 5. Reconciler — the main loop.
	rec := reconciler.New(natMgr)
	rec.OnReconciled = tracker.MarkReconciled
	return rec.Run(cancelCtx, disc.Updates())
}

// runServiceAnchor maintains a default route in the local namespace
// pointing at whatever the network-anchor's transit IP currently is
// (resolved via Docker DNS). See SPEC §2.6.
func runServiceAnchor(ctx context.Context) error {
	cfg, err := config.LoadServiceAnchor()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	setupLogger(cfg.LogLevel)

	tracker := health.NewTracker()
	startMetrics(ctx, cfg.MetricsAddr, map[string]http.Handler{
		"/healthz": health.LivenessHandler(),
		"/readyz":  health.ServiceAnchorReadinessHandler(tracker),
	})

	mgr := serviceanchor.New(cfg)
	mgr.OnRouteInstalled = tracker.MarkRouteInstalled
	return mgr.Run(ctx)
}

// startMetrics spawns the HTTP listener if addr is non-empty. The
// listener serves /metrics and any extra paths supplied by the
// caller (typically /healthz and /readyz).
//
// A bind failure is logged at warn but does not abort startup — the
// listener carries observability, not critical-path behaviour (SPEC F-32).
func startMetrics(ctx context.Context, addr string, extra map[string]http.Handler) {
	if addr == "" {
		slog.Info("metrics + health listener disabled (ANCHORD_METRICS_ADDR=\"\")")
		return
	}
	go func() {
		if err := metrics.Serve(ctx, addr, extra); err != nil {
			slog.Warn("metrics listener exited", "addr", addr, "err", err)
		}
	}()
}

// detectSharedNetwork inspects the anchord container itself to find
// which compose-project network it lives in. That's the network we'll
// read backend IPs from. If multiple, prefers the one whose name
// contains "transit", else the first.
func detectSharedNetwork(ctx context.Context, cli *client.Client) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}
	insp, err := cli.ContainerInspect(ctx, hostname)
	if err != nil {
		return "", fmt.Errorf("inspect self (%s): %w", hostname, err)
	}
	if insp.NetworkSettings == nil || len(insp.NetworkSettings.Networks) == 0 {
		return "", fmt.Errorf("no networks on self")
	}
	var first, transit string
	for name := range insp.NetworkSettings.Networks {
		if first == "" {
			first = name
		}
		if transit == "" && containsFold(name, "transit") {
			transit = name
		}
	}
	if transit != "" {
		return transit, nil
	}
	return first, nil
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
}

