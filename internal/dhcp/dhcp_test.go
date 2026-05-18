package dhcp

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/AlexCherrypi/anchord/internal/config"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv6"
)

// Tests in this file deliberately avoid the netlink-bound surface
// (applyV4Lease, applyV6Addrs, watchIP, the DHCP client I/O) — those
// are exercised end-to-end by test/e2e, which runs against a real
// container with NET_ADMIN. Here we cover the pure helpers that don't
// need a netlink socket plus the mode-dispatch logic of Run().

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

// TestRun_PassiveModes guards the v2 contract: in bootstrap and
// slaac-ra-only the supervisor must NOT touch netlink — Docker owns
// the link. The test cancels the context immediately; if the
// supervisor tried to open an nclient4/nclient6 socket on a missing
// iface it would block on retries or panic. A clean ctx.Err() return
// proves Run() short-circuited without I/O.
func TestRun_PassiveModes(t *testing.T) {
	for _, mode := range []config.AddressMode{
		config.AddressModeBootstrap,
		config.AddressModeSLAACRAOnly,
	} {
		t.Run(string(mode), func(t *testing.T) {
			s := New(mode, "no-such-iface", "host", time.Second)

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			start := time.Now()
			err := s.Run(ctx)
			elapsed := time.Since(start)

			if err != context.DeadlineExceeded {
				t.Fatalf("got err=%v, want context.DeadlineExceeded", err)
			}
			// Passive modes return immediately on ctx done — give
			// generous slack for CI variance but reject "spent the
			// whole timeout retrying DHCP" behaviour.
			if elapsed > 500*time.Millisecond {
				t.Errorf("passive mode blocked for %s, expected near-deadline", elapsed)
			}
		})
	}
}

// TestRun_UnknownMode is a defence-in-depth check: even though config
// rejects unknown modes at load time, Run() should not silently
// accept a corrupt Supervisor.
func TestRun_UnknownMode(t *testing.T) {
	s := New(config.AddressMode("garbage"), "eth0", "host", time.Second)
	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestClientID_PrefixesType(t *testing.T) {
	// RFC 2132 §9.14: first byte is the type tag, 0x00 = "other".
	got := clientID("mailcow")
	want := append([]byte{0x00}, []byte("mailcow")...)
	if !bytes.Equal(got, want) {
		t.Errorf("got % x want % x", got, want)
	}
}

func TestClientID_StableAcrossCalls(t *testing.T) {
	// Same hostname must yield the same client-id; that's the whole
	// point — DHCP servers need to recognise the same client across
	// container recreates regardless of MAC.
	a := clientID("mailcow")
	b := clientID("mailcow")
	if !bytes.Equal(a, b) {
		t.Errorf("not stable: %v vs %v", a, b)
	}
	c := clientID("nextcloud")
	if bytes.Equal(a, c) {
		t.Errorf("distinct hostnames collided: %v", a)
	}
}

// encodeUint32 is the wire encoding of a 32-bit DHCP option value:
// 4 bytes big-endian. Used by the renewal-time test to construct an
// explicit T1 option.
func encodeUint32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
