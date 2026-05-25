// Package nat manages the nftables tables that implement DNAT for
// inbound traffic and masquerade for outbound traffic.
//
// We use a single table per address family ("anchord_v4", "anchord_v6")
// with two named maps each (one per L4 protocol) and two helper sub-
// chains. DNAT is implemented as a single rule that consults the map
// — atomic map updates mean rule changes are seamless and lock-free.
//
// F-46 port translation: when the operator requests a backend-side
// port that differs from the DMZ-side port (e.g. `tcp/636:6636` for
// Authentik LDAPS), the (address, port) tuple cannot be represented
// in a single nft map value with the google/nftables library — the
// library only emits NFTA_SET_DESC_CONCAT for concat *keys*, not
// concat *values*. The kernel also rejects a two-Lookup-then-NAT-
// with-RegProtoMin pattern (it expects port data co-located with
// address data, not loaded separately). So translating entries are
// implemented as explicit per-entry rules in a sub-chain that the
// main prerouting jumps to *before* the map-lookup rule.
//
// F-47 hairpin DNAT (issue #11): prerouting matches on
// `fib daddr type local` instead of `iifname == extIface`. This
// catches packets destined for any of the anchor's own addresses
// regardless of ingress interface, which is what the LAN-ingress
// path (dst = macvlan IP) AND the sibling-on-a-bridge hairpin path
// both need. Traffic the anchor is merely forwarding to an external
// destination has a non-local dst and falls through. The cost of
// hairpin is asymmetric routing — without intervention the backend
// would reply directly to the sibling on the shared bridge, bypassing
// the anchor and dropping the connection. Postrouting therefore
// SNATs DNAT'd traffic that did NOT enter via the macvlan, so the
// backend's reply goes back via the anchor (where conntrack reverses
// both translations). LAN-ingress is excluded from this SNAT — F-9
// (client source IP preservation) remains intact for external clients.
//
// v4 always uses the fib-based guard. v6 uses it when the running
// kernel loads `nft_fib_ipv6` — probed once at Setup. Stripped-down
// kernels (WSL2 in CI/dev) reject the expression at commit time;
// the probe falls back to the legacy `iifname == extIface` predicate
// for v6 in that case. v6 hairpin is then lost, but v4 hairpin and
// all v6 LAN-ingress DNAT still work.
//
// Layout (v4 example, with ANCHORD_EXT_IFACE="eth0"):
//
//	table ip anchord_v4 {
//	  map dnat_tcp { type inet_service : ipv4_addr; }
//	  map dnat_udp { type inet_service : ipv4_addr; }
//	  chain dnat_xlat_tcp { … }
//	  chain dnat_xlat_udp { … }
//	  chain prerouting {
//	    type nat hook prerouting priority dstnat;
//	    fib daddr type local meta l4proto tcp jump dnat_xlat_tcp
//	    fib daddr type local meta l4proto tcp dnat ip to tcp dport map @dnat_tcp
//	    fib daddr type local meta l4proto udp jump dnat_xlat_udp
//	    fib daddr type local meta l4proto udp dnat ip to udp dport map @dnat_udp
//	  }
//	  chain postrouting {
//	    type nat hook postrouting priority srcnat;
//	    iifname != "eth0" ct status dnat masquerade
//	    oifname "eth0" masquerade
//	  }
//	}
package nat

import (
	"fmt"
	"net"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// Family identifies an address family.
type Family int

const (
	V4 Family = iota
	V6
)

func (f Family) String() string {
	if f == V6 {
		return "v6"
	}
	return "v4"
}

// MapKey identifies a single DNAT entry.
type MapKey struct {
	Family Family
	Proto  string // "tcp" | "udp"
	Port   uint16
}

// Target is the DNAT destination — backend IP plus backend port.
// F-46: Port may differ from the DMZ-side port when the operator
// declares a port translation via `anchord.expose=tcp/636:6636`.
// When no translation is requested, Port equals the DMZ-side port
// and the rule behaves identically to pre-F-46 DNAT.
type Target struct {
	IP   net.IP
	Port uint16
}

// Manager owns the nftables state for one anchord instance.
type Manager struct {
	mu       sync.Mutex
	extIface string

	// v6FibSupported records whether the running kernel accepts the
	// nft `fib daddr type local` expression for ip6 tables. Probed
	// once at Setup() — production kernels (TrueNAS SCALE,
	// stock Debian/Ubuntu, …) all have `nft_fib_ipv6` loaded, but
	// WSL2's stripped-down kernel does not. When false, v6
	// prerouting falls back to the legacy `iifname == extIface`
	// predicate (F-47 hairpin still works for v4, just not v6).
	v6FibSupported bool

	// Cached references after Setup so updates are O(1).
	conn    *nftables.Conn
	tableV4 *nftables.Table
	tableV6 *nftables.Table
	// Address maps — backend IP keyed by DMZ port. The fast path
	// for non-translating entries.
	mapV4TCP, mapV4UDP, mapV6TCP, mapV6UDP *nftables.Set
	// F-46 translation sub-chains — flushed and repopulated by each
	// SetMap call with literal-DNAT rules for entries whose backend
	// port differs from the DMZ port. Empty (no rules) for stacks
	// that don't use port translation, in which case prerouting
	// jumps in and immediately returns.
	xlatV4TCP, xlatV4UDP, xlatV6TCP, xlatV6UDP *nftables.Chain
}

// New returns an unconfigured Manager. Call Setup to install the base
// tables and chains.
func New(extIface string) *Manager {
	return &Manager{extIface: extIface}
}

// Setup creates (or replaces) the anchord tables, chains and maps.
// Idempotent — safe to call on every start.
func (m *Manager) Setup() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Probe whether the kernel supports v6 fib expressions before we
	// commit to a layout. Production kernels do; WSL2 doesn't. The
	// probe is its own table+flush, isolated from anchord's state.
	m.v6FibSupported = probeV6FibSupported()

	c := &nftables.Conn{}

	// Wipe any prior state so we start from a known baseline. Anything
	// not in the anchord_* tables is left alone.
	for _, name := range []string{"anchord_v4", "anchord_v6"} {
		c.DelTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: name})
		c.DelTable(&nftables.Table{Family: nftables.TableFamilyIPv6, Name: name})
	}
	if err := c.Flush(); err != nil {
		// Tables may not exist on first run — that's fine.
	}
	c = &nftables.Conn{}

	m.tableV4 = c.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4, Name: "anchord_v4",
	})
	m.tableV6 = c.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv6, Name: "anchord_v6",
	})

	// Address map: DMZ port → backend address. One per (family, proto).
	mkAddrMap := func(t *nftables.Table, name string, fam Family) *nftables.Set {
		dataType := nftables.TypeIPAddr
		if fam == V6 {
			dataType = nftables.TypeIP6Addr
		}
		s := &nftables.Set{
			Table:    t,
			Name:     name,
			IsMap:    true,
			KeyType:  nftables.TypeInetService,
			DataType: dataType,
		}
		if err := c.AddSet(s, nil); err != nil {
			// AddSet errors are deferred until Flush; signature
			// requires checking. Real failures surface on Flush.
			_ = err
		}
		return s
	}
	m.mapV4TCP = mkAddrMap(m.tableV4, "dnat_tcp", V4)
	m.mapV4UDP = mkAddrMap(m.tableV4, "dnat_udp", V4)
	m.mapV6TCP = mkAddrMap(m.tableV6, "dnat_tcp", V6)
	m.mapV6UDP = mkAddrMap(m.tableV6, "dnat_udp", V6)

	// F-46 translation sub-chains. Regular (non-base) chains: they
	// have no hook and are reached only via a jump from prerouting.
	// Initially empty — SetMap populates them with literal DNAT
	// rules for translating entries. installChains adds the jump
	// rules that route to each chain ahead of the map-lookup rule.
	mkXlat := func(t *nftables.Table, name string) *nftables.Chain {
		return c.AddChain(&nftables.Chain{Table: t, Name: name})
	}
	m.xlatV4TCP = mkXlat(m.tableV4, "dnat_xlat_tcp")
	m.xlatV4UDP = mkXlat(m.tableV4, "dnat_xlat_udp")
	m.xlatV6TCP = mkXlat(m.tableV6, "dnat_xlat_tcp")
	m.xlatV6UDP = mkXlat(m.tableV6, "dnat_xlat_udp")

	// Pre/postrouting chains, one pair per family.
	m.installChains(c, m.tableV4, V4)
	m.installChains(c, m.tableV6, V6)

	if err := c.Flush(); err != nil {
		return fmt.Errorf("nft setup flush: %w", err)
	}
	m.conn = c
	return nil
}

// installChains creates prerouting (DNAT-from-map) and postrouting
// (masquerade) chains for the given family and binds DNAT rules that
// consult the named maps.
func (m *Manager) installChains(c *nftables.Conn, t *nftables.Table, fam Family) {
	pre := c.AddChain(&nftables.Chain{
		Name:     "prerouting",
		Table:    t,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
	})
	post := c.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    t,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})

	// Per protocol: jump to translation sub-chain first (literal
	// DNAT rules for F-46 port-translating entries), then fall
	// through to the map-based DNAT rule (catch-all for
	// non-translating entries).
	//
	// F-47 / issue #11: v4 prerouting guards on `fib daddr type
	// local` so DNAT fires for any packet whose destination is one
	// of the anchor's own addresses — that covers LAN-ingress AND
	// the bridge-sibling hairpin path. Forwarded-to-external traffic
	// (non-local dst) falls through. v6 keeps the legacy iifname
	// predicate because `nft_fib_ipv6` isn't always present on
	// stripped-down kernels (e.g. WSL2). The reported hairpin
	// scenarios are all v4; refactoring v6 once the inet-family
	// consolidation lands (or once we add a runtime fib probe) is
	// tracked separately.
	addXlatJump(c, pre, m.extIface, fam, m.v6FibSupported, unix.IPPROTO_TCP, m.xlatChainForFamProto(fam, "tcp"))
	addDNATRule(c, pre, m.extIface, fam, m.v6FibSupported, unix.IPPROTO_TCP, m.mapForFamProto(fam, "tcp"))
	addXlatJump(c, pre, m.extIface, fam, m.v6FibSupported, unix.IPPROTO_UDP, m.xlatChainForFamProto(fam, "udp"))
	addDNATRule(c, pre, m.extIface, fam, m.v6FibSupported, unix.IPPROTO_UDP, m.mapForFamProto(fam, "udp"))

	// F-47 hairpin SNAT: a sibling on a bridge talking to the anchor's
	// public IP gets DNAT'd, but the backend would reply directly via
	// L2 (bypassing the anchor) and the sibling would reject the
	// unexpected 4-tuple. SNAT here so the backend replies via the
	// anchor and conntrack reverses both translations.
	// `iifname != extIface` excludes the LAN-ingress path so F-9
	// (client source IP preservation) still holds for external clients.
	// Works for both families — Meta/Ct/Bitwise don't need nft_fib.
	addHairpinSnatRule(c, post, m.extIface)

	// Masquerade outbound on the external interface — auto-tracks the
	// current DHCP-assigned source address, so we don't need to update
	// anything when the lease rotates.
	addMasqueradeRule(c, post, m.extIface)
}

func (m *Manager) mapForFamProto(fam Family, proto string) *nftables.Set {
	switch {
	case fam == V4 && proto == "tcp":
		return m.mapV4TCP
	case fam == V4 && proto == "udp":
		return m.mapV4UDP
	case fam == V6 && proto == "tcp":
		return m.mapV6TCP
	case fam == V6 && proto == "udp":
		return m.mapV6UDP
	}
	return nil
}

// xlatChainForFamProto returns the F-46 translation sub-chain for
// the given family + protocol. SetMap flushes and repopulates this
// chain with literal-DNAT rules for any entries whose backend port
// differs from the DMZ port.
func (m *Manager) xlatChainForFamProto(fam Family, proto string) *nftables.Chain {
	switch {
	case fam == V4 && proto == "tcp":
		return m.xlatV4TCP
	case fam == V4 && proto == "udp":
		return m.xlatV4UDP
	case fam == V6 && proto == "tcp":
		return m.xlatV6TCP
	case fam == V6 && proto == "udp":
		return m.xlatV6UDP
	}
	return nil
}

// probeV6FibSupported returns true iff the running kernel accepts
// `fib daddr type local` in an ip6 NAT chain. Production kernels
// (TrueNAS SCALE, stock Debian/Ubuntu, Alpine on real Linux hosts)
// have `nft_fib_ipv6` loaded; WSL2's stripped-down kernel does not,
// so the test runner in CI/dev falls through to the iifname-based
// guard for v6.
//
// The probe builds a throwaway table with a single fib-using rule,
// commits it, observes the result, and best-effort cleans up. It
// does NOT touch any anchord_* state, so a failed probe leaves no
// residue. The cleanup happens unconditionally — if create-and-flush
// succeeded, delete the table; if it failed, no table exists to
// delete, the second flush is a no-op error we ignore.
func probeV6FibSupported() bool {
	const probeName = "anchord_v6fib_probe"

	c := &nftables.Conn{}
	tbl := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv6, Name: probeName})
	ch := c.AddChain(&nftables.Chain{
		Name: "pre", Table: tbl,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
	})
	c.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: []expr.Any{
		&expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})
	supported := c.Flush() == nil

	// Best-effort cleanup. If the probe failed mid-flight the table
	// may not exist; DelTable+Flush of a non-existent table errors
	// silently — that's fine.
	cleanup := &nftables.Conn{}
	cleanup.DelTable(&nftables.Table{Family: nftables.TableFamilyIPv6, Name: probeName})
	_ = cleanup.Flush()

	return supported
}

// preroutingGuardExprs returns the two-expression guard that scopes
// each DNAT rule. v4 always uses `fib daddr type local` (F-47 /
// issue #11) so DNAT fires for any destination the anchor owns —
// that covers both LAN ingress and bridge-sibling hairpin.
//
// v6 uses fib when the kernel supports it (probeV6FibSupported in
// Setup), else falls back to `iifname == extIface`. v6 hairpin is
// available on production kernels; on WSL2/CI the fallback loses
// hairpin for v6 but keeps LAN-ingress DNAT working.
//
// For both families fib writes a uint32 into the destination
// register for NFT_FIB_RESULT_ADDRTYPE, so the Cmp data is a 4-byte
// native-endian value of unix.RTN_LOCAL.
func preroutingGuardExprs(iface string, fam Family, v6FibSupported bool) []expr.Any {
	useFib := fam == V4 || v6FibSupported
	if !useFib {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifaceBytes(iface)},
		}
	}
	return []expr.Any{
		&expr.Fib{
			Register:       1,
			FlagDADDR:      true,
			ResultADDRTYPE: true,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.NativeEndian.PutUint32(uint32(unix.RTN_LOCAL)),
		},
	}
}

// addXlatJump installs the prerouting → dnat_xlat_* jump rule:
//
//	<guard> meta l4proto P jump dnat_xlat_P
//
// where `<guard>` is `fib daddr type local` for v4 (and for v6 when
// the kernel supports it), else `iifname == extIface` for v6 (see
// preroutingGuardExprs).
//
// The jump fires before the map-lookup rule. If the sub-chain has a
// matching literal-DNAT entry it short-circuits the rest of
// prerouting (DNAT is a terminal verdict); otherwise the jump
// returns and the map rule runs.
func addXlatJump(c *nftables.Conn, from *nftables.Chain, iface string, fam Family, v6FibSupported bool, proto byte, to *nftables.Chain) {
	exprs := append(preroutingGuardExprs(iface, fam, v6FibSupported),
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Verdict{Kind: expr.VerdictJump, Chain: to.Name},
	)
	c.AddRule(&nftables.Rule{Table: from.Table, Chain: from, Exprs: exprs})
}

// addDNATRule installs the catch-all map-lookup rule:
//
//	<guard> meta l4proto P dnat to tcp dport map @MAP
//
// F-46 port translation lives in the dnat_xlat_* sub-chain (see
// addXlatJump) which runs ahead of this rule and short-circuits
// matching ports with literal DNAT.
func addDNATRule(c *nftables.Conn, ch *nftables.Chain, iface string, fam Family, v6FibSupported bool, proto byte, set *nftables.Set) {
	exprs := append(preroutingGuardExprs(iface, fam, v6FibSupported),
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Lookup{
			SourceRegister: 1,
			DestRegister:   2,
			IsDestRegSet:   true,
			SetName:        set.Name,
			SetID:          set.ID,
		},
		&expr.NAT{
			Type:       expr.NATTypeDestNAT,
			Family:     uint32(addressFamily(fam)),
			RegAddrMin: 2,
		},
	)
	c.AddRule(&nftables.Rule{Table: ch.Table, Chain: ch, Exprs: exprs})
}

// addHairpinSnatRule installs the F-47 hairpin SNAT rule:
//
//	iifname != EXT ct status dnat masquerade
//
// Fires only for packets that (a) entered via a bridge, not the
// macvlan, and (b) have already been DNAT'd by prerouting. The
// effect is that a sibling-on-a-bridge hairpin gets its source IP
// rewritten to the anchor's bridge IP so the backend's reply comes
// back via the anchor instead of directly over L2 to the sibling
// (which would otherwise drop the reply on 4-tuple mismatch).
// External LAN clients (iif == EXT) are excluded so F-9 still holds.
func addHairpinSnatRule(c *nftables.Conn, ch *nftables.Chain, iface string) {
	// ct status & IPS_DST_NAT != 0 — bit 5 of the ct status word
	// is set whenever prerouting installed a DNAT translation.
	const ipsDstNat uint32 = 1 << 5
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ifaceBytes(iface)},
		&expr.Ct{Key: expr.CtKeySTATUS, Register: 1},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(ipsDstNat),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 1,
			Data:     binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Masq{},
	}
	c.AddRule(&nftables.Rule{Table: ch.Table, Chain: ch, Exprs: exprs})
}

// addMasqueradeRule installs:
//
//	oifname EXT masquerade
func addMasqueradeRule(c *nftables.Conn, ch *nftables.Chain, iface string) {
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifaceBytes(iface),
		},
		&expr.Masq{},
	}
	c.AddRule(&nftables.Rule{Table: ch.Table, Chain: ch, Exprs: exprs})
}

func addressFamily(f Family) uint32 {
	if f == V6 {
		return uint32(nftables.TableFamilyIPv6)
	}
	return uint32(nftables.TableFamilyIPv4)
}

// ifaceBytes returns a NUL-padded 16-byte interface name as nftables
// expects for IIFNAME/OIFNAME comparisons.
func ifaceBytes(name string) []byte {
	b := make([]byte, 16)
	copy(b, []byte(name))
	return b
}

// SetMap replaces the DNAT state for one (family, proto) atomically.
//
// Non-translating entries (target.Port == DMZ key port) populate the
// address map; the catch-all map-lookup rule handles them.
// Translating entries become per-entry literal-DNAT rules in the
// dnat_xlat_* sub-chain. The address map is FlushSet+SetAddElements
// and the sub-chain is FlushChain+AddRule, all in one netlink batch
// so the data plane never sees a half-applied state.
//
// F-46: when no operator label requests translation, the sub-chain
// stays empty (each SetMap clears it; the catch-all map rule does
// all the work). The jump rule into an empty sub-chain is a no-op
// — same fast path as pre-F-46.
func (m *Manager) SetMap(family Family, proto string, entries map[uint16]Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	addrSet := m.mapForFamProto(family, proto)
	xlatCh := m.xlatChainForFamProto(family, proto)
	if addrSet == nil || xlatCh == nil {
		return fmt.Errorf("no such map: %s/%s", family, proto)
	}

	c := &nftables.Conn{}

	// Clear both the address map and the translation sub-chain.
	c.FlushSet(addrSet)
	c.FlushChain(xlatCh)

	addrElems := make([]nftables.SetElement, 0, len(entries))
	for port, target := range entries {
		if target.Port == 0 {
			return fmt.Errorf("port %d: target port is 0 (reserved)", port)
		}
		ipBytes := target.IP.To4()
		if family == V6 {
			if target.IP.To4() != nil {
				return fmt.Errorf("port %d: ip %s is v4 but family is v6", port, target.IP)
			}
			ipBytes = target.IP.To16()
		}
		if ipBytes == nil {
			return fmt.Errorf("port %d: ip %s does not fit %s family", port, target.IP, family)
		}

		if target.Port == port {
			// Non-translating: fast path via the map.
			addrElems = append(addrElems, nftables.SetElement{
				Key: binaryutil.BigEndian.PutUint16(port),
				Val: ipBytes,
			})
			continue
		}

		// Translating: literal DNAT rule in the sub-chain. The jump
		// rule in prerouting already matched iif+proto, so this
		// chain only needs to match dport.
		c.AddRule(&nftables.Rule{
			Table: xlatCh.Table,
			Chain: xlatCh,
			Exprs: xlatRuleExprs(family, port, ipBytes, target.Port),
		})
	}
	if err := c.SetAddElements(addrSet, addrElems); err != nil {
		return fmt.Errorf("addr SetAddElements: %w", err)
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nft map flush: %w", err)
	}
	return nil
}

// xlatRuleExprs builds the F-46 translation sub-chain rule:
//
//	tcp dport DMZ_PORT dnat to BACKEND_IP:BACKEND_PORT
//
// Address and port are loaded into registers via Immediate exprs
// (literal values, not map lookups — that's what tripped the kernel
// in the abandoned two-Lookup variant).
//
// Register layout: nftables exposes registers in two views — legacy
// regs 1..4 are 16-byte slots, modern regs 8..23 are 4-byte slots
// aliased over the same byte array. We use legacy registers here:
//
//   - reg 2 holds the backend address. For v4 the address is 4 bytes
//     (the upper 12 bytes of the slot are don't-care, NAT reads only
//     the family-specific length); for v6 the address fills the full
//     16-byte slot.
//   - reg 3 holds the backend port (2 bytes inside a 16-byte slot).
//
// This works for *both* families because the v6 address in reg 2
// occupies only that slot, never spilling into reg 3 — kernel-side
// legacy regs are non-overlapping 16-byte windows. Issue #1: an
// earlier version used `reg 6` for the v6 port, but raw reg 6 isn't
// in the legacy range, so the kernel routed it through the modern
// reg space and landed inside the verdict register window, returning
// ERANGE on netlink commit.
func xlatRuleExprs(fam Family, dmzPort uint16, ipBytes []byte, backendPort uint16) []expr.Any {
	const (
		regAddr  uint32 = 2
		regProto uint32 = 3
	)
	return []expr.Any{
		// Match dport.
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(dmzPort),
		},
		// Load backend address as a literal.
		&expr.Immediate{Register: regAddr, Data: ipBytes},
		// Load backend port as a literal.
		&expr.Immediate{Register: regProto, Data: binaryutil.BigEndian.PutUint16(backendPort)},
		// DNAT to (looked-up address, looked-up port).
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      uint32(addressFamily(fam)),
			RegAddrMin:  regAddr,
			RegProtoMin: regProto,
		},
	}
}

// Teardown removes all anchord tables. Used on graceful shutdown.
func (m *Manager) Teardown() error {
	c := &nftables.Conn{}
	if m.tableV4 != nil {
		c.DelTable(m.tableV4)
	}
	if m.tableV6 != nil {
		c.DelTable(m.tableV6)
	}
	return c.Flush()
}
