package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRegistryHasAllMetrics asserts every documented metric (SPEC §2.7
// surface tables) registered cleanly. Catches typos in metric names
// and label sets.
func TestRegistryHasAllMetrics(t *testing.T) {
	// Defensive: re-warm in case a sibling test cleared a series.
	// init() warms once, but Clear()-based tests can drop entries.
	warmup()

	mfs, err := Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, m := range mfs {
		got[m.GetName()] = true
	}
	want := []string{
		"anchord_reconcile_duration_seconds",
		"anchord_reconcile_total",
		"anchord_dhcp_lease_remaining_seconds",
		"anchord_dhcp_acquired_total",
		"anchord_dhcp_client_restarts_total",
		"anchord_dnat_entries",
		"anchord_conntrack_flushes_total",
		"anchord_docker_events_total",
		"anchord_gateway_resolve_total",
		"anchord_gateway_route_replaces_total",
		"anchord_default_route_present",
		"anchord_build_info",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("metric %s not registered", name)
		}
	}
}

func TestServe_ServesMetrics(t *testing.T) {
	// Port 0: kernel picks a free port, so concurrent test runs don't collide.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, nil) }()

	// Poll for readiness; Serve is asynchronous.
	url := "http://" + addr + "/metrics"
	deadline := time.Now().Add(2 * time.Second)
	var body []byte
	for {
		resp, err := http.Get(url)
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics endpoint never became ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !strings.Contains(string(body), "anchord_build_info") {
		t.Errorf("expected anchord_build_info in /metrics output, got:\n%s", body)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve returned error: %v", err)
	}
}

func TestServe_BindFailureReturnsError(t *testing.T) {
	// Take a port, then try to bind the same address twice — the second
	// attempt must surface the bind error so the caller can log it
	// (per SPEC F-32) without taking down the data plane.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, addr, nil) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected bind error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return on bind failure")
	}
}

func TestLeaseRemaining_ClampsNegative(t *testing.T) {
	// A lease that already expired (deadline in the past) should
	// surface as 0, not as a negative gauge — alerts on
	// "remaining < 60s" should fire, not be confused by a sign.
	t.Cleanup(func() { DHCPLeaseRemaining.Clear("v4") })
	DHCPLeaseRemaining.Set("v4", time.Now().Add(-1*time.Hour))

	got := gatherLeaseRemaining(t, "v4")
	if got != 0 {
		t.Errorf("expected 0, got %v", got)
	}
}

func TestLeaseRemaining_DecaysAtScrapeTime(t *testing.T) {
	// Setting a 1-hour deadline and then scraping twice with a delay
	// must show the value strictly decreasing — proves the gauge is
	// computed at scrape time, not frozen at Set().
	t.Cleanup(func() { DHCPLeaseRemaining.Clear("v6") })
	DHCPLeaseRemaining.Set("v6", time.Now().Add(time.Hour))

	first := gatherLeaseRemaining(t, "v6")
	time.Sleep(20 * time.Millisecond)
	second := gatherLeaseRemaining(t, "v6")

	if !(first > second) {
		t.Errorf("expected remaining to decay; first=%v second=%v", first, second)
	}
}

func TestLeaseRemaining_ClearDropsSeries(t *testing.T) {
	// Clear() must drop the series for that family, not leave it at zero.
	t.Cleanup(func() {
		// Restore the warmup baseline so order-sensitive sibling tests
		// (e.g. TestRegistryHasAllMetrics) still see v4 emitted.
		DHCPLeaseRemaining.Set("v4", time.Now())
	})
	DHCPLeaseRemaining.Set("v4", time.Now().Add(time.Hour))
	DHCPLeaseRemaining.Clear("v4")

	mfs, err := Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, m := range mfs {
		if m.GetName() != "anchord_dhcp_lease_remaining_seconds" {
			continue
		}
		for _, mm := range m.GetMetric() {
			for _, l := range mm.GetLabel() {
				if l.GetName() == "family" && l.GetValue() == "v4" {
					t.Error("v4 series should have been cleared")
				}
			}
		}
	}
}

func gatherLeaseRemaining(t *testing.T, family string) float64 {
	t.Helper()
	mfs, err := Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, m := range mfs {
		if m.GetName() != "anchord_dhcp_lease_remaining_seconds" {
			continue
		}
		for _, mm := range m.GetMetric() {
			for _, l := range mm.GetLabel() {
				if l.GetName() == "family" && l.GetValue() == family {
					return mm.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("no lease_remaining series for family=%s", family)
	return 0
}
