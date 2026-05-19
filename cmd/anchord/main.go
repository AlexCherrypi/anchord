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
	"sort"
	"strings"
	"sync"
	"syscall"

	"net/http"

	"github.com/AlexCherrypi/anchord/internal/config"
	"github.com/AlexCherrypi/anchord/internal/dhcp"
	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/extiface"
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

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 1. Docker client — needed before NAT/DHCP because the external
	//    iface name may come from a Docker-API lookup (F-37).
	cli, err := client.NewClientWithOpts(
		client.WithHost(cfg.DockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	// 2. Resolve the external interface.
	//    If ANCHORD_EXT_NETWORK is set, ask the Docker API which local
	//    iface attaches to that network and match by MAC — Docker's
	//    eth0/eth1 assignment is not deterministic across recreates on
	//    multi-network stacks, so name-based selection is a coin flip
	//    (SPEC-v2-DRAFT F-37). Otherwise fall back to ANCHORD_EXT_IFACE
	//    (default eth0) as set by config.LoadNetworkAnchor.
	if cfg.ExtNetwork != "" {
		if cfg.ExtIfaceName != "eth0" && os.Getenv("ANCHORD_EXT_IFACE") != "" {
			slog.Warn("both ANCHORD_EXT_NETWORK and ANCHORD_EXT_IFACE set; using EXT_NETWORK",
				"network", cfg.ExtNetwork, "ignored_iface", cfg.ExtIfaceName)
		}
		selfHost, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
		resolver := extiface.New(cli, selfHost)
		iface, err := resolver.Resolve(cancelCtx, cfg.ExtNetwork)
		if err != nil {
			return fmt.Errorf("resolve external iface from network %q: %w", cfg.ExtNetwork, err)
		}
		slog.Info("external interface resolved by network",
			"network", cfg.ExtNetwork, "iface", iface)
		cfg.ExtIfaceName = iface
	}

	slog.Info("anchord starting (network-anchor mode)",
		"project", cfg.ComposeProject,
		"ext_iface", cfg.ExtIfaceName,
		"ext_network", cfg.ExtNetwork,
		"address_mode", cfg.AddressMode,
		"hostname", cfg.DHCPHostname,
		"fp", cfg.Fingerprint())

	// 3. NAT subsystem — install tables/chains immediately so we can
	//    accept reconciles before the supervisor has settled on an
	//    address. The DNAT rule is interface-bound (iif/oif match), so
	//    it works on the Docker-bootstrap IP, on a leased IP, and
	//    across any swap between the two.
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

	// 4. DHCP supervisor — mode-aware:
	//      bootstrap / slaac-ra-only: passive, only the IP watcher runs
	//      dhcp-refresh: v4 DORA on the iface, v6 SOLICIT best-effort
	//    Docker owns the macvlan child itself; anchord never adds or
	//    deletes a link. We still WaitGroup so deferred cleanup (lease
	//    RELEASE, address-removal) completes before main returns.
	dhcpSup := dhcp.New(cfg.AddressMode, cfg.ExtIfaceName, cfg.DHCPHostname, cfg.DHCPBackoffMax)
	var wg sync.WaitGroup
	wg.Add(1)
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

	// 5. Discovery — finds the shared transit network by inspecting our
	//    own container and emits state snapshots. Selector vs project
	//    is resolved here (F-42): a non-empty ANCHORD_LABEL_SELECTOR
	//    wins outright; ANCHORD_PROJECT is logged-and-ignored when
	//    both are set.
	sharedNet, err := detectSharedNetwork(cancelCtx, cli, cfg.ExtNetwork)
	if err != nil {
		slog.Warn("could not auto-detect shared network", "err", err)
	} else {
		slog.Info("discovered shared network", "name", sharedNet)
	}
	discriminator := buildDiscoveryDiscriminator(cfg.ComposeProject, cfg.LabelSelector)
	disc := discovery.New(cli, discriminator, sharedNet, cfg.PollInterval)
	go func() {
		if err := disc.Run(cancelCtx); err != nil && cancelCtx.Err() == nil {
			slog.Error("discovery exited", "err", err)
			cancel()
		}
	}()

	// 6. Reconciler — the main loop.
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
// read backend IPs from.
//
// Selection rules (F-38):
//  1. Skip the external macvlan (excludeNet, typically cfg.ExtNetwork)
//     — backends never live there, anchord just attaches to forward
//     inbound and masquerade outbound.
//  2. From what remains: prefer a network whose name contains
//     "transit" (case-insensitive). That's the documented convention
//     for the per-project anchor↔service-anchor bridge.
//  3. Otherwise return any remaining candidate (Go-map random order,
//     but at least never the macvlan).
//  4. If excludeNet is empty: rules 2 and 3 apply unchanged from v1.
//  5. If exclusion leaves nothing: error with a clearer message than
//     "no networks on self" so wrap-pattern misconfiguration surfaces.
func detectSharedNetwork(ctx context.Context, cli *client.Client, excludeNet string) (string, error) {
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
	names := make([]string, 0, len(insp.NetworkSettings.Networks))
	for name := range insp.NetworkSettings.Networks {
		names = append(names, name)
	}
	return pickSharedNetwork(names, excludeNet)
}

// pickSharedNetwork is the pure-function core of detectSharedNetwork,
// extracted so tests can drive it without a live Docker daemon.
//
// Returns an error rather than the empty string when excludeNet
// removes the last candidate — anchord without a non-EXT network has
// nowhere to read backend IPs from, which is a configuration error.
func pickSharedNetwork(names []string, excludeNet string) (string, error) {
	if len(names) == 0 {
		return "", fmt.Errorf("no networks on self")
	}
	var first, transit string
	for _, name := range names {
		if excludeNet != "" && name == excludeNet {
			continue
		}
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
	if first != "" {
		return first, nil
	}
	return "", fmt.Errorf("only EXT_NETWORK %q on self; need at least one project-internal network", excludeNet)
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// buildDiscoveryDiscriminator resolves the F-42 selector-vs-project
// precedence into the list of Docker label predicates the discoverer
// will use to scope its backend search.
//
// Rules:
//   - selector non-empty: selector wins. Each key=value pair becomes
//     one predicate (deterministic order by key). If project is ALSO
//     set, log a WARN naming the ignored project so the operator
//     isn't surprised — but proceed.
//   - selector empty, project set: classic behaviour — single
//     "com.docker.compose.project=<project>" predicate.
//   - both empty: returns nil. Config.LoadNetworkAnchor rejects that
//     combination, so we never reach here in practice; the nil return
//     is defence-in-depth.
//
// The selector log line at INFO level is the operator-facing signal
// that "this anchord instance is the selector-scoped one"; matches
// SPEC F-42 §"Behaviour" point 4.
func buildDiscoveryDiscriminator(project string, selector map[string]string) []string {
	if len(selector) > 0 {
		if project != "" {
			slog.Warn("ANCHORD_LABEL_SELECTOR set; ANCHORD_PROJECT ignored",
				"ignored_project", project)
		}
		keys := make([]string, 0, len(selector))
		for k := range selector {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, k+"="+selector[k])
		}
		slog.Info("backend label selector active", "selector", strings.Join(out, ","))
		return out
	}
	if project != "" {
		return []string{"com.docker.compose.project=" + project}
	}
	return nil
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

