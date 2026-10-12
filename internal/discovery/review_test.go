package discovery

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// lanNets pretends this host is 192.0.2.2/24.
func lanNets() *localNets {
	return &localNets{load: func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("192.0.2.2/24")} }}
}

func announceBytes(t *testing.T, id string, urls ...string) []byte {
	t.Helper()
	b, err := encodeBeacon(beaconMsg{Type: beaconAnnounce, ID: id, Name: "n", URLs: urls})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBeaconSenderRestrictions checks the defences against unauthenticated
// beacon datagrams: senders off the local link are ignored, announced
// URLs only ever point at the sender (no blind SSRF / port scanning of
// other hosts through the server), each sender's announces are rate
// limited, and the sender of a bye is passed on for verification.
func TestBeaconSenderRestrictions(t *testing.T) {
	sink := &recordSink{}
	clk := newFakeClock()
	b := newBeacon(beaconOptions{sink: sink, logger: testLogger(t), now: clk.Now}, nil, nil, func() []net.Addr { return nil })
	b.local = lanNets()
	ctx := context.Background()

	// Off-link sender: dropped.
	b.handle(ctx, announceBytes(t, "evil0000", "http://192.0.2.9:6379"), &net.UDPAddr{IP: net.IPv4(198, 51, 100, 9), Port: 7373})
	if cs, _ := sink.snapshot(); len(cs) != 0 {
		t.Fatalf("off-link announce accepted: %+v", cs)
	}

	// On-link sender: every URL is rewritten to the sender's address.
	src := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 50), Port: 7373}
	b.handle(ctx, announceBytes(t, "evil0000", "http://172.17.0.1:19999/p0", "https://internal.example.com:8443", "http://192.0.2.50:9000"), src)
	cs, _ := sink.snapshot()
	if len(cs) != 1 || !slices.Equal(cs[0].urls, []string{"http://192.0.2.50:19999", "https://192.0.2.50:8443", "http://192.0.2.50:9000"}) {
		t.Fatalf("candidates = %+v", cs)
	}

	// Rate limit per sender.
	for i := range 10 * sourceBurst {
		b.handle(ctx, announceBytes(t, fmt.Sprintf("flood%05d", i), "http://192.0.2.50:9000"), src)
	}
	cs, _ = sink.snapshot()
	if n := len(cs) - 1; n > sourceBurst {
		t.Errorf("%d announces from one sender accepted at once, want at most %d", n, sourceBurst)
	}
	clk.Advance(10 * sourceRefill)
	before := len(cs)
	b.handle(ctx, announceBytes(t, "later000", "http://192.0.2.50:9000"), src)
	if cs, _ = sink.snapshot(); len(cs) != before+1 {
		t.Error("sender not allowed again after the refill")
	}
	// Another sender has its own budget.
	b.handle(ctx, announceBytes(t, "other000", "http://192.0.2.51:9000"), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 51), Port: 7373})
	if cs2, _ := sink.snapshot(); len(cs2) != len(cs)+1 {
		t.Error("second sender throttled")
	}

	// Queries from off-link senders are not answered (no reflection).
	b.advertise = true
	b.handle(ctx, []byte(`{"proto":"gos3-beacon/1","type":"query"}`), &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 1})
	if len(b.replySem) != 0 {
		t.Error("query from an off-link sender scheduled a reply")
	}
}

// TestByeRequiresPeerAddress checks that a spoofed bye (any datagram with a
// known peer's ID) no longer marks a verified peer unhealthy: only a bye
// from one of the peer's addresses is believed; others just make the peer
// due for a re-probe.
func TestByeRequiresPeerAddress(t *testing.T) {
	s := New(quietConfig(), nil, testLogger(t))
	clk := newFakeClock()
	s.now = clk.Now
	info := testInfo("24574d5e22530bda", "real")
	s.table.observeInfo(info, "http://10.0.0.5:9000", SourceBeacon, clk.Now(), "")

	s.bye(info.ID, netip.MustParseAddr("10.0.0.66"))
	p, _ := s.table.get(info.ID)
	if !p.Healthy {
		t.Fatalf("spoofed bye marked the peer unhealthy: %+v", p)
	}
	if d := s.table.due(clk.Now(), reprobeEvery); len(d) != 1 {
		t.Error("unconfirmed bye did not schedule a re-probe")
	}
	s.bye(info.ID, netip.MustParseAddr("10.0.0.5"))
	if p, _ = s.table.get(info.ID); p.Healthy || p.Error != "server announced shutdown" {
		t.Fatalf("bye from the peer ignored: %+v", p)
	}
}

// TestSightingFailureLeavesVerifiedPeer checks that an announce reusing a
// verified peer's ID with an unreachable URL does not mark the real peer
// unhealthy (previously one datagram every few seconds kept any peer
// "unhealthy").
func TestSightingFailureLeavesVerifiedPeer(t *testing.T) {
	defer checkLeaks(t)()
	real := newGos3Server(t, "aaaa000000000001", "alpha")
	s := New(quietConfig(), nil, testLogger(t))
	defer s.prober.close()
	if _, err := s.Probe(context.Background(), real.URL); err != nil {
		t.Fatal(err)
	}
	dead := deadURL(t)
	s.verify(context.Background(), candidate{id: "aaaa000000000001", source: SourceBeacon, urls: []string{dead}}, time.Second)
	if p, _ := s.table.get("aaaa000000000001"); !p.Healthy || p.Error != "" {
		t.Fatalf("verified peer marked unhealthy by an unverified sighting: %+v", p)
	}
	// Another server answering at the announced URL does not either.
	other := newGos3Server(t, "bbbb000000000002", "beta")
	s.verify(context.Background(), candidate{id: "aaaa000000000001", source: SourceBeacon, urls: []string{other.URL}}, time.Second)
	if p, _ := s.table.get("aaaa000000000001"); !p.Healthy {
		t.Fatalf("verified peer marked unhealthy: %+v", p)
	}
	// An unverified sighting that fails is still reported.
	s.table.hint(candidate{id: "cccc000000000003", source: SourceBeacon, urls: []string{dead}}, s.now(), "")
	s.verify(context.Background(), candidate{id: "cccc000000000003", source: SourceBeacon, urls: []string{dead}}, time.Second)
	if p, _ := s.table.get("cccc000000000003"); p.Healthy || p.Error == "" {
		t.Fatalf("unverified failure not recorded: %+v", p)
	}
	real.Close()
	other.Close()
}

// deadURL returns the base URL of a closed loopback port.
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + ln.Addr().String()
	ln.Close()
	return u
}

// TestCleanFieldControls checks that C1 controls (e.g. U+009B, an 8-bit
// CSI) and bidi overrides from the network are removed before names are
// shown in the UI or printed by `gos3 discover`.
func TestCleanFieldControls(t *testing.T) {
	in := "\u202egnp.evil\u009b2J\u2066x\u2069\u200fy\u2028z"
	got := cleanField(in, maxFieldLen)
	if got != "gnp.evil2Jxyz" {
		t.Errorf("cleanField(%q) = %q", in, got)
	}
	if got := cleanField("ünïcode – ok 🙂", maxFieldLen); got != "ünïcode – ok 🙂" {
		t.Errorf("printable text changed: %q", got)
	}
}

// TestMDNSAdvertisesListener checks that mDNS advertises the scheme and
// port of the listener at the advertised interface addresses, not those of
// server.public_url (a reverse proxy on https:443 elsewhere).
func TestMDNSAdvertisesListener(t *testing.T) {
	ips := interfaceIPs(true)
	if len(ips) == 0 {
		t.Skip("no non-loopback interface")
	}
	info := testInfo("b3f1c2d4e5f60718", "nas")
	info.Endpoints.S3 = "https://s3.example.com"
	info.Endpoints.All = []string{"https://s3.example.com", "http://" + netip.AddrPortFrom(ips[0], 9000).String()}
	svc, _, err := buildMDNSZone(info)
	if err != nil {
		t.Fatal(err)
	}
	if svc.Port != 9000 || !slices.Contains(svc.TXT, "scheme=http") {
		t.Errorf("advertised port %d, TXT %v; want the listener's 9000 over http", svc.Port, svc.TXT)
	}
	if got := mdnsBaseURL(Info{Endpoints: Endpoints{S3: "https://s3.example.com"}}, ips); got != "https://s3.example.com" {
		t.Errorf("fallback = %q", got)
	}
	if !strings.HasPrefix(mdnsBaseURL(info, ips), "http://") {
		t.Error("listener URL not preferred")
	}
}
