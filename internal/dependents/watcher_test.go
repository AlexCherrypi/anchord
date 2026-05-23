package dependents

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOps is a recording, scriptable Ops for unit tests.
type fakeOps struct {
	mu        sync.Mutex
	results   []Container
	resultErr error
	calls     int32
}

func (f *fakeOps) setResults(c []Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = c
}

func (f *fakeOps) List(_ context.Context) ([]Container, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resultErr != nil {
		return nil, f.resultErr
	}
	out := make([]Container, len(f.results))
	copy(out, f.results)
	return out, nil
}

// recordingHook is a thread-safe OnVictim collector.
type recordingHook struct {
	mu       sync.Mutex
	victims  []StaleNetns
}

func (r *recordingHook) hook(s StaleNetns) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.victims = append(r.victims, s)
}

func (r *recordingHook) snapshot() []StaleNetns {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StaleNetns, len(r.victims))
	copy(out, r.victims)
	return out
}

// Direct tick: a stale dep in scope produces exactly one victim.
func TestTick_ReportsNewVictim(t *testing.T) {
	dep := Container{
		ID:          "dep1",
		Names:       []string{"/traefik-frigate"},
		State:       "running",
		NetworkMode: "container:DEAD",
		Labels: map[string]string{
			"com.docker.compose.project": "ix-authentik",
			"com.docker.compose.service": "traefik-frigate",
		},
	}
	ops := &fakeOps{results: []Container{dep}}
	rec := &recordingHook{}
	w := newWithOps(ops, "ix-authentik", time.Second)
	w.OnVictim = rec.hook

	w.tick(context.Background())

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 victim, got %d", len(got))
	}
	if got[0].Container.ID != "dep1" || got[0].StaleTarget != "DEAD" {
		t.Errorf("wrong victim: %+v", got[0])
	}
}

// Dedup: same victim on a subsequent tick must NOT re-fire the hook.
func TestTick_DoesNotReReportSameVictim(t *testing.T) {
	dep := Container{
		ID:          "dep1",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:DEAD",
		Labels:      map[string]string{"com.docker.compose.project": "p"},
	}
	ops := &fakeOps{results: []Container{dep}}
	rec := &recordingHook{}
	w := newWithOps(ops, "p", time.Second)
	w.OnVictim = rec.hook

	w.tick(context.Background())
	w.tick(context.Background())
	w.tick(context.Background())

	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("victim should only be reported once across repeated ticks, got %d reports", got)
	}
}

// If a victim is fixed (ref resolves again) and later goes stale once
// more under the SAME ID (e.g. SA recreated twice), we must warn
// twice — the seen-set is rebuilt every tick, so the second
// stale-window produces a fresh report.
func TestTick_ReReportsWhenVictimReturnsAfterFix(t *testing.T) {
	deadTarget := "DEAD-1"
	depStaleA := Container{
		ID:          "dep1",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:" + deadTarget,
		Labels:      map[string]string{"com.docker.compose.project": "p"},
	}
	depLive := depStaleA
	depLive.NetworkMode = "container:live-target"
	liveTarget := Container{
		ID:    "live-target",
		Names: []string{"/sa"},
		State: "running",
	}
	ops := &fakeOps{results: []Container{depStaleA}}
	rec := &recordingHook{}
	w := newWithOps(ops, "p", time.Second)
	w.OnVictim = rec.hook

	// Tick 1: stale → 1 report
	w.tick(context.Background())
	// Tick 2: fixed (live-target exists, dep points there) → no new
	ops.setResults([]Container{depLive, liveTarget})
	w.tick(context.Background())
	// Tick 3: stale again under the SAME id (dep was force-recreated
	// pointing at a stale ref a second time)
	ops.setResults([]Container{depStaleA})
	w.tick(context.Background())

	if got := len(rec.snapshot()); got != 2 {
		t.Errorf("expected 2 reports across fix-then-rebreak, got %d", got)
	}
}

// Scope filter: dependents in other projects must not produce reports.
func TestTick_ScopeFiltersByComposeProject(t *testing.T) {
	mineStale := Container{
		ID:          "mine",
		Names:       []string{"/mine-traefik"},
		State:       "running",
		NetworkMode: "container:DEAD-1",
		Labels:      map[string]string{"com.docker.compose.project": "mine"},
	}
	theirsStale := Container{
		ID:          "theirs",
		Names:       []string{"/theirs-traefik"},
		State:       "running",
		NetworkMode: "container:DEAD-2",
		Labels:      map[string]string{"com.docker.compose.project": "theirs"},
	}
	ops := &fakeOps{results: []Container{mineStale, theirsStale}}
	rec := &recordingHook{}
	w := newWithOps(ops, "mine", time.Second)
	w.OnVictim = rec.hook

	w.tick(context.Background())

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 in-scope victim, got %d", len(got))
	}
	if got[0].Container.ID != "mine" {
		t.Errorf("scope leaked: got %s, want mine", got[0].Container.ID)
	}
}

// Run with empty scope: must not loop forever in a busy way; it
// blocks on ctx and exits cleanly. We don't assert on logs here —
// just on the goroutine lifecycle.
func TestRun_EmptyScopeIsNoOpAndExitsOnCtx(t *testing.T) {
	ops := &fakeOps{}
	w := newWithOps(ops, "", 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Give the goroutine a beat. List must NOT have been called.
	time.Sleep(40 * time.Millisecond)
	if atomic.LoadInt32(&ops.calls) != 0 {
		t.Errorf("empty-scope watcher must not List, got %d calls", ops.calls)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run exit err: got %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not exit on ctx cancel")
	}
}

// List failure is logged but doesn't crash the loop.
func TestTick_ListFailureToleratedNoVictims(t *testing.T) {
	ops := &fakeOps{resultErr: errors.New("docker socket closed")}
	rec := &recordingHook{}
	w := newWithOps(ops, "p", time.Second)
	w.OnVictim = rec.hook

	w.tick(context.Background())

	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("List failure must produce no victims, got %d", got)
	}
}
