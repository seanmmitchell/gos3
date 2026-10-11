package discovery

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// impostor starts a fake gos3 server that claims id with its own name and
// endpoints pointing elsewhere.
func impostor(t *testing.T, id string) *gos3Server {
	t.Helper()
	g := newGos3Server(t, id, "nas")
	g.mu.Lock()
	g.info.Endpoints.S3 = "https://evil.example"
	g.info.Endpoints.All = []string{"https://evil.example"}
	g.mu.Unlock()
	return g
}

// TestPeerTakeoverRejected: a server at a new address claiming a verified
// peer's ID (via the admin probe, a beacon sighting or a scan) must not
// replace the verified record while the peer still answers at its known
// address; the claim is recorded as a conflict.
func TestPeerTakeoverRejected(t *testing.T) {
	defer checkLeaks(t)()
	const id = "victim0000000001"
	real := newGos3Server(t, id, "nas")
	evil := impostor(t, id)
	cfg := quietConfig()
	cfg.PeerTTL = time.Hour
	s := New(cfg, nil, testLogger(t))
	clk := newFakeClock()
	s.now = clk.Now

	if _, err := s.Probe(context.Background(), real.URL); err != nil {
		t.Fatal(err)
	}
	check := func(step string) {
		t.Helper()
		p, ok := s.table.get(id)
		if !ok || p.Endpoints.S3 != real.URL || !slices.Equal(p.Addresses, []string{real.URL}) || !p.Healthy {
			t.Fatalf("%s: verified peer taken over: %+v", step, p)
		}
		if !strings.Contains(p.Error, "ID claimed by another address") || !strings.Contains(p.Error, evil.URL) {
			t.Errorf("%s: conflict not recorded: %q", step, p.Error)
		}
		if d := s.table.due(clk.Now().Add(time.Hour), reprobeEvery); len(d) != 1 || !slices.Equal(d[0].urls, []string{real.URL}) {
			t.Errorf("%s: re-probe targets = %+v", step, d)
		}
	}

	// 1. Admin probe of the impostor: the caller sees what the URL serves,
	// the table keeps the verified peer. The real peer answered moments
	// ago, so it is not re-probed.
	hits := real.Hits()
	p, err := s.Probe(context.Background(), evil.URL)
	if err != nil {
		t.Fatal(err)
	}
	if p.Endpoints.S3 != "https://evil.example" || !slices.Equal(p.Addresses, []string{evil.URL}) {
		t.Errorf("probe result = %+v", p)
	}
	check("probe")
	if real.Hits() != hits {
		t.Error("freshly verified peer re-probed")
	}

	// 2. A beacon sighting from the impostor's address, later: the known
	// address is re-probed first.
	clk.Advance(time.Minute)
	hits = real.Hits()
	s.verify(context.Background(), candidate{id: id, source: SourceBeacon, urls: []string{evil.URL}}, time.Second)
	check("beacon")
	if real.Hits() != hits+1 {
		t.Errorf("known address re-probed %d times, want 1", real.Hits()-hits)
	}

	// 3. A scan finding the impostor reports it as found there, without
	// mixing it with the verified record.
	clk.Advance(time.Minute)
	res, err := s.Scan(context.Background(), ScanRequest{Targets: []string{evil.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Found) != 1 || res.Found[0].Endpoints.S3 != "https://evil.example" || slices.Contains(res.Found[0].Addresses, real.URL) {
		t.Errorf("scan found = %+v", res.Found)
	}
	check("scan")
	if p, _ := s.table.get(id); !strings.Contains(p.Error, "(3 claims)") {
		t.Errorf("claims not counted: %q", p.Error)
	}

	// The conflict stays visible across successful re-probes for a while.
	clk.Advance(time.Minute)
	s.maintain(context.Background())
	if p, _ := s.table.get(id); !p.Healthy || !strings.Contains(p.Error, "claimed") {
		t.Errorf("after re-probe: %+v", p)
	}
	clk.Advance(conflictHold)
	s.maintain(context.Background())
	if p, _ := s.table.get(id); !p.Healthy || p.Error != "" {
		t.Errorf("conflict not cleared after %v: %+v", conflictHold, p)
	}
	s.Stop()
}

// TestPeerMoved: when the known addresses no longer answer, the peer is
// moved to the new address (a legitimate address change).
func TestPeerMoved(t *testing.T) {
	defer checkLeaks(t)()
	const id = "mover00000000001"
	old := newGos3Server(t, id, "nas")
	s := New(quietConfig(), nil, testLogger(t))
	clk := newFakeClock()
	s.now = clk.Now
	if _, err := s.Probe(context.Background(), old.URL); err != nil {
		t.Fatal(err)
	}
	old.Close()
	moved := newGos3Server(t, id, "nas-renamed")

	// Within conflictFresh the old address still counts as alive.
	if _, err := s.Probe(context.Background(), moved.URL); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.table.get(id); p.Name != "nas" {
		t.Fatalf("moved before the old address was re-probed: %+v", p)
	}

	clk.Advance(conflictFresh)
	s.verify(context.Background(), candidate{id: id, source: SourceBeacon, urls: []string{moved.URL}}, time.Second)
	p, _ := s.table.get(id)
	if p.Name != "nas-renamed" || p.Endpoints.S3 != moved.URL || !slices.Equal(p.Addresses, []string{moved.URL}) || !p.Healthy || p.Error != "" {
		t.Fatalf("after move: %+v", p)
	}
	if !slices.Contains(p.Sources, SourceBeacon) || !slices.Contains(p.Sources, SourceProbe) {
		t.Errorf("sources = %v", p.Sources)
	}
	if d := s.table.due(clk.Now().Add(time.Hour), reprobeEvery); len(d) != 1 || !slices.Equal(d[0].urls, []string{moved.URL}) {
		t.Errorf("re-probe targets = %+v", d)
	}
	s.Stop()
}

// TestPeerSameServerOtherAddress: the same document at another address (the
// same server reached differently) is not a conflict and changes nothing.
func TestPeerSameServerOtherAddress(t *testing.T) {
	const id = "same000000000001"
	a := newGos3Server(t, id, "nas")
	b := newGos3Server(t, id, "nas")
	b.mu.Lock()
	b.info = a.Info()
	b.mu.Unlock()
	s := New(quietConfig(), nil, testLogger(t))
	if _, err := s.Probe(context.Background(), a.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Probe(context.Background(), b.URL); err != nil {
		t.Fatal(err)
	}
	p, _ := s.table.get(id)
	if p.Error != "" || !slices.Equal(p.Addresses, []string{a.URL}) {
		t.Errorf("peer = %+v", p)
	}
	s.Stop()
}

// TestPeerTableIdentityRules covers which new addresses a verified peer may
// be observed at without a conflict check.
func TestPeerTableIdentityRules(t *testing.T) {
	now := time.Now()
	tb := newPeerTable(time.Hour)
	tb.configure(time.Hour, []string{"http://nas.example:9000"})
	info := testInfo("p", "nas") // advertises http://nas:9000
	if _, res := tb.observeInfo(info, "http://10.0.0.5:9000", SourceBeacon, now, ""); res != observeStored {
		t.Fatal("first verification not stored")
	}
	evil := info
	evil.Name = "evil"
	for _, tc := range []struct {
		addr string
		src  Source
		want observeResult
	}{
		{"http://10.0.0.66:9000", SourceBeacon, observeConflict},
		{"http://10.0.0.66:9000", SourceScan, observeConflict},
		{"http://10.0.0.66:9000", SourceProbe, observeConflict},
		{"http://10.0.0.66:9000", SourceMDNS, observeConflict},
		{"http://10.0.0.66:9000", SourceStatic, observeConflict}, // not a configured seed
		{"http://10.0.0.5:9000", "", observeStored},              // known address (re-probe)
	} {
		if _, res := tb.observeInfo(evil, tc.addr, tc.src, now, ""); res != tc.want {
			t.Errorf("%s via %q: %v, want %v", tc.addr, tc.src, res, tc.want)
		}
	}
	// The known address answered with "evil": that is the peer now.
	if p, _ := tb.get("p"); p.Name != "evil" {
		t.Errorf("known address update not stored: %+v", p)
	}
	for _, tc := range []struct {
		addr string
		src  Source
	}{
		{"http://nas:9000", SourceScan},           // advertised by the verified record
		{"http://nas.example:9000", SourceStatic}, // configured seed
		{"http://pod-0.gos3.svc:9000", SourceDNS}, // DNS SRV target
	} {
		if _, res := tb.observeInfo(info, tc.addr, tc.src, now, ""); res != observeStored {
			t.Errorf("%s via %q: %v, want stored", tc.addr, tc.src, res)
		}
	}
	// conflict() ignores identical documents and unknown peers.
	if tb.conflict(testInfo("unknown", "x"), "http://x:1", now) {
		t.Error("conflict recorded for an unknown peer")
	}
	p, _ := tb.get("p")
	cur := tb.peers["p"].info
	if tb.conflict(cur, "http://10.0.0.77:9000", now) {
		t.Error("identical document recorded as a conflict")
	}
	if !tb.conflict(evil2(cur), "http://10.0.0.77:9000", now) || tb.conflict(evil2(cur), "http://10.0.0.78:9000", now.Add(time.Second)) {
		t.Error("first conflict not reported once")
	}
	if p2, _ := tb.get("p"); p2.Name != p.Name || !slices.Equal(p2.Addresses, p.Addresses) || !strings.Contains(p2.Error, "10.0.0.78") {
		t.Errorf("after conflict: %+v", p2)
	}
	// Known-address targets: last good address first, bounded.
	urls, alive := tb.knownTargets("p", now.Add(time.Hour), conflictFresh)
	if alive || len(urls) == 0 || len(urls) > maxConflictReprobes || urls[0] != tb.peers["p"].probeURL {
		t.Errorf("knownTargets = %v %v", urls, alive)
	}
	if _, alive := tb.knownTargets("p", now, conflictFresh); !alive {
		t.Error("freshly probed peer not considered alive")
	}
}

func evil2(in Info) Info {
	in.Endpoints.S3 = "https://evil.example"
	return in
}
