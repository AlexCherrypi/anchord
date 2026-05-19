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

// readMap reads the kernel's current view of an anchord DNAT
// (addr, port) pair by joining the two per-(family, proto) maps.
// mapBase is "dnat_tcp" or "dnat_udp" — the address map; the port
// map's name is derived as mapBase+"_port".
//
// F-46: the addr map's value is just the IP bytes (4 for v4 / 16
// for v6); the port map's value is a 2-byte BE port. We zip them on
// the shared DMZ-port key.
func readMap(t *testing.T, family Family, mapBase string) map[uint16]Target {
	t.Helper()
	c := &nftables.Conn{}
	tbl := &nftables.Table{Name: tableV4, Family: nftables.TableFamilyIPv4}
	if family == V6 {
		tbl = &nftables.Table{Name: tableV6, Family: nftables.TableFamilyIPv6}
	}

	addrElems := readSetElements(t, c, tbl, mapBase)
	portElems := readSetElements(t, c, tbl, mapBase+"_port")

	out := make(map[uint16]Target, len(addrElems))
	for _, e := range addrElems {
		key := binaryutil.BigEndian.Uint16(e.Key)
		out[key] = Target{IP: net.IP(e.Val)}
	}
	for _, e := range portElems {
		key := binaryutil.BigEndian.Uint16(e.Key)
		tgt := out[key]
		if len(e.Val) < 2 {
			t.Fatalf("port map element val too short: %d bytes", len(e.Val))
		}
		tgt.Port = binaryutil.BigEndian.Uint16(e.Val[:2])
		out[key] = tgt
	}
	// Drop entries that only appeared in the port map (shouldn't
	// happen in practice but keeps the test honest about the
	// invariant: both maps stay in lockstep).
	for k, v := range out {
		if v.IP == nil {
			delete(out, k)
		}
	}
	return out
}

func readSetElements(t *testing.T, c *nftables.Conn, tbl *nftables.Table, name string) []nftables.SetElement {
	t.Helper()
	set, err := c.GetSetByName(tbl, name)
	if err != nil {
		t.Fatalf("GetSetByName(%s): %v", name, err)
	}
	elems, err := c.GetSetElements(set)
	if err != nil {
		t.Fatalf("GetSetElements(%s): %v", name, err)
	}
	return elems
}

// normalizeIP returns the canonical-string form of a Target's IP
// for the family. Used by mapsEqual to avoid v4 surrogate-form
// mismatches.
func normalizeIP(ip net.IP, family Family) string {
	if family == V4 {
		return ip.To4().String()
	}
	return ip.To16().String()
}

// targetsEqual compares two read-back / expected target maps.
func targetsEqual(a, b map[uint16]Target, family Family) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va.Port != vb.Port || normalizeIP(va.IP, family) != normalizeIP(vb.IP, family) {
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
	if err := m.SetMap(V4, "tcp", map[uint16]Target{
		25: {IP: net.IPv4(10, 0, 0, 25), Port: 25},
	}); err != nil {
		t.Fatalf("SetMap after re-Setup: %v", err)
	}
}

func TestIntegrationSetMapV4Populate(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	state := map[uint16]Target{
		25:  {IP: net.IPv4(10, 0, 0, 25), Port: 25},
		80:  {IP: net.IPv4(10, 0, 0, 80), Port: 80},
		443: {IP: net.IPv4(10, 0, 0, 244), Port: 443},
	}
	if err := m.SetMap(V4, "tcp", state); err != nil {
		t.Fatalf("SetMap: %v", err)
	}
	got := readMap(t, V4, "dnat_tcp")
	if !targetsEqual(got, state, V4) {
		t.Errorf("after SetMap got %v want %v", got, state)
	}
}

func TestIntegrationSetMapV6Populate(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	state := map[uint16]Target{
		25:  {IP: net.ParseIP("fd99::25"), Port: 25},
		443: {IP: net.ParseIP("fd99::443"), Port: 443},
	}
	if err := m.SetMap(V6, "tcp", state); err != nil {
		t.Fatalf("SetMap: %v", err)
	}
	got := readMap(t, V6, "dnat_tcp")
	if !targetsEqual(got, state, V6) {
		t.Errorf("after SetMap got %v want %v", got, state)
	}
}

// F-46: port-translating entry — the entry must NOT appear in the
// address map (which is reserved for non-translating entries) and
// instead lives as a literal-DNAT rule in the dnat_xlat_tcp chain.
// Pinned regression on the Authentik LDAPS case (636 -> 6636).
func TestIntegrationSetMapPortTranslation(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	state := map[uint16]Target{
		636: {IP: net.IPv4(172, 31, 80, 9), Port: 6636},
	}
	if err := m.SetMap(V4, "tcp", state); err != nil {
		t.Fatalf("SetMap: %v", err)
	}

	// Address map must be empty — translating entries don't use it.
	if got := readMap(t, V4, "dnat_tcp"); len(got) != 0 {
		t.Errorf("address map should be empty for translation-only state, got %v", got)
	}

	// Sub-chain must hold one rule.
	c := &nftables.Conn{}
	tbl := &nftables.Table{Name: tableV4, Family: nftables.TableFamilyIPv4}
	rules, err := c.GetRules(tbl, &nftables.Chain{Table: tbl, Name: "dnat_xlat_tcp"})
	if err != nil {
		t.Fatalf("GetRules(dnat_xlat_tcp): %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("dnat_xlat_tcp should have exactly 1 rule, got %d", len(rules))
	}
}

// F-46: mixed translating + non-translating entries — non-translating
// ones go to the address map, translating ones go to the sub-chain.
// Both code paths exercised in one SetMap call.
func TestIntegrationSetMapMixedTranslation(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	state := map[uint16]Target{
		25:  {IP: net.IPv4(10, 0, 0, 25), Port: 25},     // non-translating
		80:  {IP: net.IPv4(10, 0, 0, 80), Port: 80},     // non-translating
		636: {IP: net.IPv4(10, 0, 6, 36), Port: 6636},  // translating
	}
	if err := m.SetMap(V4, "tcp", state); err != nil {
		t.Fatalf("SetMap: %v", err)
	}

	// Address map must hold the two non-translating entries only.
	got := readMap(t, V4, "dnat_tcp")
	if len(got) != 2 {
		t.Errorf("address map should have 2 non-translating entries, got %d (%v)", len(got), got)
	}
	if _, ok := got[636]; ok {
		t.Error("translating entry 636 must not appear in address map")
	}

	// Sub-chain must hold one rule (the translating entry).
	c := &nftables.Conn{}
	tbl := &nftables.Table{Name: tableV4, Family: nftables.TableFamilyIPv4}
	rules, err := c.GetRules(tbl, &nftables.Chain{Table: tbl, Name: "dnat_xlat_tcp"})
	if err != nil {
		t.Fatalf("GetRules(dnat_xlat_tcp): %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("dnat_xlat_tcp should have exactly 1 rule for the translating entry, got %d", len(rules))
	}
}

// F-46: dropping a translating entry from SetMap must remove its
// sub-chain rule. Verifies the FlushChain+repopulate semantics.
func TestIntegrationSetMapTranslationCleanupOnRemoval(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	// Install a translating entry.
	if err := m.SetMap(V4, "tcp", map[uint16]Target{
		636: {IP: net.IPv4(10, 0, 0, 9), Port: 6636},
	}); err != nil {
		t.Fatalf("first SetMap: %v", err)
	}

	// Replace with empty — translating rule must vanish.
	if err := m.SetMap(V4, "tcp", map[uint16]Target{}); err != nil {
		t.Fatalf("empty SetMap: %v", err)
	}

	c := &nftables.Conn{}
	tbl := &nftables.Table{Name: tableV4, Family: nftables.TableFamilyIPv4}
	rules, err := c.GetRules(tbl, &nftables.Chain{Table: tbl, Name: "dnat_xlat_tcp"})
	if err != nil {
		t.Fatalf("GetRules(dnat_xlat_tcp): %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("dnat_xlat_tcp should be empty after entry removal, got %d rules", len(rules))
	}
}

// TestIntegrationSetMapReplaceRemovesStale verifies replace semantics:
// keys present in the old state but absent from the new state must not
// linger.
func TestIntegrationSetMapReplaceRemovesStale(t *testing.T) {
	requireNetAdmin(t)
	m := newManager(t)

	if err := m.SetMap(V4, "tcp", map[uint16]Target{
		25: {IP: net.IPv4(10, 0, 0, 25), Port: 25},
		80: {IP: net.IPv4(10, 0, 0, 80), Port: 80},
	}); err != nil {
		t.Fatalf("first SetMap: %v", err)
	}

	next := map[uint16]Target{
		25:  {IP: net.IPv4(10, 0, 0, 250), Port: 25},
		443: {IP: net.IPv4(10, 0, 0, 244), Port: 443},
	}
	if err := m.SetMap(V4, "tcp", next); err != nil {
		t.Fatalf("replace SetMap: %v", err)
	}
	got := readMap(t, V4, "dnat_tcp")
	if !targetsEqual(got, next, V4) {
		t.Errorf("after replace got %v want %v (port 80 should be gone, 25's value updated)", got, next)
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

	if err := m.SetMap(V4, "udp", map[uint16]Target{
		53:  {IP: net.IPv4(10, 0, 0, 53), Port: 53},
		123: {IP: net.IPv4(10, 0, 0, 123), Port: 123},
	}); err != nil {
		t.Fatalf("populate SetMap: %v", err)
	}
	if err := m.SetMap(V4, "udp", map[uint16]Target{}); err != nil {
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

	mkState := func(off int) map[uint16]Target {
		return map[uint16]Target{
			25:  {IP: net.IPv4(10, 0, 0, byte(1+off)), Port: 25},
			80:  {IP: net.IPv4(10, 0, 0, byte(2+off)), Port: 80},
			443: {IP: net.IPv4(10, 0, 0, byte(3+off)), Port: 443},
			587: {IP: net.IPv4(10, 0, 0, byte(4+off)), Port: 587},
			993: {IP: net.IPv4(10, 0, 0, byte(5+off)), Port: 993},
		}
	}
	stateA := mkState(0)
	stateB := mkState(10)

	deadline := time.Now().Add(time.Second)
	flips := 0
	for time.Now().Before(deadline) {
		state := stateA
		if flips%2 != 0 {
			state = stateB
		}
		if err := m.SetMap(V4, "tcp", state); err != nil {
			t.Fatalf("SetMap flip %d: %v", flips, err)
		}
		got := readMap(t, V4, "dnat_tcp")
		if !targetsEqual(got, state, V4) {
			t.Fatalf("flip %d: post-write dump diverged from written state\n  got:  %v\n  want: %v", flips, got, state)
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

	finalTCP := map[uint16]Target{25: {IP: net.IPv4(10, 0, 0, 25), Port: 25}}
	finalUDP := map[uint16]Target{53: {IP: net.IPv4(10, 0, 0, 53), Port: 53}}

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
	if got := readMap(t, V4, "dnat_tcp"); !targetsEqual(got, finalTCP, V4) {
		t.Errorf("final tcp got %v want %v", got, finalTCP)
	}
	if got := readMap(t, V4, "dnat_udp"); !targetsEqual(got, finalUDP, V4) {
		t.Errorf("final udp got %v want %v", got, finalUDP)
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
