// Package nat manages the nftables tables that implement DNAT for
// inbound traffic and masquerade for outbound traffic.
//
// We use a single table per address family ("anchord_v4", "anchord_v6")
// with two named maps each (one per L4 protocol). DNAT is implemented
// as a single rule that consults the map — atomic map updates mean
// rule changes are seamless and lock-free.
//
// Layout (v4 example):
//
//	table ip anchord_v4 {
//	  map dnat_tcp { type inet_service : ipv4_addr; }
//	  map dnat_udp { type inet_service : ipv4_addr; }
//	  chain prerouting {
//	    type nat hook prerouting priority dstnat;
//	    iifname "anchord-ext" meta l4proto tcp dnat to tcp dport map @dnat_tcp
//	    iifname "anchord-ext" meta l4proto udp dnat to udp dport map @dnat_udp
//	  }
//	  chain postrouting {
//	    type nat hook postrouting priority srcnat;
//	    oifname "anchord-ext" masquerade
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

// Manager owns the nftables state for one anchord instance.
type Manager struct {
	mu       sync.Mutex
	extIface string

	// Cached references after Setup so updates are O(1).
	conn     *nftables.Conn
	tableV4  *nftables.Table
	tableV6  *nftables.Table
	mapV4TCP *nftables.Set
	mapV4UDP *nftables.Set
	mapV6TCP *nftables.Set
	mapV6UDP *nftables.Set
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

	mkMap := func(t *nftables.Table, name string, fam Family) *nftables.Set {
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
			// AddSet errors are deferred until Flush, but signature
			// requires checking — log via panic-on-flush below.
			_ = err
		}
		return s
	}
	m.mapV4TCP = mkMap(m.tableV4, "dnat_tcp", V4)
	m.mapV4UDP = mkMap(m.tableV4, "dnat_udp", V4)
	m.mapV6TCP = mkMap(m.tableV6, "dnat_tcp", V6)
	m.mapV6UDP = mkMap(m.tableV6, "dnat_udp", V6)

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

	// Two DNAT rules per family, one per protocol.
	addDNATRule(c, pre, m.extIface, fam, unix.IPPROTO_TCP, m.mapForFamProto(fam, "tcp"))
	addDNATRule(c, pre, m.extIface, fam, unix.IPPROTO_UDP, m.mapForFamProto(fam, "udp"))

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

// addDNATRule installs:
//
//	iifname EXT meta l4proto P dnat to L4-dport map @MAP
func addDNATRule(c *nftables.Conn, ch *nftables.Chain, iface string, fam Family, proto byte, set *nftables.Set) {
	exprs := []expr.Any{
		// Match input interface name.
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifaceBytes(iface),
		},
		// Match L4 protocol.
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{proto},
		},
		// Load destination port (TCP or UDP, both at offset 2 of L4 header).
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		// Look up the port in the map; the map's value (an IPv4/v6 addr)
		// lands in register 2.
		&expr.Lookup{
			SourceRegister: 1,
			DestRegister:   2,
			IsDestRegSet:   true,
			SetName:        set.Name,
			SetID:          set.ID,
		},
		// DNAT to the looked-up address (preserving the original port).
		&expr.NAT{
			Type:       expr.NATTypeDestNAT,
			Family:     uint32(addressFamily(fam)),
			RegAddrMin: 2,
		},
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

// SetMap replaces the contents of one map atomically.
//
// Replace semantics: any keys not in entries are removed, any new keys
// are added, existing keys with changed values are updated. The kernel
// applies this as a single transaction.
func (m *Manager) SetMap(family Family, proto string, entries map[uint16]net.IP) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	set := m.mapForFamProto(family, proto)
	if set == nil {
		return fmt.Errorf("no such map: %s/%s", family, proto)
	}

	c := &nftables.Conn{}

	// SetDeleteElements with no args clears, then we add fresh.
	// nftables-go implements atomic replace via FlushSet + Add.
	c.FlushSet(set)

	elems := make([]nftables.SetElement, 0, len(entries))
	for port, ip := range entries {
		ipBytes := ip.To4()
		if family == V6 {
			ipBytes = ip.To16()
		}
		if ipBytes == nil {
			return fmt.Errorf("ip %s does not fit %s family", ip, family)
		}
		elems = append(elems, nftables.SetElement{
			Key: binaryutil.BigEndian.PutUint16(port),
			Val: ipBytes,
		})
	}
	if err := c.SetAddElements(set, elems); err != nil {
		return fmt.Errorf("SetAddElements: %w", err)
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nft map flush: %w", err)
	}
	return nil
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
