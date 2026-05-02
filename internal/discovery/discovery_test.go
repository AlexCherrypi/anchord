package discovery

import (
	"net"
	"testing"

	"github.com/AlexCherrypi/anchord/internal/labels"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

func TestRuleLess(t *testing.T) {
	cases := []struct {
		a, b labels.Rule
		less bool
	}{
		{labels.Rule{Proto: "tcp", Port: 25}, labels.Rule{Proto: "tcp", Port: 80}, true},
		{labels.Rule{Proto: "tcp", Port: 80}, labels.Rule{Proto: "tcp", Port: 25}, false},
		{labels.Rule{Proto: "tcp", Port: 80}, labels.Rule{Proto: "udp", Port: 25}, true},
		{labels.Rule{Proto: "udp", Port: 25}, labels.Rule{Proto: "tcp", Port: 80}, false},
		{labels.Rule{Proto: "tcp", Port: 25}, labels.Rule{Proto: "tcp", Port: 25}, false},
	}
	for _, tc := range cases {
		if got := ruleLess(tc.a, tc.b); got != tc.less {
			t.Errorf("ruleLess(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.less)
		}
	}
}

func TestBackendEqual(t *testing.T) {
	base := Backend{
		IPv4: net.ParseIP("10.0.0.5"),
		IPv6: net.ParseIP("fd00::5"),
		Spec: labels.Spec{
			V6:    labels.V6Auto,
			Rules: []labels.Rule{{Proto: "tcp", Port: 25}, {Proto: "tcp", Port: 80}},
		},
	}
	t.Run("identical", func(t *testing.T) {
		if !backendEqual(base, base) {
			t.Error("identical backends should be equal")
		}
	})
	t.Run("different IPv4", func(t *testing.T) {
		b := base
		b.IPv4 = net.ParseIP("10.0.0.99")
		if backendEqual(base, b) {
			t.Error("differing IPv4 should not be equal")
		}
	})
	t.Run("different IPv6", func(t *testing.T) {
		b := base
		b.IPv6 = net.ParseIP("fd00::99")
		if backendEqual(base, b) {
			t.Error("differing IPv6 should not be equal")
		}
	})
	t.Run("V6 mode differs", func(t *testing.T) {
		b := base
		b.Spec = labels.Spec{V6: labels.V6Off, Rules: base.Spec.Rules}
		if backendEqual(base, b) {
			t.Error("differing V6 mode should not be equal")
		}
	})
	t.Run("rules order swapped", func(t *testing.T) {
		b := base
		b.Spec = labels.Spec{
			V6:    labels.V6Auto,
			Rules: []labels.Rule{{Proto: "tcp", Port: 80}, {Proto: "tcp", Port: 25}},
		}
		if !backendEqual(base, b) {
			t.Error("rules in different order should still be equal (sorted compare)")
		}
	})
	t.Run("rules differ", func(t *testing.T) {
		b := base
		b.Spec = labels.Spec{
			V6:    labels.V6Auto,
			Rules: []labels.Rule{{Proto: "tcp", Port: 25}, {Proto: "udp", Port: 4500}},
		}
		if backendEqual(base, b) {
			t.Error("different rule sets should not be equal")
		}
	})
	t.Run("rules different lengths", func(t *testing.T) {
		b := base
		b.Spec = labels.Spec{
			V6:    labels.V6Auto,
			Rules: []labels.Rule{{Proto: "tcp", Port: 25}},
		}
		if backendEqual(base, b) {
			t.Error("different rule counts should not be equal")
		}
	})
}

func TestStateEqual(t *testing.T) {
	mk := func(id string, ip4 string) Backend {
		return Backend{
			ID:   id,
			IPv4: net.ParseIP(ip4),
			Spec: labels.Spec{
				V6:    labels.V6Auto,
				Rules: []labels.Rule{{Proto: "tcp", Port: 25}},
			},
		}
	}
	a := State{Backends: map[string]Backend{
		"a": mk("a", "10.0.0.1"),
		"b": mk("b", "10.0.0.2"),
	}}
	b := State{Backends: map[string]Backend{
		"a": mk("a", "10.0.0.1"),
		"b": mk("b", "10.0.0.2"),
	}}
	if !a.Equal(b) {
		t.Error("identical states should be equal")
	}
	smaller := State{Backends: map[string]Backend{
		"a": mk("a", "10.0.0.1"),
	}}
	if a.Equal(smaller) {
		t.Error("different sizes should not be equal")
	}
	mutated := State{Backends: map[string]Backend{
		"a": mk("a", "10.0.0.1"),
		"b": mk("b", "10.0.0.99"),
	}}
	if a.Equal(mutated) {
		t.Error("changed backend IP should not be equal")
	}
	swappedKey := State{Backends: map[string]Backend{
		"a": mk("a", "10.0.0.1"),
		"c": mk("c", "10.0.0.2"),
	}}
	if a.Equal(swappedKey) {
		t.Error("different backend IDs should not be equal")
	}
	bothEmpty := State{Backends: map[string]Backend{}}
	emptyAlt := State{Backends: map[string]Backend{}}
	if !bothEmpty.Equal(emptyAlt) {
		t.Error("two empty states should be equal")
	}
}

func TestPickIPs_NilNetworkSettings(t *testing.T) {
	c := container.Summary{}
	v4, v6 := pickIPs(c, "transit")
	if v4 != nil || v6 != nil {
		t.Errorf("expected nil/nil, got %v/%v", v4, v6)
	}
}

func TestPickIPs_SharedNetworkExplicit(t *testing.T) {
	c := container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"backend": {IPAddress: "10.10.0.5"},
				"transit": {IPAddress: "10.20.0.5", GlobalIPv6Address: "fd00::5"},
			},
		},
	}
	v4, v6 := pickIPs(c, "transit")
	if !v4.Equal(net.ParseIP("10.20.0.5")) {
		t.Errorf("v4: got %s want 10.20.0.5", v4)
	}
	if !v6.Equal(net.ParseIP("fd00::5")) {
		t.Errorf("v6: got %s want fd00::5", v6)
	}
}

func TestPickIPs_SharedNetworkAbsentReturnsNil(t *testing.T) {
	c := container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"backend": {IPAddress: "10.10.0.5"},
			},
		},
	}
	v4, v6 := pickIPs(c, "transit")
	if v4 != nil || v6 != nil {
		t.Errorf("expected nil/nil when shared net absent, got %v/%v", v4, v6)
	}
}

func TestPickIPs_NoSharedFallsBackToFirst(t *testing.T) {
	// Single-entry map so iteration order is deterministic.
	c := container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"only": {IPAddress: "10.1.2.3", GlobalIPv6Address: "fd00::1"},
			},
		},
	}
	v4, v6 := pickIPs(c, "")
	if !v4.Equal(net.ParseIP("10.1.2.3")) {
		t.Errorf("v4: got %s", v4)
	}
	if !v6.Equal(net.ParseIP("fd00::1")) {
		t.Errorf("v6: got %s", v6)
	}
}

func TestPickIPs_V6Only(t *testing.T) {
	c := container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"transit": {IPAddress: "", GlobalIPv6Address: "fd00::5"},
			},
		},
	}
	v4, v6 := pickIPs(c, "transit")
	if v4 != nil {
		t.Errorf("v4 should be nil with empty IPAddress, got %s", v4)
	}
	if !v6.Equal(net.ParseIP("fd00::5")) {
		t.Errorf("v6: got %s", v6)
	}
}

func TestPickIPs_V4Only(t *testing.T) {
	c := container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"transit": {IPAddress: "10.0.0.5", GlobalIPv6Address: ""},
			},
		},
	}
	v4, v6 := pickIPs(c, "transit")
	if !v4.Equal(net.ParseIP("10.0.0.5")) {
		t.Errorf("v4: got %s", v4)
	}
	if v6 != nil {
		t.Errorf("v6 should be nil with empty GlobalIPv6Address, got %s", v6)
	}
}

func TestTrimName(t *testing.T) {
	if got := trimName(nil); got != "" {
		t.Errorf("nil names: %q", got)
	}
	if got := trimName([]string{}); got != "" {
		t.Errorf("empty slice: %q", got)
	}
	if got := trimName([]string{"/foo"}); got != "foo" {
		t.Errorf("got %q", got)
	}
	if got := trimName([]string{"bar"}); got != "bar" {
		t.Errorf("got %q", got)
	}
}

func TestParseIP(t *testing.T) {
	if parseIP("") != nil {
		t.Error("empty string should be nil")
	}
	if parseIP("not-an-ip") != nil {
		t.Error("garbage should be nil")
	}
	if ip := parseIP("10.0.0.5"); ip == nil || !ip.Equal(net.ParseIP("10.0.0.5")) {
		t.Errorf("10.0.0.5 round-trip: got %v", ip)
	}
	if ip := parseIP("fd00::5"); ip == nil || !ip.Equal(net.ParseIP("fd00::5")) {
		t.Errorf("fd00::5 round-trip: got %v", ip)
	}
}
