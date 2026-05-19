package autostart

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"
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

	created     []CreateSpec // F-45: every CreateSpec the watcher built
	createErr   error
	createNewID string // ID returned from Create (default "managed-sa-new")

	selfInfo  SelfInfo
	selfErr   error
	selfCalls int

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

// F-45: Create records the CreateSpec and returns either the injected
// error or createNewID (default "managed-sa-new"). Models Docker's
// name-conflict idempotency: a second Create with the same Name as a
// previously-successful one returns an error — same shape the
// production code sees when backfill and an inbound event race.
func (f *fakeOps) Create(_ context.Context, spec CreateSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	for _, prior := range f.created {
		if prior.Name == spec.Name {
			return "", errors.New("Conflict. The container name is already in use")
		}
	}
	f.created = append(f.created, spec)
	id := f.createNewID
	if id == "" {
		id = "managed-sa-new"
	}
	return id, nil
}

func (f *fakeOps) InspectSelf(_ context.Context) (SelfInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selfCalls++
	if f.selfErr != nil {
		return SelfInfo{}, f.selfErr
	}
	return f.selfInfo, nil
}

func (f *fakeOps) createdSpecs() []CreateSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CreateSpec, len(f.created))
	copy(out, f.created)
	return out
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

// ---- F-45 managed service-anchor tests ------------------------------------

func managedRecipe(target string) config.ManagedSARecipe {
	return config.ManagedSARecipe{
		Target:    target,
		Name:      target + "-service-anchor",
		Image:     "",          // default to self.Image at runtime
		GatewayIP: "",          // default to self IP on sharedNet at runtime
		ExtraEnv:  map[string]string{},
	}
}

// F-45 acceptance: target appears, managed service-anchor doesn't
// exist yet → watcher inspects self for defaults, creates the SA
// container with the right spec, then starts it.
func TestRun_F45_CreatesAndStartsWhenSAAbsent(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	ops := newFakeOps(nil)
	ops.selfInfo = SelfInfo{
		Image: "ghcr.io/example/anchord:test",
		IPsByNetwork: map[string]string{
			"ix-authentik_transit": "172.31.80.181",
		},
	}
	ops.createNewID = "managed-sa-fresh"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "ix-authentik_transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Make sure backfill has been seen before firing the event.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		ops.mu.Lock()
		c := ops.listCalls
		ops.mu.Unlock()
		if c >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// After backfill: target shows up in the next list call.
	ops.mu.Lock()
	ops.listResults = []ContainerInfo{target}
	ops.mu.Unlock()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}

	// Wait for the Create + Start.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ops.createdSpecs()) >= 1 && len(ops.starts()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 Create call, got %d (%#v)", len(specs), specs)
	}
	got := specs[0]
	if got.Name != "ak-outpost-ldap-service-anchor" {
		t.Errorf("Name: got %q want ak-outpost-ldap-service-anchor", got.Name)
	}
	if got.Image != "ghcr.io/example/anchord:test" {
		t.Errorf("Image should default to self.Image, got %q", got.Image)
	}
	if got.NetworkMode != "container:ak-outpost-ldap" {
		t.Errorf("NetworkMode: got %q", got.NetworkMode)
	}
	if got.Restart != "unless-stopped" {
		t.Errorf("Restart: got %q", got.Restart)
	}
	// Issue #2: managed SA must NOT carry compose.* labels — F-45
	// containers are out-of-band w.r.t. compose and a half-stamped
	// `project` (without `service`) crashes orchestrators.
	for k := range got.Labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			t.Errorf("managed SA must not carry compose.* labels, got %q in %v", k, got.Labels)
		}
	}
	if got.Labels["anchord.managed-by"] != "f45" {
		t.Errorf("anchord.managed-by=f45 label missing, got %v", got.Labels)
	}
	// Env must include the gateway IP from self.IPsByNetwork[sharedNet].
	envHas := func(key, val string) bool {
		for _, e := range got.Env {
			if e == key+"="+val {
				return true
			}
		}
		return false
	}
	if !envHas("ANCHORD_MODE", "service-anchor") {
		t.Errorf("missing ANCHORD_MODE in env: %v", got.Env)
	}
	if !envHas("ANCHORD_GATEWAY_IP", "172.31.80.181") {
		t.Errorf("GatewayIP should default to self IP on shared net, got env=%v", got.Env)
	}

	// And Start was called on the returned ID.
	starts := ops.starts()
	if len(starts) != 1 || starts[0] != "managed-sa-fresh" {
		t.Errorf("expected Start of managed-sa-fresh, got %v", starts)
	}

	cancel()
	<-done
}

// F-45: managed SA already exists and is running → no create, no
// start (already up).
func TestRun_F45_NoOpWhenManagedSAAlreadyRunning(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	existing := ContainerInfo{
		ID:    "managed-sa-existing",
		Names: []string{"/ak-outpost-ldap-service-anchor"},
		State: "running",
		NetworkMode: "container:tgt-abc",
	}
	ops := newFakeOps([]ContainerInfo{target, existing})
	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "ix-authentik_transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}
	time.Sleep(80 * time.Millisecond)

	if c := len(ops.createdSpecs()); c != 0 {
		t.Errorf("must not create when managed SA already running, got %d Create calls", c)
	}

	cancel()
	<-done
}

// F-45: managed SA exists but is in Created state → F-43 handles the
// Start via matchSiblings; F-45 should NOT issue a second Create.
func TestRun_F45_SkipsCreateWhenSAInCreatedState(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	pending := ContainerInfo{
		ID:          "managed-sa-pending",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "created",
		NetworkMode: "container:ak-outpost-ldap",
	}
	ops := newFakeOps([]ContainerInfo{target, pending})
	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "ix-authentik_transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Wait for the backfill to fire (matchSiblings should start the
	// pending one — F-43 path).
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && len(ops.starts()) < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	if c := len(ops.createdSpecs()); c != 0 {
		t.Errorf("must not re-create a Created-state SA, got %d Create calls", c)
	}
	starts := ops.starts()
	if len(starts) != 1 || starts[0] != "managed-sa-pending" {
		t.Errorf("expected single F-43 start on managed-sa-pending, got %v", starts)
	}

	cancel()
	<-done
}

// F-45: target doesn't match the recipe's configured target → no
// create. Critical because the watcher sees ALL container start
// events, including unrelated ones.
func TestRun_F45_IgnoresUnrelatedTargets(t *testing.T) {
	ops := newFakeOps([]ContainerInfo{
		{ID: "unrelated", Names: []string{"/some-other-container"}, State: "running"},
	})
	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "ix-authentik_transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: "unrelated", ActorName: "some-other-container"}
	time.Sleep(80 * time.Millisecond)

	if c := len(ops.createdSpecs()); c != 0 {
		t.Errorf("watcher created a service-anchor for an unrelated start, got %d", c)
	}

	cancel()
	<-done
}

// F-45: when the recipe is INACTIVE (Target empty), the watcher
// behaves exactly as pure F-43 — no Create calls, only existing-
// Created-state Start.
func TestRun_F45_InactiveRecipeFallsBackToF43(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/tgt"},
		State: "running",
	}
	sib := ContainerInfo{
		ID:          "sib",
		State:       "created",
		NetworkMode: "container:tgt",
	}
	ops := newFakeOps([]ContainerInfo{target, sib})
	w := newWithOpsAndRecipe(ops, config.ManagedSARecipe{}, "") // inactive

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && len(ops.starts()) < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	if c := len(ops.createdSpecs()); c != 0 {
		t.Errorf("inactive recipe must NEVER call Create, got %d", c)
	}
	if len(ops.starts()) != 1 || ops.starts()[0] != "sib" {
		t.Errorf("inactive recipe must still F-43-start created siblings; got %v", ops.starts())
	}

	cancel()
	<-done
}

// F-45: buildSpec error path — GatewayIP empty AND sharedNet empty
// → cannot resolve default. Logged + skip; no Create call.
func TestRun_F45_NoSharedNetYetSkipsCreate(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	ops := newFakeOps([]ContainerInfo{target})
	ops.selfInfo = SelfInfo{Image: "anchord:test"}
	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "") // no shared net yet

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}
	time.Sleep(80 * time.Millisecond)

	if c := len(ops.createdSpecs()); c != 0 {
		t.Errorf("expected no Create call when sharedNet is unknown; got %d", c)
	}

	cancel()
	<-done
}

// Issue #1 followup: the F-44 picker settles asynchronously during
// the first discovery reconcile. The watcher must read the picker's
// choice *each time* it needs it — not snapshot a startup-time empty
// string. We simulate this by handing the watcher a function that
// returns "" first and "transit" second, and verify the second
// event-driven create succeeds.
func TestRun_F45_SharedNetworkLookupIsLazy(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	ops := newFakeOps([]ContainerInfo{target})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}

	// Picker emulation: returns "" until `settled` flips, then
	// returns the real net. Mirrors sharednet.Picker.Chosen, which
	// returns "" until the first reconcile with a real backend.
	var settled atomic.Bool
	picker := func() string {
		if !settled.Load() {
			return ""
		}
		return "transit"
	}

	w := newWithOps(ops)
	w.recipe = managedRecipe("ak-outpost-ldap")
	w.SetSharedNetworkFunc(picker)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Drain backfill + a first event while the picker is still
	// unsettled. No Create call must happen.
	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}
	time.Sleep(80 * time.Millisecond)
	if c := len(ops.createdSpecs()); c != 0 {
		t.Fatalf("create must be skipped while picker is unsettled; got %d", c)
	}

	// Picker settles. The watcher already saw the event(s), so a
	// fresh event is needed to retry — same shape as production
	// (event-driven, not poll-driven).
	settled.Store(true)
	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}
	time.Sleep(80 * time.Millisecond)
	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("after picker settles, create must run on the next event; got %d", len(specs))
	}
	hasGW := false
	for _, e := range specs[0].Env {
		if e == "ANCHORD_GATEWAY_IP=10.0.0.5" {
			hasGW = true
			break
		}
	}
	if !hasGW {
		t.Errorf("ANCHORD_GATEWAY_IP=10.0.0.5 expected (self IP on the picker's net), env=%v", specs[0].Env)
	}

	cancel()
	<-done
}

// F-45: operator-supplied GatewayIP overrides the self-IP default.
func TestRun_F45_ExplicitGatewayIPWinsOverSelfIP(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	ops := newFakeOps([]ContainerInfo{target})
	ops.selfInfo = SelfInfo{
		Image: "anchord:test",
		IPsByNetwork: map[string]string{
			"transit": "10.0.0.5", // would be the default
		},
	}

	recipe := managedRecipe("ak-outpost-ldap")
	recipe.GatewayIP = "192.0.2.99" // explicit override
	w := newWithOpsAndRecipe(ops, recipe, "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && len(ops.createdSpecs()) < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 Create, got %d", len(specs))
	}
	envHas := func(key, val string) bool {
		for _, e := range specs[0].Env {
			if e == key+"="+val {
				return true
			}
		}
		return false
	}
	if !envHas("ANCHORD_GATEWAY_IP", "192.0.2.99") {
		t.Errorf("explicit GatewayIP not honoured, env=%v", specs[0].Env)
	}

	cancel()
	<-done
}

// F-45: ExtraEnv pairs make it into the spec's Env in deterministic
// sorted-by-key order; collisions with the standard ANCHORD_* env
// are tolerated (operator override wins).
func TestRun_F45_ExtraEnvAndDeterministicOrder(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	ops := newFakeOps([]ContainerInfo{target})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}

	recipe := managedRecipe("ak-outpost-ldap")
	recipe.ExtraEnv = map[string]string{
		"AUTHENTIK_TOKEN":   "secret",
		"AUTHENTIK_HOST":    "https://authentik.example",
		"ANCHORD_LOG_LEVEL": "debug", // override the default
	}
	w := newWithOpsAndRecipe(ops, recipe, "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && len(ops.createdSpecs()) < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 Create, got %d", len(specs))
	}
	env := specs[0].Env

	// Sorted-by-key invariant — env must be in ascending order.
	for i := 1; i < len(env); i++ {
		if env[i-1] >= env[i] {
			t.Errorf("env not sorted: %q >= %q (full env: %v)", env[i-1], env[i], env)
			break
		}
	}
	// Operator override of ANCHORD_LOG_LEVEL wins.
	for _, e := range env {
		if e == "ANCHORD_LOG_LEVEL=info" {
			t.Errorf("operator override of LOG_LEVEL lost; env=%v", env)
		}
	}
	want := map[string]bool{
		"ANCHORD_LOG_LEVEL=debug":             false,
		"AUTHENTIK_TOKEN=secret":              false,
		"AUTHENTIK_HOST=https://authentik.example": false,
	}
	for _, e := range env {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("env missing %q: %v", k, env)
		}
	}

	cancel()
	<-done
}

// F-45: Create failure must be logged and the loop must continue
// (same robustness contract as Start failures).
func TestRun_F45_CreateErrorTolerated(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	ops := newFakeOps([]ContainerInfo{target})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createErr = errors.New("Conflict. The container name is already in use")

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}
	time.Sleep(80 * time.Millisecond)

	// Loop must still be alive (we'll just cancel cleanly).
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher did not exit after Create failure")
	}
}

func TestTargetMatchesRecipe(t *testing.T) {
	target := ContainerInfo{
		ID:    "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	cases := []struct {
		recipeTarget string
		want         bool
	}{
		{"ak-outpost-ldap", true},
		{"abcdef012345", true},                                                          // short ID
		{"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true},     // long ID
		{"/ak-outpost-ldap", true},                                                       // leading slash tolerated
		{"some-other-container", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.recipeTarget, func(t *testing.T) {
			if got := targetMatchesRecipe(target, tc.recipeTarget); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
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
