package autostart

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---- matcher tests ---------------------------------------------------------

func TestMatchSiblings_LongIDMatch(t *testing.T) {
	target := ContainerInfo{
		ID:    "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Names: []string{"/tgt-test"},
		State: "running",
	}
	sib := ContainerInfo{
		ID:          "11111111111111111111111111111111",
		State:       "created",
		NetworkMode: "container:" + target.ID,
	}
	got := matchSiblings([]ContainerInfo{target, sib}, target)
	if len(got) != 1 || got[0] != sib.ID {
		t.Errorf("long-ID match: got %v want [%s]", got, sib.ID)
	}
}

func TestMatchSiblings_ShortIDMatch(t *testing.T) {
	target := ContainerInfo{
		ID:    "abcdef012345fedcba0987654321deadbeefcafebabe1234abcdef0123456789",
		Names: []string{"/tgt-test"},
		State: "running",
	}
	sib := ContainerInfo{
		ID:          "ffffffffffffffffffffffffffffffff",
		State:       "created",
		NetworkMode: "container:" + target.ID[:12], // short ID — the form docker often stores
	}
	got := matchSiblings([]ContainerInfo{target, sib}, target)
	if len(got) != 1 {
		t.Errorf("short-ID match: got %v", got)
	}
}

func TestMatchSiblings_NameMatch(t *testing.T) {
	target := ContainerInfo{
		ID:    "abc12345abcd",
		Names: []string{"/tgt-test"},
		State: "running",
	}
	sib := ContainerInfo{
		ID:          "dddddddddddddddddddddddddddddddd",
		State:       "created",
		NetworkMode: "container:tgt-test",
	}
	got := matchSiblings([]ContainerInfo{target, sib}, target)
	if len(got) != 1 {
		t.Errorf("name match: got %v", got)
	}
}

// F-43 acceptance: only Created-state containers are candidates. A
// running sibling whose NetworkMode points at the target must NOT
// produce a redundant start call.
func TestMatchSiblings_IgnoresNonCreated(t *testing.T) {
	target := ContainerInfo{ID: "abcdef012345", Names: []string{"/tgt"}, State: "running"}
	all := []ContainerInfo{
		target,
		{ID: "sib-running", State: "running", NetworkMode: "container:tgt"},
		{ID: "sib-exited", State: "exited", NetworkMode: "container:tgt"},
		{ID: "sib-restarting", State: "restarting", NetworkMode: "container:tgt"},
		{ID: "sib-created", State: "created", NetworkMode: "container:tgt"},
	}
	got := matchSiblings(all, target)
	if len(got) != 1 || got[0] != "sib-created" {
		t.Errorf("only-Created policy: got %v", got)
	}
}

// F-43: containers with other NetworkMode shapes (host, none, bridge,
// container:OTHER) must not match.
func TestMatchSiblings_IgnoresUnrelatedNetworkModes(t *testing.T) {
	target := ContainerInfo{ID: "abcdef012345", Names: []string{"/tgt"}, State: "running"}
	all := []ContainerInfo{
		target,
		{ID: "host-mode", State: "created", NetworkMode: "host"},
		{ID: "none-mode", State: "created", NetworkMode: "none"},
		{ID: "bridge-mode", State: "created", NetworkMode: "bridge"},
		{ID: "container-other", State: "created", NetworkMode: "container:someone-else"},
		{ID: "no-network-mode", State: "created", NetworkMode: ""},
		{ID: "real-match", State: "created", NetworkMode: "container:tgt"},
	}
	got := matchSiblings(all, target)
	sort.Strings(got)
	if len(got) != 1 || got[0] != "real-match" {
		t.Errorf("network-mode filter: got %v want [real-match]", got)
	}
}

// F-43: leading "/" on Docker container names doesn't trip the
// matcher. Docker's `Names` field consistently includes the prefix.
func TestMatchSiblings_LeadingSlashTolerated(t *testing.T) {
	target := ContainerInfo{ID: "abc", Names: []string{"/tgt"}, State: "running"}
	all := []ContainerInfo{
		target,
		// Some Docker versions store NetworkMode with a leading slash too.
		{ID: "sib", State: "created", NetworkMode: "container:/tgt"},
	}
	if got := matchSiblings(all, target); len(got) != 1 {
		t.Errorf("leading-slash tolerance: got %v", got)
	}
}

func TestMatchSiblings_EmptyTargetReturnsNil(t *testing.T) {
	got := matchSiblings([]ContainerInfo{
		{ID: "sib", State: "created", NetworkMode: "container:tgt"},
	}, ContainerInfo{})
	if got != nil {
		t.Errorf("empty target should match nothing, got %v", got)
	}
}

// F-43 lifecycle invariant: multiple Created siblings all pointing at
// the same target — all must fire.
func TestMatchSiblings_MultipleSiblingsAllFire(t *testing.T) {
	target := ContainerInfo{ID: "tgt-id-abcdef", Names: []string{"/tgt"}, State: "running"}
	all := []ContainerInfo{
		target,
		{ID: "sib-a", State: "created", NetworkMode: "container:tgt"},
		{ID: "sib-b", State: "created", NetworkMode: "container:tgt-id-abcdef"},
		{ID: "sib-c", State: "created", NetworkMode: "container:tgt"},
	}
	got := matchSiblings(all, target)
	sort.Strings(got)
	want := []string{"sib-a", "sib-b", "sib-c"}
	if len(got) != 3 || !sliceEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestReferencesFor_IncludesShortAndLongID(t *testing.T) {
	c := ContainerInfo{
		ID:    "0123456789ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef01234567",
		Names: []string{"/foo", "/bar"},
	}
	refs := referencesFor(c)
	for _, want := range []string{c.ID, c.ID[:12], "foo", "bar"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("missing reference %q in %v", want, refs)
		}
	}
}

// ---- Watcher tests (with fake dockerOps) ----------------------------------

// fakeOps is a recording, scriptable dockerOps for unit tests.
type fakeOps struct {
	mu sync.Mutex

	listResults []ContainerInfo
	listErr     error
	listCalls   int

	startedIDs []string
	startErr   map[string]error
	startCalls int

	eventCh chan EventMsg
	errCh   chan error
}

func newFakeOps(listResults []ContainerInfo) *fakeOps {
	return &fakeOps{
		listResults: listResults,
		startErr:    map[string]error{},
		eventCh:     make(chan EventMsg, 8),
		errCh:       make(chan error, 1),
	}
}

func (f *fakeOps) List(_ context.Context) ([]ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]ContainerInfo, len(f.listResults))
	copy(out, f.listResults)
	return out, nil
}

func (f *fakeOps) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if err, ok := f.startErr[id]; ok {
		return err
	}
	f.startedIDs = append(f.startedIDs, id)
	return nil
}

func (f *fakeOps) Events(_ context.Context) (<-chan EventMsg, <-chan error) {
	return f.eventCh, f.errCh
}

func (f *fakeOps) starts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.startedIDs))
	copy(out, f.startedIDs)
	return out
}

// TestBackfill_StartsStrandedCreatedSibling covers the
// anchord-restarted-mid-deploy case: a Created-state sibling waiting
// on an already-running target. backfill must catch it without
// needing an incoming event.
func TestBackfill_StartsStrandedCreatedSibling(t *testing.T) {
	target := ContainerInfo{ID: "tgt-abc", Names: []string{"/tgt"}, State: "running"}
	sib := ContainerInfo{ID: "sib-xyz", State: "created", NetworkMode: "container:tgt"}
	ops := newFakeOps([]ContainerInfo{target, sib})

	w := newWithOps(ops)
	w.backfill(context.Background())

	starts := ops.starts()
	if len(starts) != 1 || starts[0] != "sib-xyz" {
		t.Errorf("backfill should have started sib-xyz, got starts=%v", starts)
	}
}

func TestBackfill_NoStrandedSiblings_NoOp(t *testing.T) {
	target := ContainerInfo{ID: "tgt-abc", Names: []string{"/tgt"}, State: "running"}
	ops := newFakeOps([]ContainerInfo{target})

	w := newWithOps(ops)
	w.backfill(context.Background())

	if got := ops.starts(); len(got) != 0 {
		t.Errorf("no siblings to start, got starts=%v", got)
	}
}

// F-43 acceptance: target startup arrives as an event, watcher starts
// the sibling.
func TestRun_EventTriggersSiblingStart(t *testing.T) {
	target := ContainerInfo{ID: "tgt-abc", Names: []string{"/tgt"}, State: "running"}
	sib := ContainerInfo{ID: "sib-xyz", State: "created", NetworkMode: "container:tgt"}
	// First call (backfill): target not running yet — empty list.
	// Second call (per-event re-scan): target running, sibling visible.
	ops := newFakeOps(nil)
	w := newWithOps(ops)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	// Wait for backfill (one List call) before flipping state.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		ops.mu.Lock()
		seen := ops.listCalls
		ops.mu.Unlock()
		if seen >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Now the target is "running" and the sibling is visible.
	ops.mu.Lock()
	ops.listResults = []ContainerInfo{target, sib}
	ops.mu.Unlock()

	// Fire the start event.
	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "tgt"}

	// Wait for the start call.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ops.starts()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	got := ops.starts()
	if len(got) != 1 || got[0] != "sib-xyz" {
		t.Errorf("expected start of sib-xyz, got %v", got)
	}

	cancel()
	<-done
}

// F-43: non-start events are ignored — die/destroy/restart/etc must
// not trigger sibling-start calls.
func TestRun_IgnoresNonStartEvents(t *testing.T) {
	target := ContainerInfo{ID: "tgt-abc", Names: []string{"/tgt"}, State: "running"}
	sib := ContainerInfo{ID: "sib-xyz", State: "created", NetworkMode: "container:tgt"}
	ops := newFakeOps([]ContainerInfo{target, sib})
	// Pre-list returns target+sib — backfill will start the sibling
	// once; the subsequent ignored events must NOT add to that count.

	w := newWithOps(ops)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	// Backfill should have started sib-xyz; wait for it.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && len(ops.starts()) < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	startsAfterBackfill := len(ops.starts())

	// Now emit a bunch of non-"start" events. None should cause a
	// re-start of sib-xyz.
	for _, action := range []string{"die", "destroy", "create", "stop", "kill", "restart", "pause"} {
		ops.eventCh <- EventMsg{Action: action, ActorID: target.ID, ActorName: "tgt"}
	}

	// Give the loop a beat to process and (hopefully) not act.
	time.Sleep(50 * time.Millisecond)
	if got := len(ops.starts()); got != startsAfterBackfill {
		t.Errorf("non-start events caused %d extra starts", got-startsAfterBackfill)
	}

	cancel()
	<-done
}

// F-43 idempotency: docker.Start returning an error must not abort
// the watcher; it logs and continues. The error path covers both real
// failures and the "already started" race.
func TestRun_StartFailureIsLoggedButLoopContinues(t *testing.T) {
	target := ContainerInfo{ID: "tgt-abc", Names: []string{"/tgt"}, State: "running"}
	sib := ContainerInfo{ID: "sib-fail", State: "created", NetworkMode: "container:tgt"}
	ops := newFakeOps([]ContainerInfo{target, sib})
	ops.startErr["sib-fail"] = errors.New("no such container: sib-fail")

	w := newWithOps(ops)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	// Send a start event. Watcher should try the start, fail, and
	// continue running.
	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "tgt"}

	// Give time for the failing start attempt.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		ops.mu.Lock()
		calls := ops.startCalls
		ops.mu.Unlock()
		if calls >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ops.mu.Lock()
	calls := ops.startCalls
	ops.mu.Unlock()
	if calls < 1 {
		t.Error("watcher gave up before attempting the start")
	}
	if len(ops.starts()) != 0 {
		t.Errorf("successful starts list should be empty (startErr injected); got %v", ops.starts())
	}

	cancel()
	<-done
}

// F-43 backwards-compat: AutostartSiblings=false means no Watcher
// runs. Trivially covered by main.go gating the Run() call. Not a
// unit-test concern here, but we sanity-check newWithOps returns a
// non-nil watcher (regression guard against constructor edits).
func TestNew_NotNil(t *testing.T) {
	w := newWithOps(newFakeOps(nil))
	if w == nil {
		t.Fatal("constructor returned nil")
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
