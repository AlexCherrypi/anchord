// Command anchord is a per-compose-project network anchor: it gives a
// Docker Compose project a single externally-routed IP via macvlan
// + DHCP, and dynamically maintains nftables DNAT rules pointing at
// labelled service-anchor containers.
//
// See README.md for architecture and setup.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AlexCherrypi/anchord/internal/config"
	"github.com/AlexCherrypi/anchord/internal/dhcp"
	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/nat"
	"github.com/AlexCherrypi/anchord/internal/reconciler"

	"github.com/docker/docker/client"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	setupLogger(cfg.LogLevel)

	slog.Info("anchord starting",
		"project", cfg.ComposeProject,
		"vlan_parent", cfg.VLANParent,
		"ext_iface", cfg.ExtIfaceName,
		"mac", cfg.MACString(),
		"hostname", cfg.DHCPHostname,
		"fp", cfg.Fingerprint())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap SIGTERM/SIGINT for graceful shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		slog.Info("signal received, shutting down", "signal", s)
		cancel()
	}()

	// 1. NAT subsystem — install tables/chains immediately so we can
	//    accept reconciles before the first DHCP lease arrives.
	natMgr := nat.New(cfg.ExtIfaceName)
	if err := natMgr.Setup(); err != nil {
		return fmt.Errorf("nat setup: %w", err)
	}
	defer func() {
		if err := natMgr.Teardown(); err != nil {
			slog.Warn("nat teardown", "err", err)
		}
	}()

	// 2. DHCP supervisor — runs in the background, emits IPs as it
	//    learns them. We don't block on the first IP: maps work without
	//    knowing our external address (the DNAT rule is interface-bound,
	//    not IP-bound). Masquerade auto-tracks the assigned address.
	dhcpSup := dhcp.New(cfg.VLANParent, cfg.ExtIfaceName, cfg.ExtMAC, cfg.DHCPHostname, cfg.DHCPBackoffMax)
	go func() {
		if err := dhcpSup.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("dhcp supervisor exited", "err", err)
			cancel()
		}
	}()
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
	sharedNet, err := detectSharedNetwork(ctx, cli)
	if err != nil {
		slog.Warn("could not auto-detect shared network", "err", err)
	} else {
		slog.Info("discovered shared network", "name", sharedNet)
	}
	disc := discovery.New(cli, cfg.ComposeProject, sharedNet, cfg.PollInterval)
	go func() {
		if err := disc.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("discovery exited", "err", err)
			cancel()
		}
	}()

	// 5. Reconciler — the main loop.
	rec := reconciler.New(natMgr)
	return rec.Run(ctx, disc.Updates())
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
