package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBeaconEncodeDecode(t *testing.T) {
	info := testInfo("abcdef0123456789", "nas")
	info.Endpoints.All = append(info.Endpoints.All, "http://10.0.0.5:9000", "http://10.0.0.5:9000/")
	ann := announceFor(info, time.Unix(1760000000, 0))
	b, err := encodeBeacon(ann)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["proto"] != BeaconProto || raw["type"] != "announce" || raw["info"] != InfoPath || raw["ts"].(float64) != 1760000000 {
		t.Errorf("wire = %s", b)
	}
	got, err := decodeBeacon(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != info.ID || got.Name != "nas" || !slices.Equal(got.URLs, []string{"http://nas:9000", "http://10.0.0.5:9000"}) {
		t.Errorf("decoded = %+v", got)
	}

	for _, m := range []beaconMsg{{Type: beaconQuery, Nonce: "r4nd0m"}, {Type: beaconBye, ID: "abc"}} {
		b, err := encodeBeacon(m)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeBeacon(b)
		m.Proto = BeaconProto
		if err != nil || got.Type != m.Type || got.Nonce != m.Nonce || got.ID != m.ID {
			t.Errorf("%s round trip: %+v %v", m.Type, got, err)
		}
	}

	// Encoding drops URLs to stay within the datagram limit.
	big := beaconMsg{Type: beaconAnnounce, ID: "x", Name: strings.Repeat("n", maxFieldLen)}
	for i := range 40 {
		big.URLs = append(big.URLs, fmt.Sprintf("http://very-long-host-name-number-%02d.example.com:9000", i))
	}
	b, err = encodeBeacon(big)
	if err != nil || len(b) > MaxBeaconSize {
		t.Fatalf("big: %d bytes, %v", len(b), err)
	}
	if _, err := decodeBeacon(b); err != nil {
		t.Errorf("big decode: %v", err)
	}
	if _, err := encodeBeacon(beaconMsg{Type: beaconAnnounce, ID: "x", Name: strings.Repeat("n", 2000)}); err == nil {
		t.Error("oversized message without URLs encoded")
	}
}

func TestBeaconDecodeValidation(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		ignored bool // errBeaconIgnored rather than errBeaconInvalid
	}{
		{"empty", ``, false},
		{"not json", `hello`, false},
		{"array", `[]`, false},
		{"trailing", `{"proto":"gos3-beacon/1","type":"query"}x`, false},
		{"two objects", `{"proto":"gos3-beacon/1","type":"query"}{}`, false},
		{"bad utf8", "{\"proto\":\"gos3-beacon/1\",\"type\":\"query\",\"nonce\":\"\xff\"}", false},
		{"wrong field type", `{"proto":"gos3-beacon/1","type":"announce","id":"a","urls":"x"}`, false},
		{"announce no id", `{"proto":"gos3-beacon/1","type":"announce"}`, false},
		{"announce bad id", `{"proto":"gos3-beacon/1","type":"announce","id":"a b"}`, false},
		{"bye no id", `{"proto":"gos3-beacon/1","type":"bye"}`, false},
		{"long nonce", `{"proto":"gos3-beacon/1","type":"query","nonce":"` + strings.Repeat("n", 200) + `"}`, false},
		{"oversized", `{"proto":"gos3-beacon/1","type":"query","pad":"` + strings.Repeat("p", MaxBeaconSize) + `"}`, false},
		{"other proto", `{"proto":"gos3-beacon/2","type":"announce","id":"a"}`, true},
		{"no proto", `{"type":"announce","id":"a"}`, true},
		{"unknown type", `{"proto":"gos3-beacon/1","type":"hello"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeBeacon([]byte(tt.data))
			want := errBeaconInvalid
			if tt.ignored {
				want = errBeaconIgnored
			}
			if !errors.Is(err, want) {
				t.Errorf("err = %v, want %v", err, want)
			}
		})
	}
	// Extra unknown fields are tolerated; URL lists are truncated.
	var urls []string
	for i := range 40 {
		urls = append(urls, fmt.Sprintf(`"http://h%d:1"`, i))
	}
	m, err := decodeBeacon([]byte(`{"proto":"gos3-beacon/1","type":"announce","id":"a","future":true,"name":"x\u0000y","urls":[` + strings.Join(urls, ",") + `]}`))
	if err != nil || len(m.URLs) != maxBeaconURLs || m.Name != "xy" {
		t.Errorf("tolerant decode: %+v %v", m, err)
	}
}

func loopbackUDP(t testing.TB) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestBeaconRoundTrip exercises announce, query → unicast reply and bye
// between two beacons connected through a simulated group on 127.0.0.1.
func TestBeaconRoundTrip(t *testing.T) {
	defer checkLeaks(t)()
	g := &fakeGroup{}
	open := g.opener(t)
	infoA := testInfo("aaaaaaaaaaaaaaaa", "alpha")
	infoA.Endpoints = Endpoints{S3: "http://0.0.0.0:9123", All: []string{"http://127.0.0.1:9123"}}
	sinkA, sinkB := &recordSink{}, &recordSink{}
	a, err := open(nil, beaconOptions{interval: time.Hour, advertise: true, query: true, selfInfo: func() Info { return infoA }, sink: sinkA, logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	go func() { defer close(doneA); a.run(ctxA) }()

	// B starts later (missing A's start-up announce), browses only.
	time.Sleep(50 * time.Millisecond)
	b, err := open(nil, beaconOptions{interval: time.Hour, query: true, sink: sinkB, logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan struct{})
	go func() { defer close(doneB); b.run(ctxB) }()

	// B's query is answered by A with a unicast announce within 500 ms.
	eventually(t, 3*time.Second, "B receives A's announce", func() bool {
		cs, _ := sinkB.snapshot()
		return slices.ContainsFunc(cs, func(c candidate) bool { return c.id == infoA.ID })
	})
	cs, _ := sinkB.snapshot()
	c := cs[slices.IndexFunc(cs, func(c candidate) bool { return c.id == infoA.ID })]
	// Wildcard S3 URL replaced by the (loopback) source address; loopback URL kept.
	if c.source != SourceBeacon || c.name != "alpha" || !slices.Equal(c.urls, []string{"http://127.0.0.1:9123"}) {
		t.Errorf("candidate = %+v", c)
	}

	// A ignores its own announces and B's query never makes B answer.
	if cs, _ := sinkA.snapshot(); len(cs) != 0 {
		t.Errorf("A recorded candidates: %+v", cs)
	}

	// Stopping A sends a bye.
	cancelA()
	<-doneA
	eventually(t, 2*time.Second, "B receives bye", func() bool {
		_, byes := sinkB.snapshot()
		return slices.Contains(byes, infoA.ID)
	})
	cancelB()
	<-doneB
}

// TestBeaconQueryFlood checks that replies to a flood of queries are bounded
// and that hostile datagrams are dropped without side effects.
func TestBeaconQueryFlood(t *testing.T) {
	defer checkLeaks(t)()
	info := testInfo("aaaaaaaaaaaaaaaa", "alpha")
	sink := &recordSink{}
	uc, mc := loopbackUDP(t), loopbackUDP(t)
	b := newBeacon(beaconOptions{interval: time.Hour, advertise: true, selfInfo: func() Info { return info }, sink: sink, logger: testLogger(t)},
		uc, mc, func() []net.Addr { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.run(ctx) }()

	querier := loopbackUDP(t)
	defer querier.Close()
	q, _ := encodeBeacon(beaconMsg{Type: beaconQuery, Nonce: "n"})
	for range 200 {
		_, _ = querier.WriteTo(q, mc.LocalAddr())
	}
	hostile := [][]byte{
		[]byte(`{"proto":"gos3-beacon/1","type":"announce","id":"bad id"}`),
		[]byte(strings.Repeat("x", 1500)),
		{0xff, 0xfe, 0x00},
		[]byte(`{"proto":"gos3-beacon/1","type":"bye","id":"aaaaaaaaaaaaaaaa"}`), // our own id
	}
	for _, h := range hostile {
		_, _ = querier.WriteTo(h, mc.LocalAddr())
	}
	replies := 0
	buf := make([]byte, 2048)
	for {
		_ = querier.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
		n, _, err := querier.ReadFrom(buf)
		if err != nil {
			break
		}
		m, err := decodeBeacon(buf[:n])
		if err != nil || m.Type != beaconAnnounce || m.ID != info.ID {
			t.Fatalf("unexpected reply %q: %v", buf[:n], err)
		}
		replies++
	}
	if replies == 0 || replies > maxPendingReplies+5 {
		t.Errorf("replies = %d, want 1..~%d", replies, maxPendingReplies)
	}
	cancel()
	<-done
	if cs, byes := sink.snapshot(); len(cs) != 0 || len(byes) != 0 {
		t.Errorf("hostile datagrams produced %v %v", cs, byes)
	}
}

// TestBeaconMulticast uses the real multicast path on this host; it is
// skipped where the OS or network refuses multicast (e.g. containers).
func TestBeaconMulticast(t *testing.T) {
	defer checkLeaks(t)()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	group, err := parseBeaconGroup(fmt.Sprintf("239.255.73.83:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	var mcErr error
	infoA := testInfo("aaaaaaaaaaaaaaaa", "alpha")
	sinkA, sinkB := &recordSink{}, &recordSink{}
	a, err := openBeacon(group, beaconOptions{interval: time.Hour, advertise: true, selfInfo: func() Info { return infoA }, sink: sinkA, logger: testLogger(t)}, func(err error) { mcErr = err })
	if err != nil {
		t.Skipf("UDP unavailable: %v", err)
	}
	b, err := openBeacon(group, beaconOptions{interval: time.Hour, query: true, sink: sinkB, logger: testLogger(t)}, func(err error) { mcErr = err })
	if err != nil {
		a.close()
		t.Skipf("UDP unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() { defer close(doneA); a.run(ctx) }()
	go func() { defer close(doneB); b.run(ctx) }()
	defer func() {
		cancel()
		<-doneA
		<-doneB
	}()
	if mcErr != nil || a.mconn == nil || b.mconn == nil {
		t.Skipf("multicast unavailable: %v", mcErr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cs, _ := sinkB.snapshot()
		if slices.ContainsFunc(cs, func(c candidate) bool { return c.id == infoA.ID }) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Skip("multicast datagrams not delivered on this host")
}

func TestJitter(t *testing.T) {
	d := 10 * time.Second
	for range 1000 {
		j := jitter(d)
		if j < 9*time.Second || j > 11*time.Second {
			t.Fatalf("jitter(%v) = %v", d, j)
		}
	}
	if jitter(0) != 0 {
		t.Fatal("jitter(0) != 0")
	}
}
