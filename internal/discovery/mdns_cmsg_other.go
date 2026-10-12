//go:build !windows

package discovery

import (
	"errors"
	"net"
	"net/netip"
)

// platformReader is the fallback for platforms where golang.org/x/net
// cannot report IP control messages; only Windows has one (see
// mdns_cmsg_windows.go).
func platformReader(net.PacketConn, bool) (func([]byte) (int, netip.AddrPort, mdnsMeta, error), error) {
	return nil, errors.New("IP control messages are not supported on this platform")
}
