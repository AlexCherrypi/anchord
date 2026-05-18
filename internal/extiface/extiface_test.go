package extiface

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// mac returns a net.HardwareAddr from a string for terser test setup.
func mac(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return m
}

// fakeInspect returns a canned NetworkMACs and optional error.
func fakeInspect(nets NetworkMACs, err error) inspector {
	return func(_ context.Context, _ string) (NetworkMACs, error) {
		if err != nil {
			return nil, err
		}
		return nets, nil
	}
}

// macTableResolve returns a linkResolver backed by a static
// macStr->iface map.
func macTableResolve(table map[string]string) linkResolver {
	return func(m net.HardwareAddr) (string, error) {
		if iface, ok := table[m.String()]; ok {
			return iface, nil
		}
		return "", ErrMACNotFound
	}
}

// shortDeadline keeps tests fast — production default is 10s.
func shortDeadline() time.Duration { return 200 * time.Millisecond }

func TestResolve_Success(t *testing.T) {
	r := &Resolver{
		inspect: fakeInspect(NetworkMACs{
			"dmz_macvlan": "02:4c:4b:50:0a:01",
			"transit":     "02:42:ac:1e:00:02",
		}, nil),
		resolve: macTableResolve(map[string]string{
			"02:4c:4b:50:0a:01": "eth1",
			"02:42:ac:1e:00:02": "eth0",
		}),
		selfHost: "deadbeef",
		deadline: shortDeadline(),
	}

	iface, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if iface != "eth1" {
		t.Errorf("got iface=%q, want eth1", iface)
	}
}

// TestResolve_PicksByMACNotIfaceName is the heart of F-37: even if
// Docker assigned the dmz network to eth1 this time (non-deterministic
// across recreates), the resolver must follow the MAC, not the name.
// Same inputs as TestResolve_Success but the dmz MAC is on eth0 — the
// resolver should return eth0, not "eth1 because dmz is the first
// network listed".
func TestResolve_PicksByMACNotIfaceName(t *testing.T) {
	r := &Resolver{
		inspect: fakeInspect(NetworkMACs{
			"dmz_macvlan": "02:4c:4b:50:0a:01",
			"transit":     "02:42:ac:1e:00:02",
		}, nil),
		resolve: macTableResolve(map[string]string{
			"02:4c:4b:50:0a:01": "eth0", // <- the swap
			"02:42:ac:1e:00:02": "eth1",
		}),
		selfHost: "deadbeef",
		deadline: shortDeadline(),
	}

	iface, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if iface != "eth0" {
		t.Errorf("got iface=%q, want eth0 (followed MAC, not first-network heuristic)", iface)
	}
}

func TestResolve_EmptyNetworkName(t *testing.T) {
	r := &Resolver{deadline: shortDeadline()}
	_, err := r.Resolve(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty network name")
	}
}

func TestResolve_NetworkAbsent_Fatal(t *testing.T) {
	r := &Resolver{
		inspect: fakeInspect(NetworkMACs{
			"transit": "02:42:ac:1e:00:02",
		}, nil),
		resolve:  macTableResolve(nil),
		selfHost: "deadbeef",
		deadline: shortDeadline(),
	}

	_, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNetworkNotFound) {
		t.Errorf("expected ErrNetworkNotFound, got: %v", err)
	}
}

// TestResolve_NetworkAttachedLate covers the compose-up race: the
// network is initially missing from the inspect response but appears
// before the deadline expires. Resolver should retry and succeed.
func TestResolve_NetworkAttachedLate(t *testing.T) {
	attempts := 0
	r := &Resolver{
		inspect: func(_ context.Context, _ string) (NetworkMACs, error) {
			attempts++
			if attempts < 3 {
				return NetworkMACs{}, nil // empty, simulating not-yet-attached
			}
			return NetworkMACs{"dmz_macvlan": "02:4c:4b:50:0a:01"}, nil
		},
		resolve:  macTableResolve(map[string]string{"02:4c:4b:50:0a:01": "eth0"}),
		selfHost: "deadbeef",
		deadline: 5 * time.Second, // need enough room for backoff progression
	}

	iface, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if iface != "eth0" {
		t.Errorf("got %q want eth0", iface)
	}
	if attempts < 3 {
		t.Errorf("expected at least 3 attempts (retry path), got %d", attempts)
	}
}

func TestResolve_MACMissingOnHost_Fatal(t *testing.T) {
	r := &Resolver{
		inspect: fakeInspect(NetworkMACs{
			"dmz_macvlan": "02:4c:4b:50:0a:01",
		}, nil),
		resolve:  macTableResolve(map[string]string{}), // empty — no iface carries the MAC
		selfHost: "deadbeef",
		deadline: shortDeadline(),
	}

	_, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrMACNotFound) {
		t.Errorf("expected ErrMACNotFound, got: %v", err)
	}
}

func TestResolve_APIError_RetriesThenFails(t *testing.T) {
	apiErr := fmt.Errorf("connect: connection refused")
	r := &Resolver{
		inspect:  fakeInspect(nil, apiErr),
		resolve:  macTableResolve(nil),
		selfHost: "deadbeef",
		deadline: shortDeadline(),
	}

	_, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err == nil {
		t.Fatal("expected error")
	}
	// The wrapped error must mention "docker API unreachable" so an
	// operator skimming logs can tell auth/network problems apart from
	// configuration typos.
	if msg := err.Error(); !contains(msg, "docker API unreachable") {
		t.Errorf("error text should mention docker API, got: %s", msg)
	}
}

// TestResolve_EmptyMACTreatedAsNotYet covers the case where the
// network IS attached but Docker hasn't populated MacAddress yet
// (transient very-early-attach state). We retry rather than fail.
func TestResolve_EmptyMACTreatedAsNotYet(t *testing.T) {
	attempts := 0
	r := &Resolver{
		inspect: func(_ context.Context, _ string) (NetworkMACs, error) {
			attempts++
			if attempts < 2 {
				return NetworkMACs{"dmz_macvlan": ""}, nil
			}
			return NetworkMACs{"dmz_macvlan": "02:4c:4b:50:0a:01"}, nil
		},
		resolve:  macTableResolve(map[string]string{"02:4c:4b:50:0a:01": "eth0"}),
		selfHost: "deadbeef",
		deadline: 5 * time.Second,
	}
	iface, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if iface != "eth0" {
		t.Errorf("got %q want eth0", iface)
	}
}

// TestResolve_ContextCancelStopsRetry verifies cancellation propagates
// and that we don't burn the full deadline after the context dies.
func TestResolve_ContextCancelStopsRetry(t *testing.T) {
	r := &Resolver{
		inspect:  fakeInspect(NetworkMACs{}, nil), // always missing
		resolve:  macTableResolve(nil),
		selfHost: "deadbeef",
		deadline: 30 * time.Second, // never reached
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before first call

	start := time.Now()
	_, err := r.Resolve(ctx, "dmz_macvlan")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after cancelled ctx")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("resolve did not respect ctx cancel: ran %s", elapsed)
	}
}

// TestResolve_InvalidMACFormat — Docker shouldn't return an invalid
// MAC, but if it does we surface the parse error rather than
// silently retry forever.
func TestResolve_InvalidMACFormat(t *testing.T) {
	r := &Resolver{
		inspect:  fakeInspect(NetworkMACs{"dmz_macvlan": "not-a-mac"}, nil),
		resolve:  macTableResolve(nil),
		selfHost: "deadbeef",
		deadline: shortDeadline(),
	}
	_, err := r.Resolve(context.Background(), "dmz_macvlan")
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "parse MAC") {
		t.Errorf("expected parse-MAC error, got: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
