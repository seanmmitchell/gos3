package discovery

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// mechSnapshot returns the running mechanisms by name.
func mechSnapshot(s *Service) map[string]*mechanism {
	s.life.Lock()
	defer s.life.Unlock()
	out := make(map[string]*mechanism, len(s.mechs))
	for k, v := range s.mechs {
		out[k] = v
	}
	return out
}

func mechNames(m map[string]*mechanism) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestStartStopIdempotent(t *testing.T) {
	defer checkLeaks(t)()
	g := &fakeGroup{}
	cfg := DefaultConfig()
	cfg.MDNS = false
	s := New(cfg, func() Info { return testInfo("self000000000001", "self") }, testLogger(t))
	s.openBeacon = g.opener(t)

	s.Stop() // stop before start is a no-op
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := mechSnapshot(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !maps(first, mechSnapshot(s)) {
		t.Error("second Start restarted mechanisms")
	}
	if got := mechNames(first); !slices.Equal(got, []string{mechBeacon, mechCore}) {
		t.Errorf("mechanisms = %v", got)
	}
	s.Stop()
	s.Stop()
	if len(mechSnapshot(s)) != 0 {
		t.Error("mechanisms left after Stop")
	}
	// Restart after Stop works.
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Stop()
}

func maps(a, b map[string]*mechanism) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestStartContextCancel(t *testing.T) {
	defer checkLeaks(t)()
	g := &fakeGroup{}
	cfg := DefaultConfig()
	cfg.MDNS = false
	s := New(cfg, func() Info { return testInfo("self000000000001", "self") }, testLogger(t))
	s.openBeacon = g.opener(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	// All goroutines exit with the context even without Stop ...
	for name, m := range mechSnapshot(s) {
		select {
		case <-m.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("mechanism %s still running after ctx cancel", name)
		}
	}
	// ... and Start can be called again with a fresh context.
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mechSnapshot(s)) != 2 {
		t.Error("restart did not start mechanisms")
	}
	s.Stop()
}

func TestStartInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BeaconGroup = "nonsense"
	s := New(cfg, nil, testLogger(t))
	if err := s.Start(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v", err)
	}
	s.Stop()
}

func TestReconfigure(t *testing.T) {
	defer checkLeaks(t)()
	g := &fakeGroup{}
	seed := newGos3Server(t, "seed000000000001", "seed")
	cfg := DefaultConfig()
	cfg.MDNS = false
	s := New(cfg, func() Info { return testInfo("self000000000001", "self") }, testLogger(t))
	s.openBeacon = g.opener(t)

	// Reconfigure before Start only stores the config.
	cfg.Seeds = []string{seed.URL}
	if err := s.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	if len(mechSnapshot(s)) != 0 {
		t.Fatal("Reconfigure started mechanisms before Start")
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	m1 := mechSnapshot(s)
	if got := mechNames(m1); !slices.Equal(got, []string{mechBeacon, mechCore, mechSeeds}) {
		t.Fatalf("mechanisms = %v", got)
	}
	eventually(t, 3*time.Second, "seed verified", func() bool {
		return slices.ContainsFunc(s.Peers(), func(p Peer) bool { return p.ID == "seed000000000001" && p.Verified })
	})

	// Unrelated settings restart nothing.
	cfg.AllowScan = false
	cfg.PeerTTL = 5 * time.Minute
	cfg.MaxScanHosts = 10
	if err := s.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	if !maps(m1, mechSnapshot(s)) {
		t.Error("unrelated change restarted mechanisms")
	}
	if got := s.Config(); got.AllowScan || got.PeerTTL != 5*time.Minute {
		t.Errorf("config not applied: %+v", got)
	}

	// Beacon interval change restarts only the beacon.
	cfg.BeaconInterval = 20 * time.Second
	_ = s.Reconfigure(cfg)
	m2 := mechSnapshot(s)
	if m2[mechBeacon] == m1[mechBeacon] || m2[mechCore] != m1[mechCore] || m2[mechSeeds] != m1[mechSeeds] {
		t.Error("beacon change: wrong mechanisms restarted")
	}
	select {
	case <-m1[mechBeacon].done:
	default:
		t.Error("old beacon still running")
	}

	// Seed change restarts only seeds; adding DNS SRV starts it.
	cfg.Seeds = append(cfg.Seeds, "http://127.0.0.1:1")
	cfg.DNSSRV = []string{"example.invalid"}
	s.lookupSRV = func(context.Context, string, string, string) (string, []*net.SRV, error) {
		return "", nil, errors.New("no such host")
	}
	_ = s.Reconfigure(cfg)
	m3 := mechSnapshot(s)
	if m3[mechSeeds] == m2[mechSeeds] || m3[mechBeacon] != m2[mechBeacon] || m3[mechCore] != m2[mechCore] || m3[mechDNSSRV] == nil {
		t.Errorf("seed change: %v", mechNames(m3))
	}

	// Disabling beacon and seeds stops them.
	cfg.Beacon = false
	cfg.Seeds = nil
	cfg.DNSSRV = nil
	_ = s.Reconfigure(cfg)
	if got := mechNames(mechSnapshot(s)); !slices.Equal(got, []string{mechCore}) {
		t.Errorf("after disable: %v", got)
	}

	// Invalid configs are rejected and leave the active config untouched.
	bad := cfg
	bad.BeaconInterval = -1
	if err := s.Reconfigure(bad); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("invalid reconfigure: %v", err)
	}
	if s.Config().BeaconInterval != 20*time.Second {
		t.Error("invalid config applied")
	}

	// Disabling discovery stops everything.
	cfg.Enabled = false
	_ = s.Reconfigure(cfg)
	if len(mechSnapshot(s)) != 0 {
		t.Error("mechanisms running while disabled")
	}
	// Config() returns a copy.
	c := s.Config()
	c.Seeds = append(c.Seeds, "mutated")
	if slices.Contains(s.Config().Seeds, "mutated") {
		t.Error("Config() aliases internal state")
	}
	s.Stop()
	seed.Close()
}

func TestSeedsNeverExpire(t *testing.T) {
	defer checkLeaks(t)()
	seed := newGos3Server(t, "seed000000000001", "seed")
	clk := newFakeClock()
	cfg := quietConfig()
	cfg.Seeds = []string{seed.hostPort()} // normalised to http://
	s := New(cfg, nil, testLogger(t))
	s.now = clk.Now
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "seed verified", func() bool { return len(s.Peers()) == 1 })
	p := s.Peers()[0]
	if !slices.Equal(p.Sources, []Source{SourceStatic}) || p.Addresses[0] != seed.URL {
		t.Errorf("seed peer = %+v", p)
	}
	s.Stop()

	// Seed goes away: re-probe marks it unhealthy but it is never removed.
	seed.Close()
	clk.Advance(time.Hour)
	s.maintain(context.Background())
	got := s.Peers()
	if len(got) != 1 || got[0].Healthy || got[0].Error == "" {
		t.Fatalf("after seed down: %+v", got)
	}
}

func TestMaintainReprobeAndExpiry(t *testing.T) {
	defer checkLeaks(t)()
	a := newGos3Server(t, "aaaa000000000001", "alpha")
	clk := newFakeClock()
	s := New(quietConfig(), nil, testLogger(t))
	s.now = clk.Now
	if _, err := s.Probe(context.Background(), a.URL); err != nil {
		t.Fatal(err)
	}
	hits := a.Hits()
	// Not due yet.
	clk.Advance(10 * time.Second)
	s.maintain(context.Background())
	if a.Hits() != hits {
		t.Error("re-probed too early")
	}
	// Due: successful re-probe refreshes LastSeen.
	clk.Advance(reprobeEvery)
	s.maintain(context.Background())
	if a.Hits() != hits+1 {
		t.Errorf("hits = %d, want %d", a.Hits(), hits+1)
	}
	p := s.Peers()[0]
	if !p.LastSeen.Equal(clk.Now()) || !p.Healthy {
		t.Errorf("after re-probe: %+v", p)
	}
	// The address now serves a different server: unhealthy.
	a.mu.Lock()
	a.info.ID = "other00000000001"
	a.mu.Unlock()
	clk.Advance(reprobeEvery)
	s.maintain(context.Background())
	p, _ = s.table.get("aaaa000000000001")
	if p.Healthy || !strings.Contains(p.Error, "other00000000001") {
		t.Errorf("after id change: %+v", p)
	}
	// Not seen for the TTL (60 s default): removed.
	clk.Advance(DefaultConfig().PeerTTL)
	s.maintain(context.Background())
	if _, ok := s.table.get("aaaa000000000001"); ok {
		t.Error("stale peer not expired")
	}
	a.Close()
}

func TestDNSSRV(t *testing.T) {
	defer checkLeaks(t)()
	a := newGos3Server(t, "aaaa000000000001", "alpha")
	cfg := quietConfig()
	cfg.DNSSRV = []string{"example.com"}
	s := New(cfg, nil, testLogger(t))
	var mu sync.Mutex
	var asked []string
	s.lookupSRV = func(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
		mu.Lock()
		asked = append(asked, service+"/"+proto+"/"+name)
		mu.Unlock()
		return "_gos3._tcp." + name, []*net.SRV{
			{Target: "127.0.0.1.", Port: uint16(a.port(t))},
			{Target: "s3.example.com.", Port: 443},
			{Target: ".", Port: 9000},
			nil,
		}, nil
	}
	cands := s.resolveSRV(context.Background(), []string{"example.com"})
	var urls []string
	for _, c := range cands {
		urls = append(urls, c.urls...)
		if c.source != SourceDNS {
			t.Errorf("source = %v", c.source)
		}
	}
	if !slices.Equal(urls, []string{a.URL, "https://s3.example.com:443"}) {
		t.Errorf("urls = %v", urls)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "SRV target verified", func() bool {
		return slices.ContainsFunc(s.Peers(), func(p Peer) bool { return p.ID == "aaaa000000000001" && slices.Contains(p.Sources, SourceDNS) })
	})
	s.Stop()
	mu.Lock()
	if len(asked) == 0 || asked[0] != "gos3/tcp/example.com" {
		t.Errorf("lookups = %v", asked)
	}
	mu.Unlock()
	a.Close()
}

// TestServiceBeaconDiscovery runs two services that find each other through
// the beacon (simulated group on 127.0.0.1) and verify each other over HTTP.
func TestServiceBeaconDiscovery(t *testing.T) {
	defer checkLeaks(t)()
	g := &fakeGroup{}
	srvA := newGos3Server(t, "aaaa000000000001", "alpha")
	srvB := newGos3Server(t, "bbbb000000000002", "bravo")
	cfg := DefaultConfig()
	cfg.MDNS = false
	a := New(cfg, srvA.Info, testLogger(t))
	b := New(cfg, srvB.Info, testLogger(t))
	a.openBeacon = g.opener(t)
	b.openBeacon = g.opener(t)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	verified := func(s *Service, id string) func() bool {
		return func() bool {
			return slices.ContainsFunc(s.Peers(), func(p Peer) bool { return p.ID == id && p.Verified && p.Healthy })
		}
	}
	eventually(t, 5*time.Second, "B verifies A", verified(b, srvA.Info().ID))
	eventually(t, 5*time.Second, "A verifies B", verified(a, srvB.Info().ID))
	pa := b.Peers()[0]
	if !slices.Equal(pa.Sources, []Source{SourceBeacon}) || pa.Name != "alpha" || !slices.Equal(pa.Addresses, []string{srvA.URL}) {
		t.Errorf("B's view of A: %+v", pa)
	}
	if len(a.Peers()) != 1 || len(b.Peers()) != 1 {
		t.Errorf("self included: %v / %v", ids(a.Peers()), ids(b.Peers()))
	}

	// B stops: bye marks it unhealthy at A.
	b.Stop()
	eventually(t, 3*time.Second, "A marks B unhealthy", func() bool {
		ps := a.Peers()
		return len(ps) == 1 && !ps[0].Healthy && ps[0].Error != ""
	})
	a.Stop()
	srvA.Close()
	srvB.Close()
}

func TestBrowse(t *testing.T) {
	defer checkLeaks(t)()
	// Seeds only (no multicast needed).
	seed := newGos3Server(t, "seed000000000001", "seed")
	cfg := DefaultConfig()
	cfg.MDNS, cfg.Beacon = false, false
	cfg.Seeds = []string{seed.URL, "http://127.0.0.1:1"}
	peers, err := Browse(context.Background(), cfg, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(peers), []string{"seed000000000001"}) || !peers[0].Verified {
		t.Fatalf("peers = %+v", peers)
	}
	cfg.BeaconGroup = "bad"
	if _, err := Browse(context.Background(), cfg, 0); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("invalid config: %v", err)
	}

	// Beacon query answered by a running server (simulated group).
	g := &fakeGroup{}
	srvA := newGos3Server(t, "aaaa000000000001", "alpha")
	acfg := DefaultConfig()
	acfg.MDNS = false
	a := New(acfg, srvA.Info, testLogger(t))
	a.openBeacon = g.opener(t)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	bcfg := DefaultConfig()
	bcfg.MDNS = false
	br := New(bcfg, nil, testLogger(t))
	br.openBeacon = g.opener(t)
	peers, err = br.browse(context.Background(), 800*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(peers), []string{"aaaa000000000001"}) || !slices.Equal(peers[0].Sources, []Source{SourceBeacon}) {
		t.Fatalf("browse peers = %+v", peers)
	}
	a.Stop()
	srvA.Close()
	seed.Close()
}

func TestMDNSNames(t *testing.T) {
	if got := mdnsInstanceName("My NAS (home)!", "b3f1c2d4e5f60718"); got != "My-NAS--home-b3f1c2" {
		t.Errorf("instance = %q", got)
	}
	if got := mdnsInstanceName("", "abc"); got != "gos3-abc" {
		t.Errorf("instance = %q", got)
	}
	if got := mdnsInstanceName(strings.Repeat("x", 80), "b3f1c2d4e5f60718"); got != strings.Repeat("x", 50)+"-b3f1c2" {
		t.Errorf("instance = %q", got)
	}
	if h := mdnsHostName("abcdef"); !strings.HasSuffix(h, ".local.") || strings.Count(h, ".") != 2 {
		t.Errorf("host = %q", h)
	}
}

func TestBuildMDNSZone(t *testing.T) {
	info := testInfo("b3f1c2d4e5f60718", "nas")
	info.Endpoints.S3 = "https://10.0.0.5:9443"
	if len(interfaceIPs(true)) == 0 {
		if _, _, err := buildMDNSZone(info); err == nil {
			t.Error("zone built without addresses")
		}
		t.Skip("no non-loopback interface")
	}
	svc, key, err := buildMDNSZone(info)
	if err != nil {
		t.Fatal(err)
	}
	if svc.Port != 9443 || svc.Instance != "nas-b3f1c2" || svc.Service != MDNSService {
		t.Errorf("svc = %+v", svc)
	}
	for _, want := range []string{"id=b3f1c2d4e5f60718", "name=nas", "v=1", "api=v1", "region=r", "scheme=https", "info=/_gos3/info"} {
		if !slices.Contains(svc.TXT, want) {
			t.Errorf("TXT %v missing %q", svc.TXT, want)
		}
	}
	info.Name = "renamed"
	_, key2, _ := buildMDNSZone(info)
	if key == key2 {
		t.Error("key did not change with the name")
	}
	if _, _, err := buildMDNSZone(Info{ID: "x"}); err == nil {
		t.Error("zone without endpoint built")
	}
}

func TestMDNSCandidate(t *testing.T) {
	e := &mdnsEntry{
		name:   "nas-b3f1c2._gos3._tcp.local.",
		port:   9000,
		txt:    []string{"id=b3f1c2d4e5f60718", "name=nas", "v=0.1.0", "region=eu", "scheme=http", "junk"},
		hasSRV: true,
		hasTXT: true,
		zone:   "eth0",
	}
	addrs := []netip.Addr{netip.MustParseAddr("fe80::1"), netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("10.0.0.6")}
	c, ok := e.candidate(addrs, "self")
	if !ok || c.id != "b3f1c2d4e5f60718" || c.name != "nas" || c.version != "0.1.0" || c.region != "eu" || c.source != SourceMDNS {
		t.Fatalf("candidate = %+v %v", c, ok)
	}
	if !slices.Equal(c.urls, []string{"http://10.0.0.5:9000", "http://[fe80::1%25eth0]:9000"}) {
		t.Errorf("urls = %v", c.urls)
	}
	e.txt = []string{"scheme=https"}
	c, ok = e.candidate(addrs[1:2], "self")
	if !ok || c.id != "" || c.name != "nas-b3f1c2" || !slices.Equal(c.urls, []string{"https://10.0.0.5:9000"}) {
		t.Errorf("no-id candidate = %+v", c)
	}
	ip := []netip.Addr{netip.MustParseAddr("1.2.3.4")}
	for _, bad := range []struct {
		e     mdnsEntry
		addrs []netip.Addr
	}{
		{mdnsEntry{port: 0, hasSRV: true, hasTXT: true}, ip},
		{mdnsEntry{port: 9000, hasSRV: true, hasTXT: true, txt: []string{"id=self"}}, ip},
		{mdnsEntry{port: 9000, hasSRV: true, hasTXT: true, txt: []string{"id=bad id"}}, ip},
		{mdnsEntry{port: 9000, hasSRV: true, hasTXT: true}, nil},
		{mdnsEntry{port: 9000, hasSRV: true, hasTXT: true}, []netip.Addr{netip.IPv4Unspecified()}},
		{mdnsEntry{port: 9000, hasTXT: true}, ip}, // no SRV
		{mdnsEntry{port: 9000, hasSRV: true}, ip}, // no TXT
	} {
		if c, ok := bad.e.candidate(bad.addrs, "self"); ok {
			t.Errorf("accepted %+v → %+v", bad, c)
		}
	}
}

// TestMDNSAdvertiseAndBrowse uses real mDNS on this host; skipped when
// multicast DNS is unavailable.
func TestMDNSAdvertiseAndBrowse(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	defer checkLeaks(t)()
	info := testInfo(NewServerID(), "mdns-test")
	ips := interfaceIPs(false)
	if len(ips) == 0 {
		t.Skip("no non-loopback IPv4 address")
	}
	info.Endpoints.S3 = baseURL("http", ips[0].String(), 9555)
	adv := &mdnsAdvertiser{self: func() Info { return info }, logger: testLogger(t)}
	adv.refresh()
	if len(adv.socks) == 0 {
		t.Skip("mDNS responder unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); adv.run(ctx) }()
	defer func() { cancel(); <-done }()

	sink := &recordSink{}
	if err := mdnsQuery(context.Background(), 2*time.Second, testLogger(t), "", sink); err != nil {
		t.Skipf("mDNS query failed: %v", err)
	}
	cs, _ := sink.snapshot()
	i := slices.IndexFunc(cs, func(c candidate) bool { return c.id == info.ID })
	if i < 0 {
		t.Skipf("own advertisement not seen (multicast not looped back?): %+v", cs)
	}
	if want := baseURL("http", ips[0].String(), 9555); !slices.Contains(cs[i].urls, want) && len(cs[i].urls) == 0 {
		t.Errorf("urls = %v, want to include %s", cs[i].urls, want)
	}
	if cs[i].name != "mdns-test" {
		t.Errorf("candidate = %+v", cs[i])
	}
}

// TestFullServiceNoLeaks runs every mechanism against the real network stack
// (mDNS, multicast beacon on a private port, seeds, DNS SRV) and checks that
// Stop leaves no goroutines behind.
func TestFullServiceNoLeaks(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	defer checkLeaks(t)()
	srv := newGos3Server(t, "full000000000001", "full")
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	cfg := DefaultConfig()
	cfg.BeaconGroup = "239.255.73.83:" + strconv.Itoa(port)
	cfg.Seeds = []string{"http://127.0.0.1:1"}
	cfg.DNSSRV = []string{"example.invalid"}
	s := New(cfg, srv.Info, testLogger(t))
	s.lookupSRV = func(context.Context, string, string, string) (string, []*net.SRV, error) {
		return "", nil, errors.New("no such host")
	}
	for range 2 {
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		cfg.BeaconInterval = cfg.BeaconInterval + time.Second
		if err := s.Reconfigure(cfg); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		s.Stop()
		if d := time.Since(start); d > 4*time.Second {
			t.Errorf("Stop took %v", d)
		}
	}
	srv.Close()
}
