// Package labels parses the anchord.expose container labels.
//
// Label format on a service-anchor container:
//
//	labels:
//	  anchord.expose:    "tcp/25,tcp/465,udp/4500"
//	  anchord.expose.v6: "auto"   # default; "off" to disable v6
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
type Rule struct {
	Proto string // "tcp" or "udp"
	Port  uint16
}

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
	proto, portStr, ok := strings.Cut(s, "/")
	if !ok {
		return Rule{}, fmt.Errorf("expected proto/port (e.g. tcp/25)")
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	switch proto {
	case "tcp", "udp":
	default:
		return Rule{}, fmt.Errorf("unsupported proto %q (tcp|udp)", proto)
	}
	port, err := strconv.ParseUint(strings.TrimSpace(portStr), 10, 16)
	if err != nil || port == 0 {
		return Rule{}, fmt.Errorf("invalid port %q", portStr)
	}
	return Rule{Proto: proto, Port: uint16(port)}, nil
}
