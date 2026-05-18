// Package config loads anchord configuration from environment variables.
//
// All anchord configuration is environment-based by design — there is no
// config file. This keeps the operational surface tiny and makes it easy
// to drop the same compose snippet into many projects.
//
// The same binary runs in two modes — see SPEC §2.6 — selected by
// ANCHORD_MODE (or the first non-flag CLI argument). Each mode has its
// own loader (LoadNetworkAnchor / LoadServiceAnchor) so that mode-irrelevant
// env vars don't show up as required errors when running the other mode.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"
)

// AddressMode controls how the network-anchor obtains its external
// IPv4 address on the macvlan that Docker plumbed in for it.
// See SPEC.md §2.1 (v2).
type AddressMode string

const (
	// AddressModeBootstrap keeps the Compose-assigned IPv4 (and any
	// kernel-acquired SLAAC v6) for the lifetime of the container.
	// No DHCP client runs. The least-surprising default.
	AddressModeBootstrap AddressMode = "bootstrap"

	// AddressModeDHCPRefresh starts on the Compose-assigned IPv4 so
	// the container is reachable immediately, then sends a DHCP
	// DISCOVER with a client-id derived from ANCHORD_DHCP_HOSTNAME.
	// On lease success the leased address replaces the bootstrap one
	// atomically. Useful when reservations live in OPNsense DHCP
	// rather than in compose files.
	AddressModeDHCPRefresh AddressMode = "dhcp-refresh"

	// AddressModeSLAACRAOnly keeps the Compose-assigned IPv4 and
	// relies on the kernel's RA/SLAAC for the v6 side. No DHCP client
	// runs in either family. Equivalent to bootstrap from anchord's
	// perspective; named distinctly so operators reading compose can
	// see at a glance that v6 is RA-driven on this stack.
	AddressModeSLAACRAOnly AddressMode = "slaac-ra-only"
)

// NetworkAnchor holds resolved settings for the network-anchor mode.
type NetworkAnchor struct {
	// ComposeProject scopes which containers anchord watches.
	// Required. Usually injected as ${COMPOSE_PROJECT_NAME}.
	ComposeProject string

	// ExtIfaceName is the in-container name of the macvlan interface
	// Docker plumbed in via the external macvlan network. Default
	// "eth0" — the first network Docker attaches when no priorities
	// are set. Override with ANCHORD_EXT_IFACE for stacks where the
	// macvlan is on a non-default interface.
	ExtIfaceName string

	// AddressMode picks how the external IPv4 is obtained (bootstrap
	// vs dhcp-refresh vs slaac-ra-only). Default "bootstrap".
	AddressMode AddressMode

	// DHCPHostname is the hostname (and the basis of the client-id)
	// announced to the DHCP server in dhcp-refresh mode. Defaults to
	// ComposeProject.
	DHCPHostname string

	// PollInterval is the safety-net reconcile cadence on top of
	// docker events.
	PollInterval time.Duration

	// DHCPBackoffMax caps the exponential backoff between DHCP
	// attempts (only meaningful in dhcp-refresh mode).
	DHCPBackoffMax time.Duration

	// DockerHost is the docker API endpoint. Default unix socket.
	DockerHost string

	// LogLevel: debug, info, warn, error.
	LogLevel string

	// MetricsAddr is the listen address for the Prometheus metrics
	// endpoint. Default ":9090". Empty disables the listener.
	// (Same listener hosts /healthz and /readyz.)
	MetricsAddr string
}

// ServiceAnchor holds resolved settings for the service-anchor mode.
type ServiceAnchor struct {
	// GatewayHostname is the Docker-DNS name to look up for the
	// network-anchor's transit IP. Default "anchord".
	GatewayHostname string

	// ResolveInterval is how often the service-anchor mode re-resolves
	// the gateway hostname and reconciles its default route.
	ResolveInterval time.Duration

	// LogLevel: debug, info, warn, error.
	LogLevel string

	// MetricsAddr is the listen address for the Prometheus metrics
	// endpoint. Default ":9090". Empty disables the listener.
	MetricsAddr string
}

// LoadNetworkAnchor reads network-anchor configuration from the environment.
func LoadNetworkAnchor() (*NetworkAnchor, error) {
	c := &NetworkAnchor{
		ComposeProject: os.Getenv("ANCHORD_PROJECT"),
		ExtIfaceName:   getenvDefault("ANCHORD_EXT_IFACE", "eth0"),
		DHCPHostname:   os.Getenv("ANCHORD_DHCP_HOSTNAME"),
		DockerHost:     getenvDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		LogLevel:       getenvDefault("ANCHORD_LOG_LEVEL", "info"),
		MetricsAddr:    metricsAddrFromEnv(),
	}

	if c.ComposeProject == "" {
		// Fall back to the env compose itself injects.
		c.ComposeProject = os.Getenv("COMPOSE_PROJECT_NAME")
	}
	if c.ComposeProject == "" {
		return nil, fmt.Errorf("ANCHORD_PROJECT (or COMPOSE_PROJECT_NAME) must be set")
	}
	if c.DHCPHostname == "" {
		c.DHCPHostname = c.ComposeProject
	}

	mode, err := parseAddressMode(os.Getenv("ANCHORD_ADDRESS_MODE"))
	if err != nil {
		return nil, err
	}
	c.AddressMode = mode

	c.PollInterval, err = parseDuration("ANCHORD_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	c.DHCPBackoffMax, err = parseDuration("ANCHORD_DHCP_BACKOFF_MAX", 5*time.Minute)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// LoadServiceAnchor reads service-anchor configuration from the environment.
func LoadServiceAnchor() (*ServiceAnchor, error) {
	c := &ServiceAnchor{
		GatewayHostname: getenvDefault("ANCHORD_GATEWAY_HOSTNAME", "anchord"),
		LogLevel:        getenvDefault("ANCHORD_LOG_LEVEL", "info"),
		MetricsAddr:     metricsAddrFromEnv(),
	}
	var err error
	c.ResolveInterval, err = parseDuration("ANCHORD_GATEWAY_RESOLVE_INTERVAL", 5*time.Second)
	if err != nil {
		return nil, err
	}
	if c.ResolveInterval <= 0 {
		return nil, fmt.Errorf("ANCHORD_GATEWAY_RESOLVE_INTERVAL must be positive")
	}
	return c, nil
}

// parseAddressMode maps the raw env value to an AddressMode. Empty
// string yields the default (bootstrap); unknown values are rejected
// loudly rather than silently falling through.
func parseAddressMode(raw string) (AddressMode, error) {
	switch AddressMode(raw) {
	case "":
		return AddressModeBootstrap, nil
	case AddressModeBootstrap, AddressModeDHCPRefresh, AddressModeSLAACRAOnly:
		return AddressMode(raw), nil
	default:
		return "", fmt.Errorf("invalid ANCHORD_ADDRESS_MODE %q (want %q, %q, or %q)",
			raw, AddressModeBootstrap, AddressModeDHCPRefresh, AddressModeSLAACRAOnly)
	}
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// metricsAddrFromEnv resolves ANCHORD_METRICS_ADDR with three states:
// unset -> default "127.0.0.1:9090"; set to a value -> that value; set
// to the empty string -> "" (disabled). Distinguishing "unset" from
// "explicit empty" requires LookupEnv rather than getenvDefault.
//
// Default is loopback-only (not :9090) to keep the metrics surface
// off the macvlan interface — binding 0.0.0.0 would publish metrics on
// the LAN-facing network. Operators who want project-internal scraping
// set ":9090" explicitly. In service-anchor mode, app containers share
// the netns via `network_mode: service:`, so 127.0.0.1:9090 IS
// reachable from those containers.
func metricsAddrFromEnv() string {
	v, ok := os.LookupEnv("ANCHORD_METRICS_ADDR")
	if !ok {
		return "127.0.0.1:9090"
	}
	return v
}

func parseDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	// Accept plain seconds for convenience.
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return d, nil
}

// Fingerprint returns a short identifier suitable for log lines.
func (c *NetworkAnchor) Fingerprint() string {
	h := sha256.Sum256([]byte(c.ComposeProject + "|" + c.ExtIfaceName))
	return hex.EncodeToString(h[:4])
}
