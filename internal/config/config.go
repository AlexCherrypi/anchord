// Package config loads anchord configuration from environment variables.
//
// All anchord configuration is environment-based by design — there is no
// config file. This keeps the operational surface tiny and makes it easy
// to drop the same compose snippet into many projects.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// Config holds resolved anchord settings.
type Config struct {
	// ComposeProject scopes which containers anchord watches.
	// Required. Usually injected as ${COMPOSE_PROJECT_NAME}.
	ComposeProject string

	// VLANParent is the host-side parent interface for the macvlan
	// (e.g. "eth0.42" or "br-vlan42"). Must already be up.
	VLANParent string

	// ExtIfaceName is the name of the macvlan child created inside
	// the anchord container (default "anchord-ext").
	ExtIfaceName string

	// ExtMAC is the stable MAC address used for DHCP reservations.
	// If empty, derived deterministically from ComposeProject.
	ExtMAC net.HardwareAddr

	// DHCPHostname is the hostname announced to the DHCP server.
	// Defaults to ComposeProject.
	DHCPHostname string

	// PollInterval is the safety-net reconcile cadence on top of
	// docker events.
	PollInterval time.Duration

	// DHCPBackoffMax caps the exponential backoff between DHCP
	// attempts. The user has been clear: not five months.
	DHCPBackoffMax time.Duration

	// DockerHost is the docker API endpoint. Default unix socket.
	DockerHost string

	// LogLevel: debug, info, warn, error.
	LogLevel string
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		ComposeProject: os.Getenv("ANCHORD_PROJECT"),
		VLANParent:     os.Getenv("ANCHORD_VLAN_PARENT"),
		ExtIfaceName:   getenvDefault("ANCHORD_EXT_IFACE", "anchord-ext"),
		DHCPHostname:   os.Getenv("ANCHORD_DHCP_HOSTNAME"),
		DockerHost:     getenvDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		LogLevel:       getenvDefault("ANCHORD_LOG_LEVEL", "info"),
	}

	if c.ComposeProject == "" {
		// Fall back to the env compose itself injects.
		c.ComposeProject = os.Getenv("COMPOSE_PROJECT_NAME")
	}
	if c.ComposeProject == "" {
		return nil, fmt.Errorf("ANCHORD_PROJECT (or COMPOSE_PROJECT_NAME) must be set")
	}
	if c.VLANParent == "" {
		return nil, fmt.Errorf("ANCHORD_VLAN_PARENT must be set (e.g. eth0.42)")
	}
	if c.DHCPHostname == "" {
		c.DHCPHostname = c.ComposeProject
	}

	// MAC: explicit, or deterministic-from-project.
	if mac := os.Getenv("ANCHORD_EXT_MAC"); mac != "" {
		hw, err := net.ParseMAC(mac)
		if err != nil {
			return nil, fmt.Errorf("invalid ANCHORD_EXT_MAC: %w", err)
		}
		c.ExtMAC = hw
	} else {
		c.ExtMAC = deriveMAC(c.ComposeProject)
	}

	var err error
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

// deriveMAC produces a stable locally-administered unicast MAC from a
// project name. Locally administered = bit 1 of first octet set, unicast
// = bit 0 cleared. We use the OUI 02:42:xx, which Docker also uses for
// its bridge MACs — keeps things visually consistent in `arp -a`.
func deriveMAC(project string) net.HardwareAddr {
	sum := sha256.Sum256([]byte("anchord:" + project))
	mac := make(net.HardwareAddr, 6)
	mac[0] = 0x02
	mac[1] = 0x42
	copy(mac[2:], sum[:4])
	return mac
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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

// MACString returns the configured MAC in standard colon-hex form.
func (c *Config) MACString() string {
	return c.ExtMAC.String()
}

// Fingerprint returns a short identifier suitable for log lines.
func (c *Config) Fingerprint() string {
	h := sha256.Sum256([]byte(c.ComposeProject + c.VLANParent))
	return hex.EncodeToString(h[:4])
}
