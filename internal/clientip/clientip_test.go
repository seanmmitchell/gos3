package clientip

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func mustPrefixes(t *testing.T, entries ...string) []netip.Prefix {
	t.Helper()
	p, err := ParsePrefixes(entries)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientInfo(t *testing.T) {
	type tc struct {
		name    string
		trust   bool
		proxies []string
		remote  string
		tls     bool
		xff     []string // X-Forwarded-For header lines
		proto   []string // X-Forwarded-Proto header lines
		wantIP  string
		wantSec bool
	}
	cases := []tc{
		// Headers off: the connection wins whatever the client sends.
		{name: "headers ignored when not trusted", remote: "192.0.2.1:5555",
			xff: []string{"10.1.2.3"}, proto: []string{"https"}, wantIP: "192.0.2.1"},
		{name: "tls connection without trust", remote: "192.0.2.1:5555", tls: true,
			proto: []string{"http"}, wantIP: "192.0.2.1", wantSec: true},
		{name: "ipv6 remote", remote: "[2001:db8::7]:5555", wantIP: "2001:db8::7"},
		{name: "ipv4-mapped remote", remote: "[::ffff:192.0.2.1]:5555", wantIP: "192.0.2.1"},
		{name: "zoned remote", remote: "[fe80::1%eth0]:5555", wantIP: "fe80::1"},
		{name: "unparsable remote kept", remote: "pipe", wantIP: "pipe"},

		// trust_proxy_headers with an empty trusted_proxies: rightmost entry.
		{name: "empty list uses rightmost", trust: true, remote: "10.0.0.2:1",
			xff: []string{"10.1.2.3, 198.51.100.9"}, wantIP: "198.51.100.9"},
		// The audit scenario: nginx $proxy_add_x_forwarded_for appends the
		// real peer to the client's forged value.
		{name: "spoofed leftmost entry ignored (appending proxy)", trust: true, remote: "10.0.0.2:1",
			xff: []string{"10.1.2.3, 203.0.113.50"}, wantIP: "203.0.113.50"},
		{name: "spoofed leftmost entry ignored (trusted proxy list)", trust: true,
			proxies: []string{"10.0.0.2"}, remote: "10.0.0.2:1",
			xff: []string{"10.1.2.3, 203.0.113.50"}, wantIP: "203.0.113.50"},
		{name: "single overwritten entry", trust: true, remote: "10.0.0.2:1",
			xff: []string{"203.0.113.50"}, wantIP: "203.0.113.50"},
		{name: "no header keeps remote", trust: true, remote: "10.0.0.2:1", wantIP: "10.0.0.2"},
		{name: "empty list invalid rightmost uses remote", trust: true, remote: "10.0.0.2:1",
			xff: []string{"203.0.113.50, unknown"}, wantIP: "10.0.0.2"},

		// Multi-hop chains with trusted_proxies.
		{name: "multi-hop chain", trust: true, proxies: []string{"10.0.0.0/8", "172.16.0.0/12"},
			remote: "10.0.0.2:1", xff: []string{"6.6.6.6, 203.0.113.50, 172.16.4.4, 10.9.9.9"},
			wantIP: "203.0.113.50"},
		{name: "multi-hop chain over several header lines", trust: true, proxies: []string{"10.0.0.0/8", "172.16.0.0/12"},
			remote: "10.0.0.2:1", xff: []string{"6.6.6.6", " 203.0.113.50 ,172.16.4.4", "10.9.9.9 "},
			wantIP: "203.0.113.50"},
		{name: "all hops trusted uses leftmost", trust: true, proxies: []string{"10.0.0.0/8"},
			remote: "10.0.0.2:1", xff: []string{"10.5.5.5, 10.9.9.9"}, wantIP: "10.5.5.5"},
		{name: "invalid entry stops at last trusted hop", trust: true, proxies: []string{"10.0.0.0/8"},
			remote: "10.0.0.2:1", xff: []string{"203.0.113.50, garbage, 10.9.9.9"}, wantIP: "10.9.9.9"},
		{name: "invalid rightmost entry uses remote", trust: true, proxies: []string{"10.0.0.0/8"},
			remote: "10.0.0.2:1", xff: []string{"203.0.113.50, garbage"}, wantIP: "10.0.0.2"},
		{name: "empty entries skipped", trust: true, proxies: []string{"10.0.0.0/8"},
			remote: "10.0.0.2:1", xff: []string{"203.0.113.50,, ", ""}, wantIP: "203.0.113.50"},
		{name: "ipv6 proxies and clients", trust: true, proxies: []string{"2001:db8:ffff::/48"},
			remote: "[2001:db8:ffff::2]:1", xff: []string{"[2001:db8::99]:443, 2001:db8:ffff::5"},
			wantIP: "2001:db8::99"},
		{name: "bracketed ipv6 entry", trust: true, remote: "10.0.0.2:1",
			xff: []string{"[2001:db8::99]"}, wantIP: "2001:db8::99"},
		{name: "zoned ipv6 entry", trust: true, remote: "10.0.0.2:1",
			xff: []string{"fe80::1%eth0"}, wantIP: "fe80::1"},
		{name: "ipv4 with port", trust: true, remote: "10.0.0.2:1",
			xff: []string{"203.0.113.50:6123"}, wantIP: "203.0.113.50"},
		{name: "quoted entry", trust: true, remote: "10.0.0.2:1",
			xff: []string{`"203.0.113.50"`}, wantIP: "203.0.113.50"},
		{name: "ipv4-mapped entry is unmapped and matched", trust: true, proxies: []string{"10.0.0.0/8"},
			remote: "[::ffff:10.0.0.2]:1", xff: []string{"203.0.113.50, ::ffff:10.9.9.9"},
			wantIP: "203.0.113.50"},
		{name: "ipv4-mapped proxy prefix", trust: true, proxies: []string{"::ffff:10.0.0.0/104"},
			remote: "10.0.0.2:1", xff: []string{"203.0.113.50, 10.9.9.9"}, wantIP: "203.0.113.50"},
		{name: "garbage entries", trust: true, remote: "10.0.0.2:1",
			xff: []string{"[2001:db8::1"}, wantIP: "10.0.0.2"},
		{name: "bracketed ipv4 rejected", trust: true, remote: "10.0.0.2:1",
			xff: []string{"[192.0.2.1]"}, wantIP: "10.0.0.2"},
		{name: "bad port rejected", trust: true, remote: "10.0.0.2:1",
			xff: []string{"192.0.2.1:http"}, wantIP: "10.0.0.2"},

		// Untrusted RemoteAddr: headers ignored entirely.
		{name: "untrusted remote ignores headers", trust: true, proxies: []string{"10.0.0.0/8"},
			remote: "198.51.100.1:1", xff: []string{"10.1.2.3"}, proto: []string{"https"},
			wantIP: "198.51.100.1"},
		{name: "unparsable remote is not trusted", trust: true, proxies: []string{"10.0.0.0/8"},
			remote: "pipe", xff: []string{"203.0.113.50"}, wantIP: "pipe"},

		// X-Forwarded-Proto: the last value counts.
		{name: "proto https", trust: true, remote: "10.0.0.2:1", proto: []string{"https"},
			wantIP: "10.0.0.2", wantSec: true},
		{name: "proto last value wins (list)", trust: true, remote: "10.0.0.2:1", proto: []string{"https, http"},
			wantIP: "10.0.0.2"},
		{name: "proto last value wins (lines)", trust: true, remote: "10.0.0.2:1", proto: []string{"http", " HTTPS "},
			wantIP: "10.0.0.2", wantSec: true},
		{name: "proto http over tls connection", trust: true, remote: "10.0.0.2:1", tls: true,
			proto: []string{"https,http"}, wantIP: "10.0.0.2"},
		{name: "proto trailing empty ignored", trust: true, remote: "10.0.0.2:1", proto: []string{"https,"},
			wantIP: "10.0.0.2", wantSec: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := New(c.trust, mustPrefixes(t, c.proxies...))
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = c.remote
			if c.tls {
				req.TLS = &tls.ConnectionState{}
			} else {
				req.TLS = nil
			}
			for _, v := range c.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			for _, v := range c.proto {
				req.Header.Add("X-Forwarded-Proto", v)
			}
			ip, sec := r.ClientInfo(req)
			if ip != c.wantIP || sec != c.wantSec {
				t.Fatalf("ClientInfo = %q, %v; want %q, %v", ip, sec, c.wantIP, c.wantSec)
			}
		})
	}
}

func TestZeroResolverIgnoresHeaders(t *testing.T) {
	var r Resolver
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.1:1"
	req.Header.Set("X-Forwarded-For", "10.1.2.3")
	req.Header.Set("X-Forwarded-Proto", "https")
	if ip, sec := r.ClientInfo(req); ip != "192.0.2.1" || sec {
		t.Fatalf("%s %v", ip, sec)
	}
	if r.TrustsHeaders() {
		t.Fatal("zero resolver trusts headers")
	}
}

// TestNewCopiesProxies checks that the resolver does not alias the caller's
// slice.
func TestNewCopiesProxies(t *testing.T) {
	p := mustPrefixes(t, "10.0.0.0/8")
	r := New(true, p)
	p[0] = netip.MustParsePrefix("192.168.0.0/16")
	req := &http.Request{RemoteAddr: "10.0.0.2:1", Header: http.Header{"X-Forwarded-For": {"203.0.113.50"}}}
	if ip, _ := r.ClientInfo(req); ip != "203.0.113.50" {
		t.Fatal(ip)
	}
}

func TestParsePrefixes(t *testing.T) {
	ok := map[string]string{
		"10.0.0.0/8":          "10.0.0.0/8",
		" 10.1.2.3/8 ":        "10.0.0.0/8",
		"192.0.2.10":          "192.0.2.10/32",
		"2001:db8::/32":       "2001:db8::/32",
		"::1":                 "::1/128",
		"[::1]":               "::1/128",
		"::ffff:10.0.0.0/104": "10.0.0.0/8",
		"::ffff:192.0.2.1":    "192.0.2.1/32",
		"0.0.0.0/0":           "0.0.0.0/0",
	}
	for in, want := range ok {
		p, err := ParsePrefix(in)
		if err != nil || p.String() != want {
			t.Errorf("ParsePrefix(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	for _, in := range []string{"", " ", "nope", "10.0.0.0/33", "10.0.0/8", "fe80::1%eth0", "10.0.0.1:80", "[10.0.0.1]/8", "1.2.3.4/-1", "2001:db8::/129"} {
		if p, err := ParsePrefix(in); err == nil {
			t.Errorf("ParsePrefix(%q) = %v, want error", in, p)
		}
	}
	if _, err := ParsePrefixes([]string{"10.0.0.0/8", "bad"}); err == nil || !strings.HasPrefix(err.Error(), "[1]: ") {
		t.Errorf("ParsePrefixes error = %v, want one naming index 1", err)
	}
	if p, err := ParsePrefixes(nil); err != nil || len(p) != 0 {
		t.Errorf("ParsePrefixes(nil) = %v, %v", p, err)
	}
}
