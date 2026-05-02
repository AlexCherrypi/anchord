package nat

import (
	"bytes"
	"testing"

	"github.com/google/nftables"
)

func TestFamilyString(t *testing.T) {
	if got := V4.String(); got != "v4" {
		t.Errorf("V4.String() = %q, want v4", got)
	}
	if got := V6.String(); got != "v6" {
		t.Errorf("V6.String() = %q, want v6", got)
	}
}

func TestIfaceBytes(t *testing.T) {
	t.Run("short name padded", func(t *testing.T) {
		got := ifaceBytes("eth0")
		if len(got) != 16 {
			t.Fatalf("want 16 bytes, got %d", len(got))
		}
		want := append([]byte("eth0"), bytes.Repeat([]byte{0}, 12)...)
		if !bytes.Equal(got, want) {
			t.Errorf("got % x, want % x", got, want)
		}
	})
	t.Run("typical anchord-ext", func(t *testing.T) {
		got := ifaceBytes("anchord-ext")
		if len(got) != 16 {
			t.Fatalf("want 16 bytes, got %d", len(got))
		}
		if !bytes.HasPrefix(got, []byte("anchord-ext")) {
			t.Errorf("prefix mismatch: got % x", got)
		}
		// Trailing five bytes must all be NUL.
		for i := len("anchord-ext"); i < 16; i++ {
			if got[i] != 0 {
				t.Errorf("byte %d should be NUL, got %x", i, got[i])
			}
		}
	})
	t.Run("empty", func(t *testing.T) {
		got := ifaceBytes("")
		if len(got) != 16 {
			t.Fatalf("want 16 bytes, got %d", len(got))
		}
		for i, b := range got {
			if b != 0 {
				t.Errorf("byte %d should be NUL, got %x", i, b)
			}
		}
	})
}

func TestAddressFamily(t *testing.T) {
	if got, want := addressFamily(V4), uint32(nftables.TableFamilyIPv4); got != want {
		t.Errorf("V4: got %d want %d", got, want)
	}
	if got, want := addressFamily(V6), uint32(nftables.TableFamilyIPv6); got != want {
		t.Errorf("V6: got %d want %d", got, want)
	}
}

// TestMapForFamProto verifies the family/proto -> set lookup table
// without touching the kernel. We construct a Manager with stub Sets
// and assert the dispatch.
func TestMapForFamProto(t *testing.T) {
	v4tcp := &nftables.Set{Name: "v4tcp"}
	v4udp := &nftables.Set{Name: "v4udp"}
	v6tcp := &nftables.Set{Name: "v6tcp"}
	v6udp := &nftables.Set{Name: "v6udp"}
	m := &Manager{
		mapV4TCP: v4tcp, mapV4UDP: v4udp,
		mapV6TCP: v6tcp, mapV6UDP: v6udp,
	}
	cases := []struct {
		fam   Family
		proto string
		want  *nftables.Set
	}{
		{V4, "tcp", v4tcp},
		{V4, "udp", v4udp},
		{V6, "tcp", v6tcp},
		{V6, "udp", v6udp},
		{V4, "sctp", nil},
		{V6, "icmp", nil},
		{V4, "", nil},
	}
	for _, tc := range cases {
		if got := m.mapForFamProto(tc.fam, tc.proto); got != tc.want {
			t.Errorf("%v/%q: got %v want %v", tc.fam, tc.proto, got, tc.want)
		}
	}
}
