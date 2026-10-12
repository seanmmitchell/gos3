package discovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseScanTarget(t *testing.T) {
	tests := []struct {
		in   string
		want scanTarget
		err  bool
	}{
		{in: "http://nas:9000/x", want: scanTarget{url: "http://nas:9000"}},
		{in: "https://s3.example.com", want: scanTarget{url: "https://s3.example.com"}},
		{in: "10.0.0.0/24", want: scanTarget{prefix: netip.MustParsePrefix("10.0.0.0/24")}},
		{in: "10.0.0.77/24", want: scanTarget{prefix: netip.MustParsePrefix("10.0.0.0/24")}},
		{in: "fd00::/120", want: scanTarget{prefix: netip.MustParsePrefix("fd00::/120")}},
		{in: "10.0.0.5", want: scanTarget{host: "10.0.0.5"}},
		{in: "10.0.0.5:9001", want: scanTarget{host: "10.0.0.5", port: 9001}},
		{in: "::1", want: scanTarget{host: "::1"}},
		{in: "[::1]:9001", want: scanTarget{host: "::1", port: 9001}},
		{in: "NAS.local", want: scanTarget{host: "nas.local"}},
		{in: "nas.local:9000", want: scanTarget{host: "nas.local", port: 9000}},
		{in: "", err: true},
		{in: "10.0.0.0/33", err: true},
		{in: "nas/24", err: true},
		{in: "nas:0", err: true},
		{in: "nas:x", err: true},
		{in: "bad host", err: true},
		{in: "ftp://x", err: true},
		{in: "-x-", err: true},
	}
	for _, tt := range tests {
		got, err := parseScanTarget(tt.in)
		if tt.err {
			if !errors.Is(err, ErrInvalidScan) {
				t.Errorf("%q: err = %v, want ErrInvalidScan", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%q: got %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
}

func collectAddrs(p netip.Prefix) []string {
	var out []string
	forEachAddr(p, func(a netip.Addr) bool {
		out = append(out, a.String())
		return true
	})
	return out
}

func TestCIDRExpansion(t *testing.T) {
	tests := []struct {
		cidr  string
		count uint64
		first string
		last  string
	}{
		{"10.0.0.0/24", 254, "10.0.0.1", "10.0.0.254"},
		{"10.0.0.0/30", 2, "10.0.0.1", "10.0.0.2"},
		{"10.0.0.0/31", 2, "10.0.0.0", "10.0.0.1"},
		{"10.0.0.9/32", 1, "10.0.0.9", "10.0.0.9"},
		{"127.0.0.1/32", 1, "127.0.0.1", "127.0.0.1"},
		{"192.168.0.0/22", 1022, "192.168.0.1", "192.168.3.254"},
		{"255.255.255.252/30", 2, "255.255.255.253", "255.255.255.254"},
		{"fd00::/126", 4, "fd00::", "fd00::3"},
		{"fd00::1/128", 1, "fd00::1", "fd00::1"},
	}
	for _, tt := range tests {
		st, err := parseScanTarget(tt.cidr)
		if err != nil {
			t.Fatalf("%s: %v", tt.cidr, err)
		}
		n, ok := st.hosts(1 << 20)
		if !ok || n != tt.count {
			t.Errorf("%s: hosts = %d, %v; want %d", tt.cidr, n, ok, tt.count)
		}
		got := collectAddrs(st.prefix)
		if uint64(len(got)) != tt.count || got[0] != tt.first || got[len(got)-1] != tt.last {
			t.Errorf("%s: expanded %d addrs %s..%s; want %d %s..%s", tt.cidr, len(got), got[0], got[len(got)-1], tt.count, tt.first, tt.last)
		}
	}
	// Early stop.
	var n int
	forEachAddr(netip.MustParsePrefix("10.0.0.0/16"), func(netip.Addr) bool { n++; return n < 5 })
	if n != 5 {
		t.Errorf("early stop visited %d", n)
	}
	// Huge prefixes are rejected without expansion.
	for _, c := range []string{"0.0.0.0/0", "::/0", "fd00::/64", "fd00::/65"} {
		st, _ := parseScanTarget(c)
		if _, ok := st.hosts(MaxScanHostsLimit); ok {
			t.Errorf("%s accepted", c)
		}
	}
}

func TestPlanScan(t *testing.T) {
	p, err := planScan(ScanRequest{Targets: []string{"10.0.0.0/24"}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.ports, []int{DefaultPort}) || !slices.Equal(p.schemes, []string{"http"}) ||
		p.timeout != DefaultScanTimeout || p.concurrency != DefaultScanConcurrency || p.hosts != 254 || p.probes != 254 {
		t.Errorf("defaults: %+v", p)
	}

	p, err = planScan(ScanRequest{
		Targets: []string{"10.0.0.0/30", "nas", "nas:1234", "http://x:1", "nas"},
		Ports:   []int{9000, 9001, 9000},
		Schemes: []string{"HTTP", "https"},
		Timeout: time.Hour, Concurrency: 100000,
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	// hosts: 2 + 1 + 1 + 1 (duplicate "nas" skipped); probes: 2*2*2 + 2*2 + 2 + 1
	if p.hosts != 5 || p.probes != 15 || p.timeout != maxScanTimeout || p.concurrency != maxScanConcurrency {
		t.Errorf("plan: hosts=%d probes=%d timeout=%v conc=%d", p.hosts, p.probes, p.timeout, p.concurrency)
	}
	jobs := make(chan string, 100)
	p.generate(context.Background(), jobs)
	close(jobs)
	var urls []string
	for u := range jobs {
		urls = append(urls, u)
	}
	if uint64(len(urls)) != p.probes {
		t.Errorf("generated %d urls, planned %d: %v", len(urls), p.probes, urls)
	}
	for _, want := range []string{"http://10.0.0.1:9000", "https://10.0.0.2:9001", "https://nas:9001", "http://nas:1234", "https://nas:1234", "http://x:1"} {
		if !slices.Contains(urls, want) {
			t.Errorf("missing %s in %v", want, urls)
		}
	}
	if slices.Contains(urls, "http://10.0.0.0:9000") || slices.Contains(urls, "http://10.0.0.3:9000") {
		t.Error("network/broadcast address generated")
	}

	// Limits.
	bad := []ScanRequest{
		{},
		{Targets: []string{"x"}, Ports: []int{0}},
		{Targets: []string{"x"}, Ports: []int{70000}},
		{Targets: []string{"x"}, Schemes: []string{"ftp"}},
		{Targets: []string{"bad target!"}},
		{Targets: make([]string, maxScanTargets+1)},
		{Targets: []string{"x"}, Ports: make([]int, maxScanPorts+1)},
	}
	for i, req := range bad {
		if _, err := planScan(req, 1024); !errors.Is(err, ErrInvalidScan) {
			t.Errorf("bad %d: err = %v", i, err)
		}
	}
	tooMany := []ScanRequest{
		{Targets: []string{"10.0.0.0/21"}},                     // 2046 hosts
		{Targets: []string{"10.0.0.0/22", "10.0.4.0/29"}},      // 1022 + 6
		{Targets: []string{"fd00::/100"}},                      // 2^28
		{Targets: []string{"::/0"}},                            // overflow-safe
		{Targets: []string{"10.0.0.0/23", "10.0.2.0/23", "a"}}, // 510+510+1 = 1021 ok? no: see limit below
	}
	for i, req := range tooMany[:4] {
		if _, err := planScan(req, 1024); !errors.Is(err, ErrTooManyHosts) {
			t.Errorf("tooMany %d: err = %v", i, err)
		}
	}
	if _, err := planScan(tooMany[4], 1021); err != nil {
		t.Errorf("exact limit rejected: %v", err)
	}
	if _, err := planScan(tooMany[4], 1020); !errors.Is(err, ErrTooManyHosts) {
		t.Errorf("limit+1 accepted: %v", err)
	}
	if _, err := planScan(ScanRequest{Targets: []string{"fd00::/118"}}, 1024); err != nil {
		t.Errorf("IPv6 /118 (1024 addrs) rejected: %v", err)
	}
}

func TestScan(t *testing.T) {
	defer checkLeaks(t)()
	a := newGos3Server(t, "aaaa000000000001", "alpha")
	b := newGos3Server(t, "bbbb000000000002", "bravo")
	// Same server ID reachable at two addresses (multi-homed): one result.
	b2 := newGos3Server(t, "bbbb000000000002", "bravo")
	self := newGos3Server(t, "self000000000001", "self")

	cfg := quietConfig()
	s := New(cfg, func() Info { return self.Info() }, testLogger(t))
	res, err := s.Scan(context.Background(), ScanRequest{
		Targets: []string{
			"127.0.0.1/32",        // × ports below
			b2.hostPort(),         // host:port target
			"http://127.0.0.1:1/", // closed port, URL target
			"localhost:" + "1",    // closed
			self.URL,              // ourselves: probed, not reported
		},
		Ports:   []int{a.port(t), b.port(t), 1},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Probed != 3+1+1+1+1 {
		t.Errorf("probed = %d", res.Probed)
	}
	if !slices.Equal(ids(res.Found), []string{"aaaa000000000001", "bbbb000000000002"}) {
		t.Fatalf("found = %+v", res.Found)
	}
	bravo := res.Found[1]
	if !slices.Contains(bravo.Addresses, b.URL) || !slices.Contains(bravo.Addresses, b2.URL) {
		t.Errorf("bravo addresses = %v", bravo.Addresses)
	}
	if !slices.Equal(bravo.Sources, []Source{SourceScan}) || !bravo.Verified {
		t.Errorf("bravo = %+v", bravo)
	}
	if _, err := time.ParseDuration(res.Elapsed); err != nil {
		t.Errorf("elapsed %q: %v", res.Elapsed, err)
	}
	if got := ids(s.Peers()); !slices.Equal(got, []string{"aaaa000000000001", "bbbb000000000002"}) {
		t.Errorf("peers = %v", got)
	}

	// Limits and policy.
	if _, err := s.Scan(context.Background(), ScanRequest{Targets: []string{"10.0.0.0/16"}}); !errors.Is(err, ErrTooManyHosts) {
		t.Errorf("too many: %v", err)
	}
	cfg.MaxScanHosts = 1
	_ = s.Reconfigure(cfg)
	if _, err := s.Scan(context.Background(), ScanRequest{Targets: []string{"127.0.0.1", "127.0.0.2"}}); !errors.Is(err, ErrTooManyHosts) {
		t.Errorf("maxScanHosts=1: %v", err)
	}
	cfg.AllowScan = false
	_ = s.Reconfigure(cfg)
	if _, err := s.Scan(context.Background(), ScanRequest{Targets: []string{"127.0.0.1"}}); !errors.Is(err, ErrScanDisabled) {
		t.Errorf("disabled: %v", err)
	}
	a.Close()
	b.Close()
	b2.Close()
	self.Close()
}

func TestScanCancel(t *testing.T) {
	defer checkLeaks(t)()
	release := make(chan struct{})
	blocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocker.Close()
	defer close(release)
	port := strings.TrimPrefix(blocker.URL, "http://127.0.0.1:")

	s := New(quietConfig(), nil, testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	res, err := s.Scan(ctx, ScanRequest{
		Targets:     []string{blocker.URL, "http://localhost:" + port, "127.0.0.1:" + port, "localhost"},
		Ports:       []int{1, 2, 3},
		Timeout:     10 * time.Second,
		Concurrency: 2,
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancel took %v", d)
	}
	if res.Found == nil || len(res.Found) != 0 || res.Probed > 6 {
		t.Errorf("res = %+v", res)
	}
}

func TestScanDefaultsPortZero(t *testing.T) {
	// A host target with an explicit port ignores Ports.
	a := newGos3Server(t, "aaaa000000000009", "alpha")
	s := New(quietConfig(), nil, testLogger(t))
	res, err := s.Scan(context.Background(), ScanRequest{Targets: []string{"127.0.0.1:" + strconv.Itoa(a.port(t))}, Ports: []int{1, 2, 3}})
	if err != nil || res.Probed != 1 || len(res.Found) != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
