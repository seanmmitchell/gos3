//go:build windows

package discovery

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// golang.org/x/net does not implement IP control messages on Windows, so
// mDNS sockets read datagrams with WSARecvMsg themselves to learn the
// arrival interface and the TTL / hop limit (RFC 6762 §11 check).

// Socket options and control message types of ws2ipdef.h that
// golang.org/x/sys/windows does not define.
const (
	winIPRecvTTL    = 21 // IP_RECVTTL (control message type IP_TTL)
	winIPHopLimit   = 21 // IP_HOPLIMIT, the same value
	winIPv6HopLimit = 21 // IPV6_HOPLIMIT (option and control message type)
)

// cmsgAlign is the alignment of control message headers and data
// (WSA_CMSGHDR_ALIGN / WSA_CMSGDATA_ALIGN).
const cmsgAlign = int(unsafe.Sizeof(uintptr(0)))

func alignCmsg(n int) int { return (n + cmsgAlign - 1) &^ (cmsgAlign - 1) }

// platformReader enables the arrival interface and TTL / hop limit control
// messages on conn and returns a reader that receives them.
func platformReader(conn net.PacketConn, v6 bool) (func([]byte) (int, netip.AddrPort, mdnsMeta, error), error) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return nil, errors.New("not a socket")
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		h := windows.Handle(fd)
		if v6 {
			serr = errors.Join(
				windows.SetsockoptInt(h, windows.IPPROTO_IPV6, windows.IPV6_PKTINFO, 1),
				windows.SetsockoptInt(h, windows.IPPROTO_IPV6, winIPv6HopLimit, 1))
		} else {
			serr = errors.Join(
				windows.SetsockoptInt(h, windows.IPPROTO_IP, windows.IP_PKTINFO, 1),
				windows.SetsockoptInt(h, windows.IPPROTO_IP, winIPRecvTTL, 1))
		}
	}); err != nil {
		return nil, err
	}
	if serr != nil {
		return nil, serr
	}
	return func(buf []byte) (int, netip.AddrPort, mdnsMeta, error) {
		return recvMsg(rc, buf, v6)
	}, nil
}

// recvMsg reads one datagram with WSARecvMsg. The socket is only read once
// the runtime reports it readable (the first callback declines), so the
// synchronous WSARecvMsg never blocks a thread.
func recvMsg(rc syscall.RawConn, buf []byte, v6 bool) (int, netip.AddrPort, mdnsMeta, error) {
	if len(buf) == 0 {
		return 0, netip.AddrPort{}, mdnsMeta{}, errors.New("empty buffer")
	}
	var (
		from    syscall.RawSockaddrAny
		oob     [16]uint64 // 128 bytes, aligned
		n       uint32
		rerr    error
		waited  bool
		dataBuf = windows.WSABuf{Len: uint32(len(buf)), Buf: &buf[0]}
		msg     = windows.WSAMsg{
			Name:        &from,
			Namelen:     int32(unsafe.Sizeof(from)),
			Buffers:     &dataBuf,
			BufferCount: 1,
			Control:     windows.WSABuf{Len: uint32(unsafe.Sizeof(oob)), Buf: (*byte)(unsafe.Pointer(&oob[0]))},
		}
	)
	err := rc.Read(func(fd uintptr) bool {
		if !waited {
			waited = true
			return false
		}
		rerr = windows.WSARecvMsg(windows.Handle(fd), &msg, &n, nil, nil)
		return true
	})
	if err == nil {
		err = rerr
	}
	if errors.Is(err, windows.WSAEMSGSIZE) {
		// Truncated: report the full buffer; callers drop such datagrams.
		n, err = uint32(len(buf)), nil
	}
	if err != nil {
		return 0, netip.AddrPort{}, mdnsMeta{}, err
	}
	src := sockaddrAddrPort(&from)
	ctl := unsafe.Slice((*byte)(unsafe.Pointer(&oob[0])), min(int(msg.Control.Len), int(unsafe.Sizeof(oob))))
	return int(n), src, parseCmsgs(ctl, v6), nil
}

// sockaddrAddrPort converts a received source address.
func sockaddrAddrPort(sa *syscall.RawSockaddrAny) netip.AddrPort {
	switch sa.Addr.Family {
	case windows.AF_INET:
		in := (*syscall.RawSockaddrInet4)(unsafe.Pointer(sa))
		p := (*[2]byte)(unsafe.Pointer(&in.Port))
		return netip.AddrPortFrom(netip.AddrFrom4(in.Addr), uint16(p[0])<<8|uint16(p[1]))
	case windows.AF_INET6:
		in := (*syscall.RawSockaddrInet6)(unsafe.Pointer(sa))
		p := (*[2]byte)(unsafe.Pointer(&in.Port))
		ip := netip.AddrFrom16(in.Addr).Unmap()
		if ip.Is6() && in.Scope_id != 0 {
			ip = ip.WithZone(strconv.FormatUint(uint64(in.Scope_id), 10))
		}
		return netip.AddrPortFrom(ip, uint16(p[0])<<8|uint16(p[1]))
	}
	return netip.AddrPort{}
}

// parseCmsgs extracts the arrival interface (IP_PKTINFO / IPV6_PKTINFO)
// and the TTL / hop limit from received control messages.
func parseCmsgs(b []byte, v6 bool) mdnsMeta {
	var meta mdnsMeta
	hdrLen := int(unsafe.Sizeof(windows.WSACMSGHDR{}))
	dataOff := alignCmsg(hdrLen)
	for len(b) >= hdrLen {
		var h windows.WSACMSGHDR
		copy(unsafe.Slice((*byte)(unsafe.Pointer(&h)), hdrLen), b)
		l := int(h.Len)
		if l < dataOff || l > len(b) {
			break
		}
		data := b[dataOff:l]
		u32 := func(off int) (uint32, bool) {
			if len(data) < off+4 {
				return 0, false
			}
			return uint32(data[off]) | uint32(data[off+1])<<8 | uint32(data[off+2])<<16 | uint32(data[off+3])<<24, true
		}
		switch {
		case !v6 && h.Level == windows.IPPROTO_IP && h.Type == windows.IP_PKTINFO:
			// IN_PKTINFO: in_addr, then the interface index.
			if v, ok := u32(4); ok {
				meta.ifIndex = int(v)
			}
		case !v6 && h.Level == windows.IPPROTO_IP && (h.Type == windows.IP_TTL || h.Type == winIPHopLimit):
			if v, ok := u32(0); ok {
				meta.ttl = int(v)
			} else if len(data) > 0 {
				meta.ttl = int(data[0])
			}
		case v6 && h.Level == windows.IPPROTO_IPV6 && h.Type == windows.IPV6_PKTINFO:
			// IN6_PKTINFO: in6_addr, then the interface index.
			if v, ok := u32(16); ok {
				meta.ifIndex = int(v)
			}
		case v6 && h.Level == windows.IPPROTO_IPV6 && h.Type == winIPv6HopLimit:
			if v, ok := u32(0); ok {
				meta.ttl = int(v)
			}
		}
		next := alignCmsg(l)
		if next <= 0 || next > len(b) {
			break
		}
		b = b[next:]
	}
	return meta
}
