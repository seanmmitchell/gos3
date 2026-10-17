package discovery

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// mdnsSocket is one UDP socket used for mDNS: either bound to port 5353
// and joined to the mDNS group of its family on every multicast interface
// (responder, browser), or an ephemeral socket sending queries (browser).
// Every datagram is sent with IP TTL / hop limit 255 (RFC 6762 §11), and
// the arrival interface and TTL / hop limit of received datagrams are
// reported: through golang.org/x/net where it implements control messages,
// with WSARecvMsg on Windows (where it does not). When neither works
// (cmsg false), both are reported as 0 (unknown).
type mdnsSocket struct {
	v6     bool
	conn   net.PacketConn
	p4     *ipv4.PacketConn // !v6
	p6     *ipv6.PacketConn // v6
	ifaces []net.Interface  // multicast interfaces of this family

	// cmsg: received datagrams report their arrival interface and TTL /
	// hop limit.
	cmsg bool
	// xcm: golang.org/x/net handles control messages, including the
	// outgoing interface of a send.
	xcm bool
	// recv reads a datagram with control messages when !xcm (Windows).
	recv func([]byte) (int, netip.AddrPort, mdnsMeta, error)
	// ifMu serialises switching the multicast interface for a send when
	// !xcm.
	ifMu sync.Mutex
}

// mdnsNoTTLWarned makes the missing TTL check be logged once per process.
var mdnsNoTTLWarned atomic.Bool

func newMDNSSocket(conn net.PacketConn, v6 bool, ifaces []net.Interface, logger *slog.Logger) *mdnsSocket {
	s := &mdnsSocket{v6: v6, conn: conn, ifaces: ifaces}
	var cmErr error
	var errs []error
	if v6 {
		s.p6 = ipv6.NewPacketConn(conn)
		cmErr = s.p6.SetControlMessage(ipv6.FlagInterface|ipv6.FlagHopLimit, true)
		errs = append(errs,
			s.p6.SetMulticastHopLimit(255),
			s.p6.SetHopLimit(255),
			s.p6.SetMulticastLoopback(true))
	} else {
		s.p4 = ipv4.NewPacketConn(conn)
		cmErr = s.p4.SetControlMessage(ipv4.FlagInterface|ipv4.FlagTTL, true)
		errs = append(errs,
			s.p4.SetMulticastTTL(255),
			s.p4.SetTTL(255),
			s.p4.SetMulticastLoopback(true))
	}
	if err := errors.Join(errs...); err != nil {
		logger.Debug("mDNS: socket options", "err", err)
	}
	switch {
	case cmErr == nil:
		s.cmsg, s.xcm = true, true
	default:
		recv, err := platformReader(conn, v6)
		if err == nil {
			s.recv, s.cmsg = recv, true
			break
		}
		if mdnsNoTTLWarned.CompareAndSwap(false, true) {
			logger.Warn("mDNS: the arrival TTL of datagrams cannot be read on this platform; "+
				"answering other hosts by multicast only", "err", errors.Join(cmErr, err))
		}
	}
	return s
}

// Close closes the socket.
func (s *mdnsSocket) Close() error { return s.conn.Close() }

// mdnsMeta is what the IP layer reports about a received datagram.
type mdnsMeta struct {
	ifIndex int // arrival interface, 0 when unknown
	ttl     int // IP TTL / IPv6 hop limit, 0 when unknown
}

// readFrom reads one datagram: its length, source (with the IPv6 zone),
// arrival interface and TTL / hop limit.
func (s *mdnsSocket) readFrom(buf []byte) (int, netip.AddrPort, mdnsMeta, error) {
	if s.recv != nil {
		return s.recv(buf)
	}
	var (
		n    int
		src  net.Addr
		meta mdnsMeta
		err  error
	)
	if s.v6 {
		var cm *ipv6.ControlMessage
		n, cm, src, err = s.p6.ReadFrom(buf)
		if cm != nil {
			meta = mdnsMeta{ifIndex: cm.IfIndex, ttl: cm.HopLimit}
		}
	} else {
		var cm *ipv4.ControlMessage
		n, cm, src, err = s.p4.ReadFrom(buf)
		if cm != nil {
			meta = mdnsMeta{ifIndex: cm.IfIndex, ttl: cm.TTL}
		}
	}
	if err != nil {
		return n, netip.AddrPort{}, mdnsMeta{}, err
	}
	ua, ok := src.(*net.UDPAddr)
	if !ok {
		return n, netip.AddrPort{}, meta, nil
	}
	ap := ua.AddrPort()
	return n, netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), meta, nil
}

// writeTo sends b to dst out of interface ifIndex (0: the system's choice).
func (s *mdnsSocket) writeTo(b []byte, dst netip.AddrPort, ifIndex int) error {
	ua := net.UDPAddrFromAddrPort(dst)
	if !s.xcm && ifIndex > 0 && dst.Addr().IsMulticast() {
		return s.writeMulticastVia(b, ua, ifIndex)
	}
	var err error
	if s.v6 {
		var cm *ipv6.ControlMessage
		if ifIndex > 0 {
			cm = &ipv6.ControlMessage{IfIndex: ifIndex}
		}
		_, err = s.p6.WriteTo(b, cm, ua)
	} else {
		var cm *ipv4.ControlMessage
		if ifIndex > 0 {
			cm = &ipv4.ControlMessage{IfIndex: ifIndex}
		}
		_, err = s.p4.WriteTo(b, cm, ua)
	}
	return err
}

// writeMulticastVia sends b to the group address ua out of interface
// ifIndex by switching the socket's multicast interface, for platforms
// where a send cannot carry the interface in a control message.
func (s *mdnsSocket) writeMulticastVia(b []byte, ua *net.UDPAddr, ifIndex int) error {
	var ifi *net.Interface
	for i := range s.ifaces {
		if s.ifaces[i].Index == ifIndex {
			ifi = &s.ifaces[i]
			break
		}
	}
	if ifi == nil {
		var err error
		if ifi, err = net.InterfaceByIndex(ifIndex); err != nil {
			return err
		}
	}
	s.ifMu.Lock()
	defer s.ifMu.Unlock()
	if s.v6 {
		if err := s.p6.SetMulticastInterface(ifi); err != nil {
			return err
		}
	} else if err := s.p4.SetMulticastInterface(ifi); err != nil {
		return err
	}
	_, err := s.conn.WriteTo(b, ua)
	return err
}

func (s *mdnsSocket) group() netip.AddrPort {
	if s.v6 {
		return mdnsGroup6
	}
	return mdnsGroup4
}

// multicast implements mdnsWriter: send b to the mDNS group of the socket's
// family out of interface ifIndex (the query's arrival interface; 0 when
// unknown: the system's default multicast interface).
func (s *mdnsSocket) multicast(b []byte, ifIndex int) error {
	return s.writeTo(b, s.group(), ifIndex)
}

// multicastAll sends b to the mDNS group out of every multicast interface
// (the system's default when none is known). It fails only when every
// send fails.
func (s *mdnsSocket) multicastAll(b []byte) error {
	if len(s.ifaces) == 0 {
		return s.writeTo(b, s.group(), 0)
	}
	var errs []error
	for _, ifi := range s.ifaces {
		if err := s.writeTo(b, s.group(), ifi.Index); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == len(s.ifaces) {
		return errors.Join(errs...)
	}
	return nil
}

// unicast implements mdnsWriter.
func (s *mdnsSocket) unicast(b []byte, to netip.AddrPort, ifIndex int) error {
	return s.writeTo(b, to, ifIndex)
}

// listenMDNSGroup binds the mDNS port of the family and joins the group on
// every multicast interface of that family (hashicorp/mdns, which gos3
// used before, joined the same groups on the default interface only).
func listenMDNSGroup(v6 bool, logger *slog.Logger) (*mdnsSocket, error) {
	network, group := "udp4", net.UDPAddrFromAddrPort(mdnsGroup4)
	if v6 {
		network, group = "udp6", net.UDPAddrFromAddrPort(mdnsGroup6)
	}
	ifaces := multicastInterfaces(v6)
	c, err := net.ListenMulticastUDP(network, nil, group)
	if err != nil {
		firstErr := err
		for i := range ifaces {
			if c, err = net.ListenMulticastUDP(network, &ifaces[i], group); err == nil {
				break
			}
		}
		if c == nil {
			return nil, firstErr
		}
	}
	s := newMDNSSocket(c, v6, ifaces, logger)
	joined := 0
	for i := range ifaces {
		var err error
		if v6 {
			err = s.p6.JoinGroup(&ifaces[i], group)
		} else {
			err = s.p4.JoinGroup(&ifaces[i], group)
		}
		if err == nil {
			joined++
		}
	}
	logger.Debug("mDNS: joined group", "group", group.String(), "interfaces", joined)
	return s, nil
}

// openMDNSResponderSockets opens the IPv4 and IPv6 group sockets the
// responder serves; it fails only when neither can be opened.
func openMDNSResponderSockets(logger *slog.Logger) ([]*mdnsSocket, error) {
	var socks []*mdnsSocket
	var errs []error
	for _, v6 := range []bool{false, true} {
		s, err := listenMDNSGroup(v6, logger)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		socks = append(socks, s)
	}
	if len(socks) == 0 {
		return nil, fmt.Errorf("no mDNS multicast listener could be started: %w", errors.Join(errs...))
	}
	return socks, nil
}

// listenMDNSQuerier opens an ephemeral socket of the family for sending
// queries and receiving the (legacy unicast) answers.
func listenMDNSQuerier(v6 bool, logger *slog.Logger) (*mdnsSocket, error) {
	network, ip := "udp4", net.IPv4zero
	if v6 {
		network, ip = "udp6", net.IPv6unspecified
	}
	c, err := net.ListenUDP(network, &net.UDPAddr{IP: ip})
	if err != nil {
		return nil, err
	}
	return newMDNSSocket(c, v6, multicastInterfaces(v6), logger), nil
}
