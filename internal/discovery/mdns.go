package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// mDNS settings (docs/discovery.md §2).
const (
	mdnsDomain       = "local."
	mdnsPort         = 5353
	mdnsQueryTimeout = 3 * time.Second
	mdnsBrowseEvery  = 30 * time.Second
	mdnsRefreshEvery = 30 * time.Second
	mdnsMaxInstance  = 50
	mdnsMaxTXTFields = 32
	// mdnsMaxEntries bounds the distinct services kept from one browse.
	mdnsMaxEntries = 256
	// mdnsTTL is the TTL of advertised records (as before: 120 s);
	// mdnsLegacyTTL caps it in replies to legacy unicast queries (RFC 6762
	// §6.7).
	mdnsTTL       = 120
	mdnsLegacyTTL = 10
	// mdnsMaxPacket is the largest mDNS datagram read (RFC 6762 §17).
	mdnsMaxPacket = 9000
	// mdnsMaxResponse bounds every response so it fits in one unfragmented
	// packet on any common link.
	mdnsMaxResponse = 1400
	// mdnsMaxAddrs bounds the A/AAAA records advertised.
	mdnsMaxAddrs = 16
)

var (
	mdnsGroup4 = netip.AddrPortFrom(netip.MustParseAddr("224.0.0.251"), mdnsPort)
	mdnsGroup6 = netip.AddrPortFrom(netip.MustParseAddr("ff02::fb"), mdnsPort)
)

// sanitizeLabel maps s to [A-Za-z0-9-] (other characters become '-'),
// trims leading/trailing '-' and truncates to n bytes.
func sanitizeLabel(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= n {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// shortID returns the first 6 characters of id.
func shortID(id string) string { return id[:min(6, len(id))] }

// mdnsInstanceName returns "<sanitised name ≤ 50>-<first 6 of id>".
func mdnsInstanceName(name, id string) string {
	n := sanitizeLabel(name, mdnsMaxInstance)
	if n == "" {
		n = "gos3"
	}
	return n + "-" + shortID(id)
}

// mdnsHostName returns "<os hostname>.local." (first label, sanitised).
func mdnsHostName(id string) string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	h = sanitizeLabel(h, 63)
	if h == "" {
		h = "gos3-" + shortID(id)
	}
	return h + "." + mdnsDomain
}

// mdnsServiceName is the fully qualified service type, "_gos3._tcp.local.".
const mdnsServiceName = MDNSService + "." + mdnsDomain

// mdnsEnumName is the DNS-SD service type enumeration name (RFC 6763 §9).
const mdnsEnumName = "_services._dns-sd._udp." + mdnsDomain

// mdnsZone holds the DNS-SD records advertising this server: the only
// records the responder ever answers with.
type mdnsZone struct {
	Instance string       // instance label, "nas-b3f1c2"
	Service  string       // MDNSService
	HostName string       // "host.local."
	Port     int          // listener port
	IPs      []netip.Addr // advertised addresses
	TXT      []string

	// Lower-cased owner names, for matching questions.
	serviceName, instanceName, hostName string

	enum, ptr, srv, txt dns.RR
	a, aaaa             []dns.RR
}

func rrHeader(name string, t uint16) dns.RR_Header {
	return dns.RR_Header{Name: name, Rrtype: t, Class: dns.ClassINET, Ttl: mdnsTTL}
}

// newMDNSZone builds the records of one service instance.
func newMDNSZone(instance, host string, port int, ips []netip.Addr, txt []string) (*mdnsZone, error) {
	if instance == "" || port <= 0 || port > 65535 {
		return nil, errors.New("mDNS: missing instance name or port")
	}
	instanceFQDN := instance + "." + mdnsServiceName
	if _, ok := dns.IsDomainName(instanceFQDN); !ok {
		return nil, fmt.Errorf("mDNS: invalid instance name %q", instance)
	}
	if _, ok := dns.IsDomainName(host); !ok || !dns.IsFqdn(host) {
		return nil, fmt.Errorf("mDNS: invalid host name %q", host)
	}
	z := &mdnsZone{
		Instance:     instance,
		Service:      MDNSService,
		HostName:     host,
		Port:         port,
		TXT:          slices.Clone(txt),
		serviceName:  strings.ToLower(mdnsServiceName),
		instanceName: strings.ToLower(instanceFQDN),
		hostName:     strings.ToLower(host),
	}
	z.enum = &dns.PTR{Hdr: rrHeader(mdnsEnumName, dns.TypePTR), Ptr: mdnsServiceName}
	z.ptr = &dns.PTR{Hdr: rrHeader(mdnsServiceName, dns.TypePTR), Ptr: instanceFQDN}
	z.srv = &dns.SRV{Hdr: rrHeader(instanceFQDN, dns.TypeSRV), Priority: 10, Weight: 1, Port: uint16(port), Target: host}
	z.txt = &dns.TXT{Hdr: rrHeader(instanceFQDN, dns.TypeTXT), Txt: z.TXT}
	for _, ip := range ips {
		ip = ip.Unmap().WithZone("")
		if len(z.IPs) >= mdnsMaxAddrs || !ip.IsValid() || slices.Contains(z.IPs, ip) {
			continue
		}
		z.IPs = append(z.IPs, ip)
		if ip.Is4() {
			z.a = append(z.a, &dns.A{Hdr: rrHeader(host, dns.TypeA), A: net.IP(ip.AsSlice())})
		} else {
			z.aaaa = append(z.aaaa, &dns.AAAA{Hdr: rrHeader(host, dns.TypeAAAA), AAAA: net.IP(ip.AsSlice())})
		}
	}
	return z, nil
}

// records returns the answer and additional records for question q (RFC
// 6763 §12: a PTR answer comes with the SRV, TXT and address records, an
// SRV answer with the address records). Questions for any other name, type
// or class get nothing.
func (z *mdnsZone) records(q dns.Question) (answer, extra []dns.RR) {
	if c := q.Qclass &^ (1 << 15); c != dns.ClassINET && c != dns.ClassANY {
		return nil, nil
	}
	is := func(t uint16) bool { return q.Qtype == t || q.Qtype == dns.TypeANY }
	addrs := append(slices.Clone(z.a), z.aaaa...)
	switch strings.ToLower(q.Name) {
	case strings.ToLower(mdnsEnumName):
		if is(dns.TypePTR) {
			answer = append(answer, z.enum)
		}
	case z.serviceName:
		if is(dns.TypePTR) {
			answer = append(answer, z.ptr)
			extra = append([]dns.RR{z.srv, z.txt}, addrs...)
		}
	case z.instanceName:
		if is(dns.TypeSRV) {
			answer = append(answer, z.srv)
			extra = addrs
		}
		if is(dns.TypeTXT) {
			answer = append(answer, z.txt)
		}
	case z.hostName:
		if is(dns.TypeA) {
			answer = append(answer, z.a...)
		}
		if is(dns.TypeAAAA) {
			answer = append(answer, z.aaaa...)
		}
	}
	return answer, extra
}

// buildMDNSZone builds the DNS-SD records advertising info. The returned
// key changes whenever any advertised value changes.
func buildMDNSZone(info Info) (*mdnsZone, string, error) {
	if !validID(info.ID) {
		return nil, "", errors.New("server ID unknown")
	}
	addrs := interfaceIPs(true)
	if len(addrs) == 0 {
		return nil, "", errors.New("no non-loopback interface address")
	}
	scheme, port, ok := portOf(mdnsBaseURL(info, addrs))
	if !ok {
		return nil, "", errors.New("no S3 endpoint URL to advertise")
	}
	api := cleanField(info.APIVersion, 16)
	if api == "" {
		api = "v1"
	}
	txt := []string{
		"id=" + info.ID,
		"name=" + cleanField(info.Name, 200),
		"v=" + cleanField(info.Version, 200),
		"api=" + api,
		"region=" + cleanField(info.Region, 200),
		"scheme=" + scheme,
		"info=" + InfoPath,
	}
	instance := mdnsInstanceName(info.Name, info.ID)
	host := mdnsHostName(info.ID)
	z, err := newMDNSZone(instance, host, port, addrs, txt)
	if err != nil {
		return nil, "", err
	}
	key := fmt.Sprintf("%s|%s|%d|%v|%s", instance, host, port, z.IPs, strings.Join(txt, "|"))
	return z, key, nil
}

// mdnsBaseURL picks the endpoint whose scheme and port the mDNS records
// advertise together with the interface addresses addrs: the first endpoint
// served directly at one of those addresses (the listener), not
// Endpoints.S3, which is server.public_url when one is set and may live on
// another host and port (e.g. https on 443 at a reverse proxy). Without
// such an endpoint it falls back to Endpoints.S3, then Endpoints.All[0].
func mdnsBaseURL(info Info, addrs []netip.Addr) string {
	for _, u := range append(slices.Clone(info.Endpoints.All), info.Endpoints.S3) {
		_, host, _, ok := urlHostPort(u)
		if !ok {
			continue
		}
		if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && slices.Contains(addrs, ip.Unmap()) {
			return u
		}
	}
	if info.Endpoints.S3 != "" || len(info.Endpoints.All) == 0 {
		return info.Endpoints.S3
	}
	return info.Endpoints.All[0]
}

// mdnsAdvertiser keeps the mDNS responder's zone in sync with the server's
// Info. The responder's sockets are opened on the first refresh that has a
// zone to advertise and stay open until shutdown; later changes only swap
// the zone.
type mdnsAdvertiser struct {
	self   func() Info
	logger *slog.Logger
	// open opens the responder sockets (replaceable in tests).
	open func(*slog.Logger) ([]*mdnsSocket, error)

	resp    *mdnsResponder
	socks   []*mdnsSocket
	serving sync.WaitGroup
	key     string
	lastErr string
}

// refresh updates the advertised zone, opening the sockets when needed.
func (a *mdnsAdvertiser) refresh() {
	if a.resp == nil {
		a.resp = newMDNSResponder(a.logger)
	}
	z, key, err := buildMDNSZone(a.self())
	if err == nil && a.socks == nil {
		open := a.open
		if open == nil {
			open = openMDNSResponderSockets
		}
		if a.socks, err = open(a.logger); err == nil {
			for _, s := range a.socks {
				a.serving.Go(func() { a.resp.serve(s) })
			}
		}
	}
	if err != nil {
		a.resp.setZone(nil)
		a.key = ""
		if msg := err.Error(); msg != a.lastErr {
			a.logger.Warn("mDNS advertising unavailable", "err", err)
			a.lastErr = msg
		} else {
			a.logger.Debug("mDNS advertising unavailable", "err", err)
		}
		return
	}
	if key != a.key {
		a.resp.setZone(z)
		a.key = key
		a.logger.Debug("mDNS advertising", "service", MDNSService, "instance", z.Instance, "port", z.Port)
	}
	a.lastErr = ""
}

// shutdown closes the responder sockets and waits for its goroutines.
func (a *mdnsAdvertiser) shutdown() {
	for _, s := range a.socks {
		_ = s.Close()
	}
	a.serving.Wait()
	a.socks = nil
	a.key = ""
	if a.resp != nil {
		a.resp.setZone(nil)
	}
}

// run refreshes the advertisement every 30 s until ctx is done, then
// withdraws it.
func (a *mdnsAdvertiser) run(ctx context.Context) {
	defer a.shutdown()
	t := time.NewTicker(mdnsRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refresh()
		}
	}
}

// ipFamilies reports whether IPv4 / IPv6 multicast appears usable.
func ipFamilies() (v4, v6 bool) {
	ifs, err := net.Interfaces()
	if err != nil {
		return true, false
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ipn.IP.To4() != nil {
					v4 = true
				} else {
					v6 = true
				}
			}
		}
	}
	if v6 {
		c, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
		if err != nil {
			v6 = false
		} else {
			_ = c.Close()
		}
	}
	return v4, v6
}
