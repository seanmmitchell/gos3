package config

import (
	"bytes"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestTrustedProxiesLoad checks server.trusted_proxies from the YAML file,
// the GOS3_SERVER_TRUSTED_PROXIES variable (which wins) and a flag override,
// with entries trimmed.
func TestTrustedProxiesLoad(t *testing.T) {
	file := writeFile(t, "c.yaml", "server:\n  trust_proxy_headers: true\n  trusted_proxies: [\" 10.0.0.0/8 \", \"192.0.2.10\", \"\"]\n")
	c, err := load(file, env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.0/8", "192.0.2.10"}; !slices.Equal(c.Server.TrustedProxies, want) {
		t.Errorf("file: %q, want %q", c.Server.TrustedProxies, want)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	c, err = load(file, env(map[string]string{"GOS3_SERVER_TRUSTED_PROXIES": " 172.16.0.0/12 , 2001:db8::/32,, "}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"172.16.0.0/12", "2001:db8::/32"}; !slices.Equal(c.Server.TrustedProxies, want) {
		t.Errorf("env: %q, want %q", c.Server.TrustedProxies, want)
	}

	c, err = load(file, env(map[string]string{"GOS3_SERVER_TRUSTED_PROXIES": ""}), nil)
	if err != nil || len(c.Server.TrustedProxies) != 0 {
		t.Errorf("empty env clears the list: %q %v", c.Server.TrustedProxies, err)
	}

	c, err = load("", env(nil), []Override{{"server.trusted_proxies", "::1"}})
	if err != nil || !slices.Equal(c.Server.TrustedProxies, []string{"::1"}) {
		t.Errorf("override: %q %v", c.Server.TrustedProxies, err)
	}

	if Default().Server.TrustedProxies != nil {
		t.Error("trusted_proxies has a default")
	}
}

func TestTrustedProxiesValidate(t *testing.T) {
	for _, tt := range []struct {
		proxies []string
		want    string
	}{
		{[]string{"10.0.0.0/8", "10.0.0.0/33"}, `server.trusted_proxies[1]: "10.0.0.0/33" is not an IP address or CIDR`},
		{[]string{"proxy.example.com"}, "server.trusted_proxies[0]"},
		{[]string{"10.0.0.1:80"}, "server.trusted_proxies[0]"},
		{[]string{"fe80::1%eth0"}, "server.trusted_proxies[0]"},
		{slices.Repeat([]string{"10.0.0.1"}, MaxListEntries+1), "server.trusted_proxies: at most"},
	} {
		c := Default()
		c.Server.TrustedProxies = tt.proxies
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error %v, want %q", tt.proxies, err, tt.want)
		}
	}
	// Invalid values are rejected at load time too (serve and config print
	// call Validate after Load).
	c, err := load("", env(map[string]string{"GOS3_SERVER_TRUSTED_PROXIES": "10.0.0.0/8,nope"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "server.trusted_proxies[1]") {
		t.Errorf("env value: %v", err)
	}
}

func TestTrustedProxiesPrinted(t *testing.T) {
	c := Default()
	c.Server.TrustProxyHeaders = true
	c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	r := c.Redacted()
	r.Server.TrustedProxies[0] = "changed"
	if c.Server.TrustedProxies[0] != "10.0.0.0/8" {
		t.Error("Redacted shares trusted_proxies with the original")
	}
	data, err := c.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("trusted_proxies:\n        - 10.0.0.0/8")) && !bytes.Contains(data, []byte("trusted_proxies:\n    - 10.0.0.0/8")) {
		t.Errorf("yaml lacks trusted_proxies:\n%s", data)
	}
	back, err := load(writeFile(t, "c.yaml", string(data)), env(nil), nil)
	if err != nil || !slices.Equal(back.Server.TrustedProxies, c.Server.TrustedProxies) {
		t.Errorf("round trip: %q %v", back.Server.TrustedProxies, err)
	}
	var buf bytes.Buffer
	if err := WriteEnvTable(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "GOS3_SERVER_TRUSTED_PROXIES") || !strings.Contains(buf.String(), "server.trusted_proxies") {
		t.Errorf("env table lacks the variable:\n%s", buf.String())
	}
}

// TestConfigClientIP checks the resolver built from the configuration.
func TestConfigClientIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "10.1.2.3, 203.0.113.50")

	c := Default()
	req.RemoteAddr = "10.0.0.2:1"
	r, err := c.ClientIP()
	if err != nil {
		t.Fatal(err)
	}
	if ip, _ := r.ClientInfo(req); ip != "10.0.0.2" {
		t.Errorf("headers off: %s", ip)
	}

	c.Server.TrustProxyHeaders = true
	c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	if r, err = c.ClientIP(); err != nil {
		t.Fatal(err)
	}
	if ip, _ := r.ClientInfo(req); ip != "203.0.113.50" {
		t.Errorf("trusted proxy: %s", ip)
	}
	req.RemoteAddr = "198.51.100.1:1"
	if ip, _ := r.ClientInfo(req); ip != "198.51.100.1" {
		t.Errorf("untrusted peer: %s", ip)
	}

	c.Server.TrustedProxies = []string{"bad"}
	if _, err := c.ClientIP(); err == nil || !strings.Contains(err.Error(), "server.trusted_proxies[0]") {
		t.Errorf("invalid entry: %v", err)
	}
}
