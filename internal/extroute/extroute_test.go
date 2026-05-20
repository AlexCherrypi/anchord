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
	// Mirror kernel behaviour: replace updates the current default
	// gateway so subsequent assert calls see the new value.
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

func TestRun_ReplacesBridgeDefaultWithMacvlan(t *testing.T) {
	insp := &fakeInspector{v4: net.IPv4(192, 168, 150, 1)}
	rt := &fakeRouter{currentV4: net.IPv4(172, 30, 0, 1)} // Docker bridge default
	m := NewWithDeps("dmz_macvlan", nil, nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	// One assert must happen before the first tick. 50ms is plenty.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	calls := rt.replaces()
	if len(calls) < 1 {
		t.Fatalf("expected at least 1 ReplaceDefaultRoute call, got 0")
	}
	if !calls[0].gw.Equal(net.IPv4(192, 168, 150, 1)) || calls[0].family != unix.AF_INET {
		t.Errorf("first replace = %+v, want v4 192.168.150.1", calls[0])
	}
}

// When the current default already points at the macvlan gateway,
// assert must NOT call ReplaceDefaultRoute. Guards against churn on
// every tick of the periodic re-assert loop.
func TestRun_NoReplaceWhenAlreadyCorrect(t *testing.T) {
	insp := &fakeInspector{v4: net.IPv4(192, 168, 150, 1)}
	rt := &fakeRouter{currentV4: net.IPv4(192, 168, 150, 1)} // already correct
	m := NewWithDeps("dmz_macvlan", nil, nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if calls := rt.replaces(); len(calls) != 0 {
		t.Errorf("expected 0 ReplaceDefaultRoute calls when default already correct, got %d (%+v)", len(calls), calls)
	}
}

// Operator-pinned ANCHORD_EXT_GATEWAY_IP must skip the Docker inspect
// path entirely. Use case: external macvlan networks whose IPAM
// config Docker can't read.
func TestRun_PinnedGatewaySkipsInspect(t *testing.T) {
	insp := &fakeInspector{}
	rt := &fakeRouter{currentV4: net.IPv4(172, 30, 0, 1)}
	m := NewWithDeps("", net.IPv4(192, 168, 150, 1), nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if insp.calls != 0 {
		t.Errorf("expected 0 Docker inspect calls when gateway pinned, got %d", insp.calls)
	}
	if calls := rt.replaces(); len(calls) != 1 || !calls[0].gw.Equal(net.IPv4(192, 168, 150, 1)) {
		t.Errorf("expected pin to drive 1 replace to 192.168.150.1, got %+v", calls)
	}
}

// Dual-stack: a v4+v6 inspect result must produce one replace per
// family, both with the resolved gateways.
func TestRun_DualStack(t *testing.T) {
	insp := &fakeInspector{
		v4: net.IPv4(192, 168, 150, 1),
		v6: net.ParseIP("fd96:0150::1"),
	}
	rt := &fakeRouter{
		currentV4: net.IPv4(172, 30, 0, 1),
		currentV6: net.ParseIP("fd00:1070::1"),
	}
	m := NewWithDeps("dmz_macvlan", nil, nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	calls := rt.replaces()
	var sawV4, sawV6 bool
	for _, c := range calls {
		if c.family == unix.AF_INET && c.gw.Equal(net.IPv4(192, 168, 150, 1)) {
			sawV4 = true
		}
		if c.family == unix.AF_INET6 && c.gw.Equal(net.ParseIP("fd96:0150::1")) {
			sawV6 = true
		}
	}
	if !sawV4 || !sawV6 {
		t.Errorf("expected one v4 + one v6 replace, got %+v", calls)
	}
}

// No ExtNetwork + no pin → manager is a documented no-op that waits
// for ctx without touching netlink. Used by single-network anchord
// deployments where there's nothing to enforce.
func TestRun_NoExtNetworkNoPin_IsNoop(t *testing.T) {
	insp := &fakeInspector{}
	rt := &fakeRouter{}
	m := NewWithDeps("", nil, nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if insp.calls != 0 {
		t.Errorf("inspect must not be called with empty ExtNetwork, got %d", insp.calls)
	}
	if calls := rt.replaces(); len(calls) != 0 {
		t.Errorf("router must not be touched with empty ExtNetwork, got %+v", calls)
	}
}

// Inspect failure → enforcement disabled but Run blocks until ctx is
// cancelled. Anchord must keep running; this is QoL, not a hard dep.
func TestRun_InspectErrorDoesNotKillRun(t *testing.T) {
	insp := &fakeInspector{err: errors.New("docker unreachable")}
	rt := &fakeRouter{}
	m := NewWithDeps("dmz_macvlan", nil, nil, 10*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	select {
	case <-done:
		t.Fatal("Run returned before ctx cancel — inspect error must not kill the loop")
	case <-time.After(40 * time.Millisecond):
	}
	cancel()
	<-done

	if calls := rt.replaces(); len(calls) != 0 {
		t.Errorf("no router calls expected when inspect fails, got %+v", calls)
	}
}

// Periodic re-assert: an external party (Docker, operator) flips the
// default back to a bridge. The next tick must restore it.
func TestRun_ReAssertsOnExternalRevert(t *testing.T) {
	insp := &fakeInspector{v4: net.IPv4(192, 168, 150, 1)}
	rt := &fakeRouter{currentV4: net.IPv4(172, 30, 0, 1)}
	m := NewWithDeps("dmz_macvlan", nil, nil, 20*time.Millisecond, insp, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()

	// Wait for the first replace (initial assert).
	time.Sleep(30 * time.Millisecond)
	if len(rt.replaces()) < 1 {
		t.Fatalf("expected initial assert before tick")
	}

	// Simulate Docker / operator flipping the default back.
	rt.mu.Lock()
	rt.currentV4 = net.IPv4(172, 30, 0, 1)
	rt.mu.Unlock()

	// Wait for at least one tick.
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if calls := rt.replaces(); len(calls) < 2 {
		t.Errorf("expected re-assert after external revert, got %d total replaces (%+v)", len(calls), calls)
	}
}
