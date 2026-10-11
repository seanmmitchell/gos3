package discovery

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Browser limits (docs/discovery.md §2).
const (
	// mdnsMaxPerSource bounds the service instances taken from one sender.
	mdnsMaxPerSource = 16
	// mdnsMaxHostAddrs bounds the addresses kept per host name and sender.
	mdnsMaxHostAddrs = 16
	// mdnsMaxRecords bounds the records looked at per response.
	mdnsMaxRecords = 64
	// mdnsMaxFollowUps bounds the follow-up questions of one browse.
	mdnsMaxFollowUps = 32
)

// mdnsKey identifies a record owner name as reported by one sender: every
// sender's answers are kept apart, so records of one host never complete
// (or override) another's.
type mdnsKey struct {
	src  netip.Addr // without zone
	name string     // lower-cased owner name
}

// mdnsEntry is a service instance seen in the answers of one sender.
type mdnsEntry struct {
	key    mdnsKey
	name   string // instance FQDN as received
	host   string // lower-cased SRV target
	port   int
	txt    []string
	hasSRV bool
	hasTXT bool
	zone   string // IPv6 zone of the sender (for link-local addresses)
}

// mdnsBrowser turns mDNS responses into probe candidates. It accepts a
// response only from a sender on this host or a directly connected subnet,
// and an advertised address only when it is the sender's own address (or,
// for a sender on this host, a loopback or local address): the
// unauthenticated answers can only make the server probe the sender. It is
// not safe for concurrent use.
type mdnsBrowser struct {
	selfID    string
	local     *localNets
	entries   map[mdnsKey]*mdnsEntry
	order     []mdnsKey
	hosts     map[mdnsKey][]netip.Addr
	perSource map[netip.Addr]int
	asked     map[string]bool
}

func newMDNSBrowser(selfID string, local *localNets) *mdnsBrowser {
	return &mdnsBrowser{
		selfID:    selfID,
		local:     local,
		entries:   make(map[mdnsKey]*mdnsEntry),
		hosts:     make(map[mdnsKey][]netip.Addr),
		perSource: make(map[netip.Addr]int),
		asked:     make(map[string]bool),
	}
}

// instanceOf reports whether name is a service instance name of
// _gos3._tcp.local. ("<label>._gos3._tcp.local.").
func instanceOf(name string) bool {
	suffix := "." + strings.ToLower(mdnsServiceName)
	l := strings.ToLower(name)
	return len(l) > len(suffix) && strings.HasSuffix(l, suffix)
}

// handle processes one datagram from src and returns follow-up questions
// to ask (instances seen without SRV/TXT, hosts without addresses).
func (b *mdnsBrowser) handle(data []byte, src netip.AddrPort) []dns.Question {
	srcIP := src.Addr().Unmap()
	zone := srcIP.Zone()
	srcIP = srcIP.WithZone("")
	if !b.local.onLink(srcIP) || b.local.isBroadcast(srcIP) {
		return nil
	}
	var m dns.Msg
	if err := m.Unpack(data); err != nil {
		return nil
	}
	if !m.Response || m.Opcode != dns.OpcodeQuery || m.Rcode != dns.RcodeSuccess {
		return nil
	}
	rrs := append(m.Answer, m.Extra...)
	if len(rrs) > mdnsMaxRecords {
		rrs = rrs[:mdnsMaxRecords]
	}
	var touched []*mdnsEntry
	entry := func(name string) *mdnsEntry {
		k := mdnsKey{src: srcIP, name: strings.ToLower(name)}
		if e, ok := b.entries[k]; ok {
			return e
		}
		if len(b.entries) >= mdnsMaxEntries || b.perSource[srcIP] >= mdnsMaxPerSource {
			return nil
		}
		e := &mdnsEntry{key: k, name: name, zone: zone}
		b.entries[k] = e
		b.order = append(b.order, k)
		b.perSource[srcIP]++
		return e
	}
	addAddr := func(name string, ip netip.Addr) {
		if !advertisedBy(ip, srcIP, b.local.isLocal) {
			return
		}
		k := mdnsKey{src: srcIP, name: strings.ToLower(name)}
		addrs, ok := b.hosts[k]
		if !ok && len(b.hosts) >= mdnsMaxEntries {
			return
		}
		ip = ip.Unmap()
		for _, a := range addrs {
			if a == ip {
				return
			}
		}
		if len(addrs) < mdnsMaxHostAddrs {
			b.hosts[k] = append(addrs, ip)
		}
	}
	for _, rr := range rrs {
		switch rr := rr.(type) {
		case *dns.PTR:
			if strings.EqualFold(rr.Hdr.Name, mdnsServiceName) && instanceOf(rr.Ptr) {
				if e := entry(rr.Ptr); e != nil {
					touched = append(touched, e)
				}
			}
		case *dns.SRV:
			if instanceOf(rr.Hdr.Name) {
				if e := entry(rr.Hdr.Name); e != nil {
					e.host, e.port, e.hasSRV = strings.ToLower(rr.Target), int(rr.Port), true
					touched = append(touched, e)
				}
			}
		case *dns.TXT:
			if instanceOf(rr.Hdr.Name) {
				if e := entry(rr.Hdr.Name); e != nil {
					e.txt = append([]string(nil), rr.Txt[:min(len(rr.Txt), mdnsMaxTXTFields)]...)
					e.hasTXT = true
					touched = append(touched, e)
				}
			}
		case *dns.A:
			if ip, ok := netip.AddrFromSlice(rr.A); ok {
				addAddr(rr.Hdr.Name, ip)
			}
		case *dns.AAAA:
			if ip, ok := netip.AddrFromSlice(rr.AAAA); ok {
				addAddr(rr.Hdr.Name, ip)
			}
		}
	}
	var qs []dns.Question
	ask := func(name string, types ...uint16) {
		for _, t := range types {
			k := strings.ToLower(name) + "|" + dns.TypeToString[t]
			if b.asked[k] || len(b.asked) >= mdnsMaxFollowUps {
				continue
			}
			b.asked[k] = true
			qs = append(qs, dns.Question{Name: name, Qtype: t, Qclass: dns.ClassINET})
		}
	}
	for _, e := range touched {
		if !e.hasSRV {
			ask(e.name, dns.TypeSRV)
		}
		if !e.hasTXT {
			ask(e.name, dns.TypeTXT)
		}
		if e.hasSRV && len(b.hosts[mdnsKey{src: srcIP, name: e.host}]) == 0 && dns.IsFqdn(e.host) {
			ask(e.host, dns.TypeA, dns.TypeAAAA)
		}
	}
	return qs
}

// candidates returns a probe candidate for every complete instance (SRV,
// TXT and an accepted address from the same sender), in the order seen.
func (b *mdnsBrowser) candidates() []candidate {
	var out []candidate
	for _, k := range b.order {
		e := b.entries[k]
		if c, ok := e.candidate(b.hosts[mdnsKey{src: k.src, name: e.host}], b.selfID); ok {
			out = append(out, c)
		}
	}
	return out
}

// candidate converts a DNS-SD result into a probe candidate: the first
// IPv4 and the first IPv6 address (IPv4 preferred), with the sender's zone
// for IPv6 link-local addresses.
func (e *mdnsEntry) candidate(addrs []netip.Addr, selfID string) (candidate, bool) {
	if !e.hasSRV || !e.hasTXT || e.port <= 0 || e.port > 65535 {
		return candidate{}, false
	}
	fields := make(map[string]string)
	for _, f := range e.txt {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[strings.ToLower(k)] = v
		}
	}
	id := fields["id"]
	if id != "" && (!validID(id) || id == selfID) {
		return candidate{}, false
	}
	scheme := "http"
	if strings.EqualFold(fields["scheme"], "https") {
		scheme = "https"
	}
	c := candidate{
		id:      id,
		name:    fields["name"],
		version: fields["v"],
		region:  fields["region"],
		source:  SourceMDNS,
	}
	if c.name == "" {
		c.name, _, _ = strings.Cut(e.name, ".")
	}
	var v4, v6 bool
	for _, ip := range addrs {
		if ip.IsUnspecified() || ip.IsMulticast() || (ip.Is4() && v4) || (ip.Is6() && v6) {
			continue
		}
		host := ip.String()
		if ip.Is6() && ip.IsLinkLocalUnicast() && e.zone != "" {
			host += "%" + e.zone
		}
		u, err := NormalizeBaseURL(baseURL(scheme, host, e.port))
		if err != nil {
			continue
		}
		if ip.Is4() {
			v4 = true
			c.urls = append([]string{u}, c.urls...) // IPv4 first
		} else {
			v6 = true
			c.urls = append(c.urls, u)
		}
	}
	if len(c.urls) == 0 {
		return candidate{}, false
	}
	return c, true
}

// mdnsQuery runs one DNS-SD browse for _gos3._tcp lasting timeout and, once
// it has finished, feeds the results to out as candidates. It queries from
// an ephemeral socket per IP family (responders answer such legacy queries
// by unicast) and also listens on the mDNS group port for multicast
// answers.
func mdnsQuery(ctx context.Context, timeout time.Duration, logger *slog.Logger, selfID string, out sink) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	v4, v6 := ipFamilies()
	if !v4 && !v6 {
		return errors.New("no multicast-capable interface")
	}
	var queriers, socks []*mdnsSocket
	var errs []error
	for _, fam := range []struct{ v6, on bool }{{false, v4}, {true, v6}} {
		if !fam.on {
			continue
		}
		q, err := listenMDNSQuerier(fam.v6, logger)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		queriers = append(queriers, q)
		socks = append(socks, q)
		if g, err := listenMDNSGroup(fam.v6, logger); err == nil {
			socks = append(socks, g)
		} else {
			logger.Debug("mDNS: group port unavailable for browsing", "err", err)
		}
	}
	if len(queriers) == 0 {
		return errors.Join(append([]error{errors.New("failed to bind to any unicast UDP port")}, errs...)...)
	}
	b := newMDNSBrowser(selfID, hostNets)
	err := runMDNSBrowse(ctx, timeout, queriers, socks, b)
	for _, c := range b.candidates() {
		out.candidate(c)
	}
	return err
}

// mdnsReceived is one datagram read by a browse socket.
type mdnsReceived struct {
	data []byte
	src  netip.AddrPort
	ttl  int // IP TTL / IPv6 hop limit, 0 when unknown
}

// receive handles one datagram read by a browse socket. A datagram from
// another host that a router forwarded (TTL / hop limit below 255) is
// dropped: its on-link source address is spoofed.
func (b *mdnsBrowser) receive(p mdnsReceived) []dns.Question {
	if b.local.routed(p.src.Addr(), p.ttl) {
		return nil
	}
	return b.handle(p.data, p.src)
}

// runMDNSBrowse sends the browse query, feeds every datagram received on
// socks to b (asking its follow-up questions) until timeout or ctx ends,
// then closes the sockets and waits for the readers.
func runMDNSBrowse(ctx context.Context, timeout time.Duration, queriers, socks []*mdnsSocket, b *mdnsBrowser) error {
	done := make(chan struct{})
	ch := make(chan mdnsReceived, 64)
	var readers sync.WaitGroup
	for _, s := range socks {
		readers.Go(func() {
			buf := make([]byte, mdnsMaxPacket+1)
			for {
				n, src, meta, err := s.readFrom(buf)
				if err != nil {
					if errors.Is(err, net.ErrClosed) {
						return
					}
					select {
					case <-done:
						return
					default:
						continue
					}
				}
				if n > mdnsMaxPacket {
					continue
				}
				select {
				case ch <- mdnsReceived{data: append([]byte(nil), buf[:n]...), src: src, ttl: meta.ttl}:
				case <-done:
					return
				}
			}
		})
	}
	defer func() {
		close(done)
		for _, s := range socks {
			_ = s.Close()
		}
		readers.Wait()
	}()

	send := func(qs []dns.Question) error {
		m := &dns.Msg{Question: qs}
		buf, err := m.Pack()
		if err != nil {
			return err
		}
		var errs []error
		for _, q := range queriers {
			if err := q.multicastAll(buf); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) == len(queriers) {
			return errors.Join(errs...)
		}
		return nil
	}
	if err := send([]dns.Question{{Name: mdnsServiceName, Qtype: dns.TypePTR, Qclass: dns.ClassINET}}); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case p := <-ch:
			if qs := b.receive(p); len(qs) > 0 {
				_ = send(qs)
			}
		}
	}
}
