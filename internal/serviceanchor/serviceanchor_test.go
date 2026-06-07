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

	// existingDefaults pretends the netns already has these default
	// routes at startup (F-39 wrap mode: target container's
	// Docker-managed bridge gateway). Keyed by family.
	existingDefaults map[int]net.IP
	recordErr        error
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
	// Persist into the fake kernel state so the next RecordDefaultRoute
	// reflects what's installed — matches real netlink semantics and
	// lets the applyRoute short-circuit (kernel-match → no-op) fire.
	if r.existingDefaults == nil {
		r.existingDefaults = map[int]net.IP{}
	}
	r.existingDefaults[family] = gw
	return nil
}

func (r *stubRouter) RemoveDefaultRoute(family int, gw net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remove = append(r.remove, routeOp{family: family, gw: gw.String()})
	delete(r.existingDefaults, family)
	return nil
}

// RecordDefaultRoute returns the fake kernel's current default for the
// given family. Tests can pre-seed via `existingDefaults` (e.g. F-39
// wrap mode's pre-existing Docker bridge gateway); subsequent
// ReplaceDefaultRoute calls update the same map.
func (r *stubRouter) RecordDefaultRoute(family int) (net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recordErr != nil {
		return nil, r.recordErr
	}
	if ip, ok := r.existingDefaults[family]; ok {
		return ip, nil
	}
	return nil, nil
}

// flushDefault simulates an external `ip route del default` — the
// fake kernel forgets the route for that family. Used by the
// regression test for the "route flushed under our feet" scenario.
func (r *stubRouter) flushDefault(family int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.existingDefaults, family)
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

// TestReconcile_ReinstallsAfterExternalFlush is the regression guard
// for the defensive-against-flushes change: if something external
// (`ip route del default`, an unrelated tool, a kernel quirk)
// removes the default route while the resolver IP stays the same,
// the next reconcile must put the route back. Pre-fix behaviour
// short-circuited on the in-memory cache match and missed this.
func TestReconcile_ReinstallsAfterExternalFlush(t *testing.T) {
	v4 := net.ParseIP("172.30.0.4")
	m, _, rt := newTestManager([]net.IP{v4})

	// First reconcile installs once and the fake kernel now holds it.
	m.reconcile(context.Background())
	if got := rt.replaceCount(); got != 1 {
		t.Fatalf("initial install: expected 1 ReplaceDefaultRoute, got %d", got)
	}

	// Simulate the route being flushed externally — same scenario as
	// `ip route del default` in the netns or a kernel quirk on link
	// renumber. The resolver continues to return the same IP.
	rt.flushDefault(unix.AF_INET)

	m.reconcile(context.Background())

	if got := rt.replaceCount(); got != 2 {
		t.Errorf("expected reinstall after flush (2 total), got %d (%#v)",
			got, rt.replace)
	}

	// And once back in place, further reconciles must NOT churn.
	m.reconcile(context.Background())
	m.reconcile(context.Background())
	if got := rt.replaceCount(); got != 2 {
		t.Errorf("expected no extra churn after reinstall, got %d (%#v)",
			got, rt.replace)
	}
}

// TestReconcile_ReinstallsWhenKernelHasDifferentGateway covers the
// "drift" half of the defense: kernel has a default route, but to a
// gateway different from our resolver-supplied one. Common cause:
// network-anchor was recreated with a new transit IP and Docker DNS
// already returned the new IP — but a stale route still points at
// the old one. The applyRoute path must replace.
func TestReconcile_ReinstallsWhenKernelHasDifferentGateway(t *testing.T) {
	v4 := net.ParseIP("172.30.0.4")
	m, _, rt := newTestManager([]net.IP{v4})

	// Pre-seed a stale default — say, the old network-anchor's IP.
	rt.existingDefaults = map[int]net.IP{
		unix.AF_INET: net.ParseIP("172.30.0.99"),
	}

	m.reconcile(context.Background())

	if got := rt.replaceCount(); got != 1 {
		t.Errorf("expected replace on gateway drift, got %d (%#v)",
			got, rt.replace)
	}
	if got := rt.lastReplace().gw; got != v4.String() {
		t.Errorf("expected gateway %s, got %s", v4, got)
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

// F-40: when ANCHORD_GATEWAY_IP populates cfg.GatewayIPs, Run() must
// install those routes directly and never call the DNS resolver.
func TestRun_IPMode_SkipsDNS(t *testing.T) {
	v4 := net.ParseIP("192.168.150.1").To4()
	v6 := net.ParseIP("fd00::1")
	res := &stubResolver{
		err: errors.New("resolver should not be called in IP mode"),
	}
	rt := &stubRouter{}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		GatewayIPs:      []net.IP{v4, v6},
		ResolveInterval: 50 * time.Millisecond,
	}, res, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Give the goroutine a beat to install the static routes.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rt.replaceCount() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rt.replaceCount() != 2 {
		t.Fatalf("IP-mode startup must install one route per configured IP, got %d", rt.replaceCount())
	}

	// Resolver must not have been touched.
	res.mu.Lock()
	calls := res.calls
	res.mu.Unlock()
	if calls != 0 {
		t.Errorf("DNS resolver was called %d times in IP mode; expected 0", calls)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel in IP mode")
	}

	// Both routes must have been removed on shutdown.
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.remove) != 2 {
		t.Errorf("IP-mode cleanup must remove all installed routes; got %d", len(rt.remove))
	}
}

// F-40: in IP mode, no periodic resolve ticks — installs happen once
// at startup. After the initial pair of installs, the count stays at 2
// for at least a few ResolveInterval cycles.
func TestRun_IPMode_NoPeriodicResolve(t *testing.T) {
	v4 := net.ParseIP("10.0.0.1").To4()
	res := &stubResolver{}
	rt := &stubRouter{}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		GatewayIPs:      []net.IP{v4},
		ResolveInterval: 20 * time.Millisecond, // very tight to make a periodic loop visible
	}, res, rt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	// Wait for the initial install.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && rt.replaceCount() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if rt.replaceCount() != 1 {
		t.Fatalf("expected 1 install, got %d", rt.replaceCount())
	}

	// Sit through ~10 hypothetical resolve intervals. The count must not grow.
	time.Sleep(200 * time.Millisecond)
	if got := rt.replaceCount(); got != 1 {
		t.Errorf("IP mode should be static; got %d installs after wait", got)
	}
	res.mu.Lock()
	calls := res.calls
	res.mu.Unlock()
	if calls != 0 {
		t.Errorf("IP mode should never call DNS; got %d calls", calls)
	}
}

// F-39 wrap-mode: when the target netns already has a Docker-managed
// default route, the service-anchor must record it at startup and
// restore it on shutdown so the wrapped app keeps egress after the
// wrap-stack is torn down.
func TestRun_WrapMode_RestoresOriginalOnShutdown(t *testing.T) {
	dockerGw := net.ParseIP("172.30.0.1").To4()
	anchorGw := net.ParseIP("172.30.0.5").To4()
	res := &stubResolver{addrs: []net.IP{anchorGw}}
	rt := &stubRouter{
		existingDefaults: map[int]net.IP{unix.AF_INET: dockerGw},
	}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		ResolveInterval: 50 * time.Millisecond,
	}, res, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Wait for our route to be installed (anchor as gateway).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rt.replaceCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rt.lastReplace().gw != anchorGw.String() {
		t.Fatalf("expected install of anchor gw, last replace = %v", rt.lastReplace())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// Two operations should have happened on shutdown, in order:
	//   1. Remove our installed route (anchorGw)
	//   2. Replace with the recorded original (dockerGw)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.remove) != 1 || rt.remove[0].gw != anchorGw.String() {
		t.Errorf("expected exactly one remove of anchor gw, got %#v", rt.remove)
	}
	// Replaces during the test: 1 = anchor install; 2 = restore docker gw.
	if len(rt.replace) != 2 {
		t.Fatalf("expected 2 replace calls (install + restore), got %d: %#v", len(rt.replace), rt.replace)
	}
	if rt.replace[1].gw != dockerGw.String() {
		t.Errorf("restore should put dockerGw back, got %v", rt.replace[1])
	}
}

// F-39: greenfield case — no pre-existing default route in the netns
// (the usual internal:true bridge pattern). recordOriginals captures
// nothing, cleanup is identical to pre-F-39 behaviour: remove our
// route, restore nothing.
func TestRun_GreenfieldMode_NoRestore(t *testing.T) {
	anchorGw := net.ParseIP("172.30.0.5").To4()
	res := &stubResolver{addrs: []net.IP{anchorGw}}
	rt := &stubRouter{
		// No existingDefaults seeded → RecordDefaultRoute returns nil.
	}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		ResolveInterval: 50 * time.Millisecond,
	}, res, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && rt.replaceCount() < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Exactly one replace (install) and exactly one remove. No
	// extra replace for "restore" since there was nothing to record.
	if len(rt.replace) != 1 {
		t.Errorf("greenfield mode should not call ReplaceDefaultRoute for restore; got %d total replaces", len(rt.replace))
	}
	if len(rt.remove) != 1 {
		t.Errorf("greenfield mode should remove our installed route exactly once; got %d", len(rt.remove))
	}
}

// F-39 wrap-mode IPv6 + IPv4: both families' original routes are
// recorded and both are restored on shutdown.
func TestRun_WrapMode_DualStackRestore(t *testing.T) {
	dockerV4 := net.ParseIP("172.30.0.1").To4()
	dockerV6 := net.ParseIP("fd30::1")
	anchorV4 := net.ParseIP("172.30.0.5").To4()
	anchorV6 := net.ParseIP("fd30::5")
	res := &stubResolver{addrs: []net.IP{anchorV4, anchorV6}}
	rt := &stubRouter{
		existingDefaults: map[int]net.IP{
			unix.AF_INET:  dockerV4,
			unix.AF_INET6: dockerV6,
		},
	}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		ResolveInterval: 50 * time.Millisecond,
	}, res, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Wait for both families to be installed.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && rt.replaceCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.replaceCount() < 2 {
		t.Fatalf("expected both families installed, got %d", rt.replaceCount())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// Both families' original routes restored. Look at the last two
	// replace ops — they should be the restores.
	rt.mu.Lock()
	defer rt.mu.Unlock()
	restored := map[string]bool{}
	for _, op := range rt.replace {
		if op.gw == dockerV4.String() {
			restored["v4"] = true
		}
		if op.gw == dockerV6.String() {
			restored["v6"] = true
		}
	}
	if !restored["v4"] || !restored["v6"] {
		t.Errorf("expected both v4 and v6 originals restored; got: %v from %#v", restored, rt.replace)
	}
}

// F-39: a recordErr at startup must not abort the service — we proceed
// without the safety net. Cleanup just removes our own route.
func TestRun_RecordErrorTolerated(t *testing.T) {
	anchorGw := net.ParseIP("172.30.0.5").To4()
	res := &stubResolver{addrs: []net.IP{anchorGw}}
	rt := &stubRouter{
		recordErr: errors.New("netlink failure"),
	}
	m := NewWithDeps(&config.ServiceAnchor{
		GatewayHostname: "anchord",
		ResolveInterval: 50 * time.Millisecond,
	}, res, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && rt.replaceCount() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.replaceCount() < 1 {
		t.Fatal("service-anchor should not abort when RecordDefaultRoute fails")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// F-39: isAllZerosCIDR helper handles both v4 and v6 default-route
// destination shapes correctly. Critical because the netlink kernel
// API returns 0.0.0.0/0 (or ::/0) for the default route's Dst, and
// missing the /0 check would either miss real defaults or accidentally
// match every subnet.
func TestIsAllZerosCIDR(t *testing.T) {
	cases := []struct {
		name string
		in   *net.IPNet
		want bool
	}{
		{"nil", nil, false},
		{"0.0.0.0/0", &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, true},
		{"::/0", &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, true},
		{"10.0.0.0/8", &net.IPNet{IP: net.IPv4(10, 0, 0, 0), Mask: net.CIDRMask(8, 32)}, false},
		{"fd30::/64", &net.IPNet{IP: net.ParseIP("fd30::"), Mask: net.CIDRMask(64, 128)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAllZerosCIDR(tc.in); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
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
