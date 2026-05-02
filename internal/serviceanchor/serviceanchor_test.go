package serviceanchor

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"

	"golang.org/x/sys/unix"
)

// stubResolver returns canned responses for LookupIP. It captures every
// call so tests can assert hostname + count.
type stubResolver struct {
	mu    sync.Mutex
	addrs []net.IP
	err   error
	calls int
}

func (s *stubResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]net.IP, len(s.addrs))
	copy(out, s.addrs)
	return out, nil
}

// stubRouter records every Replace/Remove invocation in order.
type stubRouter struct {
	mu      sync.Mutex
	replace []routeOp
	remove  []routeOp
	failOn  *routeOp // if set, ReplaceDefaultRoute returns errFail when matched
}

type routeOp struct {
	family int
	gw     string
}

var errFail = errors.New("fake route failure")

func (r *stubRouter) ReplaceDefaultRoute(family int, gw net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	op := routeOp{family: family, gw: gw.String()}
	if r.failOn != nil && *r.failOn == op {
		return errFail
	}
	r.replace = append(r.replace, op)
	return nil
}

func (r *stubRouter) RemoveDefaultRoute(family int, gw net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remove = append(r.remove, routeOp{family: family, gw: gw.String()})
	return nil
}

func (r *stubRouter) replaceCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.replace)
}

func (r *stubRouter) lastReplace() routeOp {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.replace) == 0 {
		return routeOp{}
	}
	return r.replace[len(r.replace)-1]
}

func newTestManager(addrs []net.IP) (*Manager, *stubResolver, *stubRouter) {
	res := &stubResolver{addrs: addrs}
	rt := &stubRouter{}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		ResolveInterval: 50 * time.Millisecond,
	}, res, rt)
	return m, res, rt
}

func TestReconcile_InstallsBothFamilies(t *testing.T) {
	v4 := net.ParseIP("172.30.0.4")
	v6 := net.ParseIP("fd30::4")
	m, _, rt := newTestManager([]net.IP{v4, v6})

	m.reconcile(context.Background())

	if got := rt.replaceCount(); got != 2 {
		t.Fatalf("expected 2 ReplaceDefaultRoute calls, got %d", got)
	}
	gotV4, gotV6 := false, false
	for _, op := range rt.replace {
		if op.family == unix.AF_INET && op.gw == v4.String() {
			gotV4 = true
		}
		if op.family == unix.AF_INET6 && op.gw == v6.String() {
			gotV6 = true
		}
	}
	if !gotV4 || !gotV6 {
		t.Errorf("missing family install: v4=%v v6=%v from %#v", gotV4, gotV6, rt.replace)
	}
}

func TestReconcile_NoOpWhenUnchanged(t *testing.T) {
	v4 := net.ParseIP("172.30.0.4")
	m, _, rt := newTestManager([]net.IP{v4})

	m.reconcile(context.Background())
	m.reconcile(context.Background())
	m.reconcile(context.Background())

	if got := rt.replaceCount(); got != 1 {
		t.Errorf("expected 1 install, got %d (%#v)", got, rt.replace)
	}
}

func TestReconcile_ReplacesOnIPChange(t *testing.T) {
	v4a := net.ParseIP("172.30.0.4")
	v4b := net.ParseIP("172.30.0.7")
	m, res, rt := newTestManager([]net.IP{v4a})

	m.reconcile(context.Background())

	res.mu.Lock()
	res.addrs = []net.IP{v4b}
	res.mu.Unlock()

	m.reconcile(context.Background())

	if got := rt.replaceCount(); got != 2 {
		t.Fatalf("expected 2 installs, got %d", got)
	}
	if last := rt.lastReplace(); last.gw != v4b.String() {
		t.Errorf("last install gw=%s, want %s", last.gw, v4b)
	}
}

func TestReconcile_KeepsLastGoodOnLookupError(t *testing.T) {
	v4 := net.ParseIP("172.30.0.4")
	m, res, rt := newTestManager([]net.IP{v4})

	m.reconcile(context.Background())
	if rt.replaceCount() != 1 {
		t.Fatalf("setup: expected 1 install")
	}

	res.mu.Lock()
	res.err = errors.New("dns down")
	res.mu.Unlock()
	m.reconcile(context.Background())

	// No new install should have been attempted; lastReplace unchanged.
	if rt.replaceCount() != 1 {
		t.Errorf("install count changed during DNS outage: %d", rt.replaceCount())
	}
}

func TestReconcile_RetriesAfterFailedInstall(t *testing.T) {
	v4 := net.ParseIP("172.30.0.4")
	res := &stubResolver{addrs: []net.IP{v4}}
	failOn := routeOp{family: unix.AF_INET, gw: v4.String()}
	rt := &stubRouter{failOn: &failOn}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		ResolveInterval: 50 * time.Millisecond,
	}, res, rt)

	// First reconcile: ReplaceDefaultRoute returns errFail. Manager
	// must NOT cache the gateway, so the next reconcile retries.
	m.reconcile(context.Background())

	// Drop the failure injection, then reconcile again.
	rt.mu.Lock()
	rt.failOn = nil
	rt.mu.Unlock()
	m.reconcile(context.Background())

	if got := rt.replaceCount(); got != 1 {
		t.Errorf("expected eventual successful install, got count=%d", got)
	}
}

func TestRun_LoopsAndCleansUp(t *testing.T) {
	v4 := net.ParseIP("172.30.0.4")
	m, _, rt := newTestManager([]net.IP{v4})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Wait briefly for at least one reconcile to have run.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rt.replaceCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rt.replaceCount() < 1 {
		t.Fatal("Run did not perform initial reconcile")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// Cleanup should have removed the installed route.
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.remove) != 1 {
		t.Errorf("expected 1 RemoveDefaultRoute, got %d", len(rt.remove))
	}
}

func TestDefaultRouteFor_Validation(t *testing.T) {
	// v6 address into v4 family should reject (the family is the truth).
	if r := defaultRouteFor(unix.AF_INET, net.ParseIP("fd30::1")); r != nil {
		t.Error("v6 addr into v4 family should be rejected")
	}
	// v4 address into v6 family should reject.
	if r := defaultRouteFor(unix.AF_INET6, net.ParseIP("172.30.0.1")); r != nil {
		t.Error("v4 addr into v6 family should be rejected")
	}
	// Bogus family.
	if r := defaultRouteFor(99, net.ParseIP("172.30.0.1")); r != nil {
		t.Error("bogus family should be rejected")
	}
	// Sane v4.
	r := defaultRouteFor(unix.AF_INET, net.ParseIP("172.30.0.1"))
	if r == nil || r.Gw == nil || r.Dst.IP.To4() == nil {
		t.Error("v4 default route should be constructed")
	}
}
