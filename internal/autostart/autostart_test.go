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

	removedIDs []string
	removeErr  map[string]error

	created     []CreateSpec // F-45: every CreateSpec the watcher built
	createErr   error
	createNewID string // ID returned from Create (default "managed-sa-new")

	selfInfo  SelfInfo
	selfErr   error
	selfCalls int

	// Issue #10 dep-rebind path. recreates records every
	// RecreateWithNetworkMode call as a (id, newNetMode, newID)
	// triple. recreateErr lets tests inject failures keyed by the
	// container ID passed in.
	recreates    []recreateCall
	recreateErr  map[string]error
	recreateNewID map[string]string // optional ID-mapping for the recreate result; default = "<id>-recreated"

	eventCh chan EventMsg
	errCh   chan error
}

type recreateCall struct {
	OldID      string
	NewNetMode string
	NewID      string
}

func newFakeOps(listResults []ContainerInfo) *fakeOps {
	return &fakeOps{
		listResults:   listResults,
		startErr:      map[string]error{},
		removeErr:     map[string]error{},
		recreateErr:   map[string]error{},
		recreateNewID: map[string]string{},
		eventCh:       make(chan EventMsg, 8),
		errCh:         make(chan error, 1),
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

func (f *fakeOps) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.removeErr[id]; ok {
		return err
	}
	f.removedIDs = append(f.removedIDs, id)
	// Mirror docker's force-remove semantics: drop the container from
	// future List results so a subsequent Create with the same name
	// doesn't trip the conflict guard.
	kept := f.listResults[:0]
	for _, c := range f.listResults {
		if c.ID != id {
			kept = append(kept, c)
		}
	}
	f.listResults = kept
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

func (f *fakeOps) removes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.removedIDs))
	copy(out, f.removedIDs)
	return out
}

// RecreateWithNetworkMode mirrors Remove + Create + Start in one call.
// Both successful and failed attempts are recorded so tests can
// assert that a per-dep failure didn't abort the rebind loop. On
// success it also updates listResults: same name, new ID, new
// netmode (mirrors docker's name-conflict-free recreate semantics).
func (f *fakeOps) RecreateWithNetworkMode(_ context.Context, id, newNetMode string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.recreateErr[id]; ok {
		f.recreates = append(f.recreates, recreateCall{OldID: id, NewNetMode: newNetMode, NewID: ""})
		return "", err
	}
	newID, ok := f.recreateNewID[id]
	if !ok {
		newID = id + "-recreated"
	}
	f.recreates = append(f.recreates, recreateCall{OldID: id, NewNetMode: newNetMode, NewID: newID})
	for i, c := range f.listResults {
		if c.ID == id {
			f.listResults[i].ID = newID
			f.listResults[i].NetworkMode = newNetMode
			break
		}
	}
	return newID, nil
}

func (f *fakeOps) recreateCalls() []recreateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recreateCall, len(f.recreates))
	copy(out, f.recreates)
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

// Issue #3 / F-45: operator-supplied recipe.Labels must reach the
// CreateSpec so F-42 selector mode can discover its own F-45 spawn.
// The built-in anchord.managed-by=f45 always wins over an operator
// attempt to override (defence-in-depth — config already rejects it).
func TestRun_F45_OperatorLabelsReachSpec(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-nextcloud",
		Names: []string{"/nextcloud-aio-talk"},
		State: "running",
	}
	ops := newFakeOps([]ContainerInfo{target})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"nextcloud-aio": "172.16.1.2"},
	}
	ops.createNewID = "managed-sa-talk"

	recipe := managedRecipe("nextcloud-aio-talk")
	recipe.Labels = map[string]string{
		"anchord.identity":   "nextcloud-talk",
		"anchord.expose":     "tcp/3478:3478,udp/3478:3478",
		"anchord.managed-by": "operator-attempt", // must be overridden
	}
	w := newWithOpsAndRecipe(ops, recipe, "nextcloud-aio")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "nextcloud-aio-talk"}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ops.createdSpecs()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 Create, got %d", len(specs))
	}
	got := specs[0].Labels
	if got["anchord.identity"] != "nextcloud-talk" {
		t.Errorf("anchord.identity missing/wrong: %v", got)
	}
	if got["anchord.expose"] != "tcp/3478:3478,udp/3478:3478" {
		t.Errorf("anchord.expose missing/wrong: %v", got)
	}
	if got["anchord.managed-by"] != "f45" {
		t.Errorf("built-in anchord.managed-by=f45 must win over operator override, got %q", got["anchord.managed-by"])
	}

	cancel()
	<-done
}

// Issue #5: pure unit test for the stale-netns predicate.
func TestSATargetsStaleNetns(t *testing.T) {
	target := ContainerInfo{ID: "new-tgt-7d6c738d", Names: []string{"/ak-outpost-ldap"}}
	other := ContainerInfo{ID: "old-tgt-0f98a101", Names: []string{"/old-ak-outpost-ldap"}}
	cases := []struct {
		name      string
		saNetMode string
		all       []ContainerInfo
		want      bool
	}{
		{
			name:      "ref resolves to current target by full ID",
			saNetMode: "container:new-tgt-7d6c738d",
			all:       []ContainerInfo{target},
			want:      false,
		},
		{
			name:      "ref resolves to current target by name",
			saNetMode: "container:ak-outpost-ldap",
			all:       []ContainerInfo{target},
			want:      false,
		},
		{
			name:      "ref resolves to a different (still-listed) container",
			saNetMode: "container:old-tgt-0f98a101",
			all:       []ContainerInfo{target, other},
			want:      true,
		},
		{
			name:      "ref doesn't resolve at all (dead netns)",
			saNetMode: "container:old-tgt-0f98a101",
			all:       []ContainerInfo{target},
			want:      true,
		},
		{
			name:      "ref is a 12-char short-ID prefix of the current target",
			saNetMode: "container:new-tgt-7d6c",
			all:       []ContainerInfo{target},
			want:      false,
		},
		{
			name:      "non-container netmode is not our concern",
			saNetMode: "bridge",
			all:       []ContainerInfo{target},
			want:      false,
		},
		{
			name:      "empty netmode tolerated",
			saNetMode: "",
			all:       []ContainerInfo{target},
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sa := ContainerInfo{ID: "sa-id", Names: []string{"/ldap-service-anchor"}, NetworkMode: tc.saNetMode}
			if got := saTargetsStaleNetns(sa, target, tc.all); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

// Issue #5: when an outside orchestrator recreates the F-45 target,
// the next start-event must cause anchord to force-remove the stale
// SA and create a new one bound to the live target's netns.
func TestRun_F45_RecreatesSAOnStaleNetns(t *testing.T) {
	newTarget := ContainerInfo{
		ID:    "new-tgt-7d6c738d",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	staleSA := ContainerInfo{
		ID:          "stale-sa-id",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "running",
		NetworkMode: "container:old-tgt-0f98a101", // dead reference
	}
	ops := newFakeOps([]ContainerInfo{newTarget, staleSA})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "fresh-sa-id"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: newTarget.ID, ActorName: "ak-outpost-ldap"}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ops.removes()) >= 1 && len(ops.createdSpecs()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if rm := ops.removes(); len(rm) != 1 || rm[0] != "stale-sa-id" {
		t.Errorf("expected stale SA to be removed exactly once, got %v", rm)
	}
	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 Create after stale-netns recreate, got %d", len(specs))
	}
	if specs[0].NetworkMode != "container:ak-outpost-ldap" {
		t.Errorf("recreate must bind to current target name, got %q", specs[0].NetworkMode)
	}

	cancel()
	<-done
}

// Issue #5 inverse: when the SA's netns reference IS the current
// target, the watcher must NOT touch it. Guards against a regression
// where every start event causes a churning recreate.
func TestRun_F45_NoRecreateWhenSANetnsCurrent(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-current",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	freshSA := ContainerInfo{
		ID:          "sa-id",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "running",
		NetworkMode: "container:tgt-current",
	}
	ops := newFakeOps([]ContainerInfo{target, freshSA})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}
	time.Sleep(80 * time.Millisecond)

	if rm := ops.removes(); len(rm) != 0 {
		t.Errorf("healthy SA must not be removed, got %v", rm)
	}
	if specs := ops.createdSpecs(); len(specs) != 0 {
		t.Errorf("healthy SA must not be recreated, got %d", len(specs))
	}

	cancel()
	<-done
}

// Issue #8: when the managed SA is removed at runtime (e.g.
// `docker rm -f` during an image upgrade) while the target keeps
// running, the watcher must respawn the SA. Pre-fix the watcher
// only listened to "start" events, so this scenario was invisible.
func TestRun_F45_RecreatesSAOnDestroy(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	priorSA := ContainerInfo{
		ID:    "sa-prior",
		Names: []string{"/ak-outpost-ldap-service-anchor"},
		State: "running",
	}
	// State at fixture: both target and SA running.
	ops := newFakeOps([]ContainerInfo{target, priorSA})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "sa-fresh"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Wait for backfill (1 list call) so the watcher's startup pass
	// finishes before we mutate fixture state.
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

	// Simulate `docker rm -f`: SA disappears from the daemon's list,
	// then a destroy event fires.
	ops.mu.Lock()
	ops.listResults = []ContainerInfo{target}
	ops.mu.Unlock()
	ops.eventCh <- EventMsg{
		Action:    "destroy",
		ActorID:   priorSA.ID,
		ActorName: "ak-outpost-ldap-service-anchor",
	}

	// Watcher should re-list, find SA absent, and create + start a
	// fresh one against the still-running target.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ops.createdSpecs()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 Create after SA destroy, got %d (specs=%+v)", len(specs), specs)
	}
	if specs[0].Name != "ak-outpost-ldap-service-anchor" {
		t.Errorf("respawned SA name: got %q, want ak-outpost-ldap-service-anchor", specs[0].Name)
	}
	if specs[0].NetworkMode != "container:ak-outpost-ldap" {
		t.Errorf("respawned SA NetworkMode: got %q, want container:ak-outpost-ldap", specs[0].NetworkMode)
	}
	foundFreshStart := false
	for _, id := range ops.starts() {
		if id == "sa-fresh" {
			foundFreshStart = true
			break
		}
	}
	if !foundFreshStart {
		t.Errorf("Start was not called on fresh SA id; starts=%v", ops.starts())
	}

	cancel()
	<-done
}

// Issue #8 guard: when the SA is destroyed but the configured target
// is also gone (full stack teardown, not just an SA upgrade), the
// watcher must NOT respawn the SA — that would fight a shutdown we
// don't own and leave a half-started container behind.
func TestRun_F45_NoRespawnIfTargetAlsoGone(t *testing.T) {
	priorSA := ContainerInfo{
		ID:    "sa-prior",
		Names: []string{"/ak-outpost-ldap-service-anchor"},
		State: "running",
	}
	// No target in the fixture: simulates the stack being torn down.
	ops := newFakeOps([]ContainerInfo{priorSA})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Wait for backfill.
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

	// SA gets destroyed; meanwhile target is also gone (never was in
	// the list to begin with).
	ops.mu.Lock()
	ops.listResults = nil
	ops.mu.Unlock()
	ops.eventCh <- EventMsg{
		Action:    "destroy",
		ActorID:   priorSA.ID,
		ActorName: "ak-outpost-ldap-service-anchor",
	}

	// Settle.
	time.Sleep(80 * time.Millisecond)

	if specs := ops.createdSpecs(); len(specs) != 0 {
		t.Errorf("must not respawn SA when target is gone; got %d Create calls", len(specs))
	}

	cancel()
	<-done
}

// Issue #8 guard: destroy events for unrelated containers must be
// silently ignored. The watcher only acts when the destroyed name
// matches the recipe's managed SA.
func TestRun_F45_IgnoresDestroyOfUnrelatedContainer(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	managedSA := ContainerInfo{
		ID:    "sa-id",
		Names: []string{"/ak-outpost-ldap-service-anchor"},
		State: "running",
	}
	ops := newFakeOps([]ContainerInfo{target, managedSA})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Wait for backfill.
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
	backfillLists := func() int {
		ops.mu.Lock()
		defer ops.mu.Unlock()
		return ops.listCalls
	}()

	// Destroy event for some other container the operator removed.
	ops.eventCh <- EventMsg{
		Action:    "destroy",
		ActorID:   "some-other-id",
		ActorName: "unrelated-container",
	}

	time.Sleep(80 * time.Millisecond)

	if specs := ops.createdSpecs(); len(specs) != 0 {
		t.Errorf("destroy of unrelated container must not trigger Create; got %d", len(specs))
	}
	// Also: the watcher should not have done an extra List for the
	// unrelated destroy (early-return before the List call).
	ops.mu.Lock()
	after := ops.listCalls
	ops.mu.Unlock()
	if after != backfillLists {
		t.Errorf("unrelated destroy caused extra List call (%d → %d) — should short-circuit on name mismatch",
			backfillLists, after)
	}

	cancel()
	<-done
}

// Issue #8 follow-up: when the parent (network-anchor) restarts on a
// new image digest, the managed SA still running on the old digest
// must be force-recreated against the new one. This is the
// cluster-rolling-deploy cascade — operators push a new anchord
// image, parents restart, standalone (non-compose-managed) SAs catch
// up automatically.
//
// Tag-floats like `:main` keep the same name across pushes; the check
// must therefore compare by sha256 digest, not by tag string.
func TestBackfill_F45_RecreatesSAOnImageDrift(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	oldSA := ContainerInfo{
		ID:      "sa-old",
		Names:   []string{"/ak-outpost-ldap-service-anchor"},
		State:   "running",
		ImageID: "sha256:OLD",
	}
	ops := newFakeOps([]ContainerInfo{target, oldSA})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:main",
		ImageID:      "sha256:NEW",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "sa-fresh"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	w.backfill(context.Background())

	rm := ops.removes()
	if len(rm) != 1 || rm[0] != "sa-old" {
		t.Errorf("expected stale-image SA to be removed exactly once, got %v", rm)
	}
	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected exactly 1 Create after image-drift recreate, got %d", len(specs))
	}
	if specs[0].Name != "ak-outpost-ldap-service-anchor" {
		t.Errorf("recreated SA name: got %q, want ak-outpost-ldap-service-anchor", specs[0].Name)
	}
	if specs[0].Image != "anchord:main" {
		t.Errorf("recreated SA must use parent's current image; got %q", specs[0].Image)
	}
}

// Image-drift guard: when parent and SA share the same digest, no
// churn. Same fixture as the drift case but with matching ImageIDs.
func TestBackfill_F45_NoRecreateWhenImagesMatch(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	sa := ContainerInfo{
		ID:          "sa-id",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "running",
		NetworkMode: "container:tgt-abc",
		ImageID:     "sha256:SAME",
	}
	ops := newFakeOps([]ContainerInfo{target, sa})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:main",
		ImageID:      "sha256:SAME",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	w.backfill(context.Background())

	if rm := ops.removes(); len(rm) != 0 {
		t.Errorf("matching-image SA must not be removed, got %v", rm)
	}
	if specs := ops.createdSpecs(); len(specs) != 0 {
		t.Errorf("matching-image SA must not be recreated, got %d specs", len(specs))
	}
}

// Image-drift override: when the recipe pins ManagedSA.Image
// explicitly, the operator's choice wins. The cascade must NOT fire
// even on a digest mismatch — otherwise we'd fight the explicit pin.
func TestBackfill_F45_NoImageCheckWhenRecipePinsImage(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	sa := ContainerInfo{
		ID:          "sa-id",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "running",
		NetworkMode: "container:tgt-abc",
		ImageID:     "sha256:OLD",
	}
	ops := newFakeOps([]ContainerInfo{target, sa})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:main",
		ImageID:      "sha256:NEW",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}

	recipe := managedRecipe("ak-outpost-ldap")
	recipe.Image = "ghcr.io/lk/anchord:v0.9.0" // operator-pinned
	w := newWithOpsAndRecipe(ops, recipe, "transit")
	w.backfill(context.Background())

	if rm := ops.removes(); len(rm) != 0 {
		t.Errorf("pinned-image SA must not be auto-recreated on parent drift, got %v", rm)
	}
	if specs := ops.createdSpecs(); len(specs) != 0 {
		t.Errorf("pinned-image SA must not be recreated, got %d specs", len(specs))
	}
}

// Backfill-only invariant: the image-drift check fires once at parent
// startup and never on subsequent target-start events. Without this,
// a noisy event source could trigger churning recreates and an
// operator running `docker run --image other-anchord` to test
// something would fight us on every event.
func TestRun_F45_ImageDriftCheckSkippedOnEvent(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	staleSA := ContainerInfo{
		ID:          "sa-stale",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "running",
		NetworkMode: "container:tgt-abc",
		ImageID:     "sha256:OLD",
	}
	// Pre-condition: backfill has ALREADY run when we set up. Simulate
	// by starting the watcher with fixture state where the SA was
	// already there pre-backfill on the SAME image as parent — so
	// backfill is a no-op — and THEN, after Run is going, mutate the
	// SA's image to "stale" via the fake's listResults. The follow-up
	// target-start event must NOT trigger recreate.
	matchingSA := staleSA
	matchingSA.ImageID = "sha256:SAME"
	ops := newFakeOps([]ContainerInfo{target, matchingSA})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:main",
		ImageID:      "sha256:SAME",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Wait for backfill to complete (1 List call).
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

	// Now flip the SA's image to OLD in the listResults — as if the
	// SA had drifted post-backfill.
	ops.mu.Lock()
	ops.listResults = []ContainerInfo{target, staleSA}
	ops.mu.Unlock()

	// Fire a target-start event. This is the F-45 startup path for
	// the event source, NOT backfill — image-drift check must NOT
	// run here.
	ops.eventCh <- EventMsg{Action: "start", ActorID: target.ID, ActorName: "ak-outpost-ldap"}

	time.Sleep(80 * time.Millisecond)

	if rm := ops.removes(); len(rm) != 0 {
		t.Errorf("image-drift check fired on event (not backfill), got removes=%v", rm)
	}
	if specs := ops.createdSpecs(); len(specs) != 0 {
		t.Errorf("image-drift check fired on event (not backfill), got %d specs", len(specs))
	}

	cancel()
	<-done
}

// ---- Issue #10: wrap-dep orphan rebind on SA recreate -----------------------

// Predicate unit test: findOrphanCandidates picks up dependents
// referencing the SA by long ID, short ID, and name; ignores the SA
// itself and unrelated containers.
func TestFindOrphanCandidates_ByAllRefForms(t *testing.T) {
	sa := ContainerInfo{
		ID:    "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Names: []string{"/fe-anchor-x"},
		State: "running",
	}
	all := []ContainerInfo{
		sa,
		// Long ID
		{ID: "dep-long", Names: []string{"/traefik"}, State: "running",
			NetworkMode: "container:" + sa.ID},
		// Short ID (>=12)
		{ID: "dep-short", Names: []string{"/acme"}, State: "running",
			NetworkMode: "container:" + sa.ID[:12]},
		// Name
		{ID: "dep-name", Names: []string{"/wrapped-app"}, State: "running",
			NetworkMode: "container:fe-anchor-x"},
		// Unrelated container with non-container netmode
		{ID: "host-mode", Names: []string{"/whatever"}, State: "running",
			NetworkMode: "host"},
		// Unrelated container pointing at someone else
		{ID: "dep-other", Names: []string{"/foreign"}, State: "running",
			NetworkMode: "container:other-id"},
	}
	got := findOrphanCandidates(all, sa)
	if len(got) != 3 {
		t.Fatalf("expected 3 orphans, got %d: %+v", len(got), got)
	}
	gotIDs := map[string]bool{}
	for _, c := range got {
		gotIDs[c.ID] = true
	}
	for _, want := range []string{"dep-long", "dep-short", "dep-name"} {
		if !gotIDs[want] {
			t.Errorf("missing orphan %q in result", want)
		}
	}
	if gotIDs[sa.ID] {
		t.Error("findOrphanCandidates must NOT include the SA itself")
	}
}

// Happy path: F-45 image-drift recreate triggers, both the SA and
// its three wrap dependents are recreated, and each dep is rebound
// against the new SA's container ID.
func TestBackfill_F45_ReboundDependentsOnImageDrift(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	oldSA := ContainerInfo{
		ID:      "sa-old",
		Names:   []string{"/ak-outpost-ldap-service-anchor"},
		State:   "running",
		ImageID: "sha256:OLD",
	}
	traefik := ContainerInfo{
		ID:          "dep-traefik",
		Names:       []string{"/ix-authentik-traefik-frigate-1"},
		State:       "running",
		NetworkMode: "container:sa-old",
	}
	acme := ContainerInfo{
		ID:          "dep-acme",
		Names:       []string{"/acme-renewer"},
		State:       "running",
		NetworkMode: "container:sa-old",
	}
	wrapped := ContainerInfo{
		ID:          "dep-wrapped",
		Names:       []string{"/authentik_server"},
		State:       "running",
		NetworkMode: "container:sa-old",
	}
	ops := newFakeOps([]ContainerInfo{target, oldSA, traefik, acme, wrapped})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:main",
		ImageID:      "sha256:NEW",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "sa-fresh"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	w.SetAutoFixDeadNetns(true)
	w.backfill(context.Background())

	// Old SA removed exactly once.
	if rm := ops.removes(); len(rm) != 1 || rm[0] != "sa-old" {
		t.Errorf("expected exactly Remove(sa-old), got %v", rm)
	}
	// New SA created and started.
	specs := ops.createdSpecs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 Create for new SA, got %d", len(specs))
	}
	// All three deps rebound, each pointing at the new SA.
	rec := ops.recreateCalls()
	if len(rec) != 3 {
		t.Fatalf("expected 3 dep rebinds, got %d: %+v", len(rec), rec)
	}
	rebound := map[string]string{}
	for _, r := range rec {
		rebound[r.OldID] = r.NewNetMode
	}
	for _, want := range []string{"dep-traefik", "dep-acme", "dep-wrapped"} {
		if got, ok := rebound[want]; !ok {
			t.Errorf("dep %q was not rebound", want)
		} else if got != "container:sa-fresh" {
			t.Errorf("dep %q rebound to wrong target: got %q, want container:sa-fresh", want, got)
		}
	}
}

// Issue #5 path (stale-netns recreate) also triggers the rebind.
func TestRun_F45_ReboundDependentsOnStaleNetns(t *testing.T) {
	newTarget := ContainerInfo{
		ID:    "new-tgt-7d6c738d",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	staleSA := ContainerInfo{
		ID:          "stale-sa-id",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "running",
		NetworkMode: "container:old-tgt-0f98a101",
	}
	traefik := ContainerInfo{
		ID:          "dep-traefik",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:stale-sa-id",
	}
	ops := newFakeOps([]ContainerInfo{newTarget, staleSA, traefik})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "fresh-sa-id"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	w.SetAutoFixDeadNetns(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: newTarget.ID, ActorName: "ak-outpost-ldap"}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ops.recreateCalls()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec := ops.recreateCalls()
	if len(rec) != 1 || rec[0].OldID != "dep-traefik" {
		t.Errorf("expected single dep rebind of dep-traefik, got %+v", rec)
	}
	if rec[0].NewNetMode != "container:fresh-sa-id" {
		t.Errorf("dep rebound to wrong target: got %q, want container:fresh-sa-id", rec[0].NewNetMode)
	}

	cancel()
	<-done
}

// Opt-out invariant: with SetAutoFixDeadNetns(false), the SA recreate
// proceeds as in v1.1.0 — but no RecreateWithNetworkMode call is
// issued. Dependents are left orphaned for the v1.1.0 watcher's
// WARN logs.
func TestBackfill_F45_NoRebindWhenAutoFixDisabled(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	oldSA := ContainerInfo{
		ID:      "sa-old",
		Names:   []string{"/ak-outpost-ldap-service-anchor"},
		State:   "running",
		ImageID: "sha256:OLD",
	}
	dep := ContainerInfo{
		ID:          "dep-traefik",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:sa-old",
	}
	ops := newFakeOps([]ContainerInfo{target, oldSA, dep})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:main",
		ImageID:      "sha256:NEW",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "sa-fresh"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	w.SetAutoFixDeadNetns(false)
	w.backfill(context.Background())

	// SA still recreated.
	if specs := ops.createdSpecs(); len(specs) != 1 {
		t.Errorf("SA should still be recreated even with AutoFix=false; got %d Create calls", len(specs))
	}
	// But NO dep rebind.
	if rec := ops.recreateCalls(); len(rec) != 0 {
		t.Errorf("AutoFix=false must not trigger dep rebinds, got %d: %+v", len(rec), rec)
	}
}

// SA-absent path: orphans pointing at an old ID we don't know about
// are NOT rebound — we only act on deps pinned to the SA WE just
// removed. (The v1.1.0 dependents watcher catches these.)
func TestBackfill_F45_NoRebindOnAbsentSA(t *testing.T) {
	target := ContainerInfo{
		ID:    "tgt-abc",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	// No SA in the list — F-45 absent path. Some random orphan
	// pointing at a long-dead container that we never managed.
	staleDep := ContainerInfo{
		ID:          "stale-dep",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:long-dead-id",
	}
	ops := newFakeOps([]ContainerInfo{target, staleDep})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:main",
		ImageID:      "sha256:X",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "sa-fresh"

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	w.SetAutoFixDeadNetns(true)
	w.backfill(context.Background())

	if rec := ops.recreateCalls(); len(rec) != 0 {
		t.Errorf("absent-SA create must not rebind unrelated orphans, got %+v", rec)
	}
}

// Dep rebind failure for one container must NOT abort the loop —
// remaining orphans still get their chance.
func TestRun_F45_RebindContinuesAfterPerDepFailure(t *testing.T) {
	newTarget := ContainerInfo{
		ID:    "new-tgt",
		Names: []string{"/ak-outpost-ldap"},
		State: "running",
	}
	staleSA := ContainerInfo{
		ID:          "stale-sa-id",
		Names:       []string{"/ak-outpost-ldap-service-anchor"},
		State:       "running",
		NetworkMode: "container:old-tgt",
	}
	depA := ContainerInfo{
		ID:          "dep-fail",
		Names:       []string{"/traefik-fail"},
		State:       "running",
		NetworkMode: "container:stale-sa-id",
	}
	depB := ContainerInfo{
		ID:          "dep-ok",
		Names:       []string{"/traefik-ok"},
		State:       "running",
		NetworkMode: "container:stale-sa-id",
	}
	ops := newFakeOps([]ContainerInfo{newTarget, staleSA, depA, depB})
	ops.selfInfo = SelfInfo{
		Image:        "anchord:test",
		IPsByNetwork: map[string]string{"transit": "10.0.0.5"},
	}
	ops.createNewID = "fresh-sa"
	ops.recreateErr["dep-fail"] = errors.New("ContainerCreate: name in use")

	w := newWithOpsAndRecipe(ops, managedRecipe("ak-outpost-ldap"), "transit")
	w.SetAutoFixDeadNetns(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	ops.eventCh <- EventMsg{Action: "start", ActorID: newTarget.ID, ActorName: "ak-outpost-ldap"}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ops.recreateCalls()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec := ops.recreateCalls()
	if len(rec) != 2 {
		t.Fatalf("loop aborted after dep-fail; expected 2 recreate calls, got %d: %+v", len(rec), rec)
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
