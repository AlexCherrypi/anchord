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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"

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

// CreateSpec is the minimal payload Watcher needs to create an
// F-45-managed service-anchor container. Production dockerAdapter
// translates this into the SDK's container.Config / HostConfig.
// Tests construct it directly and assert against it.
type CreateSpec struct {
	Name        string
	Image       string
	Env         []string          // "K=V" pairs in deterministic order
	Labels      map[string]string // includes com.docker.compose.project + anchord.managed-by
	NetworkMode string            // "container:<Target>"
	CapAdd      []string          // typically ["NET_ADMIN"]
	Restart     string            // "unless-stopped"
}

// SelfInfo is what Watcher.Run can learn about its own container
// once at startup — used to fill F-45 defaults (image, IP on shared
// network) when the operator hasn't supplied them.
type SelfInfo struct {
	Image            string            // anchord's own image, used as default for ManagedSA.Image
	IPsByNetwork     map[string]string // network name -> IP (used to default GatewayIP)
	ComposeProject   string            // copied into the managed SA's labels for clean teardown
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

	// Create issues a docker container.create call from the given
	// recipe and returns the new container ID. F-45 calls Create
	// followed by Start; idempotency on duplicate names is handled
	// at the call site by inspecting first.
	Create(ctx context.Context, spec CreateSpec) (string, error)

	// InspectSelf returns minimal info about the anchord container
	// itself — used to default F-45 fields. Called once at Watcher
	// startup (if the recipe is active).
	InspectSelf(ctx context.Context) (SelfInfo, error)

	// Events subscribes to Docker container.start events. The
	// returned channels mirror docker SDK semantics: msgs delivers
	// events, errs delivers terminal errors (caller is expected to
	// re-subscribe on error).
	Events(ctx context.Context) (<-chan EventMsg, <-chan error)
}

// Watcher is the live F-43 / F-45 worker. One instance per
// network-anchor.
type Watcher struct {
	ops dockerOps

	// recipe is the optional F-45 managed-service-anchor recipe.
	// When recipe.Active() is false the watcher behaves as pure
	// F-43 (start existing Created-state siblings only).
	recipe config.ManagedSARecipe

	// sharedNetFn returns the network whose IP we use to default
	// ManagedSA.GatewayIP. Called lazily on every F-45 createAndStart
	// so the watcher always sees the F-44 picker's *current* choice,
	// not a stale startup-time snapshot. Returning "" means "not yet
	// known" — buildSpec then errors out and the event-driven
	// watcher will retry on the next sibling-start. May be nil for
	// pure-F-43 stacks; buildSpec only consults it when GatewayIP
	// is unset.
	sharedNetFn func() string
}

// New constructs a Watcher backed by a live Docker client. recipe
// activates F-45 when recipe.Active() is true.
func New(cli *client.Client, recipe config.ManagedSARecipe) *Watcher {
	return &Watcher{
		ops:    dockerAdapter{cli: cli},
		recipe: recipe,
	}
}

// SetSharedNetworkFunc registers a callback the watcher consults each
// time it needs the F-44 picker's current choice. main.go wires this
// to picker.Chosen so the picker can settle on a later reconcile and
// the watcher picks up the new value the next time a sibling start
// fires — without needing a notify channel. Safe to call before Run.
//
// Passing nil disables the lazy lookup (pure-F-43 mode).
func (w *Watcher) SetSharedNetworkFunc(fn func() string) { w.sharedNetFn = fn }

// sharedNet returns the picker's current choice, or "" when no
// callback has been wired or the picker hasn't settled yet.
func (w *Watcher) sharedNet() string {
	if w.sharedNetFn == nil {
		return ""
	}
	return w.sharedNetFn()
}

// newWithOps is the test seam.
func newWithOps(ops dockerOps) *Watcher { return &Watcher{ops: ops} }

// newWithOpsAndRecipe is the test seam for F-45. sharedNet is the
// fixed value the watcher will report as the picker's choice — pass
// "" to simulate the not-yet-settled state.
func newWithOpsAndRecipe(ops dockerOps, recipe config.ManagedSARecipe, sharedNet string) *Watcher {
	w := &Watcher{ops: ops, recipe: recipe}
	if sharedNet != "" {
		w.sharedNetFn = func() string { return sharedNet }
	}
	return w
}

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
// to one of them. With an active F-45 recipe also runs the
// create-then-start path against the configured target.
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
		w.maybeManage(ctx, all, t, "backfill")
	}
}

// handleTargetStart is the per-event entry: a target just started,
// re-scan and trigger any matching Created siblings, and run the
// F-45 create-then-start path if the recipe applies.
func (w *Watcher) handleTargetStart(ctx context.Context, target ContainerInfo) {
	all, err := w.ops.List(ctx)
	if err != nil {
		slog.Warn("autostart per-event list failed", "err", err)
		return
	}
	for _, sib := range matchSiblings(all, target) {
		w.start(ctx, sib, target, "event")
	}
	w.maybeManage(ctx, all, target, "event")
}

// maybeManage is the F-45 create-then-start dispatcher.
//
// Pre-conditions: recipe must be Active() and the just-started
// `target` must reference the recipe's configured Target (by name or
// ID). Otherwise this is a no-op.
//
// Resolution order on a match:
//   - If the managed service-anchor already exists AND is running →
//     debug log, nothing to do.
//   - If it exists in Created state → existing F-43 start path will
//     have handled it via matchSiblings above; we skip the second
//     create (idempotent guard).
//   - If it doesn't exist → resolve runtime defaults
//     (image=self.Image, gateway_ip=self IP on sharedNet), build the
//     CreateSpec, call ops.Create then ops.Start.
//
// All failures are logged at warn-level and the watcher keeps
// running. F-45 is a quality-of-life feature; surfacing its
// problems to the operator without killing the data plane is the
// right tradeoff.
func (w *Watcher) maybeManage(ctx context.Context, all []ContainerInfo, target ContainerInfo, source string) {
	if !w.recipe.Active() {
		return
	}
	if !targetMatchesRecipe(target, w.recipe.Target) {
		return
	}

	// Find the managed SA in the current list.
	var existing *ContainerInfo
	for i := range all {
		if hasName(all[i], w.recipe.Name) {
			existing = &all[i]
			break
		}
	}
	if existing != nil {
		switch strings.ToLower(existing.State) {
		case "running":
			slog.Debug("managed service-anchor already running",
				"name", w.recipe.Name, "target", w.recipe.Target)
			return
		case "created":
			// The F-43 path above already issued Start on this sibling
			// (matchSiblings will have found it). No need to repeat —
			// duplicate Start is harmless but adds log noise.
			return
		default:
			// Exited / restarting / paused → try a Start; Docker is
			// idempotent and best-equipped to handle these states.
			w.start(ctx, existing.ID, target, "managed/"+source)
			return
		}
	}

	// F-45 NEW path: create from recipe then start.
	w.createAndStartManaged(ctx, target, source)
}

// createAndStartManaged inspects self for default values, assembles
// the CreateSpec, calls Create + Start.
func (w *Watcher) createAndStartManaged(ctx context.Context, target ContainerInfo, source string) {
	self, err := w.ops.InspectSelf(ctx)
	if err != nil {
		slog.Warn("F-45 self-inspect failed; cannot manage service-anchor",
			"name", w.recipe.Name, "err", err)
		return
	}
	spec, err := w.buildSpec(self)
	if err != nil {
		slog.Warn("F-45 recipe could not be resolved",
			"name", w.recipe.Name, "err", err)
		return
	}
	id, err := w.ops.Create(ctx, spec)
	if err != nil {
		slog.Warn("F-45 create failed",
			"name", spec.Name, "target", w.recipe.Target, "err", err)
		return
	}
	if err := w.ops.Start(ctx, id); err != nil {
		slog.Warn("F-45 created but start failed",
			"name", spec.Name, "id", id, "err", err)
		return
	}
	slog.Info("created and started managed service-anchor",
		"name", spec.Name, "id", id, "target", w.recipe.Target, "source", source)
}

// buildSpec turns the recipe (plus runtime self-info) into the
// concrete CreateSpec. Operator-supplied values win over defaults
// from self.
func (w *Watcher) buildSpec(self SelfInfo) (CreateSpec, error) {
	image := w.recipe.Image
	if image == "" {
		image = self.Image
	}
	if image == "" {
		return CreateSpec{}, fmt.Errorf("no image configured and self-inspect returned no image")
	}
	gatewayIP := w.recipe.GatewayIP
	if gatewayIP == "" {
		shared := w.sharedNet()
		if shared == "" {
			return CreateSpec{}, fmt.Errorf("ManagedSA.GatewayIP empty and shared-network is not yet known — try again after F-44 picker settles")
		}
		gatewayIP = self.IPsByNetwork[shared]
		if gatewayIP == "" {
			return CreateSpec{}, fmt.Errorf("self has no IP on shared network %q; cannot default GatewayIP", shared)
		}
	}

	// Assemble env in deterministic order so log lines, tests, and
	// recreated containers are reproducible across restarts.
	envMap := map[string]string{
		"ANCHORD_MODE":       "service-anchor",
		"ANCHORD_GATEWAY_IP": gatewayIP,
		"ANCHORD_LOG_LEVEL":  "info",
	}
	for k, v := range w.recipe.ExtraEnv {
		envMap[k] = v
	}
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+envMap[k])
	}

	labels := map[string]string{
		"anchord.managed-by": "f45",
	}
	if self.ComposeProject != "" {
		// Tag with the network-anchor's compose project so
		// `docker compose down` of the wrap stack also removes the
		// managed service-anchor — no operator-visible leftover.
		labels["com.docker.compose.project"] = self.ComposeProject
	}

	return CreateSpec{
		Name:        w.recipe.Name,
		Image:       image,
		Env:         env,
		Labels:      labels,
		NetworkMode: "container:" + w.recipe.Target,
		CapAdd:      []string{"NET_ADMIN"},
		Restart:     "unless-stopped",
	}, nil
}

// targetMatchesRecipe is true if the started container matches the
// recipe's Target field — by container name or by ID (long or
// short). Same form Docker accepts in network_mode: container:<X>.
func targetMatchesRecipe(target ContainerInfo, recipeTarget string) bool {
	if recipeTarget == "" {
		return false
	}
	refs := referencesFor(target)
	_, ok := refs[strings.TrimPrefix(recipeTarget, "/")]
	return ok
}

// hasName reports whether a container's Names list contains the
// supplied name (with or without the Docker "/" prefix).
func hasName(c ContainerInfo, name string) bool {
	target := strings.TrimPrefix(name, "/")
	for _, n := range c.Names {
		if strings.TrimPrefix(n, "/") == target {
			return true
		}
	}
	return false
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

func (a dockerAdapter) Create(ctx context.Context, spec CreateSpec) (string, error) {
	cfg := &container.Config{
		Image:  spec.Image,
		Env:    spec.Env,
		Labels: spec.Labels,
	}
	hostCfg := &container.HostConfig{
		NetworkMode: container.NetworkMode(spec.NetworkMode),
		CapAdd:      spec.CapAdd,
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyMode(spec.Restart),
		},
	}
	resp, err := a.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("ContainerCreate: %w", err)
	}
	return resp.ID, nil
}

func (a dockerAdapter) InspectSelf(ctx context.Context) (SelfInfo, error) {
	insp, err := a.cli.ContainerInspect(ctx, selfHostname())
	if err != nil {
		return SelfInfo{}, fmt.Errorf("inspect self: %w", err)
	}
	out := SelfInfo{
		Image:        insp.Config.Image,
		IPsByNetwork: map[string]string{},
	}
	if insp.Config != nil && insp.Config.Labels != nil {
		out.ComposeProject = insp.Config.Labels["com.docker.compose.project"]
	}
	if insp.NetworkSettings != nil {
		for name, n := range insp.NetworkSettings.Networks {
			if n != nil && n.IPAddress != "" {
				out.IPsByNetwork[name] = n.IPAddress
			}
		}
	}
	return out, nil
}

// selfHostname returns the container ID short form Docker sets as
// HOSTNAME — the identifier ContainerInspect accepts as a stand-in
// for the container itself. Errors fall back to empty string, which
// ContainerInspect will reject with a clear message.
func selfHostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
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
