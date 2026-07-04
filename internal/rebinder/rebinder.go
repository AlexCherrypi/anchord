// Package rebinder implements SPEC F-48: an external-network follower
// auto-rebind sidecar.
//
// The rebinder lives in the follower's compose project (not the target
// stack's). Its single job is to keep a configured follower container
// correctly attached to a configured target network *name* as the
// target stack recreates that network underneath it — for example,
// after the target stack does `docker compose down/up` and Docker hands
// out a new network ID for the same name.
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
// # Release-before-recreate and settle-before-reattach (2026-07-04)
//
// The naive "reattach on every network create" behaviour caused a
// 7.5h Mailcow outage on 2026-07-04 via two distinct failure modes
// this package now defends against:
//
//   - ENDPOINT-PIN-DEADLOCK. Keeping the follower pinned to the target
//     network blocks the target stack's own `compose down/up`: Docker
//     refuses to remove a network that still has active endpoints
//     ("has active endpoints (...)"), so the *entire* target `up`
//     aborts — even though the follower belongs to a different project.
//     A failed network-remove fires no `destroy` event, so we cannot
//     wait for one. Instead the rebinder watches the network's endpoint
//     set and, the moment the follower becomes the *sole* remaining
//     endpoint (every project-owned container has left), proactively
//     disconnects ("parks") the follower so the target's teardown can
//     proceed. There is nothing to talk to on a network with no other
//     members, so parking costs no real connectivity.
//
//   - REATTACH-RACE. Reattaching ~1s after the `create` event — while
//     compose is still assigning (partly static) IPs to the target's
//     core containers — collides the follower's dynamic IPAM lease with
//     a later static assignment ("Address already in use"), again
//     aborting the target `up`. The rebinder now waits for the target
//     stack to *settle* (its endpoint set stops changing for a quiet
//     window) before reattaching, and the connect itself retries with
//     backoff on an address-in-use collision.
//
// Both behaviours are generic: they key only on "am I the sole endpoint"
// and "has the endpoint set stopped changing", never on the target's
// compose-project name, so every follow_network user benefits, not just
// Mailcow.
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

// Settle timing defaults (production). Kept as constants rather than env
// vars so operators need change nothing to pick up the fix: unset means
// "the better behaviour". The floor grace before the first settle probe
// reuses the existing ANCHORD_FOLLOW_EVENT_BACKOFF knob. Promote these
// to env vars if a production stack ever needs a longer/shorter window.
const (
	// defaultSettlePollInterval is how often, after a network-create,
	// the rebinder re-inspects the target network's endpoint set while
	// waiting for the target stack's `up` to quiesce.
	defaultSettlePollInterval = 1 * time.Second

	// defaultSettleQuietWindow is how long the target's foreign-endpoint
	// count must hold steady (and be non-zero) before the rebinder
	// considers the stack settled and safe to reattach into.
	defaultSettleQuietWindow = 3 * time.Second

	// defaultSettleMaxWait caps the total settle wait. Hit only when the
	// target never quiesces (e.g. a crash-loop); the rebinder reattaches
	// anyway and relies on the connect-retry backstop.
	defaultSettleMaxWait = 90 * time.Second

	// defaultConnectAttempts is the total number of NetworkConnect tries
	// (initial + retries) the reattach path makes when it keeps hitting
	// an "address already in use" IPAM collision with a still-churning
	// target `up`.
	defaultConnectAttempts = 4
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
	// Members maps the container *ID* of every endpoint currently
	// attached to the network to that container's name. Empty for a
	// freshly-created network with no members yet. Drives the park
	// decision (is the follower the sole remaining endpoint?) and the
	// settle decision (has the target stack finished attaching its own
	// containers?).
	Members map[string]string
}

// EventMsg is the rebinder-shaped network event. Tests emit these via
// a channel; production translates events.Message into this form.
//
// Action values observed:
//   - "create"     — a network with our name appeared (settle+reattach).
//   - "destroy"    — our network went away; the follower's attachment is
//     now invalid, so we mark it released and wait for the
//     paired create.
//   - "connect"    — a container joined our network. While parked, that
//     means the target may be returning on the same
//     network (no recreate) → settle+reattach. Otherwise
//     typically our own reattach echoing back.
//   - "disconnect" — a container left our network. Each one is a chance
//     the follower has become the sole remaining endpoint,
//     which is the release-before-recreate trigger.
type EventMsg struct {
	Action      string
	NetworkID   string
	Name        string
	ContainerID string // Actor.Attributes["container"] on connect/disconnect
}

// dockerOps is the slice of Docker surface this package uses. Kept
// narrow so unit tests don't drag in the real SDK.
type dockerOps interface {
	// NetworkInspectByName returns the current Docker ID for the
	// network with the given name plus its current endpoint set
	// (Members), or an error if no such network exists. Used for the
	// bootstrap recheck, the settle poll, the park decision, and at the
	// start of every reattach to discover the live ID.
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

	// nowSleep is the sleep used for the settle floor grace, the settle
	// poll cadence, and the connect-retry backoff. Tests stub it to a
	// no-op to keep timing deterministic.
	nowSleep func(context.Context, time.Duration)

	// Settle/retry tunables. Production values are the default* consts;
	// tests set them tiny via newWithOps so the settle loop resolves in
	// microseconds.
	settlePollInterval time.Duration
	settleQuietWindow  time.Duration
	settleMaxWait      time.Duration
	connectAttempts    int

	// parked is true when the follower is intentionally (or effectively)
	// detached from the target network and the rebinder is waiting to
	// reattach — set when the follower is released as the sole remaining
	// endpoint (park) or when the network is destroyed, cleared on a
	// successful reattach. Only ever touched from the Run goroutine
	// (bootstrapRecheck runs before consume; both run in Run's single
	// goroutine), so no locking is needed.
	parked bool
}

// New constructs a Watcher backed by a live Docker client.
func New(cli *client.Client, cfg *config.Rebinder) *Watcher {
	return &Watcher{
		ops:                dockerAdapter{cli: cli},
		cfg:                cfg,
		nowSleep:           ctxSleep,
		settlePollInterval: defaultSettlePollInterval,
		settleQuietWindow:  defaultSettleQuietWindow,
		settleMaxWait:      defaultSettleMaxWait,
		connectAttempts:    defaultConnectAttempts,
	}
}

// newWithOps is the test seam. Settle timings are tiny so the settle
// loop resolves near-instantly under the no-op sleep.
func newWithOps(ops dockerOps, cfg *config.Rebinder) *Watcher {
	return &Watcher{
		ops:                ops,
		cfg:                cfg,
		nowSleep:           func(context.Context, time.Duration) {},
		settlePollInterval: time.Millisecond,
		settleQuietWindow:  time.Millisecond,
		settleMaxWait:      20 * time.Millisecond,
		connectAttempts:    3,
	}
}

// Run runs the bootstrap recheck once, then streams Docker network
// events, dispatching each to the settle/reattach path, the park path,
// or to log-only as the SPEC requires. Returns ctx.Err() on cancel. On
// terminal event-stream errors it waits 2s and re-subscribes — the same
// pattern internal/autostart and internal/discovery use.
//
// The event subscription is opened exactly once per outer-loop
// iteration and never from inside consume; a terminal error returns
// control here and only here re-subscribes. Re-opening the stream from
// within the message loop is a known leak pattern and is deliberately
// avoided.
func (w *Watcher) Run(ctx context.Context) error {
	slog.Info("external-rebinder starting",
		"follow_network", w.cfg.FollowNetwork,
		"follow_target", w.cfg.FollowTarget,
		"restart", w.cfg.Restart,
		"event_backoff", w.cfg.EventBackoff,
		"settle_quiet_window", w.settleQuietWindow,
		"settle_max_wait", w.settleMaxWait)

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
//
// The reattach here is immediate (no settle wait): a divergence found at
// startup means the target stack has *already* recreated its network and
// is presumably up and stable, so there is nothing to wait to settle —
// the connect-retry backstop still covers a rare in-flight collision.
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
//
// The create and connect-while-parked handlers block for the duration
// of the settle wait. That is intentional: while settling, events that
// pile up in the stream buffer are back-pressured (never dropped) and
// replayed when settle finishes, and the settle wait is bounded by
// settleMaxWait. This keeps the loop single-threaded and avoids ever
// re-subscribing to Events from inside the loop.
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
				slog.Info("target network created; waiting for target stack to settle before reattach",
					"network", msg.Name, "network_id", msg.NetworkID)
				w.settleAndReattach(ctx, "event/create")
				if ctx.Err() != nil {
					return ctx.Err()
				}
			case "destroy":
				// A destroy means our attachment (if any) is now invalid.
				// In the common release-before-recreate path we already
				// parked before this fires; mark parked either way so the
				// paired create does a full settle+reattach.
				w.parked = true
				slog.Info("target network destroyed; follower released, awaiting next create",
					"network", msg.Name)
			case "disconnect":
				w.maybePark(ctx, msg)
			case "connect":
				if w.parked {
					slog.Info("connect on target network while parked; target may be returning, settling",
						"network", msg.Name, "container", shortID(msg.ContainerID))
					w.settleAndReattach(ctx, "event/connect-while-parked")
					if ctx.Err() != nil {
						return ctx.Err()
					}
				} else {
					slog.Debug("rebinder observed connect on target network",
						"network", msg.Name, "container", shortID(msg.ContainerID))
				}
			}
		}
	}
}

// membership is a snapshot of the follower's relationship to the target
// network at one instant: the live network ID, the resolved follower ID
// (may be "" if not yet present), whether the follower is currently an
// endpoint, and how many *other* (foreign) endpoints the network holds.
type membership struct {
	netID            string
	followerID       string
	followerAttached bool
	foreignCount     int
}

// inspectMembership resolves the current membership snapshot. Returns
// ok=false (and logs at debug) when the network or container list can't
// be read right now — callers treat that as "retry on the next event"
// rather than acting on stale data.
func (w *Watcher) inspectMembership(ctx context.Context) (membership, bool) {
	netInfo, err := w.ops.NetworkInspectByName(ctx, w.cfg.FollowNetwork)
	if err != nil {
		slog.Debug("membership: network inspect failed", "network", w.cfg.FollowNetwork, "err", err)
		return membership{}, false
	}
	all, err := w.ops.ContainerList(ctx)
	if err != nil {
		slog.Debug("membership: container list failed", "err", err)
		return membership{}, false
	}
	m := membership{
		netID:      netInfo.ID,
		followerID: resolveFollower(all, w.cfg.FollowTarget, w.cfg.SelfProject),
	}
	for id := range netInfo.Members {
		if m.followerID != "" && idMatch(id, m.followerID) {
			m.followerAttached = true
			continue
		}
		m.foreignCount++
	}
	return m, true
}

// maybePark implements release-before-recreate. On each disconnect for
// our network it checks whether the follower has become the sole
// remaining endpoint; if so it releases (disconnects) the follower so
// the target stack's `network rm` can proceed. A network whose only
// member is the follower has nothing for the follower to talk to, so
// releasing costs no real connectivity — and holding on is exactly what
// deadlocked the target's `compose up` in the 2026-07-04 incident.
func (w *Watcher) maybePark(ctx context.Context, msg EventMsg) {
	if w.parked {
		return // already released; nothing to do
	}
	m, ok := w.inspectMembership(ctx)
	if !ok {
		return
	}
	if !m.followerAttached {
		// Someone (or something) already detached the follower; record
		// the released state so the next create fully reattaches.
		w.parked = true
		slog.Info("park: follower no longer attached to target network; marking released",
			"network", w.cfg.FollowNetwork, "trigger_container", shortID(msg.ContainerID))
		return
	}
	if m.foreignCount > 0 {
		// The target still has its own endpoints — this was just one of
		// several containers leaving. Stay attached.
		slog.Debug("disconnect on target network; target still has endpoints, staying attached",
			"network", w.cfg.FollowNetwork, "foreign_endpoints", m.foreignCount)
		return
	}
	w.releaseFollower(ctx, m.followerID, "sole-remaining-endpoint")
}

// releaseFollower disconnects the follower from the target network and
// records the parked state. Best-effort: a "not attached" error is
// folded into success (we wanted it gone anyway).
func (w *Watcher) releaseFollower(ctx context.Context, followerID, reason string) {
	if err := w.ops.NetworkDisconnect(ctx, w.cfg.FollowNetwork, followerID); err != nil && !isAlreadyNotAttached(err) {
		slog.Warn("park: disconnect of sole-remaining follower failed",
			"network", w.cfg.FollowNetwork, "follower", followerID, "reason", reason, "err", err)
		return
	}
	w.parked = true
	slog.Info("park: released follower to unblock target network teardown",
		"network", w.cfg.FollowNetwork, "follower", followerID, "reason", reason)
}

// settleAndReattach waits for the target stack to quiesce, then reattach.
// It exists so that a `network create` (or a target returning on the same
// network) does not race the target's own IP assignment — see the
// REATTACH-RACE note on the package doc.
func (w *Watcher) settleAndReattach(ctx context.Context, source string) {
	switch w.waitForSettle(ctx) {
	case settleCancelled:
		return
	case settleAborted:
		// The network vanished (or went unreadable) mid-settle — most
		// likely the target did another down/up cycle. Don't reattach
		// into a moving target; the next create event drives the retry.
		slog.Info("settle aborted: target network not inspectable mid-settle; awaiting next event",
			"source", source)
		return
	case settleTimedOut:
		slog.Warn("settle timed out; reattaching anyway (connect-retry backstop covers residual collisions)",
			"source", source, "max_wait", w.settleMaxWait)
	case settleReady:
		slog.Info("target stack settled; reattaching follower", "source", source)
	}
	w.reattach(ctx, source)
}

// settleOutcome is the result of waitForSettle.
type settleOutcome int

const (
	settleReady     settleOutcome = iota // endpoint set is non-zero and stable
	settleTimedOut                       // never quiesced within settleMaxWait
	settleAborted                        // network became unreadable mid-settle
	settleCancelled                      // context cancelled
)

// waitForSettle blocks until the target network's foreign-endpoint count
// has held steady (and non-zero) for settleQuietWindow, or until
// settleMaxWait elapses, or the network becomes unreadable, or ctx is
// cancelled. It first honours the ANCHORD_FOLLOW_EVENT_BACKOFF floor so
// Docker has finished wiring the fresh bridge before the first probe.
//
// The loop accounts elapsed/quiet time by adding the poll interval each
// iteration rather than reading a wall clock, so tests that stub nowSleep
// to a no-op still converge deterministically.
func (w *Watcher) waitForSettle(ctx context.Context) settleOutcome {
	if d := w.cfg.EventBackoff; d > 0 {
		w.nowSleep(ctx, d)
		if ctx.Err() != nil {
			return settleCancelled
		}
	}
	last := -1
	var stable, elapsed time.Duration
	for {
		m, ok := w.inspectMembership(ctx)
		if !ok {
			return settleAborted
		}
		c := m.foreignCount
		if c > 0 && c == last {
			stable += w.settlePollInterval
			if stable >= w.settleQuietWindow {
				return settleReady
			}
		} else {
			if c != last {
				slog.Debug("settle: target endpoint count changed",
					"network", w.cfg.FollowNetwork, "from", last, "to", c)
			}
			last = c
			stable = 0
		}
		if elapsed >= w.settleMaxWait {
			return settleTimedOut
		}
		w.nowSleep(ctx, w.settlePollInterval)
		if ctx.Err() != nil {
			return settleCancelled
		}
		elapsed += w.settlePollInterval
	}
}

// reattach is the core repair primitive. Resolves the follower fresh
// (it may have been recreated during the window), re-inspects the
// network to get the *current* ID, then disconnect (best-effort) and
// connect against the fresh ID. The connect retries with backoff on an
// "address already in use" IPAM collision — the residual REATTACH-RACE
// surface after settle. On Restart=true, follows with a container
// restart on a clean connect.
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
	// Retry on address-in-use: even after settle, a slow static
	// assignment on the target side can transiently own the address
	// IPAM would hand us.
	netID := netInfo.ID
	if !w.connectWithRetry(ctx, netID, followerID, source) {
		return
	}
	w.parked = false
	slog.Info("reattach: follower connected to current network ID",
		"network", w.cfg.FollowNetwork, "network_id", netID,
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

// connectWithRetry issues NetworkConnect, treating "already exists" as
// success and retrying with a backoff on "address already in use" up to
// connectAttempts times. Returns true on a connected (or already-
// connected) endpoint, false on a terminal failure (already warn-logged).
func (w *Watcher) connectWithRetry(ctx context.Context, networkID, followerID, source string) bool {
	for attempt := 1; ; attempt++ {
		err := w.ops.NetworkConnect(ctx, networkID, followerID)
		if err == nil || isAlreadyConnected(err) {
			return true
		}
		if isAddressInUse(err) && attempt < w.connectAttempts {
			slog.Warn("reattach: connect hit address-in-use; target still assigning IPs, retrying after backoff",
				"network", w.cfg.FollowNetwork, "network_id", networkID,
				"follower", followerID, "source", source,
				"attempt", attempt, "of", w.connectAttempts, "backoff", w.cfg.EventBackoff)
			w.nowSleep(ctx, w.cfg.EventBackoff)
			if ctx.Err() != nil {
				return false
			}
			// Re-resolve the live ID: during a still-churning up the ID
			// is stable, but if the target recreated the network again
			// we want the newest one.
			if ni, e := w.ops.NetworkInspectByName(ctx, w.cfg.FollowNetwork); e == nil {
				networkID = ni.ID
			}
			continue
		}
		slog.Warn("reattach: connect failed",
			"network", w.cfg.FollowNetwork, "network_id", networkID,
			"follower", followerID, "source", source, "err", err)
		return false
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

// idMatch reports whether two Docker container identifiers refer to the
// same container. Network endpoint maps are keyed by full 64-char IDs
// and ContainerList also returns full IDs, so an exact match is the
// common case; the prefix arms tolerate a short-ID showing up on either
// side (min 12 hex chars, Docker's short-ID length) without matching on
// trivially short strings.
func idMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) >= 12 && strings.HasPrefix(b, a) {
		return true
	}
	if len(b) >= 12 && strings.HasPrefix(a, b) {
		return true
	}
	return false
}

// shortID trims a container ID to its 12-char short form for logs.
// Leaves shorter or empty strings untouched.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
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

// isAddressInUse reports whether a NetworkConnect error is the IPAM
// collision the REATTACH-RACE produces — the target stack claimed (or
// is mid-claim on) the address IPAM would otherwise hand the follower.
// Docker surfaces this as "Address already in use" from the kernel/
// libnetwork layer.
func isAddressInUse(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "address is in use") ||
		strings.Contains(s, "no available ipv4 addresses")
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
	members := make(map[string]string, len(n.Containers))
	for id, ep := range n.Containers {
		members[id] = strings.TrimPrefix(ep.Name, "/")
	}
	return NetworkInfo{ID: n.ID, Name: n.Name, Members: members}, nil
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
					Action:      string(m.Action),
					NetworkID:   m.Actor.ID,
					Name:        m.Actor.Attributes["name"],
					ContainerID: m.Actor.Attributes["container"],
				}
			}
		}
	}()
	return msgs, errs
}
