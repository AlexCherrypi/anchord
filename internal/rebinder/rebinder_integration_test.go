//go:build integration

// Integration tests for the rebinder package's production
// dockerAdapter. These hit a real Docker daemon and require:
//   - DOCKER_HOST reachable from the test runner (default unix socket).
//   - Permission to create + delete networks and containers.
//
// Run them with: `go test -tags=integration ./internal/rebinder/...`.
//
// The headline assertions are:
//   - dockerAdapter.NetworkInspectByName resolves a freshly-created
//     network to its current ID.
//   - dockerAdapter.ContainerInspect populates Networks[name]=ID
//     correctly — the field the bootstrap-recheck uses to detect
//     divergence.
//   - End-to-end rebind: simulate a target-recreate by deleting and
//     recreating the network under the same name, then verify the
//     follower ends up attached to the *new* network ID after the
//     rebinder's reattach path runs.
//   - Idempotency: a second reattach call against an already-current
//     follower must be a no-op at the Docker level (no errors, no
//     state change).
//
// Cleanup: every test registers a t.Cleanup that removes the
// containers and network it created, so a partial failure doesn't
// leave orphans on the host.

package rebinder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

const (
	testImage      = "alpine:3.21"
	testNetPrefix  = "anchord-rebinder-test-"
	testFollowerPx = "anchord-rebinder-follower-"
)

// newCli builds a Docker client using env-defaults. Skip the entire
// test set if Docker is unavailable rather than failing — these tests
// only make sense against a live daemon.
func newCli(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		t.Skipf("docker client unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	return cli
}

// uniqueName builds a per-test resource name. testing.T.Name() is
// stable within a run but contains slashes for subtests; we strip
// those because docker names disallow them.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	safe := strings.ReplaceAll(t.Name(), "/", "_")
	safe = strings.ReplaceAll(safe, " ", "_")
	return prefix + safe
}

// createNetwork makes a bridge network and registers cleanup. Returns
// the network's current ID.
func createNetwork(t *testing.T, cli *client.Client, name string) string {
	t.Helper()
	resp, err := cli.NetworkCreate(context.Background(), name, network.CreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		t.Fatalf("NetworkCreate(%q): %v", name, err)
	}
	t.Cleanup(func() {
		_ = cli.NetworkRemove(context.Background(), resp.ID)
	})
	return resp.ID
}

// createFollower spawns a sleeping alpine container attached to the
// named network, returns its container ID, and registers cleanup.
// "sleep infinity" keeps it alive for the test's duration without
// requiring a real workload.
func createFollower(t *testing.T, cli *client.Client, netName, contName string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pull may be a slow path in CI — if the image is missing, fail
	// with guidance instead of mysteriously timing out.
	if _, _, err := cli.ImageInspectWithRaw(ctx, testImage); err != nil {
		t.Skipf("image %q not present locally and pull is out of scope; pull first or run with image cached: %v", testImage, err)
	}

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: testImage,
			Cmd:   []string{"sleep", "infinity"},
		},
		&container.HostConfig{},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {},
			},
		},
		nil,
		contName,
	)
	if err != nil {
		t.Fatalf("ContainerCreate(%q): %v", contName, err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(%q): %v", contName, err)
	}
	return resp.ID
}

// readAttachedNetworkID looks up a container's stored network ID for
// the named attachment. Returns "" if not attached. Used to verify
// rebind effects from the production-adapter's perspective.
func readAttachedNetworkID(t *testing.T, cli *client.Client, contID, netName string) string {
	t.Helper()
	insp, err := cli.ContainerInspect(context.Background(), contID)
	if err != nil {
		t.Fatalf("ContainerInspect(%q): %v", contID, err)
	}
	if insp.NetworkSettings == nil {
		return ""
	}
	n, ok := insp.NetworkSettings.Networks[netName]
	if !ok || n == nil {
		return ""
	}
	return n.NetworkID
}

// ---- adapter primitives ----------------------------------------------------

func TestIntegration_NetworkInspectByName(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	wantID := createNetwork(t, cli, netName)

	a := dockerAdapter{cli: cli}
	got, err := a.NetworkInspectByName(context.Background(), netName)
	if err != nil {
		t.Fatalf("NetworkInspectByName: %v", err)
	}
	if got.ID != wantID {
		t.Errorf("ID = %q, want %q", got.ID, wantID)
	}
	if got.Name != netName {
		t.Errorf("Name = %q, want %q", got.Name, netName)
	}
}

func TestIntegration_NetworkInspectByName_NotFound(t *testing.T) {
	cli := newCli(t)
	a := dockerAdapter{cli: cli}
	_, err := a.NetworkInspectByName(context.Background(), uniqueName(t, "anchord-no-such-net-"))
	if err == nil {
		t.Fatal("expected error for missing network, got nil")
	}
}

func TestIntegration_ContainerInspect_PopulatesNetworks(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	wantID := createNetwork(t, cli, netName)
	contName := uniqueName(t, testFollowerPx)
	contID := createFollower(t, cli, netName, contName)

	a := dockerAdapter{cli: cli}
	info, err := a.ContainerInspect(context.Background(), contID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	gotID, ok := info.Networks[netName]
	if !ok {
		t.Fatalf("container reports networks %v; expected an entry for %q", info.Networks, netName)
	}
	if gotID != wantID {
		t.Errorf("Networks[%q] = %q, want %q", netName, gotID, wantID)
	}
}

// ---- end-to-end rebind -----------------------------------------------------

// TestIntegration_RebindAfterNetworkRecreate is the headline scenario:
// create a network and a follower attached to it, then delete + recreate
// the network under the same name (simulating the target stack's
// down/up). The follower is now attached to a stale network ID. Run
// the rebinder's reattach path and verify the follower's stored
// network ID matches the *new* network's ID afterwards.
func TestIntegration_RebindAfterNetworkRecreate(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	staleID := createNetwork(t, cli, netName)
	contName := uniqueName(t, testFollowerPx)
	contID := createFollower(t, cli, netName, contName)

	// Sanity: follower starts on the stale network ID.
	if got := readAttachedNetworkID(t, cli, contID, netName); got != staleID {
		t.Fatalf("pre-test attachment: got %q want %q", got, staleID)
	}

	// Simulate target-stack recreate: detach follower (so the network
	// has no endpoints holding it open), remove network, recreate it
	// under the same name → new ID.
	if err := cli.NetworkDisconnect(context.Background(), staleID, contID, true); err != nil {
		t.Fatalf("manual disconnect for staging: %v", err)
	}
	if err := cli.NetworkRemove(context.Background(), staleID); err != nil {
		t.Fatalf("manual NetworkRemove for staging: %v", err)
	}
	// Docker stages network removal: NetworkRemove returns before the
	// name slot is freed. Poll briefly until a name-keyed inspect
	// fails — that's the daemon's signal we can safely reuse the
	// name. Up to ~3s, then proceed regardless and let the
	// recreate's error message be the diagnostic.
	for i := 0; i < 30; i++ {
		if _, err := cli.NetworkInspect(context.Background(), netName, network.InspectOptions{}); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// At this point the follower's NetworkSettings.Networks[name] is
	// gone too (Docker cleans up endpoint state on disconnect). The
	// rebinder still has to handle the "not attached" → "attach to
	// new" path cleanly — exactly the post-recreate state in the
	// production incident.
	newID := createNetwork(t, cli, netName)
	if newID == staleID {
		t.Fatalf("docker handed us the same ID twice; cannot simulate recreate")
	}

	// Run reattach against the production adapter.
	w := New(cli, &config.Rebinder{
		FollowNetwork: netName,
		FollowTarget:  contName,
		// SelfProject is empty so resolveFollower falls back to name
		// match — exactly the production sidecar path.
	})
	w.reattach(context.Background(), "integration-test")

	gotID := readAttachedNetworkID(t, cli, contID, netName)
	if gotID != newID {
		t.Errorf("after reattach: follower attached to %q, want %q", gotID, newID)
	}
}

// TestIntegration_RebindIdempotent verifies a second reattach call
// against an already-current follower is a no-op at the API level
// (the "already connected" path) and leaves attachment unchanged.
func TestIntegration_RebindIdempotent(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	netID := createNetwork(t, cli, netName)
	contName := uniqueName(t, testFollowerPx)
	contID := createFollower(t, cli, netName, contName)

	w := New(cli, &config.Rebinder{
		FollowNetwork: netName,
		FollowTarget:  contName,
	})
	// First reattach: noisy in logs (we'll see the "already exists"
	// branch) but must succeed at the state level.
	w.reattach(context.Background(), "integration-1")
	// Second reattach against now-current state.
	w.reattach(context.Background(), "integration-2")

	gotID := readAttachedNetworkID(t, cli, contID, netName)
	if gotID != netID {
		t.Errorf("after double-reattach: follower attached to %q, want %q", gotID, netID)
	}
}

// TestIntegration_BootstrapRecheck_NoDivergence verifies the bootstrap
// recheck path against a real daemon: with a follower already on the
// current network, recheck must NOT issue a reattach (no transient
// disconnect, no churn).
func TestIntegration_BootstrapRecheck_NoDivergence(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	netID := createNetwork(t, cli, netName)
	contName := uniqueName(t, testFollowerPx)
	contID := createFollower(t, cli, netName, contName)

	w := New(cli, &config.Rebinder{
		FollowNetwork: netName,
		FollowTarget:  contName,
	})
	w.bootstrapRecheck(context.Background())

	// The follower must still be on the same network ID, no churn.
	if got := readAttachedNetworkID(t, cli, contID, netName); got != netID {
		t.Errorf("bootstrap recheck on healthy state must not change attachment; got %q want %q", got, netID)
	}
}

// ---- release-before-recreate / settle (2026-07-04 fix) ---------------------

// TestIntegration_MembersPopulated verifies the production adapter's
// NetworkInspectByName reports the follower as a network Member — the
// signal the park and settle logic key on.
func TestIntegration_MembersPopulated(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	createNetwork(t, cli, netName)
	contName := uniqueName(t, testFollowerPx)
	contID := createFollower(t, cli, netName, contName)

	a := dockerAdapter{cli: cli}
	info, err := a.NetworkInspectByName(context.Background(), netName)
	if err != nil {
		t.Fatalf("NetworkInspectByName: %v", err)
	}
	if _, ok := info.Members[contID]; !ok {
		t.Errorf("Members = %v; expected an entry for follower %q", info.Members, contID)
	}
}

// TestIntegration_ParkReleasesSoleFollower is the release-before-recreate
// headline: with the follower the sole endpoint on the network, a
// disconnect event must drive a proactive release so the target stack's
// `network rm` can proceed. We prove it by asserting the network becomes
// removable (no active endpoints) after maybePark.
func TestIntegration_ParkReleasesSoleFollower(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	netID := createNetwork(t, cli, netName)
	contName := uniqueName(t, testFollowerPx)
	contID := createFollower(t, cli, netName, contName)

	// Sanity: the follower is the sole endpoint, so a NetworkRemove would
	// fail right now with "has active endpoints".
	if err := cli.NetworkRemove(context.Background(), netID); err == nil {
		t.Fatal("expected NetworkRemove to fail while follower is attached")
	}

	w := New(cli, &config.Rebinder{FollowNetwork: netName, FollowTarget: contName})
	w.maybePark(context.Background(), EventMsg{Action: "disconnect", Name: netName, ContainerID: "some-departing-core"})

	if !w.parked {
		t.Errorf("expected parked=true after sole-endpoint release")
	}
	if got := readAttachedNetworkID(t, cli, contID, netName); got != "" {
		t.Errorf("follower should be detached after park, still attached to %q", got)
	}
	// The network must now be removable — the whole point of the release.
	if err := cli.NetworkRemove(context.Background(), netID); err != nil {
		t.Errorf("network should be removable after park, got: %v", err)
	}
}

// TestIntegration_ParkHoldsWhenForeignEndpointsRemain verifies the guard:
// with another (foreign) container still attached, the follower must NOT
// be released — releasing early would drop a live path.
func TestIntegration_ParkHoldsWhenForeignEndpointsRemain(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	createNetwork(t, cli, netName)
	followerName := uniqueName(t, testFollowerPx)
	followerID := createFollower(t, cli, netName, followerName)
	// A second, foreign container sharing the network.
	coreName := uniqueName(t, testFollowerPx+"core-")
	createFollower(t, cli, netName, coreName)

	w := New(cli, &config.Rebinder{FollowNetwork: netName, FollowTarget: followerName})
	w.maybePark(context.Background(), EventMsg{Action: "disconnect", Name: netName})

	if w.parked {
		t.Errorf("must not park while a foreign endpoint remains")
	}
	if got := readAttachedNetworkID(t, cli, followerID, netName); got == "" {
		t.Errorf("follower must stay attached while foreign endpoints remain")
	}
}

// TestIntegration_SettleReadyWithStableEndpoint proves the settle loop
// reads real endpoint state through the production adapter and reports
// Ready once a foreign endpoint is present and stable. Uses the in-package
// test seam for fast timings so the loop resolves against the live daemon
// without real second-scale sleeps.
func TestIntegration_SettleReadyWithStableEndpoint(t *testing.T) {
	cli := newCli(t)
	netName := uniqueName(t, testNetPrefix)
	createNetwork(t, cli, netName)
	coreName := uniqueName(t, testFollowerPx+"core-")
	createFollower(t, cli, netName, coreName)

	// FollowTarget names a follower that does not exist, so the single
	// live container counts as a foreign (target-owned) endpoint.
	w := newWithOps(dockerAdapter{cli: cli}, &config.Rebinder{
		FollowNetwork: netName,
		FollowTarget:  "no-such-follower",
	})
	if got := w.waitForSettle(context.Background()); got != settleReady {
		t.Errorf("waitForSettle with a stable foreign endpoint = %v, want settleReady", got)
	}
}
