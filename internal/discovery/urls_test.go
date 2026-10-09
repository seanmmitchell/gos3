package discovery

import (
	"errors"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://10.0.0.5:9000", want: "http://10.0.0.5:9000"},
		{in: "10.0.0.5:9000", want: "http://10.0.0.5:9000"},
		{in: "  nas.local:9000  ", want: "http://nas.local:9000"},
		{in: "nas.local", want: "http://nas.local"},
		{in: "HTTPS://S3.Example.COM/", want: "https://s3.example.com"},
		{in: "http://host:9000/some/path/?q=1#frag", want: "http://host:9000"},
		{in: "http://user:pass@host:9000", want: "http://host:9000"},
		{in: "http://host.:9000", want: "http://host:9000"},
		{in: "http://[::1]:9000/", want: "http://[::1]:9000"},
		{in: "[fe80::1%25eth0]:9000", want: "http://[fe80::1%25eth0]:9000"},
		{in: "http://my_host:1", want: "http://my_host:1"},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "ftp://host", wantErr: true},
		{in: "http://", wantErr: true},
		{in: "http://:9000", wantErr: true},
		{in: "http://host:0", wantErr: true},
		{in: "http://host:65536", wantErr: true},
		{in: "http://host:", wantErr: true},
		{in: "http://host:abc", wantErr: true},
		{in: "http://-bad-.com", wantErr: true},
		{in: "http://a..b", wantErr: true},
		{in: "http://ho st", wantErr: true},
		{in: "http://" + strings.Repeat("a", 64) + ".com", wantErr: true},
		{in: "http://" + strings.Repeat("a", maxURLLen), wantErr: true},
		{in: "javascript:alert(1)", wantErr: true},
	}
	for _, tt := range tests {
		got, err := NormalizeBaseURL(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("NormalizeBaseURL(%q) = %q, want error", tt.in, got)
			} else if !errors.Is(err, ErrInvalidURL) {
				t.Errorf("NormalizeBaseURL(%q) error %v does not wrap ErrInvalidURL", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestValidHostname(t *testing.T) {
	good := []string{"a", "nas", "nas.local", "nas.local.", "my-host.example.com", "_gos3._tcp.example.com", "123"}
	bad := []string{"", ".", "-a", "a-", "a..b", "a b", "a/b", strings.Repeat("a", 64), strings.Repeat("a.", 127) + "aa"}
	for _, h := range good {
		if !validHostname(h) {
			t.Errorf("validHostname(%q) = false", h)
		}
	}
	for _, h := range bad {
		if validHostname(h) {
			t.Errorf("validHostname(%q) = true", h)
		}
	}
}

func TestBeaconCandidateURLs(t *testing.T) {
	remote := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 40000}
	loop := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
	tests := []struct {
		name string
		urls []string
		src  net.Addr
		want []string
	}{
		{"plain", []string{"http://10.0.0.5:9000"}, remote, []string{"http://10.0.0.5:9000"}},
		{"dedup and normalise", []string{"http://10.0.0.5:9000/", "10.0.0.5:9000"}, remote, []string{"http://10.0.0.5:9000"}},
		{"wildcard v4", []string{"http://0.0.0.0:9001"}, remote, []string{"http://10.0.0.7:9001"}},
		{"wildcard v6", []string{"https://[::]:9443"}, remote, []string{"https://10.0.0.7:9443"}},
		{"empty host", []string{"http://:9002"}, remote, []string{"http://10.0.0.7:9002"}},
		{"loopback from remote", []string{"http://127.0.0.1:9003", "http://localhost:9003"}, remote, []string{"http://10.0.0.7:9003"}},
		{"loopback from loopback kept", []string{"http://127.0.0.1:9003"}, loop, []string{"http://127.0.0.1:9003"}},
		{"no urls", nil, remote, []string{"http://10.0.0.7:9000"}},
		{"garbage only", []string{"::::", "ftp://x"}, remote, []string{"http://10.0.0.7:9000"}},
		{"wildcard default port", []string{"https://0.0.0.0"}, remote, []string{"https://10.0.0.7:443"}},
		{"no source", []string{"http://0.0.0.0:9000"}, nil, nil},
		{"mixed", []string{"http://0.0.0.0:9000", "http://192.168.1.2:9000"}, remote, []string{"http://10.0.0.7:9000", "http://192.168.1.2:9000"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := beaconCandidateURLs(tt.urls, tt.src)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
	// Bounded output.
	var many []string
	for i := range 100 {
		many = append(many, baseURL("http", "10.1.0."+strconv.Itoa(i), 9000))
	}
	if got := beaconCandidateURLs(many, remote); len(got) != maxBeaconURLs {
		t.Errorf("got %d urls, want %d", len(got), maxBeaconURLs)
	}
}

func TestIPFromAddr(t *testing.T) {
	ip, ok := ipFromAddr(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4).To16(), Port: 1})
	if !ok || ip.String() != "1.2.3.4" {
		t.Fatalf("got %v %v", ip, ok)
	}
	ip, ok = ipFromAddr(&net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"})
	if !ok || ip.String() != "fe80::1%eth0" {
		t.Fatalf("got %v %v", ip, ok)
	}
	if _, ok := ipFromAddr(nil); ok {
		t.Fatal("nil addr accepted")
	}
}

func TestClip(t *testing.T) {
	if got := clip("héllo", 2); got != "h" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("abc", 5); got != "abc" {
		t.Errorf("clip = %q", got)
	}
}

func TestNewServerID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := map[string]bool{}
	for range 100 {
		id := NewServerID()
		if !re.MatchString(id) {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		if !validID(id) {
			t.Fatalf("NewServerID %q not valid", id)
		}
	}
}

func TestSelfAdvertisedURLs(t *testing.T) {
	got := SelfAdvertisedURLs("10.0.0.5:9000", "https://s3.example.com/", false)
	want := []string{"https://s3.example.com", "http://10.0.0.5:9000"}
	if !slices.Equal(got, want) {
		t.Errorf("specific host: got %q want %q", got, want)
	}
	got = SelfAdvertisedURLs("localhost:9443", "", true)
	if !slices.Equal(got, []string{"https://localhost:9443"}) {
		t.Errorf("tls: got %q", got)
	}
	// Duplicate public URL collapses.
	got = SelfAdvertisedURLs("10.0.0.5:9000", "http://10.0.0.5:9000", false)
	if !slices.Equal(got, []string{"http://10.0.0.5:9000"}) {
		t.Errorf("dup: got %q", got)
	}
	// Invalid listen address: only the public URL.
	got = SelfAdvertisedURLs("nonsense", "http://pub:1", false)
	if !slices.Equal(got, []string{"http://pub:1"}) {
		t.Errorf("invalid: got %q", got)
	}
	if got := SelfAdvertisedURLs(":99999", "", false); len(got) != 0 {
		t.Errorf("bad port: got %q", got)
	}
	// Wildcard: every non-loopback interface address (or 127.0.0.1).
	for _, listen := range []string{":9000", "0.0.0.0:9000", "[::]:9000"} {
		got = SelfAdvertisedURLs(listen, "", false)
		if len(got) == 0 {
			t.Fatalf("%s: no urls", listen)
		}
		ips := interfaceIPs(listen != "0.0.0.0:9000")
		for _, u := range got {
			_, host, port, ok := urlHostPort(u)
			if !ok || port != "9000" {
				t.Errorf("%s: bad url %q", listen, u)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				t.Errorf("%s: host %q is not an IP", listen, host)
				continue
			}
			if len(ips) > 0 && (ip.IsLoopback() || !slices.Contains(ips, ip)) {
				t.Errorf("%s: unexpected address %v (interfaces %v)", listen, ip, ips)
			}
			if listen == "0.0.0.0:9000" && !ip.Is4() {
				t.Errorf("%s: IPv6 address %v for IPv4 listener", listen, ip)
			}
		}
	}
}

func TestConfigValidate(t *testing.T) {
	ok := []func(*Config){
		func(c *Config) {},
		func(c *Config) { *c = Config{} },
		func(c *Config) { c.BeaconGroup = "239.1.2.3:1234" },
		func(c *Config) { c.Seeds = []string{"nas:9000", "https://x.example.com"} },
		func(c *Config) { c.DNSSRV = []string{"example.com", "lan."} },
		func(c *Config) { c.BeaconInterval = time.Second },
		func(c *Config) { c.MaxScanHosts = MaxScanHostsLimit },
	}
	for i, f := range ok {
		c := DefaultConfig()
		f(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("ok case %d: %v", i, err)
		}
	}
	bad := []func(*Config){
		func(c *Config) { c.BeaconGroup = "10.0.0.1:7373" },
		func(c *Config) { c.BeaconGroup = "239.1.2.3" },
		func(c *Config) { c.BeaconGroup = "239.1.2.3:0" },
		func(c *Config) { c.BeaconGroup = "[ff02::1]:7373" },
		func(c *Config) { c.BeaconInterval = -1 },
		func(c *Config) { c.BeaconInterval = time.Millisecond },
		func(c *Config) { c.BeaconInterval = 2 * time.Hour },
		func(c *Config) { c.PeerTTL = -time.Second },
		func(c *Config) { c.MaxScanHosts = -1 },
		func(c *Config) { c.MaxScanHosts = MaxScanHostsLimit + 1 },
		func(c *Config) { c.Seeds = []string{"ftp://x"} },
		func(c *Config) { c.Seeds = make([]string, MaxSeeds+1) },
		func(c *Config) { c.DNSSRV = []string{"bad domain"} },
		func(c *Config) { c.DNSSRV = make([]string, MaxDNSSRV+1) },
	}
	for i, f := range bad {
		c := DefaultConfig()
		f(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("bad case %d: err = %v", i, err)
		}
	}
}

func TestResolveDefaults(t *testing.T) {
	e := resolve(Config{Seeds: []string{"a:1", "http://a:1/", "bad url"}, DNSSRV: []string{"Example.COM.", "example.com"}})
	if e.group.String() != DefaultBeaconGroup {
		t.Errorf("group %v", e.group)
	}
	if e.interval != DefaultBeaconInterval || e.ttl != 3*DefaultBeaconInterval || e.maxHosts != DefaultMaxScanHosts {
		t.Errorf("defaults: %v %v %v", e.interval, e.ttl, e.maxHosts)
	}
	if !slices.Equal(e.seeds, []string{"http://a:1"}) || !slices.Equal(e.domains, []string{"example.com"}) {
		t.Errorf("lists: %q %q", e.seeds, e.domains)
	}
	if e := resolve(Config{BeaconInterval: 2 * time.Second}); e.ttl != minPeerTTL {
		t.Errorf("min ttl: %v", e.ttl)
	}
	if e := resolve(Config{PeerTTL: 5 * time.Minute}); e.ttl != 5*time.Minute {
		t.Errorf("explicit ttl: %v", e.ttl)
	}
}
