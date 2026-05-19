package labels

import "testing"

// r builds a Rule with BackendPort defaulting to Port — keeps the
// pre-F-46 test cases readable without changing their semantics.
func r(proto string, port uint16) Rule {
	return Rule{Proto: proto, Port: port, BackendPort: port}
}

// xr builds a port-translating Rule for F-46 cases.
func xr(proto string, dmzPort, backendPort uint16) Rule {
	return Rule{Proto: proto, Port: dmzPort, BackendPort: backendPort}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      map[string]string
		want    *Spec
		wantErr bool
	}{
		{
			name: "absent",
			in:   map[string]string{},
			want: nil,
		},
		{
			name: "empty string ignored",
			in:   map[string]string{LabelExpose: ""},
			want: nil,
		},
		{
			name: "single tcp",
			in:   map[string]string{LabelExpose: "tcp/25"},
			want: &Spec{Rules: []Rule{r("tcp", 25)}, V6: V6Auto},
		},
		{
			name: "mixed protos with whitespace",
			in:   map[string]string{LabelExpose: " tcp/25 , udp/4500 ,tcp/465 "},
			want: &Spec{
				Rules: []Rule{
					r("tcp", 25),
					r("udp", 4500),
					r("tcp", 465),
				},
				V6: V6Auto,
			},
		},
		{
			name: "v6 off",
			in: map[string]string{
				LabelExpose:   "tcp/443",
				LabelExposeV6: "off",
			},
			want: &Spec{Rules: []Rule{r("tcp", 443)}, V6: V6Off},
		},
		{
			name:    "bad proto",
			in:      map[string]string{LabelExpose: "sctp/22"},
			wantErr: true,
		},
		{
			name:    "bad port",
			in:      map[string]string{LabelExpose: "tcp/abc"},
			wantErr: true,
		},
		{
			name:    "port zero",
			in:      map[string]string{LabelExpose: "tcp/0"},
			wantErr: true,
		},
		{
			name:    "missing port",
			in:      map[string]string{LabelExpose: "tcp/"},
			wantErr: true,
		},

		// ---- F-46 port-translating DNAT cases ----------------------
		{
			name: "F-46 translation — Authentik LDAPS 636 -> 6636",
			in:   map[string]string{LabelExpose: "tcp/636:6636"},
			want: &Spec{Rules: []Rule{xr("tcp", 636, 6636)}, V6: V6Auto},
		},
		{
			name: "F-46 mixed list — one translating, one not",
			in:   map[string]string{LabelExpose: "tcp/443,tcp/636:6636"},
			want: &Spec{Rules: []Rule{
				r("tcp", 443),
				xr("tcp", 636, 6636),
			}, V6: V6Auto},
		},
		{
			name: "F-46 udp translation also supported",
			in:   map[string]string{LabelExpose: "udp/53:5353"},
			want: &Spec{Rules: []Rule{xr("udp", 53, 5353)}, V6: V6Auto},
		},
		{
			name: "F-46 whitespace around translation suffix tolerated",
			in:   map[string]string{LabelExpose: "tcp/ 636 : 6636 "},
			want: &Spec{Rules: []Rule{xr("tcp", 636, 6636)}, V6: V6Auto},
		},
		{
			name:    "F-46 trailing colon (empty backend port) is fatal",
			in:      map[string]string{LabelExpose: "tcp/636:"},
			wantErr: true,
		},
		{
			name:    "F-46 backend port 0 rejected",
			in:      map[string]string{LabelExpose: "tcp/636:0"},
			wantErr: true,
		},
		{
			name:    "F-46 backend port out of range",
			in:      map[string]string{LabelExpose: "tcp/636:99999"},
			wantErr: true,
		},
		{
			name:    "F-46 non-numeric backend port",
			in:      map[string]string{LabelExpose: "tcp/636:six-six-three-six"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
			if got == nil {
				return
			}
			if got.V6 != tc.want.V6 {
				t.Errorf("V6: got=%v want=%v", got.V6, tc.want.V6)
			}
			if len(got.Rules) != len(tc.want.Rules) {
				t.Fatalf("rules len: got=%d want=%d", len(got.Rules), len(tc.want.Rules))
			}
			for i := range got.Rules {
				if got.Rules[i] != tc.want.Rules[i] {
					t.Errorf("rule[%d]: got=%v want=%v", i, got.Rules[i], tc.want.Rules[i])
				}
			}
		})
	}
}
