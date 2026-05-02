package config

import (
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
		"ANCHORD_PROJECT", "ANCHORD_VLAN_PARENT", "ANCHORD_EXT_IFACE",
		"ANCHORD_EXT_MAC", "ANCHORD_DHCP_HOSTNAME", "ANCHORD_POLL_INTERVAL",
		"ANCHORD_DHCP_BACKOFF_MAX", "ANCHORD_LOG_LEVEL",
		"COMPOSE_PROJECT_NAME", "DOCKER_HOST",
	} {
		t.Setenv(k, "")
	}
}

func TestDeriveMAC(t *testing.T) {
	a := deriveMAC("mailcow")
	b := deriveMAC("mailcow")
	if a.String() != b.String() {
		t.Errorf("not deterministic: %s vs %s", a, b)
	}
	c := deriveMAC("nextcloud")
	if a.String() == c.String() {
		t.Errorf("collision between distinct projects: %s", a)
	}
	if a[0] != 0x02 || a[1] != 0x42 {
		t.Errorf("expected 02:42 OUI prefix, got %s", a)
	}
	// First octet semantics:
	//   bit 0 (LSB) = 0 → unicast
	//   bit 1       = 1 → locally administered
	if a[0]&0x01 != 0 {
		t.Errorf("first octet must be unicast (LSB clear): %s", a)
	}
	if a[0]&0x02 == 0 {
		t.Errorf("first octet must be locally administered (bit 1 set): %s", a)
	}
}

func TestLoad_RequiresProject(t *testing.T) {
	clearAnchordEnv(t)
	_, err := LoadNetworkAnchor()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_PROJECT") {
		t.Fatalf("expected error mentioning ANCHORD_PROJECT, got: %v", err)
	}
}

func TestLoad_RequiresVLANParent(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	_, err := LoadNetworkAnchor()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_VLAN_PARENT") {
		t.Fatalf("expected error mentioning ANCHORD_VLAN_PARENT, got: %v", err)
	}
}

func TestLoad_ComposeProjectFallback(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("COMPOSE_PROJECT_NAME", "from-compose")
	t.Setenv("ANCHORD_VLAN_PARENT", "eth0.42")
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
	t.Setenv("ANCHORD_VLAN_PARENT", "eth0.42")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ComposeProject != "explicit" {
		t.Errorf("ANCHORD_PROJECT should win over COMPOSE_PROJECT_NAME, got %q", cfg.ComposeProject)
	}
}

func TestLoad_DefaultsAndDerivations(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_VLAN_PARENT", "eth0.42")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ExtIfaceName != "anchord-ext" {
		t.Errorf("ExtIfaceName default: %q", cfg.ExtIfaceName)
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
	want := deriveMAC("mailcow").String()
	if cfg.ExtMAC.String() != want {
		t.Errorf("ExtMAC should be derived from project name: got %s want %s", cfg.ExtMAC, want)
	}
}

func TestLoad_HostnameOverride(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_VLAN_PARENT", "eth0.42")
	t.Setenv("ANCHORD_DHCP_HOSTNAME", "mail.example.com")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.DHCPHostname != "mail.example.com" {
		t.Errorf("got %q", cfg.DHCPHostname)
	}
}

func TestLoad_MACOverride(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_VLAN_PARENT", "eth0.42")
	t.Setenv("ANCHORD_EXT_MAC", "aa:bb:cc:dd:ee:ff")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ExtMAC.String() != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("got %s", cfg.ExtMAC)
	}
}

func TestLoad_MACInvalid(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_VLAN_PARENT", "eth0.42")
	t.Setenv("ANCHORD_EXT_MAC", "not-a-mac")
	_, err := LoadNetworkAnchor()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_EXT_MAC") {
		t.Errorf("expected error mentioning ANCHORD_EXT_MAC, got: %v", err)
	}
}

func TestLoad_PollIntervalOverride(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_VLAN_PARENT", "eth0.42")
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
	if _, err := LoadServiceAnchor(); err == nil {
		t.Fatal("expected error for zero interval")
	}
}

func TestFingerprintDeterministic(t *testing.T) {
	c1 := &NetworkAnchor{ComposeProject: "mailcow", VLANParent: "eth0.42"}
	c2 := &NetworkAnchor{ComposeProject: "mailcow", VLANParent: "eth0.42"}
	c3 := &NetworkAnchor{ComposeProject: "mailcow", VLANParent: "eth0.99"}
	c4 := &NetworkAnchor{ComposeProject: "nextcloud", VLANParent: "eth0.42"}
	if c1.Fingerprint() != c2.Fingerprint() {
		t.Errorf("fingerprint not deterministic")
	}
	if c1.Fingerprint() == c3.Fingerprint() {
		t.Errorf("fingerprint should change with VLAN parent")
	}
	if c1.Fingerprint() == c4.Fingerprint() {
		t.Errorf("fingerprint should change with project name")
	}
}
