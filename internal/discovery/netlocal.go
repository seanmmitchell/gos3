package discovery

import (
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
// When the interfaces cannot be listed every address is accepted.
func (l *localNets) onLink(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	prefixes := l.get()
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// isBroadcast reports whether ip is the IPv4 limited broadcast address or
// the directed broadcast address of a directly connected subnet (prefixes
// up to /30). Such a source address is spoofed: replying to it would reach
// every host of the link.
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
		a := p.Masked().Addr().As4()
		host := uint32(1)<<(32-p.Bits()) - 1
		n := (uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])) | host
		if ip == netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}) {
			return true
		}
	}
	return false
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
