package reconciler

import (
	"net"
	"testing"

	"github.com/AlexCherrypi/anchord/internal/discovery"
	"github.com/AlexCherrypi/anchord/internal/labels"
	"github.com/AlexCherrypi/anchord/internal/nat"
)

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
				Rules: []labels.Rule{{Proto: "tcp", Port: 25}, {Proto: "tcp", Port: 587}},
			},
		},
	}}
	got := desiredFromState(st)
	want := map[key]net.IP{
		{nat.V4, "tcp", 25}:  net.ParseIP("10.0.0.5"),
		{nat.V4, "tcp", 587}: net.ParseIP("10.0.0.5"),
		{nat.V6, "tcp", 25}:  net.ParseIP("fd00::5"),
		{nat.V6, "tcp", 587}: net.ParseIP("fd00::5"),
	}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d", len(got), len(want))
	}
	for k, ip := range want {
		if !got[k].Equal(ip) {
			t.Errorf("entry %v: got %v want %v", k, got[k], ip)
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
				Rules: []labels.Rule{{Proto: "tcp", Port: 443}},
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
				Rules: []labels.Rule{{Proto: "tcp", Port: 25}},
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
				Rules: []labels.Rule{{Proto: "tcp", Port: 25}},
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
				Rules: []labels.Rule{{Proto: "tcp", Port: 25}},
			},
		},
		"vpn": {
			IPv4: net.ParseIP("10.0.0.6"),
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{{Proto: "tcp", Port: 143}, {Proto: "udp", Port: 4500}},
			},
		},
	}}
	got := desiredFromState(st)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d (%v)", len(got), got)
	}
	if !got[key{nat.V4, "tcp", 25}].Equal(net.ParseIP("10.0.0.5")) {
		t.Errorf("smtp tcp/25: %v", got[key{nat.V4, "tcp", 25}])
	}
	if !got[key{nat.V4, "tcp", 143}].Equal(net.ParseIP("10.0.0.6")) {
		t.Errorf("vpn tcp/143: %v", got[key{nat.V4, "tcp", 143}])
	}
	if !got[key{nat.V4, "udp", 4500}].Equal(net.ParseIP("10.0.0.6")) {
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
			Spec: labels.Spec{V6: labels.V6Auto, Rules: []labels.Rule{{Proto: "tcp", Port: 25}}},
		},
		"b": {
			IPv4: net.ParseIP("10.0.0.6"),
			Spec: labels.Spec{V6: labels.V6Auto, Rules: []labels.Rule{{Proto: "tcp", Port: 25}}},
		},
	}}
	got := desiredFromState(st)
	if len(got) != 1 {
		t.Fatalf("collision should still produce a single entry (not duplicate), got %d", len(got))
	}
}
