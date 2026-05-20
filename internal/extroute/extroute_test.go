package extroute

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeInspector returns canned gateways per network name.
type fakeInspector struct {
	mu       sync.Mutex
	v4       net.IP
	v6       net.IP
	err      error
	calls    int
	lastName string
}

func (f *fakeInspector) Gateways(_ context.Context, name string) (net.IP, net.IP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastName = name
	return f.v4, f.v6, f.err
}

// fakeRouter records ReplaceDefaultRoute calls and serves a scripted
// CurrentDefaultGateway for the cross-family assertions.
type fakeRouter struct {
	mu sync.Mutex

	currentV4 net.IP
	currentV6 net.IP
	listErr   error

	replaceErr   error
	replaceCalls []replaceCall
}

type replaceCall struct {
	family int
	gw     net.IP
}

func (r *fakeRouter) CurrentDefaultGateway(family int) (net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	if family == unix.AF_INET {
		return r.currentV4, nil
	}
	return r.currentV6, nil
}

func (r *fakeRouter) ReplaceDefaultRoute(family int, gw net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replaceErr != nil {
		return r.replaceErr
	}
	r.replaceCalls = append(r.replaceCalls, replaceCall{family: family, gw: gw})
	if family == unix.AF_INET {
		r.currentV4 = gw
	} else {
		r.currentV6 = gw
	}
	return nil
}

func (r *fakeRouter) replaces() []replaceCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]replaceCall, len(r.replaceCalls))
	copy(out, r.replaceCalls)
	return out
}

// waitForReplaces busy-waits up to `deadline` until rt.replaces() has
// at least `n` entries. Lets the loop ginish a tick cleanly before
// we assert. Returns the snapshot at the moment the condition held.
func waitForReplaces(rt *fakeRouter, n int, deadline time.Duration) []replaceCall {
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if calls := rt.replaces(); len(calls) >= n {
			return calls
		}
		time.Sleep(2 * time.Millisecond)
	}
	return rt.replaces()
}

// Prio 3 (IPAM fallback): no pin, no DHCP — IPAM gateway is taken
// as the default-route source. This is the bootstrap-mode path.
func TestRun_IPAMFallbackWhenNoPinNoDHCP(t *testing.T) {
	insp := &fakeInspector{v4: net.IPv4(192, 168, 150, 1)}
	rt := &fakeRouter{currentV4: net.IPv4(172, 30, 0, 1)}
	m := NewWithDeps("dmz_macvlan", nil, nil, nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	calls := waitForReplaces(rt, 1, 200*time.Millisecond)
	cancel()
	<-done

	if len(calls) < 1 || !calls[0].gw.Equal(net.IPv4(192, 168, 150, 1)) || calls[0].family != unix.AF_INET {
		t.Errorf("expected IPAM v4 gateway as fallback, got %+v", calls)
	}
}

// Prio 2 (pin > IPAM): when both are present, pin wins. Plus the
// inspect MUST be skipped to save the Docker round-trip when both
// pin slots are set.
func TestRun_PinWinsOverIPAM(t *testing.T) {
	insp := &fakeInspector{v4: net.IPv4(192, 168, 150, 1)}
	rt := &fakeRouter{currentV4: net.IPv4(172, 30, 0, 1)}
	m := NewWithDeps("dmz_macvlan",
		net.IPv4(10, 200, 0, 1), // pin v4
		net.ParseIP("fd00::1"),  // pin v6
		nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	calls := waitForReplaces(rt, 1, 200*time.Millisecond)
	cancel()
	<-done

	if insp.calls != 0 {
		t.Errorf("inspect must be skipped when both v4 and v6 pins are set, got %d", insp.calls)
	}
	if len(calls) < 1 || !calls[0].gw.Equal(net.IPv4(10, 200, 0, 1)) {
		t.Errorf("expected pin to win, got %+v", calls)
	}
}

// Pin v4 only + IPAM v6 only: each family takes its respective
// source. The mixed-source case is supported (unlike the previous
// either/or implementation).
func TestRun_PinV4_IPAMv6_MixedSource(t *testing.T) {
	insp := &fakeInspector{v6: net.ParseIP("fd96:0150::1")}
	rt := &fakeRouter{}
	m := NewWithDeps("dmz_macvlan",
		net.IPv4(192, 168, 150, 1), nil,
		nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	calls := waitForReplaces(rt, 2, 200*time.Millisecond)
	cancel()
	<-done

	var sawV4Pin, sawV6Ipam bool
	for _, c := range calls {
		if c.family == unix.AF_INET && c.gw.Equal(net.IPv4(192, 168, 150, 1)) {
			sawV4Pin = true
		}
		if c.family == unix.AF_INET6 && c.gw.Equal(net.ParseIP("fd96:0150::1")) {
			sawV6Ipam = true
		}
	}
	if !sawV4Pin || !sawV6Ipam {
		t.Errorf("expected v4-from-pin + v6-from-IPAM, got %+v", calls)
	}
}

// Prio 1 (DHCP > pin > IPAM): a value pushed onto the dynamic source
// channel overrides both pin and IPAM for v4. v6 stays on its prior
// source (no DHCPv6 Option-3 equivalent).
func TestRun_DHCPDynamicOverridesPinAndIPAM(t *testing.T) {
	insp := &fakeInspector{v4: net.IPv4(192, 168, 150, 1)} // would be IPAM fallback
	rt := &fakeRouter{currentV4: net.IPv4(172, 30, 0, 1)}
	ch := make(chan net.IP, 2)

	m := NewWithDeps("dmz_macvlan",
		net.IPv4(10, 200, 0, 1), nil, // pin v4 only
		ch, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	// First assert: pin wins (DHCP hasn't pushed yet).
	calls := waitForReplaces(rt, 1, 200*time.Millisecond)
	if len(calls) < 1 || !calls[0].gw.Equal(net.IPv4(10, 200, 0, 1)) {
		t.Fatalf("expected pin to win pre-DHCP, got %+v", calls)
	}

	// DHCP pushes Option 3 — must override.
	ch <- net.IPv4(192, 168, 150, 254)

	calls = waitForReplaces(rt, 2, 200*time.Millisecond)
	cancel()
	<-done

	gotDHCP := false
	for _, c := range calls {
		if c.family == unix.AF_INET && c.gw.Equal(net.IPv4(192, 168, 150, 254)) {
			gotDHCP = true
		}
	}
	if !gotDHCP {
		t.Errorf("expected DHCP gateway to override after push, got %+v", calls)
	}
}

// When DHCP pushes the same value twice (typical of lease renewals
// returning the same Router option), no churn — the second push must
// not trigger another replace.
func TestRun_DHCPRenewalSameValueNoChurn(t *testing.T) {
	insp := &fakeInspector{}
	rt := &fakeRouter{}
	ch := make(chan net.IP, 4)
	m := NewWithDeps("", nil, nil, ch, 50*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	ch <- net.IPv4(192, 168, 150, 1)
	calls1 := waitForReplaces(rt, 1, 200*time.Millisecond)
	if len(calls1) != 1 {
		t.Fatalf("first push must install: got %+v", calls1)
	}

	// Renewal — same value. Wait a bit, no new replace must fire.
	ch <- net.IPv4(192, 168, 150, 1)
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if got := len(rt.replaces()); got != 1 {
		t.Errorf("renewal with same value must not churn, got %d replaces (%+v)", got, rt.replaces())
	}
}

// DHCP closes the channel mid-flight (graceful supervisor shutdown
// before manager's ctx cancel). Manager must keep ticking with the
// last-known-good dynV4 — the closed channel must NOT degrade to
// pin/IPAM (DHCP-supplied is still the most authoritative value we
// have for this process lifetime, even after the source has gone
// quiet).
func TestRun_DHCPChannelClosedKeepsLastValue(t *testing.T) {
	insp := &fakeInspector{}
	rt := &fakeRouter{}
	ch := make(chan net.IP, 2)
	m := NewWithDeps("",
		net.IPv4(10, 200, 0, 1), nil, // pin should be ignored once DHCP spoke
		ch, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	ch <- net.IPv4(192, 168, 150, 1)
	// Wait until the DHCP push has been processed (replace happened).
	waitForReplaces(rt, 2, 200*time.Millisecond) // 1 pin-startup + 1 dhcp-push

	// Snapshot replace-count at the moment we close the channel.
	preClose := len(rt.replaces())
	close(ch)
	time.Sleep(50 * time.Millisecond) // let several ticks elapse
	cancel()
	<-done

	post := rt.replaces()[preClose:]
	for _, c := range post {
		if c.family == unix.AF_INET && c.gw.Equal(net.IPv4(10, 200, 0, 1)) {
			t.Errorf("after DHCP closed, manager fell back to pin; got %v in post-close replaces %+v", c.gw, post)
		}
	}
}

// Prio 4 (nothing): no pin, no DHCP channel push, IPAM lookup empty.
// Manager must be a quiet no-op — no router writes — but the loop
// must keep running (so a later DHCP push would still be honoured).
func TestRun_NothingResolved_QuietNoop(t *testing.T) {
	insp := &fakeInspector{} // both v4/v6 nil, no error
	rt := &fakeRouter{}
	ch := make(chan net.IP)
	m := NewWithDeps("dmz_macvlan", nil, nil, ch, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done

	if calls := rt.replaces(); len(calls) != 0 {
		t.Errorf("expected no router writes with nothing resolved, got %+v", calls)
	}
}

// IPAM lookup error: enforcement falls back to pin (if any), loop
// keeps running. Same robustness contract as before.
func TestRun_IPAMErrorFallsBackToPin(t *testing.T) {
	insp := &fakeInspector{err: errors.New("docker unreachable")}
	rt := &fakeRouter{}
	m := NewWithDeps("dmz_macvlan",
		net.IPv4(10, 200, 0, 1), nil,
		nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	calls := waitForReplaces(rt, 1, 200*time.Millisecond)
	cancel()
	<-done

	if len(calls) < 1 || !calls[0].gw.Equal(net.IPv4(10, 200, 0, 1)) {
		t.Errorf("expected pin fallback when IPAM errors, got %+v", calls)
	}
}

// Periodic re-assert: external party (Docker, operator) flips the
// default back to a bridge. Next tick restores the effective gateway
// — pin/IPAM/DHCP source unchanged.
func TestRun_ReAssertsOnExternalRevert(t *testing.T) {
	insp := &fakeInspector{v4: net.IPv4(192, 168, 150, 1)}
	rt := &fakeRouter{currentV4: net.IPv4(172, 30, 0, 1)}
	m := NewWithDeps("dmz_macvlan", nil, nil, nil, 20*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	waitForReplaces(rt, 1, 100*time.Millisecond)

	// Simulate Docker / operator flipping the default.
	rt.mu.Lock()
	rt.currentV4 = net.IPv4(172, 30, 0, 1)
	rt.mu.Unlock()

	calls := waitForReplaces(rt, 2, 200*time.Millisecond)
	cancel()
	<-done

	if len(calls) < 2 {
		t.Errorf("expected re-assert after external revert, got %d (%+v)", len(calls), calls)
	}
}
