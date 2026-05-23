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
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
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

// ManagedSARecipe is the F-45 recipe for a service-anchor container
// the network-anchor will create on demand when a matching target
// appears. Activated by setting ANCHORD_MANAGED_SA_TARGET. All other
// fields have sensible defaults filled in at startup by the
// autostart watcher.
type ManagedSARecipe struct {
	// Target is the stable name of the container the managed
	// service-anchor will be bound to via
	// `network_mode: container:<Target>`. Empty means F-45 is
	// inactive — only the F-43 start-existing-Created-sibling path
	// runs.
	Target string

	// Name is the container name the network-anchor will create.
	// Default: "<Target>-service-anchor". The default is computed
	// at load time so cfg.ManagedSA.Name is always non-empty when
	// cfg.ManagedSA.Target is.
	Name string

	// Image is the container image. Empty means "use whatever image
	// the network-anchor itself is running" — resolved at runtime
	// by inspecting the anchord container against the Docker socket.
	Image string

	// GatewayIP is the value passed as ANCHORD_GATEWAY_IP into the
	// created service-anchor. Empty means "use the network-anchor's
	// own IP on the chosen shared network" — resolved at runtime
	// once the shared-network picker (F-44) has settled.
	GatewayIP string

	// ExtraEnv is additional env vars to inject into the created
	// service-anchor. Parsed from ANCHORD_MANAGED_SA_EXTRA_ENV as a
	// JSON object {"KEY": "value", …}; empty by default.
	ExtraEnv map[string]string

	// Labels are extra container labels to stamp onto the created
	// service-anchor. Parsed from ANCHORD_MANAGED_SA_LABELS as a JSON
	// object {"key": "value", …}; empty by default.
	//
	// Use case (issue #3): operators using ANCHORD_LABEL_SELECTOR
	// (F-42) need to mark the spawned service-anchor with the same
	// selector labels (anchord.identity, anchord.expose, …) so the
	// network-anchor's own discovery filter matches it. Without this,
	// the wrap-pattern is unusable in selector mode.
	//
	// Disallowed keys (fatal at load): com.docker.compose.* (per
	// issue #2 — anchord-managed containers are out-of-band w.r.t.
	// compose) and anchord.managed-by (reserved for the built-in
	// bookkeeping value). All other keys are passed through.
	Labels map[string]string
}

// Active reports whether F-45 is in play. False means the autostart
// watcher behaves exactly as F-43 (start-only on existing Created
// siblings); true adds the create-then-start code path.
func (r ManagedSARecipe) Active() bool { return r.Target != "" }

// NetworkAnchor holds resolved settings for the network-anchor mode.
type NetworkAnchor struct {
	// ComposeProject scopes which containers anchord watches.
	// Required. Usually injected as ${COMPOSE_PROJECT_NAME}.
	ComposeProject string

	// ExtIfaceName is the in-container name of the macvlan interface
	// Docker plumbed in via the external macvlan network. Default
	// "eth0" — the first network Docker attaches when no priorities
	// are set. Used only when ExtNetwork is empty. Unreliable on
	// stacks with 2+ networks because Docker's eth0/eth1 assignment
	// is not deterministic across recreates; see SPEC-v2-DRAFT F-37
	// and prefer ExtNetwork in that case.
	ExtIfaceName string

	// SharedNetwork is the operator-pinned network name for backend
	// IP reads — bypasses the F-44 co-attachment heuristic. When
	// non-empty, anchord uses this network and does not try to
	// auto-detect. Must be one of the networks anchord is attached
	// to (validated at startup, fatal on typo).
	//
	// Empty means heuristic mode: detect at startup, refine on
	// reconciles until a backend is observed (SPEC F-44 §"Behaviour"
	// points 1-2). The most-common single-shared-bridge stacks
	// don't need this var — the heuristic picks the only candidate.
	SharedNetwork string

	// ExtNetwork is the Docker network name of the external macvlan
	// (matches the `name:` field on the network in compose, e.g.
	// "dmz_macvlan"). When set, anchord queries the Docker API for
	// its own NetworkSettings.Networks[ExtNetwork].MacAddress and
	// resolves the in-container iface by MAC match — independent of
	// whether Docker happened to call it eth0 or eth1 this restart.
	// Takes precedence over ExtIfaceName. See SPEC-v2-DRAFT F-37.
	ExtNetwork string

	// ExtGatewayIP, when non-empty, is the gateway address anchord
	// installs as its default route on the external (macvlan)
	// interface — issue #6. Empty means "resolve at runtime from
	// Docker NetworkInspect on ExtNetwork.IPAM.Config[].Gateway".
	// Provide an explicit pin when the macvlan network is external
	// to Docker (no IPAM config visible to the daemon) or when the
	// Docker-side IPAM gateway differs from the actual L3 gateway.
	// Accepts v4 and/or v6 separated by comma — same shape as the
	// service-anchor's ANCHORD_GATEWAY_IP (F-40).
	ExtGatewayIPs []net.IP

	// AddressMode picks how the external IPv4 is obtained (bootstrap
	// vs dhcp-refresh vs slaac-ra-only). Default "bootstrap".
	AddressMode AddressMode

	// ManagedSA is the optional F-45 recipe for an anchord-managed
	// service-anchor. When Target is non-empty, the network-anchor's
	// event handler not only auto-starts existing Created-state
	// siblings (F-43) but also CREATES a service-anchor container on
	// demand when a matching target appears. Used for runtime-spawned
	// targets like authentik outposts that Docker Compose cannot
	// declare a `network_mode: container:<X>` peer for (Compose
	// halts on create-but-cant-start; F-43 alone never gets a
	// chance to retry because Compose aborts the deploy).
	//
	// All fields except Target have defaults: Name defaults to
	// "<Target>-service-anchor", Image defaults to the network-
	// anchor's own image (auto-detected at startup), GatewayIP
	// defaults to anchord's own IP on the chosen shared network
	// (post-F-44).
	ManagedSA ManagedSARecipe

	// AutostartSiblings controls whether the network-anchor watches
	// for `container start` events and starts any sibling container
	// in `Created` state whose `network_mode: container:<X>` matches
	// the just-started target (SPEC F-43). Default true.
	//
	// Use case: service-anchors in wrap-mode with a target container
	// that is spawned at runtime (e.g. authentik outposts via the
	// Docker API). Docker accepts `docker create --network
	// container:NONEXISTENT` but rejects start until the target
	// exists, and does NOT auto-retry. Setting this to true lets the
	// network-anchor be the auto-retrier.
	//
	// Requires `POST=1` on docker-socket-proxy (or equivalent
	// write-side socket access).
	AutostartSiblings bool

	// AutoFixDeadNetns controls whether the network-anchor, when it
	// recreates its F-45-managed service-anchor, also re-creates any
	// dependent containers that were netns-mode'd to the old SA's
	// container ID. Default true.
	//
	// Without this, the dependents (per-stack Traefik, acme renewers,
	// the wrapped service itself) end up running in a destroyed netns
	// — they look running to Docker but have no interface, routes, or
	// DNAT. anchord knows exactly which deps it's about to orphan
	// because it knows the old SA's container ID at the moment of
	// removal, so the scope is tight and the race-with-operator
	// surface is essentially nil (anchord caused the orphan, anchord
	// fixes it inline).
	//
	// Set to false to keep v1.1.0 behaviour (detection-only via the
	// dependents watcher's WARN log; operator runs the recovery
	// command manually).
	//
	// Requires `DELETE=1` AND container create/start endpoints on
	// docker-socket-proxy (same as AutostartSiblings, F-45's existing
	// recreate path needs these too).
	AutoFixDeadNetns bool

	// LabelSelector is the operator-defined set of labels a container
	// must carry (AND-joined) to be considered a backend candidate.
	// When non-empty it REPLACES the legacy project-label filter
	// (com.docker.compose.project=ComposeProject); when empty the
	// legacy filter is used unchanged.
	//
	// Two motivations (SPEC F-42):
	//   1. Multiple network-anchors in the same compose project, each
	//      scoped to a disjoint slice of containers (per-team, per-
	//      service-flavour).
	//   2. Backend containers spawned outside Compose (e.g. authentik
	//      outposts) that carry no com.docker.compose.project label
	//      and are therefore invisible to the legacy filter.
	LabelSelector map[string]string

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
	// network-anchor's transit IP. Default "anchord". Ignored when
	// GatewayIPs is non-empty.
	GatewayHostname string

	// GatewayIPs is the explicit gateway address list (one v4 and/or
	// one v6, comma-separated in ANCHORD_GATEWAY_IP). When non-empty,
	// the service-anchor skips DNS resolution and routes directly to
	// these addresses. Needed for wrap-pattern deployments where the
	// service-anchor lives in a container belonging to a different
	// Compose project than the network-anchor, so the target's Docker
	// DNS can't see the wrap-stack's `anchord` service (SPEC F-40).
	GatewayIPs []net.IP

	// ResolveInterval is how often the service-anchor mode re-resolves
	// the gateway hostname and reconciles its default route. Has no
	// effect when GatewayIPs is set (IP mode is static).
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
		ExtNetwork:     os.Getenv("ANCHORD_EXT_NETWORK"),
		SharedNetwork:  os.Getenv("ANCHORD_SHARED_NETWORK"),
		DHCPHostname:   os.Getenv("ANCHORD_DHCP_HOSTNAME"),
		DockerHost:     getenvDefault("DOCKER_HOST", "unix:///var/run/docker.sock"),
		LogLevel:       getenvDefault("ANCHORD_LOG_LEVEL", "info"),
		MetricsAddr:    metricsAddrFromEnv(),
	}

	if c.ComposeProject == "" {
		// Fall back to the env compose itself injects.
		c.ComposeProject = os.Getenv("COMPOSE_PROJECT_NAME")
	}

	// F-42 label selector: parsed up-front so we can decide whether
	// ANCHORD_PROJECT is required (legacy path) or optional (selector
	// path).
	selector, err := parseLabelSelector(os.Getenv("ANCHORD_LABEL_SELECTOR"))
	if err != nil {
		return nil, err
	}
	c.LabelSelector = selector

	// F-43 sibling auto-start: default on. Operator opts out
	// explicitly via "false"/"0"/"no".
	autostart, err := parseBoolDefault("ANCHORD_AUTOSTART_SIBLINGS", true)
	if err != nil {
		return nil, err
	}
	c.AutostartSiblings = autostart

	// Issue #10: dead-netns dependent auto-fix on SA recreate.
	// Default on. Operator opts out for the v1.1.0 detection-only
	// behaviour, or to keep narrower docker-socket-proxy permissions.
	autofix, err := parseBoolDefault("ANCHORD_AUTOFIX_DEAD_NETNS", true)
	if err != nil {
		return nil, err
	}
	c.AutoFixDeadNetns = autofix

	// F-45 managed service-anchor recipe — opt-in via TARGET. All
	// other fields default to "fill in at runtime" if unset.
	managedSA, err := parseManagedSARecipe()
	if err != nil {
		return nil, err
	}
	c.ManagedSA = managedSA

	// Issue #6: optional pin for the external default-route gateway.
	// Empty means "resolve at runtime from Docker NetworkInspect on
	// ExtNetwork". Uses the same comma-separated v4,v6 grammar as
	// the service-anchor's ANCHORD_GATEWAY_IP.
	extGws, err := parseGatewayIPs(os.Getenv("ANCHORD_EXT_GATEWAY_IP"))
	if err != nil {
		return nil, fmt.Errorf("ANCHORD_EXT_GATEWAY_IP: %w", err)
	}
	c.ExtGatewayIPs = extGws

	if c.ComposeProject == "" && len(c.LabelSelector) == 0 {
		return nil, fmt.Errorf("ANCHORD_PROJECT (or COMPOSE_PROJECT_NAME) must be set unless ANCHORD_LABEL_SELECTOR is")
	}
	if c.DHCPHostname == "" {
		// Reasonable default: project name if we have one, else first
		// selector value (deterministic across restarts of the same
		// selector config; a single-value selector is the common
		// per-anchor-flavour case from F-42).
		if c.ComposeProject != "" {
			c.DHCPHostname = c.ComposeProject
		} else {
			c.DHCPHostname = firstSelectorValue(c.LabelSelector)
		}
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
	ips, err := parseGatewayIPs(os.Getenv("ANCHORD_GATEWAY_IP"))
	if err != nil {
		return nil, err
	}
	c.GatewayIPs = ips
	c.ResolveInterval, err = parseDuration("ANCHORD_GATEWAY_RESOLVE_INTERVAL", 5*time.Second)
	if err != nil {
		return nil, err
	}
	if c.ResolveInterval <= 0 {
		return nil, fmt.Errorf("ANCHORD_GATEWAY_RESOLVE_INTERVAL must be positive")
	}
	return c, nil
}

// parseGatewayIPs reads ANCHORD_GATEWAY_IP as a comma-separated list
// of at most one IPv4 and one IPv6 address. Empty input returns
// (nil, nil) so the caller falls back to DNS-based resolution.
//
// Malformed addresses are fatal — silently dropping a typo into the
// DNS fallback would mask the misconfiguration; F-40 acceptance
// requires loud failure.
func parseGatewayIPs(raw string) ([]net.IP, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []net.IP
	var sawV4, sawV6 bool
	for _, part := range strings.Split(raw, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("ANCHORD_GATEWAY_IP=%q is not a valid IP", s)
		}
		if ip.To4() != nil {
			if sawV4 {
				return nil, fmt.Errorf("ANCHORD_GATEWAY_IP lists more than one IPv4 address")
			}
			sawV4 = true
			out = append(out, ip.To4())
		} else {
			if sawV6 {
				return nil, fmt.Errorf("ANCHORD_GATEWAY_IP lists more than one IPv6 address")
			}
			sawV6 = true
			out = append(out, ip)
		}
	}
	return out, nil
}

// parseManagedSARecipe loads the F-45 service-anchor recipe from
// env. Returns the zero value when ANCHORD_MANAGED_SA_TARGET is
// unset — recipe.Active() returns false and the autostart watcher
// falls back to pure F-43 behaviour.
//
// Defaults filled here (i.e. statically derivable):
//   - Name = "<Target>-service-anchor"
//
// Runtime-resolved defaults (Image, GatewayIP) are left empty here;
// the autostart watcher fills them by inspecting its own container
// after the shared-network picker has settled.
//
// Validation:
//   - Empty Target → entire recipe inactive (no error).
//   - ANCHORD_MANAGED_SA_EXTRA_ENV must be valid JSON (object of
//     string→string). Anything else is a fatal startup error.
//   - Operator-specified Image / GatewayIP are not validated here;
//     Docker / the service-anchor itself will surface failures.
func parseManagedSARecipe() (ManagedSARecipe, error) {
	target := strings.TrimSpace(os.Getenv("ANCHORD_MANAGED_SA_TARGET"))
	if target == "" {
		return ManagedSARecipe{}, nil
	}
	name := strings.TrimSpace(os.Getenv("ANCHORD_MANAGED_SA_NAME"))
	if name == "" {
		name = target + "-service-anchor"
	}
	extra, err := parseJSONStringMap(os.Getenv("ANCHORD_MANAGED_SA_EXTRA_ENV"), "ANCHORD_MANAGED_SA_EXTRA_ENV")
	if err != nil {
		return ManagedSARecipe{}, err
	}
	labels, err := parseJSONStringMap(os.Getenv("ANCHORD_MANAGED_SA_LABELS"), "ANCHORD_MANAGED_SA_LABELS")
	if err != nil {
		return ManagedSARecipe{}, err
	}
	for k := range labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			return ManagedSARecipe{}, fmt.Errorf("ANCHORD_MANAGED_SA_LABELS: %q is reserved (compose.* labels stamped without compose.service crash orchestrators — see issue #2)", k)
		}
		if k == "anchord.managed-by" {
			return ManagedSARecipe{}, fmt.Errorf("ANCHORD_MANAGED_SA_LABELS: %q is reserved (built-in bookkeeping label)", k)
		}
	}
	return ManagedSARecipe{
		Target:    target,
		Name:      name,
		Image:     strings.TrimSpace(os.Getenv("ANCHORD_MANAGED_SA_IMAGE")),
		GatewayIP: strings.TrimSpace(os.Getenv("ANCHORD_MANAGED_SA_GATEWAY_IP")),
		ExtraEnv:  extra,
		Labels:    labels,
	}, nil
}

// parseJSONStringMap decodes a JSON object of string→string into a
// map. Empty input returns an empty map (not nil) so callers can
// range over it without a nil check. envName is used in error
// messages so the operator knows which variable they fat-fingered.
func parseJSONStringMap(raw, envName string) (map[string]string, error) {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("invalid %s: must be a JSON object of string->string, got %v", envName, err)
	}
	return out, nil
}

// parseLabelSelector turns the ANCHORD_LABEL_SELECTOR env value into
// a map[string]string for use as a Docker label filter.
//
// Format (F-42): comma-separated key=value pairs, whitespace tolerated
// around commas and around `=`. Equality only — no `!=`, no set
// membership, no wildcards.
//
// Errors are fatal because they signal misconfiguration that would
// otherwise produce a silently-wrong discovery scope:
//   - entry without `=` → `"label selector entry %q missing '='"`
//   - same key with two different values → conflict message naming
//     the offending key and both values
//
// Empty value (`key=`) is *valid* and matches containers carrying the
// label `key` with the literal empty string. No "any value" wildcard;
// the spec defers that to a hypothetical F-43.
func parseLabelSelector(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("label selector entry %q missing '='", trimmed)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			return nil, fmt.Errorf("label selector entry %q has empty key", trimmed)
		}
		if existing, dup := out[k]; dup && existing != v {
			return nil, fmt.Errorf("label selector has duplicate key %q with conflicting values %q and %q", k, existing, v)
		}
		out[k] = v
	}
	return out, nil
}

// parseBoolDefault reads an env var with strconv.ParseBool semantics
// (accepts 1/0, t/f, true/false, TRUE/FALSE, etc.), falling back to
// `def` when the env var is unset or empty-string. Malformed values
// are rejected — silently defaulting on typos would mask
// misconfiguration of a security-relevant feature flag.
func parseBoolDefault(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s=%q: must be true/false (or 1/0)", key, raw)
	}
	return v, nil
}

// firstSelectorValue returns one selector value, deterministically
// (keys sorted) so the chosen DHCP hostname is stable across process
// restarts when no ANCHORD_DHCP_HOSTNAME is supplied. Returns "" for
// an empty selector — the caller guards against that.
func firstSelectorValue(sel map[string]string) string {
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return sel[keys[0]]
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
