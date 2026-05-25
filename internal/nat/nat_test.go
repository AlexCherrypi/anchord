package nat

import (
	"bytes"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
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
	t.Run("typical eth0", func(t *testing.T) {
		got := ifaceBytes("eth0")
		if len(got) != 16 {
			t.Fatalf("want 16 bytes, got %d", len(got))
		}
		if !bytes.HasPrefix(got, []byte("eth0")) {
			t.Errorf("prefix mismatch: got % x", got)
		}
		// Trailing twelve bytes must all be NUL.
		for i := len("eth0"); i < 16; i++ {
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

// TestPreroutingGuardExprs pins the F-47 prerouting guard shape.
// v4 must always use `fib daddr type local` (Fib + Cmp == RTN_LOCAL).
// v6 uses fib when the kernel supports it (probeV6FibSupported in
// Setup), else falls back to `iifname == extIface`. A regression
// that re-introduces iifname for v4 — or drops fib for v6 even when
// the kernel supports it — surfaces here as a mismatched list.
func TestPreroutingGuardExprs(t *testing.T) {
	assertFibLocal := func(t *testing.T, got []expr.Any) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("expected 2 exprs, got %d", len(got))
		}
		fib, ok := got[0].(*expr.Fib)
		if !ok {
			t.Fatalf("first expr is %T, want *expr.Fib", got[0])
		}
		if !fib.FlagDADDR {
			t.Error("Fib.FlagDADDR not set")
		}
		if !fib.ResultADDRTYPE {
			t.Error("Fib.ResultADDRTYPE not set")
		}
		if fib.Register != 1 {
			t.Errorf("Fib.Register = %d, want 1", fib.Register)
		}
		cmp, ok := got[1].(*expr.Cmp)
		if !ok {
			t.Fatalf("second expr is %T, want *expr.Cmp", got[1])
		}
		if cmp.Op != expr.CmpOpEq {
			t.Errorf("Cmp.Op = %v, want CmpOpEq", cmp.Op)
		}
		wantData := binaryutil.NativeEndian.PutUint32(uint32(unix.RTN_LOCAL))
		if !bytes.Equal(cmp.Data, wantData) {
			t.Errorf("Cmp.Data = % x, want % x (RTN_LOCAL)", cmp.Data, wantData)
		}
	}
	assertIifname := func(t *testing.T, got []expr.Any) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("expected 2 exprs, got %d", len(got))
		}
		m, ok := got[0].(*expr.Meta)
		if !ok || m.Key != expr.MetaKeyIIFNAME {
			t.Fatalf("first expr = %T, want Meta{IIFNAME}", got[0])
		}
		cmp, ok := got[1].(*expr.Cmp)
		if !ok || cmp.Op != expr.CmpOpEq {
			t.Fatalf("second expr unexpected: %T", got[1])
		}
		if !bytes.Equal(cmp.Data, ifaceBytes("eth0")) {
			t.Errorf("Cmp.Data = % x, want ifaceBytes(eth0)", cmp.Data)
		}
	}

	t.Run("v4 always uses fib (kernel support irrelevant)", func(t *testing.T) {
		assertFibLocal(t, preroutingGuardExprs("eth0", V4, false))
		assertFibLocal(t, preroutingGuardExprs("eth0", V4, true))
	})
	t.Run("v6 with fib support uses fib", func(t *testing.T) {
		assertFibLocal(t, preroutingGuardExprs("eth0", V6, true))
	})
	t.Run("v6 without fib support falls back to iifname", func(t *testing.T) {
		assertIifname(t, preroutingGuardExprs("eth0", V6, false))
	})
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
