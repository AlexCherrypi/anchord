package dependents

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Ops is the slice of Docker surface the Watcher uses. Kept narrow
// so unit tests don't pull in the SDK.
type Ops interface {
	// List returns every container Docker knows about including
	// non-running ones — we need stopped containers in the result so
	// the predicate doesn't false-positive on dependents whose netns
	// target is exited-but-still-extant (Docker keeps the netns until
	// destroy).
	List(ctx context.Context) ([]Container, error)
}

// Watcher periodically scans for dead-netns dependents within a
// compose-project scope and reports each new victim exactly once.
//
// Log-only by design (issue #9): the watcher tells the operator what
// to recreate, it never recreates anything on its own. Auto-fix is a
// separate policy choice tracked in a follow-up issue.
type Watcher struct {
	ops          Ops
	scopeProject string
	interval     time.Duration

	// OnVictim is the per-detection callback. When nil, the watcher
	// uses its built-in slog.Warn formatter. Tests override to assert
	// without parsing log output.
	OnVictim func(StaleNetns)

	seen map[string]struct{} // (depID|staleRef) reported this cycle
}

// New constructs a Watcher backed by a live Docker client. Pass the
// anchord network-anchor's compose project as `scopeProject`; pass
// "" to keep the watcher in disabled mode (no-op Run).
//
// `interval` is the period between full host scans. A few seconds
// is plenty — dead-netns is a sticky state, not a fleeting one, and
// faster ticks just multiply the docker.List rate.
func New(cli *client.Client, scopeProject string, interval time.Duration) *Watcher {
	return &Watcher{
		ops:          dockerAdapter{cli: cli},
		scopeProject: scopeProject,
		interval:     interval,
		seen:         map[string]struct{}{},
	}
}

// newWithOps is the test seam.
func newWithOps(ops Ops, scopeProject string, interval time.Duration) *Watcher {
	return &Watcher{
		ops:          ops,
		scopeProject: scopeProject,
		interval:     interval,
		seen:         map[string]struct{}{},
	}
}

// Run blocks until ctx is cancelled. Ticks immediately on entry so
// startup-time orphans are reported without waiting `interval`.
//
// In disabled mode (empty scopeProject) Run logs a one-time info and
// then sleeps on ctx.Done — keeping the goroutine alive so main's
// supervisor doesn't see an early exit.
func (w *Watcher) Run(ctx context.Context) error {
	if w.scopeProject == "" {
		slog.Info("dead-netns detector disabled: no compose project scope " +
			"(label-selector mode has no obvious single-project boundary; " +
			"use `anchord doctor stale-netns` for ad-hoc cluster-wide scans)")
		<-ctx.Done()
		return ctx.Err()
	}
	slog.Info("dead-netns detector starting",
		"scope_project", w.scopeProject, "interval", w.interval)

	w.tick(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick is one detection pass: list all containers, scope candidates
// to the configured compose project, run Find, dispatch each new
// victim once. The `seen` set is fully rebuilt every tick — a victim
// that disappears (operator recreated it, or the container itself
// was removed) won't pin the slot, so if the same name later goes
// stale under a new container ID we'll warn again.
func (w *Watcher) tick(ctx context.Context) {
	all, err := w.ops.List(ctx)
	if err != nil {
		slog.Warn("dead-netns detector list failed", "err", err)
		return
	}
	candidates := scopeByProject(all, w.scopeProject)
	stale := Find(candidates, all)

	hook := w.OnVictim
	if hook == nil {
		hook = logVictim
	}

	newSeen := make(map[string]struct{}, len(stale))
	for _, s := range stale {
		key := s.Container.ID + "|" + s.StaleTarget
		newSeen[key] = struct{}{}
		if _, already := w.seen[key]; already {
			continue
		}
		hook(s)
	}
	w.seen = newSeen
}

func scopeByProject(all []Container, project string) []Container {
	var out []Container
	for _, c := range all {
		if c.Labels["com.docker.compose.project"] == project {
			out = append(out, c)
		}
	}
	return out
}

// logVictim is the default detection sink — emits a structured WARN.
func logVictim(s StaleNetns) {
	attrs := []any{
		"container", FirstName(s.Container),
		"container_id", s.Container.ID,
		"stale_target", s.StaleTarget,
	}
	if s.ComposeHint != "" {
		attrs = append(attrs, "hint", s.ComposeHint)
	}
	slog.Warn("dependent in dead netns", attrs...)
}

// ---- production Docker adapter ---------------------------------------------

type dockerAdapter struct {
	cli *client.Client
}

func (a dockerAdapter) List(ctx context.Context) ([]Container, error) {
	list, err := a.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("ContainerList: %w", err)
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		out = append(out, Container{
			ID:          c.ID,
			Names:       c.Names,
			State:       c.State,
			NetworkMode: c.HostConfig.NetworkMode,
			Labels:      c.Labels,
		})
	}
	return out, nil
}
