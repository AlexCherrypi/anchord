package dhcp

import (
	"context"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv6"
)

// Tests in this file deliberately avoid the netlink-bound surface
// (ensureLink, removeLink, watchIP, watchLinkUsable, the DHCP client
// I/O) — those are exercised end-to-end by test/e2e, which runs
// against a real container with NET_ADMIN. Here we cover the pure
// helpers that don't need a netlink socket.

func TestRenewalInterval_UsesT1(t *testing.T) {
	ack, err := dhcpv4.New(dhcpv4.WithLeaseTime(3600), dhcpv4.WithGeneric(dhcpv4.OptionRenewTimeValue, encodeUint32(900)))
	if err != nil {
		t.Fatalf("build ack: %v", err)
	}
	got := renewalInterval(ack)
	want := 15 * time.Minute
	if got != want {
		t.Errorf("renewalInterval with explicit T1=900s: got %s, want %s", got, want)
	}
}

func TestRenewalInterval_FallsBackToHalfLease(t *testing.T) {
	// Lease=1h, no T1 in the packet → expect 30m.
	ack, err := dhcpv4.New(dhcpv4.WithLeaseTime(3600))
	if err != nil {
		t.Fatalf("build ack: %v", err)
	}
	got := renewalInterval(ack)
	want := 30 * time.Minute
	if got != want {
		t.Errorf("renewalInterval without T1, lease=1h: got %s, want %s", got, want)
	}
}

func TestExtractV6Addrs_NoIANAYieldsNil(t *testing.T) {
	// A reply with no IA_NA option must produce no addresses, not
	// panic. This is the SLAAC-only-server path: the server replies
	// but doesn't hand out a stateful address.
	msg, err := dhcpv6.NewMessage()
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if got := extractV6Addrs(msg); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestSleepBackoff_DoublesBelowCap(t *testing.T) {
	s := &Supervisor{backoffMax: 5 * time.Second}
	backoff := 100 * time.Millisecond

	ok := s.sleepBackoff(context.Background(), &backoff)
	if !ok {
		t.Fatal("sleepBackoff returned false on un-cancelled ctx")
	}
	if backoff != 200*time.Millisecond {
		t.Errorf("backoff doubling: got %s, want 200ms", backoff)
	}
}

func TestSleepBackoff_CapsAtMax(t *testing.T) {
	s := &Supervisor{backoffMax: 250 * time.Millisecond}
	backoff := 200 * time.Millisecond // doubles to 400ms — should clamp to 250ms

	if ok := s.sleepBackoff(context.Background(), &backoff); !ok {
		t.Fatal("unexpected cancellation")
	}
	if backoff != 250*time.Millisecond {
		t.Errorf("cap behaviour: got %s, want 250ms", backoff)
	}

	// Subsequent call: already at cap, should stay at cap.
	if ok := s.sleepBackoff(context.Background(), &backoff); !ok {
		t.Fatal("unexpected cancellation")
	}
	if backoff != 250*time.Millisecond {
		t.Errorf("stays at cap: got %s, want 250ms", backoff)
	}
}

func TestSleepBackoff_RespectsContextCancel(t *testing.T) {
	s := &Supervisor{backoffMax: 10 * time.Second}
	backoff := 5 * time.Second // would block for 5s if not cancelled

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before call

	start := time.Now()
	ok := s.sleepBackoff(ctx, &backoff)
	elapsed := time.Since(start)

	if ok {
		t.Error("sleepBackoff returned true despite cancelled ctx")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("sleepBackoff blocked for %s, expected near-zero", elapsed)
	}
}

// encodeUint32 is the wire encoding of a 32-bit DHCP option value:
// 4 bytes big-endian. Used by the renewal-time test to construct an
// explicit T1 option.
func encodeUint32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

