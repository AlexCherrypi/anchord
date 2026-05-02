package dhcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Tests in this file deliberately avoid the netlink-bound surface
// (ensureLink, removeLink, watchIP, watchLinkUsable, the dhclient
// subprocess) — those are exercised end-to-end by test/e2e, which
// runs against a real container with NET_ADMIN. Here we cover the
// pure logic that doesn't need a netlink socket: the per-family
// dhclient.conf generator and the backoff helper.

func TestWriteDhclientConf_V4(t *testing.T) {
	s := &Supervisor{hostname: "mailcow"}

	path, cleanup, err := s.writeDhclientConf(4)
	if err != nil {
		t.Fatalf("writeDhclientConf(4): %v", err)
	}
	defer cleanup()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read conf: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `send host-name "mailcow";`) {
		t.Errorf("v4 conf missing host-name option: %q", got)
	}
	if strings.Contains(got, "dhcp6") {
		t.Errorf("v4 conf must not contain v6 options: %q", got)
	}
}

func TestWriteDhclientConf_V6(t *testing.T) {
	s := &Supervisor{hostname: "mailcow"}

	path, cleanup, err := s.writeDhclientConf(6)
	if err != nil {
		t.Fatalf("writeDhclientConf(6): %v", err)
	}
	defer cleanup()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read conf: %v", err)
	}
	got := string(body)
	// RFC 4704 Client-FQDN — flags=0 ("server decides"), then hostname.
	if !strings.Contains(got, `send dhcp6.client-fqdn "0 mailcow";`) {
		t.Errorf("v6 conf missing client-fqdn option: %q", got)
	}
	if strings.Contains(got, "host-name") {
		t.Errorf("v6 conf must not contain v4 host-name option: %q", got)
	}
}

func TestWriteDhclientConf_Cleanup(t *testing.T) {
	s := &Supervisor{hostname: "mailcow"}

	path, cleanup, err := s.writeDhclientConf(4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be removed after cleanup, stat err: %v", err)
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
