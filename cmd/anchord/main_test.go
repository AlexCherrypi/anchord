package main

import (
	"strings"
	"testing"
)

// TestPickSharedNetwork covers F-38: the shared-network picker must
// never return ANCHORD_EXT_NETWORK (the external macvlan), prefer a
// "transit"-named bridge when present, and fail loudly when the only
// candidate is EXT_NETWORK.
func TestPickSharedNetwork(t *testing.T) {
	cases := []struct {
		name       string
		networks   []string
		excludeNet string
		want       string
		wantErr    string // substring, "" for no error
	}{
		{
			name:     "no exclusion, transit wins",
			networks: []string{"mailcow_backend", "mailcow_transit"},
			want:     "mailcow_transit",
		},
		{
			name:     "no exclusion, no transit → first match",
			networks: []string{"backend"},
			want:     "backend",
		},
		{
			name:       "F-38 wrap pattern — exclude macvlan, transit wins among the rest",
			networks:   []string{"dmz_macvlan", "transit"},
			excludeNet: "dmz_macvlan",
			want:       "transit",
		},
		{
			name:       "F-38 wrap pattern — exclude macvlan, no transit → picks the surviving bridge",
			networks:   []string{"dmz_macvlan", "ix-mailcow_mailcow-network"},
			excludeNet: "dmz_macvlan",
			want:       "ix-mailcow_mailcow-network",
		},
		{
			name:       "F-38 acceptance — only EXT_NETWORK on self is fatal",
			networks:   []string{"dmz_macvlan"},
			excludeNet: "dmz_macvlan",
			wantErr:    "only EXT_NETWORK",
		},
		{
			name:       "exclude must not crash when not present in the list",
			networks:   []string{"transit", "backend"},
			excludeNet: "dmz_macvlan",
			want:       "transit",
		},
		{
			name:     "empty list is an error (defensive)",
			networks: nil,
			wantErr:  "no networks",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickSharedNetwork(tc.networks, tc.excludeNet)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want err containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPickSharedNetwork_CaseInsensitiveTransit guards the convention
// that "Transit" / "TRANSIT" / "wrapped_TRANSIT_bridge" all match the
// transit-preference heuristic. The naming convention is operator-set,
// and the v1 behaviour was tolerant of casing — keep that.
func TestPickSharedNetwork_CaseInsensitiveTransit(t *testing.T) {
	for _, name := range []string{"transit", "Transit", "TRANSIT", "myproj_TransitBridge"} {
		got, err := pickSharedNetwork([]string{"backend", name}, "")
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if got != name {
			t.Errorf("%q: got %q", name, got)
		}
	}
}

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
