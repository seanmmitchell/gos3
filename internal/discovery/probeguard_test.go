package discovery

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestBlockedProbeAddr(t *testing.T) {
	for _, s := range []string{
		"169.254.169.254", "169.254.170.2", "169.254.170.23", "fd00:ec2::254", "100.100.100.200",
		"169.254.0.23", "168.63.129.16", "192.0.0.192", "fd20:ce::254", "::ffff:168.63.129.16",
		"::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "fd00:ec2::254%eth0",
		"fd00:ec2::23", "fd00:ec2::23%eth0", "::ffff:169.254.170.23", "64:ff9b::a9fe:aa17",
		"64:ff9b:1::a9fe:a9fe", "64:ff9b:1:abcd::a9fe:a9fe", "64:ff9b:1::c000:c0",
		"::169.254.169.254", "::a9fe:a9fe", "::168.63.129.16",
		"0.0.0.0", "0.1.2.3", "::", "::ffff:0.0.0.0",
		"224.0.0.251", "239.255.73.83", "ff02::fb", "ff05::1",
		"255.255.255.255",
	} {
		if !blockedProbeAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s not blocked", s)
		}
	}
	for _, s := range []string{
		"127.0.0.1", "::1", "10.0.0.5", "192.168.1.20", "169.254.10.20", "169.254.169.253",
		"fe80::1%eth0", "fd00:ec2::253", "100.100.100.201", "8.8.8.8", "2001:db8::1", "10.0.0.255",
		"fd20:ce::253", "168.63.129.17", "64:ff9b:1::a00:5", "64:ff9b:2::a9fe:a9fe",
		"::10.0.0.5", "::1:a9fe:a9fe", "fd00:ec2::22", "169.254.170.22",
	} {
		if blockedProbeAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked", s)
		}
	}
	if blockedProbeAddr(netip.Addr{}) != true {
		t.Error("invalid address not blocked")
	}
	for addr, blocked := range map[string]bool{
		"169.254.169.254:80":          true,
		"[::ffff:169.254.169.254]:80": true,
		"[fd00:ec2::254]:80":          true,
		"0.0.0.0:9000":                true,
		"[ff02::fb]:5353":             true,
		"not an address":              true,
		"127.0.0.1:9000":              false,
		"[fe80::1%eth0]:9000":         false,
	} {
		if err := checkDialAddr(context.Background(), "tcp", addr, nil); (err != nil) != blocked || (err != nil && !errors.Is(err, ErrBlockedAddress)) {
			t.Errorf("checkDialAddr(%q) = %v, want blocked=%v", addr, err, blocked)
		}
	}
}

// TestProberRefusesBlockedAddresses: literal and resolved blocked addresses
// are refused before a connection is made; other resolved addresses are
// still tried.
func TestProberRefusesBlockedAddresses(t *testing.T) {
	srv := newGos3Server(t, "peer000000000001", "alpha")
	port := strings.TrimPrefix(srv.hostPort(), "127.0.0.1:")
	p := newProber()
	defer p.close()
	lookups := 0
	p.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups++
		switch host {
		case "metadata.test":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		case "zero.test":
			return []netip.Addr{netip.MustParseAddr("0.0.0.0")}, nil
		case "mapped.test":
			return []netip.Addr{netip.MustParseAddr("::ffff:0.0.0.0")}, nil
		case "mixed.test":
			return []netip.Addr{netip.MustParseAddr("100.100.100.200"), netip.MustParseAddr("127.0.0.1")}, nil
		case "ok.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, u := range []string{
		"http://0.0.0.0:" + port, // Linux connects 0.0.0.0 to the local host
		"http://[::]:" + port,
		"http://[::ffff:0.0.0.0]:" + port,
		"http://zero.test:" + port,
		"http://mapped.test:" + port,
		"http://metadata.test",
		"http://169.254.169.254",
		"http://[fd00:ec2::254]",
		"http://100.100.100.200",
		"http://224.0.0.251:" + port,
		"http://255.255.255.255:" + port,
	} {
		start := time.Now()
		_, err := p.fetch(ctx, u)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: err = %v, want ErrBlockedAddress", u, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: refusal took %v (connected?)", u, d)
		}
		if got := ProbeErrorText(err); !strings.Contains(got, "not allowed") {
			t.Errorf("%s: ProbeErrorText = %q", u, got)
		}
	}
	if srv.Hits() != 0 {
		t.Fatalf("blocked address reached the server %d times", srv.Hits())
	}
	for _, u := range []string{"http://mixed.test:" + port, "http://ok.test:" + port} {
		if info, err := p.fetch(ctx, u); err != nil || info.ID != "peer000000000001" {
			t.Errorf("%s: %+v %v", u, info, err)
		}
	}
	if lookups != 5 {
		t.Errorf("resolver called %d times, want 5", lookups)
	}
}

// TestProberNoRedirects: a redirect, even to a valid gos3 server, is not
// followed.
func TestProberNoRedirects(t *testing.T) {
	target := newGos3Server(t, "peer000000000001", "alpha")
	redir := httptest.NewServer(http.RedirectHandler(target.URL+InfoPath, http.StatusTemporaryRedirect))
	defer redir.Close()
	p := newProber()
	defer p.close()
	if _, err := p.fetch(context.Background(), redir.URL); !errors.Is(err, ErrNotGos3) {
		t.Errorf("err = %v", err)
	}
	if target.Hits() != 0 {
		t.Error("redirect followed")
	}
}

// TestServiceRefusesBlockedAddresses: every mechanism shares the prober, so
// probes, scans and seeds never reach a blocked address.
func TestServiceRefusesBlockedAddresses(t *testing.T) {
	defer checkLeaks(t)()
	srv := newGos3Server(t, "peer000000000001", "alpha")
	port := srv.port(t)
	s := New(quietConfig(), nil, testLogger(t))
	if _, err := s.Probe(context.Background(), "0.0.0.0:"+srv.hostPort()[len("127.0.0.1:"):]); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("probe err = %v", err)
	}
	res, err := s.Scan(context.Background(), ScanRequest{Targets: []string{"0.0.0.0", "::", "0.0.0.0/30"}, Ports: []int{port}})
	if err != nil || len(res.Found) != 0 {
		t.Errorf("scan = %+v %v", res, err)
	}
	s.verifyAll(context.Background(), []candidate{{source: SourceStatic, urls: []string{baseURL("http", "0.0.0.0", port)}}}, 1, time.Second)
	if srv.Hits() != 0 || len(s.Peers()) != 0 {
		t.Errorf("blocked address reached: hits %d, peers %+v", srv.Hits(), s.Peers())
	}
	s.Stop()
}

// TestSightingErrorIsCategorised: the peer table stores an error category,
// never text a probed service sent (e.g. a banner of a non-HTTP service).
func TestSightingErrorIsCategorised(t *testing.T) {
	defer checkLeaks(t)()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Read(make([]byte, 512)) // the GET request
			_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6 secret-banner\r\n"))
			_ = c.Close()
		}
	}()
	s := New(quietConfig(), nil, testLogger(t))
	s.verify(context.Background(), candidate{id: "spoof00000000001", source: SourceMDNS, urls: []string{"http://" + ln.Addr().String()}}, time.Second)
	s.table.hint(candidate{id: "spoof00000000001", source: SourceMDNS}, s.now(), "")
	s.verify(context.Background(), candidate{id: "spoof00000000001", source: SourceMDNS, urls: []string{"http://" + ln.Addr().String()}}, time.Second)
	p, ok := s.table.get("spoof00000000001")
	if !ok || p.Error == "" || strings.Contains(p.Error, "SSH") || strings.Contains(p.Error, "banner") {
		t.Errorf("peer = %+v", p)
	}
	if got := ProbeErrorText(errors.New(`malformed HTTP response "SSH-2.0"`)); got != "not an HTTP server" {
		t.Errorf("ProbeErrorText = %q", got)
	}
	s.Stop()
}
