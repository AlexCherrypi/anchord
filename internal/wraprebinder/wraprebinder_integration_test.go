//go:build integration

// Integration tests for the wraprebinder package's production
// dockerAdapter. Hit a real Docker daemon; require permission to
// create + delete containers. Driven by
// `go test -tags=integration ./internal/wraprebinder/...`.
//
// Headline scenario reproduces issue #13:
//   1. Create a target container with a stable name.
//   2. Create a wrap-anchor with `network_mode: container:<target>`
//      and a compose.project label.
//   3. Snapshot the wrap-anchor's started-at timestamp.
//   4. Recreate the target under the same name (new container ID).
//   5. Run bootstrapRecheck against the real adapter.
//   6. Verify the wrap-anchor's started-at advances — proof it was
//      restarted via the F-49 path.
//
// Cleanup uses t.Cleanup so partial failures don't leave orphans.

package wraprebinder

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
	testImage   = "alpine:3.21"
	testProject = "anchord-wraprebinder-test"
)

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

func uniqueName(t *testing.T, suffix string) string {
	t.Helper()
	safe := strings.ReplaceAll(t.Name(), "/", "_")
	safe = strings.ReplaceAll(safe, " ", "_")
	return testProject + "-" + safe + "-" + suffix
}

// createPlainContainer spawns a sleeping alpine container with a
// stable name and the test compose-project label. Returns the
// container ID and registers cleanup.
func createPlainContainer(t *testing.T, cli *client.Client, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := cli.ImageInspectWithRaw(ctx, testImage); err != nil {
		t.Skipf("image %q not present locally: %v", testImage, err)
	}

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:  testImage,
			Cmd:    []string{"sleep", "infinity"},
			Labels: map[string]string{"com.docker.compose.project": testProject},
		},
		&container.HostConfig{},
		nil, nil, name,
	)
	if err != nil {
		t.Fatalf("ContainerCreate(%q): %v", name, err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(%q): %v", name, err)
	}
	return resp.ID
}

// createWrapAnchor spawns an anchor pointing at `target` via
// `network_mode: container:<target>`. Mirrors the F-39/F-40 wrap
// pattern. Registers cleanup.
func createWrapAnchor(t *testing.T, cli *client.Client, name, targetName string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:  testImage,
			Cmd:    []string{"sleep", "infinity"},
			Labels: map[string]string{"com.docker.compose.project": testProject},
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode("container:" + targetName),
		},
		&network.NetworkingConfig{},
		nil,
		name,
	)
	if err != nil {
		t.Fatalf("ContainerCreate(anchor %q): %v", name, err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(anchor %q): %v", name, err)
	}
	return resp.ID
}

// inspectByName returns (id, started_at, network_mode) for the
// container currently registered under `name`. The whole point of
// F-49 is that the ANCHOR's container ID changes after a recreate
// (remove + create + start), so identity-by-name is the right
// identity to track here, not the original ID we got at create
// time. Skip-on-not-found because tests sometimes call this between
// remove + create.
func inspectByName(t *testing.T, cli *client.Client, name string) (id, startedAt, netmode string) {
	t.Helper()
	insp, err := cli.ContainerInspect(context.Background(), name)
	if err != nil {
		t.Fatalf("ContainerInspect(%q): %v", name, err)
	}
	if insp.State == nil || insp.HostConfig == nil {
		return insp.ID, "", string(insp.HostConfig.NetworkMode)
	}
	return insp.ID, insp.State.StartedAt, string(insp.HostConfig.NetworkMode)
}

// recreateTarget removes a container by ID and re-creates one under
// the same name. Used to simulate the target stack's `compose down/up`
// or auto-updater cycle that triggers the F-49 incident pattern.
func recreateTarget(t *testing.T, cli *client.Client, oldID, name string) string {
	t.Helper()
	if err := cli.ContainerRemove(context.Background(), oldID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("manual remove of stale target: %v", err)
	}
	// Poll briefly so the daemon frees the name slot.
	for i := 0; i < 30; i++ {
		_, err := cli.ContainerInspect(context.Background(), name)
		if err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return createPlainContainer(t, cli, name)
}

// ---- enumerateTargetPool against real Docker ------------------------------

func TestIntegration_EnumerateTargetPool(t *testing.T) {
	cli := newCli(t)
	targetName := uniqueName(t, "target")
	anchorName := uniqueName(t, "anchor")
	createPlainContainer(t, cli, targetName)
	anchorID := createWrapAnchor(t, cli, anchorName, targetName)

	w := New(cli, &config.WrapRebinder{
		SelfProject:    testProject,
		PollInterval:   30 * time.Second,
		RestartTimeout: 10 * time.Second,
	})
	w.enumerateTargetPool(context.Background())

	got := w.lookupAnchors(targetName)
	if len(got) != 1 || got[0] != anchorID {
		t.Errorf("pool[%q] = %v, want [%s]", targetName, got, anchorID)
	}
}

// ---- end-to-end recreate scenario -----------------------------------------

// TestIntegration_RecreatesAnchorAfterTargetRecreate is the headline
// scenario reproducing the 2026-06-07 Mailcow incident: recreate the
// target, run bootstrapRecheck, verify the wrap-anchor was recreated
// (new container ID + netmode now points to the new target).
func TestIntegration_RecreatesAnchorAfterTargetRecreate(t *testing.T) {
	cli := newCli(t)
	targetName := uniqueName(t, "target")
	anchorName := uniqueName(t, "anchor")

	staleTargetID := createPlainContainer(t, cli, targetName)
	oldAnchorID := createWrapAnchor(t, cli, anchorName, targetName)

	w := New(cli, &config.WrapRebinder{
		SelfProject:    testProject,
		PollInterval:   30 * time.Second,
		RestartTimeout: 10 * time.Second,
	})
	w.enumerateTargetPool(context.Background())

	// Now recreate the target — new container ID, same name.
	newTargetID := recreateTarget(t, cli, staleTargetID, targetName)
	if newTargetID == staleTargetID {
		t.Fatalf("docker reused container ID; cannot simulate recreate")
	}

	// Run the bootstrap recheck path. It must detect drift and
	// recreate (remove+create+start) the anchor — `docker restart`
	// alone fails with `joining network namespace of container: No
	// such container` because the stored ID is dead.
	w.bootstrapRecheck(context.Background(), "integration")

	// The anchor's container ID should now be different from
	// oldAnchorID; identity-by-name persists across the recreate.
	newID, _, newNetmode := inspectByName(t, cli, anchorName)
	if newID == oldAnchorID {
		t.Errorf("wrap-anchor container ID did not change after recreate; old=%s new=%s",
			oldAnchorID, newID)
	}
	// Netmode should now resolve against the NEW target's ID.
	if newNetmode != "container:"+newTargetID {
		t.Errorf("wrap-anchor netmode = %q, want container:%s",
			newNetmode, newTargetID)
	}
}

// TestIntegration_NoChurnOnHealthyState verifies bootstrapRecheck
// against a healthy wrap-stack does NOT recreate the anchor.
func TestIntegration_NoChurnOnHealthyState(t *testing.T) {
	cli := newCli(t)
	targetName := uniqueName(t, "target")
	anchorName := uniqueName(t, "anchor")

	createPlainContainer(t, cli, targetName)
	oldAnchorID := createWrapAnchor(t, cli, anchorName, targetName)

	w := New(cli, &config.WrapRebinder{
		SelfProject:    testProject,
		PollInterval:   30 * time.Second,
		RestartTimeout: 10 * time.Second,
	})
	w.enumerateTargetPool(context.Background())

	w.bootstrapRecheck(context.Background(), "integration")
	// Give the daemon a moment in case a recreate was (wrongly) issued.
	time.Sleep(500 * time.Millisecond)
	stillID, _, _ := inspectByName(t, cli, anchorName)
	if stillID != oldAnchorID {
		t.Errorf("healthy state must not recreate anchor; ID changed from %s to %s",
			oldAnchorID, stillID)
	}
}

// TestIntegration_RecreateFromStartEvent simulates the event-driven
// path: handle a `start` event for the target's name and confirm the
// anchor gets recreated. Uses the live adapter end-to-end.
func TestIntegration_RecreateFromStartEvent(t *testing.T) {
	cli := newCli(t)
	targetName := uniqueName(t, "target")
	anchorName := uniqueName(t, "anchor")
	createPlainContainer(t, cli, targetName)
	oldAnchorID := createWrapAnchor(t, cli, anchorName, targetName)

	w := New(cli, &config.WrapRebinder{
		SelfProject:    testProject,
		PollInterval:   30 * time.Second,
		RestartTimeout: 10 * time.Second,
	})
	w.enumerateTargetPool(context.Background())

	w.handleStart(context.Background(), EventMsg{
		Action:        "start",
		ContainerName: targetName,
	})

	// Poll briefly — recreate is multi-step (inspect+remove+create+
	// start) and may take a moment on slower daemons.
	deadline := time.Now().Add(5 * time.Second)
	var newID string
	for time.Now().Before(deadline) {
		newID, _, _ = inspectByName(t, cli, anchorName)
		if newID != oldAnchorID {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if newID == oldAnchorID {
		t.Errorf("wrap-anchor container ID did not change after event-driven recreate")
	}
}
