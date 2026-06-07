// Package rebinder implements SPEC F-48: an external-network follower
// auto-rebind sidecar.
//
// The rebinder lives in the follower's compose project (not the target
// stack's). Its single job is to watch Docker network events for a
// configured target network *name* and reattach a configured follower
// container to the current network ID every time that ID changes — for
// example, after the target stack does `docker compose down/up` and
// Docker hands out a new network ID for the same name.
//
// Concrete incident this exists to prevent (2026-06-07): Mailcow's
// weekly auto-update recreated its bridge network. The
// `authentik-mailbox-sync` follower (own compose project) was attached
// via `external: true` and ended up holding a stale Docker network ID.
// Outbound packets to nginx-mailcow silently timed out; IMAP auth
// broke for ~24h until manually stopped+started.
//
// The mental model is a generalisation of F-43/F-45: those features
// handle "the netns my sibling joined is gone"; F-48 handles "the
// bridge network my sibling joined is gone." Both are recreate-side-
// effects on a peer stack the rebinder's compose project doesn't own.
//
// One sidecar = one (network, target) pair. Multiple pairs ⇒ multiple
// sidecars. The binary is small enough (~30 MB RSS, single Goroutine
// per long-lived loop) that this trades complexity for clarity.
package rebinder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// ContainerInfo is the narrow projection of a Docker container view
// the rebinder uses. Production builds it from container.Summary +
// container.InspectResponse; tests construct it directly.
type ContainerInfo struct {
	ID     string
	Names  []string
	Labels map[string]string
	// Networks maps the network *name* the container is attached to
	// to the Docker network *ID* recorded at attach time. Empty for
	// freshly-listed containers (we only have IDs after Inspect).
	Networks map[string]string
}

// NetworkInfo is the narrow projection of a Docker network used by the
// rebinder.
type NetworkInfo struct {
	ID   string
	Name string
}

// EventMsg is the rebinder-shaped network event. Tests emit these via
// a channel; production translates events.Message into this form.
//
// Action values observed:
//   - "create"    — a network with our name appeared (rebind trigger).
//   - "destroy"   — our network went away; log only, follower's own
//                   restart-policy carries the gap.
//   - "connect"   — typically our own reattach echoing back.
//   - "disconnect" — same.
type EventMsg struct {
	Action    string
	NetworkID string
	Name      string
}

// dockerOps is the slice of Docker surface this package uses. Kept
// narrow so unit tests don't drag in the real SDK.
type dockerOps interface {
	// NetworkInspectByName returns the current Docker ID for the
	// network with the given name, or an error if no such network
	// exists. Used for the bootstrap recheck and at the start of
	// every reattach to discover the live ID.
	NetworkInspectByName(ctx context.Context, name string) (NetworkInfo, error)

	// ContainerList returns every container Docker knows about. The
	// rebinder uses it to resolve follower-by-service-name within the
	// sidecar's own compose project, and as a fallback for follower-
	// by-container-name when no compose labels are present.
	ContainerList(ctx context.Context) ([]ContainerInfo, error)

	// ContainerInspect returns the network attachments of a single
	// container by ID or name. Used by the bootstrap recheck to
	// compare the follower's stored network ID against the live one.
	ContainerInspect(ctx context.Context, idOrName string) (ContainerInfo, error)

	// NetworkConnect attaches the given container to the network
	// identified by ID (NOT name — IDs disambiguate when a stale
	// network with the same name still ghost-exists in the daemon
	// state). Idempotent at the daemon: already-attached returns an
	// error the caller must classify as success.
	NetworkConnect(ctx context.Context, networkID, containerID string) error

	// NetworkDisconnect detaches the given container from the network
	// identified by name (Docker's disconnect API resolves by name
	// here, since we may not have the *current* ID and the stale one
	// is gone). Idempotent at the daemon: not-attached returns an
	// error the caller must classify as success.
	NetworkDisconnect(ctx context.Context, networkName, containerID string) error

	// ContainerRestart bounces a container. Used only when the
	// rebinder's Restart toggle is on.
	ContainerRestart(ctx context.Context, containerID string) error

	// Events subscribes to Docker network events. The returned
	// channels mirror docker SDK semantics: msgs delivers events,
	// errs delivers terminal errors; on either error or msgs-closed
	// the caller re-subscribes.
	Events(ctx context.Context) (<-chan EventMsg, <-chan error)
}

// Watcher is the long-running F-48 worker. One instance per follower
// stack.
type Watcher struct {
	ops dockerOps
	cfg *config.Rebinder

	// nowSleep is the sleep used to honour cfg.EventBackoff between a
	// network-create event and the reattach call. Tests stub it to
	// keep deterministic timing.
	nowSleep func(context.Context, time.Duration)
}

// New constructs a Watcher backed by a live Docker client.
func New(cli *client.Client, cfg *config.Rebinder) *Watcher {
	return &Watcher{
		ops:      dockerAdapter{cli: cli},
		cfg:      cfg,
		nowSleep: ctxSleep,
	}
}

// newWithOps is the test seam.
func newWithOps(ops dockerOps, cfg *config.Rebinder) *Watcher {
	return &Watcher{
		ops:      ops,
		cfg:      cfg,
		nowSleep: func(context.Context, time.Duration) {},
	}
}

// Run runs the bootstrap recheck once, then streams Docker network
// events, dispatching each to the reattach path or to log-only as the
// SPEC requires. Returns ctx.Err() on cancel. On terminal event-stream
// errors it waits 2s and re-subscribes — the same pattern
// internal/autostart and internal/discovery use.
func (w *Watcher) Run(ctx context.Context) error {
	slog.Info("external-rebinder starting",
		"follow_network", w.cfg.FollowNetwork,
		"follow_target", w.cfg.FollowTarget,
		"restart", w.cfg.Restart,
		"event_backoff", w.cfg.EventBackoff)

	w.bootstrapRecheck(ctx)

	for {
		msgs, errs := w.ops.Events(ctx)
		err := w.consume(ctx, msgs, errs)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("rebinder event stream error, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// bootstrapRecheck closes the race where the rebinder itself was down
// while the target's network got recreated — the destroy/create
// events are already past, but the follower's stored network ID
// diverges from the live one, which is observable. Non-blocking on
// errors: anything we can't inspect right now (follower not yet up,
// network not yet up) we just log and skip; the first relevant event
// will re-trigger the reattach.
func (w *Watcher) bootstrapRecheck(ctx context.Context) {
	netInfo, err := w.ops.NetworkInspectByName(ctx, w.cfg.FollowNetwork)
	if err != nil {
		slog.Warn("bootstrap recheck: target network not inspectable; will wait for events",
			"network", w.cfg.FollowNetwork, "err", err)
		return
	}
	all, err := w.ops.ContainerList(ctx)
	if err != nil {
		slog.Warn("bootstrap recheck: container list failed; will wait for events",
			"err", err)
		return
	}
	followerID := resolveFollower(all, w.cfg.FollowTarget, w.cfg.SelfProject)
	if followerID == "" {
		slog.Warn("bootstrap recheck: follower not found; will wait for events",
			"target", w.cfg.FollowTarget, "self_project", w.cfg.SelfProject)
		return
	}
	follower, err := w.ops.ContainerInspect(ctx, followerID)
	if err != nil {
		slog.Warn("bootstrap recheck: follower inspect failed; will wait for events",
			"target", w.cfg.FollowTarget, "err", err)
		return
	}
	storedID, attached := follower.Networks[w.cfg.FollowNetwork]
	if attached && storedID == netInfo.ID {
		slog.Info("bootstrap recheck: follower already attached to current network ID",
			"target", w.cfg.FollowTarget, "network_id", netInfo.ID)
		return
	}
	slog.Info("bootstrap recheck: divergence detected, reattaching",
		"target", w.cfg.FollowTarget,
		"network", w.cfg.FollowNetwork,
		"stored_id", storedID,
		"current_id", netInfo.ID,
		"attached", attached)
	w.reattach(ctx, "bootstrap")
}

// consume reads one event at a time, dispatching by Action and
// filtering by network name. Returns on context cancel or terminal
// channel error.
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
			if msg.Name != w.cfg.FollowNetwork {
				continue
			}
			switch msg.Action {
			case "create":
				w.nowSleep(ctx, w.cfg.EventBackoff)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Info("target network created, reattaching follower",
					"network", msg.Name, "network_id", msg.NetworkID)
				w.reattach(ctx, "event/create")
			case "destroy":
				slog.Info("target network destroyed; follower will be stranded until next create",
					"network", msg.Name)
			case "connect", "disconnect":
				slog.Debug("rebinder observed self-connect/disconnect",
					"action", msg.Action, "network", msg.Name)
			}
		}
	}
}

// reattach is the core repair primitive. Resolves the follower fresh
// (it may have been recreated during the window), re-inspects the
// network to get the *current* ID, then disconnect (best-effort) and
// connect against the fresh ID. On Restart=true, follows with a
// container.Restart.
//
// Failures are warn-logged and the function returns. The next event
// (or the operator) re-runs the path; anchord does not retry-in-loop
// because a stuck retry loop competing with docker network state
// changes is a worse mode than waiting for the next event.
func (w *Watcher) reattach(ctx context.Context, source string) {
	netInfo, err := w.ops.NetworkInspectByName(ctx, w.cfg.FollowNetwork)
	if err != nil {
		slog.Warn("reattach: network inspect failed",
			"network", w.cfg.FollowNetwork, "source", source, "err", err)
		return
	}
	all, err := w.ops.ContainerList(ctx)
	if err != nil {
		slog.Warn("reattach: container list failed",
			"source", source, "err", err)
		return
	}
	followerID := resolveFollower(all, w.cfg.FollowTarget, w.cfg.SelfProject)
	if followerID == "" {
		slog.Warn("reattach: follower not found",
			"target", w.cfg.FollowTarget,
			"self_project", w.cfg.SelfProject,
			"source", source)
		return
	}

	// Disconnect by name — Docker resolves by name here and tolerates
	// the "not attached" case which the helper folds into success.
	if err := w.ops.NetworkDisconnect(ctx, w.cfg.FollowNetwork, followerID); err != nil && !isAlreadyNotAttached(err) {
		// Not fatal — Docker may have already cleaned the endpoint
		// after the destroy. Log at info (visibility) and proceed.
		slog.Info("reattach: disconnect returned error (continuing)",
			"network", w.cfg.FollowNetwork, "follower", followerID,
			"source", source, "err", err)
	}

	// Connect by ID — guarantees we attach to the *new* network even
	// in the unlikely case Docker still has stale name-keyed state.
	if err := w.ops.NetworkConnect(ctx, netInfo.ID, followerID); err != nil && !isAlreadyConnected(err) {
		slog.Warn("reattach: connect failed",
			"network", w.cfg.FollowNetwork, "network_id", netInfo.ID,
			"follower", followerID, "source", source, "err", err)
		return
	}
	slog.Info("reattach: follower connected to current network ID",
		"network", w.cfg.FollowNetwork, "network_id", netInfo.ID,
		"follower", followerID, "source", source)

	if w.cfg.Restart {
		if err := w.ops.ContainerRestart(ctx, followerID); err != nil {
			slog.Warn("reattach: follower restart failed",
				"follower", followerID, "source", source, "err", err)
			return
		}
		slog.Info("reattach: follower restarted",
			"follower", followerID, "source", source)
	}
}

// resolveFollower locates the follower container ID using two
// strategies, in order:
//
//  1. Compose service name match: looks for a container in the
//     sidecar's own compose project whose
//     com.docker.compose.service label equals target. This is the
//     preferred form because it survives compose-driven renames.
//  2. Bare container name match: looks for any container whose
//     Names list contains target (with or without the docker "/"
//     prefix). Fallback for non-compose deployments.
//
// Returns "" if neither strategy finds a candidate. The caller
// is expected to log and skip — the follower may yet appear.
func resolveFollower(all []ContainerInfo, target, selfProject string) string {
	target = strings.TrimPrefix(target, "/")
	if target == "" {
		return ""
	}
	// Strategy 1: compose service name in self project.
	if selfProject != "" {
		for _, c := range all {
			if c.Labels["com.docker.compose.project"] != selfProject {
				continue
			}
			if c.Labels["com.docker.compose.service"] == target {
				return c.ID
			}
		}
	}
	// Strategy 2: container name fallback.
	for _, c := range all {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == target {
				return c.ID
			}
		}
	}
	return ""
}

// isAlreadyConnected reports whether a NetworkConnect error means
// "endpoint already exists on this network". Docker phrases it a few
// ways across versions; we match on the substring most stable across
// engines.
func isAlreadyConnected(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already exists") ||
		strings.Contains(s, "endpoint with name") && strings.Contains(s, "exists")
}

// isAlreadyNotAttached reports whether a NetworkDisconnect error
// means the container wasn't attached to the named network. Same
// stability caveat as isAlreadyConnected.
func isAlreadyNotAttached(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "is not connected") ||
		strings.Contains(s, "not connected to network") ||
		strings.Contains(s, "no such network endpoint")
}

// ctxSleep sleeps for d or until ctx is cancelled, whichever comes
// first. The production nowSleep so backoffs respect shutdown.
func ctxSleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// ---- production Docker adapter -----------------------------------------

type dockerAdapter struct {
	cli *client.Client
}

func (a dockerAdapter) NetworkInspectByName(ctx context.Context, name string) (NetworkInfo, error) {
	n, err := a.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		return NetworkInfo{}, fmt.Errorf("NetworkInspect(%q): %w", name, err)
	}
	return NetworkInfo{ID: n.ID, Name: n.Name}, nil
}

func (a dockerAdapter) ContainerList(ctx context.Context) ([]ContainerInfo, error) {
	list, err := a.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("ContainerList: %w", err)
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, c := range list {
		out = append(out, ContainerInfo{
			ID:     c.ID,
			Names:  c.Names,
			Labels: c.Labels,
		})
	}
	return out, nil
}

func (a dockerAdapter) ContainerInspect(ctx context.Context, idOrName string) (ContainerInfo, error) {
	insp, err := a.cli.ContainerInspect(ctx, idOrName)
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("ContainerInspect(%q): %w", idOrName, err)
	}
	out := ContainerInfo{
		ID:       insp.ID,
		Networks: map[string]string{},
	}
	if insp.Name != "" {
		out.Names = []string{insp.Name}
	}
	if insp.Config != nil {
		out.Labels = insp.Config.Labels
	}
	if insp.NetworkSettings != nil {
		for name, n := range insp.NetworkSettings.Networks {
			if n == nil {
				continue
			}
			out.Networks[name] = n.NetworkID
		}
	}
	return out, nil
}

func (a dockerAdapter) NetworkConnect(ctx context.Context, networkID, containerID string) error {
	return a.cli.NetworkConnect(ctx, networkID, containerID, nil)
}

func (a dockerAdapter) NetworkDisconnect(ctx context.Context, networkName, containerID string) error {
	// Force=true: even if the endpoint state is partially stale (which
	// is exactly the situation that motivated this feature), drop it.
	return a.cli.NetworkDisconnect(ctx, networkName, containerID, true)
}

func (a dockerAdapter) ContainerRestart(ctx context.Context, containerID string) error {
	return a.cli.ContainerRestart(ctx, containerID, container.StopOptions{})
}

func (a dockerAdapter) Events(ctx context.Context) (<-chan EventMsg, <-chan error) {
	f := filters.NewArgs()
	f.Add("type", "network")
	f.Add("event", "create")
	f.Add("event", "destroy")
	f.Add("event", "connect")
	f.Add("event", "disconnect")
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
				if !errors.Is(e, context.Canceled) {
					errs <- e
				}
				return
			case m, ok := <-rawMsgs:
				if !ok {
					return
				}
				msgs <- EventMsg{
					Action:    string(m.Action),
					NetworkID: m.Actor.ID,
					Name:      m.Actor.Attributes["name"],
				}
			}
		}
	}()
	return msgs, errs
}
