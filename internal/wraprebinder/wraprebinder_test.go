package wraprebinder

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

// ---- buildTargetPool -------------------------------------------------------

// TestBuildTargetPool_OnlySelfProjectSiblings exercises the most
// important slice: wrap-anchors in our project make it into the pool,
// keyed by the *name* of the live target their stored ID resolves to.
// Anchors outside the self project, and siblings with non-container
// netmodes, are excluded.
func TestBuildTargetPool_OnlySelfProjectSiblings(t *testing.T) {
	// Target lives in the unrelated `ix-mailcow` project; pool key is
	// its name. Anchor in the wrap-stack stores the target's long ID
	// in NetworkMode (post-resolution), which buildTargetPool maps
	// back to the name.
	target := ContainerInfo{
		ID:    "target-long-id-fixed-len-for-test-only-pad-aaaa",
		Names: []string{"/dovecot-mailcow"},
	}
	all := []ContainerInfo{
		target,
		{
			ID:          "wrap-1",
			Names:       []string{"/dovecot-anchor"},
			NetworkMode: "container:" + target.ID,
			Labels:      map[string]string{"com.docker.compose.project": "wrap-stack"},
		},
		{
			// Wrong project — must NOT contribute even though netmode looks like a wrap.
			ID:          "stranger",
			Names:       []string{"/stranger"},
			NetworkMode: "container:" + target.ID,
			Labels:      map[string]string{"com.docker.compose.project": "other"},
		},
		{
			// Same project but bridge netmode — not a wrap-anchor.
			ID:          "sidecar",
			Names:       []string{"/sidecar"},
			NetworkMode: "bridge",
			Labels:      map[string]string{"com.docker.compose.project": "wrap-stack"},
		},
	}
	pool, orphans := buildTargetPool(all, "wrap-stack", "")
	if len(orphans) != 0 {
		t.Errorf("expected no orphans, got %v", orphans)
	}
	if got := pool["dovecot-mailcow"]; len(got) != 1 || got[0] != "wrap-1" {
		t.Errorf("pool[dovecot-mailcow] = %v, want [wrap-1]", got)
	}
}

func TestBuildTargetPool_ExcludesSelfID(t *testing.T) {
	all := []ContainerInfo{
		{
			ID:          "self-id",
			Names:       []string{"/sidecar"},
			NetworkMode: "container:weird-self-wrap",
			Labels:      map[string]string{"com.docker.compose.project": "wrap-stack"},
		},
	}
	pool, _ := buildTargetPool(all, "wrap-stack", "self-id")
	if len(pool) != 0 {
		t.Errorf("expected empty pool when only container is self, got %v", pool)
	}
}

func TestBuildTargetPool_MultipleAnchorsSharingTarget(t *testing.T) {
	target := ContainerInfo{ID: "shared-target", Names: []string{"/nginx-mailcow"}}
	all := []ContainerInfo{
		target,
		{ID: "a1", NetworkMode: "container:shared-target",
			Labels: map[string]string{"com.docker.compose.project": "ws"}},
		{ID: "a2", NetworkMode: "container:shared-target",
			Labels: map[string]string{"com.docker.compose.project": "ws"}},
	}
	pool, _ := buildTargetPool(all, "ws", "")
	if got := pool["nginx-mailcow"]; len(got) != 2 {
		t.Errorf("expected 2 anchors for shared target, got %v", got)
	}
}

func TestBuildTargetPool_SkipsNonContainerNetmodes(t *testing.T) {
	all := []ContainerInfo{
		{ID: "h", NetworkMode: "host", Labels: map[string]string{"com.docker.compose.project": "ws"}},
		{ID: "b", NetworkMode: "bridge", Labels: map[string]string{"com.docker.compose.project": "ws"}},
		{ID: "n", NetworkMode: "none", Labels: map[string]string{"com.docker.compose.project": "ws"}},
		{ID: "s", NetworkMode: "service:foo", Labels: map[string]string{"com.docker.compose.project": "ws"}},
		{ID: "e", NetworkMode: "", Labels: map[string]string{"com.docker.compose.project": "ws"}},
	}
	pool, orphans := buildTargetPool(all, "ws", "")
	if len(pool) != 0 || len(orphans) != 0 {
		t.Errorf("non-container netmodes must not enter the pool, got pool=%v orphans=%v", pool, orphans)
	}
}

func TestBuildTargetPool_OrphanWhenTargetIDDead(t *testing.T) {
	// Anchor's stored ID isn't in the live container list — target
	// was recreated while the sidecar was down, and we can't recover
	// the original target name. Goes to orphans, NOT pool.
	all := []ContainerInfo{
		{
			ID:          "anchor-1",
			NetworkMode: "container:long-dead-target-id",
			Labels:      map[string]string{"com.docker.compose.project": "ws"},
		},
	}
	pool, orphans := buildTargetPool(all, "ws", "")
	if len(pool) != 0 {
		t.Errorf("pool should be empty when target is dead, got %v", pool)
	}
	if len(orphans) != 1 || orphans[0].anchorID != "anchor-1" {
		t.Errorf("expected one orphan anchor-1, got %v", orphans)
	}
}

func TestBuildTargetPool_AcceptsNameReference(t *testing.T) {
	// Occasionally Docker stores the netmode reference as a name
	// (freshly-resolved-by-name, never restarted). Pool key is still
	// the name; not an orphan.
	target := ContainerInfo{ID: "id-x", Names: []string{"/by-name-target"}}
	all := []ContainerInfo{
		target,
		{
			ID:          "anchor-1",
			NetworkMode: "container:by-name-target",
			Labels:      map[string]string{"com.docker.compose.project": "ws"},
		},
	}
	pool, orphans := buildTargetPool(all, "ws", "")
	if len(orphans) != 0 {
		t.Errorf("name-keyed netmode should not be orphan, got %v", orphans)
	}
	if got := pool["by-name-target"]; len(got) != 1 || got[0] != "anchor-1" {
		t.Errorf("pool[by-name-target] = %v, want [anchor-1]", got)
	}
}

// ---- resolveTargetName + resolveContainerByName ---------------------------

func TestResolveTargetName_LongID(t *testing.T) {
	all := []ContainerInfo{{ID: "abcdef0123456789abcdef0123456789", Names: []string{"/nginx"}}}
	got := resolveTargetName(all, "abcdef0123456789abcdef0123456789")
	if got != "nginx" {
		t.Errorf("long-ID resolution: got %q", got)
	}
}

func TestResolveTargetName_ShortIDPrefix(t *testing.T) {
	all := []ContainerInfo{{ID: "abcdef012345fedcba9876", Names: []string{"/nginx"}}}
	got := resolveTargetName(all, "abcdef012345")
	if got != "nginx" {
		t.Errorf("short-ID resolution: got %q", got)
	}
}

func TestResolveTargetName_NotFound(t *testing.T) {
	got := resolveTargetName([]ContainerInfo{{ID: "x", Names: []string{"/other"}}}, "nope")
	if got != "" {
		t.Errorf("expected empty for miss, got %q", got)
	}
}

func TestResolveContainerByName_NameWithSlashPrefix(t *testing.T) {
	all := []ContainerInfo{{ID: "id1", Names: []string{"/nginx-mailcow"}}}
	got := resolveContainerByName(all, "nginx-mailcow")
	if got != "id1" {
		t.Errorf("name match failed: %q", got)
	}
}

// ---- fakeOps ---------------------------------------------------------------

// recreateCall records each RecreateWithNetworkMode invocation for
// later inspection. We track the OLD anchor ID, the new NetworkMode
// the rebinder passed, and what the fake returned as the new ID.
type recreateCall struct {
	oldID      string
	newNetMode string
	newID      string
}

type fakeOps struct {
	mu sync.Mutex

	containers []ContainerInfo
	listErr    error
	recreateErr error

	msgsCh chan EventMsg
	errsCh chan error

	calls         []string
	recreateCalls []recreateCall
	// newIDCounter is the fake's monotonic counter used to mint new
	// IDs for recreated anchors — so test assertions can verify the
	// returned ID is actually different from the input.
	newIDCounter int
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		msgsCh: make(chan EventMsg, 16),
		errsCh: make(chan error, 1),
	}
}

func (f *fakeOps) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeOps) ContainerList(ctx context.Context) ([]ContainerInfo, error) {
	f.record("ContainerList")
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.containers, nil
}

func (f *fakeOps) RecreateWithNetworkMode(ctx context.Context, id, newNetMode string, timeout time.Duration) (string, error) {
	f.record("Recreate:" + id + "->" + newNetMode)
	if f.recreateErr != nil {
		return "", f.recreateErr
	}
	f.mu.Lock()
	f.newIDCounter++
	newID := fmt.Sprintf("new-id-%d", f.newIDCounter)
	f.recreateCalls = append(f.recreateCalls, recreateCall{
		oldID: id, newNetMode: newNetMode, newID: newID,
	})
	f.mu.Unlock()
	return newID, nil
}

func (f *fakeOps) Events(ctx context.Context) (<-chan EventMsg, <-chan error) {
	return f.msgsCh, f.errsCh
}

func (f *fakeOps) recreateCountForAnchor(oldID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.recreateCalls {
		if c.oldID == oldID {
			n++
		}
	}
	return n
}

// ---- bootstrapRecheck ------------------------------------------------------

// TestBootstrapRecheck_DriftTriggersRecreate: anchor was created
// against an old target ID; the live target has a new ID; recheck
// must recreate the anchor with the target's NAME (not ID) so future
// drift cycles work.
func TestBootstrapRecheck_DriftTriggersRecreate(t *testing.T) {
	ops := newFakeOps()
	target := ContainerInfo{ID: "new-target-id", Names: []string{"/dovecot-mailcow"}}
	anchor := ContainerInfo{
		ID:          "anchor-1",
		Names:       []string{"/dovecot-anchor"},
		NetworkMode: "container:old-target-id",
		Labels:      map[string]string{"com.docker.compose.project": "ws"},
	}
	ops.containers = []ContainerInfo{anchor, target}
	w := newWithOps(ops, &config.WrapRebinder{
		SelfProject:    "ws",
		PollInterval:   30 * time.Second,
		RestartTimeout: 10 * time.Second,
	}, "self")
	// Seed the pool as enumeration would have.
	w.pool = targetPool{"dovecot-mailcow": []string{"anchor-1"}}

	w.bootstrapRecheck(context.Background(), "test")

	if got := ops.recreateCountForAnchor("anchor-1"); got != 1 {
		t.Fatalf("expected 1 recreate of anchor-1, got %d", got)
	}
	call := ops.recreateCalls[0]
	if call.newNetMode != "container:dovecot-mailcow" {
		t.Errorf("recreate should pass `container:<name>`, got %q", call.newNetMode)
	}
}

// TestBootstrapRecheck_NoDriftNoRecreate: anchor's stored netmode
// already resolves to the live target's ID; no recreate needed.
func TestBootstrapRecheck_NoDriftNoRecreate(t *testing.T) {
	ops := newFakeOps()
	target := ContainerInfo{ID: "fresh-target-id", Names: []string{"/dovecot-mailcow"}}
	anchor := ContainerInfo{
		ID:          "anchor-1",
		NetworkMode: "container:fresh-target-id",
		Labels:      map[string]string{"com.docker.compose.project": "ws"},
	}
	ops.containers = []ContainerInfo{anchor, target}
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{"dovecot-mailcow": []string{"anchor-1"}}

	w.bootstrapRecheck(context.Background(), "test")

	if len(ops.recreateCalls) != 0 {
		t.Errorf("no-drift must not recreate, got %v", ops.recreateCalls)
	}
}

// TestBootstrapRecheck_TargetMissing_NoRecreate: target not in the
// list yet (sidecar started before target stack came up). The
// anchor's netmode reference is unresolvable, but the right action
// is "wait for the target's next start event," not eager recreate
// (we'd have to guess the netmode, which is exactly what we don't
// know).
func TestBootstrapRecheck_TargetMissing_NoRecreate(t *testing.T) {
	ops := newFakeOps()
	ops.containers = []ContainerInfo{
		{
			ID:          "anchor-1",
			NetworkMode: "container:old-id",
			Labels:      map[string]string{"com.docker.compose.project": "ws"},
		},
		// Target NOT in the list.
	}
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{"dovecot-mailcow": []string{"anchor-1"}}

	w.bootstrapRecheck(context.Background(), "test")
	if len(ops.recreateCalls) != 0 {
		t.Errorf("missing target must not recreate, got %v", ops.recreateCalls)
	}
}

// TestBootstrapRecheck_NameMatchNotDrift: occasionally Docker stores
// netmode by name (freshly-name-resolved, never restarted). That's
// NOT drift.
func TestBootstrapRecheck_NameMatchNotDrift(t *testing.T) {
	ops := newFakeOps()
	target := ContainerInfo{ID: "current-id", Names: []string{"/dovecot-mailcow"}}
	anchor := ContainerInfo{
		ID:          "anchor-1",
		NetworkMode: "container:dovecot-mailcow",
		Labels:      map[string]string{"com.docker.compose.project": "ws"},
	}
	ops.containers = []ContainerInfo{anchor, target}
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{"dovecot-mailcow": []string{"anchor-1"}}

	w.bootstrapRecheck(context.Background(), "test")
	if len(ops.recreateCalls) != 0 {
		t.Errorf("name-keyed netmode must not be drift, got %v", ops.recreateCalls)
	}
}

// TestBootstrapRecheck_ListError_NoRecreate: if ContainerList fails,
// recheck must not panic and must not recreate anything.
func TestBootstrapRecheck_ListError_NoRecreate(t *testing.T) {
	ops := newFakeOps()
	ops.listErr = errors.New("daemon down")
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{"x": []string{"y"}}
	w.bootstrapRecheck(context.Background(), "test")
	if len(ops.recreateCalls) != 0 {
		t.Errorf("list error must not recreate")
	}
}

// ---- handleStart -----------------------------------------------------------

func TestHandleStart_TrackedTargetTriggersRecreate(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{
		"dovecot-mailcow": []string{"anchor-1", "anchor-2"},
	}
	w.handleStart(context.Background(), EventMsg{
		Action:        "start",
		ContainerName: "dovecot-mailcow",
		Project:       "ix-mailcow",
	})
	if ops.recreateCountForAnchor("anchor-1") != 1 || ops.recreateCountForAnchor("anchor-2") != 1 {
		t.Errorf("both mapped anchors must be recreated, got %v", ops.recreateCalls)
	}
	for _, c := range ops.recreateCalls {
		if c.newNetMode != "container:dovecot-mailcow" {
			t.Errorf("recreate netmode should be `container:dovecot-mailcow`, got %q", c.newNetMode)
		}
	}
}

func TestHandleStart_UnrelatedNameIgnored(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{
		"dovecot-mailcow": []string{"anchor-1"},
	}
	w.handleStart(context.Background(), EventMsg{
		Action:        "start",
		ContainerName: "completely-unrelated-container",
		Project:       "other",
	})
	if len(ops.recreateCalls) != 0 {
		t.Errorf("unrelated start must be ignored, got %v", ops.recreateCalls)
	}
}

// TestHandleStart_OwnProjectSiblingTriggersReenum verifies Anmerkung 2
// from issue #13: a `start` event for a container in the SIDECAR'S
// OWN compose project re-enumerates the target pool.
func TestHandleStart_OwnProjectSiblingTriggersReenum(t *testing.T) {
	ops := newFakeOps()
	target := ContainerInfo{ID: "live-target-id", Names: []string{"/dovecot-mailcow"}}
	ops.containers = []ContainerInfo{
		target,
		{
			ID:          "anchor-NEW",
			NetworkMode: "container:live-target-id",
			Labels:      map[string]string{"com.docker.compose.project": "ws"},
		},
	}
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{"dovecot-mailcow": []string{"anchor-OLD"}}

	w.handleStart(context.Background(), EventMsg{
		Action:        "start",
		ContainerName: "some-sibling-name",
		Project:       "ws",
	})

	got := w.lookupAnchors("dovecot-mailcow")
	if len(got) != 1 || got[0] != "anchor-NEW" {
		t.Errorf("after own-project sibling start, pool should hold the new ID; got %v", got)
	}
}

// ---- rapid-restart suppression --------------------------------------------

// TestRecreateAnchor_RapidRepeatSuppressed: dedup is keyed by
// (target, anchor) — back-to-back start events for the same target
// only fire one recreate.
func TestRecreateAnchor_RapidRepeatSuppressed(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.recreateAnchor(context.Background(), "anchor-1", "target-X", "first")
	w.recreateAnchor(context.Background(), "anchor-1", "target-X", "second-rapid")
	if got := ops.recreateCountForAnchor("anchor-1"); got != 1 {
		t.Errorf("expected 1 recreate (second suppressed), got %d", got)
	}
}

func TestRecreateAnchor_AfterWindowAllowed(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.recreateAnchor(context.Background(), "anchor-1", "target-X", "first")
	// Roll the clock back so the second call is "more than a window" later.
	w.mu.Lock()
	for k := range w.lastRestartedAt {
		w.lastRestartedAt[k] = time.Now().Add(-2 * rapidRestartWindow)
	}
	w.mu.Unlock()
	w.recreateAnchor(context.Background(), "anchor-1", "target-X", "second-after-window")
	if got := ops.recreateCountForAnchor("anchor-1"); got != 2 {
		t.Errorf("expected 2 recreates (second after window), got %d", got)
	}
}

func TestRecreateAnchor_DifferentAnchorsNotSuppressed(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.recreateAnchor(context.Background(), "anchor-A", "target", "")
	w.recreateAnchor(context.Background(), "anchor-B", "target", "")
	if ops.recreateCountForAnchor("anchor-A") != 1 || ops.recreateCountForAnchor("anchor-B") != 1 {
		t.Errorf("dedup must be per anchor, got %v", ops.recreateCalls)
	}
}

// ---- consume ---------------------------------------------------------------

func TestConsume_ReturnsOnErrChannel(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{
		SelfProject:    "ws",
		PollInterval:   time.Hour,
		RestartTimeout: time.Second,
	}, "self")
	wantErr := errors.New("daemon died")
	ops.errsCh <- wantErr

	pollT := time.NewTicker(time.Hour)
	defer pollT.Stop()

	got := w.consume(context.Background(), ops.msgsCh, ops.errsCh, pollT)
	if !errors.Is(got, wantErr) {
		t.Errorf("consume returned %v, want %v", got, wantErr)
	}
}

func TestConsume_ReturnsOnMsgChannelClosed(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{
		SelfProject:    "ws",
		PollInterval:   time.Hour,
		RestartTimeout: time.Second,
	}, "self")
	close(ops.msgsCh)

	pollT := time.NewTicker(time.Hour)
	defer pollT.Stop()

	got := w.consume(context.Background(), ops.msgsCh, ops.errsCh, pollT)
	if got == nil || !strings.Contains(got.Error(), "message channel closed") {
		t.Errorf("expected message-channel-closed error, got %v", got)
	}
}

func TestConsume_IgnoresNonStartActions(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.pool = targetPool{"dovecot-mailcow": []string{"anchor-1"}}

	pollT := time.NewTicker(time.Hour)
	defer pollT.Stop()

	go func() {
		ops.msgsCh <- EventMsg{Action: "destroy", ContainerName: "dovecot-mailcow"}
		ops.msgsCh <- EventMsg{Action: "die", ContainerName: "dovecot-mailcow"}
		time.Sleep(20 * time.Millisecond)
		close(ops.msgsCh)
	}()
	_ = w.consume(context.Background(), ops.msgsCh, ops.errsCh, pollT)
	if len(ops.recreateCalls) != 0 {
		t.Errorf("non-start actions must not recreate, got %v", ops.recreateCalls)
	}
}

// ---- Run lifecycle ---------------------------------------------------------

func TestRun_ExitsOnContextCancel(t *testing.T) {
	ops := newFakeOps()
	w := newWithOps(ops, &config.WrapRebinder{
		SelfProject:    "ws",
		PollInterval:   time.Hour,
		RestartTimeout: time.Second,
	}, "self")

	ctx, cancel := context.WithCancel(context.Background())
	var done atomic.Bool
	go func() {
		_ = w.Run(ctx)
		done.Store(true)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
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

// ---- end-to-end pool-then-event ------------------------------------------

func TestEndToEnd_PoolEnumerationThenTargetStart(t *testing.T) {
	ops := newFakeOps()
	nginxTarget := ContainerInfo{ID: "nginx-target-id", Names: []string{"/nginx-mailcow"}}
	dovecotTarget := ContainerInfo{ID: "dovecot-target-id", Names: []string{"/dovecot-mailcow"}}
	ops.containers = []ContainerInfo{
		nginxTarget,
		dovecotTarget,
		{
			ID:          "nginx-anchor",
			NetworkMode: "container:nginx-target-id",
			Labels:      map[string]string{"com.docker.compose.project": "ws"},
		},
		{
			ID:          "dovecot-anchor",
			NetworkMode: "container:dovecot-target-id",
			Labels:      map[string]string{"com.docker.compose.project": "ws"},
		},
	}
	w := newWithOps(ops, &config.WrapRebinder{SelfProject: "ws", RestartTimeout: time.Second}, "self")
	w.enumerateTargetPool(context.Background())

	got := fmt.Sprintf("nginx=%v|dovecot=%v",
		w.lookupAnchors("nginx-mailcow"),
		w.lookupAnchors("dovecot-mailcow"))
	want := "nginx=[nginx-anchor]|dovecot=[dovecot-anchor]"
	if got != want {
		t.Errorf("pool wrong: %s, want %s", got, want)
	}

	// Now simulate dovecot-mailcow starting (recreated).
	w.handleStart(context.Background(), EventMsg{
		Action:        "start",
		ContainerName: "dovecot-mailcow",
		Project:       "ix-mailcow",
	})
	if ops.recreateCountForAnchor("dovecot-anchor") != 1 {
		t.Errorf("dovecot-anchor not recreated")
	}
	if ops.recreateCountForAnchor("nginx-anchor") != 0 {
		t.Errorf("nginx-anchor must not be touched by dovecot start, got %v", ops.recreateCalls)
	}
}
