// Package autostart implements SPEC F-43: when a sibling container in
// the project sits in `Created` state because its
// `network_mode: container:<X>` target wasn't running at compose-up
// time, watch for the target's appearance and kick `docker start` on
// the sibling once it shows up.
//
// Why this exists: Docker accepts `docker create --network
// container:NONEXISTENT` (the container lands in Created state) but
// rejects start until the target exists, and does NOT auto-retry the
// failed initial start — `restart: always` only fires after a
// successful start at least once. The Authentik DMZ migration trips
// this: the worker spawns LDAP/proxy outposts via the Docker API at
// runtime, with no compose-level depends_on hook to wait on, so the
// service-anchors for those targets stay in Created state forever
// unless someone (a) restarts them manually or (b) watches events.
// anchord already watches events for backend discovery; F-43 piggy-
// backs on that capability for siblings.
//
// The watcher is deliberately stateless across process restarts: on
// startup it does a single backfill scan (find every Created-state
// container whose target is already running and start it), then
// streams Docker events and reacts to each `container start` event.
// All start calls are idempotent at the Docker API layer — a duplicate
// start on a running container returns success — so two anchords in
// the same project racing for the same sibling is fine.
//
// The package is mockable end-to-end: production wires it to a
// *client.Client via the dockerAdapter; tests pass a fake dockerOps
// that records calls and replays canned events.
package autostart

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// ContainerInfo is the minimal projection of a Docker container view
// that matchSiblings needs. Production builds it from
// container.Summary; tests construct it directly.
type ContainerInfo struct {
	ID          string
	Names       []string
	State       string // "created", "running", "exited", …
	NetworkMode string // raw HostConfig.NetworkMode — e.g. "container:abc123"
}

// EventMsg is the narrowed event shape autostart cares about. Tests
// emit these via a channel; in production dockerAdapter translates
// docker SDK events.Message into this form.
type EventMsg struct {
	Action  string // "start" — autostart ignores everything else
	ActorID string
	ActorName string
}

// dockerOps is the slice of Docker surface this package uses. Kept
// narrow so unit tests don't drag the SDK in.
type dockerOps interface {
	// List returns every container Docker knows about, regardless of
	// state. The autostart code filters internally so the same call
	// services both the backfill scan and the per-event re-scan.
	List(ctx context.Context) ([]ContainerInfo, error)

	// Start triggers a single container.start API call. Docker
	// returns success for an already-running container (idempotent),
	// so callers don't need to deduplicate.
	Start(ctx context.Context, id string) error

	// Events subscribes to Docker container.start events. The
	// returned channels mirror docker SDK semantics: msgs delivers
	// events, errs delivers terminal errors (caller is expected to
	// re-subscribe on error).
	Events(ctx context.Context) (<-chan EventMsg, <-chan error)
}

// Watcher is the live F-43 worker. One instance per network-anchor.
type Watcher struct {
	ops dockerOps
}

// New constructs a Watcher backed by a live Docker client.
func New(cli *client.Client) *Watcher {
	return &Watcher{ops: dockerAdapter{cli: cli}}
}

// newWithOps is the test seam.
func newWithOps(ops dockerOps) *Watcher { return &Watcher{ops: ops} }

// Run does the startup backfill and then streams Docker events,
// reacting to each `container start` by attempting to start any
// Created-state siblings that point at the just-started target.
//
// Returns ctx.Err() when the supplied context is cancelled. On a
// terminal event-stream error it sleeps 2 s and re-subscribes — the
// same pattern internal/discovery uses.
func (w *Watcher) Run(ctx context.Context) error {
	w.backfill(ctx)

	for {
		msgs, errs := w.ops.Events(ctx)
		err := w.consume(ctx, msgs, errs)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("autostart event stream error, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// consume reads from the message and error channels until one closes
// (or returns a terminal error), then returns. Caller decides whether
// to re-subscribe.
func (w *Watcher) consume(ctx context.Context, msgs <-chan EventMsg, errs <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errs:
			if !ok {
				return fmt.Errorf("event error channel closed")
			}
			return err
		case msg, ok := <-msgs:
			if !ok {
				return fmt.Errorf("event message channel closed")
			}
			if msg.Action != "start" {
				continue
			}
			w.handleTargetStart(ctx, ContainerInfo{
				ID:    msg.ActorID,
				Names: []string{msg.ActorName},
				State: "running",
			})
		}
	}
}

// backfill runs once at startup to catch siblings that were stranded
// before the watcher came up — e.g. anchord restart while the cluster
// was mid-deploy. Lists every container, finds the running ones, and
// kicks start on any Created-state sibling whose NetworkMode resolves
// to one of them.
func (w *Watcher) backfill(ctx context.Context) {
	all, err := w.ops.List(ctx)
	if err != nil {
		slog.Warn("autostart backfill list failed", "err", err)
		return
	}
	for _, t := range all {
		if t.State != "running" {
			continue
		}
		for _, sib := range matchSiblings(all, t) {
			w.start(ctx, sib, t, "backfill")
		}
	}
}

// handleTargetStart is the per-event entry: a target just started,
// re-scan and trigger any matching Created siblings.
func (w *Watcher) handleTargetStart(ctx context.Context, target ContainerInfo) {
	all, err := w.ops.List(ctx)
	if err != nil {
		slog.Warn("autostart per-event list failed", "err", err)
		return
	}
	for _, sib := range matchSiblings(all, target) {
		w.start(ctx, sib, target, "event")
	}
}

// start issues the Docker start API call and logs the result. Failure
// is warn-level (operator-visible); success is info-level. Source is
// "backfill" or "event" for traceability.
func (w *Watcher) start(ctx context.Context, sibID string, target ContainerInfo, source string) {
	tgtName := firstName(target)
	if err := w.ops.Start(ctx, sibID); err != nil {
		slog.Warn("sibling failed to start",
			"sibling", sibID, "target", tgtName, "source", source, "err", err)
		return
	}
	slog.Info("auto-starting sibling waiting for target",
		"sibling", sibID, "target", tgtName, "source", source)
}

// matchSiblings returns IDs of Created-state containers whose
// `network_mode: container:<X>` references `target`. Match is by ID
// (long or short) or by any of `target`'s names — Docker accepts all
// three forms in NetworkMode.
//
// Pure function so unit tests don't need a Docker daemon.
func matchSiblings(all []ContainerInfo, target ContainerInfo) []string {
	refs := referencesFor(target)
	if len(refs) == 0 {
		return nil
	}
	var out []string
	for _, c := range all {
		if !strings.EqualFold(c.State, "created") {
			continue
		}
		ref, ok := strings.CutPrefix(c.NetworkMode, "container:")
		if !ok {
			continue
		}
		if _, matched := refs[strings.TrimPrefix(ref, "/")]; matched {
			out = append(out, c.ID)
		}
	}
	return out
}

// referencesFor returns a set of strings any of which would be a
// valid `network_mode: container:<...>` reference to `c` — long ID,
// short ID, and every name (with the leading '/' stripped, since
// Docker exposes names with one).
func referencesFor(c ContainerInfo) map[string]struct{} {
	out := map[string]struct{}{}
	if c.ID != "" {
		out[c.ID] = struct{}{}
		if len(c.ID) >= 12 {
			out[c.ID[:12]] = struct{}{}
		}
	}
	for _, n := range c.Names {
		out[strings.TrimPrefix(n, "/")] = struct{}{}
	}
	return out
}

func firstName(c ContainerInfo) string {
	for _, n := range c.Names {
		if n != "" {
			return strings.TrimPrefix(n, "/")
		}
	}
	if c.ID != "" && len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}

// ---- production Docker adapter -----------------------------------------

type dockerAdapter struct {
	cli *client.Client
}

func (a dockerAdapter) List(ctx context.Context) ([]ContainerInfo, error) {
	// All=true so Created/Exited states show up — the whole point of
	// F-43 is finding things NOT currently running.
	list, err := a.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("ContainerList: %w", err)
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, c := range list {
		out = append(out, ContainerInfo{
			ID:          c.ID,
			Names:       c.Names,
			State:       c.State,
			NetworkMode: c.HostConfig.NetworkMode,
		})
	}
	return out, nil
}

func (a dockerAdapter) Start(ctx context.Context, id string) error {
	return a.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (a dockerAdapter) Events(ctx context.Context) (<-chan EventMsg, <-chan error) {
	f := filters.NewArgs()
	f.Add("type", "container")
	f.Add("event", "start")
	rawMsgs, rawErrs := a.cli.Events(ctx, events.ListOptions{Filters: f})
	msgs := make(chan EventMsg, 4)
	errs := make(chan error, 1)
	go func() {
		defer close(msgs)
		defer close(errs)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-rawErrs:
				if !ok {
					return
				}
				errs <- e
				return
			case m, ok := <-rawMsgs:
				if !ok {
					return
				}
				msgs <- EventMsg{
					Action:    string(m.Action),
					ActorID:   m.Actor.ID,
					ActorName: m.Actor.Attributes["name"],
				}
			}
		}
	}()
	return msgs, errs
}
