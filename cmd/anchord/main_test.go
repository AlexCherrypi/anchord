package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/AlexCherrypi/anchord/internal/dependents"
)

// F-42 precedence resolution: selector wins, project is informational
// only when both are set, project alone is the legacy path.
func TestBuildDiscoveryDiscriminator(t *testing.T) {
	cases := []struct {
		name     string
		project  string
		selector map[string]string
		want     []string
	}{
		{
			name:    "legacy project only",
			project: "mailcow",
			want:    []string{"com.docker.compose.project=mailcow"},
		},
		{
			name:     "selector replaces project (both set, both ignored on selector path)",
			project:  "ix-authentik",
			selector: map[string]string{"anchord.role": "ldap-outpost"},
			want:     []string{"anchord.role=ldap-outpost"},
		},
		{
			name:    "selector alone",
			project: "",
			selector: map[string]string{
				"anchord.role": "ldap-outpost",
			},
			want: []string{"anchord.role=ldap-outpost"},
		},
		{
			name:    "selector AND-joined, deterministic order by key",
			project: "",
			selector: map[string]string{
				"env":          "prod",
				"anchord.role": "ldap-outpost",
			},
			// Sorted by key: "anchord.role" < "env".
			want: []string{"anchord.role=ldap-outpost", "env=prod"},
		},
		{
			name:    "empty selector empty project → nil (config layer guards this)",
			project: "",
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDiscoveryDiscriminator(tc.project, tc.selector)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

// F-42 determinism guard: the selector → predicates conversion must
// produce the same ordering on every call. Run a high-cardinality
// selector many times and assert byte-equal output.
func TestBuildDiscoveryDiscriminator_Deterministic(t *testing.T) {
	sel := map[string]string{
		"z-key":          "1",
		"a-key":          "2",
		"m-key":          "3",
		"anchord.role":   "ldap-outpost",
		"env":            "prod",
		"com.acme.team":  "infra",
		"another.label":  "value",
	}
	first := buildDiscoveryDiscriminator("", sel)
	for i := 0; i < 200; i++ {
		again := buildDiscoveryDiscriminator("", sel)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("iteration %d differed: %v vs %v", i, again, first)
		}
	}
}

// The F-38 picker tests that used to live here moved into
// internal/sharednet/sharednet_test.go when F-44 absorbed the
// algorithm into a stateful Picker. The "transit"-preference and
// alphabetic-fallback semantics are still covered there — see
// TestPick_TieTransitPreferred, TestPick_TieAllTransitAlphabetical,
// and TestPick_EmptyBackendSet_FallbackNoSettle.

func TestSelectMode(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		envMode string
		want    Mode
		wantErr string
	}{
		{
			name:    "no args, no env -> default network-anchor",
			args:    []string{"anchord"},
			envMode: "",
			want:    ModeNetworkAnchor,
		},
		{
			name:    "ANCHORD_MODE=service-anchor",
			args:    []string{"anchord"},
			envMode: "service-anchor",
			want:    ModeServiceAnchor,
		},
		{
			name:    "subcommand wins over env",
			args:    []string{"anchord", "service-anchor"},
			envMode: "network-anchor",
			want:    ModeServiceAnchor,
		},
		{
			name:    "explicit network-anchor subcommand",
			args:    []string{"anchord", "network-anchor"},
			envMode: "",
			want:    ModeNetworkAnchor,
		},
		{
			name:    "flag-only args are ignored",
			args:    []string{"anchord", "-debug"},
			envMode: "",
			want:    ModeNetworkAnchor,
		},
		{
			name:    "unknown subcommand errors",
			args:    []string{"anchord", "garbage-anchor"},
			envMode: "",
			wantErr: "unknown mode",
		},
		{
			name:    "unknown env errors",
			args:    []string{"anchord"},
			envMode: "wat",
			wantErr: "unknown mode",
		},
		{
			name:    "doctor subcommand recognised",
			args:    []string{"anchord", "doctor", "stale-netns"},
			envMode: "",
			want:    ModeDoctor,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectMode(tc.args, tc.envMode)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// runDoctor dispatch surface: empty args prints usage and returns
// nil; unknown subcommand returns an error. The actual stale-netns
// run touches docker.sock and isn't unit-testable here — its
// rendering is covered by TestPrintStaleReport below and the
// underlying scan logic by internal/dependents.
func TestRunDoctor_Dispatch(t *testing.T) {
	t.Run("no args prints usage", func(t *testing.T) {
		err := runDoctor(context.Background(), nil)
		if err != nil {
			t.Errorf("no-args usage should succeed, got %v", err)
		}
	})
	t.Run("unknown subcommand errors", func(t *testing.T) {
		err := runDoctor(context.Background(), []string{"unknown-thing"})
		if err == nil || !strings.Contains(err.Error(), "unknown doctor subcommand") {
			t.Errorf("got err=%v, want error mentioning unknown subcommand", err)
		}
	})
	t.Run("--help is not an error", func(t *testing.T) {
		err := runDoctor(context.Background(), []string{"--help"})
		if err != nil {
			t.Errorf("--help should succeed, got %v", err)
		}
	})
}

// Stale-netns rendering: grouped by dead target, sorted, includes
// the compose hint when present.
func TestPrintStaleReport(t *testing.T) {
	stale := []dependents.StaleNetns{
		{
			Container:   dependents.Container{ID: "id-traefik-frigate", Names: []string{"/ix-authentik-traefik-frigate-1"}},
			StaleTarget: "158f0cc2",
			ComposeHint: "docker compose -p ix-authentik up -d --no-deps --force-recreate traefik-frigate",
		},
		{
			Container:   dependents.Container{ID: "id-authentik-server", Names: []string{"/authentik_server"}},
			StaleTarget: "2a8f83ad",
			ComposeHint: "docker compose -p ix-authentik up -d --no-deps --force-recreate authentik_server",
		},
		{
			Container:   dependents.Container{ID: "id-traefik-nc", Names: []string{"/ix-nextcloud-traefik-nextcloud-1"}},
			StaleTarget: "74a219e7",
			ComposeHint: "docker compose -p ix-nextcloud up -d --no-deps --force-recreate traefik-nextcloud",
		},
		// Two victims for the same dead target — they must cluster.
		{
			Container:   dependents.Container{ID: "id-acme-1", Names: []string{"/acme-renewer-xibo"}},
			StaleTarget: "a7c53426",
			ComposeHint: "docker compose -p ix-xibo up -d --no-deps --force-recreate acme-renewer-xibo",
		},
		{
			Container:   dependents.Container{ID: "id-xibo-traefik", Names: []string{"/ix-xibo-traefik-1"}},
			StaleTarget: "a7c53426",
			ComposeHint: "docker compose -p ix-xibo up -d --no-deps --force-recreate traefik",
		},
		// Victim without compose labels — must still be listed but
		// without a hint.
		{
			Container:   dependents.Container{ID: "id-manual", Names: []string{"/manually-docker-run-ed"}},
			StaleTarget: "ffffffff",
			ComposeHint: "",
		},
	}
	var buf bytes.Buffer
	printStaleReport(&buf, stale)
	out := buf.String()

	// Header is correct (6 victims across 5 distinct targets).
	if !strings.Contains(out, "Found 6 dependent(s) in dead netns across 5 target(s):") {
		t.Errorf("missing or wrong header line; got:\n%s", out)
	}
	// Each distinct dead target appears.
	for _, tgt := range []string{"158f0cc2", "2a8f83ad", "74a219e7", "a7c53426", "ffffffff"} {
		if !strings.Contains(out, "dead target: "+tgt) {
			t.Errorf("missing target %q in output:\n%s", tgt, out)
		}
	}
	// The two victims of a7c53426 must cluster: their names appear
	// without an intervening "dead target:" line between them.
	idxA := strings.Index(out, "acme-renewer-xibo")
	idxB := strings.Index(out, "ix-xibo-traefik-1")
	idxNextDeadTarget := strings.Index(out[idxA:], "dead target:")
	if idxA < 0 || idxB < 0 {
		t.Fatalf("expected both xibo victims in output, got:\n%s", out)
	}
	if idxNextDeadTarget != -1 && idxA+idxNextDeadTarget < idxB {
		t.Errorf("victims of same dead target did not cluster; got:\n%s", out)
	}
	// Compose hint included where present.
	if !strings.Contains(out, "→ docker compose -p ix-authentik up -d --no-deps --force-recreate authentik_server") {
		t.Errorf("missing compose hint for authentik_server; got:\n%s", out)
	}
	// Victim without compose hint is still listed.
	if !strings.Contains(out, "manually-docker-run-ed") {
		t.Errorf("hint-less victim missing from report; got:\n%s", out)
	}
	// And does NOT carry an empty " → " stub.
	if strings.Contains(out, "manually-docker-run-ed\n      → ") {
		t.Errorf("hint-less victim should not have empty arrow; got:\n%s", out)
	}
}
