package dependents

import (
	"reflect"
	"sort"
	"testing"
)

func TestFind_DeadRef(t *testing.T) {
	dependent := Container{
		ID:          "dep-running-but-orphaned",
		Names:       []string{"/ix-authentik-traefik-frigate-1"},
		State:       "running",
		NetworkMode: "container:158f0cc2-DEAD",
		Labels: map[string]string{
			"com.docker.compose.project": "ix-authentik",
			"com.docker.compose.service": "traefik-frigate",
		},
	}
	other := Container{ID: "some-other-running", Names: []string{"/whatever"}, State: "running"}
	all := []Container{dependent, other}

	got := Find(all, all)
	if len(got) != 1 {
		t.Fatalf("expected 1 stale, got %d", len(got))
	}
	if got[0].Container.ID != dependent.ID {
		t.Errorf("wrong container: got %s, want %s", got[0].Container.ID, dependent.ID)
	}
	if got[0].StaleTarget != "158f0cc2-DEAD" {
		t.Errorf("wrong stale target: got %q", got[0].StaleTarget)
	}
	wantHint := "docker compose -p ix-authentik up -d --no-deps --force-recreate traefik-frigate"
	if got[0].ComposeHint != wantHint {
		t.Errorf("compose hint:\n  got %q\n  want %q", got[0].ComposeHint, wantHint)
	}
}

func TestFind_LiveRefByLongID(t *testing.T) {
	target := Container{
		ID:    "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Names: []string{"/fe-anchor-X"},
		State: "running",
	}
	dependent := Container{
		ID:          "dep1",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:" + target.ID,
	}
	got := Find([]Container{dependent}, []Container{target, dependent})
	if len(got) != 0 {
		t.Errorf("live long-ID ref must resolve; got %d false-positives: %+v", len(got), got)
	}
}

func TestFind_LiveRefByShortID(t *testing.T) {
	target := Container{
		ID:    "abcdef012345fedcba0987654321deadbeefcafebabe1234abcdef0123456789",
		Names: []string{"/fe-anchor-X"},
		State: "running",
	}
	dependent := Container{
		ID:          "dep1",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:" + target.ID[:12], // short form
	}
	got := Find([]Container{dependent}, []Container{target, dependent})
	if len(got) != 0 {
		t.Errorf("live short-ID ref must resolve; got %d false-positives", len(got))
	}
}

func TestFind_LiveRefByName(t *testing.T) {
	target := Container{
		ID:    "tgt-id",
		Names: []string{"/fe-anchor-X"},
		State: "running",
	}
	dependent := Container{
		ID:          "dep1",
		Names:       []string{"/traefik"},
		State:       "running",
		NetworkMode: "container:fe-anchor-X",
	}
	got := Find([]Container{dependent}, []Container{target, dependent})
	if len(got) != 0 {
		t.Errorf("live name ref must resolve; got %d false-positives", len(got))
	}
}

// A dependent pointing at a stopped-but-still-extant container is
// NOT in dead netns — Docker still keeps the netns alive until the
// container is destroyed. The predicate must not flag those.
func TestFind_RefToStoppedContainerIsLive(t *testing.T) {
	target := Container{
		ID:    "tgt-id",
		Names: []string{"/fe-anchor-X"},
		State: "exited", // stopped but still in the list
	}
	dependent := Container{
		ID:          "dep1",
		NetworkMode: "container:tgt-id",
		State:       "running",
	}
	got := Find([]Container{dependent}, []Container{target, dependent})
	if len(got) != 0 {
		t.Errorf("exited target with surviving netns must not flag dependents; got %d", len(got))
	}
}

// Containers with non-container NetworkMode (host, bridge, none,
// a docker network name, empty) must not be considered. They're not
// using the shared-netns mechanism at all.
func TestFind_NonContainerNetworkModesIgnored(t *testing.T) {
	all := []Container{
		{ID: "host-mode", State: "running", NetworkMode: "host"},
		{ID: "none-mode", State: "running", NetworkMode: "none"},
		{ID: "bridge-mode", State: "running", NetworkMode: "bridge"},
		{ID: "net-mode", State: "running", NetworkMode: "my-network"},
		{ID: "no-net-mode", State: "running", NetworkMode: ""},
	}
	got := Find(all, all)
	if len(got) != 0 {
		t.Errorf("non-container netmodes must be ignored; got %d false-positives: %+v", len(got), got)
	}
}

// "container:" with empty ref (mis-configured but Docker tolerates
// this shape on inspect output) must be skipped, not crash or flag.
func TestFind_EmptyRefSkipped(t *testing.T) {
	all := []Container{
		{ID: "weird", State: "running", NetworkMode: "container:"},
		{ID: "weird2", State: "running", NetworkMode: "container:   "},
	}
	got := Find(all, all)
	if len(got) != 0 {
		t.Errorf("empty ref must be skipped; got %d false-positives", len(got))
	}
}

// No compose labels → no hint, but still a detection.
func TestFind_NoComposeHintWhenLabelsMissing(t *testing.T) {
	dep := Container{
		ID:          "dep-no-compose",
		Names:       []string{"/manually-docker-run-ed"},
		State:       "running",
		NetworkMode: "container:DEAD",
	}
	got := Find([]Container{dep}, []Container{dep})
	if len(got) != 1 {
		t.Fatalf("expected 1 stale, got %d", len(got))
	}
	if got[0].ComposeHint != "" {
		t.Errorf("compose hint must be empty when labels missing, got %q", got[0].ComposeHint)
	}
}

// Production incident shape: 11 dependents in one project all
// pointing at the same dead SA. Find must report every one.
func TestFind_ManyDependentsOnOneDeadTarget(t *testing.T) {
	deadID := "158f0cc2"
	all := []Container{
		{ID: "live-sa-new", Names: []string{"/fe-anchor-X-NEW"}, State: "running"},
	}
	var want []string
	for _, n := range []string{
		"ix-authentik-traefik-frigate-1",
		"ix-nextcloud-traefik-nextcloud-1",
		"ix-nextcloud-traefik-aio-1",
		"ix-xibo-traefik-1",
		"acme-renewer-xibo",
		"ix-vaultwarden-traefik-1",
		"cups",
		"meshcentral",
		"ix-chocolatey-server-traefik-1",
		"authentik_server",
		"dvr-demux",
	} {
		all = append(all, Container{
			ID:          "id-" + n,
			Names:       []string{"/" + n},
			State:       "running",
			NetworkMode: "container:" + deadID,
		})
		want = append(want, "id-" + n)
	}

	got := Find(all, all)
	if len(got) != len(want) {
		t.Fatalf("expected %d stale, got %d", len(want), len(got))
	}
	var gotIDs []string
	for _, s := range got {
		gotIDs = append(gotIDs, s.Container.ID)
		if s.StaleTarget != deadID {
			t.Errorf("stale_target for %s: got %q, want %q", s.Container.ID, s.StaleTarget, deadID)
		}
	}
	sort.Strings(gotIDs)
	sort.Strings(want)
	if !reflect.DeepEqual(gotIDs, want) {
		t.Errorf("victim set mismatch\n  got %v\n  want %v", gotIDs, want)
	}
}

func TestFirstName(t *testing.T) {
	cases := []struct {
		c    Container
		want string
	}{
		{Container{Names: []string{"/my-svc"}}, "my-svc"},
		{Container{Names: []string{"my-svc"}}, "my-svc"}, // already no slash
		{Container{ID: "abcdef0123456789"}, "abcdef012345"},
		{Container{}, ""},
	}
	for _, tc := range cases {
		if got := FirstName(tc.c); got != tc.want {
			t.Errorf("FirstName(%+v): got %q, want %q", tc.c, got, tc.want)
		}
	}
}
