package server

import (
	"net"
	"os"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
)

func TestHostPolicyKnown(t *testing.T) {
	cfg := testConfig()
	cfg.Server.Address = "gos3.internal:9000"
	cfg.Server.AllowedHosts = []string{"files.example.org", "*.cdn.example"}
	p := newHostPolicy(cfg)
	st := config.Settings{PublicURL: "https://s3.example.com", Domains: []string{"s3.example.net"}}
	known := []string{
		"", "127.0.0.1:9000", "192.168.1.10", "[::1]:9000", "[2001:db8::1]",
		"localhost:9000", "LOCALHOST", "ui.localhost:3000", "gos3.internal:9000",
		"s3.example.com", "S3.Example.Com.:443", "s3.example.net", "photos.s3.example.net:9000",
		"files.example.org", "a.cdn.example", "x.y.cdn.example",
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		label, _, _ := strings.Cut(strings.ToLower(name), ".")
		known = append(known, name, label+".local:9000")
	}
	for _, h := range known {
		if !p.known(h, &st) {
			t.Errorf("%q not known", h)
		}
	}
	for _, h := range []string{
		"rebind.attacker.example", "attacker.example:9000", "s3.example.com.attacker.example",
		"example.com", "cdn.example", "evil-s3.example.net", "localhost.attacker.example",
	} {
		if p.known(h, &st) {
			t.Errorf("%q known", h)
		}
	}
	if p.strict != true {
		t.Error("allowed_hosts set: not strict")
	}
	if any := newHostPolicy(config.Config{Server: config.ServerConfig{AllowedHosts: []string{"*"}}}); !any.known("evil.example", &st) || any.strict {
		t.Error(`allowed_hosts ["*"] does not turn the check off`)
	}
	if def := newHostPolicy(testConfig()); def.strict {
		t.Error("strict without allowed_hosts")
	}
}

// TestDNSRebinding: a website whose host name resolves to the server
// (DNS rebinding) is same-origin with it in the visitor's browser, so CORS
// does not protect anonymously accessible LAN-only data. Requests for a
// Host that is not one of the server's names get no anonymous access, UI
// or public metrics; signed requests and the public probes still work.
func TestDNSRebinding(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.Public = true
	srv := start(t, cfg, WithUIFS(fakeUI()))
	u := srv.url
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(u, "http://"))
	evil := map[string]string{"Host": "rebind.attacker.example:" + port}

	setupLANBucket(t, u)
	if r := do(t, "GET", u+"/lan/secret.txt", nil, nil, nil); r.status != 200 || string(r.body) != "lan-only-secret" {
		t.Fatalf("anonymous GET by IP: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", u+"/lan/secret.txt", nil, nil, evil); r.status != 403 || strings.Contains(string(r.body), "lan-only-secret") {
		t.Errorf("anonymous GET for a rebinding host: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", u+"/lan/x", []byte("planted"), nil, evil); r.status != 403 {
		t.Errorf("anonymous PUT for a rebinding host: %d %s", r.status, r.body)
	}
	for _, p := range []string{"/_gos3/ui/", "/_gos3/metrics"} {
		if r := do(t, "GET", u+p, nil, nil, evil); r.status != 421 {
			t.Errorf("GET %s for a rebinding host: %d %s", p, r.status, r.body)
		}
	}
	if r := do(t, "GET", u+"/", nil, nil, map[string]string{"Host": evil["Host"], "Accept": "text/html"}); r.status != 421 {
		t.Errorf("browser GET / for a rebinding host: %d", r.status)
	}
	// Signed requests (the signature covers the Host) and public probes.
	if r := do(t, "GET", u+"/lan/secret.txt", nil, &rootCreds, evil); r.status != 200 {
		t.Errorf("signed GET for another host name: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", u+"/_gos3/admin/v1/info", nil, &rootCreds, evil); r.status != 200 {
		t.Errorf("signed admin request for another host name: %d %s", r.status, r.body)
	}
	for _, p := range []string{"/_gos3/health", "/_gos3/ready", "/_gos3/info"} {
		if r := do(t, "GET", u+p, nil, nil, evil); r.status != 200 {
			t.Errorf("GET %s for another host name: %d %s", p, r.status, r.body)
		}
	}
	// Known names keep working anonymously.
	for _, h := range []string{"localhost:" + port, "127.0.0.1:" + port} {
		if r := do(t, "GET", u+"/_gos3/ui/", nil, nil, map[string]string{"Host": h}); r.status != 200 {
			t.Errorf("UI for %s: %d", h, r.status)
		}
	}
}

// TestAllowedHostsStrict: with server.allowed_hosts set, requests for any
// other host name are refused, signed ones included, so a signature
// captured by a foreign endpoint (Host: evil.example) cannot be relayed to
// this server.
func TestAllowedHostsStrict(t *testing.T) {
	cfg := testConfig()
	cfg.Server.AllowedHosts = []string{"s3.example.com"}
	srv := start(t, cfg)
	u := srv.url
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(u, "http://"))
	if r := do(t, "GET", u+"/", nil, &rootCreds, map[string]string{"Host": "evil.example:" + port}); r.status != 421 ||
		!strings.Contains(string(r.body), "MisdirectedRequest") {
		t.Errorf("signed request relayed with a foreign Host: %d %s", r.status, r.body)
	}
	if r := do(t, "GET", u+"/_gos3/health", nil, nil, map[string]string{"Host": "evil.example"}); r.status != 421 {
		t.Errorf("health for a foreign Host: %d", r.status)
	}
	for _, h := range []string{"s3.example.com", "S3.EXAMPLE.COM:" + port, "127.0.0.1:" + port} {
		if r := do(t, "GET", u+"/", nil, &rootCreds, map[string]string{"Host": h}); r.status != 200 {
			t.Errorf("signed request for %s: %d %s", h, r.status, r.body)
		}
	}

	cfg = testConfig()
	cfg.Server.AllowedHosts = []string{"*"}
	srv = start(t, cfg)
	setupLANBucket(t, srv.url)
	if r := do(t, "GET", srv.url+"/lan/secret.txt", nil, nil, map[string]string{"Host": "any.example"}); r.status != 200 {
		t.Errorf(`allowed_hosts ["*"]: anonymous GET for any host: %d %s`, r.status, r.body)
	}
}

// setupLANBucket creates bucket "lan" readable and writable anonymously
// from loopback only, holding secret.txt.
func setupLANBucket(t *testing.T, u string) {
	t.Helper()
	if r := do(t, "PUT", u+"/lan", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("create bucket: %d %s", r.status, r.body)
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:PutObject"],` +
		`"Resource":"arn:aws:s3:::lan/*","Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}}]}`
	if r := do(t, "PUT", u+"/lan?policy", []byte(policy), &rootCreds, nil); r.status/100 != 2 {
		t.Fatalf("put policy: %d %s", r.status, r.body)
	}
	if r := do(t, "PUT", u+"/lan/secret.txt", []byte("lan-only-secret"), &rootCreds, nil); r.status != 200 {
		t.Fatalf("put object: %d %s", r.status, r.body)
	}
}
