// Package labels parses the anchord.expose container labels.
//
// Label format on a service-anchor container:
//
//	labels:
//	  anchord.expose:    "tcp/25,tcp/465,udp/4500"
//	  anchord.expose.v6: "auto"   # default; "off" to disable v6
//
// F-46 port-translating DNAT: each entry may carry an optional
// backend-port suffix to express "DMZ-side port 636 maps to the
// backend's port 6636". Syntax: <proto>/<dmz_port>[:<backend_port>].
// When the suffix is omitted, BackendPort defaults to Port — same
// behaviour as before F-46.
//
//	labels:
//	  anchord.expose: "tcp/636:6636"     # DMZ 636 -> backend 6636
//	  anchord.expose: "tcp/443,tcp/636:6636"   # mixed entries fine
package labels

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	LabelExpose   = "anchord.expose"
	LabelExposeV6 = "anchord.expose.v6"
)

// Rule is one port/proto exposure entry.
//
// F-46: Port is the DMZ-side listen port (what clients connect to);
// BackendPort is the port on the backend container the connection is
// DNAT'd to. When the operator writes "tcp/636" without a translation
// suffix, BackendPort equals Port. When they write "tcp/636:6636",
// Port=636 and BackendPort=6636.
type Rule struct {
	Proto       string // "tcp" or "udp"
	Port        uint16 // DMZ-side port (what clients connect to)
	BackendPort uint16 // backend-side port (where the DNAT lands)
}

// Translates reports whether this rule changes the destination port
// during DNAT. False for the common case where Port==BackendPort.
func (r Rule) Translates() bool { return r.Port != r.BackendPort }

// V6Mode controls IPv6 exposure for a container.
type V6Mode int

const (
	V6Auto V6Mode = iota // mirror v4 rules onto AAAA address (default)
	V6Off                // skip v6 entirely for this container
)

// Spec is the parsed exposure intent for one container.
type Spec struct {
	Rules []Rule
	V6    V6Mode
}

// Parse extracts a Spec from a container's labels. Returns (nil, nil)
// if the container has no anchord.expose label — that's not an error,
// it just means anchord shouldn't touch it.
func Parse(lbl map[string]string) (*Spec, error) {
	raw, ok := lbl[LabelExpose]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	spec := &Spec{V6: V6Auto}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r, err := parseRule(part)
		if err != nil {
			return nil, fmt.Errorf("anchord.expose %q: %w", part, err)
		}
		spec.Rules = append(spec.Rules, r)
	}
	if len(spec.Rules) == 0 {
		return nil, fmt.Errorf("anchord.expose set but empty")
	}

	switch strings.ToLower(strings.TrimSpace(lbl[LabelExposeV6])) {
	case "", "auto":
		spec.V6 = V6Auto
	case "off", "false", "no", "0":
		spec.V6 = V6Off
	default:
		return nil, fmt.Errorf("anchord.expose.v6 must be auto|off")
	}

	return spec, nil
}

func parseRule(s string) (Rule, error) {
	proto, rest, ok := strings.Cut(s, "/")
	if !ok {
		return Rule{}, fmt.Errorf("expected proto/port (e.g. tcp/25)")
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	switch proto {
	case "tcp", "udp":
	default:
		return Rule{}, fmt.Errorf("unsupported proto %q (tcp|udp)", proto)
	}

	// F-46: port spec is now "<dmz_port>[:<backend_port>]". The
	// optional second port translates the destination on DNAT.
	dmzStr, backendStr, hasBackend := strings.Cut(rest, ":")
	dmzPort, err := parsePort(dmzStr)
	if err != nil {
		return Rule{}, fmt.Errorf("invalid dmz port %q: %w", dmzStr, err)
	}
	backendPort := dmzPort
	if hasBackend {
		bp, err := parsePort(backendStr)
		if err != nil {
			return Rule{}, fmt.Errorf("invalid backend port %q: %w", backendStr, err)
		}
		backendPort = bp
	}
	return Rule{Proto: proto, Port: dmzPort, BackendPort: backendPort}, nil
}

// parsePort accepts "1".."65535" with surrounding whitespace and
// rejects 0, empty, non-numeric, and out-of-range values with
// distinct error messages so the caller can wrap them with context.
func parsePort(s string) (uint16, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty port")
	}
	port, err := strconv.ParseUint(trimmed, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("not a 16-bit integer")
	}
	if port == 0 {
		return 0, fmt.Errorf("port 0 is reserved")
	}
	return uint16(port), nil
}
