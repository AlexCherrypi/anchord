package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearAnchordEnv blanks every env var LoadNetworkAnchor() consults so each test
// starts from a deterministic baseline. Empty string and "unset" are
// equivalent for LoadNetworkAnchor() because every check is `os.Getenv(...) == ""`.
func clearAnchordEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANCHORD_PROJECT", "ANCHORD_EXT_IFACE", "ANCHORD_EXT_NETWORK",
		"ANCHORD_ADDRESS_MODE", "ANCHORD_DHCP_HOSTNAME",
		"ANCHORD_POLL_INTERVAL", "ANCHORD_DHCP_BACKOFF_MAX",
		"ANCHORD_LOG_LEVEL", "COMPOSE_PROJECT_NAME", "DOCKER_HOST",
	} {
		t.Setenv(k, "")
	}
	// MetricsAddr uses LookupEnv (unset != empty), so we have to
	// explicitly unset it for "default" tests rather than just blanking.
	t.Setenv("ANCHORD_METRICS_ADDR", "")
	_ = os.Unsetenv("ANCHORD_METRICS_ADDR")
}

func TestLoad_RequiresProject(t *testing.T) {
	clearAnchordEnv(t)
	_, err := LoadNetworkAnchor()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_PROJECT") {
		t.Fatalf("expected error mentioning ANCHORD_PROJECT, got: %v", err)
	}
}

func TestLoad_ComposeProjectFallback(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("COMPOSE_PROJECT_NAME", "from-compose")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ComposeProject != "from-compose" {
		t.Errorf("ComposeProject=%q, want from-compose", cfg.ComposeProject)
	}
}

func TestLoad_ProjectOverridesCompose(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "explicit")
	t.Setenv("COMPOSE_PROJECT_NAME", "from-compose")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ComposeProject != "explicit" {
		t.Errorf("ANCHORD_PROJECT should win over COMPOSE_PROJECT_NAME, got %q", cfg.ComposeProject)
	}
}

// TestLoad_NoVLANParentRequired guards the v2 contract: anchord no
// longer owns macvlan creation, so the old required ANCHORD_VLAN_PARENT
// must not gate startup.
func TestLoad_NoVLANParentRequired(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ExtIfaceName != "eth0" {
		t.Errorf("ExtIfaceName default in v2 should be eth0, got %q", cfg.ExtIfaceName)
	}
}

func TestLoad_DefaultsAndDerivations(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ExtIfaceName != "eth0" {
		t.Errorf("ExtIfaceName default: %q", cfg.ExtIfaceName)
	}
	if cfg.AddressMode != AddressModeBootstrap {
		t.Errorf("AddressMode default should be bootstrap, got %q", cfg.AddressMode)
	}
	if cfg.DHCPHostname != "mailcow" {
		t.Errorf("DHCPHostname should default to project name, got %q", cfg.DHCPHostname)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("PollInterval default: %s", cfg.PollInterval)
	}
	if cfg.DHCPBackoffMax != 5*time.Minute {
		t.Errorf("DHCPBackoffMax default: %s", cfg.DHCPBackoffMax)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: %q", cfg.LogLevel)
	}
	if cfg.DockerHost != "unix:///var/run/docker.sock" {
		t.Errorf("DockerHost default: %q", cfg.DockerHost)
	}
	if cfg.MetricsAddr != "127.0.0.1:9090" {
		t.Errorf("MetricsAddr default: %q", cfg.MetricsAddr)
	}
}

func TestLoad_ExtIfaceOverride(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_EXT_IFACE", "eth1")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ExtIfaceName != "eth1" {
		t.Errorf("ExtIfaceName override: got %q want eth1", cfg.ExtIfaceName)
	}
}

func TestLoad_ExtNetworkOptional(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ExtNetwork != "" {
		t.Errorf("ExtNetwork should default to empty (fallback path), got %q", cfg.ExtNetwork)
	}
}

func TestLoad_ExtNetworkSet(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_EXT_NETWORK", "dmz_macvlan")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ExtNetwork != "dmz_macvlan" {
		t.Errorf("ExtNetwork: got %q want dmz_macvlan", cfg.ExtNetwork)
	}
}

func TestLoad_AddressModeOverride(t *testing.T) {
	cases := []struct {
		raw  string
		want AddressMode
	}{
		{"bootstrap", AddressModeBootstrap},
		{"dhcp-refresh", AddressModeDHCPRefresh},
		{"slaac-ra-only", AddressModeSLAACRAOnly},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			clearAnchordEnv(t)
			t.Setenv("ANCHORD_PROJECT", "mailcow")
			t.Setenv("ANCHORD_ADDRESS_MODE", tc.raw)
			cfg, err := LoadNetworkAnchor()
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if cfg.AddressMode != tc.want {
				t.Errorf("got %q want %q", cfg.AddressMode, tc.want)
			}
		})
	}
}

func TestLoad_AddressModeInvalid(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_ADDRESS_MODE", "gibberish")
	_, err := LoadNetworkAnchor()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_ADDRESS_MODE") {
		t.Fatalf("expected error mentioning ANCHORD_ADDRESS_MODE, got: %v", err)
	}
}

func TestLoad_HostnameOverride(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_DHCP_HOSTNAME", "mail.example.com")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.DHCPHostname != "mail.example.com" {
		t.Errorf("got %q", cfg.DHCPHostname)
	}
}

func TestLoad_PollIntervalOverride(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_POLL_INTERVAL", "5s")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.PollInterval != 5*time.Second {
		t.Errorf("got %s", cfg.PollInterval)
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		want    time.Duration
		wantErr bool
	}{
		{"empty uses default", "", 30 * time.Second, false},
		{"plain int = seconds", "45", 45 * time.Second, false},
		{"duration string", "2m30s", 2*time.Minute + 30*time.Second, false},
		{"invalid", "wat", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("X_ANCHORD_TEST_DURATION", tc.val)
			d, err := parseDuration("X_ANCHORD_TEST_DURATION", 30*time.Second)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && d != tc.want {
				t.Errorf("got %s want %s", d, tc.want)
			}
		})
	}
}

func TestGetenvDefault(t *testing.T) {
	t.Setenv("X_ANCHORD_TEST_GETENV", "")
	if got := getenvDefault("X_ANCHORD_TEST_GETENV", "fallback"); got != "fallback" {
		t.Errorf("empty should yield default, got %q", got)
	}
	t.Setenv("X_ANCHORD_TEST_GETENV", "set")
	if got := getenvDefault("X_ANCHORD_TEST_GETENV", "fallback"); got != "set" {
		t.Errorf("set should yield value, got %q", got)
	}
}

func TestLoadServiceAnchor_Defaults(t *testing.T) {
	for _, k := range []string{
		"ANCHORD_GATEWAY_HOSTNAME", "ANCHORD_GATEWAY_RESOLVE_INTERVAL",
		"ANCHORD_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
	cfg, err := LoadServiceAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.GatewayHostname != "anchord" {
		t.Errorf("GatewayHostname default: %q", cfg.GatewayHostname)
	}
	if cfg.ResolveInterval != 5*time.Second {
		t.Errorf("ResolveInterval default: %s", cfg.ResolveInterval)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: %q", cfg.LogLevel)
	}
}

func TestLoadServiceAnchor_Overrides(t *testing.T) {
	t.Setenv("ANCHORD_GATEWAY_HOSTNAME", "router")
	t.Setenv("ANCHORD_GATEWAY_RESOLVE_INTERVAL", "2s")
	t.Setenv("ANCHORD_LOG_LEVEL", "debug")
	cfg, err := LoadServiceAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.GatewayHostname != "router" {
		t.Errorf("hostname: %q", cfg.GatewayHostname)
	}
	if cfg.ResolveInterval != 2*time.Second {
		t.Errorf("interval: %s", cfg.ResolveInterval)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("level: %q", cfg.LogLevel)
	}
}

func TestLoadServiceAnchor_RejectsZeroInterval(t *testing.T) {
	t.Setenv("ANCHORD_GATEWAY_HOSTNAME", "")
	t.Setenv("ANCHORD_GATEWAY_RESOLVE_INTERVAL", "0")
	t.Setenv("ANCHORD_GATEWAY_IP", "")
	if _, err := LoadServiceAnchor(); err == nil {
		t.Fatal("expected error for zero interval")
	}
}

// F-40: ANCHORD_GATEWAY_IP single-value parsing for either family.
func TestLoadServiceAnchor_GatewayIPSingle(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want string // expected IP.String() of the single parsed result
	}{
		{"v4", "192.168.150.1", "192.168.150.1"},
		{"v6", "fd00::1", "fd00::1"},
		{"v4 with whitespace", "  10.0.0.5  ", "10.0.0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANCHORD_GATEWAY_HOSTNAME", "")
			t.Setenv("ANCHORD_GATEWAY_RESOLVE_INTERVAL", "")
			t.Setenv("ANCHORD_GATEWAY_IP", tc.val)
			cfg, err := LoadServiceAnchor()
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if len(cfg.GatewayIPs) != 1 {
				t.Fatalf("got %d IPs, want 1", len(cfg.GatewayIPs))
			}
			if cfg.GatewayIPs[0].String() != tc.want {
				t.Errorf("got %q, want %q", cfg.GatewayIPs[0].String(), tc.want)
			}
		})
	}
}

// F-40: comma-separated v4+v6 pair, in either order.
func TestLoadServiceAnchor_GatewayIPDualStack(t *testing.T) {
	for _, raw := range []string{
		"192.168.150.1,fd00::1",
		"fd00::1, 192.168.150.1", // order independence + whitespace
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("ANCHORD_GATEWAY_HOSTNAME", "")
			t.Setenv("ANCHORD_GATEWAY_RESOLVE_INTERVAL", "")
			t.Setenv("ANCHORD_GATEWAY_IP", raw)
			cfg, err := LoadServiceAnchor()
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if len(cfg.GatewayIPs) != 2 {
				t.Fatalf("got %d IPs, want 2 (one v4 + one v6)", len(cfg.GatewayIPs))
			}
			var v4, v6 bool
			for _, ip := range cfg.GatewayIPs {
				if ip.To4() != nil {
					v4 = true
				} else {
					v6 = true
				}
			}
			if !v4 || !v6 {
				t.Errorf("expected one of each family, got %v", cfg.GatewayIPs)
			}
		})
	}
}

// F-40: malformed IP must be a fatal error (loud), not silently fall
// back to DNS — operators need to see misconfiguration.
func TestLoadServiceAnchor_GatewayIPInvalid(t *testing.T) {
	t.Setenv("ANCHORD_GATEWAY_HOSTNAME", "")
	t.Setenv("ANCHORD_GATEWAY_RESOLVE_INTERVAL", "")
	t.Setenv("ANCHORD_GATEWAY_IP", "not-an-ip")
	_, err := LoadServiceAnchor()
	if err == nil {
		t.Fatal("expected error for malformed IP")
	}
	if !strings.Contains(err.Error(), "ANCHORD_GATEWAY_IP") {
		t.Errorf("error should mention the env var name, got: %v", err)
	}
}

// F-40: two IPs of the same family is a configuration error — we
// only accept one v4 + one v6.
func TestLoadServiceAnchor_GatewayIPDuplicateFamily(t *testing.T) {
	t.Setenv("ANCHORD_GATEWAY_HOSTNAME", "")
	t.Setenv("ANCHORD_GATEWAY_RESOLVE_INTERVAL", "")
	t.Setenv("ANCHORD_GATEWAY_IP", "10.0.0.1,10.0.0.2")
	_, err := LoadServiceAnchor()
	if err == nil {
		t.Fatal("expected error for two v4 IPs")
	}
}

// F-40: empty/unset means fall back to DNS mode (existing behaviour).
func TestLoadServiceAnchor_GatewayIPEmpty(t *testing.T) {
	t.Setenv("ANCHORD_GATEWAY_HOSTNAME", "")
	t.Setenv("ANCHORD_GATEWAY_RESOLVE_INTERVAL", "")
	t.Setenv("ANCHORD_GATEWAY_IP", "")
	cfg, err := LoadServiceAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(cfg.GatewayIPs) != 0 {
		t.Errorf("empty env should yield no IPs (DNS-mode fallback), got %v", cfg.GatewayIPs)
	}
}

func TestMetricsAddrFromEnv(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want string
	}{
		{"unset → loopback default", false, "", "127.0.0.1:9090"},
		{"set → value", true, ":9090", ":9090"},
		{"explicit empty → disabled", true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("ANCHORD_METRICS_ADDR", tc.val)
			} else {
				_ = os.Unsetenv("ANCHORD_METRICS_ADDR")
			}
			if got := metricsAddrFromEnv(); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestFingerprintDeterministic(t *testing.T) {
	c1 := &NetworkAnchor{ComposeProject: "mailcow", ExtIfaceName: "eth0"}
	c2 := &NetworkAnchor{ComposeProject: "mailcow", ExtIfaceName: "eth0"}
	c3 := &NetworkAnchor{ComposeProject: "mailcow", ExtIfaceName: "eth1"}
	c4 := &NetworkAnchor{ComposeProject: "nextcloud", ExtIfaceName: "eth0"}
	if c1.Fingerprint() != c2.Fingerprint() {
		t.Errorf("fingerprint not deterministic")
	}
	if c1.Fingerprint() == c3.Fingerprint() {
		t.Errorf("fingerprint should change with ext iface")
	}
	if c1.Fingerprint() == c4.Fingerprint() {
		t.Errorf("fingerprint should change with project name")
	}
}

func TestParseAddressMode(t *testing.T) {
	cases := []struct {
		raw     string
		want    AddressMode
		wantErr bool
	}{
		{"", AddressModeBootstrap, false},
		{"bootstrap", AddressModeBootstrap, false},
		{"dhcp-refresh", AddressModeDHCPRefresh, false},
		{"slaac-ra-only", AddressModeSLAACRAOnly, false},
		{"BOOTSTRAP", "", true}, // case-sensitive — keep the env contract tight
		{"static", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseAddressMode(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
