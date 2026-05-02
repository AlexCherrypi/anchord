package labels

import "testing"

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
			want: &Spec{Rules: []Rule{{Proto: "tcp", Port: 25}}, V6: V6Auto},
		},
		{
			name: "mixed protos with whitespace",
			in:   map[string]string{LabelExpose: " tcp/25 , udp/4500 ,tcp/465 "},
			want: &Spec{
				Rules: []Rule{
					{Proto: "tcp", Port: 25},
					{Proto: "udp", Port: 4500},
					{Proto: "tcp", Port: 465},
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
			want: &Spec{Rules: []Rule{{Proto: "tcp", Port: 443}}, V6: V6Off},
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
