package discovery

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// testNets: this host is 10.0.0.1/24 and fe80::1/64; everything else is
// off-link.
func testNets() *localNets {
	return &localNets{load: func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("10.0.0.1/24"), netip.MustParsePrefix("fe80::1/64")}
	}}
}

// fakeWriter records the responder's sends.
type fakeWriter struct {
	mu        sync.Mutex
	multi     [][]byte
	multiIf   []int
	uni       [][]byte
	uniTo     []netip.AddrPort
	uniIfaces []int
}

func (w *fakeWriter) multicast(b []byte, ifIndex int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.multi = append(w.multi, slices.Clone(b))
	w.multiIf = append(w.multiIf, ifIndex)
	return nil
}

func (w *fakeWriter) unicast(b []byte, to netip.AddrPort, ifIndex int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.uni = append(w.uni, slices.Clone(b))
	w.uniTo = append(w.uniTo, to)
	w.uniIfaces = append(w.uniIfaces, ifIndex)
	return nil
}

func (w *fakeWriter) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.multi, w.multiIf, w.uni, w.uniTo, w.uniIfaces = nil, nil, nil, nil, nil
}

func (w *fakeWriter) counts() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.multi), len(w.uni)
}

func testZone(t *testing.T, ips ...string) *mdnsZone {
	t.Helper()
	var addrs []netip.Addr
	for _, s := range ips {
		addrs = append(addrs, netip.MustParseAddr(s))
	}
	z, err := newMDNSZone("nas-b3f1c2", "host.local.", 9000, addrs,
		[]string{"id=b3f1c2d4e5f60718", "name=nas", "v=1", "api=v1", "region=r", "scheme=http", "info=" + InfoPath})
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func testResponder(t *testing.T) (*mdnsResponder, *fakeClock) {
	t.Helper()
	r := newMDNSResponder(testLogger(t))
	r.local = testNets()
	clk := newFakeClock()
	r.now = clk.Now
	r.setZone(testZone(t, "10.0.0.1", "fe80::1"))
	return r, clk
}

func query(t testing.TB, id uint16, qs ...dns.Question) []byte {
	t.Helper()
	m := &dns.Msg{MsgHdr: dns.MsgHdr{Id: id}, Question: qs}
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func q(name string, t uint16) dns.Question {
	return dns.Question{Name: name, Qtype: t, Qclass: dns.ClassINET}
}

func qu(name string, t uint16) dns.Question {
	return dns.Question{Name: name, Qtype: t, Qclass: dns.ClassINET | 1<<15}
}

func src(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func unpack(t *testing.T, b []byte) *dns.Msg {
	t.Helper()
	var m dns.Msg
	if err := m.Unpack(b); err != nil {
		t.Fatal(err)
	}
	return &m
}

func rrTypes(rrs []dns.RR) []string {
	var out []string
	for _, rr := range rrs {
		out = append(out, dns.TypeToString[rr.Header().Rrtype])
	}
	return out
}

// TestMDNSResponderSourceFilter: only senders on this host or a directly
// connected subnet are answered (the hashicorp responder answered any
// source by unicast).
func TestMDNSResponderSourceFilter(t *testing.T) {
	r, _ := testResponder(t)
	w := &fakeWriter{}
	b := query(t, 1, q(mdnsServiceName, dns.TypePTR))
	for _, from := range []string{
		"203.0.113.7:5353", "203.0.113.7:40000", "[2001:db8::1]:5353", "10.0.1.5:5353",
		"10.0.0.255:5353", "255.255.255.255:5353", "0.0.0.0:5353", "224.0.0.251:5353", "10.0.0.7:0",
	} {
		r.handle(mdnsPacket{data: b, src: src(from), ifIndex: 2}, w)
		if m, u := w.counts(); m+u != 0 {
			t.Errorf("answered off-link/invalid source %s", from)
		}
		w.reset()
	}
	for _, from := range []string{"10.0.0.7:5353", "127.0.0.1:5353", "[fe80::7%eth0]:5353", "[::1]:5353"} {
		r.handle(mdnsPacket{data: b, src: src(from), ifIndex: 2}, w)
		if m, u := w.counts(); m != 1 || u != 0 {
			t.Errorf("%s: multicast %d unicast %d, want one multicast answer", from, m, u)
		}
		w.reset()
		r.lastMulticast = map[string]time.Time{} // not testing the 1 s gap here
	}
}

// TestMDNSResponderDropsRoutedDatagrams: queries from other hosts that
// arrived with a TTL / hop limit below 255 were forwarded by a router, so
// their on-link source is spoofed and they get no answer (no reflection
// towards a neighbour). Queries from this host and datagrams whose TTL the
// platform does not report are unaffected.
func TestMDNSResponderDropsRoutedDatagrams(t *testing.T) {
	r, _ := testResponder(t)
	w := &fakeWriter{}
	b := query(t, 1, q(mdnsServiceName, dns.TypePTR))
	for _, tc := range []struct {
		from    string
		ttl     int
		answers bool
	}{
		{"10.0.0.7:5353", 254, false},
		{"10.0.0.7:40000", 64, false}, // legacy port: a unicast reply would reach the spoofed neighbour
		{"[fe80::7%eth0]:5353", 1, false},
		{"10.0.0.7:5353", 255, true},
		{"10.0.0.7:5353", 0, true},
		{"127.0.0.1:40000", 64, true}, // a local tool such as dig
		{"10.0.0.1:40000", 64, true},  // this host's own address
	} {
		r.handle(mdnsPacket{data: b, src: src(tc.from), ttl: tc.ttl}, w)
		m, u := w.counts()
		if got := m+u > 0; got != tc.answers {
			t.Errorf("%s ttl %d: answered = %v, want %v", tc.from, tc.ttl, got, tc.answers)
		}
		w.reset()
		r.lastMulticast = map[string]time.Time{}
	}
}

// TestMDNSResponderSuppressedQueriesCostNothing: queries whose multicast
// answer is suppressed (sent less than a second ago) send nothing, so they
// use neither the sender's nor the global budget; a flood of them must not
// starve queries that do get answers.
func TestMDNSResponderSuppressedQueriesCostNothing(t *testing.T) {
	r, _ := testResponder(t)
	w := &fakeWriter{}
	qm := query(t, 1, q(mdnsServiceName, dns.TypePTR))
	r.handle(mdnsPacket{data: qm, src: src("10.0.0.7:5353"), ifIndex: 2}, w)
	if m, _ := w.counts(); m != 1 {
		t.Fatalf("first query: %d multicast answers, want 1", m)
	}
	for i := range 5 * mdnsMaxResponsesPerSecond {
		r.handle(mdnsPacket{data: qm, src: src(fmt.Sprintf("10.0.0.%d:5353", 10+i%200)), ifIndex: 2}, w)
		r.handle(mdnsPacket{data: qm, src: src("10.0.0.7:5353"), ifIndex: 2}, w)
	}
	if m, _ := w.counts(); m != 1 {
		t.Errorf("suppressed queries were answered: %d multicast answers", m)
	}
	// The global budget and 10.0.0.7's bucket are untouched: QU queries
	// (answered by unicast) still get answers.
	r.handle(mdnsPacket{data: query(t, 2, qu(mdnsServiceName, dns.TypePTR)), src: src("10.0.0.8:5353"), ifIndex: 2}, w)
	r.handle(mdnsPacket{data: query(t, 3, qu(mdnsServiceName, dns.TypePTR)), src: src("10.0.0.7:5353"), ifIndex: 2}, w)
	if _, u := w.counts(); u != 2 {
		t.Errorf("unicast answers after the flood: %d, want 2", u)
	}
}

// TestMDNSResponderDuplicatesAndCap: 200 copies of a question get one
// answer of normal size; only the first mdnsMaxQuestions distinct
// questions are considered.
func TestMDNSResponderDuplicatesAndCap(t *testing.T) {
	r, _ := testResponder(t)
	w := &fakeWriter{}
	qs := make([]dns.Question, 200)
	for i := range qs {
		qs[i] = q(mdnsServiceName, dns.TypePTR)
	}
	b := query(t, 7, qs...)
	r.handle(mdnsPacket{data: b, src: src("10.0.0.7:5353")}, w)
	if len(w.multi) != 1 {
		t.Fatalf("responses: %d multicast", len(w.multi))
	}
	m := unpack(t, w.multi[0])
	if !slices.Equal(rrTypes(m.Answer), []string{"PTR"}) || !slices.Equal(rrTypes(m.Extra), []string{"SRV", "TXT", "A", "AAAA"}) {
		t.Errorf("answer %v extra %v", rrTypes(m.Answer), rrTypes(m.Extra))
	}
	if m.Id != 0 || len(m.Question) != 0 || !m.Response || !m.Authoritative {
		t.Errorf("header = %+v", m.MsgHdr)
	}
	single := w.multi[0]
	w.reset()

	// Same question with different letter case and the QU bit is still a
	// duplicate of the first.
	r.lastMulticast = map[string]time.Time{}
	r.handle(mdnsPacket{data: query(t, 7, q(mdnsServiceName, dns.TypePTR), q(strings.ToUpper(mdnsServiceName), dns.TypePTR), qu(mdnsServiceName, dns.TypePTR)), src: src("10.0.0.8:5353")}, w)
	if len(w.multi) != 1 || len(w.uni) != 0 || len(w.multi[0]) != len(single) {
		t.Errorf("case/QU duplicates answered: %d multicast, %d unicast", len(w.multi), len(w.uni))
	}
	w.reset()

	// Eight distinct questions nobody answers, then ours: ignored.
	var many []dns.Question
	for i := range mdnsMaxQuestions {
		many = append(many, q(fmt.Sprintf("_other%d._tcp.local.", i), dns.TypePTR))
	}
	many = append(many, q(mdnsServiceName, dns.TypePTR))
	r.lastMulticast = map[string]time.Time{}
	r.handle(mdnsPacket{data: query(t, 1, many...), src: src("10.0.0.9:5353")}, w)
	if m, u := w.counts(); m+u != 0 {
		t.Error("question beyond the cap answered")
	}
}

// TestMDNSResponderOwnRecordsOnly: only questions about our own service,
// instance and host records are answered.
func TestMDNSResponderOwnRecordsOnly(t *testing.T) {
	r, _ := testResponder(t)
	w := &fakeWriter{}
	for _, tc := range []struct {
		q              dns.Question
		answer, extras []string
	}{
		{q(mdnsServiceName, dns.TypePTR), []string{"PTR"}, []string{"SRV", "TXT", "A", "AAAA"}},
		{q(mdnsServiceName, dns.TypeANY), []string{"PTR"}, []string{"SRV", "TXT", "A", "AAAA"}},
		{q(mdnsEnumName, dns.TypePTR), []string{"PTR"}, nil},
		{q("nas-b3f1c2."+mdnsServiceName, dns.TypeSRV), []string{"SRV"}, []string{"A", "AAAA"}},
		{q("NAS-B3F1C2."+mdnsServiceName, dns.TypeTXT), []string{"TXT"}, nil},
		{q("nas-b3f1c2."+mdnsServiceName, dns.TypeANY), []string{"SRV", "TXT"}, []string{"A", "AAAA"}},
		{q("host.local.", dns.TypeA), []string{"A"}, nil},
		{q("host.local.", dns.TypeAAAA), []string{"AAAA"}, nil},
		{q("host.local.", dns.TypeANY), []string{"A", "AAAA"}, nil},
		{q("_http._tcp.local.", dns.TypePTR), nil, nil},
		{q("other.local.", dns.TypeA), nil, nil},
		{q("host.local.", dns.TypeTXT), nil, nil},
		{q(mdnsServiceName, dns.TypeSRV), nil, nil},
		{dns.Question{Name: mdnsServiceName, Qtype: dns.TypePTR, Qclass: dns.ClassCHAOS}, nil, nil},
	} {
		r.lastMulticast = map[string]time.Time{}
		r.handle(mdnsPacket{data: query(t, 1, tc.q), src: src("10.0.0.7:5353")}, w)
		if tc.answer == nil {
			if m, u := w.counts(); m+u != 0 {
				t.Errorf("%v answered", tc.q)
			}
			continue
		}
		if len(w.multi) != 1 {
			t.Errorf("%v: %d responses", tc.q, len(w.multi))
			continue
		}
		m := unpack(t, w.multi[0])
		if !slices.Equal(rrTypes(m.Answer), tc.answer) || !slices.Equal(rrTypes(m.Extra), tc.extras) {
			t.Errorf("%v: answer %v extra %v", tc.q, rrTypes(m.Answer), rrTypes(m.Extra))
		}
		w.reset()
	}
	// Responses (and other opcodes) are never answered.
	resp := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Question: []dns.Question{q(mdnsServiceName, dns.TypePTR)}}
	notify := &dns.Msg{MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeNotify}, Question: []dns.Question{q(mdnsServiceName, dns.TypePTR)}}
	for _, m := range []*dns.Msg{resp, notify} {
		b, _ := m.Pack()
		r.handle(mdnsPacket{data: b, src: src("10.0.0.7:5353")}, w)
	}
	r.handle(mdnsPacket{data: []byte("garbage"), src: src("10.0.0.7:5353")}, w)
	if m, u := w.counts(); m+u != 0 {
		t.Error("non-query answered")
	}
	// No zone: silent.
	r.setZone(nil)
	r.handle(mdnsPacket{data: query(t, 1, q(mdnsServiceName, dns.TypePTR)), src: src("10.0.0.7:5353")}, w)
	if m, u := w.counts(); m+u != 0 {
		t.Error("answered without a zone")
	}
}

// TestMDNSResponderTransport: normal queries are answered on the group
// (once per second per interface), QU questions and legacy queries by
// unicast to the on-link sender.
func TestMDNSResponderTransport(t *testing.T) {
	r, clk := testResponder(t)
	w := &fakeWriter{}
	ptr := q(mdnsServiceName, dns.TypePTR)

	r.handle(mdnsPacket{data: query(t, 9, ptr), src: src("10.0.0.7:5353"), ifIndex: 3}, w)
	r.handle(mdnsPacket{data: query(t, 9, ptr), src: src("10.0.0.8:5353"), ifIndex: 3}, w)
	if len(w.multi) != 1 || w.multiIf[0] != 3 || len(w.uni) != 0 {
		t.Fatalf("multicast %d (ifaces %v), unicast %d; want one multicast on interface 3", len(w.multi), w.multiIf, len(w.uni))
	}
	r.handle(mdnsPacket{data: query(t, 9, ptr), src: src("10.0.0.8:5353"), ifIndex: 4}, w)
	clk.Advance(mdnsMulticastGap)
	r.handle(mdnsPacket{data: query(t, 9, ptr), src: src("10.0.0.8:5353"), ifIndex: 3}, w)
	if len(w.multi) != 3 {
		t.Errorf("multicast %d, want 3 (other interface, then after the gap)", len(w.multi))
	}
	w.reset()

	// QU bit: unicast to the sender's port 5353, ID 0, no questions.
	r.handle(mdnsPacket{data: query(t, 9, qu(mdnsServiceName, dns.TypePTR)), src: src("10.0.0.7:5353"), ifIndex: 3}, w)
	if len(w.uni) != 1 || len(w.multi) != 0 || w.uniTo[0] != src("10.0.0.7:5353") {
		t.Fatalf("QU: unicast %v multicast %d", w.uniTo, len(w.multi))
	}
	if m := unpack(t, w.uni[0]); m.Id != 0 || len(m.Question) != 0 || m.Answer[0].Header().Ttl != mdnsTTL {
		t.Errorf("QU response = %v", m)
	}
	w.reset()

	// Legacy unicast (source port ≠ 5353): unicast only, ID and question
	// echoed, TTL ≤ 10 s.
	r.handle(mdnsPacket{data: query(t, 0x1234, ptr, q("host.local.", dns.TypeA)), src: src("[fe80::7%eth0]:40000"), ifIndex: 3}, w)
	if len(w.uni) != 1 || len(w.multi) != 0 || w.uniTo[0] != src("[fe80::7%eth0]:40000") {
		t.Fatalf("legacy: unicast %v multicast %d", w.uniTo, len(w.multi))
	}
	m := unpack(t, w.uni[0])
	if m.Id != 0x1234 || len(m.Question) != 2 || m.Question[0].Qclass != dns.ClassINET {
		t.Errorf("legacy header/questions = %+v %v", m.MsgHdr, m.Question)
	}
	for _, rr := range append(m.Answer, m.Extra...) {
		if rr.Header().Ttl > mdnsLegacyTTL {
			t.Errorf("legacy TTL %d", rr.Header().Ttl)
		}
	}
	if !slices.Equal(rrTypes(m.Answer), []string{"PTR", "A"}) || !slices.Equal(rrTypes(m.Extra), []string{"SRV", "TXT", "AAAA"}) {
		t.Errorf("legacy answer %v extra %v (duplicates not removed?)", rrTypes(m.Answer), rrTypes(m.Extra))
	}
	// The zone's records are not modified by the TTL cap.
	if r.zone.Load().ptr.Header().Ttl != mdnsTTL {
		t.Error("zone record modified")
	}
}

// TestMDNSResponderRateLimit: each sender gets the beacon's token bucket;
// queries the responder does not answer cost nothing.
func TestMDNSResponderRateLimit(t *testing.T) {
	r, clk := testResponder(t)
	w := &fakeWriter{}
	other := query(t, 1, q("_http._tcp.local.", dns.TypePTR))
	for range 100 {
		r.handle(mdnsPacket{data: other, src: src("10.0.0.7:40000")}, w)
	}
	ours := query(t, 1, q(mdnsServiceName, dns.TypePTR))
	for range sourceBurst + 10 {
		r.handle(mdnsPacket{data: ours, src: src("10.0.0.7:40000")}, w)
	}
	if _, u := w.counts(); u != sourceBurst {
		t.Errorf("answered %d queries from one sender, want %d", u, sourceBurst)
	}
	r.handle(mdnsPacket{data: ours, src: src("10.0.0.8:40000")}, w)
	if _, u := w.counts(); u != sourceBurst+1 {
		t.Error("second sender throttled")
	}
	clk.Advance(sourceRefill)
	r.handle(mdnsPacket{data: ours, src: src("10.0.0.7:40000")}, w)
	if _, u := w.counts(); u != sourceBurst+2 {
		t.Error("sender not allowed again after the refill")
	}
	// Global budget across (spoofed) senders.
	clk.Advance(time.Second)
	w.reset()
	for i := range 200 {
		r.handle(mdnsPacket{data: ours, src: src(fmt.Sprintf("10.0.0.%d:40000", 10+i%200))}, w)
	}
	if _, u := w.counts(); u != mdnsMaxResponsesPerSecond {
		t.Errorf("answered %d queries in one second, want %d", u, mdnsMaxResponsesPerSecond)
	}
}

// TestMDNSResponseSizeBounded: a response always fits in one packet of at
// most mdnsMaxResponse bytes, even with many addresses and long TXT values.
func TestMDNSResponseSizeBounded(t *testing.T) {
	var ips []netip.Addr
	for i := range 40 {
		ips = append(ips, netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 15: byte(i)}))
	}
	long := strings.Repeat("x", 200)
	z, err := newMDNSZone(strings.Repeat("n", 50)+"-b3f1c2", strings.Repeat("h", 63)+".local.", 9000, ips,
		[]string{"id=b3f1c2d4e5f60718", "name=" + long, "v=" + long, "api=v1", "region=" + long, "scheme=https", "info=" + InfoPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(z.IPs) != mdnsMaxAddrs {
		t.Errorf("zone keeps %d addresses", len(z.IPs))
	}
	r, _ := testResponder(t)
	r.setZone(z)
	w := &fakeWriter{}
	r.handle(mdnsPacket{data: query(t, 1, q(mdnsServiceName, dns.TypeANY), q(z.HostName, dns.TypeANY), q("nn."+mdnsServiceName, dns.TypeANY)), src: src("10.0.0.7:40000")}, w)
	if len(w.uni) != 1 {
		t.Fatalf("%d responses", len(w.uni))
	}
	if n := len(w.uni[0]); n > mdnsMaxResponse {
		t.Errorf("response is %d bytes", n)
	}
	if m := unpack(t, w.uni[0]); len(m.Answer) == 0 || m.Answer[0].Header().Rrtype != dns.TypePTR {
		t.Errorf("answers = %v", rrTypes(m.Answer))
	}
}

func response(t testing.TB, rrs ...string) []byte {
	t.Helper()
	m := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}}
	for _, s := range rrs {
		rr, err := dns.NewRR(s)
		if err != nil {
			t.Fatal(err)
		}
		m.Answer = append(m.Answer, rr)
	}
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serviceRRs is a complete DNS-SD answer for instance "nas" at host h
// (port, address).
func serviceRRs(inst, host string, port int, addr, id string) []string {
	fq := inst + "." + mdnsServiceName
	rrs := []string{
		mdnsServiceName + " 120 IN PTR " + fq,
		fmt.Sprintf("%s 120 IN SRV 10 1 %d %s", fq, port, host),
		fmt.Sprintf(`%s 120 IN TXT "id=%s" "name=%s" "scheme=http"`, fq, id, inst),
	}
	if addr != "" {
		typ := "A"
		if strings.Contains(addr, ":") {
			typ = "AAAA"
		}
		rrs = append(rrs, host+" 120 IN "+typ+" "+addr)
	}
	return rrs
}

// TestMDNSBrowserSourceFilter: answers from off-link senders are dropped,
// and only addresses the sender may advertise for itself are probed.
func TestMDNSBrowserSourceFilter(t *testing.T) {
	b := newMDNSBrowser("self", testNets())
	feed := func(from string, rrs ...string) { b.handle(response(t, rrs...), src(from)) }

	feed("203.0.113.9:5353", serviceRRs("offlink", "off.local.", 9000, "203.0.113.9", "id0000000000000a")...) // off-link
	feed("10.0.0.255:5353", serviceRRs("bcast", "b.local.", 9000, "10.0.0.255", "id0000000000000b")...)       // broadcast source
	feed("10.0.0.7:5353", serviceRRs("good", "good.local.", 9000, "10.0.0.7", "id0000000000000c")...)
	feed("10.0.0.8:5353", serviceRRs("redirect", "r.local.", 6379, "127.0.0.1", "id0000000000000d")...) // loopback from a remote sender
	feed("10.0.0.9:5353", serviceRRs("other", "o.local.", 22, "10.0.0.50", "id0000000000000e")...)      // another host's address
	feed("10.0.0.1:5353", serviceRRs("local", "l.local.", 9001, "127.0.0.1", "id0000000000000f")...)    // this host: loopback ok
	feed("[fe80::7%eth0]:5353", serviceRRs("v6", "v6.local.", 9000, "fe80::7", "id00000000000010")...)
	feed("10.0.0.10:5353", serviceRRs("self", "s.local.", 9000, "10.0.0.10", "self")...)
	feed("10.0.0.11:5353", serviceRRs("badid", "s.local.", 9000, "10.0.0.11", "bad id")...)

	got := map[string][]string{}
	for _, c := range b.candidates() {
		got[c.id] = c.urls
		if c.source != SourceMDNS {
			t.Errorf("source = %q", c.source)
		}
	}
	want := map[string][]string{
		"id0000000000000c": {"http://10.0.0.7:9000"},
		"id0000000000000f": {"http://127.0.0.1:9001"},
		"id00000000000010": {"http://[fe80::7%25eth0]:9000"},
	}
	if len(got) != len(want) {
		t.Errorf("candidates = %v", got)
	}
	for id, urls := range want {
		if !slices.Equal(got[id], urls) {
			t.Errorf("%s: urls = %v, want %v", id, got[id], urls)
		}
	}
}

// TestMDNSBrowserDropsRoutedDatagrams: an answer from another host that
// arrived with a TTL / hop limit below 255 was forwarded by a router, so
// its on-link source address is spoofed (RFC 6762 §11) and it is dropped.
func TestMDNSBrowserDropsRoutedDatagrams(t *testing.T) {
	b := newMDNSBrowser("self", testNets())
	feed := func(from string, ttl int, rrs ...string) {
		b.receive(mdnsReceived{data: response(t, rrs...), src: src(from), ttl: ttl})
	}
	feed("10.0.0.7:5353", 254, serviceRRs("routed", "r.local.", 9000, "10.0.0.7", "id0000000000000a")...)
	feed("10.0.0.8:5353", 64, serviceRRs("routed2", "r2.local.", 9000, "10.0.0.8", "id0000000000000b")...)
	feed("10.0.0.9:5353", 255, serviceRRs("onlink", "o.local.", 9000, "10.0.0.9", "id0000000000000c")...)
	feed("10.0.0.10:5353", 0, serviceRRs("unknown", "u.local.", 9000, "10.0.0.10", "id0000000000000d")...) // TTL not reported
	feed("10.0.0.1:5353", 64, serviceRRs("local", "l.local.", 9001, "127.0.0.1", "id0000000000000e")...)   // this host
	// TTL missing although the socket reports it: unchecked, dropped
	// unless from this host.
	b.receive(mdnsReceived{data: response(t, serviceRRs("missing", "m.local.", 9000, "10.0.0.11", "id0000000000000f")...), src: src("10.0.0.11:5353"), ttlMissing: true})
	b.receive(mdnsReceived{data: response(t, serviceRRs("local2", "l2.local.", 9002, "127.0.0.1", "id00000000000010")...), src: src("10.0.0.1:5353"), ttlMissing: true})

	var got []string
	for _, c := range b.candidates() {
		got = append(got, c.id)
	}
	slices.Sort(got)
	if want := []string{"id0000000000000c", "id0000000000000d", "id0000000000000e", "id00000000000010"}; !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

// TestMDNSBrowserSendersKeptApart: one sender's records never complete or
// change another sender's instance (e.g. a spoofed SRV port for a real
// peer's instance name).
func TestMDNSBrowserSendersKeptApart(t *testing.T) {
	b := newMDNSBrowser("", testNets())
	real := serviceRRs("nas", "nas.local.", 9000, "10.0.0.7", "id0000000000000c")
	b.handle(response(t, real...), src("10.0.0.7:5353"))
	// The attacker repeats the instance with another port and address.
	fq := "nas." + mdnsServiceName
	b.handle(response(t, fq+" 120 IN SRV 10 1 6379 nas.local.", "nas.local. 120 IN A 10.0.0.66"), src("10.0.0.66:5353"))
	// Records without an instance (an address only) add nothing.
	b.handle(response(t, "nas.local. 120 IN A 10.0.0.66"), src("10.0.0.66:5353"))
	cs := b.candidates()
	if len(cs) != 1 || !slices.Equal(cs[0].urls, []string{"http://10.0.0.7:9000"}) {
		t.Errorf("candidates = %+v", cs)
	}

	// Instances from one sender are bounded.
	var rrs []string
	for i := range mdnsMaxPerSource + 5 {
		rrs = append(rrs, serviceRRs(fmt.Sprintf("i%d", i), "x.local.", 9000, "10.0.0.20", fmt.Sprintf("id%014d", i))...)
	}
	for i := 0; i < len(rrs); i += 16 {
		b.handle(response(t, rrs[i:min(i+16, len(rrs))]...), src("10.0.0.20:5353"))
	}
	if n := b.perSource[netip.MustParseAddr("10.0.0.20")]; n != mdnsMaxPerSource {
		t.Errorf("instances from one sender: %d", n)
	}
}

// TestMDNSBrowserFollowUps: incomplete answers trigger (bounded, one-time)
// follow-up questions, and the answers complete the instance.
func TestMDNSBrowserFollowUps(t *testing.T) {
	b := newMDNSBrowser("", testNets())
	fq := "nas." + mdnsServiceName
	qs := b.handle(response(t, mdnsServiceName+" 120 IN PTR "+fq), src("10.0.0.7:5353"))
	if len(qs) != 2 || qs[0].Qtype != dns.TypeSRV || qs[1].Qtype != dns.TypeTXT || qs[0].Name != fq {
		t.Fatalf("follow-ups = %v", qs)
	}
	if again := b.handle(response(t, mdnsServiceName+" 120 IN PTR "+fq), src("10.0.0.7:5353")); len(again) != 0 {
		t.Errorf("asked twice: %v", again)
	}
	qs = b.handle(response(t, fq+" 120 IN SRV 10 1 9000 nas.local.", fq+` 120 IN TXT "id=id0000000000000c"`), src("10.0.0.7:5353"))
	if len(qs) != 2 || qs[0].Name != "nas.local." || qs[0].Qtype != dns.TypeA || qs[1].Qtype != dns.TypeAAAA {
		t.Fatalf("host follow-ups = %v", qs)
	}
	if len(b.candidates()) != 0 {
		t.Error("candidate without an address")
	}
	b.handle(response(t, "nas.local. 120 IN A 10.0.0.7"), src("10.0.0.7:5353"))
	if cs := b.candidates(); len(cs) != 1 || !slices.Equal(cs[0].urls, []string{"http://10.0.0.7:9000"}) {
		t.Errorf("candidates = %+v", cs)
	}
	// Queries are not responses.
	if qs := b.handle(query(t, 1, q(mdnsServiceName, dns.TypePTR)), src("10.0.0.7:5353")); len(qs) != 0 {
		t.Error("query handled as a response")
	}
}

func TestLocalNetsBroadcast(t *testing.T) {
	l := &localNets{load: func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("10.0.0.1/24"), netip.MustParsePrefix("192.168.7.9/30"), netip.MustParsePrefix("172.16.0.1/32")}
	}}
	for s, want := range map[string]bool{
		"10.0.0.255": true, "192.168.7.11": true, "255.255.255.255": true, "::ffff:10.0.0.255": true,
		"10.0.0.254": false, "10.0.1.255": false, "192.168.7.10": false, "172.16.0.1": false, "fe80::ff": false,
	} {
		if got := l.isBroadcast(netip.MustParseAddr(s)); got != want {
			t.Errorf("isBroadcast(%s) = %v", s, got)
		}
	}
}

// TestMDNSAdvertiserSockets runs the advertiser's responder on a real
// loopback socket: a legacy query with 200 copies of the PTR question (the
// audit's amplification probe) gets a single answer no larger than the one
// for a single question.
func TestMDNSAdvertiserSockets(t *testing.T) {
	defer checkLeaks(t)()
	ips := interfaceIPs(true)
	if len(ips) == 0 {
		t.Skip("no non-loopback interface")
	}
	info := testInfo("b3f1c2d4e5f60718", "nas")
	info.Endpoints.All = []string{baseURL("http", ips[0].String(), 9000)}
	var addr netip.AddrPort
	a := &mdnsAdvertiser{
		self:   func() Info { return info },
		logger: testLogger(t),
		open: func(l *slog.Logger) ([]*mdnsSocket, error) {
			c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				return nil, err
			}
			addr = c.LocalAddr().(*net.UDPAddr).AddrPort()
			return []*mdnsSocket{newMDNSSocket(c, false, nil, l)}, nil
		},
	}
	a.refresh()
	defer a.shutdown()
	if len(a.socks) != 1 {
		t.Fatal("responder socket not opened")
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ask := func(n int) []byte {
		qs := make([]dns.Question, n)
		for i := range qs {
			qs[i] = q(mdnsServiceName, dns.TypePTR)
		}
		if _, err := c.WriteToUDPAddrPort(query(t, uint16(n), qs...), addr); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, mdnsMaxPacket)
		n2, _, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatalf("no answer to %d questions: %v", n, err)
		}
		return buf[:n2]
	}
	one := ask(1)
	many := ask(200)
	if len(many) > len(one) || len(many) > mdnsMaxResponse {
		t.Errorf("200 questions: %d bytes, 1 question: %d bytes", len(many), len(one))
	}
	m := unpack(t, many)
	if m.Id != 200 || len(m.Answer) != 1 || len(m.Question) != 1 {
		t.Errorf("answer to 200 questions: id %d, %d answers, %d questions", m.Id, len(m.Answer), len(m.Question))
	}
}
