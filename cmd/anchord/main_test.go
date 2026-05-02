package main

import (
	"strings"
	"testing"
)

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
