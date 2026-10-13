package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// bigInfo returns an identity document close to MaxInfoSize: a long name,
// 200 long endpoints.all URLs and 1000 features.
func bigInfo(t testing.TB, id string) Info {
	t.Helper()
	in := Info{Service: ServiceName, ID: id, Name: strings.Repeat("N", 2000), APIVersion: "v1",
		Endpoints: Endpoints{S3: "http://" + strings.Repeat("s", 60) + ".example:9000"}}
	host := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + ".example"
	for i := range 200 {
		in.Endpoints.All = append(in.Endpoints.All, fmt.Sprintf("http://%s:%d", host, 1000+i))
	}
	for i := range 1000 {
		in.Features = append(in.Features, fmt.Sprintf("feature-%d-%s", i, strings.Repeat("f", 20)))
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxInfoSize {
		b = b[:0]
		in.Features = in.Features[:500]
		if b, err = json.Marshal(in); err != nil || len(b) > MaxInfoSize {
			t.Fatalf("test document too large: %d", len(b))
		}
	}
	out, err := ParseInfo(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStoredInfoBounded: a near-64 KiB identity document is cut down to
// maxStoredInfo bytes of strings (discovery-1).
func TestStoredInfoBounded(t *testing.T) {
	info := bigInfo(t, "big")
	if n := infoSize(info); n > maxStoredInfo {
		t.Fatalf("stored document has %d bytes of strings, want at most %d", n, maxStoredInfo)
	}
	if len(info.Endpoints.All) == 0 || len(info.Endpoints.All) > maxEndpointsAll || len(info.Features) > maxFeatures {
		t.Errorf("lists: %d endpoints, %d features", len(info.Endpoints.All), len(info.Features))
	}
	// Endpoint URLs that are too long are dropped, not cut into other
	// URLs, and endpoints.all keeps base URLs only.
	in := testInfo("x", "x")
	in.Endpoints.Admin = "http://x:9000/" + strings.Repeat("p", maxEndpointLen)
	in.Endpoints.All = []string{"not a url", "ftp://x", "http://ok:1"}
	out := sanitizeInfo(in)
	if out.Endpoints.Admin != "" || len(out.Endpoints.All) != 1 || out.Endpoints.All[0] != "http://ok:1" {
		t.Errorf("endpoints = %+v", out.Endpoints)
	}
}

// TestPeerTableSenderQuota: one sender serving a new ID per port (or per
// request) gets at most maxPeersPerHost verified peers from announcements,
// and announcements altogether at most maxAutoPeers (discovery-1).
func TestPeerTableSenderQuota(t *testing.T) {
	tb := newPeerTable(time.Hour)
	now := time.Now()
	stored := 0
	for i := range 100 {
		_, res := tb.observeInfo(testInfo(fmt.Sprintf("fake%04d", i), "f"), fmt.Sprintf("http://192.0.2.66:%d", 1000+i), SourceBeacon, now, "")
		if res == observeStored {
			stored++
		}
	}
	if stored != maxPeersPerHost {
		t.Fatalf("one sender got %d verified peers, want %d", stored, maxPeersPerHost)
	}
	// IPv6 senders are charged per /64.
	stored = 0
	for i := range 40 {
		if _, res := tb.observeInfo(testInfo(fmt.Sprintf("six%04d", i), "f"), fmt.Sprintf("http://[2001:db8::%x]:9000", i+1), SourceMDNS, now, ""); res == observeStored {
			stored++
		}
	}
	if stored != maxPeersPerHost {
		t.Fatalf("one IPv6 /64 got %d verified peers, want %d", stored, maxPeersPerHost)
	}
	// Other senders still get their own share, up to maxAutoPeers in all.
	for i := 0; tb.countLocked((*peerEntry).auto) < maxAutoPeers; i++ {
		ip := netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)})
		if _, res := tb.observeInfo(testInfo(fmt.Sprintf("auto%05d", i), "a"), "http://"+ip.String()+":9000", SourceBeacon, now, ""); res != observeStored {
			t.Fatalf("sender %s rejected with %d automatic peers", ip, tb.countLocked((*peerEntry).auto))
		}
	}
	if _, res := tb.observeInfo(testInfo("onemore", "a"), "http://10.9.9.9:9000", SourceBeacon, now, ""); res != observeRejected {
		t.Fatal("automatic peer accepted beyond maxAutoPeers")
	}
	// Operator-driven sources are not limited by the announcement quotas:
	// a seed, a DNS SRV target or an admin probe still gets in.
	if _, res := tb.observeInfo(testInfo("seed", "s"), "http://192.0.2.66:9999", SourceStatic, now, ""); res != observeStored {
		t.Fatal("seed rejected because announcements filled their share")
	}
	if _, res := tb.observeInfo(testInfo("probed", "p"), "http://10.9.9.9:9000", SourceProbe, now, ""); res != observeStored {
		t.Fatal("admin probe rejected because announcements filled their share")
	}
}

// TestPeerTableFullEviction: when the table is full, a peer found through
// a trusted source displaces the stalest peer known only from
// announcements, while a new announced peer displaces nothing verified
// (discovery-1).
func TestPeerTableFullEviction(t *testing.T) {
	tb := newPeerTable(time.Hour)
	t0 := time.Now()
	// The stalest automatic peer.
	if _, res := tb.observeInfo(testInfo("oldauto", "o"), "http://10.2.0.1:9000", SourceBeacon, t0, ""); res != observeStored {
		t.Fatal("auto peer rejected")
	}
	for i := range maxPeers - 1 {
		now := t0.Add(time.Duration(i+1) * time.Millisecond)
		if _, res := tb.observeInfo(testInfo(fmt.Sprintf("trusted%04d", i), "t"), fmt.Sprintf("http://t%d:1", i), SourceScan, now, ""); res != observeStored {
			t.Fatalf("trusted peer %d rejected", i)
		}
	}
	if len(tb.peers) != maxPeers {
		t.Fatalf("size %d", len(tb.peers))
	}
	later := t0.Add(time.Hour)
	if _, res := tb.observeInfo(testInfo("newauto", "n"), "http://10.2.0.2:9000", SourceBeacon, later, ""); res != observeRejected {
		t.Fatal("announced peer accepted into a full table")
	}
	if _, ok := tb.get("oldauto"); !ok {
		t.Fatal("announced peer displaced a verified peer")
	}
	if _, res := tb.observeInfo(testInfo("newseed", "n"), "http://seed:1", SourceStatic, later, ""); res != observeStored {
		t.Fatal("seed rejected from a full table")
	}
	if _, ok := tb.get("oldauto"); ok {
		t.Fatal("the stalest announced peer was not displaced by the seed")
	}
	if _, res := tb.observeInfo(testInfo("newseed2", "n"), "http://seed2:1", SourceStatic, later, ""); res != observeRejected {
		t.Fatal("trusted peer displaced another trusted peer")
	}
}

// TestPeerTableUnverifiedPerSender: one sender's unverified sightings use at
// most maxPeersPerHost entries (replacing its own stalest), so a flood
// from one address does not push out other senders' sightings.
func TestPeerTableUnverifiedPerSender(t *testing.T) {
	tb := newPeerTable(time.Hour)
	now := time.Now()
	tb.hint(candidate{id: "legit", source: SourceBeacon, urls: []string{"http://192.0.2.10:9000"}}, now, "")
	for i := range 500 {
		tb.hint(candidate{id: fmt.Sprintf("flood%04d", i), source: SourceBeacon, urls: []string{"http://192.0.2.66:9000"}}, now.Add(time.Duration(i+1)), "")
	}
	if tb.unverified != maxPeersPerHost+1 {
		t.Fatalf("unverified entries %d, want %d", tb.unverified, maxPeersPerHost+1)
	}
	if _, ok := tb.get("legit"); !ok {
		t.Fatal("another sender's sighting was pushed out")
	}
	if _, ok := tb.get("flood0499"); !ok {
		t.Fatal("newest sighting of the flooding sender missing")
	}
}

// TestPeerTableBoundedOutput: a table filled with peers serving
// near-64 KiB documents stays small, and so does its JSON (GET
// /_gos3/admin/v1/discovery) (discovery-1).
func TestPeerTableBoundedOutput(t *testing.T) {
	tb := newPeerTable(time.Hour)
	now := time.Now()
	base := bigInfo(t, "x")
	for i := range maxPeers + 100 {
		info := base
		info.ID = fmt.Sprintf("big%05d", i)
		src := SourceScan
		addr := fmt.Sprintf("http://h%d.example:9000", i)
		if i%2 == 0 {
			src = SourceBeacon
			ip := netip.AddrFrom4([4]byte{10, 3, byte(i >> 8), byte(i)})
			addr = "http://" + ip.String() + ":9000"
		}
		tb.observeInfo(info, addr, src, now, "")
	}
	peers := tb.snapshot("")
	if len(peers) > maxPeers {
		t.Fatalf("%d peers", len(peers))
	}
	b, err := json.Marshal(peers)
	if err != nil {
		t.Fatal(err)
	}
	if limit := maxPeers * 12 << 10; len(b) > limit {
		t.Fatalf("snapshot JSON is %d bytes, want at most %d", len(b), limit)
	}
	t.Logf("%d peers, %d bytes of JSON", len(peers), len(b))
}

// TestSightingsDoNotKeepVerifiedPeersAlive: announcing a verified peer's ID
// (from anywhere, even its own address) no longer refreshes LastSeen: a
// peer that stops answering probes expires (discovery-v1).
func TestSightingsDoNotKeepVerifiedPeersAlive(t *testing.T) {
	clk := newFakeClock()
	tb := newPeerTable(time.Minute)
	tb.observeInfo(testInfo("p", "p"), "http://10.0.0.2:9000", SourceBeacon, clk.Now(), "")
	for range 20 {
		clk.Advance(10 * time.Second)
		tb.hint(candidate{id: "p", source: SourceBeacon, urls: []string{"http://10.0.0.2:9000"}}, clk.Now(), "")
		tb.expire(clk.Now())
	}
	// The announces re-create an unverified sighting at most.
	if p, ok := tb.get("p"); ok && p.Verified {
		t.Fatal("verified peer kept alive by unauthenticated announces")
	}
	// Re-probes come at least twice per TTL, so live peers are refreshed
	// in time.
	if got := newPeerTable(MinPeerTTL).reprobeInterval(); got != MinPeerTTL/2 {
		t.Errorf("reprobe interval for the minimum TTL = %v", got)
	}
	if got := newPeerTable(time.Hour).reprobeInterval(); got != reprobeEvery {
		t.Errorf("reprobe interval for a long TTL = %v", got)
	}
}

// TestSourceLimiterNoReset: many other senders do not reset the bucket of
// an active sender (discovery-7).
func TestSourceLimiterNoReset(t *testing.T) {
	l := newSourceLimiter(sourceBurst, sourceRefill)
	now := time.Now()
	a := netip.MustParseAddr("192.0.2.66")
	for range sourceBurst {
		l.allow(a, now)
	}
	for i := range 5 * maxSourceEntries {
		l.allow(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), now)
		if i%64 == 0 && l.allow(a, now) {
			t.Fatalf("bucket of the active sender was reset after %d other senders", i)
		}
	}
	if n := l.buckets.len(); n > maxSourceEntries {
		t.Errorf("%d buckets", n)
	}
}

// TestThrottleNoReset: a flood of new candidate URLs does not re-enable
// probing of a URL probed moments ago (discovery-7).
func TestThrottleNoReset(t *testing.T) {
	th := newThrottle(candidateThrottle)
	now := time.Now()
	if !th.allow("http://192.0.2.66:9000", now) {
		t.Fatal("first probe refused")
	}
	for i := range 3 * maxThrottleEntries {
		th.allow(fmt.Sprintf("http://10.0.0.1:%d", i), now)
		if i%128 == 0 && th.allow("http://192.0.2.66:9000", now) {
			t.Fatalf("throttle reset after %d other URLs", i)
		}
	}
	if !th.allow("http://192.0.2.66:9000", now.Add(candidateThrottle)) {
		t.Error("URL still throttled after the window")
	}
}

// TestRateLimiter: the global budgets refill over time.
func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(3, time.Second)
	now := time.Now()
	n := 0
	for range 10 {
		if l.allow(now) {
			n++
		}
	}
	if n != 3 || l.allow(now) || !l.allow(now.Add(time.Second)) {
		t.Errorf("allowed %d", n)
	}
}

// TestBeaconQueryRestrictions: queries from a broadcast or network source
// address are not answered (a reply would reach the whole link), and each
// sender's queries and all announces together are budgeted (discovery-2).
func TestBeaconQueryRestrictions(t *testing.T) {
	sink := &recordSink{}
	clk := newFakeClock()
	info := testInfo("aaaaaaaaaaaaaaaa", "alpha")
	// No sockets: a scheduled reply would panic on the nil connection.
	b := newBeacon(beaconOptions{advertise: true, selfInfo: func() Info { return info }, sink: sink, logger: testLogger(t), now: clk.Now},
		nil, nil, func() []net.Addr { return nil })
	b.local = lanNets() // 192.0.2.2/24
	ctx := context.Background()
	q, _ := encodeBeacon(beaconMsg{Type: beaconQuery, Nonce: "n"})
	for _, s := range []string{"192.0.2.255", "192.0.2.0", "255.255.255.255"} {
		b.handle(ctx, q, &net.UDPAddr{IP: net.ParseIP(s), Port: 7373})
		b.handle(ctx, announceBytes(t, "bcast000", "http://192.0.2.9:9000"), &net.UDPAddr{IP: net.ParseIP(s), Port: 7373})
	}
	if len(b.replySem) != 0 {
		t.Fatal("query from a broadcast source scheduled a reply")
	}
	if cs, _ := sink.snapshot(); len(cs) != 0 {
		t.Fatalf("announce from a broadcast source accepted: %+v", cs)
	}

	// Per-sender query budget.
	src := netip.MustParseAddr("192.0.2.50")
	for range queryBurst {
		if !b.qlimit.allow(src, clk.Now()) {
			t.Fatal("query within the burst refused")
		}
	}
	if b.qlimit.allow(src, clk.Now()) {
		t.Fatal("query beyond the per-sender burst allowed")
	}

	// Global announce budget: spoofed senders each have their own bucket,
	// but all of them together get announceBurst.
	for i := range 2 * announceBurst {
		ip := net.IPv4(192, 0, 2, byte(10+i%200))
		b.handle(ctx, announceBytes(t, fmt.Sprintf("spoof%04d", i), "http://192.0.2.9:9000"), &net.UDPAddr{IP: ip, Port: 7373})
	}
	if cs, _ := sink.snapshot(); len(cs) != announceBurst {
		t.Errorf("%d announces accepted at once from many senders, want %d", len(cs), announceBurst)
	}
}

// TestLocalNetsFailClosed: when the interfaces cannot be listed, only
// loopback senders are on-link (discovery-6).
func TestLocalNetsFailClosed(t *testing.T) {
	l := &localNets{load: func() []netip.Prefix { return nil }}
	if l.onLink(netip.MustParseAddr("192.0.2.5")) || l.onLink(netip.MustParseAddr("2001:db8::1")) {
		t.Error("an unknown subnet is treated as on-link")
	}
	if !l.onLink(netip.MustParseAddr("127.0.0.1")) || !l.onLink(netip.MustParseAddr("::1")) {
		t.Error("loopback is not on-link")
	}
	if !lanNets().isBroadcast(netip.MustParseAddr("192.0.2.0")) {
		t.Error("network address not treated as broadcast")
	}
}

// TestMDNSSocketReportsTTL: mDNS sockets report the IP TTL of received
// datagrams on every supported platform, including Windows, where
// golang.org/x/net cannot (discovery-3).
func TestMDNSSocketReportsTTL(t *testing.T) {
	s, err := listenMDNSQuerier(false, testLogger(t))
	if err != nil {
		t.Skipf("UDP unavailable: %v", err)
	}
	defer s.Close()
	if runtime.GOOS == "windows" && s.recv == nil {
		t.Fatal("the WSARecvMsg reader is not used on Windows")
	}
	if !s.cmsg {
		t.Skip("this platform cannot report IP control messages")
	}
	port := s.conn.LocalAddr().(*net.UDPAddr).Port
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	ps := ipv4.NewPacketConn(sender)
	buf := make([]byte, 64)
	for _, ttl := range []int{255, 64} {
		if err := ps.SetTTL(ttl); err != nil {
			t.Fatal(err)
		}
		if _, err := sender.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}); err != nil {
			t.Fatal(err)
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, meta, err := s.readFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 || from.Addr() != netip.MustParseAddr("127.0.0.1") || from.Port() != uint16(sender.LocalAddr().(*net.UDPAddr).Port) {
			t.Errorf("read %d bytes from %v", n, from)
		}
		if meta.ttl != ttl || meta.ifIndex <= 0 {
			t.Errorf("TTL %d, interface %d; want TTL %d and an interface", meta.ttl, meta.ifIndex, ttl)
		}
	}
}

// TestMDNSResponderNoUnicastWithoutTTL: where the TTL of a query cannot be
// read, other hosts are answered by multicast to the link-local group only,
// never by unicast to a possibly spoofed source (discovery-3).
func TestMDNSResponderNoUnicastWithoutTTL(t *testing.T) {
	r, _ := testResponder(t)
	w := &fakeWriter{}
	ptr := q(mdnsServiceName, dns.TypePTR)
	r.handle(mdnsPacket{data: query(t, 0x1234, ptr), src: src("10.0.0.7:40000"), ifIndex: 3, noTTL: true}, w)
	r.handle(mdnsPacket{data: query(t, 9, qu(mdnsServiceName, dns.TypePTR)), src: src("10.0.0.8:5353"), ifIndex: 4, noTTL: true}, w)
	if m, u := w.counts(); u != 0 || m == 0 {
		t.Fatalf("multicast %d, unicast %d; want multicast only", m, u)
	}
	// A socket that reports control messages but delivered no TTL for a
	// datagram: that datagram is unchecked too.
	if p := newMDNSPacket(nil, src("10.0.0.7:40000"), mdnsMeta{ifIndex: 3}, true); !p.noTTL {
		t.Error("datagram without a TTL treated as checked")
	}
	if p := newMDNSPacket(nil, src("10.0.0.7:40000"), mdnsMeta{ifIndex: 3, ttl: 255}, true); p.noTTL {
		t.Error("datagram with TTL 255 treated as unchecked")
	}
	if p := newMDNSPacket(nil, src("10.0.0.7:40000"), mdnsMeta{ttl: 255}, false); !p.noTTL {
		t.Error("datagram from a socket without control messages treated as checked")
	}
	// This host itself is still answered by unicast.
	w.reset()
	r.handle(mdnsPacket{data: query(t, 0x1234, ptr), src: src("127.0.0.1:40000"), noTTL: true}, w)
	if _, u := w.counts(); u != 1 {
		t.Fatalf("legacy query from this host: unicast %d", u)
	}
}

// TestSightingOfLocalAddressCoarseError: a failed verification of a
// sighting that named a loopback address records a fixed text, so the peer
// table is no oracle for local ports (discovery-5).
func TestSightingOfLocalAddressCoarseError(t *testing.T) {
	s := New(quietConfig(), nil, testLogger(t))
	defer s.prober.close()
	dead := deadURL(t) // http://127.0.0.1:<closed port>
	s.table.hint(candidate{id: "dddd000000000004", source: SourceBeacon, urls: []string{dead}}, s.now(), "")
	s.verify(context.Background(), candidate{id: "dddd000000000004", source: SourceBeacon, urls: []string{dead}}, time.Second)
	if p, _ := s.table.get("dddd000000000004"); p.Healthy || p.Error != sightingNotVerified {
		t.Fatalf("sighting of a closed local port recorded %q", p.Error)
	}
}

// TestScanFoundBounded: a host answering with a new ID on every address
// adds at most maxPeers peers to a scan result (discovery-1).
func TestScanFoundBounded(t *testing.T) {
	found := make(map[string]Peer)
	now := time.Now()
	for i := range maxPeers + 100 {
		addFound(found, peerFromInfo(testInfo(fmt.Sprintf("gen%06d", i), "gen"), fmt.Sprintf("http://10.0.0.5:%d", 1000+i), SourceScan, now))
	}
	if len(found) != maxPeers {
		t.Fatalf("found %d peers, want %d", len(found), maxPeers)
	}
	// Known IDs still merge their addresses.
	addFound(found, peerFromInfo(testInfo("gen000000", "gen"), "http://10.0.0.6:9000", SourceScan, now))
	if p := found["gen000000"]; len(p.Addresses) != 2 {
		t.Errorf("addresses = %v", p.Addresses)
	}
}
