package rebinder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"
)

// ---- resolveFollower -------------------------------------------------------

func TestResolveFollower_ComposeServicePreferred(t *testing.T) {
	all := []ContainerInfo{
		{ // wrong project — must be skipped even though service name matches
			ID:    "wrong-project",
			Names: []string{"/wrong"},
			Labels: map[string]string{
				"com.docker.compose.project": "other",
				"com.docker.compose.service": "sync",
			},
		},
		{
			ID:    "right-one",
			Names: []string{"/follower-stack-sync-1"},
			Labels: map[string]string{
				"com.docker.compose.project": "follower-stack",
				"com.docker.compose.service": "sync",
			},
		},
	}
	got := resolveFollower(all, "sync", "follower-stack")
	if got != "right-one" {
		t.Errorf("got %q, want right-one", got)
	}
}

func TestResolveFollower_NameFallback(t *testing.T) {
	all := []ContainerInfo{
		{ID: "id-1", Names: []string{"/other"}},
		{ID: "id-2", Names: []string{"/authentik-mailbox-sync"}},
	}
	// Empty selfProject + a name match: name fallback wins.
	got := resolveFollower(all, "authentik-mailbox-sync", "")
	if got != "id-2" {
		t.Errorf("got %q, want id-2", got)
	}
}

func TestResolveFollower_NotFound(t *testing.T) {
	all := []ContainerInfo{
		{ID: "id-1", Names: []string{"/other"}},
	}
	if got := resolveFollower(all, "missing", "any-project"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveFollower_EmptyTarget(t *testing.T) {
	if got := resolveFollower(nil, "", "p"); got != "" {
		t.Errorf("empty target should not match anything")
	}
}

// ---- isAlreadyConnected / isAlreadyNotAttached -----------------------------

func TestIsAlreadyConnected(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("Error response from daemon: endpoint with name foo already exists in network bar"), true},
		{errors.New("endpoint already exists"), true},
		{errors.New("unrelated docker error"), false},
	}
	for _, c := range cases {
		got := isAlreadyConnected(c.err)
		if got != c.want {
			t.Errorf("isAlreadyConnected(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestIsAlreadyNotAttached(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("container foo is not connected to network bar"), true},
		{errors.New("Error response from daemon: container abc not connected to network xyz"), true},
		{errors.New("no such network endpoint"), true},
		{errors.New("something else"), false},
	}
	for _, c := range cases {
		got := isAlreadyNotAttached(c.err)
		if got != c.want {
			t.Errorf("isAlreadyNotAttached(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// ---- fakeOps ---------------------------------------------------------------

// fakeOps records all calls and replays canned responses. Thread-safe
// so the event consumer can be exercised from a separate goroutine.
type fakeOps struct {
	mu sync.Mutex

	// Canned state.
	netByName    map[string]NetworkInfo
	containers   []ContainerInfo
	followerInsp ContainerInfo
	netErr       error
	listErr      error
	inspectErr   error
	connectErr   error
	disconnErr   error
	restartErr   error

	// Event-stream wiring; tests close msgsCh to end consume cleanly.
	msgsCh chan EventMsg
	errsCh chan error

	// Recorded calls.
	calls       []string
	connectedID string
	restartedID string
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		netByName: map[string]NetworkInfo{},
		msgsCh:    make(chan EventMsg, 16),
		errsCh:    make(chan error, 1),
	}
}

func (f *fakeOps) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeOps) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeOps) NetworkInspectByName(ctx context.Context, name string) (NetworkInfo, error) {
	f.record("NetworkInspectByName:" + name)
	if f.netErr != nil {
		return NetworkInfo{}, f.netErr
	}
	n, ok := f.netByName[name]
	if !ok {
		return NetworkInfo{}, fmt.Errorf("no such network %q", name)
	}
	return n, nil
}

func (f *fakeOps) ContainerList(ctx context.Context) ([]ContainerInfo, error) {
	f.record("ContainerList")
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.containers, nil
}

func (f *fakeOps) ContainerInspect(ctx context.Context, idOrName string) (ContainerInfo, error) {
	f.record("ContainerInspect:" + idOrName)
	if f.inspectErr != nil {
		return ContainerInfo{}, f.inspectErr
	}
	return f.followerInsp, nil
}

func (f *fakeOps) NetworkConnect(ctx context.Context, networkID, containerID string) error {
	f.record("NetworkConnect:" + networkID + "/" + containerID)
	if f.connectErr != nil {
		return f.connectErr
	}
	f.mu.Lock()
	f.connectedID = networkID
	f.mu.Unlock()
	return nil
}

func (f *fakeOps) NetworkDisconnect(ctx context.Context, networkName, containerID string) error {
	f.record("NetworkDisconnect:" + networkName + "/" + containerID)
	return f.disconnErr
}

func (f *fakeOps) ContainerRestart(ctx context.Context, containerID string) error {
	f.record("ContainerRestart:" + containerID)
	if f.restartErr != nil {
		return f.restartErr
	}
	f.mu.Lock()
	f.restartedID = containerID
	f.mu.Unlock()
	return nil
}

func (f *fakeOps) Events(ctx context.Context) (<-chan EventMsg, <-chan error) {
	return f.msgsCh, f.errsCh
}

// ---- bootstrapRecheck ------------------------------------------------------

func TestBootstrapRecheck_NoDivergence_NoReattach(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "current-id", Name: "mailcow_net"}
	ops.containers = []ContainerInfo{
		{
			ID:    "follower-id",
			Names: []string{"/follower"},
			Labels: map[string]string{
				"com.docker.compose.project": "self",
				"com.docker.compose.service": "sync",
			},
		},
	}
	ops.followerInsp = ContainerInfo{
		ID: "follower-id",
		Networks: map[string]string{
			"mailcow_net": "current-id",
		},
	}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
		SelfProject:   "self",
	})
	w.bootstrapRecheck(context.Background())
	for _, c := range ops.callLog() {
		if c == "NetworkConnect:current-id/follower-id" || c == "NetworkDisconnect:mailcow_net/follower-id" {
			t.Errorf("matching IDs must not trigger reattach, got call %q", c)
		}
	}
}

func TestBootstrapRecheck_Divergence_TriggersReattach(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id", Name: "mailcow_net"}
	ops.containers = []ContainerInfo{
		{
			ID:    "follower-id",
			Names: []string{"/follower"},
			Labels: map[string]string{
				"com.docker.compose.project": "self",
				"com.docker.compose.service": "sync",
			},
		},
	}
	ops.followerInsp = ContainerInfo{
		ID: "follower-id",
		Networks: map[string]string{
			"mailcow_net": "stale-id",
		},
	}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
		SelfProject:   "self",
	})
	w.bootstrapRecheck(context.Background())
	if ops.connectedID != "new-id" {
		t.Errorf("expected reattach to new-id, got %q", ops.connectedID)
	}
}

func TestBootstrapRecheck_NetworkInspectError_DoesNotPanic(t *testing.T) {
	ops := newFakeOps()
	ops.netErr = errors.New("daemon unreachable")
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
		SelfProject:   "self",
	})
	// Must not panic; must not call any write API.
	w.bootstrapRecheck(context.Background())
	for _, c := range ops.callLog() {
		if c == "NetworkConnect:new-id/follower-id" {
			t.Errorf("must not connect when network is uninspectable")
		}
	}
}

func TestBootstrapRecheck_FollowerNotFound_DoesNotPanic(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "id1", Name: "mailcow_net"}
	ops.containers = nil // empty
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
		SelfProject:   "self",
	})
	w.bootstrapRecheck(context.Background())
	// No reattach attempted.
	for _, c := range ops.callLog() {
		if c == "NetworkConnect:id1/" {
			t.Errorf("must not connect with empty follower ID")
		}
	}
}

// ---- consume / event dispatch ----------------------------------------------

// TestConsume_DispatchesCreateToReattach verifies a network-create event
// for the configured network drives the reattach path end-to-end.
func TestConsume_DispatchesCreateToReattach(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id", Name: "mailcow_net"}
	ops.containers = []ContainerInfo{
		{
			ID:    "follower-id",
			Names: []string{"/follower"},
			Labels: map[string]string{
				"com.docker.compose.project": "self",
				"com.docker.compose.service": "sync",
			},
		},
	}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
		SelfProject:   "self",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		ops.msgsCh <- EventMsg{Action: "create", Name: "mailcow_net", NetworkID: "new-id"}
		// Give the consumer a beat to process before closing the
		// stream so the message isn't dropped on close-race.
		time.Sleep(20 * time.Millisecond)
		close(ops.msgsCh)
	}()

	_ = w.consume(ctx, ops.msgsCh, ops.errsCh)
	if ops.connectedID != "new-id" {
		t.Errorf("expected reattach to new-id, got %q", ops.connectedID)
	}
}

// TestConsume_IgnoresUnrelatedNetwork verifies events for OTHER network
// names don't trigger reattach.
func TestConsume_IgnoresUnrelatedNetwork(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
		SelfProject:   "self",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		ops.msgsCh <- EventMsg{Action: "create", Name: "some-other-net", NetworkID: "irrelevant"}
		time.Sleep(20 * time.Millisecond)
		close(ops.msgsCh)
	}()

	_ = w.consume(ctx, ops.msgsCh, ops.errsCh)
	if ops.connectedID != "" {
		t.Errorf("must not reattach for unrelated network, got connect to %q", ops.connectedID)
	}
}

// TestConsume_ReturnsOnErrChannel verifies a terminal error on the
// event stream's errs channel propagates back to Run so it can
// resubscribe. This is the path Docker daemon restarts / socket flaps
// hit in production.
func TestConsume_ReturnsOnErrChannel(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
	})

	wantErr := errors.New("simulated docker event stream death")
	ops.errsCh <- wantErr

	got := w.consume(context.Background(), ops.msgsCh, ops.errsCh)
	if !errors.Is(got, wantErr) {
		t.Errorf("consume returned %v, want %v", got, wantErr)
	}
}

// TestConsume_ReturnsOnErrChannelClosed verifies that a closed errs
// channel (the SDK's other terminal signal) also returns control to
// the caller — so Run's outer loop can resubscribe rather than
// blocking on a dead channel forever.
func TestConsume_ReturnsOnErrChannelClosed(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
	})
	close(ops.errsCh)
	got := w.consume(context.Background(), ops.msgsCh, ops.errsCh)
	if got == nil || !strings.Contains(got.Error(), "error channel closed") {
		t.Errorf("expected 'error channel closed' error, got %v", got)
	}
}

// TestConsume_ReturnsOnMsgChannelClosed verifies the same for the
// message channel — the SDK closes it on stream teardown.
func TestConsume_ReturnsOnMsgChannelClosed(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
	})
	close(ops.msgsCh)
	got := w.consume(context.Background(), ops.msgsCh, ops.errsCh)
	if got == nil || !strings.Contains(got.Error(), "message channel closed") {
		t.Errorf("expected 'message channel closed' error, got %v", got)
	}
}

// TestConsume_DestroyIsLogOnly verifies destroy events do not cause any
// write-side API calls.
func TestConsume_DestroyIsLogOnly(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "sync",
		SelfProject:   "self",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		ops.msgsCh <- EventMsg{Action: "destroy", Name: "mailcow_net"}
		time.Sleep(20 * time.Millisecond)
		close(ops.msgsCh)
	}()
	_ = w.consume(ctx, ops.msgsCh, ops.errsCh)
	for _, c := range ops.callLog() {
		if c == "ContainerList" || c == "NetworkConnect" || c == "ContainerRestart" {
			t.Errorf("destroy must not trigger write API, got call %q", c)
		}
	}
}

// ---- reattach idempotency --------------------------------------------------

func TestReattach_AlreadyConnected_TreatedAsSuccess(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = []ContainerInfo{
		{ID: "follower-id", Names: []string{"/follower"}},
	}
	ops.connectErr = errors.New("Error response from daemon: endpoint with name foo already exists in network bar")
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
	})
	// Should not error out; restart toggle off means no restart call.
	w.reattach(context.Background(), "test")
	for _, c := range ops.callLog() {
		if c == "ContainerRestart:follower-id" {
			t.Errorf("restart=false must not restart, got %q", c)
		}
	}
}

func TestReattach_NotAttached_DisconnectAbsorbed(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = []ContainerInfo{
		{ID: "follower-id", Names: []string{"/follower"}},
	}
	ops.disconnErr = errors.New("container abc is not connected to network mailcow_net")
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
	})
	w.reattach(context.Background(), "test")
	// We still expect the connect to fire (and succeed).
	if ops.connectedID != "new-id" {
		t.Errorf("expected connect to new-id despite disconnect error, got %q", ops.connectedID)
	}
}

func TestReattach_Restart_OptIn(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = []ContainerInfo{
		{ID: "follower-id", Names: []string{"/follower"}},
	}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
		Restart:       true,
	})
	w.reattach(context.Background(), "test")
	if ops.restartedID != "follower-id" {
		t.Errorf("Restart=true must restart follower, got %q", ops.restartedID)
	}
}

func TestReattach_RestartDisabled_NoRestart(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = []ContainerInfo{
		{ID: "follower-id", Names: []string{"/follower"}},
	}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
		Restart:       false,
	})
	w.reattach(context.Background(), "test")
	if ops.restartedID != "" {
		t.Errorf("Restart=false must not restart, got %q", ops.restartedID)
	}
}

// TestReattach_CallOrder asserts disconnect happens BEFORE connect.
// Reverse order would leave Docker in a transient state where the
// follower has two endpoints on the same network — for that moment
// it has the *correct* attachment plus the stale one, which on some
// kernels triggers ARP / IP-conflict warnings on the bridge. Order is
// part of the contract, not an implementation detail.
func TestReattach_CallOrder(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = []ContainerInfo{
		{ID: "follower-id", Names: []string{"/follower"}},
	}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
	})
	w.reattach(context.Background(), "test")

	var disconnectIdx, connectIdx = -1, -1
	for i, c := range ops.callLog() {
		if strings.HasPrefix(c, "NetworkDisconnect:") && disconnectIdx < 0 {
			disconnectIdx = i
		}
		if strings.HasPrefix(c, "NetworkConnect:") && connectIdx < 0 {
			connectIdx = i
		}
	}
	if disconnectIdx < 0 || connectIdx < 0 {
		t.Fatalf("expected both Disconnect and Connect calls, got %v", ops.callLog())
	}
	if disconnectIdx > connectIdx {
		t.Errorf("Disconnect must precede Connect; got disconnect@%d connect@%d (full log: %v)",
			disconnectIdx, connectIdx, ops.callLog())
	}
}

// TestReattach_RestartAfterConnect asserts the restart (when enabled)
// is the LAST step. A restart before the connect would tear down the
// follower's process before its replacement attachment is in place —
// pointless extra downtime.
func TestReattach_RestartAfterConnect(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = []ContainerInfo{
		{ID: "follower-id", Names: []string{"/follower"}},
	}
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
		Restart:       true,
	})
	w.reattach(context.Background(), "test")

	connectIdx, restartIdx := -1, -1
	for i, c := range ops.callLog() {
		if strings.HasPrefix(c, "NetworkConnect:") && connectIdx < 0 {
			connectIdx = i
		}
		if strings.HasPrefix(c, "ContainerRestart:") && restartIdx < 0 {
			restartIdx = i
		}
	}
	if connectIdx < 0 || restartIdx < 0 {
		t.Fatalf("expected both Connect and Restart calls, got %v", ops.callLog())
	}
	if restartIdx < connectIdx {
		t.Errorf("Restart must follow Connect; got connect@%d restart@%d", connectIdx, restartIdx)
	}
}

// TestReattach_ConnectFailureSkipsRestart asserts that if connect
// fails (not via an absorbed "already exists"), the optional restart
// is skipped — restarting on a half-done attach would extend the
// outage rather than recover it.
func TestReattach_ConnectFailureSkipsRestart(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = []ContainerInfo{
		{ID: "follower-id", Names: []string{"/follower"}},
	}
	ops.connectErr = errors.New("connection refused")
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
		Restart:       true,
	})
	w.reattach(context.Background(), "test")
	if ops.restartedID != "" {
		t.Errorf("restart must not fire when connect failed, got restart of %q", ops.restartedID)
	}
}

func TestReattach_FollowerNotFound_NoConnect(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "new-id"}
	ops.containers = nil
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
	})
	w.reattach(context.Background(), "test")
	if ops.connectedID != "" {
		t.Errorf("must not connect when follower is unknown, got %q", ops.connectedID)
	}
}

// ---- ctxSleep --------------------------------------------------------------

func TestCtxSleep_CancelsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ctxSleep(ctx, 5*time.Second)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctxSleep did not return after cancel")
	}
}

func TestCtxSleep_ZeroDuration(t *testing.T) {
	// Must return immediately without spinning a timer.
	start := time.Now()
	ctxSleep(context.Background(), 0)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("zero-duration sleep took %v", d)
	}
}

// ---- Run lifecycle ---------------------------------------------------------

// TestRun_ExitsOnContextCancel verifies the Run loop honours ctx.
func TestRun_ExitsOnContextCancel(t *testing.T) {
	ops := newFakeOps()
	ops.netByName["mailcow_net"] = NetworkInfo{ID: "id"}
	ops.containers = nil
	w := newWithOps(ops, &config.Rebinder{
		FollowNetwork: "mailcow_net",
		FollowTarget:  "follower",
	})

	ctx, cancel := context.WithCancel(context.Background())
	var done atomic.Bool
	go func() {
		_ = w.Run(ctx)
		done.Store(true)
	}()

	// Give Run a moment to enter the event loop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Need to close the channels so the consume loop unblocks cleanly
	// once it's seen the cancel signal.
	close(ops.msgsCh)

	deadline := time.After(time.Second)
	for !done.Load() {
		select {
		case <-deadline:
			t.Fatal("Run did not return after context cancel")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
