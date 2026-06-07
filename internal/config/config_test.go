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
		"ANCHORD_SHARED_NETWORK", "ANCHORD_ADDRESS_MODE",
		"ANCHORD_DHCP_HOSTNAME", "ANCHORD_POLL_INTERVAL",
		"ANCHORD_DHCP_BACKOFF_MAX", "ANCHORD_LOG_LEVEL",
		"ANCHORD_LABEL_SELECTOR", "ANCHORD_AUTOSTART_SIBLINGS",
		"ANCHORD_AUTOFIX_DEAD_NETNS",
		"ANCHORD_MANAGED_SA_TARGET", "ANCHORD_MANAGED_SA_NAME",
		"ANCHORD_MANAGED_SA_IMAGE", "ANCHORD_MANAGED_SA_GATEWAY_IP",
		"ANCHORD_MANAGED_SA_EXTRA_ENV", "ANCHORD_MANAGED_SA_LABELS",
		"COMPOSE_PROJECT_NAME", "DOCKER_HOST",
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

// F-44: ANCHORD_SHARED_NETWORK is optional; default empty means
// "use heuristic". Validation of "must be in self-networks" happens
// in internal/sharednet, not at config-load time.
func TestLoad_SharedNetworkPin(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_SHARED_NETWORK", "wrap_transit")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.SharedNetwork != "wrap_transit" {
		t.Errorf("SharedNetwork: got %q want wrap_transit", cfg.SharedNetwork)
	}
}

func TestLoad_SharedNetworkEmptyByDefault(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.SharedNetwork != "" {
		t.Errorf("SharedNetwork should default to empty, got %q", cfg.SharedNetwork)
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

// F-42 parser tests — verbatim from SPEC-LABEL-SELECTOR-DRAFT.md
// §"Acceptance tests / Unit". These are the canonical cases the spec
// commits to; if any of them changes here, the spec changes too.
func TestParseLabelSelector(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantMap   map[string]string
		wantErr   string // substring; "" for no error
	}{
		{
			name:    "empty input yields empty map",
			raw:     "",
			wantMap: map[string]string{},
		},
		{
			name:    "whitespace-only input yields empty map",
			raw:     "   ",
			wantMap: map[string]string{},
		},
		{
			name:    "single pair",
			raw:     "a=1",
			wantMap: map[string]string{"a": "1"},
		},
		{
			name:    "comma-joined whitespace tolerant",
			raw:     "a=1, b = 2",
			wantMap: map[string]string{"a": "1", "b": "2"},
		},
		{
			name:    "empty value is valid (matches literal empty)",
			raw:     "anchord.role=",
			wantMap: map[string]string{"anchord.role": ""},
		},
		{
			name:    "duplicate key with same value collapses (idempotent)",
			raw:     "a=1,a=1",
			wantMap: map[string]string{"a": "1"},
		},
		{
			name:    "duplicate key with conflicting values is fatal",
			raw:     "a=1,a=2",
			wantErr: "duplicate key",
		},
		{
			name:    "entry without '=' is fatal",
			raw:     "nokey",
			wantErr: "missing '='",
		},
		{
			name:    "empty key is fatal",
			raw:     "=value",
			wantErr: "empty key",
		},
		{
			name:    "F-42 example — Authentik LDAP outpost role selector",
			raw:     "anchord.role=ldap-outpost",
			wantMap: map[string]string{"anchord.role": "ldap-outpost"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLabelSelector(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err=%v; want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != len(tc.wantMap) {
				t.Fatalf("size mismatch: got %v want %v", got, tc.wantMap)
			}
			for k, v := range tc.wantMap {
				if got[k] != v {
					t.Errorf("key %q: got %q want %q", k, got[k], v)
				}
			}
		})
	}
}

// F-42: ANCHORD_PROJECT becomes optional when a label selector is
// supplied. The legacy required-error must NOT fire.
func TestLoad_LabelSelectorReplacesProject(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_LABEL_SELECTOR", "anchord.role=ldap-outpost")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(cfg.LabelSelector) != 1 || cfg.LabelSelector["anchord.role"] != "ldap-outpost" {
		t.Errorf("LabelSelector = %v, want {anchord.role: ldap-outpost}", cfg.LabelSelector)
	}
	if cfg.ComposeProject != "" {
		t.Errorf("ComposeProject should remain empty when selector replaces it, got %q", cfg.ComposeProject)
	}
	// DHCP hostname fallback: when project is absent, the first
	// selector value becomes the hostname (deterministic, sorted).
	if cfg.DHCPHostname != "ldap-outpost" {
		t.Errorf("DHCPHostname fallback = %q, want ldap-outpost", cfg.DHCPHostname)
	}
}

// Both set is allowed — the precedence handling lives in main.go
// (PROJECT ignored with a warn). Config just records both; the
// loader should not reject this.
func TestLoad_LabelSelectorAndProject_BothLoad(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-authentik")
	t.Setenv("ANCHORD_LABEL_SELECTOR", "anchord.role=ldap-outpost")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ComposeProject != "ix-authentik" {
		t.Errorf("ComposeProject not preserved: %q", cfg.ComposeProject)
	}
	if cfg.LabelSelector["anchord.role"] != "ldap-outpost" {
		t.Errorf("LabelSelector not preserved: %v", cfg.LabelSelector)
	}
}

// F-42: malformed selector fails fast — same code path as other
// malformed-env vars (no silent fall-back to project-only).
func TestLoad_LabelSelectorMalformed(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	t.Setenv("ANCHORD_LABEL_SELECTOR", "this-has-no-equals")
	_, err := LoadNetworkAnchor()
	if err == nil {
		t.Fatal("expected error for malformed selector")
	}
}

// Greenfield baseline: no selector set, only project. Existing
// behaviour preserved — LabelSelector is empty, ComposeProject
// drives discovery.
func TestLoad_LegacyProjectOnly(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(cfg.LabelSelector) != 0 {
		t.Errorf("LabelSelector should be empty in legacy path, got %v", cfg.LabelSelector)
	}
	if cfg.ComposeProject != "mailcow" {
		t.Errorf("ComposeProject lost: %q", cfg.ComposeProject)
	}
}

// F-43: AutostartSiblings defaults to true so the auto-rescue happens
// without operator opt-in. Explicit "false" disables. Garbage is fatal.
func TestLoad_AutostartSiblings(t *testing.T) {
	cases := []struct {
		name      string
		val       string
		want      bool
		wantErr   bool
	}{
		{"unset → default true", "", true, false},
		{"explicit true", "true", true, false},
		{"explicit false", "false", false, false},
		{"shorthand 1", "1", true, false},
		{"shorthand 0", "0", false, false},
		{"TRUE", "TRUE", true, false},
		{"FALSE", "FALSE", false, false},
		{"garbage rejected", "yes-please", false, true},
		{"empty-string treated as default", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAnchordEnv(t)
			t.Setenv("ANCHORD_PROJECT", "mailcow")
			t.Setenv("ANCHORD_AUTOSTART_SIBLINGS", tc.val)
			cfg, err := LoadNetworkAnchor()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for value %q", tc.val)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if cfg.AutostartSiblings != tc.want {
				t.Errorf("got %v want %v", cfg.AutostartSiblings, tc.want)
			}
		})
	}
}

// Issue #10: AutoFixDeadNetns defaults to true so v1.2.0 operators
// get the recreate-cascade without opt-in. Same shape as
// AutostartSiblings: explicit false disables, garbage is fatal.
func TestLoad_AutoFixDeadNetns(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		want    bool
		wantErr bool
	}{
		{"unset → default true", "", true, false},
		{"explicit true", "true", true, false},
		{"explicit false", "false", false, false},
		{"shorthand 1", "1", true, false},
		{"shorthand 0", "0", false, false},
		{"TRUE", "TRUE", true, false},
		{"FALSE", "FALSE", false, false},
		{"garbage rejected", "maybe", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAnchordEnv(t)
			t.Setenv("ANCHORD_PROJECT", "mailcow")
			t.Setenv("ANCHORD_AUTOFIX_DEAD_NETNS", tc.val)
			cfg, err := LoadNetworkAnchor()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for value %q", tc.val)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if cfg.AutoFixDeadNetns != tc.want {
				t.Errorf("got %v want %v", cfg.AutoFixDeadNetns, tc.want)
			}
		})
	}
}

// F-45: unset ANCHORD_MANAGED_SA_TARGET → inactive recipe; F-43-only
// behaviour at runtime. .Active() reports false.
func TestLoad_ManagedSA_InactiveByDefault(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "mailcow")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ManagedSA.Active() {
		t.Errorf("expected inactive recipe by default, got %+v", cfg.ManagedSA)
	}
}

// F-45: only TARGET set → all other fields filled with statically
// derivable defaults; Image and GatewayIP stay empty for runtime
// resolution.
func TestLoad_ManagedSA_DefaultsFromTarget(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-authentik")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "ak-outpost-ldap")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !cfg.ManagedSA.Active() {
		t.Fatal("recipe should be active when TARGET is set")
	}
	if cfg.ManagedSA.Target != "ak-outpost-ldap" {
		t.Errorf("Target: got %q", cfg.ManagedSA.Target)
	}
	if cfg.ManagedSA.Name != "ak-outpost-ldap-service-anchor" {
		t.Errorf("default Name should be <Target>-service-anchor, got %q", cfg.ManagedSA.Name)
	}
	if cfg.ManagedSA.Image != "" {
		t.Errorf("default Image should be empty (resolve at runtime), got %q", cfg.ManagedSA.Image)
	}
	if cfg.ManagedSA.GatewayIP != "" {
		t.Errorf("default GatewayIP should be empty (resolve at runtime), got %q", cfg.ManagedSA.GatewayIP)
	}
	if len(cfg.ManagedSA.ExtraEnv) != 0 {
		t.Errorf("default ExtraEnv should be empty, got %v", cfg.ManagedSA.ExtraEnv)
	}
}

// F-45: explicit overrides for all fields are preserved.
func TestLoad_ManagedSA_AllExplicit(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-authentik")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "ak-outpost-ldap")
	t.Setenv("ANCHORD_MANAGED_SA_NAME", "custom-sa-name")
	t.Setenv("ANCHORD_MANAGED_SA_IMAGE", "ghcr.io/example/anchord:v3")
	t.Setenv("ANCHORD_MANAGED_SA_GATEWAY_IP", "172.31.80.181")
	t.Setenv("ANCHORD_MANAGED_SA_EXTRA_ENV", `{"FOO":"bar","BAZ":"qux"}`)
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ManagedSA.Name != "custom-sa-name" {
		t.Errorf("Name override lost: %q", cfg.ManagedSA.Name)
	}
	if cfg.ManagedSA.Image != "ghcr.io/example/anchord:v3" {
		t.Errorf("Image override lost: %q", cfg.ManagedSA.Image)
	}
	if cfg.ManagedSA.GatewayIP != "172.31.80.181" {
		t.Errorf("GatewayIP override lost: %q", cfg.ManagedSA.GatewayIP)
	}
	if cfg.ManagedSA.ExtraEnv["FOO"] != "bar" || cfg.ManagedSA.ExtraEnv["BAZ"] != "qux" {
		t.Errorf("ExtraEnv not parsed correctly: %v", cfg.ManagedSA.ExtraEnv)
	}
}

// F-45: malformed EXTRA_ENV JSON is a fatal startup error — silently
// dropping a typo would hide misconfiguration of a security-relevant
// env-injection field.
func TestLoad_ManagedSA_ExtraEnvMalformed(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-authentik")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "tgt")
	t.Setenv("ANCHORD_MANAGED_SA_EXTRA_ENV", "{this-is: not, valid: json}")
	_, err := LoadNetworkAnchor()
	if err == nil {
		t.Fatal("expected fatal error for malformed EXTRA_ENV JSON")
	}
	if !strings.Contains(err.Error(), "ANCHORD_MANAGED_SA_EXTRA_ENV") {
		t.Errorf("error must mention the env var name, got: %v", err)
	}
}

// Issue #3 / F-45: operator-supplied labels via JSON env land on the
// recipe. anchord.identity / anchord.expose are the headline use case
// (so F-42 selector mode can discover its own F-45 spawn).
func TestLoad_ManagedSA_LabelsParsed(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-nextcloud")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "nextcloud-aio-talk")
	t.Setenv("ANCHORD_MANAGED_SA_LABELS", `{"anchord.identity":"nextcloud-talk","anchord.expose":"tcp/3478:3478,udp/3478:3478"}`)
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ManagedSA.Labels["anchord.identity"] != "nextcloud-talk" {
		t.Errorf("anchord.identity not parsed: %v", cfg.ManagedSA.Labels)
	}
	if cfg.ManagedSA.Labels["anchord.expose"] != "tcp/3478:3478,udp/3478:3478" {
		t.Errorf("anchord.expose not parsed: %v", cfg.ManagedSA.Labels)
	}
}

// Issue #3: malformed JSON is fatal (same policy as EXTRA_ENV).
func TestLoad_ManagedSA_LabelsMalformed(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-nextcloud")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "tgt")
	t.Setenv("ANCHORD_MANAGED_SA_LABELS", "{not: json}")
	_, err := LoadNetworkAnchor()
	if err == nil {
		t.Fatal("expected fatal error for malformed LABELS JSON")
	}
	if !strings.Contains(err.Error(), "ANCHORD_MANAGED_SA_LABELS") {
		t.Errorf("error must mention the env var name, got: %v", err)
	}
}

// Issue #2 + #3: compose.* keys are reserved and rejected at load.
func TestLoad_ManagedSA_LabelsRejectsComposeKeys(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-nextcloud")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "tgt")
	t.Setenv("ANCHORD_MANAGED_SA_LABELS", `{"com.docker.compose.service":"x"}`)
	_, err := LoadNetworkAnchor()
	if err == nil {
		t.Fatal("expected fatal error for compose.* label")
	}
	if !strings.Contains(err.Error(), "com.docker.compose.") {
		t.Errorf("error must name the offending prefix, got: %v", err)
	}
}

// Issue #3: anchord.managed-by is reserved (built-in bookkeeping
// value), so operator-supplied override is rejected at load.
func TestLoad_ManagedSA_LabelsRejectsManagedBy(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-nextcloud")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "tgt")
	t.Setenv("ANCHORD_MANAGED_SA_LABELS", `{"anchord.managed-by":"custom"}`)
	_, err := LoadNetworkAnchor()
	if err == nil {
		t.Fatal("expected fatal error for anchord.managed-by override")
	}
	if !strings.Contains(err.Error(), "anchord.managed-by") {
		t.Errorf("error must name the reserved key, got: %v", err)
	}
}

// F-45: empty EXTRA_ENV is valid → empty (non-nil) map.
func TestLoad_ManagedSA_ExtraEnvEmpty(t *testing.T) {
	clearAnchordEnv(t)
	t.Setenv("ANCHORD_PROJECT", "ix-authentik")
	t.Setenv("ANCHORD_MANAGED_SA_TARGET", "tgt")
	t.Setenv("ANCHORD_MANAGED_SA_EXTRA_ENV", "")
	cfg, err := LoadNetworkAnchor()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.ManagedSA.ExtraEnv == nil {
		t.Error("ExtraEnv should never be nil — caller ranges over it")
	}
	if len(cfg.ManagedSA.ExtraEnv) != 0 {
		t.Errorf("empty input should yield empty map, got %v", cfg.ManagedSA.ExtraEnv)
	}
}

func TestParseBoolDefault(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		def     bool
		want    bool
		wantErr bool
	}{
		{"unset returns default true", "", true, true, false},
		{"unset returns default false", "", false, false, false},
		{"whitespace-only treated as unset", "   ", true, true, false},
		{"explicit true overrides default false", "true", false, true, false},
		{"explicit false overrides default true", "false", true, false, false},
		{"invalid yields error", "maybe", true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("X_TEST_BOOL", tc.val)
			got, err := parseBoolDefault("X_TEST_BOOL", tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestFirstSelectorValue_Deterministic(t *testing.T) {
	// Two different keys must sort by key — running the function
	// repeatedly must yield the same value (no map-iteration random).
	sel := map[string]string{
		"zebra":       "z-val",
		"anchord.role": "lex-first",
	}
	for i := 0; i < 50; i++ {
		if got := firstSelectorValue(sel); got != "lex-first" {
			t.Fatalf("not deterministic on iteration %d: got %q", i, got)
		}
	}
	if got := firstSelectorValue(nil); got != "" {
		t.Errorf("empty selector should yield empty string, got %q", got)
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

// ---- LoadRebinder (F-48) ---------------------------------------------------

// clearRebinderEnv blanks every env var LoadRebinder consults so each
// test starts from a deterministic baseline.
func clearRebinderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANCHORD_FOLLOW_NETWORK",
		"ANCHORD_FOLLOW_TARGET",
		"ANCHORD_FOLLOW_RESTART",
		"ANCHORD_FOLLOW_EVENT_BACKOFF",
		"COMPOSE_PROJECT_NAME",
		"DOCKER_HOST",
		"ANCHORD_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("ANCHORD_METRICS_ADDR", "")
	_ = os.Unsetenv("ANCHORD_METRICS_ADDR")
}

func TestLoadRebinder_RequiresFollowNetwork(t *testing.T) {
	clearRebinderEnv(t)
	t.Setenv("ANCHORD_FOLLOW_TARGET", "sync")
	_, err := LoadRebinder()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_FOLLOW_NETWORK") {
		t.Fatalf("expected ANCHORD_FOLLOW_NETWORK error, got: %v", err)
	}
}

func TestLoadRebinder_RequiresFollowTarget(t *testing.T) {
	clearRebinderEnv(t)
	t.Setenv("ANCHORD_FOLLOW_NETWORK", "ix-mailcow_mailcow-network")
	_, err := LoadRebinder()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_FOLLOW_TARGET") {
		t.Fatalf("expected ANCHORD_FOLLOW_TARGET error, got: %v", err)
	}
}

func TestLoadRebinder_MinimalRequiredVars(t *testing.T) {
	clearRebinderEnv(t)
	t.Setenv("ANCHORD_FOLLOW_NETWORK", "ix-mailcow_mailcow-network")
	t.Setenv("ANCHORD_FOLLOW_TARGET", "sync")
	cfg, err := LoadRebinder()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FollowNetwork != "ix-mailcow_mailcow-network" {
		t.Errorf("FollowNetwork = %q", cfg.FollowNetwork)
	}
	if cfg.FollowTarget != "sync" {
		t.Errorf("FollowTarget = %q", cfg.FollowTarget)
	}
	if cfg.Restart {
		t.Errorf("Restart default should be false, got true")
	}
	if cfg.EventBackoff != 2*time.Second {
		t.Errorf("EventBackoff default should be 2s, got %s", cfg.EventBackoff)
	}
	if cfg.SelfProject != "" {
		t.Errorf("SelfProject should be empty when COMPOSE_PROJECT_NAME unset, got %q", cfg.SelfProject)
	}
}

func TestLoadRebinder_SelfProjectFromCompose(t *testing.T) {
	clearRebinderEnv(t)
	t.Setenv("ANCHORD_FOLLOW_NETWORK", "net")
	t.Setenv("ANCHORD_FOLLOW_TARGET", "tgt")
	t.Setenv("COMPOSE_PROJECT_NAME", "follower-stack")
	cfg, err := LoadRebinder()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SelfProject != "follower-stack" {
		t.Errorf("SelfProject = %q, want follower-stack", cfg.SelfProject)
	}
}

func TestLoadRebinder_RestartOptIn(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
		// Empty string is treated as "unset" by parseBoolDefault → default false.
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			clearRebinderEnv(t)
			t.Setenv("ANCHORD_FOLLOW_NETWORK", "net")
			t.Setenv("ANCHORD_FOLLOW_TARGET", "tgt")
			if tc.raw != "" {
				t.Setenv("ANCHORD_FOLLOW_RESTART", tc.raw)
			}
			cfg, err := LoadRebinder()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Restart != tc.want {
				t.Errorf("Restart = %v, want %v", cfg.Restart, tc.want)
			}
		})
	}
}

func TestLoadRebinder_RestartInvalid(t *testing.T) {
	clearRebinderEnv(t)
	t.Setenv("ANCHORD_FOLLOW_NETWORK", "net")
	t.Setenv("ANCHORD_FOLLOW_TARGET", "tgt")
	t.Setenv("ANCHORD_FOLLOW_RESTART", "maybe")
	_, err := LoadRebinder()
	if err == nil || !strings.Contains(err.Error(), "ANCHORD_FOLLOW_RESTART") {
		t.Fatalf("expected ANCHORD_FOLLOW_RESTART parse error, got: %v", err)
	}
}

func TestLoadRebinder_EventBackoff(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"5s", 5 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"10", 10 * time.Second}, // parseDuration accepts bare seconds
		{"0", 0},                  // zero is a valid (non-negative) backoff
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			clearRebinderEnv(t)
			t.Setenv("ANCHORD_FOLLOW_NETWORK", "net")
			t.Setenv("ANCHORD_FOLLOW_TARGET", "tgt")
			t.Setenv("ANCHORD_FOLLOW_EVENT_BACKOFF", tc.raw)
			cfg, err := LoadRebinder()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.EventBackoff != tc.want {
				t.Errorf("EventBackoff = %s, want %s", cfg.EventBackoff, tc.want)
			}
		})
	}
}

func TestLoadRebinder_EventBackoffNegative(t *testing.T) {
	clearRebinderEnv(t)
	t.Setenv("ANCHORD_FOLLOW_NETWORK", "net")
	t.Setenv("ANCHORD_FOLLOW_TARGET", "tgt")
	t.Setenv("ANCHORD_FOLLOW_EVENT_BACKOFF", "-1s")
	_, err := LoadRebinder()
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("expected non-negative error, got: %v", err)
	}
}

// ---- LoadWrapRebinder (F-49) -----------------------------------------------

func clearWrapRebinderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"COMPOSE_PROJECT_NAME",
		"ANCHORD_WRAP_POLL_INTERVAL",
		"ANCHORD_WRAP_RESTART_TIMEOUT",
		"DOCKER_HOST",
		"ANCHORD_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("ANCHORD_METRICS_ADDR", "")
	_ = os.Unsetenv("ANCHORD_METRICS_ADDR")
}

func TestLoadWrapRebinder_RequiresProject(t *testing.T) {
	clearWrapRebinderEnv(t)
	_, err := LoadWrapRebinder()
	if err == nil || !strings.Contains(err.Error(), "COMPOSE_PROJECT_NAME") {
		t.Fatalf("expected COMPOSE_PROJECT_NAME error, got: %v", err)
	}
}

func TestLoadWrapRebinder_Defaults(t *testing.T) {
	clearWrapRebinderEnv(t)
	t.Setenv("COMPOSE_PROJECT_NAME", "mailcow-anchord-wrap")
	cfg, err := LoadWrapRebinder()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SelfProject != "mailcow-anchord-wrap" {
		t.Errorf("SelfProject = %q", cfg.SelfProject)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("PollInterval default = %s, want 30s", cfg.PollInterval)
	}
	if cfg.RestartTimeout != 10*time.Second {
		t.Errorf("RestartTimeout default = %s, want 10s", cfg.RestartTimeout)
	}
}

func TestLoadWrapRebinder_PollIntervalPositive(t *testing.T) {
	clearWrapRebinderEnv(t)
	t.Setenv("COMPOSE_PROJECT_NAME", "ws")
	t.Setenv("ANCHORD_WRAP_POLL_INTERVAL", "0s")
	_, err := LoadWrapRebinder()
	if err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("expected must-be-positive error for poll, got: %v", err)
	}
}

func TestLoadWrapRebinder_RestartTimeoutPositive(t *testing.T) {
	clearWrapRebinderEnv(t)
	t.Setenv("COMPOSE_PROJECT_NAME", "ws")
	t.Setenv("ANCHORD_WRAP_RESTART_TIMEOUT", "0s")
	_, err := LoadWrapRebinder()
	if err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("expected must-be-positive error for restart timeout, got: %v", err)
	}
}

func TestLoadWrapRebinder_CustomDurations(t *testing.T) {
	clearWrapRebinderEnv(t)
	t.Setenv("COMPOSE_PROJECT_NAME", "ws")
	t.Setenv("ANCHORD_WRAP_POLL_INTERVAL", "1m")
	t.Setenv("ANCHORD_WRAP_RESTART_TIMEOUT", "5s")
	cfg, err := LoadWrapRebinder()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PollInterval != time.Minute {
		t.Errorf("PollInterval = %s, want 1m", cfg.PollInterval)
	}
	if cfg.RestartTimeout != 5*time.Second {
		t.Errorf("RestartTimeout = %s, want 5s", cfg.RestartTimeout)
	}
}

func TestLoadRebinder_TrimWhitespace(t *testing.T) {
	clearRebinderEnv(t)
	// Operators copy-paste from compose; trailing whitespace shouldn't
	// be a footgun.
	t.Setenv("ANCHORD_FOLLOW_NETWORK", "  ix-mailcow_mailcow-network  ")
	t.Setenv("ANCHORD_FOLLOW_TARGET", "  sync  ")
	cfg, err := LoadRebinder()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FollowNetwork != "ix-mailcow_mailcow-network" {
		t.Errorf("FollowNetwork not trimmed: %q", cfg.FollowNetwork)
	}
	if cfg.FollowTarget != "sync" {
		t.Errorf("FollowTarget not trimmed: %q", cfg.FollowTarget)
	}
}
