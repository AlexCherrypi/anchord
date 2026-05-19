package reconciler

import (
	"net"
	"testing"

	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/labels"
	"github.com/AlexCherrypi/anchord/internal/nat"
)

// r builds a Rule without port translation (BackendPort == Port) —
// keeps the pre-F-46 test fixtures compact.
func r(proto string, port uint16) labels.Rule {
	return labels.Rule{Proto: proto, Port: port, BackendPort: port}
}

// xr builds a port-translating Rule (DMZ port -> backend port).
func xr(proto string, dmzPort, backendPort uint16) labels.Rule {
	return labels.Rule{Proto: proto, Port: dmzPort, BackendPort: backendPort}
}

func TestDesiredFromState_Empty(t *testing.T) {
	got := desiredFromState(discovery.State{})
	if len(got) != 0 {
		t.Errorf("empty state should produce empty desired, got %d entries", len(got))
	}
}

func TestDesiredFromState_DualStack(t *testing.T) {
	st := discovery.State{Backends: map[string]discovery.Backend{
		"smtp": {
			IPv4: net.ParseIP("10.0.0.5"),
			IPv6: net.ParseIP("fd00::5"),
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{r("tcp", 25), r("tcp", 587)},
			},
		},
	}}
	got := desiredFromState(st)
	want := map[key]nat.Target{
		{nat.V4, "tcp", 25}:  {IP: net.ParseIP("10.0.0.5"), Port: 25},
		{nat.V4, "tcp", 587}: {IP: net.ParseIP("10.0.0.5"), Port: 587},
		{nat.V6, "tcp", 25}:  {IP: net.ParseIP("fd00::5"), Port: 25},
		{nat.V6, "tcp", 587}: {IP: net.ParseIP("fd00::5"), Port: 587},
	}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d", len(got), len(want))
	}
	for k, w := range want {
		if !got[k].IP.Equal(w.IP) || got[k].Port != w.Port {
			t.Errorf("entry %v: got %+v want %+v", k, got[k], w)
		}
	}
}

// V6Off mirrors the SPEC: a container with anchord.expose.v6=off keeps
// only its v4 mapping even when an IPv6 address is present.
func TestDesiredFromState_V6Off(t *testing.T) {
	st := discovery.State{Backends: map[string]discovery.Backend{
		"smtp": {
			IPv4: net.ParseIP("10.0.0.5"),
			IPv6: net.ParseIP("fd00::5"),
			Spec: labels.Spec{
				V6:    labels.V6Off,
				Rules: []labels.Rule{r("tcp", 443)},
			},
		},
	}}
	got := desiredFromState(st)
	if len(got) != 1 {
		t.Fatalf("V6Off should yield 1 entry, got %d", len(got))
	}
	if _, ok := got[key{nat.V4, "tcp", 443}]; !ok {
		t.Error("missing v4 entry")
	}
	if _, ok := got[key{nat.V6, "tcp", 443}]; ok {
		t.Error("v6 entry should be absent when V6Off")
	}
}

// IPv4-only DHCP scenario: backend has no IPv6 address.
func TestDesiredFromState_V4OnlyBackend(t *testing.T) {
	st := discovery.State{Backends: map[string]discovery.Backend{
		"smtp": {
			IPv4: net.ParseIP("10.0.0.5"),
			IPv6: nil,
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{r("tcp", 25)},
			},
		},
	}}
	got := desiredFromState(st)
	if len(got) != 1 {
		t.Fatalf("v4-only backend should yield 1 entry, got %d", len(got))
	}
	if _, ok := got[key{nat.V4, "tcp", 25}]; !ok {
		t.Error("missing v4 entry")
	}
}

// IPv6-only DHCP scenario: backend has no IPv4 address.
func TestDesiredFromState_V6OnlyBackend(t *testing.T) {
	st := discovery.State{Backends: map[string]discovery.Backend{
		"smtp": {
			IPv4: nil,
			IPv6: net.ParseIP("fd00::5"),
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{r("tcp", 25)},
			},
		},
	}}
	got := desiredFromState(st)
	if len(got) != 1 {
		t.Fatalf("v6-only backend should yield 1 entry, got %d", len(got))
	}
	if _, ok := got[key{nat.V6, "tcp", 25}]; !ok {
		t.Error("missing v6 entry")
	}
}

func TestDesiredFromState_MultipleBackendsAndProtocols(t *testing.T) {
	st := discovery.State{Backends: map[string]discovery.Backend{
		"smtp": {
			IPv4: net.ParseIP("10.0.0.5"),
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{r("tcp", 25)},
			},
		},
		"vpn": {
			IPv4: net.ParseIP("10.0.0.6"),
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{r("tcp", 143), r("udp", 4500)},
			},
		},
	}}
	got := desiredFromState(st)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d (%v)", len(got), got)
	}
	if !got[key{nat.V4, "tcp", 25}].IP.Equal(net.ParseIP("10.0.0.5")) {
		t.Errorf("smtp tcp/25: %v", got[key{nat.V4, "tcp", 25}])
	}
	if !got[key{nat.V4, "tcp", 143}].IP.Equal(net.ParseIP("10.0.0.6")) {
		t.Errorf("vpn tcp/143: %v", got[key{nat.V4, "tcp", 143}])
	}
	if !got[key{nat.V4, "udp", 4500}].IP.Equal(net.ParseIP("10.0.0.6")) {
		t.Errorf("vpn udp/4500: %v", got[key{nat.V4, "udp", 4500}])
	}
}

// SPEC F-8: each (proto, port) tuple must map to exactly one container.
// desiredFromState is permissive — it would let a later iteration win —
// but we lock in the current "one backend per port" expectation by
// asserting that two backends claiming the same port produce a single
// entry, not two. (The startup-error behavior for collisions, when it
// lands, will live one layer up.)
func TestDesiredFromState_SamePortFromTwoBackends(t *testing.T) {
	st := discovery.State{Backends: map[string]discovery.Backend{
		"a": {
			IPv4: net.ParseIP("10.0.0.5"),
			Spec: labels.Spec{V6: labels.V6Auto, Rules: []labels.Rule{r("tcp", 25)}},
		},
		"b": {
			IPv4: net.ParseIP("10.0.0.6"),
			Spec: labels.Spec{V6: labels.V6Auto, Rules: []labels.Rule{r("tcp", 25)}},
		},
	}}
	got := desiredFromState(st)
	if len(got) != 1 {
		t.Fatalf("collision should still produce a single entry (not duplicate), got %d", len(got))
	}
}

// F-46: a port-translating expose label ("tcp/636:6636") populates
// the Target.Port with the backend-side port, while the map key
// stays the DMZ-side port. The reconciler does no port-rewriting
// itself — it just passes BackendPort through to nat.SetMap, which
// puts it in the nft map's tuple value.
func TestDesiredFromState_F46PortTranslation(t *testing.T) {
	st := discovery.State{Backends: map[string]discovery.Backend{
		"ldap-outpost": {
			IPv4: net.ParseIP("172.31.80.9"),
			IPv6: net.ParseIP("fd31:80::9"),
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{xr("tcp", 636, 6636)},
			},
		},
	}}
	got := desiredFromState(st)

	// Both families produce an entry keyed by the DMZ port (636).
	v4tgt, ok := got[key{nat.V4, "tcp", 636}]
	if !ok {
		t.Fatal("v4 entry missing for DMZ port 636")
	}
	if v4tgt.Port != 6636 {
		t.Errorf("v4 target port: got %d want 6636 (the backend-side port)", v4tgt.Port)
	}
	if !v4tgt.IP.Equal(net.ParseIP("172.31.80.9")) {
		t.Errorf("v4 target IP: got %v", v4tgt.IP)
	}
	v6tgt, ok := got[key{nat.V6, "tcp", 636}]
	if !ok {
		t.Fatal("v6 entry missing for DMZ port 636")
	}
	if v6tgt.Port != 6636 {
		t.Errorf("v6 target port: got %d want 6636", v6tgt.Port)
	}

	// The DMZ-side port 636 is NOT also present as a key with port 636
	// on the target side; the translation flows only through the
	// tuple value, not via a second map key.
	if _, ok := got[key{nat.V4, "tcp", 6636}]; ok {
		t.Error("backend-side port 6636 must not appear as a map key")
	}
}
