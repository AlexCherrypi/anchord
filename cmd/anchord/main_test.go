package main

import (
	"reflect"
	"strings"
	"testing"
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
