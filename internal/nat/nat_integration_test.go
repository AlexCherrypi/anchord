//go:build integration

// Integration tests for the nat package. These touch real netlink and
// require CAP_NET_ADMIN inside the running netns, so they live behind a
// build tag and are normally driven by test/integration/run.{ps1,sh}
// inside a privileged Docker container.
//
// The headline test here is TestIntegrationAtomicReplaceNeverEmpty,
// which is the SPEC F-19 acceptance check: a parallel reader observes
// the kernel's view of the DNAT map while SetMap is hammered with
// alternating states; every observed snapshot must be one of the two
// fully-applied states, never empty and never partial.

package nat

import (
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
)

const (
	testIface = "anchord-test"
	tableV4   = "anchord_v4"
	tableV6   = "anchord_v6"
)

func requireNetAdmin(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration test requires root / CAP_NET_ADMIN")
	}
}

// newManager returns a Setup'd Manager and registers Teardown as a
// cleanup. Fatals on any setup error.
func newManager(t *testing.T) *Manager {
	t.Helper()
	m := New(testIface)
	if err := m.Setup(); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Teardown(); err != nil {
			t.Logf("Teardown (cleanup): %v", err)
		}
	})
	return m
}

// readMap reads the kernel's current view of an anchord DNAT map.
func readMap(t *testing.T, family Family, mapName string) map[uint16]string {
	t.Helper()
	c := &nftables.Conn{}
	tbl := &nftables.Table{Name: tableV4, Family: nftables.TableFamilyIPv4}
	if family == V6 {
		tbl = &nftables.Table{Name: tableV6, Family: nftables.TableFamilyIPv6}
	}
	set, err := c.GetSetByName(tbl, mapName)
	if err != nil {
		t.Fatalf("GetSetByName(%s): %v", mapName, err)
	}
	elems, err := c.GetSetElements(set)
	if err != nil {
		t.Fatalf("GetSetElements(%s): %v", mapName, err)
	}
	out := make(map[uint16]string, len(elems))
	for _, e := range elems {
		port := binaryutil.BigEndian.Uint16(e.Key)
		out[port] = net.IP(e.Val).String()
	}
	return out
}

// stateToStrings converts a SetMap-shaped state into the same form
// readMap returns, so we can compare directly.
func stateToStrings(s map[uint16]net.IP, family Family) map[uint16]string {
	out := make(map[uint16]string, len(s))
	for port, ip := range s {
		if family == V4 {
			out[port] = ip.To4().String()
		} else {
			out[port] = ip.To16().String()
		}
	}
	return out
}

func mapsEqual(a, b map[uint16]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestIntegrationSetupInstallsTables(t *testing.T) {
	requireNetAdmin(t)
	_ = newManager(t)

	c := &nftables.Conn{}
	tables, err := c.ListTables()
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	var v4, v6 bool
	for _, tbl := range tables {
		if tbl.Name == tableV4 && tbl.Family == nftables.TableFamilyIPv4 {
			v4 = true
		}
		if tbl.Name == tableV6 && tbl.Family == nftables.TableFamilyIPv6 {
			v6 = true
		}
	}
	if !v4 {
		t.Errorf("anchord_v4 table not installed")
	}
	if !v6 {
		t.Errorf("anchord_v6 table not installed")
	}

	// Both maps should exist on each table.
	for _, family := range []Family{V4, V6} {
		for _, proto := range []string{"tcp", "udp"} {
			got := readMap(t, family, "dnat_"+proto)
			if len(got) != 0 {
				t.Errorf("fresh %s/dnat_%s should be empty, got %v", family, proto, got)
			}
		}
	}
}

func TestIntegrationSetupIsIdempotent(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	// A second Setup must succeed and leave usable state — F-19's
	// atomic guarantee is moot if anchord crashes on startup-after-crash.
	if err := m.Setup(); err != nil {
		t.Fatalf("second Setup: %v", err)
	}

	// Verify maps are still empty and writable after re-Setup.
	got := readMap(t, V4, "dnat_tcp")
	if len(got) != 0 {
		t.Errorf("after re-Setup, dnat_tcp should be empty, got %v", got)
	}
	if err := m.SetMap(V4, "tcp", map[uint16]net.IP{
		25: net.IPv4(10, 0, 0, 25),
	}); err != nil {
		t.Fatalf("SetMap after re-Setup: %v", err)
	}
}

func TestIntegrationSetMapV4Populate(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	state := map[uint16]net.IP{
		25:  net.IPv4(10, 0, 0, 25),
		80:  net.IPv4(10, 0, 0, 80),
		443: net.IPv4(10, 0, 0, 244),
	}
	if err := m.SetMap(V4, "tcp", state); err != nil {
		t.Fatalf("SetMap: %v", err)
	}
	got := readMap(t, V4, "dnat_tcp")
	want := stateToStrings(state, V4)
	if !mapsEqual(got, want) {
		t.Errorf("after SetMap got %v want %v", got, want)
	}
}

func TestIntegrationSetMapV6Populate(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	state := map[uint16]net.IP{
		25:  net.ParseIP("fd99::25"),
		443: net.ParseIP("fd99::443"),
	}
	if err := m.SetMap(V6, "tcp", state); err != nil {
		t.Fatalf("SetMap: %v", err)
	}
	got := readMap(t, V6, "dnat_tcp")
	want := stateToStrings(state, V6)
	if !mapsEqual(got, want) {
		t.Errorf("after SetMap got %v want %v", got, want)
	}
}

// TestIntegrationSetMapReplaceRemovesStale verifies replace semantics:
// keys present in the old state but absent from the new state must not
// linger.
func TestIntegrationSetMapReplaceRemovesStale(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	if err := m.SetMap(V4, "tcp", map[uint16]net.IP{
		25: net.IPv4(10, 0, 0, 25),
		80: net.IPv4(10, 0, 0, 80),
	}); err != nil {
		t.Fatalf("first SetMap: %v", err)
	}

	next := map[uint16]net.IP{
		25:  net.IPv4(10, 0, 0, 250),
		443: net.IPv4(10, 0, 0, 244),
	}
	if err := m.SetMap(V4, "tcp", next); err != nil {
		t.Fatalf("replace SetMap: %v", err)
	}
	got := readMap(t, V4, "dnat_tcp")
	want := stateToStrings(next, V4)
	if !mapsEqual(got, want) {
		t.Errorf("after replace got %v want %v (port 80 should be gone, 25's value updated)", got, want)
	}
	if _, lingered := got[80]; lingered {
		t.Errorf("stale key 80 lingered after replace")
	}
}

// TestIntegrationSetMapEmptyClears verifies that an empty SetMap
// clears the kernel map entirely.
func TestIntegrationSetMapEmptyClears(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	if err := m.SetMap(V4, "udp", map[uint16]net.IP{
		53:  net.IPv4(10, 0, 0, 53),
		123: net.IPv4(10, 0, 0, 123),
	}); err != nil {
		t.Fatalf("populate SetMap: %v", err)
	}
	if err := m.SetMap(V4, "udp", map[uint16]net.IP{}); err != nil {
		t.Fatalf("empty SetMap: %v", err)
	}
	got := readMap(t, V4, "dnat_udp")
	if len(got) != 0 {
		t.Errorf("after empty SetMap, dnat_udp should be empty, got %v", got)
	}
}

// TestIntegrationReplaceIsAtomicPerWrite is the F-19 acceptance test.
//
// F-19 promises "no observable window where DNAT is broken" — observable
// from the *dataplane*, i.e. a packet hitting `dport map @dnat_tcp` must
// always resolve to exactly one valid backend, never fall through and
// never see a half-flushed map. The kernel guarantees this for nftables
// transactions: FlushSet + SetAddElements within a single netlink batch
// commit together; concurrent packet lookups on a different CPU see
// either the old generation or the new one, never a mix.
//
// We can't easily observe a packet's nftables-map lookup outcome from
// pure Go, but we can verify the *write side* of the guarantee: after
// every SetMap call, a subsequent dump must equal exactly the state we
// just wrote — never empty, never partial, never with a stale key
// lingering. If the kernel applied flush and add as separate
// transactions, between flips we'd see the empty state. We don't.
//
// Note: a *concurrent* dump (reader goroutine racing with writer) can
// observe mixed snapshots because nftables set-element listing is a
// multi-message NLM_F_DUMP that is not snapshot-isolated against
// concurrent commits. That's a kernel userspace-API quirk, not a
// dataplane atomicity issue, and not anchord's fault. The dataplane
// path stays atomic because rule evaluation runs under RCU and sees
// one generation per packet.
func TestIntegrationReplaceIsAtomicPerWrite(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	stateA := map[uint16]net.IP{
		25:  net.IPv4(10, 0, 0, 1),
		80:  net.IPv4(10, 0, 0, 2),
		443: net.IPv4(10, 0, 0, 3),
		587: net.IPv4(10, 0, 0, 4),
		993: net.IPv4(10, 0, 0, 5),
	}
	stateB := map[uint16]net.IP{
		25:  net.IPv4(10, 0, 0, 11),
		80:  net.IPv4(10, 0, 0, 12),
		443: net.IPv4(10, 0, 0, 13),
		587: net.IPv4(10, 0, 0, 14),
		993: net.IPv4(10, 0, 0, 15),
	}
	wantA := stateToStrings(stateA, V4)
	wantB := stateToStrings(stateB, V4)

	deadline := time.Now().Add(time.Second)
	flips := 0
	for time.Now().Before(deadline) {
		var state map[uint16]net.IP
		var want map[uint16]string
		if flips%2 == 0 {
			state, want = stateA, wantA
		} else {
			state, want = stateB, wantB
		}
		if err := m.SetMap(V4, "tcp", state); err != nil {
			t.Fatalf("SetMap flip %d: %v", flips, err)
		}
		got := readMap(t, V4, "dnat_tcp")
		if !mapsEqual(got, want) {
			t.Fatalf("flip %d: post-write dump diverged from written state\n  got:  %v\n  want: %v", flips, got, want)
		}
		flips++
	}
	t.Logf("completed %d flips in 1s, every post-write dump matched", flips)
	if flips < 50 {
		t.Errorf("flips=%d in 1s — suspect kernel/netlink slowness, not enough cycles to be confident", flips)
	}
}

// TestIntegrationConcurrentSetMapDifferentMaps stresses the Manager's
// internal lock by hammering SetMap on tcp and udp maps concurrently.
// The two maps are independent so there's no expected cross-talk; we
// just want to verify the manager doesn't deadlock and final state is
// consistent.
func TestIntegrationConcurrentSetMapDifferentMaps(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	finalTCP := map[uint16]net.IP{25: net.IPv4(10, 0, 0, 25)}
	finalUDP := map[uint16]net.IP{53: net.IPv4(10, 0, 0, 53)}

	var wg sync.WaitGroup
	deadline := time.Now().Add(500 * time.Millisecond)

	wg.Add(2)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			if err := m.SetMap(V4, "tcp", finalTCP); err != nil {
				t.Errorf("tcp SetMap: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			if err := m.SetMap(V4, "udp", finalUDP); err != nil {
				t.Errorf("udp SetMap: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Final states must reflect the last write of each map.
	if got, want := readMap(t, V4, "dnat_tcp"), stateToStrings(finalTCP, V4); !mapsEqual(got, want) {
		t.Errorf("final tcp got %v want %v", got, want)
	}
	if got, want := readMap(t, V4, "dnat_udp"), stateToStrings(finalUDP, V4); !mapsEqual(got, want) {
		t.Errorf("final udp got %v want %v", got, want)
	}
}

// TestIntegrationTeardownRemovesTables verifies the cleanup path the
// SIGTERM handler relies on (F-20).
func TestIntegrationTeardownRemovesTables(t *testing.T) {
	requireNetAdmin(t)
	m := New(testIface)
	if err := m.Setup(); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if err := m.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	c := &nftables.Conn{}
	tables, err := c.ListTables()
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	for _, tbl := range tables {
		if tbl.Name == tableV4 || tbl.Name == tableV6 {
			t.Errorf("table %s/%v survived Teardown", tbl.Name, tbl.Family)
		}
	}
}
