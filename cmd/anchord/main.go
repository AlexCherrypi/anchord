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

	"github.com/AlexCherrypi/anchord/internal/autostart"
	"github.com/AlexCherrypi/anchord/internal/config"
	"github.com/AlexCherrypi/anchord/internal/dhcp"
	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/extiface"
	"github.com/AlexCherrypi/anchord/internal/health"
	"github.com/AlexCherrypi/anchord/internal/metrics"
	"github.com/AlexCherrypi/anchord/internal/nat"
	"github.com/AlexCherrypi/anchord/internal/reconciler"
	"github.com/AlexCherrypi/anchord/internal/serviceanchor"
	"github.com/AlexCherrypi/anchord/internal/sharednet"

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

	// 5. Discovery — build a SharedNetworkPicker (F-44) from anchord's
	//    own container inspect, then hand it to the Discoverer so each
	//    snapshot reconciles its choice against observed backend
	//    co-attachment. Empty ANCHORD_SHARED_NETWORK = heuristic mode;
	//    explicit value = pinned (validated to be one of self-networks).
	//
	//    Selector vs project (F-42) is resolved here: a non-empty
	//    ANCHORD_LABEL_SELECTOR wins outright; ANCHORD_PROJECT is
	//    logged-and-ignored when both are set.
	selfNets, err := selfNetworks(cancelCtx, cli)
	if err != nil {
		return fmt.Errorf("inspect self for shared-network picker: %w", err)
	}
	picker, err := sharednet.New(selfNets, cfg.ExtNetwork, cfg.SharedNetwork)
	if err != nil {
		return fmt.Errorf("shared-network picker: %w", err)
	}
	slog.Info("shared-network picker initialised",
		"candidates", picker.Candidates(),
		"pinned", cfg.SharedNetwork)
	discriminator := buildDiscoveryDiscriminator(cfg.ComposeProject, cfg.LabelSelector)
	disc := discovery.New(cli, discriminator, picker, cfg.PollInterval)
	go func() {
		if err := disc.Run(cancelCtx); err != nil && cancelCtx.Err() == nil {
			slog.Error("discovery exited", "err", err)
			cancel()
		}
	}()

	// 6. Sibling auto-start (F-43) — opt-out via ANCHORD_AUTOSTART_SIBLINGS=false.
	//    Watches Docker for `container start` events and bootstraps
	//    any Created-state sibling whose network_mode: container:<X>
	//    matches the just-started target. Independent of selector
	//    scope: the spawning service may carry no anchord labels at
	//    all (authentik outpost case) yet still need the rescue.
	if cfg.AutostartSiblings {
		watcher := autostart.New(cli, cfg.ManagedSA)
		// F-45 needs the shared network to default ManagedSA.GatewayIP
		// to anchord's own IP on it. The picker may not have settled
		// yet, but Candidates() returns a stable list and the first
		// reconcile typically settles within seconds; we pass the
		// picker's current Chosen() (may be empty initially) and the
		// watcher self-recovers on the next event.
		watcher.SetSharedNetwork(picker.Chosen())
		if cfg.ManagedSA.Active() {
			slog.Info("managed service-anchor recipe active",
				"target", cfg.ManagedSA.Target,
				"name", cfg.ManagedSA.Name,
				"image", orDefault(cfg.ManagedSA.Image, "<self>"),
				"gateway_ip", orDefault(cfg.ManagedSA.GatewayIP, "<self on shared net>"))
		}
		go func() {
			if err := watcher.Run(cancelCtx); err != nil && cancelCtx.Err() == nil {
				slog.Error("autostart watcher exited", "err", err)
				// Auto-start is a quality-of-life feature; its loss
				// doesn't justify killing the network-anchor. Log and
				// let the rest of the data plane keep working.
			}
		}()
	} else {
		slog.Info("sibling auto-start disabled (ANCHORD_AUTOSTART_SIBLINGS=false)")
	}

	// 7. Reconciler — the main loop.
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

// selfNetworks inspects the anchord container itself and returns the
// list of Docker networks it's currently attached to. The
// sharednet.Picker takes this plus ANCHORD_EXT_NETWORK and the
// optional ANCHORD_SHARED_NETWORK pin to decide which network to use
// for backend IP reads. F-38 (exclude EXT) and F-44 (co-attachment-
// based pick + re-evaluation) both live in internal/sharednet now.
func selfNetworks(ctx context.Context, cli *client.Client) ([]string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	insp, err := cli.ContainerInspect(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("inspect self (%s): %w", hostname, err)
	}
	if insp.NetworkSettings == nil || len(insp.NetworkSettings.Networks) == 0 {
		return nil, fmt.Errorf("no networks on self")
	}
	names := make([]string, 0, len(insp.NetworkSettings.Networks))
	for name := range insp.NetworkSettings.Networks {
		names = append(names, name)
	}
	return names, nil
}

// orDefault returns s when non-empty, otherwise def. Tiny helper for
// log lines where a configured value should be shown verbatim and an
// empty value should surface a placeholder so the operator can tell
// "not set" from "set to empty".
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
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

