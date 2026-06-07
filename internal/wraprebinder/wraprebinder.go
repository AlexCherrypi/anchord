// Package wraprebinder implements SPEC F-49: a sidecar that auto-
// restarts wrap-anchors when their `network_mode: container:<X>`
// target is recreated under the same name.
//
// The wrap-rebinder lives in the wrap-stack's compose project (e.g.
// `mailcow-anchord-wrap`). It auto-discovers every sibling whose
// HostConfig.NetworkMode parses as `container:<NAME>`, builds a
// target-name → wrap-anchor-IDs map, and watches `docker events` for
// `container start` events. Whenever a tracked target's name starts,
// the matching wrap-anchors are `docker restart`'d so Docker
// re-resolves their netmode against the new container ID.
//
// Sibling to F-48 (external-rebinder, internal/rebinder): F-48
// handles stale-bridge-network references, F-49 handles stale-netns-
// target references. Same incident chain (2026-06-07 Mailcow recreate),
// different layer.
//
// Two subtle behaviours the SPEC marks explicit:
//
//  1. Name-based filtering happens in-process. Docker's event filter
//     API is OR-ed for repeated keys and we track a dynamic set of
//     names; the daemon-side filter only narrows the type+event.
//     consume() looks each `start` event up in the target-pool map.
//
//  2. The sidecar re-enumerates its target pool on EVERY `start`
//     event whose container belongs to the sidecar's own compose
//     project — because that means a sibling wrap-anchor was just
//     recreated and its ID is no longer in our internal map. Without
//     this, the sidecar holds stale anchor IDs after every
//     `compose up --force-recreate` of the wrap-stack itself.
package wraprebinder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// ContainerInfo is the narrow projection of a Docker container view
// the wrap-rebinder uses. Built from container.Summary in production;
// constructed directly by tests.
type ContainerInfo struct {
	ID          string
	Names       []string
	State       string
	NetworkMode string
	Labels      map[string]string
}

// EventMsg is the wrap-rebinder-shaped container event. Only the
// `start` action drives behaviour. ContainerName and Project are
// extracted from event.Actor.Attributes by the production adapter.
type EventMsg struct {
	Action        string
	ContainerID   string
	ContainerName string
	Project       string // com.docker.compose.project, if set
}

// dockerOps is the slice of Docker surface this package uses. Kept
// narrow so unit tests don't drag the SDK in.
type dockerOps interface {
	// ContainerList returns every container Docker knows about. The
	// rebinder uses it both for target-pool enumeration (siblings in
	// self project) and for live-target lookup by name (any project).
	ContainerList(ctx context.Context) ([]ContainerInfo, error)

	// RecreateWithNetworkMode atomically inspects an anchor, removes
	// it, and re-creates it with the same Config/HostConfig except for
	// HostConfig.NetworkMode (replaced with newNetMode). Returns the
	// new container's ID after it's been started.
	//
	// Why this — not ContainerRestart: Docker resolves
	// `network_mode: container:<X>` at container *creation* time, not
	// at every start, and stores the resolved long-ID in
	// HostConfig.NetworkMode. A subsequent `docker restart` against a
	// dead target ID fails with
	// `joining network namespace of container: No such container`,
	// and the anchor lands in `exited`. The only way to re-resolve is
	// to recreate the container with a fresh NetworkMode string —
	// same primitive F-45 uses (internal/autostart). timeout is
	// forwarded as the stop-timeout during remove.
	RecreateWithNetworkMode(ctx context.Context, id, newNetMode string, timeout time.Duration) (string, error)

	// Events subscribes to docker container start events. Other
	// actions are filtered at the daemon by the production adapter.
	Events(ctx context.Context) (<-chan EventMsg, <-chan error)
}

// targetPool is target_name → list of wrap-anchor container IDs whose
// HostConfig.NetworkMode resolves to that name. Holds at most a few
// entries per typical wrap-stack (Mailcow has 3; Authentik tops out
// around 5).
type targetPool map[string][]string

// Watcher is the long-running F-49 worker. One instance per
// wrap-stack.
type Watcher struct {
	ops dockerOps
	cfg *config.WrapRebinder

	// selfID is the wrap-rebinder's own container ID — used to
	// exclude itself from the target-pool enumeration. Resolved
	// once at Run() time via os.Hostname() (Docker sets the
	// container ID as HOSTNAME).
	selfID string

	mu sync.Mutex

	// pool is the live target_name → []anchorID map. Rebuilt on
	// every periodic poll and on every `start` event for a sibling
	// in our own project.
	pool targetPool

	// lastRestartedAt deduplicates rapid back-to-back restarts of
	// the same anchor. Restarts within rapidRestartWindow of a prior
	// restart on the same anchor are dropped at warn-level.
	lastRestartedAt map[string]time.Time
}

// rapidRestartWindow is the dedup window. Two `start` events for the
// same target name within this window — possible if the target is in
// a tight crash-loop, or if a `compose up` emits start events back to
// back — only trigger one anchor restart.
const rapidRestartWindow = 1 * time.Second

// New constructs a Watcher backed by a live Docker client.
func New(cli *client.Client, cfg *config.WrapRebinder) *Watcher {
	return &Watcher{
		ops:             dockerAdapter{cli: cli},
		cfg:             cfg,
		pool:            targetPool{},
		lastRestartedAt: map[string]time.Time{},
	}
}

// newWithOps is the test seam.
func newWithOps(ops dockerOps, cfg *config.WrapRebinder, selfID string) *Watcher {
	return &Watcher{
		ops:             ops,
		cfg:             cfg,
		selfID:          selfID,
		pool:            targetPool{},
		lastRestartedAt: map[string]time.Time{},
	}
}

// Run is the supervised main loop. Enumerates the target pool, runs a
// bootstrap recheck, then alternates between consuming events and a
// periodic poll. Returns ctx.Err() on cancel. Recovers from event-
// stream errors with a 2s back-off and resubscribe.
func (w *Watcher) Run(ctx context.Context) error {
	if w.selfID == "" {
		// Best-effort self-resolution. Empty selfID just means we
		// don't filter ourselves out of the pool; harmless because a
		// wrap-rebinder's NetworkMode is `bridge`, not `container:*`,
		// so it won't be picked up anyway.
		if h, err := os.Hostname(); err == nil {
			w.selfID = h
		}
	}
	slog.Info("wrap-rebinder starting",
		"project", w.cfg.SelfProject,
		"poll_interval", w.cfg.PollInterval,
		"restart_timeout", w.cfg.RestartTimeout)

	w.enumerateAndRecheck(ctx, "startup")

	pollT := time.NewTicker(w.cfg.PollInterval)
	defer pollT.Stop()

	for {
		msgs, errs := w.ops.Events(ctx)
		err := w.consume(ctx, msgs, errs, pollT)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("wrap-rebinder event stream error, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// consume reads events, periodically re-polls, and exits on ctx
// cancel or channel close/error.
func (w *Watcher) consume(ctx context.Context, msgs <-chan EventMsg, errs <-chan error, pollT *time.Ticker) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errs:
			if !ok {
				return fmt.Errorf("event error channel closed")
			}
			return err
		case <-pollT.C:
			w.enumerateAndRecheck(ctx, "poll")
		case msg, ok := <-msgs:
			if !ok {
				return fmt.Errorf("event message channel closed")
			}
			if msg.Action != "start" {
				continue
			}
			w.handleStart(ctx, msg)
		}
	}
}

// handleStart dispatches a `start` event:
//   - If the event is for a tracked target name, recreate all anchors
//     mapped to it (SPEC §"On `container start` event" bullet 1).
//   - If the event is for a sibling in our own compose project,
//     re-enumerate the target pool (SPEC bullet 2 / Anmerkung 2). The
//     sibling that just started may BE a freshly-recreated wrap-anchor
//     whose new container ID we don't know yet.
//   - Else: drop silently.
//
// The two cases can both be true (a sibling wrap-anchor's start fires
// AND it happens to be named the same as a target we track) — handled
// in order: recreate-first, re-enumerate-second.
func (w *Watcher) handleStart(ctx context.Context, msg EventMsg) {
	if anchors := w.lookupAnchors(msg.ContainerName); len(anchors) > 0 {
		slog.Info("tracked target started, recreating affected wrap-anchors",
			"target", msg.ContainerName,
			"anchors", anchors)
		for _, anchorID := range anchors {
			w.recreateAnchor(ctx, anchorID, msg.ContainerName, "event/target-start")
		}
	}
	if msg.Project == w.cfg.SelfProject {
		slog.Debug("own-project sibling started; re-enumerating target pool",
			"sibling", msg.ContainerName)
		w.enumerateTargetPool(ctx)
	}
}

// lookupAnchors returns a defensive copy of the anchor list for the
// given target name, or nil. The copy is so the caller can range
// without holding the mutex.
func (w *Watcher) lookupAnchors(targetName string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	src, ok := w.pool[targetName]
	if !ok {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// enumerateAndRecheck rebuilds the target pool and then verifies
// each pool entry's current state against the live target, restarting
// drifted anchors. Used at startup and on every periodic poll.
func (w *Watcher) enumerateAndRecheck(ctx context.Context, source string) {
	w.enumerateTargetPool(ctx)
	w.bootstrapRecheck(ctx, source)
}

// enumerateTargetPool re-builds the target_name → []anchorID map by
// scanning all containers in self project for `container:<X>`
// netmodes. Excludes the sidecar's own ID. Holds the mutex briefly
// for the assignment; never holds it across a Docker API call.
//
// Anchors whose stored `container:<ID>` reference doesn't resolve to a
// live container are tracked as "orphans" — we know the anchor is
// broken but can't recover the original target name (Docker doesn't
// preserve the operator's `container:<NAME>` spec; it resolves to ID
// at create time). Orphans are surfaced as a WARN and otherwise
// skipped. The (b-ii) label-based discovery from issue #13 would
// close this gap; deferred to a future revision.
func (w *Watcher) enumerateTargetPool(ctx context.Context) {
	all, err := w.ops.ContainerList(ctx)
	if err != nil {
		slog.Warn("wrap-rebinder: container list failed during pool enumeration",
			"err", err)
		return
	}
	pool, orphans := buildTargetPool(all, w.cfg.SelfProject, w.selfID)
	w.mu.Lock()
	w.pool = pool
	w.mu.Unlock()
	for _, o := range orphans {
		slog.Warn("wrap-rebinder: anchor has stale netmode whose target ID no longer exists; manual recreate required",
			"anchor", o.anchorID,
			"stored_ref", o.storedRef)
	}
	if len(pool) == 0 {
		slog.Info("wrap-rebinder: target pool is empty; nothing to track yet")
		return
	}
	slog.Info("wrap-rebinder: target pool refreshed", "targets", len(pool))
}

// orphanAnchor describes a wrap-anchor whose stored `container:<X>`
// reference no longer resolves. We retain the stored reference for
// the operator-visible log line.
type orphanAnchor struct {
	anchorID  string
	storedRef string
}

// buildTargetPool extracts target_name → []anchorID from the given
// container list. Pure function so unit tests don't need a daemon.
//
//   - Siblings must be in `selfProject` (matched against the
//     compose.project label).
//   - The sidecar itself is excluded by ID match.
//   - Only `network_mode: container:<X>` entries contribute. Other
//     shapes (host, bridge, none, service:, empty) are skipped.
//   - `<X>` may be a long ID, short ID, or name; the function
//     resolves each against the full container list to recover the
//     canonical target *name* used as the pool key. Docker stores
//     `container:<X>` references as long IDs after a successful start
//     — we re-derive the name so the pool is name-keyed and matches
//     the names that come out of the event stream.
//   - Anchors whose `<X>` doesn't resolve to any live container are
//     returned in the `orphans` slice instead of the pool. They're
//     known-broken but unrecoverable without label-based discovery.
func buildTargetPool(all []ContainerInfo, selfProject, selfID string) (targetPool, []orphanAnchor) {
	pool := targetPool{}
	var orphans []orphanAnchor
	for _, c := range all {
		if c.ID == selfID {
			continue
		}
		if c.Labels["com.docker.compose.project"] != selfProject {
			continue
		}
		ref, ok := strings.CutPrefix(c.NetworkMode, "container:")
		if !ok {
			continue
		}
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		targetName := resolveTargetName(all, ref)
		if targetName == "" {
			orphans = append(orphans, orphanAnchor{anchorID: c.ID, storedRef: ref})
			continue
		}
		pool[targetName] = append(pool[targetName], c.ID)
	}
	return pool, orphans
}

// resolveTargetName maps a `container:<X>` reference (long ID, short
// ID prefix, or name) to the canonical first-name (without leading
// slash) of the live container it refers to. Returns "" if `ref`
// doesn't resolve.
func resolveTargetName(all []ContainerInfo, ref string) string {
	for _, c := range all {
		if c.ID == ref {
			return firstName(c)
		}
	}
	if len(ref) >= 12 {
		for _, c := range all {
			if strings.HasPrefix(c.ID, ref) {
				return firstName(c)
			}
		}
	}
	for _, c := range all {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == ref {
				return strings.TrimPrefix(n, "/")
			}
		}
	}
	return ""
}

// firstName returns the container's first name with the leading "/"
// stripped. Empty for containers with no names (transient daemon
// state; not expected in normal use).
func firstName(c ContainerInfo) string {
	for _, n := range c.Names {
		if n != "" {
			return strings.TrimPrefix(n, "/")
		}
	}
	return ""
}

// bootstrapRecheck verifies each pool entry against the live target.
// For each (target, anchorIDs) pair, look up the live target by name
// across the entire host; if found and its ID differs from what the
// anchor's stored NetworkMode references, restart the anchor.
//
// Non-blocking on errors: if the live container list is unavailable,
// log and return — the next poll will retry.
func (w *Watcher) bootstrapRecheck(ctx context.Context, source string) {
	w.mu.Lock()
	if len(w.pool) == 0 {
		w.mu.Unlock()
		return
	}
	poolSnapshot := make(targetPool, len(w.pool))
	for k, v := range w.pool {
		clone := make([]string, len(v))
		copy(clone, v)
		poolSnapshot[k] = clone
	}
	w.mu.Unlock()

	all, err := w.ops.ContainerList(ctx)
	if err != nil {
		slog.Warn("wrap-rebinder: bootstrap recheck list failed", "err", err, "source", source)
		return
	}

	for targetName, anchors := range poolSnapshot {
		currentID := resolveContainerByName(all, targetName)
		if currentID == "" {
			slog.Debug("wrap-rebinder: target not currently present; will react on its next start",
				"target", targetName)
			continue
		}
		for _, anchorID := range anchors {
			anchor := findByID(all, anchorID)
			if anchor == nil {
				// The anchor itself is gone — pool is stale, but the
				// next enumerate will rebuild. Skip.
				continue
			}
			storedRef, ok := strings.CutPrefix(anchor.NetworkMode, "container:")
			if !ok {
				continue
			}
			storedRef = strings.TrimSpace(storedRef)
			// storedRef is the value from docker — usually a long ID
			// (Docker stores the resolved ID, not the name, after a
			// successful start), occasionally a name on freshly-
			// resolved-by-name containers. Compare by ID first, then
			// fall back to name-match (a non-drift state where the
			// netmode is name-keyed because Docker never re-wrote it).
			if storedRef == currentID || storedRef == targetName {
				continue
			}
			slog.Info("wrap-rebinder: drift detected, recreating wrap-anchor",
				"target", targetName,
				"anchor", anchorID,
				"stored_ref", storedRef,
				"current_id", currentID,
				"source", source)
			w.recreateAnchor(ctx, anchorID, targetName, source)
		}
	}
}

// recreateAnchor invokes RecreateWithNetworkMode against the live
// adapter. Docker does not re-resolve `container:<X>` on plain
// ContainerRestart, so a restart against a dead target ID just
// fails with "No such container" and the anchor lands in `exited`.
// The only working recovery is remove+create+start with a fresh
// NetworkMode string — exactly what F-45's autostart.RecreateWith
// NetworkMode does for SA wrap deps. We pass the target's *name*
// (not its current ID) so Docker resolves it freshly on each
// future drift cycle.
//
// Rapid-restart suppression is keyed by target name — the trigger
// for back-to-back events is a target in a tight crash-loop, and
// that's what we want to dedup. Anchor IDs change on every recreate
// so keying dedup by anchor ID would never fire.
//
// Failures are logged at warn; the next event/poll retries.
func (w *Watcher) recreateAnchor(ctx context.Context, anchorID, targetName, source string) {
	dedupKey := "target:" + targetName + "|anchor:" + anchorID
	w.mu.Lock()
	if last, ok := w.lastRestartedAt[dedupKey]; ok && time.Since(last) < rapidRestartWindow {
		w.mu.Unlock()
		slog.Warn("wrap-rebinder: suppressing rapid recreate",
			"anchor", anchorID, "target", targetName,
			"since_last", time.Since(last), "source", source)
		return
	}
	w.lastRestartedAt[dedupKey] = time.Now()
	w.mu.Unlock()

	newNetMode := "container:" + targetName
	newID, err := w.ops.RecreateWithNetworkMode(ctx, anchorID, newNetMode, w.cfg.RestartTimeout)
	if err != nil {
		slog.Warn("wrap-rebinder: recreate failed",
			"anchor", anchorID, "target", targetName, "source", source, "err", err)
		return
	}
	slog.Info("wrap-rebinder: anchor recreated against live target",
		"old_id", anchorID, "new_id", newID,
		"target", targetName, "source", source)
}

// resolveContainerByName returns the container ID for the live
// container named `name` (any project), or "" if not found. Docker
// containers can have multiple names; we accept any match.
func resolveContainerByName(all []ContainerInfo, name string) string {
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return ""
	}
	// Long ID hit (the netmode field often stores the resolved ID).
	for _, c := range all {
		if c.ID == name {
			return c.ID
		}
	}
	// Short ID prefix hit.
	if len(name) >= 12 {
		for _, c := range all {
			if strings.HasPrefix(c.ID, name) {
				return c.ID
			}
		}
	}
	// Name hit.
	for _, c := range all {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == name {
				return c.ID
			}
		}
	}
	return ""
}

// findByID returns a pointer into `all` for the matching container,
// or nil. Used to refetch the wrap-anchor's current state during
// recheck.
func findByID(all []ContainerInfo, id string) *ContainerInfo {
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}

// ---- production Docker adapter -----------------------------------------

type dockerAdapter struct {
	cli *client.Client
}

func (a dockerAdapter) ContainerList(ctx context.Context) ([]ContainerInfo, error) {
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
			Labels:      c.Labels,
		})
	}
	return out, nil
}

// RecreateWithNetworkMode atomically inspects, removes, re-creates,
// and starts a wrap-anchor with HostConfig.NetworkMode replaced by
// newNetMode (typically `container:<target-name>`). Preserves
// everything else from the original Config and HostConfig — image,
// env, cmd, labels, capabilities, restart-policy, volume mounts. The
// name is preserved so siblings that resolve the anchor by name keep
// working (anchord's F-45 dependents, for example).
//
// timeout is forwarded as the stop-timeout for the Remove call.
//
// Mirrors internal/autostart/autostart.go's RecreateWithNetworkMode
// almost exactly — same primitive, different caller. Kept local to
// this package rather than extracted because (a) the autostart copy
// has its own subtle edge cases (NetworkingConfig handling) and (b)
// pulling autostart in as a dependency for one helper would bloat
// the package graph for no win.
func (a dockerAdapter) RecreateWithNetworkMode(ctx context.Context, id, newNetMode string, timeout time.Duration) (string, error) {
	insp, err := a.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", id, err)
	}
	if insp.Config == nil || insp.HostConfig == nil {
		return "", fmt.Errorf("inspect %s: missing Config or HostConfig", id)
	}
	newHostConfig := *insp.HostConfig
	newHostConfig.NetworkMode = container.NetworkMode(newNetMode)

	// Container-mode netmode forbids Config.Hostname and
	// Config.Domainname being set (the netns owner provides them).
	// ContainerInspect returns the auto-assigned hostname even for
	// container-mode containers, so ContainerCreate fails with
	// "conflicting options: hostname and the network mode" unless
	// we blank them here. Same applies to MacAddress, ExposedPorts
	// (top-level Config field; the in-HostConfig PortBindings is
	// already a no-op for container-mode and harmless to leave).
	newConfig := *insp.Config
	if strings.HasPrefix(newNetMode, "container:") {
		newConfig.Hostname = ""
		newConfig.Domainname = ""
		newConfig.MacAddress = ""
		newConfig.ExposedPorts = nil
	}

	name := strings.TrimPrefix(insp.Name, "/")

	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	if err := a.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		return "", fmt.Errorf("remove %s: %w", id, err)
	}
	// Containers in `container:X` mode share the target's namespace
	// and don't carry separate endpoint configs (Docker forbids
	// combining container-mode netmode with explicit network
	// attachments). NetworkingConfig is nil for the same reason
	// internal/autostart's recreate path uses nil here.
	resp, err := a.cli.ContainerCreate(ctx, &newConfig, &newHostConfig, nil, nil, name)
	if err != nil {
		return "", fmt.Errorf("recreate %s: %w", name, err)
	}
	if err := a.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start recreated %s: %w", name, err)
	}
	_ = secs // reserved for future use as graceful stop hint
	return resp.ID, nil
}

func (a dockerAdapter) Events(ctx context.Context) (<-chan EventMsg, <-chan error) {
	f := filters.NewArgs()
	f.Add("type", "container")
	f.Add("event", "start")
	rawMsgs, rawErrs := a.cli.Events(ctx, events.ListOptions{Filters: f})
	msgs := make(chan EventMsg, 8)
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
				if !errors.Is(e, context.Canceled) {
					errs <- e
				}
				return
			case m, ok := <-rawMsgs:
				if !ok {
					return
				}
				msgs <- EventMsg{
					Action:        string(m.Action),
					ContainerID:   m.Actor.ID,
					ContainerName: m.Actor.Attributes["name"],
					Project:       m.Actor.Attributes["com.docker.compose.project"],
				}
			}
		}
	}()
	return msgs, errs
}
