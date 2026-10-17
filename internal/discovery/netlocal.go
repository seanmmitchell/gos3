package discovery

import (
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

// localNetsTTL is how long the interface address list is cached.
const localNetsTTL = 10 * time.Second

// localNets answers whether an address belongs to this host or to one of its
// directly connected subnets. Beacon datagrams are link-local by design
// (multicast TTL 1), so datagrams from elsewhere are not trusted. It is safe
// for concurrent use.
type localNets struct {
	load func() []netip.Prefix // interface prefixes; nil = the host's interfaces

	mu       sync.Mutex
	loadedAt time.Time
	prefixes []netip.Prefix
	warned   bool // the empty list was logged
}

// hostNets is the shared cache of this host's interface prefixes.
var hostNets = &localNets{}

func (l *localNets) get() []netip.Prefix {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadedAt.IsZero() || time.Since(l.loadedAt) > localNetsTTL {
		load := l.load
		if load == nil {
			load = interfacePrefixes
		}
		l.prefixes, l.loadedAt = load(), time.Now()
		switch {
		case len(l.prefixes) == 0 && !l.warned:
			l.warned = true
			slog.Warn("discovery: network interfaces could not be listed; " +
				"ignoring beacon and mDNS datagrams from other hosts until they can")
		case len(l.prefixes) > 0:
			l.warned = false
		}
	}
	return l.prefixes
}

// isLocal reports whether ip is a loopback address or one of this host's
// interface addresses.
func (l *localNets) isLocal(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, p := range l.get() {
		if p.Addr() == ip {
			return true
		}
	}
	return false
}

// onLink reports whether ip is local or inside a directly connected subnet.
// When the interfaces cannot be listed only loopback addresses are
// accepted (fail closed): the subnets are unknown, and datagrams from
// anywhere could otherwise make the server probe or answer them.
func (l *localNets) onLink(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, p := range l.get() {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// isBroadcast reports whether ip is the IPv4 limited broadcast address or
// the directed broadcast or network address of a directly connected subnet
// (prefixes up to /30). Such a source address is spoofed: replying to it
// would reach every host of the link (some stacks treat the network
// address as a broadcast address too).
func (l *localNets) isBroadcast(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.Is4() {
		return false
	}
	if ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return true
	}
	for _, p := range l.get() {
		if !p.Addr().Is4() || p.Bits() > 30 || !p.Contains(ip) {
			continue
		}
		network := p.Masked().Addr()
		a := network.As4()
		host := uint32(1)<<(32-p.Bits()) - 1
		n := (uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])) | host
		if ip == network || ip == netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}) {
			return true
		}
	}
	return false
}

// routed reports whether a datagram from src that arrived with IP TTL /
// IPv6 hop limit ttl (0: unknown) was forwarded by a router. mDNS senders
// use 255 (RFC 6762 §11) and every router decrements it, so a lower value
// from another host means the datagram came from off the link and its
// on-link source address is spoofed. Datagrams from this host itself are
// never routed.
func (l *localNets) routed(src netip.Addr, ttl int) bool {
	return ttl > 0 && ttl < 255 && !l.isLocal(src.WithZone("").Unmap())
}

// advertisedBy reports whether a host at src may advertise ip as its own
// address: ip is src itself or, when src is this host (isLocal), a
// loopback or local address. Unauthenticated announcements (beacons, mDNS)
// therefore only ever make a server probe the sender.
func advertisedBy(ip, src netip.Addr, isLocal func(netip.Addr) bool) bool {
	ip, src = ip.Unmap().WithZone(""), src.Unmap().WithZone("")
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || !src.IsValid() {
		return false
	}
	return ip == src || (isLocal != nil && isLocal(src) && (ip.IsLoopback() || isLocal(ip)))
}

// interfacePrefixes returns the addresses (with their subnet) of every up
// interface.
func interfacePrefixes() []netip.Prefix {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, _ := ipn.Mask.Size()
			if ip.Is4() && ones > 32 {
				ones -= 96
			}
			if p, err := ip.Prefix(ones); err == nil {
				out = append(out, netip.PrefixFrom(ip, p.Bits()))
			}
		}
	}
	return out
}
